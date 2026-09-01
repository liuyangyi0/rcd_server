package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rcd_manager/internal/domain"
)

type operationServiceStub struct {
	fakeService
	request  domain.OperationRequest
	source   string
	revision uint64
	calls    int
	err      error
}

func (f *operationServiceStub) SourceOperationsCapabilities(context.Context, string) (domain.OperationsCapabilities, error) {
	return domain.OperationsCapabilities{Supported: false, Reason: "old lower", Features: []string{}}, f.err
}
func (f *operationServiceStub) ExecuteSourceOperation(ctx context.Context, id string, revision uint64, request domain.OperationRequest) (json.RawMessage, error) {
	f.source, f.revision, f.request = id, revision, request
	f.requestInfo = RequestInfoFromContext(ctx)
	f.calls++
	return json.RawMessage(`{"revision":"sha256:abc","ports":[]}`), f.err
}

func operationHTTP(t *testing.T, service *operationServiceStub, method, path, body, revision string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-RCD-Operator", "test-operator")
	if revision != "" {
		r.Header.Set("If-Match", revision)
	}
	w := httptest.NewRecorder()
	New(service, Options{}).ServeHTTP(w, r)
	return w
}

func TestLowerOperationsHTTPPinsPhysicalTargetAndBothRevisions(t *testing.T) {
	f := &operationServiceStub{}
	w := operationHTTP(t, f, http.MethodPut, "/api/v1/sources/lower-a/operations/device-read", `{"source_revision":5,"port_id":"/dev/ttyUSB0","device_id":"17","enabled":false}`, `"42"`)
	if w.Code != 200 || f.calls != 1 || f.source != "lower-a" || f.revision != 5 || f.request.ExpectedRevision != "42" || f.request.Action != "set_device_read_enabled" || f.request.Enabled == nil || *f.request.Enabled || f.request.PortID != "/dev/ttyUSB0" || f.request.DeviceID != "17" {
		t.Fatalf("status=%d body=%s target=%+v", w.Code, w.Body.String(), f)
	}
	if w.Header().Get("ETag") != `"sha256:abc"` || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(f.requestInfo.Actor, "test-operator") {
		t.Fatalf("headers=%v actor=%s", w.Header(), f.requestInfo.Actor)
	}
}

func TestLowerOperationsHTTPRejectsMissingAndStaleTokensWithoutDispatch(t *testing.T) {
	for _, tc := range []struct {
		body, revision string
		status         int
	}{
		{`{"source_revision":1,"port_id":"COM1","enabled":true}`, "", 428},
		{`{"port_id":"COM1","enabled":true}`, `"1"`, 428},
		{`{"source_revision":1,"port_id":"COM1","enabled":true}`, `W/"1"`, 400},
		{`{"source_revision":1,"port_id":"COM1","enabled":true}`, `"1","2"`, 400},
		{`{"source_revision":1,"port_id":"COM1","enabled":true}`, `*`, 400},
		{`{"source_revision":1,"port_id":"COM1"}`, `"1"`, 422},
		{`{"source_revision":1,"port_id":"COM1","device_id":"2","enabled":true}`, `"1"`, 400},
	} {
		f := &operationServiceStub{}
		w := operationHTTP(t, f, http.MethodPut, "/api/v1/sources/a/operations/port", tc.body, tc.revision)
		if w.Code != tc.status || f.calls != 0 {
			t.Fatalf("body=%s token=%s status=%d response=%s calls=%d", tc.body, tc.revision, w.Code, w.Body.String(), f.calls)
		}
	}
}

func TestLowerOperationsHTTPConfigSupportsTwoMiBAndRejectsExtras(t *testing.T) {
	content := strings.Repeat("x", domain.MaxOperationConfigBytes)
	body, _ := json.Marshal(map[string]any{"name": "config.toml", "content": content, "source_revision": 2})
	f := &operationServiceStub{}
	w := operationHTTP(t, f, http.MethodPut, "/api/v1/sources/a/operations/config", string(body), `"sha256:abc"`)
	if w.Code != 200 || f.calls != 1 || f.request.Content == nil || len(*f.request.Content) != len(content) {
		t.Fatalf("max config status=%d", w.Code)
	}
	for _, body := range []string{`{"name":"../config.toml","content":"x"}`, `{"name":"config.toml","content":"x","unknown":true}`, `{"name":"config.toml","content":"x"} {}`, `null`} {
		f = &operationServiceStub{}
		w = operationHTTP(t, f, http.MethodPost, "/api/v1/sources/a/operations/config/validate", body, "")
		if w.Code < 400 || f.calls != 0 {
			t.Fatalf("accepted %s status=%d", body, w.Code)
		}
	}
	body, _ = json.Marshal(map[string]any{"name": "config.toml", "content": content + "x"})
	f = &operationServiceStub{}
	w = operationHTTP(t, f, http.MethodPost, "/api/v1/sources/a/operations/config/validate", string(body), "")
	if w.Code != 413 || f.calls != 0 {
		t.Fatalf("oversized config=%d", w.Code)
	}
}

