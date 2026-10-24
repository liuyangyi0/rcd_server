package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestCanceledHTTPServiceCallDoesNotEmitFalseErrorLog(t *testing.T) {
	var logs strings.Builder
	handler := &Handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}

	handler.respond(httptest.NewRecorder(), nil,
		fmt.Errorf("read latest lower log receive time: %w", context.Canceled), http.StatusOK)
	if strings.Contains(logs.String(), "HTTP service error") {
		t.Fatalf("client cancellation was logged as a service error: %s", logs.String())
	}

	handler.respond(httptest.NewRecorder(), nil, fmt.Errorf("database failed"), http.StatusOK)
	if !strings.Contains(logs.String(), "HTTP service error") {
		t.Fatalf("real internal failure was not logged: %s", logs.String())
	}
}

type fakeService struct {
	groups          []GroupView
	selection       domain.GroupRoute
	selectionErr    error
	selectionCalled bool
	selectionArgs   struct {
		session, group, source string
		revision               uint64
	}
	pageRequest PageRequest
	requestInfo RequestInfo
	broker      *Broker
}

func (f *fakeService) Overview(context.Context) (Overview, error) {
	return Overview{ManagerState: "Running"}, nil
}
func (f *fakeService) ListGroups(context.Context) ([]GroupView, error) { return f.groups, nil }
func (f *fakeService) GetGroup(_ context.Context, id string) (GroupView, error) {
	for _, group := range f.groups {
		if group.ID == id {
			return group, nil
		}
	}
	return GroupView{}, domain.ErrNotFound
}
func (f *fakeService) CreateGroup(ctx context.Context, mutation GroupMutation) (GroupView, error) {
	f.requestInfo = RequestInfoFromContext(ctx)
	return GroupView{Group: domain.Group{ID: mutation.ID, Name: mutation.Name, Revision: 1}}, nil
}
func (f *fakeService) UpdateGroup(_ context.Context, id string, revision uint64, mutation GroupMutation) (GroupView, error) {
	return GroupView{Group: domain.Group{ID: id, Name: mutation.Name, Revision: revision + 1}}, nil
}
func (f *fakeService) DeleteGroup(context.Context, string, uint64) error { return nil }
func (f *fakeService) SetDefaultSource(_ context.Context, id, source string, revision uint64) (GroupView, error) {
	return GroupView{Group: domain.Group{ID: id, DefaultSourceID: source, Revision: revision + 1}}, nil
}
func (f *fakeService) ListSources(context.Context) ([]SourceView, error) { return nil, nil }
func (f *fakeService) GetSource(context.Context, string) (SourceView, error) {
	return SourceView{}, domain.ErrNotFound
}
func (f *fakeService) CreateSource(context.Context, SourceMutation) (SourceView, error) {
	return SourceView{}, nil
}
func (f *fakeService) UpdateSource(context.Context, string, uint64, SourceMutation) (SourceView, error) {
	return SourceView{}, nil
}
func (f *fakeService) DeleteSource(context.Context, string, uint64) error { return nil }
func (f *fakeService) ProbeSource(context.Context, string) (SourceProbeResult, error) {
	return SourceProbeResult{OK: true}, nil
}
func (f *fakeService) RestartSource(context.Context, string, uint64, bool) (SourceView, error) {
	return SourceView{}, nil
}
func (f *fakeService) ListSessions(context.Context) ([]SessionView, error) { return nil, nil }
func (f *fakeService) SetSessionSelection(_ context.Context, session, group, source string, revision uint64) (domain.GroupRoute, error) {
	f.selectionCalled = true
	f.selectionArgs.session, f.selectionArgs.group, f.selectionArgs.source, f.selectionArgs.revision = session, group, source, revision
	if f.selectionErr != nil {
		return domain.GroupRoute{}, f.selectionErr
	}
	if f.selection.Revision == 0 {
		f.selection = domain.GroupRoute{GroupID: group, SourceID: source, Revision: revision + 1}
	}
	return f.selection, nil
}
func (f *fakeService) ListPoints(_ context.Context, request PageRequest) (PointPage, error) {
	f.pageRequest = request
	return PointPage{Items: []PointView{}, Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *fakeService) ListCommands(_ context.Context, request PageRequest) (CommandPage, error) {
	f.pageRequest = request
	return CommandPage{Items: []domain.PendingCommand{}, Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *fakeService) ListAlarms(_ context.Context, request PageRequest) (AlarmPage, error) {
	f.pageRequest = request
	return AlarmPage{Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *fakeService) AcknowledgeAlarm(context.Context, int64, uint64) (domain.Alarm, error) {
	return domain.Alarm{}, nil
}
func (f *fakeService) SilenceAlarm(context.Context, int64, uint64, time.Time) (domain.Alarm, error) {
	return domain.Alarm{}, nil
}
func (f *fakeService) ListSourceEvents(_ context.Context, request PageRequest) (SourceEventPage, error) {
	f.pageRequest = request
	return SourceEventPage{Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *fakeService) ListManagementAudit(_ context.Context, request PageRequest) (AuditPage, error) {
	f.pageRequest = request
	return AuditPage{Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *fakeService) SystemInfo(context.Context) (SystemInfo, error) { return SystemInfo{}, nil }
func (f *fakeService) ExportConfiguration(context.Context) (ConfigurationDocument, error) {
	return ConfigurationDocument{SchemaVersion: 1}, nil
}
func (f *fakeService) ImportConfiguration(context.Context, ConfigurationImportRequest) (ConfigurationImportResult, error) {
	return ConfigurationImportResult{}, nil
}
func (f *fakeService) DiagnosticBundle(context.Context) ([]byte, error)  { return []byte("zip"), nil }
func (f *fakeService) PrometheusMetrics(context.Context) (string, error) { return "metric 1\n", nil }
func (f *fakeService) Subscribe(ctx context.Context) (Subscription, error) {
	if f.broker == nil {
		f.broker = NewBroker()
	}
	return f.broker.Subscribe(ctx)
}
func (f *fakeService) Health(context.Context) HealthStatus {
	return HealthStatus{OK: true, Status: "ok"}
}
func (f *fakeService) Ready(context.Context) HealthStatus {
	return HealthStatus{OK: true, Status: "ready"}
}

func TestSessionSelectionUsesIfMatchAndPath(t *testing.T) {
	service := &fakeService{}
	handler := New(service, Options{})

	withoutRevision := httptest.NewRequest(http.MethodPut, "/api/v1/sessions/client/groups/g/selection", strings.NewReader(`{"source_id":"b"}`))
	withoutRevision.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, withoutRevision)
	if response.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodPut, "/api/v1/sessions/client/groups/g/selection", strings.NewReader(`{"source_id":"b"}`))
	request.Host = "manager.local:8080"
	request.Header.Set("Origin", "http://manager.local:8080")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"7"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !service.selectionCalled || service.selectionArgs.session != "client" || service.selectionArgs.group != "g" || service.selectionArgs.source != "b" || service.selectionArgs.revision != 7 {
		t.Fatalf("selection args = %#v", service.selectionArgs)
	}
	if got := response.Header().Get("ETag"); got != `"8"` {
		t.Fatalf("ETag = %q", got)
	}
}

func TestCrossOriginWriteIsRejectedBeforeService(t *testing.T) {
	service := &fakeService{}
	handler := New(service, Options{})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/sessions/client/groups/g/selection", strings.NewReader(`{"source_id":"b"}`))
	request.Host = "manager.local:8080"
	request.Header.Set("Origin", "http://attacker.invalid")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"1"`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || service.selectionCalled {
		t.Fatalf("status=%d called=%v body=%s", response.Code, service.selectionCalled, response.Body.String())
	}
}

func TestOperatorAndRequestOriginAreAvailableToManagementAudit(t *testing.T) {
	service := &fakeService{}
	handler := New(service, Options{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/groups", strings.NewReader(`{"id":"line","name":"Line"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-RCD-Operator", "Alice")
	request.Header.Set("User-Agent", "RCD-Test/1.0")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.requestInfo.Actor != "Alice@192.0.2.1" || service.requestInfo.RemoteAddress != "192.0.2.1" || service.requestInfo.UserAgent != "RCD-Test/1.0" {
		t.Fatalf("request info=%#v", service.requestInfo)
	}
}

func TestSameSiteButCrossOriginBrowserWriteIsRejected(t *testing.T) {
	service := &fakeService{}
	handler := New(service, Options{})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/sessions/client/groups/g/selection", strings.NewReader(`{"source_id":"b"}`))
	request.Header.Set("Sec-Fetch-Site", "same-site")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"1"`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || service.selectionCalled {
		t.Fatalf("status=%d called=%v body=%s", response.Code, service.selectionCalled, response.Body.String())
	}
}

func TestRevisionConflictReturnsCurrentRevision(t *testing.T) {
	service := &fakeService{selectionErr: &domain.RevisionConflictError{Expected: 2, Actual: 5}}
	handler := New(service, Options{})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/sessions/s/groups/g/selection", strings.NewReader(`{"source_id":"b"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"2"`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Details map[string]uint64 `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "revision_conflict" || body.Error.Details["actual"] != 5 {
		t.Fatalf("body = %#v", body)
	}
}

func TestListPointsParsesAllFilters(t *testing.T) {
	service := &fakeService{}
	handler := New(service, Options{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/points?group_id=g&source_id=s&route_source_id=route&session_id=session&status=Bad&kind=connection&q=KKS&limit=25&offset=50", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	want := PageRequest{GroupID: "g", SourceID: "s", RouteSourceID: "route", SessionID: "session", Status: "Bad", Kind: "connection", Query: "KKS", Limit: 25, Offset: 50}
	if service.pageRequest != want {
		t.Fatalf("request=%#v want=%#v", service.pageRequest, want)
	}
}

func TestOperationalDownloadAndMetricsRoutes(t *testing.T) {
	service := &fakeService{}
	handler := New(service, Options{})
	tests := []struct {
		path        string
		contentType string
		disposition string
		body        string
	}{
		{path: "/metrics", contentType: "text/plain", body: "metric 1"},
		{path: "/api/v1/diagnostics", contentType: "application/zip", disposition: "rcd-manager-diagnostics.zip", body: "zip"},
		{path: "/api/v1/configuration", contentType: "application/json", disposition: "rcd-manager-configuration.json", body: `"schema_version":1`},
		{path: "/api/v1/points/export?group_id=g&route_source_id=s", contentType: "text/csv", disposition: "rcd-manager-points.csv", body: "NodeId"},
		{path: "/api/v1/commands/export?status=Waiting", contentType: "text/csv", disposition: "rcd-manager-commands.csv", body: "StartedAt"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), test.contentType) {
				t.Fatalf("status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
			}
			if test.disposition != "" && !strings.Contains(response.Header().Get("Content-Disposition"), test.disposition) {
				t.Fatalf("content-disposition=%q", response.Header().Get("Content-Disposition"))
			}
			if !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("body=%q does not contain %q", response.Body.String(), test.body)
			}
		})
	}
}

func TestBrokerClosesSubscriptionAndRejectsAfterClose(t *testing.T) {
	broker := NewBroker()
	ctx, cancel := context.WithCancel(context.Background())
	subscription, err := broker.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	broker.Publish(Event{Type: "groups.changed", Time: time.Unix(1, 0)})
	select {
	case event := <-subscription.Events:
		if event.Type != "groups.changed" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not delivered")
	}
	cancel()
	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("subscription remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription was not closed")
	}
	broker.Close()
	if _, err := broker.Subscribe(context.Background()); err == nil {
		t.Fatal("subscribe after close unexpectedly succeeded")
	}
}

func TestEventsStreamsNamedSSEEvent(t *testing.T) {
	service := &fakeService{broker: NewBroker()}
	server := httptest.NewServer(New(service, Options{}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	service.broker.Publish(Event{Type: "selection.changed", SessionID: "client", GroupID: "g", Revision: 4})
	scanner := bufio.NewScanner(response.Body)
	var eventLine, dataLine string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventLine = line
		}
		if strings.HasPrefix(line, "data:") {
			dataLine = line
			break
		}
	}
	if eventLine != "event: selection.changed" || !strings.Contains(dataLine, `"session_id":"client"`) || !strings.Contains(dataLine, `"revision":4`) {
		t.Fatalf("event=%q data=%q scanErr=%v", eventLine, dataLine, scanner.Err())
	}
}
