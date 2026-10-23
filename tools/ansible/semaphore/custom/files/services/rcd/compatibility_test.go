package rcd

import (
	"strings"
	"testing"
)

func TestCompatibilityReasonFailsClosedBeforeRestore(t *testing.T) {
	lo, hi := 2, 3
	m := Manifest{Component: "manager", Target: "linux/amd64", Compatibility: Compatibility{AdapterMinVersion: "1.0.0", StateFormats: map[string]SchemaRange{"manager_db": {Min: &lo, Max: &hi}}, ComponentMinVersions: map[string]string{}}}
	d := Device{State: "checked", Components: []ComponentObservation{{Component: "manager", State: "running", RunningBuild: &BuildObservation{Target: "linux/x86_64"}}}}
	if !strings.Contains(CompatibilityReason(d, m), "数据格式检查") {
		t.Fatal("missing evidence allowed")
	}
	d.Components[0].Compatibility = &CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 3}}
	if reason := CompatibilityReason(d, m); reason != "" {
		t.Fatal(reason)
	}
	d.Components[0].Compatibility.StateFormats["manager_db"] = 4
	if !strings.Contains(CompatibilityReason(d, m), "不兼容") {
		t.Fatal("schema mismatch allowed")
	}
	d.Components[0].Compatibility.StateFormats["manager_db"] = 3
	m.Compatibility.ComponentMinVersions["agent"] = "1.1.0"
	if !strings.Contains(CompatibilityReason(d, m), "依赖组件") {
		t.Fatal("missing dependency allowed")
	}
	d.State = "stale"
	if !strings.Contains(CompatibilityReason(d, m), "过期") {
		t.Fatal("stale allowed")
	}
}

func TestAgentStoppedLowerHoldNeedsSchemaThreeButOfflineAgentDoesNotMigrate(t *testing.T) {
	lo, hi := 1, 2
	m := Manifest{Component: "agent", Target: "linux/amd64", Compatibility: Compatibility{AdapterMinVersion: "1.0.0", StateFormats: map[string]SchemaRange{"agent_state": {Min: &lo, Max: &hi}}}}
	build := &BuildObservation{Target: "linux/amd64"}
	d := Device{State: "checked", Components: []ComponentObservation{{Component: "agent", State: "running", RunningBuild: build, DiskBuild: build, Compatibility: &CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"agent_state": 2}}}, {Component: "lower", State: "stopped"}}}
	if !strings.Contains(CompatibilityReason(d, m), "格式 3") {
		t.Fatal("offline hold migration was not checked")
	}
	d.Components[0].State = "stopped"
	if reason := CompatibilityReason(d, m); reason != "" {
		t.Fatal(reason)
	}
}

func TestCommunicationPublishingRequiresStoppedIndependentService(t *testing.T) {
	m := Manifest{Component: "communicate", Target: "linux/x86_64", Compatibility: Compatibility{AdapterMinVersion: "1.0.0", StateFormats: map[string]SchemaRange{}, ComponentMinVersions: map[string]string{}}}
	build := &BuildObservation{Component: "communicate", Target: "linux/amd64", Version: "1.0.0"}
	d := Device{State: "checked", Components: []ComponentObservation{{Component: "communicate", State: "running", RunningBuild: build, DiskBuild: build, Compatibility: &CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{}}}}}
	if reason := CompatibilityReason(d, m); !strings.Contains(reason, "排空") {
		t.Fatal("running communication may not be stopped by publisher", reason)
	}
	d.Components[0].State = "stopped"
	if reason := CompatibilityReason(d, m); reason != "" {
		t.Fatal(reason)
	}
	d.Components[0].State = "not_installed"
	if reason := CompatibilityReason(d, m); reason == "" {
		t.Fatal("missing software/service profile is not permission to install a new service")
	}
	d.Components[0].State = "stopped"
	d.Components[0].Compatibility.StateFormats = nil
	if reason := CompatibilityReason(d, m); !strings.Contains(reason, "不完整") {
		t.Fatal("absent format evidence cannot masquerade as an explicit empty inventory", reason)
	}
}
