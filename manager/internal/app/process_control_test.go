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
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

type processAgentStub struct {
	statusFn    func(context.Context, domain.ProcessBinding) (domain.ProcessSnapshot, error)
	actionFn    func(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error)
	operationFn func(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error)
	resolveFn   func(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error)
	logFn       func(context.Context, domain.ProcessBinding, int) (domain.ProcessLog, error)
	actions     atomic.Int32
	queries     atomic.Int32
	resolves    atomic.Int32
}

func (s *processAgentStub) Status(ctx context.Context, b domain.ProcessBinding) (domain.ProcessSnapshot, error) {
	if s.statusFn != nil {
		return s.statusFn(ctx, b)
	}
	return domain.ProcessSnapshot{InstanceID: b.InstanceID, Revision: "1", State: "stopped", DesiredState: "stopped"}, nil
}
func (s *processAgentStub) Action(ctx context.Context, b domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	s.actions.Add(1)
	if s.actionFn != nil {
		return s.actionFn(ctx, b, req)
	}
	return processStubOperation(req, "accepted"), nil
}
func (s *processAgentStub) Operation(ctx context.Context, b domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	s.queries.Add(1)
	if s.operationFn != nil {
		return s.operationFn(ctx, b, req)
	}
	return processStubOperation(req, "succeeded"), nil
}
func (s *processAgentStub) Resolve(ctx context.Context, b domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	s.resolves.Add(1)
	if s.resolveFn != nil {
		return s.resolveFn(ctx, b, req)
	}
	op := processStubOperation(req, "failed")
	op.ErrorCode = "unconfirmed_cancelled"
	return op, nil
}
func (s *processAgentStub) Log(ctx context.Context, b domain.ProcessBinding, limit int) (domain.ProcessLog, error) {
	if s.logFn != nil {
		return s.logFn(ctx, b, limit)
	}
	return domain.ProcessLog{InstanceID: b.InstanceID, Text: "log", SizeBytes: 3}, nil
}

func processStubOperation(req domain.ProcessActionRequest, state string) domain.ProcessOperation {
	now := time.Now().UTC()
	op := domain.ProcessOperation{RequestID: req.RequestID, InstanceID: req.InstanceID, Action: req.Action, State: state, CreatedAt: now, UpdatedAt: now}
	if state == "succeeded" {
		value, pid := "running", 123
		if req.Action == "stop" {
			value, pid = "stopped", 0
		}
		op.Snapshot = &domain.ProcessSnapshot{InstanceID: req.InstanceID, Revision: "2", State: value, DesiredState: value, PID: pid}
	}
	return op
}

