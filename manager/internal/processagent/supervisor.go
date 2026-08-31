package processagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"rcd_manager/internal/domain"
)

const retainedOperations = 256

type stateCommitError struct{ err error }

func (e *stateCommitError) Error() string { return e.err.Error() }
func (e *stateCommitError) Unwrap() error { return e.err }

type savedOperation struct {
	Request   domain.ProcessActionRequest `json:"request"`
	Operation domain.ProcessOperation     `json:"operation"`
}

type durableState struct {
	SchemaVersion int                    `json:"schema_version"`
	InstanceID    string                 `json:"instance_id"`
	Counter       uint64                 `json:"counter"`
	DesiredState  string                 `json:"desired_state"`
	RestartCount  uint64                 `json:"restart_count"`
	Operations    []savedOperation       `json:"operations"`
	LastSnapshot  domain.ProcessSnapshot `json:"last_snapshot"`
}

type ownedChild struct {
	process *os.Process
	done    chan struct{}
	err     error // written before done closes
}

// Supervisor owns the *os.Process returned by its own exec.Cmd.Start. It never
// accepts a PID from HTTP and never adopts or kills an unrelated process.
type Supervisor struct {
	mu         sync.Mutex
	cfg        Config
	state      durableState
	snapshot   domain.ProcessSnapshot
	bootID     string
	child      *ownedChild
	lock       *os.File
	output     *rotatingLog
	events     *rotatingLog
	logger     *slog.Logger
	jobs       chan domain.ProcessActionRequest
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	closing    bool
	storageErr error
	retryAt    time.Time
	failures   int
}

func New(cfg Config) (*Supervisor, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	var err error
	cfg, err = cfg.normalized()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(cfg.DataDir, "agent.lock"))
	if err != nil {
		return nil, err
	}
	output, err := openLog(cfg.DataDir, "process.log")
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	events, err := openLog(cfg.DataDir, "agent.log")
	if err != nil {
		_ = output.Close()
		_ = lock.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{cfg: cfg, bootID: newID(), lock: lock, output: output, events: events, logger: slog.New(slog.NewJSONHandler(events, nil)), jobs: make(chan domain.ProcessActionRequest, 1), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if err := s.load(); err != nil {
		cancel()
		_ = output.Close()
		_ = events.Close()
		_ = lock.Close()
		return nil, err
	}
	// Reserve the restore operation before exposing HTTP so a simultaneous user
	// request cannot race a second startup. Persisted manual stop wins AutoStart.
	if s.state.DesiredState == "running" {
		r := domain.ProcessActionRequest{RequestID: "boot." + s.bootID, InstanceID: cfg.InstanceID, Action: "start", ExpectedRevision: s.snapshot.Revision}
		if _, err := s.Submit(r); err != nil {
			cancel()
			_ = output.Close()
			_ = events.Close()
			_ = lock.Close()
			return nil, err
		}
	}
	go s.loop()
	return s, nil
}

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("operating system random source unavailable")
	}
	return hex.EncodeToString(value[:])
}

