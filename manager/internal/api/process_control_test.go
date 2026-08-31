package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

type processServiceStub struct {
	fakeService
	method, source, revision, requestID string
	binding                             domain.ProcessBinding
	input                               ProcessActionInput
	numericRevision                     uint64
	limit, offset, calls                int
	state                               string
	err                                 error
}

func (s *processServiceStub) SourceProcessBinding(_ context.Context, id string) (domain.ProcessBinding, error) {
	s.source = id
	s.calls++
	return domain.ProcessBinding{SourceID: id, SourceRevision: 4}, s.err
}
func (s *processServiceStub) SaveSourceProcessBinding(_ context.Context, b domain.ProcessBinding, revision uint64) (domain.ProcessBinding, error) {
	s.binding = b
	s.numericRevision = revision
	s.calls++
	b.Revision = revision + 1
	return b, s.err
}
func (s *processServiceStub) SourceProcessStatus(_ context.Context, id string) (ProcessStatusView, error) {
	s.calls++
	return ProcessStatusView{ProcessSnapshot: domain.ProcessSnapshot{InstanceID: "instance-a", Revision: "agent:8", State: "running", DesiredState: "running", PID: 22}, SourceID: id, SourceRevision: 4, BindingRevision: 2}, s.err
}
func (s *processServiceStub) process(ctx context.Context, method, id, revision string, input ProcessActionInput) (domain.ProcessOperation, error) {
	s.method, s.source, s.revision, s.input = method, id, revision, input
	s.calls++
	s.requestInfo = RequestInfoFromContext(ctx)
	state := s.state
	if state == "" {
		state = "accepted"
	}
	now := time.Now().UTC()
	return domain.ProcessOperation{SourceID: id, SourceRevision: input.SourceRevision, BindingRevision: input.BindingRevision, RequestID: input.RequestID, InstanceID: "instance-a", Action: input.Action, State: state, CreatedAt: now, UpdatedAt: now}, s.err
}
func (s *processServiceStub) ExecuteSourceProcess(ctx context.Context, id, revision string, input ProcessActionInput) (domain.ProcessOperation, error) {
	return s.process(ctx, "action", id, revision, input)
}
func (s *processServiceStub) ResolveSourceProcess(ctx context.Context, id, revision string, input ProcessActionInput) (domain.ProcessOperation, error) {
	return s.process(ctx, "resolve", id, revision, input)
}
func (s *processServiceStub) SourceProcessOperation(_ context.Context, id, requestID string) (domain.ProcessOperation, error) {
	s.calls++
	s.source, s.requestID = id, requestID
	return domain.ProcessOperation{SourceID: id, RequestID: requestID, State: "unknown"}, s.err
}
func (s *processServiceStub) ListSourceProcessOperations(_ context.Context, id string, limit, offset int) (ProcessOperationPage, error) {
	s.calls++
	s.limit, s.offset = limit, offset
	return ProcessOperationPage{SourceID: id, SourceRevision: 4, Items: []domain.ProcessOperation{}, Limit: limit, Offset: offset}, s.err
}
func (s *processServiceStub) SourceProcessLog(_ context.Context, id string, limit int) (ProcessLogView, error) {
	s.calls++
	s.limit = limit
	return ProcessLogView{ProcessLog: domain.ProcessLog{InstanceID: "instance-a", Text: "log", SizeBytes: 3}, SourceID: id, SourceRevision: 4, BindingRevision: 2}, s.err
}

func processHTTP(service Service, method, path, body, revision string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-RCD-Operator", "maintenance")
	if revision != "" {
		r.Header.Set("If-Match", revision)
	}
	w := httptest.NewRecorder()
	New(service, Options{}).ServeHTTP(w, r)
	return w
}

func TestProcessHTTPBindingInitialZeroAndExactCAS(t *testing.T) {
	f := &processServiceStub{}
	w := processHTTP(f, "GET", "/api/v1/sources/a/process/binding", "", "")
	if w.Code != 200 || w.Header().Get("ETag") != `"0"` || !strings.Contains(w.Body.String(), `"source_revision":4`) {
		t.Fatalf("binding %d %s", w.Code, w.Body.String())
	}
	f = &processServiceStub{}
	w = processHTTP(f, "PUT", "/api/v1/sources/a/process/binding", `{"source_revision":4,"agent_url":"http://agent:8086","instance_id":"instance-a"}`, `"0"`)
	if w.Code != 200 || f.calls != 1 || f.numericRevision != 0 || f.binding.SourceID != "a" || f.binding.SourceRevision != 4 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("save %d %s %+v", w.Code, w.Body.String(), f)
	}
	for _, revision := range []string{"", `W/"0"`, `"0","1"`, `*`, `"-1"`, `"00"`} {
		f = &processServiceStub{}
		w = processHTTP(f, "PUT", "/api/v1/sources/a/process/binding", `{"source_revision":4,"agent_url":"","instance_id":""}`, revision)
		if w.Code < 400 || f.calls != 0 {
			t.Fatalf("accepted binding token %s %d", revision, w.Code)
		}
	}
}