func TestLowerOperationsHTTPLogBoundsAndPagination(t *testing.T) {
	f := &operationServiceStub{}
	w := operationHTTP(t, f, http.MethodGet, "/api/v1/sources/a/operations/log?name=rcd.log&offset=17&generation=abc&limit=65536&level=warn&q=port&tail=false", "", "")
	if w.Code != 200 || f.request.Offset == nil || *f.request.Offset != 17 || f.request.Level != "WARN" || f.request.Limit != 65536 || f.request.Query != "port" {
		t.Fatalf("request=%+v status=%d", f.request, w.Code)
	}
	for _, query := range []string{"offset=-1", "offset=bogus", "limit=0", "limit=65537", "tail=yes", "level=UNKNOWN", "generation=%0D%0A"} {
		f = &operationServiceStub{}
		w = operationHTTP(t, f, http.MethodGet, "/api/v1/sources/a/operations/log?name=rcd.log&"+query, "", "")
		if w.Code < 400 || f.calls != 0 {
			t.Fatalf("accepted %s status=%d", query, w.Code)
		}
	}
}

func TestLowerOperationsHTTPLoggingLevelPinsConfigRevision(t *testing.T) {
	f := &operationServiceStub{}
	w := operationHTTP(t, f, http.MethodPut, "/api/v1/sources/lower-a/operations/logging", `{"source_revision":5,"level":"DEBUG"}`, `"sha256:config"`)
	if w.Code != http.StatusOK || f.calls != 1 || f.source != "lower-a" || f.revision != 5 || f.request.Action != "set_logging_level" || f.request.LogLevel != "debug" || f.request.ExpectedRevision != "sha256:config" {
		t.Fatalf("status=%d body=%s request=%+v", w.Code, w.Body.String(), f.request)
	}

	for _, tc := range []struct {
		body, revision string
		status         int
	}{
		{`{"source_revision":5,"level":"verbose"}`, `"sha256:config"`, http.StatusUnprocessableEntity},
		{`{"source_revision":5,"level":"info"}`, "", http.StatusPreconditionRequired},
		{`{"level":"info"}`, `"sha256:config"`, http.StatusPreconditionRequired},
	} {
		f = &operationServiceStub{}
		w = operationHTTP(t, f, http.MethodPut, "/api/v1/sources/lower-a/operations/logging", tc.body, tc.revision)
		if w.Code != tc.status || f.calls != 0 {
			t.Fatalf("body=%s status=%d response=%s calls=%d", tc.body, w.Code, w.Body.String(), f.calls)
		}
	}
}

func TestLowerOperationsHTTPBusinessErrorsRemainUseful(t *testing.T) {
	for code, status := range map[string]int{"conflict": 409, "busy": 409, "not_found": 404, "unavailable": 503, "timeout": 504, "unsupported": 501, "invalid_response": 502, "io_error": 502, "invalid_request": 422} {
		f := &operationServiceStub{err: &domain.OperationError{Code: code, Message: "refresh state", CurrentRevision: "9"}}
		w := operationHTTP(t, f, http.MethodGet, "/api/v1/sources/a/operations/status", "", "")
		if w.Code != status || !strings.Contains(w.Body.String(), `"current_revision":"9"`) || !strings.Contains(w.Body.String(), "refresh state") {
			t.Fatalf("%s status=%d body=%s", code, w.Code, w.Body.String())
		}
	}
}

func TestLowerOperationsHTTPLegacyCapabilitiesAreNotAnError(t *testing.T) {
	w := operationHTTP(t, &operationServiceStub{}, http.MethodGet, "/api/v1/sources/a/operations/capabilities", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"supported":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