func (s *Supervisor) load() error {
	path := filepath.Join(s.cfg.DataDir, "process-state.json")
	s.state = durableState{SchemaVersion: 1, InstanceID: s.cfg.InstanceID, DesiredState: "stopped", Operations: []savedOperation{}}
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		dec := json.NewDecoder(io.LimitReader(f, 8<<20))
		if err := dec.Decode(&s.state); err != nil {
			return fmt.Errorf("read process-agent state; refusing to overwrite or guess desired state: %w", err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return errors.New("unexpected trailing process-agent state data")
		}
		if s.state.SchemaVersion != 1 || s.state.InstanceID != s.cfg.InstanceID || (s.state.DesiredState != "running" && s.state.DesiredState != "stopped") || len(s.state.Operations) > retainedOperations {
			return errors.New("process-agent state schema, identity, or capacity mismatch")
		}
		seen := map[string]bool{}
		for i := range s.state.Operations {
			item := &s.state.Operations[i]
			if item.Request.Validate() != nil || item.Request.InstanceID != s.cfg.InstanceID || seen[item.Request.RequestID] || item.Operation.Validate(item.Request) != nil {
				return errors.New("invalid or duplicate durable process operation")
			}
			seen[item.Request.RequestID] = true
			if !item.Operation.Terminal() {
				// Do not replay a restart after an agent crash. The Linux parent-
				// death contract stops its old owned child; a separate boot action
				// restores desired state and remains visible in the journal.
				item.Operation.State = "failed"
				item.Operation.ErrorCode = "agent_restarted"
				item.Operation.Error = "守护进程重新启动，上次作业未确认；请核对已恢复的进程状态"
				item.Operation.UpdatedAt = time.Now().UTC()
			}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if s.cfg.AutoStart {
			s.state.DesiredState = "running"
		}
	} else {
		return err
	}
	s.snapshot = domain.ProcessSnapshot{SchemaVersion: 1, InstanceID: s.cfg.InstanceID, State: "stopped", DesiredState: s.state.DesiredState, Executable: s.cfg.Executable, WorkDir: s.cfg.WorkDir, RestartPolicy: s.cfg.RestartPolicy, RestartCount: s.state.RestartCount, ExitCode: s.state.LastSnapshot.ExitCode, LastExitAt: s.state.LastSnapshot.LastExitAt, LastStopForced: s.state.LastSnapshot.LastStopForced}
	s.bumpLocked()
	return s.persistLocked()
}

func (s *Supervisor) bumpLocked() {
	s.state.Counter++
	s.snapshot.Revision = fmt.Sprintf("%s.%d", s.bootID, s.state.Counter)
	s.snapshot.DesiredState = s.state.DesiredState
	s.snapshot.RestartCount = s.state.RestartCount
}

func (s *Supervisor) persistLocked() error {
	s.state.LastSnapshot = s.snapshot
	data, err := json.Marshal(s.state)
	if err == nil {
		err = atomicState(filepath.Join(s.cfg.DataDir, "process-state.json"), data)
	}
	if err != nil {
		s.storageErr = err
		s.snapshot.LastError = "守护状态保存失败，已禁止新的进程操作: " + err.Error()
		s.logger.Error("process state persistence failed", "instance_id", s.cfg.InstanceID, "error", err)
	}
	return err
}

func atomicState(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".process-state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o640); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return &stateCommitError{err}
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return &stateCommitError{err}
	}
	return nil
}

func (s *Supervisor) snapshotLocked() domain.ProcessSnapshot {
	value := s.snapshot
	if value.StartedAt != nil && value.PID > 0 {
		value.UptimeSeconds = max(0, int64(time.Since(*value.StartedAt)/time.Second))
	}
	return value
}

func (s *Supervisor) Snapshot() domain.ProcessSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func cloneOperation(value domain.ProcessOperation) domain.ProcessOperation {
	if value.Snapshot != nil {
		copy := *value.Snapshot
		value.Snapshot = &copy
	}
	return value
}

func (s *Supervisor) findLocked(id string) *savedOperation {
	for i := len(s.state.Operations) - 1; i >= 0; i-- {
		if s.state.Operations[i].Request.RequestID == id {
			return &s.state.Operations[i]
		}
	}
	return nil
}

func (s *Supervisor) Operation(id string) (domain.ProcessOperation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item := s.findLocked(id); item != nil {
		return cloneOperation(item.Operation), true
	}
	return domain.ProcessOperation{}, false
}

func (s *Supervisor) History() []domain.ProcessOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]domain.ProcessOperation, 0, min(100, len(s.state.Operations)))
	for i := len(s.state.Operations) - 1; i >= 0 && len(items) < 100; i-- {
		items = append(items, cloneOperation(s.state.Operations[i].Operation))
	}
	return items
}

