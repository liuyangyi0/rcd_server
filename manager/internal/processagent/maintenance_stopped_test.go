//go:build linux

package processagent

import (
	"context"
	"testing"

	"rcd_manager/internal/domain"
)

func TestStoppedReleaseHoldSurvivesAgentRestartAndNeverStartsLower(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	cfg.AutoStart = false
	s := newTestSupervisor(t, cfg)
	req := maintenanceReq(s, "hold-stopped")
	v, err := s.MaintenanceAction(context.Background(), req)
	if err != nil || v.Job == nil || !v.Job.OfflineHold || s.state.SchemaVersion != 3 {
		t.Fatalf("hold: %+v %v", v, err)
	}
	if _, err = s.Submit(domain.ProcessActionRequest{RequestID: "must-not-start", InstanceID: cfg.InstanceID, Action: "start", ExpectedRevision: s.Snapshot().Revision}); err == nil {
		t.Fatal("ordinary start bypassed hold")
	}
	s.Close()
	s = newTestSupervisor(t, cfg)
	if s.Snapshot().State != "stopped" || !s.Maintenance().Job.OfflineHold {
		t.Fatal("restart lost stopped hold")
	}
	req = maintenanceReq(s, "release-stopped")
	v, err = s.MaintenanceAction(context.Background(), req)
	if err != nil || v.Job.Phase != "completed" || s.Snapshot().DesiredState != "stopped" {
		t.Fatalf("release: %+v %v", v, err)
	}
	s.Close()
	s = newTestSupervisor(t, cfg)
	if s.Snapshot().State != "stopped" || s.Snapshot().DesiredState != "stopped" {
		t.Fatal("release started previously stopped lower")
	}
}

func TestStoppedReleaseHoldCannotStealLiveMaintenance(t *testing.T) {
	s, _ := maintenanceFixture(t)
	enterMaintenance(t, s)
	if _, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "hold-stopped")); err == nil {
		t.Fatal("stole fenced maintenance")
	}
}
