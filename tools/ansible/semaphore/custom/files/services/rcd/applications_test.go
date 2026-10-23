// SPDX-License-Identifier: MIT
package rcd

import (
	"strings"
	"testing"
)

func TestRCDApplicationDefinitionFixesAllExecutionTargets(t *testing.T) {
	req := ApplicationRequest{Component: "weather-reader", Name: "气象软件", Arguments: []string{"--listen=127.0.0.1:9001"}, GracefulShutdownConfirmed: true}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	a := applicationDefinition(req)
	if a.InstallPath != "/opt/rcd/apps/weather-reader/bin/weather-reader" || a.ServiceUnit != "rcd-app-weather-reader.service" || a.WorkDir != "/var/lib/rcd/apps/weather-reader" || a.ServiceUser != "device_owner" || a.VersionMode != "manifest_sha256" || len(a.DefinitionSHA256) != 64 {
		t.Fatal(a)
	}
	req.Arguments[0] = "--changed"
	if a.Arguments[0] != "--listen=127.0.0.1:9001" {
		t.Fatal("definition must own immutable arguments")
	}
	if applicationDefinition(req).DefinitionSHA256 == a.DefinitionSHA256 {
		t.Fatal("changed runtime definition reused the identity")
	}
}

func TestRCDApplicationDefinitionRejectsReservedAndExecutableInput(t *testing.T) {
	valid := ApplicationRequest{Component: "my-app", Name: "My App", Arguments: []string{}, GracefulShutdownConfirmed: true}
	for _, component := range []string{"lower", "manager", "agent", "communicate", "lower-stack", "rcd-server", "systemd", "semaphore", "../app", "/bin/sh", "UPPER", "9test", "app-", "two--parts", strings.Repeat("a", 33)} {
		req := valid
		req.Component = component
		if req.Validate() == nil {
			t.Fatalf("unsafe component %q", component)
		}
	}
	for _, arg := range []string{"$(reboot)", "`id`", "--x=$HOME", "/tmp/../etc", "%h", "x;y", "a|b", "a\nb", " a", strings.Repeat("a", 257)} {
		req := valid
		req.Arguments = []string{arg}
		if req.Validate() == nil {
			t.Fatalf("unsafe argument %q", arg)
		}
	}
	req := valid
	req.GracefulShutdownConfirmed = false
	if req.Validate() == nil {
		t.Fatal("must acknowledge process-only graceful stopping")
	}
	req = valid
	req.Arguments = nil
	if req.Validate() == nil {
		t.Fatal("explicit arguments required")
	}
	var decoded ApplicationRequest
	if StrictJSON([]byte(`{"component":"app","name":"App","arguments":[],"graceful_shutdown_confirmed":true,"install_path":"/bin/sh"}`), &decoded) == nil {
		t.Fatal("browser path injection")
	}
}

func TestRCDGenericManifestRequiresEmptyStateFormats(t *testing.T) {
	m, _ := fixture(t)
	m.Component = "my-app"
	m.Compatibility.StateFormats = map[string]SchemaRange{}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Compatibility.StateFormats = nil
	if m.Validate() == nil {
		t.Fatal("null state formats")
	}
	zero := 0
	m.Compatibility.StateFormats = map[string]SchemaRange{"manager_db": {Min: &zero, Max: &zero}}
	if m.Validate() == nil {
		t.Fatal("generic software cannot claim RCD database ownership")
	}
}
