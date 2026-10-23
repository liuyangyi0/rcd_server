package app

import (
	"context"
	"errors"
	"fmt"
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
	"strconv"
	"sync"
	"time"
)

// Owns capability/sync state independently of Runtime's sample/catalog lock.
type commandTrackingService struct {
	repo          *store.Repository
	managerID     string
	mu            sync.Mutex
	capabilities  map[string]commandCapability
	errors        map[string]string
	wake          chan struct{}
	audit         *commandAuditService
	dirtySources  map[string]bool
	nextPoll      map[string]time.Time
	inFlight      map[string]bool
	pendingFlight map[string]bool
	historyErrors map[string]error
	pendingErrors map[string]error
	backfillAfter map[string]string
}
type commandCapability struct {
	epoch     uint64
	info      domain.CommandTrackingHealth
	supported bool
	checked   time.Time
}

const commandSyncTimeout = 2 * time.Second

func newCommandTrackingService(repo *store.Repository, id string) *commandTrackingService {
	return &commandTrackingService{repo: repo, managerID: id, capabilities: map[string]commandCapability{}, errors: map[string]string{}, wake: make(chan struct{}, 1), audit: newCommandAuditService(repo, id), dirtySources: map[string]bool{}, nextPoll: map[string]time.Time{}, inFlight: map[string]bool{}, pendingFlight: map[string]bool{}, historyErrors: map[string]error{}, pendingErrors: map[string]error{}, backfillAfter: map[string]string{}}
}
func (s *commandTrackingService) notify(source string) {
	s.mu.Lock()
	s.dirtySources[source] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *commandTrackingService) beginSync(source string, epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[source] {
		return false
	}
	if !s.dirtySources[source] && s.capabilities[source].epoch == epoch && time.Now().Before(s.nextPoll[source]) {
		return false
	}
	s.inFlight[source] = true
	delete(s.dirtySources, source)
	s.nextPoll[source] = time.Now().Add(5 * time.Second)
	return true
}

