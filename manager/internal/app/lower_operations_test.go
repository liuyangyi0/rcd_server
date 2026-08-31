package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

type operationLowerStub struct {
	fakeLowerClient
	execute func(context.Context, domain.OperationRequest, uint64) (json.RawMessage, error)
	calls   atomic.Int32
}

func (*operationLowerStub) OperationsCapabilities(context.Context, uint64) (domain.OperationsCapabilities, error) {
	return domain.OperationsCapabilities{Supported: true, SchemaVersion: 1, Features: []string{"acquisition", "configuration", "logs"}}, nil
}
func (f *operationLowerStub) ExecuteOperation(ctx context.Context, request domain.OperationRequest, epoch uint64) (json.RawMessage, error) {
	f.calls.Add(1)
	if f.execute != nil {
		return f.execute(ctx, request, epoch)
	}
	return json.RawMessage(`{"revision":"2","status":"Applied","source_id":"spoof","source_revision":999,"ports":[]}`), nil
}
func installOperationsLower(t *testing.T, r *Runtime, f *operationLowerStub) uint64 {
	t.Helper()
	r.mu.Lock()
	r.lowers["source-a"] = lowerHandle{client: f, generation: 3}
	r.statuses["source-a"] = connectedStatus("source-a", 3<<32|7)
	r.mu.Unlock()
	source, err := r.source("source-a")
	if err != nil {
		t.Fatal(err)
	}
	return source.Revision
}
func portOperation() domain.OperationRequest {
	enabled := false
	return domain.OperationRequest{Action: "set_port_enabled", PortID: "COM1", Enabled: &enabled, ExpectedRevision: "1"}
}

func TestOperationsPinPhysicalSourceAndOverwriteUntrustedIdentity(t *testing.T) {
	r := seededRuntime(t)
	f := &operationLowerStub{execute: func(_ context.Context, request domain.OperationRequest, epoch uint64) (json.RawMessage, error) {
		if epoch != 7 || request.PortID != "COM1" {
			t.Errorf("epoch=%d request=%+v", epoch, request)
		}
		return json.RawMessage(`{"source_id":"other","source_revision":999,"revision":"2","ports":[]}`), nil
	}}
	revision := installOperationsLower(t, r, f)
	if err := r.openSession("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.selectSource("session", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	result, err := r.ExecuteSourceOperation(context.Background(), "source-a", revision, portOperation())
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		SourceID       string `json:"source_id"`
		SourceRevision uint64 `json:"source_revision"`
	}
	if json.Unmarshal(result, &identity) != nil || identity.SourceID != "source-a" || identity.SourceRevision != revision || f.calls.Load() != 1 {
		t.Fatalf("result=%s calls=%d", result, f.calls.Load())
	}
	capabilities, err := r.SourceOperationsCapabilities(context.Background(), "source-a")
	if err != nil || capabilities.SourceRevision != revision || capabilities.SourceID != "source-a" || !capabilities.Supported {
		t.Fatalf("capabilities=%+v err=%v", capabilities, err)
	}
}

func TestOperationsRejectStaleSourceRevisionAndCommandPending(t *testing.T) {
	r := seededRuntime(t)
	f := &operationLowerStub{}
	revision := installOperationsLower(t, r, f)
	_, err := r.ExecuteSourceOperation(context.Background(), "source-a", revision+1, portOperation())
	var conflict *domain.RevisionConflictError
	if !errors.As(err, &conflict) || f.calls.Load() != 0 {
		t.Fatalf("stale=%v calls=%d", err, f.calls.Load())
	}
	if err := r.openSession("operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.routes.BeginCommand("operator", "line", "COM1", "Write"); err != nil {
		t.Fatal(err)
	}
	_, err = r.ExecuteSourceOperation(context.Background(), "source-a", revision, portOperation())
	if !errors.Is(err, domain.ErrCommandPending) || f.calls.Load() != 0 {
		t.Fatalf("pending=%v calls=%d", err, f.calls.Load())
	}
	r.mu.Lock()
	catalog := r.catalogs["source-a"]
	catalog.Topology = &domain.SourceTopology{Ports: []domain.TopologyPort{{PortID: "/dev/COM1", SendNodeID: "COM1_Send"}}}
	r.catalogs["source-a"] = catalog
	r.mu.Unlock()
	request := portOperation()
	request.PortID = "/dev/COM1"
	_, err = r.ExecuteSourceOperation(context.Background(), "source-a", revision, request)
	if !errors.Is(err, domain.ErrCommandPending) || f.calls.Load() != 0 {
		t.Fatalf("physical ID bypassed normalized command lock: %v", err)
	}
}

