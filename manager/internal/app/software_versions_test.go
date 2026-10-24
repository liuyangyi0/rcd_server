package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
	"testing"
	"time"
)

func TestSoftwareVersionsRealAgentReadOnly(t *testing.T) {
	raw := os.Getenv("RCD_VERSION_AGENT_READONLY")
	if raw == "" {
		t.Skip("explicit read-only target list required")
	}
	var targets []struct {
		URL      string `json:"url"`
		Instance string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		t.Run(target.Instance, func(t *testing.T) {
			r := seededRuntime(t)
			source, _ := r.source("source-a")
			_, err := r.SaveSourceProcessBinding(context.Background(), domain.ProcessBinding{SourceID: source.ID, SourceRevision: source.Revision, AgentURL: target.URL, InstanceID: target.Instance}, 0)
			if err != nil {
				t.Fatal(err)
			}
			r.options.ProcessAgentClient = newProcessAgentHTTPClient()
			r.refreshSoftwareSource(context.Background(), source, "agent")
			value := findSoftware(t, r, source.ID, "agent")
			if value.State != "online" || value.Build == nil || value.Cached {
				t.Fatalf("read-only version: %+v", value)
			}
			t.Logf("GET-only agent version %s: %s", target.Instance, value.Build.Version)
		})
	}
}

func TestLowerSoftwareVersionReadAndLateEpoch(t *testing.T) {
	r := seededRuntime(t)
	changed := false
	client := &operationLowerStub{execute: func(_ context.Context, req domain.OperationRequest, epoch uint64) (json.RawMessage, error) {
		if req.Action != "status" || epoch != 7 {
			t.Errorf("not a pinned read: %+v epoch=%d", req, epoch)
		}
		if changed {
			r.mu.Lock()
			status := r.statuses["source-a"]
			status.Epoch++
			r.statuses["source-a"] = status
			r.mu.Unlock()
		}
		return json.Marshal(map[string]any{"build_info": buildinfo.ForComponent("lower")})
	}}
	installOperationsLower(t, r, client)
	source, _ := r.source("source-a")
	r.refreshSoftwareSource(context.Background(), source, "lower")
	row := findSoftware(t, r, source.ID, "lower")
	if row.Build == nil || row.Cached || row.State != "online" {
		t.Fatalf("lower: %+v", row)
	}
	changed = true
	r.refreshSoftwareSource(context.Background(), source, "lower")
	row = findSoftware(t, r, source.ID, "lower")
	if !row.Cached || row.State != "cached" {
		t.Fatalf("late epoch live: %+v", row)
	}
}

func TestLowerSoftwareVersionUnsupportedIsNotConnectionFailure(t *testing.T) {
	tests := []struct {
		name          string
		features      []string
		capabilityErr error
		readErr       error
		wantState     string
		wantCalls     int32
	}{
		{name: "configuration-only communication source", features: []string{"configuration", "configuration_forms", "configuration_set"}, wantState: "unsupported", wantCalls: 0},
		{name: "capability explicitly unsupported", capabilityErr: &domain.OperationError{Code: "unsupported", Message: "old lower"}, wantState: "unsupported", wantCalls: 0},
		{name: "status explicitly unsupported", features: []string{"acquisition"}, readErr: &domain.OperationError{Code: "unsupported", Message: "status unavailable"}, wantState: "unsupported", wantCalls: 1},
		{name: "real connection failure during capability read", capabilityErr: errors.New("connection reset"), wantState: "offline", wantCalls: 0},
		{name: "real connection failure during status read", features: []string{"acquisition"}, readErr: errors.New("connection reset"), wantState: "offline", wantCalls: 1},
		{name: "legacy v1 empty feature list", wantState: "online", wantCalls: 1},
		{name: "existing acquisition feature without a new version feature", features: []string{"acquisition", "configuration"}, wantState: "online", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := seededRuntime(t)
			client := &operationLowerStub{
				capabilities: func(context.Context, uint64) (domain.OperationsCapabilities, error) {
					return domain.OperationsCapabilities{Supported: true, SchemaVersion: 1, Features: test.features}, test.capabilityErr
				},
				execute: func(context.Context, domain.OperationRequest, uint64) (json.RawMessage, error) {
					if test.readErr != nil {
						return nil, test.readErr
					}
					return json.Marshal(map[string]any{"build_info": buildinfo.ForComponent("lower")})
				},
			}
			installOperationsLower(t, r, client)
			source, err := r.source("source-a")
			if err != nil {
				t.Fatal(err)
			}
			r.refreshSoftwareSource(context.Background(), source, "lower")
			row := findSoftware(t, r, source.ID, "lower")
			if row.State != test.wantState || client.calls.Load() != test.wantCalls {
				t.Fatalf("row=%+v calls=%d, want state=%s calls=%d", row, client.calls.Load(), test.wantState, test.wantCalls)
			}
			if test.wantState == "online" && row.Build == nil {
				t.Fatal("supported legacy source lost its verified software version")
			}
		})
	}
}

