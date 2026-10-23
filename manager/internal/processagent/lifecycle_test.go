//go:build linux

package processagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func lifecycleFixture(t *testing.T, mode string) (*Supervisor, *fakeMaintenancePeer) {
	t.Helper()
	cfg := supervisorConfig(t, mode)
	cfg.HostReleaseDir = t.TempDir()
	if err := os.Mkdir(filepath.Join(cfg.HostReleaseDir, ".rcd-host-control"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg.MaintenanceEndpoint, cfg.GatewayInstanceID = "opc.tcp://127.0.0.1:14991", "test-gateway"
	cfg.RemoteManagers = "192.168.10.30,192.168.10.46,192.168.10.58"
	s := newTestSupervisor(t, cfg)
	p := &fakeMaintenancePeer{fence: LowerFence{SchemaVersion: 1, GatewayInstanceID: cfg.GatewayInstanceID, FencingToken: "0", BootID: "boot-a", Durable: true}}
	s.maintenancePeer = p
	return s, p
}

func safeRequest(s *Supervisor, action, manager string) domain.ProcessActionRequest {
	return domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, GatewayInstanceID: s.cfg.GatewayInstanceID,
		Lifecycle: true, ManagerInstanceID: manager, Actor: "operator", Action: action, ExpectedRevision: s.Snapshot().Revision}
}

func safeAction(t *testing.T, s *Supervisor, action, manager string) domain.ProcessActionRequest {
	t.Helper()
	req := safeRequest(s, action, manager)
	if _, err := s.SubmitLifecycle(req); err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, s, req); result.State != "succeeded" {
		t.Fatalf("safe operation failed: %+v", result)
	}
	return req
}

func TestLifecycleStopStartAcrossManagersPreservesFenceAndNoReplay(t *testing.T) {
	s, p := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-30")
	old := s.Snapshot().PID
	stop := safeAction(t, s, "stop", "manager-46")
	if v := s.Snapshot(); v.PID != 0 || !v.ManualStopped || v.MaintenanceDeploymentID != "" {
		t.Fatalf("stop is not terminal/manual: %+v", v)
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "process-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var durable durableState
	if json.Unmarshal(raw, &durable) != nil {
		t.Fatal("invalid durable state")
	}
	for _, record := range durable.Operations {
		if e := record.Operation.Validate(record.Request); e != nil {
			t.Fatalf("invalid persisted operation before Close: %v", e)
		}
	}
	if _, err := s.SubmitLifecycle(stop); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: "start", ExpectedRevision: s.Snapshot().Revision}); err == nil {
		t.Fatal("legacy start bypassed retained fence")
	}
	safeAction(t, s, "start", "manager-58")
	if v := s.Snapshot(); v.PID == old || v.ManualStopped || v.MaintenanceDeploymentID != "" {
		t.Fatalf("start did not clear original fence: %+v", v)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Join(p.calls, ",") != "enter_maintenance,leave_maintenance" {
		t.Fatalf("unexpected maintenance mutations: %v", p.calls)
	}
}

func TestLifecycleConfigurationRechecksAfterDrainingAcrossManagers(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "second_manager_changed_config"}[changed], func(t *testing.T) {
			s, p := lifecycleFixture(t, "graceful")
			safeAction(t, s, "start", "manager-30")
			pid := s.Snapshot().PID
			oldHash, newHash := strings.Repeat("a", 64), strings.Repeat("b", 64)
			p.mu.Lock()
			p.configurations = map[string]string{"config.toml": oldHash}
			if changed {
				p.beforeChange = func() { p.configurations["config.toml"] = newHash }
			}
			p.mu.Unlock()
			req := safeRequest(s, "restart", "manager-46")
			raw, _ := json.Marshal(map[string]string{"config.toml": oldHash})
			req.ConfigurationGuard = string(raw)
			if _, err := s.SubmitLifecycle(req); err != nil {
				t.Fatal(err)
			}
			result := awaitOperation(t, s, req)
			if changed {
				if result.State != "failed" || result.ErrorCode != "conflict" || s.Snapshot().PID != pid || s.Snapshot().MaintenanceDeploymentID != "" {
					t.Fatalf("changed config caused restart or retained lock: %+v / %+v", result, s.Snapshot())
				}
			} else if result.State != "succeeded" || s.Snapshot().PID == pid {
				t.Fatalf("matching config did not restart: %+v", result)
			}
			p.mu.Lock()
			checks := p.configurationChecks
			p.mu.Unlock()
			if checks != 1 {
				t.Fatalf("configuration checks=%d", checks)
			}
			if _, err := s.SubmitLifecycle(req); err != nil {
				t.Fatal(err)
			} // lookup only
		})
	}
}

