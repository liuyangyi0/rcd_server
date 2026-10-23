//go:build linux

package processagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

// Deterministic fault injection only; this never stands in for hardware acceptance.
type fakeMaintenancePeer struct {
	mu                             sync.Mutex
	fence                          LowerFence
	failRead, loseEnter, loseLeave bool
	calls                          []string
	beforeChange                   func()
	configurations                 map[string]string
	configurationChecks            int
}

func (p *fakeMaintenancePeer) ValidateConfiguration(_ context.Context, fence LowerFence, expected map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configurationChecks++
	if !p.fence.Active || !p.fence.Drained || p.fence.DeploymentID != fence.DeploymentID || p.fence.FencingToken != fence.FencingToken {
		return errors.New("configuration check escaped maintenance")
	}
	for name, hash := range expected {
		if p.configurations[name] != hash {
			return &domain.OperationError{Code: "conflict", Message: "configuration changed before maintenance"}
		}
	}
	return nil
}

func (p *fakeMaintenancePeer) Read(context.Context) (LowerFence, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failRead {
		return LowerFence{}, errors.New("injected disconnected lower")
	}
	return p.fence, nil
}
func (p *fakeMaintenancePeer) Change(_ context.Context, action, id, token, boot string) (LowerFence, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.beforeChange != nil {
		p.beforeChange()
	}
	p.calls = append(p.calls, action)
	if boot != p.fence.BootID {
		return LowerFence{}, errors.New("wrong boot")
	}
	p.fence.DeploymentID, p.fence.FencingToken = id, token
	p.fence.Active = action == "enter_maintenance"
	p.fence.AdmissionBlocked = p.fence.Active
	p.fence.Drained = p.fence.Active && p.fence.PendingAdmissions == 0 && p.fence.PendingPortOperations == 0
	if action == "enter_maintenance" && p.loseEnter || action == "leave_maintenance" && p.loseLeave {
		return LowerFence{}, errors.New("injected lost response after commit")
	}
	return p.fence, nil
}
func maintenanceFixture(t *testing.T) (*Supervisor, *fakeMaintenancePeer) {
	t.Helper()
	cfg := supervisorConfig(t, "graceful")
	cfg.MaintenanceEndpoint = "opc.tcp://127.0.0.1:14991"
	cfg.GatewayInstanceID = "test-gateway"
	s := newTestSupervisor(t, cfg)
	p := &fakeMaintenancePeer{fence: LowerFence{SchemaVersion: 1, GatewayInstanceID: cfg.GatewayInstanceID, FencingToken: "0", BootID: "boot-a", Durable: true}}
	s.maintenancePeer = p
	return s, p
}
func maintenanceReq(s *Supervisor, action string) MaintenanceRequest {
	return MaintenanceRequest{Action: action, InstanceID: s.cfg.InstanceID, DeploymentID: "job-a", ExpectedRevision: s.Snapshot().Revision, TargetVersion: "1.0.1", Initiator: "operator"}
}

func TestMaintenanceProcessSnapshotCapability(t *testing.T) {
	legacy := newTestSupervisor(t, supervisorConfig(t, "graceful"))
	plain := legacy.Snapshot()
	if plain.MaintenanceEnabled == nil || *plain.MaintenanceEnabled {
		t.Fatal("unconfigured agent must explicitly report disabled")
	}
	s, _ := maintenanceFixture(t)
	before := s.Snapshot()
	if before.MaintenanceEnabled == nil || !*before.MaintenanceEnabled || before.MaintenancePhase != "" {
		t.Fatal("configured idle capability missing")
	}
	*before.MaintenanceEnabled = false
	if !*s.Snapshot().MaintenanceEnabled {
		t.Fatal("snapshot pointer leaks mutable capability")
	}
	enterMaintenance(t, s)
	active := s.Snapshot()
	if active.MaintenanceDeploymentID != "job-a" || active.MaintenancePhase == "" || active.Validate(s.cfg.InstanceID) != nil {
		t.Fatalf("invalid active snapshot: %+v", active)
	}
}
func enterMaintenance(t *testing.T, s *Supervisor) MaintenanceSnapshot {
	t.Helper()
	value, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "enter"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func ownedRequest(s *Supervisor, action string) domain.ProcessActionRequest {
	job := s.Maintenance().Job
	return domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: action, ExpectedRevision: s.Snapshot().Revision, DeploymentID: job.DeploymentID, FencingToken: job.FencingToken}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var op *domain.OperationError
	if !errors.As(err, &op) || op.Code != code {
		t.Fatalf("want %s got %v", code, err)
	}
}

