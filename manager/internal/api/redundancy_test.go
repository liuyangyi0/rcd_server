package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"rcd_manager/internal/domain"
	"strings"
	"testing"
)

type redundancyStub struct {
	fakeService
	policy domain.RedundancyPolicy
	mode   bool
	err    error
}

func (f *redundancyStub) Redundancy(context.Context, string) (domain.RedundancyView, error) {
	return domain.RedundancyView{Policy: f.policy}, f.err
}
func (f *redundancyStub) SaveRedundancy(_ context.Context, _ string, _ uint64, p domain.RedundancyPolicy) (domain.RedundancyPolicy, error) {
	f.policy = p
	return p, f.err
}
func (f *redundancyStub) SetSessionAutomatic(_ context.Context, _, _ string, a bool, _ uint64) (domain.GroupRoute, error) {
	f.mode = a
	return domain.GroupRoute{Automatic: a}, f.err
}
func TestRedundancyHTTPRevisionAndClosedSession(t *testing.T) {
	f := &redundancyStub{policy: domain.DefaultRedundancyPolicy("g")}
	h := New(f, Options{})
	call := func(method, path, body, revision string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if revision != "" {
			req.Header.Set("If-Match", revision)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	w := call("GET", "/api/v1/groups/g/redundancy", "", "")
	if w.Code != 200 || w.Header().Get("ETag") != "\"1\"" {
		t.Fatal(w.Code, w.Header())
	}
	body, _ := json.Marshal(f.policy)
	w = call("PUT", "/api/v1/groups/g/redundancy", string(body), "")
	if w.Code != 428 {
		t.Fatal(w.Code)
	}
	f.err = &domain.RevisionConflictError{Expected: 1, Actual: 2}
	w = call("PUT", "/api/v1/groups/g/redundancy", string(body), "1")
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	f.err = domain.ErrGroupPending
	w = call("PUT", "/api/v1/sessions/s/groups/g/automatic", `{"automatic":true,"revision":1}`, "1")
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	f.err = domain.ErrSessionClosed
	w = call("PUT", "/api/v1/sessions/s/groups/g/automatic", `{"automatic":true,"revision":1}`, "1")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	f.err = nil
	w = call("PUT", "/api/v1/sessions/s/groups/g/automatic", `{"revision":1}`, "1")
	if w.Code != 422 {
		t.Fatal(w.Code)
	}
	w = call("PUT", "/api/v1/sessions/s/groups/g/automatic", `{"automatic":true,"revision":1}`, "1")
	if w.Code != 200 || !f.mode {
		t.Fatal(w.Code)
	}
}