func bindProcessTest(t *testing.T, r *Runtime, id string) domain.ProcessBinding {
	t.Helper()
	source, err := r.source(id)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := r.SaveSourceProcessBinding(context.Background(), domain.ProcessBinding{SourceID: id, SourceRevision: source.Revision, AgentURL: "http://" + id + ":8086", InstanceID: id + "-instance"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func processTestInput(binding domain.ProcessBinding, id string) api.ProcessActionInput {
	return api.ProcessActionInput{Action: "restart", RequestID: id, SourceRevision: binding.SourceRevision, BindingRevision: binding.Revision}
}

func processErrorCode(err error) string {
	var result *domain.OperationError
	if errors.As(err, &result) {
		return result.Code
	}
	return ""
}

func assertProcessFence(t *testing.T, r *Runtime, id string, want bool) {
	t.Helper()
	r.configMu.RLock()
	err := r.checkProcessMaintenanceLocked(id)
	r.configMu.RUnlock()
	if (err != nil) != want {
		t.Fatalf("maintenance fence %s=%v, want %v", id, err, want)
	}
}

func TestProcessControlWorksForOfflineAndManagerDisabledPhysicalSource(t *testing.T) {
	r := seededRuntime(t)
	agent := &processAgentStub{}
	r.options.ProcessAgentClient = agent
	if _, err := r.UpdateSource(context.Background(), "source-b", 1, api.SourceMutation{Name: "B", GroupID: "line", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-a", "source-b"} {
		binding := bindProcessTest(t, r, id)
		status, err := r.SourceProcessStatus(context.Background(), id)
		if err != nil || status.SourceID != id || status.SourceRevision != binding.SourceRevision || status.State != "stopped" {
			t.Fatalf("status %+v %v", status, err)
		}
		input := processTestInput(binding, id+"-start")
		input.Action = "start"
		operation, err := r.ExecuteSourceProcess(context.Background(), id, "1", input)
		if err != nil || operation.State != "accepted" || operation.SourceID != id || operation.InstanceID != binding.InstanceID {
			t.Fatalf("start %+v %v", operation, err)
		}
		assertProcessFence(t, r, id, true)
		operation, err = r.SourceProcessOperation(context.Background(), id, input.RequestID)
		if err != nil || operation.State != "succeeded" {
			t.Fatalf("finish %+v %v", operation, err)
		}
		assertProcessFence(t, r, id, false)
	}
	if agent.actions.Load() != 2 {
		t.Fatalf("dispatch count %d", agent.actions.Load())
	}
}

func TestProcessMaintenanceAndAllPortCommandsExcludeEachOther(t *testing.T) {
	r := seededRuntime(t)
	agent := &processAgentStub{}
	r.options.ProcessAgentClient = agent
	binding := bindProcessTest(t, r, "source-a")
	input := processTestInput(binding, "restart")
	lower := &operationLowerStub{}
	installOperationsLower(t, r, lower)
	if err := r.openSession("session"); err != nil {
		t.Fatal(err)
	}
	command, err := r.routes.BeginCommand("session", "line", "OTHER_PHYSICAL_PORT", "Write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); !errors.Is(err, domain.ErrCommandPending) || agent.actions.Load() != 0 {
		t.Fatalf("pending command accepted restart %v", err)
	}
	if _, err := r.routes.FinishCommand(command.ID, domain.CommandReceived, domain.StatusGood, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); err != nil {
		t.Fatal(err)
	}
	if status, err := r.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "01 06"); status != domain.StatusBadWaitingForResponse || processErrorCode(err) != "busy" {
		t.Fatalf("Write admitted %x %v", status, err)
	}
	if _, status, err := r.CallCommand(context.Background(), "session", testKey("COM1_SendCommand"), "01 06"); status != domain.StatusBadWaitingForResponse || processErrorCode(err) != "busy" {
		t.Fatalf("Call admitted %x %v", status, err)
	}
	if _, err := r.ExecuteSourceOperation(context.Background(), "source-a", binding.SourceRevision, portOperation()); processErrorCode(err) != "busy" || lower.calls.Load() != 0 {
		t.Fatalf("acquisition admitted %v", err)
	}
	if _, err := r.SaveSourceProcessBinding(context.Background(), binding, binding.Revision); processErrorCode(err) != "busy" {
		t.Fatalf("binding admitted %v", err)
	}
	mutation := api.SourceMutation{Name: "Changed", GroupID: "line", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}
	if _, err := r.UpdateSource(context.Background(), "source-a", 1, mutation); processErrorCode(err) != "busy" {
		t.Fatalf("source update admitted %v", err)
	}
	if err := r.DeleteSource(context.Background(), "source-a", 1); processErrorCode(err) != "busy" {
		t.Fatalf("source delete admitted %v", err)
	}
	if _, err := r.RestartSource(context.Background(), "source-a", 1, false); processErrorCode(err) != "busy" {
		t.Fatalf("OPC reconnect admitted %v", err)
	}
	if r.routes.SourcePendingCount("source-a") != 0 {
		t.Fatal("rejected command left a pending lock")
	}
	if _, err := r.selectSource("session", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	client.writeHook = func() {
		r.OnSample("source-b", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	}
	r.mu.Lock()
	r.lowers["source-b"] = lowerHandle{client: client, generation: 1}
	r.statuses["source-b"] = connectedStatus("source-b", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-b", "COM1", 1)
	if _, status, err := r.CallCommand(context.Background(), "session", testKey("COM1_SendCommand"), "01 06"); err != nil || status != domain.StatusGood {
		t.Fatalf("unrelated physical source blocked %x %v", status, err)
	}
}

func TestProcessBlockedAgentIODoesNotHoldConfigurationLock(t *testing.T) {
	r := seededRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	agent := &processAgentStub{actionFn: func(ctx context.Context, _ domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return processStubOperation(req, "accepted"), nil
	}}
	r.options.ProcessAgentClient = agent
	binding := bindProcessTest(t, r, "source-a")
	done := make(chan error, 1)
	go func() {
		_, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", processTestInput(binding, "blocked"))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("agent not entered")
	}
	read := make(chan error, 1)
	go func() { read <- r.openSession("independent") }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("OPC activation blocked by process HTTP I/O")
	}
	if _, err := r.SourceProcessBinding(context.Background(), "source-b"); err != nil {
		t.Fatal(err)
	}
	assertProcessFence(t, r, "source-a", true)
	// Let deferred release wake the dispatch before repository cleanup.
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("dispatch did not finish")
		}
	})
}

func TestProcessTimeout404ResolveAndLateOriginalPostNeverRedispatch(t *testing.T) {
	r := seededRuntime(t)
	agent := &processAgentStub{
		actionFn: func(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error) {
			return domain.ProcessOperation{}, &domain.OperationError{Code: "timeout", Message: "lost response"}
		},
		operationFn: func(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error) {
			return domain.ProcessOperation{}, &domain.OperationError{Code: "not_found", Message: "not accepted yet"}
		},
	}
	r.options.ProcessAgentClient = agent
	binding := bindProcessTest(t, r, "source-a")
	input := processTestInput(binding, "lost")
	op, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input)
	if processErrorCode(err) != "timeout" || op.State != "unknown" {
		t.Fatalf("timeout %+v %v", op, err)
	}
	assertProcessFence(t, r, "source-a", true)
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); err != nil {
		t.Fatal(err)
	}
	if agent.actions.Load() != 1 {
		t.Fatal("duplicate Action was redispatched")
	}
	op, err = r.SourceProcessOperation(context.Background(), "source-a", input.RequestID)
	if err != nil || op.State != "failed" || op.ErrorCode != "unconfirmed_cancelled" || agent.resolves.Load() != 1 {
		t.Fatalf("resolve %+v %v", op, err)
	}
	assertProcessFence(t, r, "source-a", false)
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); err != nil || agent.actions.Load() != 1 {
		t.Fatalf("late Action %v calls %d", err, agent.actions.Load())
	}
	entries, _, err := r.repo.ListAudit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, entry := range entries {
		if request, ok := entry.Details["request_id"].(string); ok && request == input.RequestID {
			kinds[entry.Kind]++
		}
	}
	for _, kind := range []string{"source.process.accepted", "source.process.unknown", "source.process.terminal"} {
		if kinds[kind] != 1 {
			t.Fatalf("audit %s=%d all=%v", kind, kinds[kind], kinds)
		}
	}
}