func (s *Supervisor) addLocked(req domain.ProcessActionRequest, state string) *savedOperation {
	if len(s.state.Operations) == retainedOperations {
		// Resolution tombstones may arrive while a slow operation is pending;
		// evict only a terminal entry, never that pending operation.
		for i := range s.state.Operations {
			if s.state.Operations[i].Operation.Terminal() {
				copy(s.state.Operations[i:], s.state.Operations[i+1:])
				s.state.Operations = s.state.Operations[:len(s.state.Operations)-1]
				break
			}
		}
	}
	now := time.Now().UTC()
	s.state.Operations = append(s.state.Operations, savedOperation{Request: req, Operation: domain.ProcessOperation{RequestID: req.RequestID, InstanceID: req.InstanceID, Action: req.Action, State: state, CreatedAt: now, UpdatedAt: now}})
	return &s.state.Operations[len(s.state.Operations)-1]
}

func (s *Supervisor) Submit(req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	if err := req.Validate(); err != nil {
		return domain.ProcessOperation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.InstanceID != s.cfg.InstanceID {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "not_found", Message: "守护实例标识不匹配"}
	}
	if item := s.findLocked(req.RequestID); item != nil {
		if item.Request != req {
			return domain.ProcessOperation{}, &domain.OperationError{Code: "conflict", Message: "同一作业编号不能用于不同操作"}
		}
		return cloneOperation(item.Operation), nil
	}
	if s.closing || s.storageErr != nil {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "operation_failed", Message: "守护正在关闭或持久化故障，不能接收操作"}
	}
	if req.ExpectedRevision != s.snapshot.Revision {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "conflict", Message: "进程状态版本已变化，请刷新后确认", CurrentRevision: s.snapshot.Revision}
	}
	if s.snapshot.PendingRequestID != "" {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "busy", Message: "该下级已有进行中的进程操作"}
	}
	previousDesired := s.state.DesiredState
	previousSnapshot := s.snapshot
	item := s.addLocked(req, "accepted")
	s.state.DesiredState = "running"
	if req.Action == "stop" {
		s.state.DesiredState = "stopped"
	}
	s.snapshot.PendingRequestID = req.RequestID
	s.retryAt = time.Time{}
	s.bumpLocked()
	snap := s.snapshotLocked()
	item.Operation.Snapshot = &snap
	if err := s.persistLocked(); err != nil {
		var committed *stateCommitError
		if errors.As(err, &committed) {
			item.Operation.State, item.Operation.ErrorCode, item.Operation.Error = "unknown", "persistence_failed", "作业登记的持久化未确认，未派发进程操作；请核对守护状态，不能重复提交"
		} else {
			// The durable rename never happened: a rejected start must not
			// become desired=running on a later Close/agent restart.
			lastError := s.snapshot.LastError
			s.state.DesiredState, s.snapshot = previousDesired, previousSnapshot
			s.snapshot.LastError = lastError
			s.bumpLocked()
			item.Operation.State, item.Operation.ErrorCode, item.Operation.Error = "failed", "persistence_failed", "作业登记失败，未派发进程操作；请核对守护存储"
			snap := s.snapshotLocked()
			item.Operation.Snapshot = &snap
		}
		return domain.ProcessOperation{}, &domain.OperationError{Code: "persistence_failed", Message: item.Operation.Error}
	}
	s.jobs <- req // capacity one, admitted only when no job is pending
	s.logger.Info("process operation accepted", "instance_id", req.InstanceID, "request_id", req.RequestID, "action", req.Action)
	return cloneOperation(item.Operation), nil
}