func TestMaintenancePersistsIntentBeforeLowerAndBlocksUnownedProcess(t *testing.T) {
	s, p := maintenanceFixture(t)
	p.beforeChange = func() {
		raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "process-state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var state durableState
		if json.Unmarshal(raw, &state) != nil || state.SchemaVersion != 2 || state.Maintenance == nil || state.Maintenance.FencingToken != "1" {
			t.Fatal("lower mutation preceded durable agent token/schema")
		}
	}
	value := enterMaintenance(t, s)
	if value.Job.Phase != "ready" || !value.Job.LowerConfirmed {
		t.Fatalf("%+v", value)
	}
	for _, action := range []string{"start", "stop", "restart"} {
		_, err := s.Submit(domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: action, ExpectedRevision: s.Snapshot().Revision})
		requireCode(t, err, "busy")
	}
	wrong := ownedRequest(s, "stop")
	wrong.FencingToken = "2"
	_, err := s.Submit(wrong)
	requireCode(t, err, "conflict")
	other := maintenanceReq(s, "enter")
	other.DeploymentID = "job-b"
	_, err = s.MaintenanceAction(context.Background(), other)
	requireCode(t, err, "conflict")
}

func TestMaintenanceLostEnterReplyReconcilesWithoutMutationReplay(t *testing.T) {
	s, p := maintenanceFixture(t)
	p.loseEnter = true
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "enter"))
	requireCode(t, err, "operation_failed")
	if s.Maintenance().Job.Phase != "unknown" {
		t.Fatal("uncertainty cleared")
	}
	value, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	if err != nil || value.Job.Phase != "ready" {
		t.Fatal(value, err)
	}
	if len(p.calls) != 1 {
		t.Fatal("reconcile replayed entry")
	}
}

func TestMaintenanceCompletedReconcileRequiresFreshLowerEvidence(t *testing.T) {
	s, p := maintenanceFixture(t)
	enterMaintenance(t, s)
	if _, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "complete")); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.failRead = true
	changes := len(p.calls)
	p.mu.Unlock()
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	requireCode(t, err, "operation_failed")
	if s.Maintenance().Job.Phase != "unknown" {
		t.Fatal("cached completion disguised disconnected lower")
	}
	p.mu.Lock()
	p.failRead = false
	p.mu.Unlock()
	value, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	if err != nil || value.Job.Phase != "completed" {
		t.Fatal(value, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != changes {
		t.Fatal("reconcile replayed a lower mutation")
	}
}

func TestMaintenanceDrainingRejectsStopAndFinishUntilReady(t *testing.T) {
	s, p := maintenanceFixture(t)
	p.fence.PendingAdmissions = 1
	enterMaintenance(t, s)
	_, err := s.Submit(ownedRequest(s, "stop"))
	requireCode(t, err, "busy")
	_, err = s.MaintenanceAction(context.Background(), maintenanceReq(s, "complete"))
	requireCode(t, err, "busy")
	if len(p.calls) != 1 {
		t.Fatal("pending operations were bypassed")
	}
	p.mu.Lock()
	p.fence.PendingAdmissions = 0
	p.fence.Drained = true
	p.mu.Unlock()
	req := ownedRequest(s, "start")
	if _, err = s.Submit(req); err != nil {
		t.Fatal(err)
	}
	if awaitOperation(t, s, req).State != "succeeded" {
		t.Fatal("owned start failed")
	}
	pid := s.Snapshot().PID
	req = ownedRequest(s, "stop")
	if _, err = s.Submit(req); err != nil {
		t.Fatal(err)
	}
	if awaitOperation(t, s, req).State != "succeeded" || s.Snapshot().PID != 0 || pid <= 1 {
		t.Fatal("owned stop failed")
	}
	value, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "complete"))
	if err != nil || value.Job.Phase != "completed" {
		t.Fatal(value, err)
	}
	_, err = s.Submit(ownedRequest(s, "start"))
	requireCode(t, err, "conflict")
}

