package domain

import (
	"rcd_manager/internal/buildinfo"
	"strings"
	"testing"
	"time"
)

func TestProcessAgentIdentityValidation(t *testing.T) {
	base := ProcessAgentIdentity{Build: buildinfo.ForComponent("agent"), BootID: "agent-boot", StartedAt: time.Now().UTC(), ReportedAt: time.Now().UTC()}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ProcessAgentIdentity){
		"wrong role":         func(v *ProcessAgentIdentity) { v.Build.Component = "manager" },
		"unsupported schema": func(v *ProcessAgentIdentity) { v.Build.SchemaVersion = 2 },
		"empty version":      func(v *ProcessAgentIdentity) { v.Build.Version = "" },
		"oversized commit":   func(v *ProcessAgentIdentity) { v.Build.Commit = strings.Repeat("a", 257) },
		"control character":  func(v *ProcessAgentIdentity) { v.Build.ReleaseID = "bad\nvalue" },
		"invalid boot":       func(v *ProcessAgentIdentity) { v.BootID = "bad/boot" },
		"missing report":     func(v *ProcessAgentIdentity) { v.ReportedAt = time.Time{} },
		"missing start":      func(v *ProcessAgentIdentity) { v.StartedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if value.Validate() == nil {
				t.Fatal("accepted invalid identity")
			}
		})
	}
	base.ReportedAt = base.StartedAt.Add(-time.Hour)
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}