// Resolve closes a transport-uncertainty window without retrying the action.
// An absent request receives a durable terminal tombstone, so an in-flight late
// POST with that same ID cannot start or restart the child afterwards.
func (s *Supervisor) Resolve(req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	if err := req.Validate(); err != nil {
		return domain.ProcessOperation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.InstanceID != s.cfg.InstanceID {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "not_found", Message: "守护实例标识不匹配"}
	}
	if item := s.findLocked(req.RequestID); item != nil {
		if item.Request != req {
			return domain.ProcessOperation{}, &domain.OperationError{Code: "conflict", Message: "作业编号与原始请求不匹配"}
		}
		return cloneOperation(item.Operation), nil
	}
	if s.closing || s.storageErr != nil {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "operation_failed", Message: "守护不可写，暂时不能封存未确认作业"}
	}
	item := s.addLocked(req, "failed")
	item.Operation.ErrorCode = "unconfirmed_cancelled"
	item.Operation.Error = "未找到本次作业登记，已封存该编号以阻止迟到执行；请核对当前进程状态"
	// Even after bounded history evicts this tombstone, its original revision
	// must stay stale. Otherwise an arbitrarily late POST could execute again.
	s.bumpLocked()
	snap := s.snapshotLocked()
	item.Operation.Snapshot = &snap
	if err := s.persistLocked(); err != nil {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "persistence_failed", Message: "未确认作业封存失败"}
	}
	return cloneOperation(item.Operation), nil
}

func (s *Supervisor) loop() {
	defer close(s.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			_, err := s.stopOwned()
			s.mu.Lock()
			if err != nil {
				s.snapshot.LastError = "守护关闭时下级退出未确认: " + err.Error()
			}
			s.bumpLocked()
			_ = s.persistLocked()
			s.mu.Unlock()
			return
		case req := <-s.jobs:
			s.perform(req)
		case <-ticker.C:
			s.observeExitAndRetry()
		}
	}
}

func (s *Supervisor) perform(req domain.ProcessActionRequest) {
	s.mu.Lock()
	item := s.findLocked(req.RequestID)
	item.Operation.State, item.Operation.UpdatedAt = "running", time.Now().UTC()
	s.snapshot.State = "starting"
	if req.Action == "stop" || req.Action == "restart" {
		s.snapshot.State = "stopping"
	}
	s.snapshot.LastError = ""
	s.snapshot.LastStopForced = false
	s.bumpLocked()
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		s.finish(req, false, err)
		return
	}
	s.mu.Unlock()
	var forced bool
	var err error
	if req.Action == "stop" || req.Action == "restart" {
		forced, err = s.stopOwned()
	}
	if err == nil && (req.Action == "start" || req.Action == "restart") {
		if req.Action == "restart" {
			s.mu.Lock()
			s.state.RestartCount++
			s.mu.Unlock()
		}
		err = s.startOwned()
	}
	s.finish(req, forced, err)
}

func (s *Supervisor) finish(req domain.ProcessActionRequest, forced bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.findLocked(req.RequestID)
	item.Operation.UpdatedAt = time.Now().UTC()
	forced = forced || s.snapshot.LastStopForced
	item.Operation.Forced = forced
	s.snapshot.PendingRequestID = ""
	s.snapshot.LastStopForced = forced
	if err == nil {
		item.Operation.State = "succeeded"
		s.snapshot.State = "running"
		if req.Action == "stop" {
			s.snapshot.State = "stopped"
		}
	} else {
		item.Operation.State, item.Operation.ErrorCode = "failed", "operation_failed"
		item.Operation.Error = boundedError(err)
		s.snapshot.State, s.snapshot.LastError = "failed", item.Operation.Error
		if s.cfg.RestartPolicy == "on-failure" && s.state.DesiredState == "running" {
			s.scheduleRetryLocked()
		}
	}
	s.bumpLocked()
	snap := s.snapshotLocked()
	item.Operation.Snapshot = &snap
	if persistErr := s.persistLocked(); persistErr != nil {
		// A process side effect may already have happened. Never report it as
		// confirmed success if its completion could not be durably recorded.
		item.Operation.State, item.Operation.ErrorCode = "unknown", "persistence_failed"
		item.Operation.Error = "进程操作已执行，但结果保存失败；请核对进程与守护存储"
	}
	s.logger.Info("process operation completed", "instance_id", req.InstanceID, "request_id", req.RequestID, "action", req.Action, "state", item.Operation.State, "pid", s.snapshot.PID, "forced", forced, "error", item.Operation.Error)
}