func TestOperationsDoNotHoldConfigurationLockAcrossLowerIO(t *testing.T) {
	r := seededRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f := &operationLowerStub{execute: func(context.Context, domain.OperationRequest, uint64) (json.RawMessage, error) {
		close(entered)
		<-release
		return json.RawMessage(`{"revision":"2","status":"Pending","ports":[]}`), nil
	}}
	revision := installOperationsLower(t, r, f)
	done := make(chan error, 1)
	go func() {
		_, err := r.ExecuteSourceOperation(context.Background(), "source-a", revision, portOperation())
		done <- err
	}()
	defer close(release)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not enter")
	}
	checkBusy := func(name string, err error) {
		t.Helper()
		var operationErr *domain.OperationError
		if !errors.As(err, &operationErr) || operationErr.Code != "busy" {
			t.Fatalf("%s=%v", name, err)
		}
	}
	_, err := r.ExecuteSourceOperation(context.Background(), "source-a", revision, portOperation())
	checkBusy("second operation", err)
	_, err = r.RestartSource(context.Background(), "source-a", revision, false)
	checkBusy("reconnect", err)
	checkBusy("delete", r.DeleteSource(context.Background(), "source-a", revision))
	source, _ := r.source("source-a")
	_, err = r.UpdateSource(context.Background(), source.ID, revision, api.SourceMutation{ID: source.ID, GroupID: source.GroupID, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: source.Enabled})
	checkBusy("update", err)
	group, _ := r.GetGroup(context.Background(), "line")
	checkBusy("delete group", r.DeleteGroup(context.Background(), "line", group.Revision))
	opened := make(chan error, 1)
	go func() { opened <- r.openSession("during-maintenance") }()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow lower blocked Session creation")
	}
	// Release in defer even on a failed assertion, then wait before repository
	// cleanup so no background audit accesses a closed SQLite connection.
	t.Cleanup(func() {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("operation did not finish")
		}
	})
}

func TestOperationsAuditOnlyHashesContentAndDoesNotRetryTimeout(t *testing.T) {
	r := seededRuntime(t)
	f := &operationLowerStub{execute: func(context.Context, domain.OperationRequest, uint64) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	}}
	revision := installOperationsLower(t, r, f)
	content := "a_private_test_value_that_must_not_be_audited"
	_, err := r.ExecuteSourceOperation(context.Background(), "source-a", revision, domain.OperationRequest{Action: "write_config", Name: "config.toml", Content: &content, ExpectedRevision: "hash"})
	var operationErr *domain.OperationError
	if !errors.As(err, &operationErr) || operationErr.Code != "timeout" || f.calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, f.calls.Load())
	}
	page, err := r.ListManagementAudit(context.Background(), api.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), content) || !strings.Contains(string(encoded), "content_sha256") || !strings.Contains(string(encoded), `"outcome":"unknown"`) {
		t.Fatalf("audit=%s", encoded)
	}
	// The failed call releases its admission guard, without issuing a retry.
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if err := r.checkSourceOperationLocked("source-a"); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsAuditCorrelatesCommitNotLaterSnapshot(t *testing.T) {
	r := seededRuntime(t)
	f := &operationLowerStub{execute: func(context.Context, domain.OperationRequest, uint64) (json.RawMessage, error) {
		return json.RawMessage(`{"status":"Pending","revision":"4","operation_revision":"3","ports":[]}`), nil
	}}
	revision := installOperationsLower(t, r, f)
	if _, err := r.ExecuteSourceOperation(context.Background(), "source-a", revision, portOperation()); err != nil {
		t.Fatal(err)
	}
	page, err := r.ListManagementAudit(context.Background(), api.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"lower_revision":"3"`, `"operation_revision":"3"`, `"snapshot_revision":"4"`, `"outcome":"pending"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("audit missing %s: %s", field, encoded)
		}
	}
}
