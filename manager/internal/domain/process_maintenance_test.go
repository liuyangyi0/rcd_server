package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProcessMaintenanceSnapshotCompatibility(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		enabled   *bool
		id, phase string
		invalid   bool
	}{
		{name: "legacy"},
		{name: "disabled", enabled: &no},
		{name: "enabled idle", enabled: &yes},
		{name: "active old agent", id: "release-a", phase: "stopped"},
		{name: "future phase", enabled: &yes, id: "release-a", phase: "future_phase"},
		{name: "partial id", id: "release-a", invalid: true},
		{name: "partial phase", phase: "unknown", invalid: true},
		{name: "contradiction", enabled: &no, id: "release-a", phase: "unknown", invalid: true},
		{name: "oversized", id: strings.Repeat("a", 129), phase: "ready", invalid: true},
		{name: "invalid phase", id: "release-a", phase: "bad\nphase", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := ProcessSnapshot{InstanceID: "agent-a", Revision: "r1", State: "stopped", DesiredState: "stopped", MaintenanceEnabled: tc.enabled, MaintenanceDeploymentID: tc.id, MaintenancePhase: tc.phase}
			if err := snap.Validate("agent-a"); (err != nil) != tc.invalid {
				t.Fatalf("invalid=%v, error=%v", tc.invalid, err)
			}
			raw, err := json.Marshal(snap)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"maintenance_enabled"`) != (tc.enabled != nil) {
				t.Fatalf("optional capability lost: %s", raw)
			}
		})
	}
}
