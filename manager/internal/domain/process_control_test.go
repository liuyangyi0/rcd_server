package domain

import (
	"strings"
	"testing"
	"time"
)

func TestProcessBindingValidatesFixedAgentIdentity(t *testing.T) {
	for _, binding := range []ProcessBinding{
		{}, {AgentURL: "http://127.0.0.1:8086", InstanceID: "lower-a"}, {AgentURL: "https://agent.local/", InstanceID: "instance:1"},
	} {
		if err := binding.Validate(); err != nil {
			t.Fatalf("%+v: %v", binding, err)
		}
	}
	for _, binding := range []ProcessBinding{
		{AgentURL: "http://agent"}, {InstanceID: "only-id"}, {AgentURL: "file:///tmp/rcd", InstanceID: "x"},
		{AgentURL: "http://user:password@agent", InstanceID: "x"}, {AgentURL: "http://agent/path", InstanceID: "x"},
		{AgentURL: "http://agent?target=other", InstanceID: "x"}, {AgentURL: "http://agent#fragment", InstanceID: "x"},
		{AgentURL: "http://agent", InstanceID: "bad id"},
	} {
		if binding.Validate() == nil {
			t.Fatalf("accepted %+v", binding)
		}
	}
}

func TestProcessActionRequiresExactStableTokens(t *testing.T) {
	valid := ProcessActionRequest{RequestID: "req-1", InstanceID: "lower-a", Action: "restart", ExpectedRevision: "epoch:3"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ProcessActionRequest){
		func(r *ProcessActionRequest) { r.RequestID = "" }, func(r *ProcessActionRequest) { r.RequestID = ".." },
		func(r *ProcessActionRequest) { r.RequestID = "../late" }, func(r *ProcessActionRequest) { r.InstanceID = "" },
		func(r *ProcessActionRequest) { r.Action = "reboot" }, func(r *ProcessActionRequest) { r.ExpectedRevision = "*" },
		func(r *ProcessActionRequest) { r.ExpectedRevision = strings.Repeat("x", 129) },
	} {
		candidate := valid
		mutate(&candidate)
		if candidate.Validate() == nil {
			t.Fatalf("accepted %+v", candidate)
		}
	}
}

func TestProcessOperationOnlyConfirmedTargetStateIsSuccess(t *testing.T) {
	now := time.Now().UTC()
	request := ProcessActionRequest{RequestID: "req", InstanceID: "lower", Action: "restart", ExpectedRevision: "1"}
	valid := ProcessOperation{RequestID: "req", InstanceID: "lower", Action: "restart", State: "succeeded", CreatedAt: now, UpdatedAt: now,
		Snapshot: &ProcessSnapshot{InstanceID: "lower", Revision: "2", State: "running", DesiredState: "running", PID: 99}}
	if err := valid.Validate(request); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ProcessOperation){
		func(o *ProcessOperation) { o.InstanceID = "other" }, func(o *ProcessOperation) { o.RequestID = "other" },
		func(o *ProcessOperation) { o.Action = "start" }, func(o *ProcessOperation) { o.State = "done" },
		func(o *ProcessOperation) { o.Snapshot = nil }, func(o *ProcessOperation) { o.Snapshot.State = "starting" },
		func(o *ProcessOperation) { o.Snapshot.DesiredState = "stopped" }, func(o *ProcessOperation) { o.Snapshot.PID = 0 },
		func(o *ProcessOperation) { o.Snapshot.SchemaVersion = 99 }, func(o *ProcessOperation) { o.Snapshot.InstanceID = "other" },
		func(o *ProcessOperation) { o.Snapshot.Revision = "" }, func(o *ProcessOperation) { o.UpdatedAt = now.Add(-time.Second) },
		func(o *ProcessOperation) { o.Error = strings.Repeat("x", 16385) },
	} {
		candidate := valid
		snapshot := *valid.Snapshot
		candidate.Snapshot = &snapshot
		mutate(&candidate)
		if candidate.Validate(request) == nil {
			t.Fatalf("accepted %+v", candidate)
		}
	}
	for _, state := range []string{"accepted", "running", "unknown"} {
		candidate := valid
		candidate.State = state
		candidate.Snapshot = nil
		if candidate.Terminal() || candidate.Validate(request) != nil {
			t.Fatalf("pending %s", state)
		}
	}
	valid.State = "failed"
	valid.Snapshot = nil
	valid.Forced = true
	if !valid.Terminal() || valid.Validate(request) != nil {
		t.Fatal("forced failed result must remain inspectable")
	}
	request.Action = "stop"
	valid.Action = "stop"
	valid.State = "succeeded"
	valid.Snapshot = &ProcessSnapshot{InstanceID: "lower", Revision: "3", State: "stopped", DesiredState: "stopped"}
	if err := valid.Validate(request); err != nil {
		t.Fatal(err)
	}
}