func boundedError(err error) string {
	value := strings.ToValidUTF8(err.Error(), "�")
	if len(value) > 8192 {
		value = strings.ToValidUTF8(value[:8192], "") + "…"
	}
	return value
}

func (s *Supervisor) startOwned() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.child != nil {
		select {
		case <-s.child.done:
			s.recordExitLocked(s.child)
		default:
			s.snapshot.State = "running"
			s.mu.Unlock()
			return nil // start is idempotent for an already owned, live process
		}
	}
	s.snapshot.State = "starting"
	s.bumpLocked()
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	cmd := exec.Command(s.cfg.Executable, s.cfg.Arguments...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = s.cfg.WorkDir, s.output, s.output
	cmd.WaitDelay = 2 * time.Second
	prepareChild(cmd)
	started := make(chan error, 1)
	child := &ownedChild{done: make(chan struct{})}
	go func() {
		// Linux Pdeathsig applies to the creating OS thread. Pin that thread
		// through Wait so normal Go thread retirement cannot kill the child.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		if err == nil {
			child.process = cmd.Process
		}
		started <- err
		if err == nil {
			child.err = cmd.Wait()
		} else {
			child.err = err
		}
		close(child.done)
	}()
	if err := <-started; err != nil {
		return fmt.Errorf("start configured lower: %w", err)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.child, s.snapshot.PID, s.snapshot.StartedAt = child, child.process.Pid, &now
	s.snapshot.ExitCode = nil
	s.bumpLocked()
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		_, _ = s.stopOwned()
		return err
	}
	s.logger.Info("owned lower started", "instance_id", s.cfg.InstanceID, "pid", child.process.Pid)
	grace := time.NewTimer(s.cfg.StartGrace)
	defer grace.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-child.done:
		s.mu.Lock()
		s.recordExitLocked(child)
		s.mu.Unlock()
		return fmt.Errorf("lower exited during startup: %v", child.err)
	case <-grace.C:
	}
	if s.cfg.ReadyAddress != "" {
		deadline := time.NewTimer(s.cfg.StartTimeout)
		defer deadline.Stop()
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		ready := false
		for !ready {
			conn, err := (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext(s.ctx, "tcp", s.cfg.ReadyAddress)
			if err == nil {
				_ = conn.Close()
				ready = true
				continue
			}
			select {
			case <-child.done:
				s.mu.Lock()
				s.recordExitLocked(child)
				s.mu.Unlock()
				return fmt.Errorf("lower exited before TCP readiness: %v", child.err)
			case <-s.ctx.Done():
				return s.ctx.Err()
			case <-deadline.C:
				_, _ = s.stopOwned()
				return fmt.Errorf("lower did not listen at %s within %s; inspect process output", s.cfg.ReadyAddress, s.cfg.StartTimeout)
			case <-tick.C:
			}
		}
	}
	select {
	case <-child.done:
		s.mu.Lock()
		s.recordExitLocked(child)
		s.mu.Unlock()
		return fmt.Errorf("lower exited at startup confirmation: %v", child.err)
	default:
		return nil
	}
}

func (s *Supervisor) recordExitLocked(child *ownedChild) {
	if s.child != child {
		return
	}
	code := 0
	if child.err != nil {
		code = -1
		var exitError *exec.ExitError
		if errors.As(child.err, &exitError) {
			code = exitError.ExitCode()
		}
	}
	now := time.Now().UTC()
	s.snapshot.PID, s.snapshot.StartedAt = 0, nil
	s.snapshot.ExitCode, s.snapshot.LastExitAt = &code, &now
	s.child = nil
}