func TestMaintenanceReleaseReplyLostRemainsBlockedUntilReadReconcile(t *testing.T) {
	s, p := maintenanceFixture(t)
	enterMaintenance(t, s)
	p.loseLeave = true
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "complete"))
	requireCode(t, err, "operation_failed")
	_, err = s.Submit(ownedRequest(s, "start"))
	requireCode(t, err, "conflict")
	value, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	if err != nil || value.Job.Phase != "completed" {
		t.Fatal(value, err)
	}
	if len(p.calls) != 2 {
		t.Fatal("release was repeated")
	}
	stale := maintenanceReq(s, "enter")
	_, err = s.MaintenanceAction(context.Background(), stale)
	requireCode(t, err, "conflict")
	next := maintenanceReq(s, "enter")
	next.DeploymentID = "job-b"
	value, err = s.MaintenanceAction(context.Background(), next)
	if err != nil || value.Job.FencingToken != "2" {
		t.Fatal(value, err)
	}
	stale = maintenanceReq(s, "complete")
	_, err = s.MaintenanceAction(context.Background(), stale)
	requireCode(t, err, "conflict")
}

func TestMaintenanceRestartRetainsJobSuppressesBootAndRetry(t *testing.T) {
	s, p := maintenanceFixture(t)
	start := submitAction(t, s, "start")
	awaitOperation(t, s, start)
	enterMaintenance(t, s)
	cfg := s.cfg
	s.Close()
	cfg.AutoStart = true
	cfg.RestartPolicy = "on-failure"
	restarted := newTestSupervisor(t, cfg)
	restarted.maintenancePeer = p
	if restarted.Snapshot().PID != 0 || restarted.Maintenance().Job.Phase != "unknown" {
		t.Fatal("maintenance restart auto-started the lower")
	}
	restarted.mu.Lock()
	restarted.retryAt = time.Now().Add(-time.Second)
	restarted.mu.Unlock()
	restarted.observeExitAndRetry()
	if restarted.Snapshot().PendingRequestID != "" {
		t.Fatal("maintenance allowed automatic retry")
	}
	_, err := restarted.Submit(ownedRequest(restarted, "start"))
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, restarted, func(s domain.ProcessSnapshot) bool { return s.State == "running" })
}

func TestMaintenanceStorageFailureHasNoLowerSideEffect(t *testing.T) {
	s, p := maintenanceFixture(t)
	path := filepath.Join(s.cfg.DataDir, "process-state.json")
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	} // rename cannot replace a directory
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "enter"))
	requireCode(t, err, "persistence_failed")
	if len(p.calls) != 0 {
		t.Fatal("storage failure still contacted lower mutation")
	}
	_, err = s.Submit(ownedRequest(s, "start"))
	if err == nil {
		t.Fatal("storage failure allowed process operation")
	}
}

func TestMaintenanceIdentityAndBootEvidenceRejectsMismatchedPeer(t *testing.T) {
	s, p := maintenanceFixture(t)
	p.fence.GatewayInstanceID = "wrong"
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "enter"))
	requireCode(t, err, "operation_failed")
	if len(p.calls) != 0 {
		t.Fatal("mismatched gateway received mutation")
	}
}

func TestMaintenanceContradictoryFenceRevokesCachedStartPermission(t *testing.T) {
	s, p := maintenanceFixture(t)
	enterMaintenance(t, s)
	p.mu.Lock()
	p.fence.DeploymentID = "another-job"
	p.fence.FencingToken = "2"
	p.mu.Unlock()
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	requireCode(t, err, "operation_failed")
	if s.Maintenance().Job.LowerConfirmed {
		t.Fatal("contradicted proof still permits start")
	}
	_, err = s.Submit(ownedRequest(s, "start"))
	requireCode(t, err, "conflict")
	p.mu.Lock()
	p.fence.DeploymentID = "job-a"
	p.fence.FencingToken = "1"
	p.mu.Unlock()
	value, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	if err != nil || !value.Job.LowerConfirmed {
		t.Fatal(value, err)
	}
}

