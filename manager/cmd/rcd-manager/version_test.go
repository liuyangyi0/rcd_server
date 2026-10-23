package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"rcd_manager/internal/buildinfo"
)

func TestVersionOutputWithoutRuntimeConfiguration(t *testing.T) {
	for _, role := range []string{"manager", "agent"} {
		var out bytes.Buffer
		if err := writeVersion(&out, role, []string{"--json"}); err != nil {
			t.Fatal(err)
		}
		var info buildinfo.Info
		if err := json.Unmarshal(out.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if info.Component != role || info.SchemaVersion != 1 || info.Target == "" || info.ReleaseID == "" {
			t.Fatalf("invalid identity: %+v", info)
		}
		out.Reset()
		if err := writeVersion(&out, role, nil); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.String(), "RCD ") {
			t.Fatal(out.String())
		}
	}
	for _, args := range [][]string{{"--bad"}, {"--json", "--json"}} {
		var out bytes.Buffer
		if writeVersion(&out, "manager", args) == nil || out.Len() != 0 {
			t.Fatal("invalid option accepted")
		}
	}
}
