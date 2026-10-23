package processagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/domain"
)

type MaintenanceJob struct {
	OfflineHold          bool       `json:"offline_hold,omitempty"`
	DeploymentID         string     `json:"deployment_id"`
	InstanceID           string     `json:"instance_id"`
	GatewayInstanceID    string     `json:"gateway_instance_id"`
	Endpoint             string     `json:"endpoint"`
	LaunchBinding        string     `json:"launch_binding"`
	FencingToken         string     `json:"fencing_token"`
	TargetVersion        string     `json:"target_version"`
	Initiator            string     `json:"initiator"`
	Phase                string     `json:"phase"`
	LowerConfirmed       bool       `json:"lower_confirmed"`
	ReleaseRequested     bool       `json:"release_requested"`
	LastProcessRequestID string     `json:"last_process_request_id,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	Lower                LowerFence `json:"lower"`
	Error                string     `json:"error,omitempty"`
}

type MaintenanceRequest struct {
	Action           string `json:"action"`
	InstanceID       string `json:"instance_id"`
	DeploymentID     string `json:"deployment_id"`
	ExpectedRevision string `json:"expected_revision"`
	TargetVersion    string `json:"target_version,omitempty"`
	Initiator        string `json:"initiator,omitempty"`
}

type MaintenanceSnapshot struct {
	Supported         bool            `json:"supported"`
	Revision          string          `json:"revision"`
	Job               *MaintenanceJob `json:"job"`
	GatewayInstanceID string          `json:"gateway_instance_id,omitempty"`
	Endpoint          string          `json:"endpoint,omitempty"`
}

func maintenanceID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func maintenanceError(code, message string) error {
	return &domain.OperationError{Code: code, Message: message}
}

func (s *Supervisor) launchBinding() string {
	data, _ := json.Marshal([]any{s.cfg.Executable, s.cfg.WorkDir, s.cfg.Arguments})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Supervisor) maintenanceActiveLocked() bool {
	return s.state.Maintenance != nil && s.state.Maintenance.Phase != "completed"
}

func (s *Supervisor) Maintenance() MaintenanceSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := MaintenanceSnapshot{Supported: s.cfg.MaintenanceEndpoint != "", Revision: s.snapshot.Revision,
		GatewayInstanceID: s.cfg.GatewayInstanceID, Endpoint: s.cfg.MaintenanceEndpoint}
	if s.state.Maintenance != nil {
		copy := *s.state.Maintenance
		value.Job = &copy
	}
	return value
}

func (s *Supervisor) validateMaintenanceLoaded() error {
	job := s.state.Maintenance
	if s.state.SchemaVersion < 4 && (s.state.SchemaVersion >= 2) != (job != nil) {
		return errors.New("maintenance state schema mismatch; refusing to discard deployment state")
	}
	if job == nil {
		return nil
	}
	if job.OfflineHold {
		if (s.state.SchemaVersion != 3 && s.state.SchemaVersion != 4) || !maintenanceID(job.DeploymentID) || job.InstanceID != s.cfg.InstanceID ||
			!maintenanceID(job.TargetVersion) || strings.TrimSpace(job.Initiator) == "" || len(job.Initiator) > 128 ||
			job.LaunchBinding != s.launchBinding() || job.CreatedAt.IsZero() || job.UpdatedAt.Before(job.CreatedAt) ||
			(job.Phase != "stopped" && job.Phase != "completed") ||
			(job.Phase != "completed" && s.state.DesiredState != "stopped") {
			return errors.New("invalid offline maintenance hold; refusing to restore child")
		}
		return nil
	}
	phases := map[string]bool{"preparing": true, "entering": true, "draining": true, "ready": true, "stopping": true, "stopped": true, "starting": true, "verifying": true, "releasing": true, "unknown": true, "completed": true}
	token, err := strconv.ParseUint(job.FencingToken, 10, 64)
	if !maintenanceID(job.DeploymentID) || job.InstanceID != s.cfg.InstanceID || !maintenanceID(job.TargetVersion) ||
		strings.TrimSpace(job.Initiator) == "" || len(job.Initiator) > 128 || !phases[job.Phase] || job.CreatedAt.IsZero() || job.UpdatedAt.Before(job.CreatedAt) ||
		(job.FencingToken != "" && (err != nil || token == 0 || strconv.FormatUint(token, 10) != job.FencingToken)) ||
		(job.LowerConfirmed && job.FencingToken == "") || (job.ReleaseRequested && !job.LowerConfirmed) ||
		(job.Phase == "completed" && !job.ReleaseRequested) || !domain.ValidOperationRevision(job.GatewayInstanceID) {
		return errors.New("invalid durable maintenance job; refusing to restore process automatically")
	}
	if job.LowerConfirmed && (job.Lower.validate(job.GatewayInstanceID) != nil || !sameFence(job.Lower, *job) || !job.Lower.Durable || (!job.Lower.Active && !job.ReleaseRequested)) {
		return errors.New("invalid durable lower maintenance evidence")
	}
	if s.maintenanceActiveLocked() {
		if job.GatewayInstanceID != s.cfg.GatewayInstanceID || job.Endpoint != s.cfg.MaintenanceEndpoint || s.cfg.MaintenanceEndpoint == "" || job.LaunchBinding != s.launchBinding() {
			return errors.New("active maintenance binding differs from configured local gateway")
		}
		job.Phase = "unknown"
		job.Error = "agent 已重启；维护仍保留，未自动重做上次进程操作，请按原作业对账"
		job.UpdatedAt = time.Now().UTC()
	}
	return nil
}

// MaintenanceAction serializes a bounded handshake, never a lease. Persistent
// intent is committed before any lower mutation. HTTP cancellation cannot clear it.
func (s *Supervisor) MaintenanceAction(ctx context.Context, req MaintenanceRequest) (MaintenanceSnapshot, error) {
	if req.Action == "hold-stopped" || req.Action == "release-stopped" {
		return s.stoppedMaintenance(req)
	}
	if req.Action != "enter" && req.Action != "reconcile" && req.Action != "complete" || !maintenanceID(req.DeploymentID) || !domain.ValidOperationRevision(req.ExpectedRevision) {
		return MaintenanceSnapshot{}, maintenanceError("invalid_request", "需要有效维护动作、作业编号和精确版本")
	}
	if !s.maintenanceMu.TryLock() {
		return MaintenanceSnapshot{}, maintenanceError("busy", "维护握手正在执行")
	}
	defer s.maintenanceMu.Unlock()
	s.mu.Lock()
	if req.InstanceID != s.cfg.InstanceID {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("not_found", "守护实例不匹配")
	}
	if s.maintenancePeer == nil {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("unsupported", "尚未配置本地下级维护端点及稳定身份")
	}
	if s.closing || s.storageErr != nil {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("operation_failed", "守护关闭中或存储不可写")
	}
	if s.lifecycleActiveLocked() && !s.ownsLifecycleMaintenanceLocked(req.DeploymentID) {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("busy", "安全运维作业尚未结束")
	}
	if req.ExpectedRevision != s.snapshot.Revision {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("conflict", "守护状态版本已变化")
	}
	if s.snapshot.PendingRequestID != "" && !s.ownsLifecycleMaintenanceLocked(req.DeploymentID) {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("busy", "先等待已接收的进程作业结束")
	}
	job := s.state.Maintenance
	if job != nil && job.OfflineHold && s.maintenanceActiveLocked() {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("busy", "已停止进程的发布保留尚未结束")
	}
	if req.Action == "enter" {
		if !maintenanceID(req.TargetVersion) || strings.TrimSpace(req.Initiator) == "" || len(req.Initiator) > 128 {
			s.mu.Unlock()
			return MaintenanceSnapshot{}, maintenanceError("invalid_request", "维护需要目标软件版本及发起者")
		}
		if s.maintenanceActiveLocked() {
			if job.DeploymentID != req.DeploymentID || job.TargetVersion != req.TargetVersion || job.Initiator != req.Initiator || job.ReleaseRequested {
				s.mu.Unlock()
				return MaintenanceSnapshot{}, maintenanceError("conflict", "已有不同的维护作业或正在收尾")
			}
		} else {
			if job != nil && job.DeploymentID == req.DeploymentID {
				s.mu.Unlock()
				return MaintenanceSnapshot{}, maintenanceError("conflict", "已结束作业不能重新进入")
			}
			previousToken := "0"
			if job != nil && job.GatewayInstanceID == s.cfg.GatewayInstanceID {
				previousToken = job.FencingToken
			}
			now := time.Now().UTC()
			// Older agents must refuse this file rather than silently ignore
			// maintenance and restore/start a child. Public HTTP schema stays v1.
			if s.state.SchemaVersion < 2 {
				s.state.SchemaVersion = 2
			}
			s.state.Maintenance = &MaintenanceJob{DeploymentID: req.DeploymentID, InstanceID: s.cfg.InstanceID, GatewayInstanceID: s.cfg.GatewayInstanceID,
				Endpoint: s.cfg.MaintenanceEndpoint, LaunchBinding: s.launchBinding(), TargetVersion: req.TargetVersion, Initiator: req.Initiator, Phase: "preparing", CreatedAt: now, UpdatedAt: now,
				Lower: LowerFence{FencingToken: previousToken}}
			if err := s.saveMaintenanceLocked(); err != nil {
				s.mu.Unlock()
				return MaintenanceSnapshot{}, err
			}
		}
	} else if job == nil || job.DeploymentID != req.DeploymentID {
		s.mu.Unlock()
		return MaintenanceSnapshot{}, maintenanceError("conflict", "维护作业编号不匹配")
	}
	current := *s.state.Maintenance
	s.mu.Unlock()
	// Explicit reconciliation must refresh OPC evidence even after completion:
	// a publisher recovering a lost release reply cannot rely on a cached fence.
	if current.Phase == "completed" && req.Action != "reconcile" {
		return s.Maintenance(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	stopCancel := context.AfterFunc(s.ctx, cancel)
	defer stopCancel()
	fence, err := s.maintenancePeer.Read(ctx)
	if err != nil {
		return s.maintenanceUnknown(err)
	}
	if err = fence.validate(current.GatewayInstanceID); err != nil {
		return s.maintenanceEvidenceUnknown(err)
	}
	switch req.Action {
	case "enter":
		if current.FencingToken == "" {
			lowerToken, _ := strconv.ParseUint(fence.FencingToken, 10, 64)
			previous, _ := strconv.ParseUint(current.Lower.FencingToken, 10, 64)
			if fence.Active || !fence.Durable || lowerToken < previous || lowerToken == ^uint64(0) {
				return s.maintenanceUnknown(errors.New("下级仍在维护、状态不明或代次回退，不能分配新代次"))
			}
			current.FencingToken = strconv.FormatUint(lowerToken+1, 10)
			s.mu.Lock()
			s.state.Maintenance.FencingToken = current.FencingToken
			s.state.Maintenance.Phase = "entering"
			err = s.saveMaintenanceLocked()
			s.mu.Unlock()
			if err != nil {
				return s.Maintenance(), err
			}
		}
		if !sameFence(fence, current) {
			token, _ := strconv.ParseUint(current.FencingToken, 10, 64)
			lowerToken, _ := strconv.ParseUint(fence.FencingToken, 10, 64)
			if fence.Active || !fence.Durable || token == 0 || lowerToken != token-1 {
				return s.maintenanceEvidenceUnknown(errors.New("下级维护代次/作业不匹配，禁止抢占或重放"))
			}
		}
		fence, err = s.maintenancePeer.Change(ctx, "enter_maintenance", current.DeploymentID, current.FencingToken, fence.BootID)
		if err != nil {
			return s.maintenanceUnknown(err)
		}
		return s.recordLowerFence(fence, false)
	case "reconcile":
		return s.recordLowerFence(fence, current.ReleaseRequested)
	case "complete":
		if current.ReleaseRequested && sameFence(fence, current) && !fence.Active {
			return s.recordLowerFence(fence, true)
		}
		if !sameFence(fence, current) || !fence.Durable || !fence.Active {
			return s.maintenanceEvidenceUnknown(errors.New("下级维护所有者或持久状态不匹配，不能收尾"))
		}
		if !fence.Drained {
			value, err := s.recordLowerFence(fence, false)
			if err != nil {
				return value, err
			}
			return value, maintenanceError("busy", "下级已接受事务尚未结束")
		}
		s.mu.Lock()
		s.state.Maintenance.ReleaseRequested = true
		s.state.Maintenance.LowerConfirmed = true
		s.state.Maintenance.Lower = fence
		s.state.Maintenance.Phase = "releasing"
		err = s.saveMaintenanceLocked()
		s.mu.Unlock()
		if err != nil {
			return s.Maintenance(), err
		}
		_, err = s.maintenancePeer.Change(ctx, "leave_maintenance", current.DeploymentID, current.FencingToken, fence.BootID)
		if err != nil {
			return s.maintenanceUnknown(err)
		}
		// A fresh read confirms release; a lost reply is reconciled, not resent blindly.
		fence, err = s.maintenancePeer.Read(ctx)
		if err != nil {
			return s.maintenanceUnknown(err)
		}
		return s.recordLowerFence(fence, true)
	}
	panic("validated maintenance action")
}

func sameFence(f LowerFence, j MaintenanceJob) bool {
	return f.GatewayInstanceID == j.GatewayInstanceID && f.DeploymentID == j.DeploymentID && f.FencingToken == j.FencingToken
}

func (s *Supervisor) saveMaintenanceLocked() error {
	s.state.Maintenance.UpdatedAt = time.Now().UTC()
	s.bumpLocked()
	if err := s.persistLocked(); err != nil {
		return maintenanceError("persistence_failed", "维护状态保存失败，操作被阻止: "+boundedError(err))
	}
	job := s.state.Maintenance
	s.logger.Info("maintenance state committed", "instance_id", job.InstanceID, "deployment_id", job.DeploymentID, "fencing_token", job.FencingToken, "phase", job.Phase, "target_version", job.TargetVersion, "initiator", job.Initiator, "error", job.Error)
	return nil
}

func (s *Supervisor) maintenanceUnknown(cause error) (MaintenanceSnapshot, error) {
	s.mu.Lock()
	s.state.Maintenance.Phase = "unknown"
	s.state.Maintenance.Error = boundedError(cause)
	err := s.saveMaintenanceLocked()
	s.mu.Unlock()
	if err != nil {
		return s.Maintenance(), err
	}
	return s.Maintenance(), maintenanceError("operation_failed", "维护结果未确认，保留阻止状态: "+boundedError(cause))
}

// A disconnected peer is not proof of fence loss. A contradictory response is:
// revoke permission to start from cached evidence until an exact read reconciles.
func (s *Supervisor) maintenanceEvidenceUnknown(cause error) (MaintenanceSnapshot, error) {
	s.mu.Lock()
	if !s.state.Maintenance.ReleaseRequested {
		s.state.Maintenance.LowerConfirmed = false
	}
	s.mu.Unlock()
	return s.maintenanceUnknown(cause)
}

func (s *Supervisor) recordLowerFence(fence LowerFence, allowReleased bool) (MaintenanceSnapshot, error) {
	s.mu.Lock()
	job := s.state.Maintenance
	err := fence.validate(job.GatewayInstanceID)
	if err == nil && (!sameFence(fence, *job) || !fence.Durable || (!fence.Active && !(allowReleased && job.ReleaseRequested))) {
		err = errors.New("维护所有者/代次不匹配或持久化状态未知")
	}
	if err != nil {
		s.mu.Unlock()
		return s.maintenanceEvidenceUnknown(err)
	}
	job.Lower = fence
	job.Error = ""
	if !fence.Active {
		job.Phase = "completed"
	} else {
		job.LowerConfirmed = true
		job.Phase = "draining"
		if fence.Drained {
			job.Phase = "ready"
		}
		if job.ReleaseRequested {
			job.Phase = "releasing"
		}
	}
	err = s.saveMaintenanceLocked()
	s.mu.Unlock()
	return s.Maintenance(), err
}

func (s *Supervisor) submitMaintenanceProcess(req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	if !s.maintenanceMu.TryLock() {
		return domain.ProcessOperation{}, maintenanceError("busy", "维护握手正在执行")
	}
	defer s.maintenanceMu.Unlock()
	s.mu.Lock()
	if req.InstanceID != s.cfg.InstanceID {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("not_found", "守护实例标识不匹配")
	}
	if item := s.findLocked(req.RequestID); item != nil {
		s.mu.Unlock()
		return s.submit(req, true) // duplicate read only, never another enqueue
	}
	if s.closing || s.storageErr != nil {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("operation_failed", "守护关闭中或存储不可写")
	}
	if req.ExpectedRevision != s.snapshot.Revision {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("conflict", "进程状态版本已变化")
	}
	if s.snapshot.PendingRequestID != "" {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("busy", "已有进行中的进程操作")
	}
	job := s.state.Maintenance
	if !s.maintenanceActiveLocked() || job.DeploymentID != req.DeploymentID || job.FencingToken != req.FencingToken || !job.LowerConfirmed || job.ReleaseRequested || s.maintenancePeer == nil {
		s.mu.Unlock()
		return domain.ProcessOperation{}, maintenanceError("conflict", "进程操作未持有已确认的维护令牌")
	}
	current := *job
	s.mu.Unlock()
	if req.Action == "stop" || req.Action == "restart" {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		fence, err := s.maintenancePeer.Read(ctx)
		if err != nil {
			_, failure := s.maintenanceUnknown(err)
			return domain.ProcessOperation{}, failure
		}
		if err = fence.validate(current.GatewayInstanceID); err != nil || !sameFence(fence, current) || !fence.Durable || !fence.Active {
			_, failure := s.maintenanceEvidenceUnknown(errors.New("下级未确认当前维护代次"))
			return domain.ProcessOperation{}, failure
		}
		if !fence.Drained {
			return domain.ProcessOperation{}, maintenanceError("busy", "下级已接受事务尚未结束，不能停机")
		}
	}
	return s.submit(req, true)
}

func (s *Supervisor) maintenanceProcessAllowedLocked(req domain.ProcessActionRequest, owner bool) error {
	if s.lifecycleActiveLocked() {
		return maintenanceError("busy", "安全运维作业尚未结束")
	}
	if s.state.DeferredFence != nil && !owner {
		return maintenanceError("busy", "人为停止保留的维护身份须通过安全生命周期启动")
	}
	if s.maintenanceActiveLocked() {
		job := s.state.Maintenance
		if !owner || req.DeploymentID != job.DeploymentID || req.FencingToken != job.FencingToken || !job.LowerConfirmed || job.ReleaseRequested {
			return maintenanceError("busy", "下级处于维护状态；进程操作必须使用当前维护作业及令牌")
		}
	} else if req.DeploymentID != "" || req.FencingToken != "" {
		return maintenanceError("conflict", "维护作业已经结束或不存在")
	} else if s.cfg.MaintenanceEndpoint != "" && req.Action != "start" {
		return maintenanceError("busy", "先登记维护作业并确认下级排空，再停止或重启")
	}
	return nil
}

func (s *Supervisor) maintenanceProcessFinishedLocked(req domain.ProcessActionRequest, err error) {
	if req.DeploymentID == "" || !s.maintenanceActiveLocked() {
		return
	}
	job := s.state.Maintenance
	job.UpdatedAt = time.Now().UTC()
	job.Phase = "verifying"
	if req.Action == "stop" {
		job.Phase = "stopped"
	}
	if err != nil {
		job.Phase = "unknown"
		job.Error = boundedError(err)
	}
}
