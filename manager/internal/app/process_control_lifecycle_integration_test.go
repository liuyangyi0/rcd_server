//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/processagent"
	"rcd_manager/internal/store"
)

// Use the production HTTP client and Agent handler, including its remote-write
// ACL. No lower process, OPC server, or serial port is started by this fixture.
type lifecycleHandlerTransport struct {
	handler     http.Handler
	before      func(*http.Request)
	dropResolve atomic.Bool
	requests    atomic.Int32
	actions     atomic.Int32
	resolutions atomic.Int32
}

func (p *lifecycleHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.requests.Add(1)
	if p.before != nil {
		p.before(req)
	}
	if strings.HasSuffix(req.URL.Path, "/actions") {
		p.actions.Add(1)
	}
	resolve := strings.HasSuffix(req.URL.Path, "/resolve")
	if resolve {
		p.resolutions.Add(1)
	}
	remote := req.Clone(req.Context())
	remote.RemoteAddr = "192.168.10.30:32000" // explicitly not loopback
	recorder := httptest.NewRecorder()
	p.handler.ServeHTTP(recorder, remote)
	response := recorder.Result()
	if resolve && p.dropResolve.Swap(false) {
		response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, nil
}

func lifecycleHTTPFixture(t *testing.T, binding domain.ProcessBinding, original *domain.ProcessActionRequest) (*processagent.Supervisor, *processAgentHTTPClient, *lifecycleHandlerTransport) {
	t.Helper()
	root := t.TempDir()
	host := filepath.Join(root, "host")
	data := filepath.Join(root, "state")
	for _, directory := range []string{filepath.Join(host, ".rcd-host-control"), data} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if original != nil {
		created := time.Now().UTC().Add(-2 * time.Second)
		exited := created.Add(time.Second)
		operation := domain.ProcessOperation{RequestID: original.RequestID, InstanceID: original.InstanceID,
			Action: original.Action, State: "unknown", CreatedAt: created, UpdatedAt: created,
			ErrorCode: "lifecycle_unconfirmed", Error: "exit confirmation timed out"}
		// Persist the same evidence used by Agent's late-exit reconciliation:
		// the owned process exited after this request, but the result is unknown.
		state := map[string]any{
			"schema_version": 4, "instance_id": binding.InstanceID, "desired_state": "running", "counter": 1,
			"operations":    []any{map[string]any{"request": original, "operation": operation}},
			"last_snapshot": domain.ProcessSnapshot{LastExitAt: &exited},
			"lifecycle": map[string]any{"request_id": original.RequestID, "manager_instance_id": original.ManagerInstanceID,
				"actor": original.Actor, "action": original.Action, "state": "unknown", "phase": "unknown", "fence_id": "routine.late-exit"},
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "process-state.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := processagent.New(processagent.Config{InstanceID: binding.InstanceID, GatewayInstanceID: "lifecycle-test-gateway",
		Executable: executable, WorkDir: root, DataDir: data, HTTPListen: "127.0.0.1:0", RestartPolicy: "never",
		MaintenanceEndpoint: "opc.tcp://127.0.0.1:1", HostReleaseDir: host, RemoteManagers: "192.168.10.30",
		StopTimeout: time.Second, StartTimeout: time.Second, StartGrace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Close)
	transport := &lifecycleHandlerTransport{handler: agent.Handler()}
	client := newProcessAgentHTTPClient().(*processAgentHTTPClient)
	client.client.Transport = transport
	return agent, client, transport
}

func TestProcessLifecycleUnknownUsesOriginalResolveOverRemoteHTTP(t *testing.T) {
	for _, scenario := range []string{"poll", "explicit_resolve", "lost_resolve_reply"} {
		t.Run(scenario, func(t *testing.T) {
			r := seededRuntime(t)
			binding := bindProcessTest(t, r, "source-a")
			req := domain.ProcessActionRequest{RequestID: "late-exit", InstanceID: binding.InstanceID, Action: "stop", ExpectedRevision: "original.1",
				Lifecycle: true, ManagerInstanceID: r.release.InstanceID, Actor: "operator", GatewayInstanceID: "lifecycle-test-gateway"}
			agent, client, transport := lifecycleHTTPFixture(t, binding, &req)
			r.options.ProcessAgentClient = client
			op, ok := agent.Operation(req.RequestID)
			if !ok || op.State != "unknown" {
				t.Fatalf("missing unknown fixture: %+v", op)
			}
			record := store.ProcessControlRecord{Binding: binding, Request: req, Operation: op, Dispatched: true, Actor: req.Actor}
			record.Operation = bindProcessOperation(op, record)
			if err := r.repo.CreateProcessOperation(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if err := r.maintenance.restoreFence(binding.SourceID, req.RequestID); err != nil {
				t.Fatal(err)
			}
			transport.dropResolve.Store(scenario == "lost_resolve_reply")
			var result domain.ProcessOperation
			var err error
			if scenario == "explicit_resolve" {
				input := processTestInput(binding, req.RequestID)
				input.Action = req.Action
				result, err = r.ResolveSourceProcess(context.Background(), binding.SourceID, req.ExpectedRevision, input)
			} else {
				result, err = r.SourceProcessOperation(context.Background(), binding.SourceID, req.RequestID)
			}
			if scenario == "lost_resolve_reply" {
				if err != nil || result.State != "unknown" {
					t.Fatalf("lost reply must remain unknown: %+v %v", result, err)
				}
				assertProcessFence(t, r, binding.SourceID, true)
				result, err = r.SourceProcessOperation(context.Background(), binding.SourceID, req.RequestID)
			}
			if err != nil || result.State != "succeeded" || result.Snapshot == nil || !result.Snapshot.ManualStopped {
				t.Fatalf("late exit did not reconcile: %+v %v", result, err)
			}
			assertProcessFence(t, r, binding.SourceID, false)
			if transport.actions.Load() != 0 || transport.resolutions.Load() != 1 || agent.Snapshot().PID != 0 {
				t.Fatalf("reconciliation repeated an action: actions=%d resolves=%d", transport.actions.Load(), transport.resolutions.Load())
			}
		})
	}
}

func TestProcessLifecycleResolveFirstSealsLocallyDespiteRemoteLegacyACL(t *testing.T) {
	r := seededRuntime(t)
	binding := bindProcessTest(t, r, "source-a")
	agent, client, transport := lifecycleHTTPFixture(t, binding, nil)
	r.options.ProcessAgentClient = client
	revision := agent.Snapshot().Revision
	input := processTestInput(binding, "not-delivered")
	legacy := domain.ProcessActionRequest{RequestID: input.RequestID, InstanceID: binding.InstanceID, Action: input.Action, ExpectedRevision: revision}
	if _, err := client.Resolve(context.Background(), binding, legacy); processErrorCode(err) != "unsupported" {
		t.Fatalf("fixture bypassed the remote legacy ACL: %v", err)
	}
	before := transport.requests.Load()
	op, err := r.ResolveSourceProcess(context.Background(), binding.SourceID, revision, input)
	if err != nil || op.State != "failed" || op.ErrorCode != "unconfirmed_cancelled" {
		t.Fatalf("local seal: %+v %v", op, err)
	}
	assertProcessFence(t, r, binding.SourceID, false)
	record, err := r.repo.GetProcessOperation(context.Background(), binding.SourceID, input.RequestID)
	if err != nil || record.Dispatched {
		t.Fatalf("seal was marked dispatched: %+v %v", record, err)
	}
	// Completion is durable and still blocks a delayed original POST after a
	// manager recreation; it needs neither Agent connectivity nor a new identity.
	restored, err := NewRuntime(context.Background(), r.repo, RuntimeOptions{ProcessAgentClient: client})
	if err != nil {
		t.Fatal(err)
	}
	op, err = restored.ExecuteSourceProcess(context.Background(), binding.SourceID, revision, input)
	if err != nil || op.State != "failed" || op.ErrorCode != "unconfirmed_cancelled" || transport.requests.Load() != before {
		t.Fatalf("late Action contacted Agent: %+v %v requests=%d", op, err, transport.requests.Load())
	}
	if _, found := agent.Operation(input.RequestID); found || agent.Snapshot().PID != 0 {
		t.Fatal("local cancellation reached the process owner")
	}
}

func TestProcessLifecycleResolveWinsWhileOriginalPrepareIsInFlight(t *testing.T) {
	r := seededRuntime(t)
	binding := bindProcessTest(t, r, "source-a")
	agent, client, transport := lifecycleHTTPFixture(t, binding, nil)
	r.options.ProcessAgentClient = client
	revision := agent.Snapshot().Revision
	input := processTestInput(binding, "prepare-race")
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	transport.before = func(req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/api/v1/process" {
			close(entered)
			select {
			case <-release:
			case <-req.Context().Done():
			}
		}
	}
	done := make(chan error, 1)
	go func() {
		op, err := r.ExecuteSourceProcess(context.Background(), binding.SourceID, revision, input)
		if err == nil && (op.State != "failed" || op.ErrorCode != "unconfirmed_cancelled") {
			err = errors.New("late Prepare did not return the durable cancellation")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("original request did not reach Prepare")
	}
	op, err := r.ResolveSourceProcess(context.Background(), binding.SourceID, revision, input)
	if err != nil || op.State != "failed" || op.ErrorCode != "unconfirmed_cancelled" {
		t.Fatalf("resolve during Prepare: %+v %v", op, err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("original request did not finish")
	}
	assertProcessFence(t, r, binding.SourceID, false)
	if transport.actions.Load() != 0 || transport.resolutions.Load() != 0 {
		t.Fatal("resolve/Prepare race dispatched a remote write")
	}
}