func (s *commandTrackingService) beginPendingSync(source string) bool {
	pending := false
	for _, detail := range s.audit.pending(source) {
		if detail.Trace != nil && detail.Trace.Protocol == "tracked" {
			pending = true
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingFlight[source] {
		return false
	}
	if !pending {
		delete(s.pendingErrors, source)
		s.updateSyncErrorLocked(source)
		return false
	}
	s.pendingFlight[source] = true
	return true
}

func (s *commandTrackingService) endSync(source string, pending bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flight, failures := s.inFlight, s.historyErrors
	if pending {
		flight, failures = s.pendingFlight, s.pendingErrors
	}
	delete(flight, source)
	if err != nil {
		failures[source] = err
	} else {
		delete(failures, source)
	}
	s.updateSyncErrorLocked(source)
}

func (s *commandTrackingService) updateSyncErrorLocked(source string) {
	if err := errors.Join(s.pendingErrors[source], s.historyErrors[source]); err != nil {
		s.errors[source] = err.Error()
	} else {
		delete(s.errors, source)
	}
}

func (s *commandTrackingService) prune(sources []domain.Source) {
	active := map[string]bool{}
	for _, source := range sources {
		active[source.ID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for source := range s.nextPoll {
		if !active[source] && !s.inFlight[source] && !s.pendingFlight[source] {
			delete(s.errors, source)
			delete(s.historyErrors, source)
			delete(s.pendingErrors, source)
			delete(s.capabilities, source)
			delete(s.nextPoll, source)
			delete(s.dirtySources, source)
		}
	}
}
func (s *commandTrackingService) info(ctx context.Context, source string, target lowerCommandTarget) (domain.CommandTrackingHealth, bool, error) {
	client, ok := target.client.(opc.TrackedCommandClient)
	if !ok {
		return domain.CommandTrackingHealth{}, false, nil
	}
	s.mu.Lock()
	cached, exists := s.capabilities[source]
	s.mu.Unlock()
	if exists && cached.epoch == target.globalEpoch && time.Since(cached.checked) < 5*time.Second {
		return cached.info, cached.supported, nil
	}
	info, supported, err := client.CommandTrackingInfo(ctx, target.localEpoch)
	if err == nil {
		s.mu.Lock()
		s.capabilities[source] = commandCapability{target.globalEpoch, info, supported, time.Now()}
		s.mu.Unlock()
	}
	return info, supported, err
}

func (r *Runtime) dispatchCommand(ctx context.Context, command domain.PendingCommand, route commandRoute, target lowerCommandTarget, payload string) (domain.StatusCode, error) {
	info, supported, err := r.tracking.info(ctx, command.SourceID, target)
	if err != nil {
		return domain.StatusBadNoCommunication, err
	} // unknown support is not permission to send twice
	if !supported {
		client, ok := target.client.(opc.LegacyCommandClient)
		if !ok {
			return domain.StatusBadNotSupported, errors.New("lower adapter cannot fence legacy ACKs; command not sent")
		}
		return client.WriteLegacyCommandAtEpoch(ctx, route.SendKey, payload, target.localEpoch, func() bool {
			r.mu.RLock()
			current := r.statuses[command.SourceID].Epoch == target.globalEpoch && r.connections.current(command.SourceID, target.generation)
			sequence := r.ackSequences[command.SourceID+"\x00"+route.Port]
			r.mu.RUnlock()
			return current && r.commands.resetLegacyBaseline(command.ID, sequence)
		})
	}
	physical := route.PhysicalPort
	if physical == "" {
		physical = r.commandPhysicalPort(route)
	}
	trace := domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: info.GatewayInstanceID, BootID: info.BootID, PhysicalPort: physical, RequestHash: domain.CommandRequestHash(physical, payload)}
	r.tracking.audit.trace(command, trace)
	if !r.commands.markTracked(command.ID) {
		return domain.StatusBadTimeout, fmt.Errorf("command ended before dispatch")
	}
	// Queueing, capability discovery and audit I/O have already consumed part
	// of this command's budget. Never give the lower a fresh full timeout: it
	// could otherwise start a serial write after our original deadline expired.
	timeout, err := r.trackedCommandBudget(ctx, command)
	if err != nil {
		return domain.StatusBadTimeout, err
	}
	request := map[string]any{"action": "submit_command", "schema_version": 1, "manager_instance_id": r.tracking.managerID, "command_id": command.ID,
		"port_id": physical, "hex_data": payload, "expected_gateway_instance_id": info.GatewayInstanceID, "expected_boot_id": info.BootID, "timeout_ms": timeout.Milliseconds()}
	value, err := target.client.(opc.TrackedCommandClient).SubmitTrackedCommand(ctx, target.localEpoch, request)
	r.tracking.notify(command.SourceID)
	if err != nil {
		return domain.StatusBadNoCommunication, err
	}
	if value.CommandID != command.ID {
		return domain.StatusBadUnexpectedError, fmt.Errorf("lower returned a different command ID")
	}
	if err = r.tracking.audit.evidence(ctx, command.SourceID, value); err != nil {
		return domain.StatusBadUnexpectedError, err
	}
	r.acceptCommandEvidence(command.SourceID, value)
	if !value.Accepted {
		status := value.StatusCode
		if status.IsGood() {
			status = domain.StatusBadUnexpectedError
		}
		return status, fmt.Errorf("lower rejected command: %s", value.Error)
	}
	return domain.StatusGood, nil
}

func (r *Runtime) trackedCommandBudget(ctx context.Context, command domain.PendingCommand) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	remaining := r.options.CommandTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if budget := time.Until(deadline); budget < remaining {
			remaining = budget
		}
	}
	if budget := time.Until(command.StartedAt.Add(r.options.CommandPendingTTL)); budget < remaining {
		remaining = budget
	}
	// The lower protocol accepts integer milliseconds and clamps zero to one.
	// Reject a sub-millisecond remainder instead of rounding it up there.
	if remaining < time.Millisecond {
		return 0, context.DeadlineExceeded
	}
	return remaining, nil
}

func (r *Runtime) commandPhysicalPort(route commandRoute) string {
	physical := route.Port
	r.mu.RLock()
	if topology := r.catalogs[route.SourceID].Topology; topology != nil {
		for _, p := range topology.Ports {
			if p.SendNodeID == route.SendKey.Identifier {
				physical = p.PortID
				break
			}
		}
	}
	r.mu.RUnlock()
	return physical
}

func (r *Runtime) acceptCommandEvidence(source string, v domain.TrackedCommand) {
	if !v.Terminal() {
		return
	}
	command, exists := r.routes.PendingCommand(source, r.commandPort(v.CommandID))
	if !exists || command.ID != v.CommandID {
		return
	} // late evidence is audit only
	terminal := pendingCommandTerminal{state: domain.CommandFailed, status: domain.StatusBadUnexpectedError, message: v.Error}
	switch v.Phase {
	case "AckReceived":
		terminal = pendingCommandTerminal{state: domain.CommandReceived, status: domain.StatusGood}
	case "Expired":
		terminal.state = domain.CommandTimedOut
		terminal.status = domain.StatusBadTimeout
		// The lower watchdog can expire a command while the serial worker
		// still owns the port. Timer evidence does not prove I/O has drained.
		terminal.uncertain = true
	case "Failed":
		if v.Error == "ack_timeout" {
			terminal.state = domain.CommandTimedOut
			terminal.status = domain.StatusBadTimeout
		}
	case "Unknown":
		terminal.message = "下级结果未知：" + v.Error
		terminal.uncertain = true
	}
	if r.commands.observe(v.CommandID, terminal, true, 0, 0) {
		r.finishCommand(v.CommandID, terminal.state, terminal.status, terminal.message)
	}
}
func (r *Runtime) commandPort(id string) string {
	detail, err := r.tracking.audit.detail(context.Background(), id)
	if err != nil {
		return ""
	}
	return detail.Command.Port
}

func (r *Runtime) commandTrackingWorker() {
	defer r.wg.Done()
	ctx := r.runContext
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	schedule := func(source string, target lowerCommandTarget, pending bool) {
		if pending {
			if !r.tracking.beginPendingSync(source) {
				return
			}
		} else if !r.tracking.beginSync(source, target.globalEpoch) {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.syncCommandEvidencePath(ctx, source, target, pending)
			r.tracking.endSync(source, pending, err)
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.tracking.wake:
		}
		r.tracking.audit.flush()
		sources := r.routes.Sources()
		r.tracking.prune(sources)
		for _, source := range sources {
			target, ok := r.captureLowerCommandTarget(source.ID, "", false)
			if !ok {
				continue
			}
			if _, ok := target.client.(opc.TrackedCommandClient); !ok {
				continue
			}
			// Independent slots matter even if the command arrives AFTER a slow
			// history read has begun. Revision notifications and the 250 ms poll
			// must still be able to query its live snapshot before Call times out.
			schedule(source.ID, target, true)
			schedule(source.ID, target, false)
		}
	}
}
func (r *Runtime) syncCommandEvidence(ctx context.Context, source string, target lowerCommandTarget) error {
	return errors.Join(r.syncCommandEvidencePath(ctx, source, target, true), r.syncCommandEvidencePath(ctx, source, target, false))
}

func (r *Runtime) syncCommandEvidencePath(ctx context.Context, source string, target lowerCommandTarget, pending bool) error {
	infoCtx, cancelInfo := context.WithTimeout(ctx, commandSyncTimeout)
	info, supported, err := r.tracking.info(infoCtx, source, target)
	cancelInfo()
	if err != nil || !supported {
		return err
	}
	// Capability and I/O budgets are local to this lane. History persistence
	// errors remain visible without preventing live command completion.
	syncCtx, cancel := context.WithTimeout(ctx, commandSyncTimeout)
	defer cancel()
	if pending {
		return r.syncPendingCommandEvidence(syncCtx, source, target, info.GatewayInstanceID)
	}
	return r.syncCommandHistory(syncCtx, source, target, info.GatewayInstanceID)
}

func (r *Runtime) syncPendingCommandEvidence(ctx context.Context, source string, target lowerCommandTarget, gateway string) error {
	var syncErr error
	changed := false
	for _, detail := range r.tracking.audit.pending(source) {
		if err := ctx.Err(); err != nil {
			syncErr = errors.Join(syncErr, err)
			break
		}
		if detail.Trace == nil || detail.Trace.Protocol != "tracked" || detail.Trace.GatewayInstanceID != gateway {
			continue
		}
		value, err := target.client.(opc.TrackedCommandClient).GetTrackedCommand(ctx, target.localEpoch, r.tracking.managerID, detail.Command.ID)
		if err != nil {
			syncErr = errors.Join(syncErr, err)
			continue
		}
		if value == nil {
			continue
		}
		if value.CommandID != detail.Command.ID {
			syncErr = errors.Join(syncErr, fmt.Errorf("lower returned a different pending command ID"))
			continue
		}
		if err := r.tracking.audit.evidence(ctx, source, *value); err != nil {
			syncErr = errors.Join(syncErr, err)
			continue
		}
		changed = changed || detail.Trace.Snapshot == nil || value.Revision > detail.Trace.Snapshot.Revision
		r.acceptCommandEvidence(source, *value)
	}
	if changed {
		r.broker.Publish(api.Event{Type: "command.changed", SourceID: source})
	}
	return syncErr
}

func (r *Runtime) syncCommandHistory(ctx context.Context, source string, target lowerCommandTarget, gateway string) error {
	cursor, cursorErr := r.repo.CommandTraceCursor(ctx, source, gateway)
	if cursorErr != nil {
		return cursorErr
	}
	changed := false
	batch, batchErr := target.client.(opc.TrackedCommandClient).TrackedCommandEvents(ctx, target.localEpoch, r.tracking.managerID, cursor)
	if batchErr == nil {
		if batch.Health.GatewayInstanceID != gateway {
			return fmt.Errorf("lower journal instance changed")
		}
		next, parseErr := strconv.ParseUint(batch.NextCursor, 10, 64)
		if parseErr != nil || next < cursor {
			return fmt.Errorf("invalid command event cursor")
		}
		previous := cursor
		for _, v := range batch.Items {
			if v.Revision <= previous || v.Revision > next {
				return fmt.Errorf("nonmonotonic command event page")
			}
			previous = v.Revision
		}
		if batch.HistoryGap || batch.Health.HistoryGap {
			r.tracking.audit.markHistoryGap(source, gateway)
		}
		for _, v := range batch.Items {
			detail, readErr := r.tracking.audit.detail(ctx, v.CommandID)
			if errors.Is(readErr, domain.ErrNotFound) {
				continue
			}
			if readErr != nil {
				return readErr
			}
			if detail.Command.SourceID != source || detail.Trace == nil {
				continue
			} // shared gateway or no durable pinned identity
			if evidenceErr := r.tracking.audit.evidence(ctx, source, v); evidenceErr != nil {
				return evidenceErr
			}
			r.acceptCommandEvidence(source, v)
		}
		batchErr = r.tracking.audit.commitPage(ctx, source, gateway, batch.Items, &next, batch.HistoryGap)
		changed = batchErr == nil && next > cursor && len(batch.Items) > 0
	}
	// Recovery also covers already-finished commands and survives manager
	// restarts. Pending-only polling cannot repair a NULL persisted snapshot.
	if batchErr == nil {
		recovered, recoveryErr := r.backfillCommandEvidence(ctx, source, target, gateway)
		changed = changed || recovered
		if recoveryErr != nil {
			batchErr = recoveryErr
		}
	}
	if changed {
		r.broker.Publish(api.Event{Type: "command.changed", SourceID: source})
	}
	if batchErr != nil {
		return batchErr
	}
	if batch.Health.AuditDegraded {
		return fmt.Errorf("命令审计降级，控制继续执行：%s", batch.Health.AuditError)
	}
	if batch.HistoryGap || batch.Health.HistoryGap {
		return fmt.Errorf("下级命令审计存在记录缺口，已继续补查现存记录")
	}
	return nil
}

func (r *Runtime) backfillCommandEvidence(ctx context.Context, source string, target lowerCommandTarget, gateway string) (bool, error) {
	key := source + "\x00" + gateway
	r.tracking.mu.Lock()
	before := r.tracking.backfillAfter[key]
	r.tracking.mu.Unlock()
	ids, err := r.repo.IncompleteCommandIDs(ctx, gateway, before, 20)
	if err != nil {
		return false, err
	}
	changed := false
	for _, id := range ids {
		// A source endpoint may have been corrected since this command. Recover
		// through a connection to its original gateway, preserving its old logical
		// source and all pinned identity fields; never follow the source's new IP.
		detail, err := r.tracking.audit.detail(ctx, id)
		if err != nil {
			return changed, err
		}
		if detail.Trace == nil || detail.Trace.GatewayInstanceID != gateway {
			return changed, fmt.Errorf("recovery gateway mismatch")
		}
		owner := detail.Command.SourceID
		v, err := target.client.(opc.TrackedCommandClient).GetTrackedCommand(ctx, target.localEpoch, r.tracking.managerID, id)
		if err != nil {
			return changed, err
		}
		if v != nil {
			if v.CommandID != id {
				return changed, fmt.Errorf("lower returned a different recovery command ID")
			}
			if err := r.tracking.audit.evidence(ctx, owner, *v); err != nil {
				return changed, err
			}
			if err := r.tracking.audit.commitPage(ctx, owner, gateway, []domain.TrackedCommand{*v}, nil, false); err != nil {
				return changed, err
			}
			changed = true
		}
		r.tracking.mu.Lock()
		r.tracking.backfillAfter[key] = id
		r.tracking.mu.Unlock()
	}
	if len(ids) < 20 {
		r.tracking.mu.Lock()
		delete(r.tracking.backfillAfter, key)
		r.tracking.mu.Unlock()
	}
	return changed, nil
}

func (r *Runtime) CommandDetail(ctx context.Context, id string) (domain.CommandDetail, error) {
	return r.tracking.audit.detail(ctx, id)
}
func (r *Runtime) CommandEvents(ctx context.Context, id string, limit, offset int) ([]domain.TrackedCommand, int, error) {
	return r.repo.CommandTraceEvents(ctx, id, limit, offset)
}
