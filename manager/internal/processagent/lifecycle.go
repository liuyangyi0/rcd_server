package processagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"rcd_manager/internal/domain"
)

// The regular supervisor worker also executes lifecycle jobs. There is no
// second process owner or remote job queue. A stopped fence remains durable
// until a later explicit safe start can release it against the original lower.
type lifecycleState struct {
	domain.ProcessLifecycleView
	FenceID string `json:"fence_id"`
}

type lifecyclePreconditionError struct{ err error }

func (e *lifecyclePreconditionError) Error() string { return e.err.Error() }
func (e *lifecyclePreconditionError) Unwrap() error { return e.err }

func (s *Supervisor) lifecycleSupported() bool {
	return s.cfg.HostReleaseDir != "" && s.maintenancePeer != nil
}
func (s *Supervisor) lifecycleActiveLocked() bool {
	j := s.state.Lifecycle
	return j != nil && j.State != "succeeded" && j.State != "failed"
}
func (s *Supervisor) ownsLifecycleMaintenanceLocked(id string) bool {
	j := s.state.Lifecycle
	return j != nil && s.lifecycleActiveLocked() && j.FenceID == id && s.snapshot.PendingRequestID == j.RequestID
}

func (s *Supervisor) validateLifecycleLoaded() error {
	if s.state.DeferredFence != nil {
		j := s.state.DeferredFence
		if s.state.SchemaVersion != 4 || j.InstanceID != s.cfg.InstanceID || j.GatewayInstanceID != s.cfg.GatewayInstanceID || j.Endpoint != s.cfg.MaintenanceEndpoint || j.LaunchBinding != s.launchBinding() || !j.LowerConfirmed || !j.Lower.Active || !j.Lower.Drained || !sameFence(j.Lower, *j) || j.Lower.validate(j.GatewayInstanceID) != nil {
			return errors.New("invalid deferred manual-stop fence; refusing to discard identity")
		}
	}
	if j := s.state.Lifecycle; j != nil {
		item := s.findLocked(j.RequestID)
		if s.state.SchemaVersion != 4 || item == nil || !item.Request.Lifecycle || j.ManagerInstanceID != item.Request.ManagerInstanceID || j.Actor != item.Request.Actor || j.Action != item.Request.Action || !maintenanceID(j.FenceID) {
			return errors.New("invalid durable lifecycle operation")
		}
		if s.lifecycleActiveLocked() {
			j.State, j.Phase, j.Error = "unknown", "unknown", "Agent 已重启；未重复执行，请核对原作业"
		}
	}
	return nil
}

func (s *Supervisor) lifecycleMarker() string {
	return filepath.Join(s.cfg.HostReleaseDir, ".rcd-host-control", "lifecycle.json")
}

// Both publishers and routine controls inspect each other's durable intent
// under the same flock. Releasing an OS lock on crash is not a completed job.
func (s *Supervisor) acquireLifecycleHost(ownRequest string) (*os.File, error) {
	f, err := lockLifecycle(filepath.Join(s.cfg.HostReleaseDir, ".rcd-host-release.lock"))
	if err != nil {
		return nil, maintenanceError("busy", "目标机器正在发布或运维，或共享锁不可访问")
	}
	paths := []string{filepath.Join(s.cfg.HostReleaseDir, ".rcd-host-release-active.json"), s.lifecycleMarker()}
	legacy, e := filepath.Glob(filepath.Join(s.cfg.HostReleaseDir, ".*.release-active.json"))
	if e != nil {
		f.Close()
		return nil, e
	}
	paths = append(paths, legacy...)
	for _, path := range paths {
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
			f.Close()
			return nil, maintenanceError("busy", "目标机器作业记录不可核对")
		}
		raw, e := os.ReadFile(path)
		var v struct {
			State     string `json:"state"`
			RequestID string `json:"request_id"`
		}
		valid := e == nil && len(raw) <= 65536 && json.Unmarshal(raw, &v) == nil
		terminal := v.State == "succeeded"
		if path == s.lifecycleMarker() {
			terminal = terminal || v.State == "failed"
		}
		if path == s.lifecycleMarker() && ownRequest != "" && v.RequestID == ownRequest {
			terminal = true
		}
		if filepath.Base(path) == ".rcd-host-release-active.json" {
			terminal = terminal || v.State == "recovered" || v.State == "failed_before_install"
		}
		if !valid || !terminal {
			f.Close()
			return nil, maintenanceError("busy", "目标机器有未确认的发布或运维作业，请核对原操作")
		}
	}
	return f, nil
}