func TestProcessManagerResolveBeforeOriginalPostFreezesExactFingerprint(t *testing.T) {
	r := seededRuntime(t)
	agent := &processAgentStub{}
	r.options.ProcessAgentClient = agent
	binding := bindProcessTest(t, r, "source-a")
	input := processTestInput(binding, "not-arrived")
	input.ExpectedConfigRevisions = map[string]string{"config.toml": strings.Repeat("a", 64)}
	if _, err := r.SourceProcessOperation(context.Background(), "source-a", input.RequestID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing operation=%v", err)
	}
	op, err := r.ResolveSourceProcess(context.Background(), "source-a", "1", input)
	if err != nil || !op.Terminal() || agent.actions.Load() != 0 || agent.resolves.Load() != 1 {
		t.Fatalf("manager resolve %+v %v", op, err)
	}
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); err != nil || agent.actions.Load() != 0 {
		t.Fatalf("late Action dispatched %v", err)
	}
	changed := input
	changed.ExpectedConfigRevisions = map[string]string{"config.toml": strings.Repeat("b", 64)}
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", changed); processErrorCode(err) != "conflict" {
		t.Fatalf("changed input reused ID %v", err)
	}
	changed = input
	changed.BindingRevision++
	if _, err := r.ResolveSourceProcess(context.Background(), "source-a", "1", changed); processErrorCode(err) != "conflict" {
		t.Fatalf("changed binding reused ID %v", err)
	}
}

