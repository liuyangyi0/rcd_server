//go:build linux

package processagent

import (
	"encoding/json"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
	"testing"
)

func TestAgentRunningIdentityIsSeparateFreshAndCopied(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	s := newTestSupervisor(t, cfg)
	first := s.Snapshot()
	if first.AgentIdentity == nil || first.Validate(cfg.InstanceID) != nil || first.AgentIdentity.Build != buildinfo.ForComponent("agent") {
		t.Fatalf("invalid identity: %+v", first)
	}
	if first.StartedAt != nil || first.PID != 0 {
		t.Fatal("child should not be started for version reporting")
	}
	saved := *first.AgentIdentity
	first.AgentIdentity.Build.Version = "mutated-client-copy"
	first.AgentIdentity.BootID = "mutated"
	next := s.Snapshot()
	if next.AgentIdentity.Build != saved.Build || next.AgentIdentity.BootID != saved.BootID || !next.AgentIdentity.StartedAt.Equal(saved.StartedAt) {
		t.Fatal("snapshot aliases supervisor identity")
	}
	if next.AgentIdentity.ReportedAt.Before(saved.ReportedAt) {
		t.Fatal("report time did not advance")
	}
	response := agentHTTP(t, s, "GET", "/api/v1/process", cfg.InstanceID, nil)
	var wire domain.ProcessSnapshot
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &wire) != nil || wire.AgentIdentity == nil || wire.AgentIdentity.Build != saved.Build {
		t.Fatal(response.Body.String())
	}
	revision := s.Snapshot().Revision
	_ = agentHTTP(t, s, "GET", "/api/v1/process", cfg.InstanceID, nil)
	if s.Snapshot().Revision != revision {
		t.Fatal("identity read mutated control CAS revision")
	}
	s.Close()
	restored := newTestSupervisor(t, cfg)
	after := restored.Snapshot().AgentIdentity
	if after.BootID == saved.BootID || !after.StartedAt.After(saved.StartedAt) || after.Build != saved.Build {
		t.Fatalf("agent restart reused old identity: %+v", after)
	}
}