func TestProcessHTTPActionsRequireSourceBindingAndAgentRevisions(t *testing.T) {
	const body = `{"action":"restart","request_id":"r-1","source_revision":4,"binding_revision":2,"expected_config_revisions":{"config.toml":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`
	f := &processServiceStub{}
	w := processHTTP(f, "POST", "/api/v1/sources/a/process/actions", body, `"agent:8"`)
	if w.Code != 202 || f.source != "a" || f.revision != "agent:8" || f.input.BindingRevision != 2 || f.input.SourceRevision != 4 || f.input.ExpectedConfigRevisions["config.toml"] == "" || !strings.HasPrefix(f.requestInfo.Actor, "maintenance") {
		t.Fatalf("action %d %s %+v", w.Code, w.Body.String(), f)
	}
	f = &processServiceStub{state: "failed"}
	w = processHTTP(f, "POST", "/api/v1/sources/a/process/operations/r-1/resolve", body, `"agent:8"`)
	if w.Code != 200 || f.method != "resolve" || f.input.RequestID != "r-1" {
		t.Fatalf("resolve %d %s %+v", w.Code, w.Body.String(), f)
	}
	for _, test := range []struct {
		path, body, revision string
		status               int
	}{
		{"/actions", body, "", 428},
		{"/actions", `{"action":"restart","request_id":"r-1","source_revision":4}`, `"agent:8"`, 428},
		{"/actions", `{"action":"restart","request_id":"r-1","binding_revision":2}`, `"agent:8"`, 428},
		{"/actions", body, `W/"agent:8"`, 400},
		{"/actions", `{"action":"restart","request_id":"r-1","source_revision":4,"binding_revision":2,"executable":"/tmp/other"}`, `"agent:8"`, 400},
		{"/operations/other/resolve", body, `"agent:8"`, 400},
	} {
		f = &processServiceStub{}
		w = processHTTP(f, "POST", "/api/v1/sources/a/process"+test.path, test.body, test.revision)
		if w.Code != test.status || f.calls != 0 {
			t.Fatalf("accepted %s => %d %s", test.path, w.Code, w.Body.String())
		}
	}
}

func TestProcessHTTPStatusHistoryAndLogStaySeparateFromOPCHealth(t *testing.T) {
	f := &processServiceStub{}
	w := processHTTP(f, "GET", "/api/v1/sources/a/process/status", "", "")
	if w.Code != 200 || w.Header().Get("ETag") != `"agent:8"` || !strings.Contains(w.Body.String(), `"binding_revision":2`) {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	w = processHTTP(f, "GET", "/api/v1/sources/a/process/operations?limit=25&offset=5", "", "")
	if w.Code != 200 || f.limit != 25 || f.offset != 5 || !strings.Contains(w.Body.String(), `"source_id":"a"`) {
		t.Fatalf("history %d %s", w.Code, w.Body.String())
	}
	w = processHTTP(f, "GET", "/api/v1/sources/a/process/log?limit=4", "", "")
	if w.Code != 200 || f.limit != 4 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("log %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/log?limit=0", "/log?limit=65537", "/operations?limit=101", "/operations?offset=-1"} {
		f = &processServiceStub{}
		w = processHTTP(f, "GET", "/api/v1/sources/a/process"+path, "", "")
		if w.Code != 400 || f.calls != 0 {
			t.Fatalf("bad bounds %s => %d", path, w.Code)
		}
	}
	for code, status := range map[string]int{"not_found": 404, "unavailable": 503, "timeout": 504, "unsupported": 501, "conflict": 409, "invalid_response": 502} {
		f = &processServiceStub{err: &domain.OperationError{Code: code, Message: "未确认进程状态"}}
		w = processHTTP(f, "GET", "/api/v1/sources/a/process/status", "", "")
		if w.Code != status || strings.Contains(w.Body.String(), `"state":"running"`) {
			t.Fatalf("error was shown as running %s %d %s", code, w.Code, w.Body.String())
		}
	}
}

func TestProcessHTTPLegacyServiceReportsUnsupported(t *testing.T) {
	w := processHTTP(&fakeService{}, http.MethodGet, "/api/v1/sources/a/process/status", "", "")
	if w.Code != 501 {
		t.Fatalf("legacy %d %s", w.Code, w.Body.String())
	}
}