func TestProcessUnknownAgentUnavailableKeepsFenceAcrossManagerRecreation(t *testing.T) {
	r := seededRuntime(t)
	agent := &processAgentStub{operationFn: func(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error) {
		return domain.ProcessOperation{}, &domain.OperationError{Code: "unavailable", Message: "agent offline"}
	}}
	r.options.ProcessAgentClient = agent
	binding := bindProcessTest(t, r, "source-a")
	input := processTestInput(binding, "restart-recovery")
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRuntime(context.Background(), r.repo, RuntimeOptions{ProcessAgentClient: agent})
	if err != nil {
		t.Fatal(err)
	}
	assertProcessFence(t, restored, "source-a", true)
	op, err := restored.SourceProcessOperation(context.Background(), "source-a", input.RequestID)
	if err != nil || op.State != "unknown" || agent.actions.Load() != 1 {
		t.Fatalf("restored op %+v %v actions=%d", op, err, agent.actions.Load())
	}
	assertProcessFence(t, restored, "source-a", true)
	agent.operationFn = nil
	op, err = restored.SourceProcessOperation(context.Background(), "source-a", input.RequestID)
	if err != nil || !op.Terminal() || agent.actions.Load() != 1 {
		t.Fatalf("final reconcile %+v %v", op, err)
	}
	assertProcessFence(t, restored, "source-a", false)
}

func TestProcessNeverDispatchedJournalFailsClosedOnManagerRestart(t *testing.T) {
	r := seededRuntime(t)
	agent := &processAgentStub{}
	binding := bindProcessTest(t, r, "source-a")
	input := processTestInput(binding, "preflight-crash")
	req := domain.ProcessActionRequest{RequestID: input.RequestID, InstanceID: binding.InstanceID, Action: input.Action, ExpectedRevision: "1"}
	op := processStubOperation(req, "accepted")
	op.SourceID, op.SourceRevision, op.BindingRevision = "source-a", binding.SourceRevision, binding.Revision
	if err := r.repo.CreateProcessOperation(context.Background(), store.ProcessControlRecord{Binding: binding, Request: req, Operation: op}); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRuntime(context.Background(), r.repo, RuntimeOptions{ProcessAgentClient: agent})
	if err != nil {
		t.Fatal(err)
	}
	assertProcessFence(t, restored, "source-a", false)
	op, err = restored.ExecuteSourceProcess(context.Background(), "source-a", "1", input)
	if err != nil || op.State != "failed" || op.ErrorCode != "interrupted_before_dispatch" || agent.actions.Load() != 0 || agent.queries.Load() != 0 {
		t.Fatalf("unsafe startup replay %+v %v", op, err)
	}
}