func (s *Supervisor) saveLifecycleLocked() error {
	s.bumpLocked()
	if err := s.persistLocked(); err != nil {
		return err
	}
	raw, err := json.Marshal(s.state.Lifecycle)
	if err == nil {
		err = atomicState(s.lifecycleMarker(), raw)
	}
	if err != nil {
		s.storageErr = err
	}
	return err
}

func (s *Supervisor) SubmitLifecycle(req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	if err := req.Validate(); err != nil {
		return domain.ProcessOperation{}, err
	}
	if !req.Lifecycle || !s.lifecycleSupported() {
		return domain.ProcessOperation{}, maintenanceError("unsupported", "代理未启用安全生命周期能力")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.InstanceID != s.cfg.InstanceID || req.GatewayInstanceID != s.cfg.GatewayInstanceID {
		return domain.ProcessOperation{}, maintenanceError("conflict", "固定 Agent 或下级身份不匹配")
	}
	if item := s.findLocked(req.RequestID); item != nil {
		if item.Request != req {
			return domain.ProcessOperation{}, maintenanceError("conflict", "操作编号已用于另一请求")
		}
		return cloneOperation(item.Operation), nil
	}
	if s.closing || s.storageErr != nil {
		return domain.ProcessOperation{}, maintenanceError("persistence_failed", "代理不可写或正在退出")
	}
	if req.ExpectedRevision != s.snapshot.Revision {
		return domain.ProcessOperation{}, maintenanceError("conflict", "进程状态已变化，请刷新")
	}
	if s.lifecycleActiveLocked() || s.maintenanceActiveLocked() || s.snapshot.PendingRequestID != "" {
		return domain.ProcessOperation{}, maintenanceError("busy", "目标已有进行中的发布或运维作业")
	}
	guard, err := s.acquireLifecycleHost("")
	if err != nil {
		return domain.ProcessOperation{}, err
	}
	// This narrowly scoped state directory is provisioned by installation; it
	// does not require giving the Agent write access to its binary directory.
	if info, err := os.Stat(filepath.Dir(s.lifecycleMarker())); err != nil || !info.IsDir() {
		guard.Close()
		return domain.ProcessOperation{}, maintenanceError("unsupported", "尚未安装宿主机共享运维状态目录")
	}
	sum := sha256.Sum256([]byte(req.ManagerInstanceID + "\x00" + req.RequestID))
	j := &lifecycleState{ProcessLifecycleView: domain.ProcessLifecycleView{RequestID: req.RequestID, ManagerInstanceID: req.ManagerInstanceID, Actor: req.Actor, Action: req.Action, State: "accepted", Phase: "accepted"}, FenceID: "routine." + hex.EncodeToString(sum[:])}
	if s.state.DeferredFence != nil {
		j.FenceID = s.state.DeferredFence.DeploymentID
	}
	s.state.SchemaVersion, s.state.Lifecycle = 4, j
	s.snapshot.PendingRequestID = req.RequestID
	s.retryAt = time.Time{}
	item := s.addLocked(req, "accepted")
	if err := s.saveLifecycleLocked(); err != nil {
		j.State, j.Phase = "unknown", "unknown"
		item.Operation.State = "unknown"
		guard.Close()
		return domain.ProcessOperation{}, maintenanceError("persistence_failed", "运维登记未确认，未派发动作")
	}
	s.lifecycleLock = guard
	s.jobs <- req
	return cloneOperation(item.Operation), nil
}

func (s *Supervisor) lifecyclePhase(phase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Lifecycle.Phase, s.state.Lifecycle.State = phase, "running"
	item := s.findLocked(s.state.Lifecycle.RequestID)
	item.Operation.State, item.Operation.UpdatedAt = "running", time.Now().UTC()
	return s.saveLifecycleLocked()
}

func (s *Supervisor) lifecycleMaintenance(ctx context.Context, action string) error {
	s.mu.Lock()
	j := *s.state.Lifecycle
	req := MaintenanceRequest{Action: action, InstanceID: s.cfg.InstanceID, DeploymentID: j.FenceID, ExpectedRevision: s.snapshot.Revision, TargetVersion: "routine", Initiator: j.Actor}
	s.mu.Unlock()
	_, err := s.MaintenanceAction(ctx, req)
	return err
}

func (s *Supervisor) lifecycleDrain(ctx context.Context) error {
	if err := s.lifecyclePhase("draining"); err != nil {
		return err
	}
	for {
		if err := s.lifecycleMaintenance(ctx, "reconcile"); err != nil {
			return err
		}
		m := s.Maintenance()
		if m.Job != nil && m.Job.LowerConfirmed && m.Job.Lower.Drained {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("下级排空超时；未停止进程，保留维护供核对")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Supervisor) lifecycleRun(req domain.ProcessActionRequest) error {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute+s.cfg.StartTimeout+s.cfg.StopTimeout)
	defer cancel()
	s.mu.Lock()
	running := s.child != nil
	if req.ConfigurationGuard != "" && !running {
		s.mu.Unlock()
		return &lifecyclePreconditionError{errors.New("配置应用重启要求下级正在运行；未启动原本停止的程序")}
	}
	deferred := s.state.DeferredFence
	if deferred != nil {
		copy := *deferred
		s.state.Maintenance = &copy
	}
	s.mu.Unlock()
	if running && req.Action != "start" {
		if err := s.lifecyclePhase("entering"); err != nil {
			return err
		}
		if err := s.lifecycleMaintenance(ctx, "enter"); err != nil {
			return err
		}
		drainCtx, drainCancel := context.WithTimeout(ctx, 120*time.Second)
		err := s.lifecycleDrain(drainCtx)
		drainCancel()
		if err != nil {
			return err
		}
		if req.ConfigurationGuard != "" {
			if err := s.lifecyclePhase("checking_configuration"); err != nil {
				return err
			}
			expected, err := domain.ParseConfigurationGuard(req.ConfigurationGuard)
			peer, supported := s.maintenancePeer.(maintenanceConfigurationPeer)
			job := s.Maintenance().Job
			if err == nil && (!supported || job == nil) {
				err = errors.New("configuration guard is unavailable")
			}
			if err == nil {
				err = peer.ValidateConfiguration(ctx, job.Lower, expected)
			}
			if err != nil {
				// No process signal was sent. Release our own fence before marking
				// a definitive precondition failure; an unconfirmed release stays unknown.
				if releaseErr := s.lifecycleMaintenance(ctx, "complete"); releaseErr != nil {
					return errors.Join(err, releaseErr)
				}
				return &lifecyclePreconditionError{err}
			}
		}
		if err := s.lifecyclePhase("stopping"); err != nil {
			return err
		}
		if _, err := s.stopOwnedGracefully(); err != nil {
			return err
		}
	}
	if req.Action == "stop" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.child != nil || s.snapshot.PID != 0 {
			return errors.New("进程退出尚未确认")
		}
		if m := s.state.Maintenance; m != nil && m.LowerConfirmed && m.Lower.Active {
			copy := *m
			s.state.DeferredFence = &copy
			s.state.Maintenance = nil
		}
		s.state.DesiredState, s.snapshot.State = "stopped", "stopped"
		return s.saveLifecycleLocked()
	}
	if err := s.lifecyclePhase("starting"); err != nil {
		return err
	}
	s.mu.Lock()
	s.state.DesiredState = "running"
	s.bumpLocked()
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.startOwned(); err != nil {
		return err
	}
	if err := s.lifecyclePhase("verifying"); err != nil {
		return err
	}
	fence, err := s.maintenancePeer.Read(ctx)
	if err != nil {
		return err
	}
	if err := fence.validate(s.cfg.GatewayInstanceID); err != nil {
		return err
	}
	if !fence.Durable {
		return errors.New("下级维护状态未持久确认")
	}
	if s.Maintenance().Job != nil && s.Maintenance().Job.Phase != "completed" {
		if err := s.lifecycleDrain(ctx); err != nil {
			return err
		}
		if err := s.lifecyclePhase("releasing"); err != nil {
			return err
		}
		if err := s.lifecycleMaintenance(ctx, "complete"); err != nil {
			return err
		}
	} else if fence.Active {
		return errors.New("下级存在另一未确认维护，未解除")
	}
	s.mu.Lock()
	s.state.DeferredFence = nil
	s.snapshot.State = "running"
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) performLifecycle(req domain.ProcessActionRequest) {
	err := s.lifecycleRun(req)
	s.mu.Lock()
	j := s.state.Lifecycle
	item := s.findLocked(req.RequestID)
	item.Operation.UpdatedAt = time.Now().UTC()
	if err != nil {
		var precondition *lifecyclePreconditionError
		if errors.As(err, &precondition) {
			j.State, j.Phase, j.Error = "failed", "configuration_rejected", boundedError(err)
			item.Operation.State, item.Operation.ErrorCode, item.Operation.Error = "failed", "conflict", j.Error
		} else {
			j.State, j.Phase, j.Error = "unknown", "unknown", boundedError(err)
			item.Operation.State, item.Operation.ErrorCode, item.Operation.Error = "unknown", "lifecycle_unconfirmed", j.Error
		}
	} else {
		j.State, j.Phase = "succeeded", "completed"
		if req.Action == "stop" {
			j.Phase = "manual_stopped"
		}
		item.Operation.State = "succeeded"
	}
	s.snapshot.PendingRequestID = ""
	snap := s.snapshotLocked()
	item.Operation.Snapshot = &snap
	if e := s.saveLifecycleLocked(); e != nil {
		j.State, j.Phase = "unknown", "unknown"
		item.Operation.State, item.Operation.ErrorCode, item.Operation.Error = "unknown", "persistence_failed", "运维结果保存未确认"
	}
	guard := s.lifecycleLock
	s.lifecycleLock = nil
	if guard != nil {
		guard.Close()
	}
	s.mu.Unlock()
	s.logger.Info("safe lifecycle finished", "request_id", req.RequestID, "manager_instance_id", req.ManagerInstanceID, "action", req.Action, "error", fmt.Sprint(err))
}

// A routine stop never escalates SIGTERM to SIGKILL. Unknown exit keeps its
// durable operation blocked instead of starting a second process.
func (s *Supervisor) stopOwnedGracefully() (bool, error) {
	s.mu.Lock()
	child := s.child
	s.mu.Unlock()
	if child == nil {
		return false, nil
	}
	select {
	case <-child.done:
	default:
		if err := requestStop(child.process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return false, err
		}
	}
	timer := time.NewTimer(s.cfg.StopTimeout)
	defer timer.Stop()
	select {
	case <-child.done:
		s.mu.Lock()
		s.recordExitLocked(child)
		s.snapshot.State = "stopped"
		s.mu.Unlock()
		return false, nil
	case <-timer.C:
		return false, errors.New("下级未在宽限期退出；未强杀，结果待确认")
	case <-s.ctx.Done():
		return false, s.ctx.Err()
	}
}

func (s *Supervisor) cleanupFailedStart() (bool, error) {
	s.mu.Lock()
	safe := s.lifecycleActiveLocked()
	s.mu.Unlock()
	if safe {
		return s.stopOwnedGracefully()
	}
	return s.stopOwned()
}

// Resolve performs read-only reconciliation of the ORIGINAL operation. It
// never repeats enter/stop/start/leave, including after a lost reply. Missing
// requests are durably tombstoned by resolveRecorded before a late POST arrives.
func (s *Supervisor) Resolve(req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	if err := req.Validate(); err != nil {
		return domain.ProcessOperation{}, err
	}
	if !req.Lifecycle {
		return s.resolveRecorded(req)
	}
	s.mu.Lock()
	item := s.findLocked(req.RequestID)
	if item == nil || s.state.Lifecycle == nil || s.state.Lifecycle.RequestID != req.RequestID || item.Operation.State != "unknown" {
		s.mu.Unlock()
		return s.resolveRecorded(req)
	}
	if item.Request != req || req.InstanceID != s.cfg.InstanceID || req.GatewayInstanceID != s.cfg.GatewayInstanceID {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("conflict", "原作业身份不匹配")
	}
	if s.closing || s.storageErr != nil || s.snapshot.PendingRequestID != "" {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("busy", "原作业仍在执行或持久状态不可写")
	}
	guard, err := s.acquireLifecycleHost(req.RequestID)
	if err != nil {
		s.mu.Unlock()
		return domain.ProcessOperation{}, err
	}
	defer guard.Close()
	// A confirmed exit from THIS owned process is evidence. PID=0 after an
	// Agent crash alone is not proof of the previous process having exited.
	if s.child != nil {
		select {
		case <-s.child.done:
			s.recordExitLocked(s.child)
		default:
		}
	}
	stopConfirmed := req.Action == "stop" && s.child == nil && s.snapshot.PID == 0 && s.snapshot.LastExitAt != nil && !s.snapshot.LastExitAt.Before(item.Operation.CreatedAt)
	running := s.child != nil && s.snapshot.PID > 0
	m := s.state.Maintenance
	var fenceJob MaintenanceJob
	if m != nil {
		fenceJob = *m
	}
	s.mu.Unlock()
	var fence LowerFence
	startConfirmed := false
	if running && req.Action != "stop" {
		ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
		fence, err = s.maintenancePeer.Read(ctx)
		cancel()
		startConfirmed = err == nil && fence.validate(s.cfg.GatewayInstanceID) == nil && fence.Durable && !fence.Active && !fence.AdmissionBlocked &&
			(m == nil || (fenceJob.ReleaseRequested && sameFence(fence, fenceJob)))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item = s.findLocked(req.RequestID)
	if s.closing || item == nil || s.snapshot.PendingRequestID != "" {
		return domain.ProcessOperation{}, maintenanceError("busy", "状态已变化，请重新对账原操作")
	}
	if stopConfirmed && m != nil && (!fenceJob.LowerConfirmed || !fenceJob.Lower.Drained || !fenceJob.Lower.Active) {
		stopConfirmed = false
	}
	if !stopConfirmed && !startConfirmed {
		return cloneOperation(item.Operation), nil
	}
	if stopConfirmed {
		if m != nil {
			copy := fenceJob
			s.state.DeferredFence = &copy
			s.state.Maintenance = nil
		}
		s.state.DesiredState, s.snapshot.State = "stopped", "stopped"
		s.state.Lifecycle.Phase = "manual_stopped"
	} else {
		if s.child == nil || s.snapshot.PID == 0 {
			return cloneOperation(item.Operation), nil
		}
		if m != nil {
			s.state.Maintenance.Lower = fence
			s.state.Maintenance.LowerConfirmed = true
			s.state.Maintenance.Phase = "completed"
			s.state.Maintenance.Error = ""
		}
		s.state.DeferredFence = nil
		s.state.Lifecycle.Phase = "completed"
		s.snapshot.State = "running"
	}
	s.state.Lifecycle.State, s.state.Lifecycle.Error = "succeeded", ""
	item.Operation.State, item.Operation.ErrorCode, item.Operation.Error = "succeeded", "", ""
	item.Operation.UpdatedAt = time.Now().UTC()
	// Reconciliation can change desired=running to desired=stopped after a
	// late exit. Refresh derived snapshot fields BEFORE freezing the terminal
	// operation; otherwise its persisted/HTTP result says succeeded while its
	// DesiredState still describes the pre-reconciliation running process.
	s.bumpLocked()
	snap := s.snapshotLocked()
	item.Operation.Snapshot = &snap
	if err := s.saveLifecycleLocked(); err != nil {
		return domain.ProcessOperation{}, err
	}
	return cloneOperation(item.Operation), nil
}