func findSoftware(t *testing.T, r *Runtime, source, role string) domain.SoftwareVersion {
	t.Helper()
	rows, err := r.SoftwareVersions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.SourceID == source && row.Component == role {
			return row
		}
	}
	t.Fatal("missing version row")
	return domain.SoftwareVersion{}
}
func TestSoftwareVersionsPersistOfflineAcrossRuntimeRestart(t *testing.T) {
	r := seededRuntime(t)
	source, _ := r.source("source-a")
	binding := bindProcessTest(t, r, source.ID)
	identity := &domain.ProcessAgentIdentity{Build: buildinfo.ForComponent("agent"), BootID: "agent-boot", StartedAt: time.Now().UTC(), ReportedAt: time.Now().UTC()}
	fail := false
	client := &processAgentStub{statusFn: func(_ context.Context, b domain.ProcessBinding) (domain.ProcessSnapshot, error) {
		if fail {
			return domain.ProcessSnapshot{}, errors.New("offline")
		}
		return domain.ProcessSnapshot{InstanceID: b.InstanceID, Revision: "r1", State: "stopped", DesiredState: "stopped", AgentIdentity: identity}, nil
	}}
	r.options.ProcessAgentClient = client
	r.refreshSoftwareSource(context.Background(), source, "agent")
	live := findSoftware(t, r, source.ID, "agent")
	if live.Build == nil || live.Cached || live.State != "online" {
		t.Fatalf("live: %+v", live)
	}
	fail = true
	r.refreshSoftwareSource(context.Background(), source, "agent")
	offline := findSoftware(t, r, source.ID, "agent")
	if offline.Build == nil || !offline.Cached || offline.State != "offline" || !offline.ObservedAt.Equal(live.ObservedAt) {
		t.Fatalf("offline: %+v", offline)
	}
	restarted, err := NewRuntime(context.Background(), r.repo, r.options)
	if err != nil {
		t.Fatal(err)
	}
	cached := findSoftware(t, restarted, source.ID, "agent")
	if cached.Build == nil || !cached.Cached || cached.State != "cached" {
		t.Fatalf("restart: %+v", cached)
	}
	// Returned objects cannot mutate the next response or stored identity.
	cached.Build.Version = "tampered"
	if findSoftware(t, restarted, source.ID, "agent").Build.Version == "tampered" {
		t.Fatal("identity aliased")
	}
	binding.AgentURL = "http://replacement:8090"
	if _, err = r.SaveSourceProcessBinding(context.Background(), binding, binding.Revision); err != nil {
		t.Fatal(err)
	}
	if got := findSoftware(t, r, source.ID, "agent"); got.Build != nil {
		t.Fatalf("old identity applied to replacement: %+v", got)
	}
	if client.actions.Load() != 0 {
		t.Fatal("version query dispatched process control")
	}
}

func TestSoftwareVersionLateBindingResponseCannotPersist(t *testing.T) {
	r := seededRuntime(t)
	source, _ := r.source("source-a")
	binding := bindProcessTest(t, r, source.ID)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r.options.ProcessAgentClient = &processAgentStub{statusFn: func(_ context.Context, b domain.ProcessBinding) (domain.ProcessSnapshot, error) {
		close(entered)
		<-release
		identity := &domain.ProcessAgentIdentity{Build: buildinfo.ForComponent("agent"), BootID: "old-agent", StartedAt: time.Now(), ReportedAt: time.Now()}
		return domain.ProcessSnapshot{InstanceID: b.InstanceID, Revision: "r1", State: "stopped", DesiredState: "stopped", AgentIdentity: identity}, nil
	}}
	go func() { defer close(done); r.refreshSoftwareSource(context.Background(), source, "agent") }()
	<-entered
	updated := binding
	updated.AgentURL = "http://replacement:8090"
	if _, err := r.SaveSourceProcessBinding(context.Background(), updated, binding.Revision); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if _, err := r.repo.LoadSoftwareVersion(context.Background(), source.ID, "agent", softwareKey(source, "agent", binding)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("late identity persisted: %v", err)
	}
}

func TestSoftwareVersionsLegacyAndInvalidAreNotFabricated(t *testing.T) {
	r := seededRuntime(t)
	source, _ := r.source("source-a")
	bindProcessTest(t, r, source.ID)
	r.options.ProcessAgentClient = &processAgentStub{}
	r.refreshSoftwareSource(context.Background(), source, "agent")
	if got := findSoftware(t, r, source.ID, "agent"); got.Build != nil || got.State != "unsupported" {
		t.Fatalf("legacy: %+v", got)
	}
	if err := domain.ValidateSoftwareBuild(buildinfo.ForComponent("manager"), "agent"); err == nil {
		t.Fatal("wrong component accepted")
	}
	lower := buildinfo.ForComponent("lower")
	row := domain.SoftwareVersion{SourceID: source.ID, Component: "lower", Build: &lower, ObservedAt: time.Now().UTC()}
	if err := r.repo.SaveSoftwareVersion(context.Background(), source, domain.ProcessBinding{}, softwareKey(source, "lower", domain.ProcessBinding{}), row); err != nil {
		t.Fatal(err)
	}
	if got := findSoftware(t, r, source.ID, "lower"); !got.Cached || got.Build == nil {
		t.Fatalf("lower offline cache: %+v", got)
	}
	changed := source
	changed.Endpoint = "opc.tcp://replacement:4841"
	if softwareKey(changed, "lower", domain.ProcessBinding{}) == softwareKey(source, "lower", domain.ProcessBinding{}) {
		t.Fatal("endpoint not bound")
	}
}
