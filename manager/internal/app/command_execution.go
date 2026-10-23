package app

import (
	"rcd_manager/internal/domain"
	"sync"
	"time"
)

type pendingCommandMeta struct {
	tracked          bool
	generation       uint64
	epoch            uint64
	baselineSequence uint64
	armed            bool
	terminal         *pendingCommandTerminal
	done             chan pendingCommandTerminal
	queued           *queuedCommandDispatch
	ready            bool
}
type pendingCommandTerminal struct {
	state     domain.CommandState
	status    domain.StatusCode
	message   string
	uncertain bool
}

// Owns dispatch/notification state only. RouteTable remains the sole owner of
// physical-port occupancy and group locks; there is no second pending index.
type commandExecutionService struct {
	mu     sync.Mutex
	meta   map[string]pendingCommandMeta
	routes *domain.RouteTable
}

func newCommandExecutionService(routes *domain.RouteTable) *commandExecutionService {
	return &commandExecutionService{routes: routes, meta: map[string]pendingCommandMeta{}}
}
func (s *commandExecutionService) register(id string, meta pendingCommandMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.routes.HasCommand(id) {
		return
	}
	s.meta[id] = meta
}

func (s *commandExecutionService) readyQueued(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta, ok := s.meta[id]; ok {
		meta.ready = true
		s.meta[id] = meta
	}
}

func (s *commandExecutionService) claimQueued(source, port string, expectedID ...string) (domain.PendingCommand, queuedCommandDispatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head, ok := s.routes.QueueHead(source, port)
	if !ok {
		return domain.PendingCommand{}, queuedCommandDispatch{}, false
	}
	// A Write caller may synchronously dispatch only its own command. Otherwise
	// concurrent writers could receive another command's admission failure.
	if len(expectedID) != 0 && head.ID != expectedID[0] {
		return domain.PendingCommand{}, queuedCommandDispatch{}, false
	}
	meta, ok := s.meta[head.ID]
	if !ok || !meta.ready || meta.queued == nil {
		return domain.PendingCommand{}, queuedCommandDispatch{}, false
	}
	command, err := s.routes.ActivateQueued(head.ID)
	if err != nil {
		return domain.PendingCommand{}, queuedCommandDispatch{}, false
	}
	dispatch := *meta.queued
	meta.queued = nil
	s.meta[head.ID] = meta
	return command, dispatch, true
}

// Rehydrate only the completion coordinator, never a dispatch or an old RPC
// waiter. Correlated evidence can finish this reservation after reconnect;
// uncorrelated legacy ACKs from the new connection must remain ineligible.
func (s *commandExecutionService) restoreTracked(detail domain.CommandDetail, now time.Time, maxAge time.Duration) bool {
	command, trace := detail.Command, detail.Trace
	if command.State != domain.CommandWaiting || !command.FinishedAt.IsZero() || command.StartedAt.IsZero() ||
		maxAge <= 0 || !command.StartedAt.After(now.Add(-maxAge)) ||
		(command.Kind != "Write" && command.Kind != "Call") || trace == nil || trace.Protocol != "tracked" ||
		trace.GatewayInstanceID == "" || trace.BootID == "" || trace.PhysicalPort == "" ||
		trace.RequestHash != domain.CommandRequestHash(trace.PhysicalPort, command.Request) {
		return false
	}
	pending, exists := s.routes.PendingCommand(command.SourceID, command.Port)
	if !exists || pending.ID != command.ID {
		return false
	}
	s.register(command.ID, pendingCommandMeta{tracked: true, armed: true})
	return true
}

func (s *commandExecutionService) markTracked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.meta[id]
	if !ok {
		return false
	}
	meta.tracked = true
	meta.terminal = nil
	s.meta[id] = meta
	return true
}

// Called inside the lower adapter's subscription-delivery fence. Notifications
// delivered before its pre-write Read belong to the old baseline, even if they
// arrived after BeginCommand. Do not let them survive as provisional terminals.
func (s *commandExecutionService) resetLegacyBaseline(id string, sequence uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.meta[id]
	if !ok || meta.tracked || meta.armed {
		return false
	}
	meta.baselineSequence, meta.terminal = sequence, nil
	s.meta[id] = meta
	return true
}

// True asks the coordinator to finish an asynchronous Write outside the lock.
// Method results use a bounded channel; duplicates cannot block the observer.
func (s *commandExecutionService) observe(id string, terminal pendingCommandTerminal, tracked bool, epoch, sequence uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.meta[id]
	if !ok || meta.tracked != tracked || meta.terminal != nil {
		return false
	}
	if !tracked && (epoch != meta.epoch || sequence <= meta.baselineSequence || (epoch>>32 != 0 && epoch>>32 != meta.generation)) {
		return false
	}
	meta.terminal = &terminal
	s.meta[id] = meta
	if !meta.armed {
		return false
	}
	if meta.done != nil {
		select {
		case meta.done <- terminal:
		default:
		}
		return false
	}
	return true
}
func (s *commandExecutionService) arm(id string) (*pendingCommandTerminal, <-chan pendingCommandTerminal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.meta[id]
	if !ok {
		return nil, nil
	}
	meta.armed = true
	s.meta[id] = meta
	if meta.terminal == nil {
		return nil, meta.done
	}
	value := *meta.terminal
	return &value, meta.done
}
func (s *commandExecutionService) finish(id string, state domain.CommandState, status domain.StatusCode, message string, beforeRelease ...func(domain.PendingCommand)) (domain.PendingCommand, []domain.SessionRouteChange, []domain.PendingCommand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(id, state, status, message, beforeRelease...)
}

func (s *commandExecutionService) cancelQueued(id, reason string, publish func(domain.PendingCommand)) (domain.PendingCommand, []domain.SessionRouteChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Claim and cancellation use the same lock: never label an already
	// dispatched command "not sent" merely because it was in an old snapshot.
	if !s.routes.IsQueued(id) {
		return domain.PendingCommand{}, nil, domain.ErrNotFound
	}
	command, changes, _, err := s.finishLocked(id, domain.CommandFailed, domain.StatusBadNoCommunication, reason, publish)
	return command, changes, err
}

func (s *commandExecutionService) finishLocked(id string, state domain.CommandState, status domain.StatusCode, message string, beforeRelease ...func(domain.PendingCommand)) (domain.PendingCommand, []domain.SessionRouteChange, []domain.PendingCommand, error) {
	command, err := s.routes.PrepareCommandFinish(id, state, status, message, time.Now().UTC())
	if err != nil {
		return command, nil, nil, err
	}
	// Publish the immutable terminal result before another command can replace
	// this Session/node history. Callbacks must not perform network or disk I/O.
	for _, publish := range beforeRelease {
		publish(command)
	}
	meta := s.meta[id]
	cancelTail := (meta.terminal == nil || meta.terminal.uncertain) && state != domain.CommandReceived && !s.routes.IsQueued(id)
	changes, cancelled, err := s.routes.CommitCommandFinishWithQueue(id, cancelTail)
	if err == nil {
		delete(s.meta, id)
		for _, queued := range cancelled {
			delete(s.meta, queued.ID)
		}
	}
	return command, changes, cancelled, err
}
