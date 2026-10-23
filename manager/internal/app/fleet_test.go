package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"rcd_manager/internal/domain"
	"strings"
	"testing"
)

func TestFleetReadOnlyFetchValidatesBearerIdentityAndDoesNotFollowRedirect(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if e := os.WriteFile(token, []byte("private-token"), 0600); e != nil {
		t.Fatal(e)
	}
	m := domain.FleetMachine{ID: "d", Revision: 2, Name: "46", Address: "192.168.10.46", OPCEndpoint: "opc.tcp://192.168.10.46:4841", AgentURL: "http://192.168.10.46:8090", WebURL: "http://192.168.10.46:8087", AgentInstanceID: "agent", GatewayInstanceID: "gateway", ManagerInstanceID: "manager"}
	mode := "valid"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/project/1/rcd/fleet" || r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("incorrect read-only request")
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, "http://127.0.0.1:1/credential-leak", 302)
			return
		case "invalid":
			m.AgentURL = "http://192.168.10.58:8090"
		}
		_ = json.NewEncoder(w).Encode(domain.FleetSnapshot{SchemaVersion: 1, Machines: []domain.FleetMachine{m}})
	}))
	defer s.Close()
	c := fleetConfig{SchemaVersion: 1, PlatformURL: s.URL, ProjectID: 1, TokenFile: token, SourceDevices: map[string]string{"s": "d"}}
	if v, e := fetchFleet(context.Background(), c); e != nil || len(v.Machines) != 1 {
		t.Fatal(v, e)
	}
	mode = "redirect"
	if _, e := fetchFleet(context.Background(), c); e == nil || strings.Contains(e.Error(), "private-token") {
		t.Fatal(e)
	}
	mode = "invalid"
	if _, e := fetchFleet(context.Background(), c); e == nil {
		t.Fatal("different physical machine address accepted")
	}
	path := filepath.Join(t.TempDir(), "fleet.json")
	raw, _ := json.Marshal(c)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readFleetConfig(path); e != nil {
		t.Fatal(e)
	}
	c.SourceDevices["another"] = "d"
	raw, _ = json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	if _, e := readFleetConfig(path); e == nil {
		t.Fatal("duplicate source mapping accepted")
	}
}
