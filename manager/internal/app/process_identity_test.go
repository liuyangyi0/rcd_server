package app

import (
	"context"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
	"testing"
	"time"
)

func TestProcessStatusForwardsOnlyMatchingAgentIdentity(t *testing.T) {
	r := seededRuntime(t)
	identity := &domain.ProcessAgentIdentity{Build: buildinfo.ForComponent("agent"), BootID: "boot-agent", StartedAt: time.Now().UTC(), ReportedAt: time.Now().UTC()}
	agent := &processAgentStub{statusFn: func(_ context.Context, b domain.ProcessBinding) (domain.ProcessSnapshot, error) {
		return domain.ProcessSnapshot{InstanceID: b.InstanceID, Revision: "r1", State: "stopped", DesiredState: "stopped", AgentIdentity: identity}, nil
	}}
	r.options.ProcessAgentClient = agent
	bindProcessTest(t, r, "source-a")
	got, err := r.SourceProcessStatus(context.Background(), "source-a")
	if err != nil || got.AgentIdentity == nil || got.AgentIdentity.Build != identity.Build {
		t.Fatalf("identity lost: %+v %v", got, err)
	}
	identity.Build.Component = "manager"
	if _, err := r.SourceProcessStatus(context.Background(), "source-a"); processErrorCode(err) != "invalid_response" {
		t.Fatalf("wrong identity accepted: %v", err)
	}
	identity = nil
	got, err = r.SourceProcessStatus(context.Background(), "source-a")
	if err != nil || got.AgentIdentity != nil {
		t.Fatalf("legacy identity invented: %+v %v", got, err)
	}
	if agent.actions.Load() != 0 {
		t.Fatal("read dispatched process action")
	}
}