func (s *Supervisor) stopOwned() (bool, error) {
	s.mu.Lock()
	child := s.child
	if child == nil {
		s.snapshot.State = "stopped"
		s.mu.Unlock()
		return false, nil
	}
	select {
	case <-child.done:
		s.recordExitLocked(child)
		s.snapshot.State = "stopped"
		s.mu.Unlock()
		return false, nil
	default:
	}
	s.snapshot.State = "stopping"
	s.bumpLocked()
	_ = s.persistLocked()
	s.mu.Unlock()
	if err := requestStop(child.process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return false, fmt.Errorf("signal owned lower: %w", err)
	}
	forced := false
	timer := time.NewTimer(s.cfg.StopTimeout)
	defer timer.Stop()
	select {
	case <-child.done:
	case <-timer.C:
		s.logger.Warn("lower grace period expired; killing owned child", "instance_id", s.cfg.InstanceID, "pid", child.process.Pid)
		if err := child.process.Kill(); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				return false, fmt.Errorf("kill owned lower: %w", err)
			}
		} else {
			forced = true
		}
		killTimer := time.NewTimer(5 * time.Second)
		defer killTimer.Stop()
		select {
		case <-child.done:
		case <-killTimer.C:
			return forced, errors.New("owned lower still has not exited after termination; no replacement was started")
		}
	}
	s.mu.Lock()
	s.recordExitLocked(child)
	s.snapshot.State, s.snapshot.LastStopForced = "stopped", forced
	s.mu.Unlock()
	return forced, nil
}

func (s *Supervisor) scheduleRetryLocked() {
	s.failures++
	delay := time.Second << min(s.failures-1, 5)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	s.retryAt = time.Now().Add(delay)
}

func (s *Supervisor) observeExitAndRetry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.storageErr != nil || s.snapshot.PendingRequestID != "" {
		return
	}
	if s.child != nil {
		select {
		case <-s.child.done:
			child := s.child
			s.recordExitLocked(child)
			s.snapshot.State = "failed"
			s.snapshot.LastError = fmt.Sprintf("下级进程意外退出，退出码 %d；请查看进程日志", *s.snapshot.ExitCode)
			if s.cfg.RestartPolicy == "on-failure" && s.state.DesiredState == "running" && *s.snapshot.ExitCode != 0 {
				s.scheduleRetryLocked()
			}
			s.bumpLocked()
			_ = s.persistLocked()
		default:
			if s.snapshot.StartedAt != nil && time.Since(*s.snapshot.StartedAt) > 5*time.Minute {
				s.failures = 0
			}
		}
	}
	if s.retryAt.IsZero() || time.Now().Before(s.retryAt) || s.state.DesiredState != "running" || s.storageErr != nil {
		return
	}
	s.retryAt = time.Time{}
	req := domain.ProcessActionRequest{RequestID: "retry." + newID(), InstanceID: s.cfg.InstanceID, Action: "start", ExpectedRevision: s.snapshot.Revision}
	item := s.addLocked(req, "accepted")
	s.snapshot.PendingRequestID = req.RequestID
	s.state.RestartCount++
	s.bumpLocked()
	snap := s.snapshotLocked()
	item.Operation.Snapshot = &snap
	if err := s.persistLocked(); err != nil {
		return
	}
	s.jobs <- req
}

func (s *Supervisor) Log(limit int) (domain.ProcessLog, error) {
	if limit < 1 || limit > domain.MaxProcessLogBytes {
		return domain.ProcessLog{}, &domain.OperationError{Code: "invalid_request", Message: "日志上限必须为 1–65536 字节"}
	}
	return s.output.tail(s.cfg.InstanceID, limit)
}

func (s *Supervisor) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.cancel()
		s.mu.Unlock()
		<-s.done
		_ = s.output.Close()
		_ = s.events.Close()
		_ = s.lock.Close()
	})
}