func TestProcessRestartConfigPreconditionPinsEpochAndRejectsChangedSavedFile(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{true: "match", false: "changed"}[matching], func(t *testing.T) {
			r := seededRuntime(t)
			agent := &processAgentStub{}
			r.options.ProcessAgentClient = agent
			binding := bindProcessTest(t, r, "source-a")
			want, actual := strings.Repeat("a", 64), strings.Repeat("b", 64)
			if matching {
				actual = want
			}
			lower := &operationLowerStub{execute: func(_ context.Context, request domain.OperationRequest, epoch uint64) (json.RawMessage, error) {
				if request.Action != "list_configs" || epoch != 7 {
					t.Errorf("preflight %+v epoch=%d", request, epoch)
				}
				assertProcessFence(t, r, "source-a", true)
				return json.RawMessage(`{"items":[{"name":"config.toml","revision":"` + actual + `"}]}`), nil
			}}
			installOperationsLower(t, r, lower)
			input := processTestInput(binding, "apply")
			input.ExpectedConfigRevisions = map[string]string{"config.toml": want}
			op, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input)
			if matching {
				if err != nil || op.State != "accepted" || agent.actions.Load() != 1 {
					t.Fatalf("matching %+v %v", op, err)
				}
			} else {
				if processErrorCode(err) != "conflict" || !op.Terminal() || agent.actions.Load() != 0 {
					t.Fatalf("changed %+v %v", op, err)
				}
				assertProcessFence(t, r, "source-a", false)
			}
		})
	}
}