func TestMaintenanceHTTPRequiresCASAndFixedBinding(t *testing.T) {
	s, _ := maintenanceFixture(t)
	send := func(body string, match string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/actions", strings.NewReader(body))
		req.Header.Set(domain.ProcessInstanceHeader, s.cfg.InstanceID)
		req.Header.Set("If-Match", match)
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		return out.Code
	}
	request := maintenanceReq(s, "enter")
	raw, _ := json.Marshal(request)
	if code := send(string(raw), ""); code != 409 {
		t.Fatal(code)
	}
	if code := send(strings.TrimSuffix(string(raw), "}")+`,"endpoint":"opc.tcp://other:4841"}`, request.ExpectedRevision); code != 400 {
		t.Fatal(code)
	}
	if code := send(string(raw), request.ExpectedRevision); code != 200 {
		t.Fatal(code)
	}
	if code := send(string(raw), request.ExpectedRevision); code != 409 {
		t.Fatal(code)
	}
	process, _ := json.Marshal(ownedRequest(s, "start"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/process/actions", strings.NewReader(string(process)))
	req.Header.Set(domain.ProcessInstanceHeader, s.cfg.InstanceID)
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	if out.Code != 409 {
		t.Fatal("maintenance process request omitted If-Match", out.Code)
	}
}

type waitingMaintenancePeer struct{ entered chan struct{} }

func (p waitingMaintenancePeer) Read(ctx context.Context) (LowerFence, error) {
	close(p.entered)
	<-ctx.Done()
	return LowerFence{}, ctx.Err()
}
func (p waitingMaintenancePeer) Change(context.Context, string, string, string, string) (LowerFence, error) {
	return LowerFence{}, errors.New("must never mutate")
}

func TestMaintenanceCloseJoinsInFlightWriterBeforeReleasingInstanceLock(t *testing.T) {
	s, _ := maintenanceFixture(t)
	peer := waitingMaintenancePeer{entered: make(chan struct{})}
	s.maintenancePeer = peer
	req := maintenanceReq(s, "enter")
	done := make(chan error, 1)
	go func() { _, err := s.MaintenanceAction(context.Background(), req); done <- err }()
	select {
	case <-peer.entered:
	case <-time.After(time.Second):
		t.Fatal("handshake did not start")
	}
	_, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	requireCode(t, err, "busy")
	s.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("late writer outlived agent close")
	}
	path := filepath.Join(s.cfg.DataDir, "process-state.json")
	before, _ := os.ReadFile(path)
	req = maintenanceReq(s, "enter")
	_, err = s.MaintenanceAction(context.Background(), req)
	requireCode(t, err, "operation_failed")
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("closed agent still wrote durable state")
	}
	restored := newTestSupervisor(t, s.cfg)
	if restored.Maintenance().Job.Phase != "unknown" {
		t.Fatal("restart cleared unresolved job")
	}
}

func TestMaintenanceBindingCannotBeRemovedOrRedirectedWhileActive(t *testing.T) {
	s, _ := maintenanceFixture(t)
	enterMaintenance(t, s)
	cfg := s.cfg
	s.Close()
	for _, change := range []func(*Config){
		func(c *Config) { c.MaintenanceEndpoint = ""; c.GatewayInstanceID = "" },
		func(c *Config) { c.GatewayInstanceID = "other-gateway" },
		func(c *Config) { c.MaintenanceEndpoint = "opc.tcp://127.0.0.1:14992" },
		func(c *Config) { c.Arguments = append(c.Arguments, "changed") },
	} {
		test := cfg
		test.Arguments = append([]string(nil), cfg.Arguments...)
		change(&test)
		instance, err := New(test)
		if err == nil {
			instance.Close()
			t.Fatal("active maintenance binding was changed")
		}
	}
}