func TestLifecycleUsesPublisherHostLockAndUnknownMarker(t *testing.T) {
	s, _ := lifecycleFixture(t, "graceful")
	guard, err := lockLifecycle(filepath.Join(s.cfg.HostReleaseDir, ".rcd-host-release.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitLifecycle(safeRequest(s, "start", "manager-30"))
	requireCode(t, err, "busy")
	guard.Close()
	if err := os.WriteFile(filepath.Join(s.cfg.HostReleaseDir, ".rcd-host-release-active.json"), []byte(`{"state":"unknown"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitLifecycle(safeRequest(s, "start", "manager-58"))
	requireCode(t, err, "busy")
	if s.Snapshot().PID != 0 {
		t.Fatal("blocked action started a process")
	}
}

func TestLifecycleLostMaintenanceReplyDoesNotStopOrReplay(t *testing.T) {
	s, p := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-30")
	pid := s.Snapshot().PID
	p.mu.Lock()
	p.loseEnter = true
	p.mu.Unlock()
	req := safeRequest(s, "restart", "manager-46")
	if _, err := s.SubmitLifecycle(req); err != nil {
		t.Fatal(err)
	}
	awaitState(t, s, func(v domain.ProcessSnapshot) bool { return v.Lifecycle != nil && v.Lifecycle.State == "unknown" })
	if s.Snapshot().PID != pid {
		t.Fatal("unconfirmed admission stopped or restarted the process")
	}
	if _, err := s.Resolve(req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitLifecycle(req); err != nil {
		t.Fatal(err)
	}
	_, err := s.SubmitLifecycle(safeRequest(s, "restart", "manager-58"))
	requireCode(t, err, "busy")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 1 {
		t.Fatal("lost response replayed maintenance")
	}
}

func TestLifecycleStoppedSurvivesAgentRestart(t *testing.T) {
	s, p := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-30")
	safeAction(t, s, "stop", "manager-30")
	cfg := s.cfg
	s.Close()
	restored := newTestSupervisor(t, cfg)
	restored.maintenancePeer = p
	if v := restored.Snapshot(); v.PID != 0 || v.DesiredState != "stopped" || !v.ManualStopped {
		t.Fatalf("stopped state not retained: %+v", v)
	}
	safeAction(t, restored, "start", "manager-58")
}

func TestLifecycleConcurrentManagersOnlyOneAccepted(t *testing.T) {
	s, _ := lifecycleFixture(t, "graceful")
	a, b := safeRequest(s, "start", "manager-30"), safeRequest(s, "start", "manager-58")
	if _, err := s.SubmitLifecycle(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitLifecycle(b); err == nil {
		t.Fatal("concurrent operation accepted")
	}
	awaitOperation(t, s, a)
	changed := a
	changed.Actor = "another operator"
	_, err := s.SubmitLifecycle(changed)
	requireCode(t, err, "conflict")
}

func TestLifecycleHTTPRemoteCannotUseRawMaintenance(t *testing.T) {
	s, _ := lifecycleFixture(t, "graceful")
	for _, path := range []string{"/api/v1/process/actions", "/api/v1/maintenance/actions"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.RemoteAddr = "192.168.10.30:22222"
		req.Header.Set(domain.ProcessInstanceHeader, s.cfg.InstanceID)
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusNotImplemented {
			t.Fatalf("remote raw write accepted: %d %s", res.Code, res.Body.String())
		}
	}
	request := safeRequest(s, "start", "manager-30")
	raw, _ := json.Marshal(request)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/lifecycle/actions", strings.NewReader(string(raw)))
	req.RemoteAddr = "192.168.10.30:22222"
	req.Header.Set(domain.ProcessInstanceHeader, s.cfg.InstanceID)
	req.Header.Set("If-Match", `"`+request.ExpectedRevision+`"`)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("safe API: %d %s", res.Code, res.Body.String())
	}
	awaitOperation(t, s, request)
}

func TestLifecycleGracefulStopDoesNotKill(t *testing.T) {
	s, _ := lifecycleFixture(t, "stubborn")
	safeAction(t, s, "start", "manager-46")
	req := safeRequest(s, "stop", "manager-46")
	if _, err := s.SubmitLifecycle(req); err != nil {
		t.Fatal(err)
	}
	awaitState(t, s, func(v domain.ProcessSnapshot) bool { return v.Lifecycle != nil && v.Lifecycle.State == "unknown" })
	if s.Snapshot().PID == 0 {
		t.Fatal("routine stop forced termination")
	}
}

func TestLifecycleSnapshotDoesNotLeakPointers(t *testing.T) {
	s, _ := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-46")
	v := s.Snapshot()
	v.Lifecycle.Actor = "mutated"
	if s.Snapshot().Lifecycle.Actor == "mutated" {
		t.Fatal("mutable lifecycle snapshot")
	}
	time.Sleep(time.Millisecond)
}

func TestLifecycleManualStopSurvivesPublisherHoldAndHistoryRetention(t *testing.T) {
	s, p := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-30")
	stop := safeAction(t, s, "stop", "manager-58")
	for _, action := range []string{"hold-stopped", "release-stopped"} {
		_, err := s.MaintenanceAction(s.ctx, MaintenanceRequest{Action: action, InstanceID: s.cfg.InstanceID, DeploymentID: "publication", ExpectedRevision: s.Snapshot().Revision, TargetVersion: "v2", Initiator: "publisher"})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < retainedOperations+10; i++ {
		req := safeRequest(s, "restart", "manager-46")
		if _, err := s.Resolve(req); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Operation(stop.RequestID); !ok {
		t.Fatal("retention removed active manual-stop evidence")
	}
	cfg := s.cfg
	s.Close()
	restored := newTestSupervisor(t, cfg)
	restored.maintenancePeer = p
	if !restored.Snapshot().ManualStopped {
		t.Fatal("publisher cleared manual stop")
	}
	safeAction(t, restored, "start", "manager-46")
}

func TestLifecycleLateExitReconcilesOriginalStopWithoutAnotherSignal(t *testing.T) {
	s, _ := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-46")
	stop := safeAction(t, s, "stop", "manager-46")
	s.mu.Lock()
	// A real grace-period timeout retains desired=running and the active
	// maintenance fence until the owned child exits and Resolve confirms it.
	s.state.DesiredState, s.snapshot.State = "running", "failed"
	s.state.Maintenance, s.state.DeferredFence = s.state.DeferredFence, nil
	s.state.Lifecycle.State = "unknown"
	s.state.Lifecycle.Phase = "unknown"
	s.findLocked(stop.RequestID).Operation.State = "unknown"
	s.saveLifecycleLocked()
	s.mu.Unlock()
	v, err := s.Resolve(stop)
	if err != nil || v.State != "succeeded" {
		t.Fatal(v, err)
	}
	if err := v.Validate(stop); err != nil {
		t.Fatalf("terminal response is not valid for the original stop: %v; %+v", err, v)
	}
	if v.Snapshot.DesiredState != "stopped" || v.Snapshot.State != "stopped" {
		t.Fatalf("terminal snapshot retained the pre-reconciliation state: %+v", v.Snapshot)
	}
	if !s.Snapshot().ManualStopped || s.Snapshot().PID != 0 {
		t.Fatal("reconciliation restarted process")
	}
	cfg := s.cfg
	s.Close()
	restored := newTestSupervisor(t, cfg)
	stored, ok := restored.Operation(stop.RequestID)
	if !ok || stored.Validate(stop) != nil || stored.State != "succeeded" || !restored.Snapshot().ManualStopped {
		t.Fatalf("reconciled operation did not survive reload: %+v", stored)
	}
}

func TestLifecycleDrainsAcceptedWorkBeforeStoppingAndBlocksPublisher(t *testing.T) {
	s, p := lifecycleFixture(t, "graceful")
	safeAction(t, s, "start", "manager-46")
	pid := s.Snapshot().PID
	p.mu.Lock()
	p.fence.PendingPortOperations = 1
	p.mu.Unlock()
	req := safeRequest(s, "stop", "manager-30")
	if _, err := s.SubmitLifecycle(req); err != nil {
		t.Fatal(err)
	}
	awaitState(t, s, func(v domain.ProcessSnapshot) bool { return v.Lifecycle != nil && v.Lifecycle.Phase == "draining" })
	if s.Snapshot().PID != pid {
		t.Fatal("stopped before accepted transactions drained")
	}
	guard, e := lockLifecycle(filepath.Join(s.cfg.HostReleaseDir, ".rcd-host-release.lock"))
	if e == nil {
		guard.Close()
		t.Fatal("publisher bypassed active lifecycle")
	}
	p.mu.Lock()
	p.fence.PendingPortOperations = 0
	p.fence.Drained = true
	p.mu.Unlock()
	if v := awaitOperation(t, s, req); v.State != "succeeded" {
		t.Fatal(v)
	}
}