func TestProcessConfigPreflightBlocksOtherSavesButNotUnrelatedSession(t *testing.T) {
	r := seededRuntime(t)
	r.options.ProcessAgentClient = &processAgentStub{}
	binding := bindProcessTest(t, r, "source-a")
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	lower := &operationLowerStub{execute: func(ctx context.Context, _ domain.OperationRequest, _ uint64) (json.RawMessage, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{"items":[{"name":"config.toml","revision":"` + strings.Repeat("a", 64) + `"}]}`), nil
	}}
	installOperationsLower(t, r, lower)
	input := processTestInput(binding, "apply-locked")
	input.ExpectedConfigRevisions = map[string]string{"config.toml": strings.Repeat("a", 64)}
	done := make(chan error, 1)
	go func() { _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", input); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("preflight not entered")
	}
	content := "draft"
	if _, err := r.ExecuteSourceOperation(context.Background(), "source-a", binding.SourceRevision, domain.OperationRequest{Action: "write_config", Name: "config.toml", Content: &content, ExpectedRevision: strings.Repeat("a", 64)}); processErrorCode(err) != "busy" {
		t.Fatalf("save crossed maintenance fence %v", err)
	}
	if err := r.openSession("during-preflight"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("preflight did not finish")
		}
	})
}

func TestProcessInvalidResponseAndTerminalPersistenceFailureKeepFence(t *testing.T) {
	for _, kind := range []string{"invalid_response", "persistence"} {
		t.Run(kind, func(t *testing.T) {
			r := seededRuntime(t)
			agent := &processAgentStub{}
			r.options.ProcessAgentClient = agent
			binding := bindProcessTest(t, r, "source-a")
			agent.actionFn = func(_ context.Context, _ domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
				op := processStubOperation(req, "succeeded")
				if kind == "invalid_response" {
					op.InstanceID = "wrong-instance"
				} else {
					if err := r.repo.Close(); err != nil {
						t.Error(err)
					}
				}
				return op, nil
			}
			_, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", processTestInput(binding, kind))
			if err == nil {
				t.Fatal("expected failure")
			}
			assertProcessFence(t, r, "source-a", true)
		})
	}
}

func TestProcessWorkerShutdownCancelsReconciliationWithoutActionRetry(t *testing.T) {
	r := seededRuntime(t)
	entered := make(chan struct{})
	agent := &processAgentStub{operationFn: func(ctx context.Context, _ domain.ProcessBinding, _ domain.ProcessActionRequest) (domain.ProcessOperation, error) {
		close(entered)
		<-ctx.Done()
		return domain.ProcessOperation{}, ctx.Err()
	}}
	r.options.ProcessAgentClient = agent
	r.options.LowerFactory = func(opc.LowerConfig, opc.LowerSink) (opc.LowerClient, error) { return &contextLowerClient{}, nil }
	binding := bindProcessTest(t, r, "source-a")
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", processTestInput(binding, "shutdown")); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not restore pending intent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if agent.actions.Load() != 1 {
		t.Fatal("shutdown retried Action")
	}
}

func TestProcessConcurrentResolveAndOriginalActionUseOneAdmission(t *testing.T) {
	for _, resolveFirst := range []bool{false, true} {
		t.Run(map[bool]string{true: "resolve_first", false: "action_first"}[resolveFirst], func(t *testing.T) {
			r := seededRuntime(t)
			entered, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			block := func(ctx context.Context, _ domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				state := "accepted"
				if resolveFirst {
					state = "failed"
				}
				return processStubOperation(req, state), nil
			}
			agent := &processAgentStub{}
			if resolveFirst {
				agent.resolveFn = block
			} else {
				agent.actionFn = block
			}
			r.options.ProcessAgentClient = agent
			binding := bindProcessTest(t, r, "source-a")
			input := processTestInput(binding, "same-id")
			done := make(chan error, 1)
			go func() {
				var err error
				if resolveFirst {
					_, err = r.ResolveSourceProcess(context.Background(), "source-a", "1", input)
				} else {
					_, err = r.ExecuteSourceProcess(context.Background(), "source-a", "1", input)
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("first admission did not reach agent")
			}
			var op domain.ProcessOperation
			var err error
			if resolveFirst {
				op, err = r.ExecuteSourceProcess(context.Background(), "source-a", "1", input)
			} else {
				op, err = r.ResolveSourceProcess(context.Background(), "source-a", "1", input)
			}
			if err != nil || op.State != "accepted" {
				t.Fatalf("second admission %+v %v", op, err)
			}
			if resolveFirst && agent.actions.Load() != 0 || !resolveFirst && agent.resolves.Load() != 0 {
				t.Fatal("second endpoint dispatched a side effect")
			}
			close(release)
			released = true
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first admission did not complete")
			}
		})
	}
}

func TestProcessMaintenanceAlsoProtectsGroupDeleteImportAndDeviceRebinding(t *testing.T) {
	r := seededRuntime(t)
	r.options.ProcessAgentClient = &processAgentStub{}
	binding := bindProcessTest(t, r, "source-a")
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", processTestInput(binding, "group-fence")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteGroup(context.Background(), "line", 2); processErrorCode(err) != "busy" {
		t.Fatalf("group delete crossed process fence %v", err)
	}
	document, err := r.ExportConfiguration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ImportConfiguration(context.Background(), api.ConfigurationImportRequest{Document: document}); !errors.Is(err, domain.ErrSourceInUse) {
		t.Fatalf("import crossed existing configuration %v", err)
	}
	mutation := api.SourceMutation{Name: "A", GroupID: "line", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
		GroupBindings: []domain.GroupBinding{{PortID: "COM1", DeviceID: "1", GroupID: "line"}}}
	if _, err := r.UpdateSource(context.Background(), "source-a", binding.SourceRevision, mutation); processErrorCode(err) != "busy" {
		t.Fatalf("device rebinding crossed fence %v", err)
	}
	if _, err := r.SourceProcessBinding(context.Background(), "source-a"); err != nil {
		t.Fatalf("source disappeared: %v", err)
	}
	assertProcessFence(t, r, "source-a", true)
}

func TestProcessSlowStatusAndLogRetainCapturedBindingIdentity(t *testing.T) {
	for _, kind := range []string{"status", "log"} {
		t.Run(kind, func(t *testing.T) {
			r := seededRuntime(t)
			entered, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			agent := &processAgentStub{}
			agent.statusFn = func(ctx context.Context, b domain.ProcessBinding) (domain.ProcessSnapshot, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return domain.ProcessSnapshot{InstanceID: b.InstanceID, Revision: "1", State: "stopped", DesiredState: "stopped"}, nil
			}
			agent.logFn = func(ctx context.Context, b domain.ProcessBinding, _ int) (domain.ProcessLog, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return domain.ProcessLog{InstanceID: b.InstanceID, Text: "old", SizeBytes: 3}, nil
			}
			r.options.ProcessAgentClient = agent
			binding := bindProcessTest(t, r, "source-a")
			type response struct {
				source, instance                string
				sourceRevision, bindingRevision uint64
				err                             error
			}
			done := make(chan response, 1)
			go func() {
				if kind == "status" {
					v, err := r.SourceProcessStatus(context.Background(), "source-a")
					done <- response{v.SourceID, v.InstanceID, v.SourceRevision, v.BindingRevision, err}
				} else {
					v, err := r.SourceProcessLog(context.Background(), "source-a", 64)
					done <- response{v.SourceID, v.InstanceID, v.SourceRevision, v.BindingRevision, err}
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("read not entered")
			}
			changed := binding
			changed.AgentURL = "http://other-agent:8086"
			changed.InstanceID = "replacement-instance"
			if _, err := r.SaveSourceProcessBinding(context.Background(), changed, binding.Revision); err != nil {
				t.Fatal(err)
			}
			mutation := api.SourceMutation{Name: "Renamed", GroupID: "line", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}
			if _, err := r.UpdateSource(context.Background(), "source-a", binding.SourceRevision, mutation); err != nil {
				t.Fatal(err)
			}
			close(release)
			released = true
			select {
			case got := <-done:
				if got.err != nil || got.source != "source-a" || got.instance != binding.InstanceID || got.sourceRevision != binding.SourceRevision || got.bindingRevision != binding.Revision {
					t.Fatalf("old response mislabeled %+v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("read did not finish")
			}
		})
	}
}

func TestProcessWorkerConfirmsHealthySourceWhileAnotherAgentIsOffline(t *testing.T) {
	r := seededRuntime(t)
	blocked, other := make(chan struct{}), make(chan struct{})
	agent := &processAgentStub{operationFn: func(ctx context.Context, b domain.ProcessBinding, req domain.ProcessActionRequest) (domain.ProcessOperation, error) {
		if b.SourceID == "source-a" {
			close(blocked)
			<-ctx.Done()
			return domain.ProcessOperation{}, ctx.Err()
		}
		close(other)
		return processStubOperation(req, "succeeded"), nil
	}}
	r.options.ProcessAgentClient = agent
	r.options.LowerFactory = func(opc.LowerConfig, opc.LowerSink) (opc.LowerClient, error) { return &contextLowerClient{}, nil }
	for _, id := range []string{"source-a", "source-b"} {
		binding := bindProcessTest(t, r, id)
		if _, err := r.ExecuteSourceProcess(context.Background(), id, "1", processTestInput(binding, "parallel-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("offline agent not entered")
	}
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("healthy agent starved behind offline source")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	assertProcessFence(t, r, "source-b", false)
	assertProcessFence(t, r, "source-a", true)
}

func TestProcessPendingOfflineLowerRejectsMutationAsConflictButReadAsUnavailable(t *testing.T) {
	r := seededRuntime(t)
	r.options.ProcessAgentClient = &processAgentStub{}
	binding := bindProcessTest(t, r, "source-a")
	if _, err := r.ExecuteSourceProcess(context.Background(), "source-a", "1", processTestInput(binding, "offline-fence")); err != nil {
		t.Fatal(err)
	}
	content := "draft"
	request := domain.OperationRequest{Action: "write_config", Name: "config.toml", Content: &content, ExpectedRevision: strings.Repeat("a", 64)}
	if _, err := r.ExecuteSourceOperation(context.Background(), "source-a", binding.SourceRevision, request); processErrorCode(err) != "busy" {
		t.Fatalf("offline mutation should keep 409 maintenance semantics: %v", err)
	}
	if _, err := r.ExecuteSourceOperation(context.Background(), "source-a", 0, domain.OperationRequest{Action: "list_configs"}); processErrorCode(err) != "unavailable" {
		t.Fatalf("read should remain a normal availability check: %v", err)
	}
	if _, err := r.ExecuteSourceOperation(context.Background(), "source-a", binding.SourceRevision+1, request); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("source CAS must precede dispatch/fence: %v", err)
	}
	assertProcessFence(t, r, "source-a", true)
}
