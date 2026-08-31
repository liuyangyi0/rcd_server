//go:build linux

package processagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rcd_manager/internal/domain"
)

func agentHTTP(t *testing.T, s *Supervisor, method, path, instance string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set(domain.ProcessInstanceHeader, instance)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	return recorder
}

func TestAgentHTTPInstanceScopeAndResourceBoundary(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t, "graceful"))
	if got := agentHTTP(t, s, "GET", "/healthz", "", nil); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, id := range []string{"", "another-lower"} {
		if got := agentHTTP(t, s, "GET", "/api/v1/process", id, nil); got.Code != 404 {
			t.Fatalf("wrong target exposed: %d %s", got.Code, got.Body.String())
		}
	}
	response := agentHTTP(t, s, "GET", "/api/v1/process", s.cfg.InstanceID, nil)
	var snapshot domain.ProcessSnapshot
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Validate(s.cfg.InstanceID) != nil {
		t.Fatalf("invalid status: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing revision/no-store")
	}
	for _, path := range []string{"/api/v1/process/log?name=/etc/passwd", "/api/v1/process/log?limit=65537", "/api/v1/process/log?limit=-1"} {
		if got := agentHTTP(t, s, "GET", path, s.cfg.InstanceID, nil); got.Code != http.StatusBadRequest {
			t.Fatalf("unbounded resource accepted: %s %d", path, got.Code)
		}
	}
	bad := map[string]any{"request_id": "bad-input", "instance_id": s.cfg.InstanceID, "action": "start", "expected_revision": snapshot.Revision, "executable": "/bin/anything"}
	if got := agentHTTP(t, s, "POST", "/api/v1/process/actions", s.cfg.InstanceID, bad); got.Code != 400 || s.Snapshot().PID != 0 {
		t.Fatal("arbitrary launch input accepted", got.Code)
	}
}

func TestAgentHTTPResolutionNeverDispatchesAction(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t, "graceful"))
	req := domain.ProcessActionRequest{RequestID: "lost-request", InstanceID: s.cfg.InstanceID, Action: "restart", ExpectedRevision: s.Snapshot().Revision}
	response := agentHTTP(t, s, "POST", "/api/v1/process/operations/lost-request/resolve", s.cfg.InstanceID, req)
	var op domain.ProcessOperation
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &op) != nil || op.State != "failed" {
		t.Fatal(response.Code, response.Body.String())
	}
	late := agentHTTP(t, s, "POST", "/api/v1/process/actions", s.cfg.InstanceID, req)
	if late.Code != 200 || s.Snapshot().PID != 0 {
		t.Fatal("late action dispatched after resolution", late.Code)
	}
	if got := agentHTTP(t, s, "GET", "/api/v1/process/operations/lost-request", s.cfg.InstanceID, nil); got.Code != 200 {
		t.Fatal(got.Code)
	}
	if got := agentHTTP(t, s, "GET", "/api/v1/process/operations/missing", s.cfg.InstanceID, nil); got.Code != 404 {
		t.Fatal(got.Code)
	}
}

func TestAgentHTTPAcceptedNotReportedAsCompletedAndOriginChecked(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	s := newTestSupervisor(t, cfg)
	req := domain.ProcessActionRequest{RequestID: "start-http", InstanceID: cfg.InstanceID, Action: "start", ExpectedRevision: s.Snapshot().Revision}
	response := agentHTTP(t, s, "POST", "/api/v1/process/actions", cfg.InstanceID, req)
	var accepted domain.ProcessOperation
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &accepted) != nil || accepted.Terminal() {
		t.Fatalf("accepted disguised as success: %d %s", response.Code, response.Body.String())
	}
	awaitOperation(t, s, req)
	raw, _ := json.Marshal(req)
	request := httptest.NewRequest("POST", "/api/v1/process/actions", bytes.NewReader(raw))
	request.Header.Set(domain.ProcessInstanceHeader, cfg.InstanceID)
	request.Header.Set("Origin", "http://unrelated.invalid")
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, request)
	if out.Code != 403 {
		t.Fatal("cross-origin action accepted", out.Code)
	}
}
