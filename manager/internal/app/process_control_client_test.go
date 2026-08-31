package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"rcd_manager/internal/domain"
)

func TestProcessAgentHTTPPinsInstanceAndOriginalRequestOnAllMethods(t *testing.T) {
	request := domain.ProcessActionRequest{RequestID: "r-1", InstanceID: "physical-a", Action: "restart", ExpectedRevision: "epoch:3"}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(domain.ProcessInstanceHeader) != request.InstanceID {
			t.Errorf("identity header=%q", r.Header.Get(domain.ProcessInstanceHeader))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/process":
			if r.Method != "GET" {
				t.Error("status method")
			}
			_ = json.NewEncoder(w).Encode(domain.ProcessSnapshot{InstanceID: request.InstanceID, Revision: "3", State: "stopped", DesiredState: "stopped"})
		case "/api/v1/process/actions", "/api/v1/process/operations/r-1/resolve":
			if r.Method != "POST" {
				t.Error("action/resolve method")
			}
			var actual domain.ProcessActionRequest
			if json.NewDecoder(r.Body).Decode(&actual) != nil || actual != request {
				t.Errorf("request changed %+v", actual)
			}
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(processStubOperation(request, "accepted"))
		case "/api/v1/process/operations/r-1":
			if r.Method != "GET" {
				t.Error("operation method")
			}
			_ = json.NewEncoder(w).Encode(processStubOperation(request, "succeeded"))
		case "/api/v1/process/log":
			if r.Method != "GET" || r.URL.RawQuery != "limit=64" {
				t.Errorf("log query %s", r.URL.String())
			}
			_ = json.NewEncoder(w).Encode(domain.ProcessLog{InstanceID: request.InstanceID, Text: "设备日志", SizeBytes: 12})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &processAgentHTTPClient{client: server.Client()}
	binding := domain.ProcessBinding{AgentURL: server.URL, InstanceID: request.InstanceID}
	if _, err := client.Status(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Action(context.Background(), binding, request); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Operation(context.Background(), binding, request); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(context.Background(), binding, request); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Log(context.Background(), binding, 64); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestProcessAgentHTTPRejectsWrongIdentityMalformedAndOversizedResponses(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"wrong_instance", `{"instance_id":"other","revision":"1","state":"stopped","desired_state":"stopped"}`, 200, "invalid_response"},
		{"no_revision", `{"instance_id":"a","state":"stopped","desired_state":"stopped"}`, 200, "invalid_response"},
		{"invalid_running_pid", `{"instance_id":"a","revision":"1","state":"running","desired_state":"running"}`, 200, "invalid_response"},
		{"null", `null`, 200, "invalid_response"},
		{"malformed", `{"instance_id":"a"`, 200, "invalid_response"},
		{"trailing_json", `{} {}`, 200, "invalid_response"},
		{"oversize", strings.Repeat("x", domain.MaxProcessResponseBytes+1), 200, "invalid_response"},
		{"html_error", `<html>unavailable</html>`, 503, "invalid_response"},
		{"conflict", `{"error":{"code":"conflict","message":"refresh state","current_revision":"4"}}`, 409, "conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := &processAgentHTTPClient{client: server.Client()}
			if _, err := client.Status(context.Background(), domain.ProcessBinding{AgentURL: server.URL, InstanceID: "a"}); processErrorCode(err) != test.want {
				t.Fatalf("error %v want %s", err, test.want)
			}
		})
	}
}

func TestProcessAgentHTTPLogRequiresBoundedCompleteShape(t *testing.T) {
	for _, body := range []string{
		`{"instance_id":"a"}`,
		`{"instance_id":"a","text":"abc","truncated":false,"size_bytes":-1}`,
		`{"instance_id":"a","text":"12345","truncated":false,"size_bytes":5}`,
		`{"instance_id":"b","text":"","truncated":false,"size_bytes":0}`,
		`{"instance_id":"a","text":null,"truncated":false,"size_bytes":0}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		client := &processAgentHTTPClient{client: server.Client()}
		_, err := client.Log(context.Background(), domain.ProcessBinding{AgentURL: server.URL, InstanceID: "a"}, 4)
		server.Close()
		if processErrorCode(err) != "invalid_response" {
			t.Fatalf("accepted %s: %v", body, err)
		}
	}
}

type processRoundTripFunc func(*http.Request) (*http.Response, error)

func (f processRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProcessAgentHTTPTimeoutNeverRetriesAction(t *testing.T) {
	var calls atomic.Int32
	client := &processAgentHTTPClient{client: &http.Client{Transport: processRoundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.DeadlineExceeded })}}
	_, err := client.Action(context.Background(), domain.ProcessBinding{AgentURL: "http://agent", InstanceID: "a"}, domain.ProcessActionRequest{InstanceID: "a", RequestID: "once", Action: "restart", ExpectedRevision: "1"})
	if processErrorCode(err) != "timeout" || calls.Load() != 1 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
}

func TestProcessAgentHTTPRejectsRedirectsWithoutLeakingActionToNewTarget(t *testing.T) {
	var other atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := newProcessAgentHTTPClient()
	_, err := client.Action(context.Background(), domain.ProcessBinding{AgentURL: origin.URL, InstanceID: "a"}, domain.ProcessActionRequest{InstanceID: "a", RequestID: "once", Action: "restart", ExpectedRevision: "1"})
	if err == nil || other.Load() != 0 {
		t.Fatalf("redirect followed: %v calls %d", err, other.Load())
	}
	var operationError *domain.OperationError
	if !errors.As(err, &operationError) {
		t.Fatalf("untyped transport failure %v", err)
	}
}
