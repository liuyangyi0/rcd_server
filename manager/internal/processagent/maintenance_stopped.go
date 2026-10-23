package processagent

import "time"

// An already stopped lower must not be started merely to enter maintenance.
// This holds only the Agent's process admission; it never fabricates an OPC
// fence or contacts serial hardware. Schema 3 makes older Agents fail closed.
func (s *Supervisor) stoppedMaintenance(req MaintenanceRequest) (MaintenanceSnapshot, error) {
	if !s.maintenanceMu.TryLock() {
		return MaintenanceSnapshot{}, maintenanceError("busy", "维护握手正在执行")
	}
	defer s.maintenanceMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	current := func() MaintenanceSnapshot {
		v := MaintenanceSnapshot{Supported: s.cfg.MaintenanceEndpoint != "", Revision: s.snapshot.Revision,
			GatewayInstanceID: s.cfg.GatewayInstanceID, Endpoint: s.cfg.MaintenanceEndpoint}
		if s.state.Maintenance != nil {
			job := *s.state.Maintenance
			v.Job = &job
		}
		return v
	}
	if s.lifecycleActiveLocked() {
		return current(), maintenanceError("busy", "安全运维作业尚未结束")
	}
	if s.closing || s.storageErr != nil || req.InstanceID != s.cfg.InstanceID || req.ExpectedRevision != s.snapshot.Revision || !maintenanceID(req.DeploymentID) {
		return current(), maintenanceError("conflict", "守护身份、状态或维护版本不匹配")
	}
	if s.child != nil || s.snapshot.PID != 0 || s.snapshot.State != "stopped" || s.state.DesiredState != "stopped" || s.snapshot.PendingRequestID != "" {
		return current(), maintenanceError("busy", "仅允许已确认停止且无进行中作业的下级使用停止保留")
	}
	job := s.state.Maintenance
	if req.Action == "release-stopped" {
		if job == nil || !job.OfflineHold || job.DeploymentID != req.DeploymentID {
			return current(), maintenanceError("conflict", "停止保留作业不匹配")
		}
		if job.Phase == "completed" {
			return current(), nil
		}
		job.Phase = "completed"
	} else {
		if !maintenanceID(req.TargetVersion) || req.Initiator == "" || len(req.Initiator) > 128 {
			return current(), maintenanceError("invalid_request", "需要目标版本和发起者")
		}
		if s.maintenanceActiveLocked() {
			if job.OfflineHold && job.DeploymentID == req.DeploymentID && job.TargetVersion == req.TargetVersion && job.Initiator == req.Initiator {
				return current(), nil
			}
			return current(), maintenanceError("busy", "已有未结束维护作业")
		}
		if job != nil && job.DeploymentID == req.DeploymentID {
			return current(), maintenanceError("conflict", "已完成作业不得重新执行")
		}
		now := time.Now().UTC()
		if s.state.SchemaVersion < 3 {
			s.state.SchemaVersion = 3
		}
		s.state.Maintenance = &MaintenanceJob{DeploymentID: req.DeploymentID, InstanceID: s.cfg.InstanceID,
			GatewayInstanceID: s.cfg.GatewayInstanceID, Endpoint: s.cfg.MaintenanceEndpoint, LaunchBinding: s.launchBinding(),
			TargetVersion: req.TargetVersion, Initiator: req.Initiator, Phase: "stopped", OfflineHold: true, CreatedAt: now, UpdatedAt: now}
	}
	if err := s.saveMaintenanceLocked(); err != nil {
		return current(), err
	}
	return current(), nil
}
