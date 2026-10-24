package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/store"
)

type slowFlowWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *slowFlowWriter) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(body)
}

func TestPublicFlowSlowReaderDoesNotHoldSnapshotLock(t *testing.T) {
	h := New(&flowStub{}, Options{})
	w := &slowFlowWriter{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	first := make(chan struct{})
	go func() {
		defer close(first)
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/flow", nil))
	}()
	<-w.entered
	defer func() { close(w.release); <-first }()
	second := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/public/flow", nil))
		second <- response.Code
	}()
	select {
	case status := <-second:
		if status != 200 {
			t.Fatal(status)
		}
	case <-time.After(time.Second):
		t.Fatal("slow reader blocked shared snapshot")
	}
}

type flowStub struct {
	fakeService
	calls   int
	request PageRequest
}

func (f *flowStub) PublicFlow(context.Context) (FlowSnapshot, error) {
	f.calls++
	return FlowSnapshot{GeneratedAt: time.Unix(100, 0), Sources: []FlowSource{{ID: "rcd-46", Samples: "9007199254740993"}}}, nil
}
func (f *flowStub) PublicFlowPoints(_ context.Context, request PageRequest) (FlowPointPage, error) {
	f.request = request
	return FlowPointPage{Items: []FlowPoint{}, Total: 0}, nil
}

func TestPublicFlowExactReadOnlyAuthBoundary(t *testing.T) {
	dir := t.TempDir()
	repo, err := store.Open(context.Background(), filepath.Join(dir, "auth.db"), store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	auth, err := NewWebAuth(context.Background(), repo, dir, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	f := &flowStub{}
	h := New(f, Options{Auth: auth})
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/public/flow", 200}, {"HEAD", "/api/v1/public/flow", 200},
		{"GET", "/api/v1/public/flow/points?source_id=rcd-46", 200},
		{"GET", "/api/v1/public/flow/points", 400},
		{"GET", "/api/v1/public/flow/points?source_id=rcd-46&offset=-1", 400},
		{"POST", "/api/v1/public/flow", 401}, {"PUT", "/api/v1/public/flow/points", 401},
		{"DELETE", "/api/v1/public/flow", 401}, {"GET", "/api/v1/public/flow/configuration", 401},
		{"GET", "/api/v1/sources", 401}, {"GET", "/api/v1/sessions", 401},
		{"GET", "/api/v1/events", 401}, {"GET", "/api/v1/commands", 401},
		{"GET", "/api/v1/calculations/config/draft", 401},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, tc.path, nil)
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s=%d body=%s", tc.method, tc.path, w.Code, w.Body)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unexpected CORS permission")
		}
		if tc.method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD returned payload")
		}
	}
	if f.request.Limit != 20 {
		t.Fatalf("unbounded public point page: %+v", f.request)
	}
}

func TestPublicFlowSharedSnapshotAndCancellation(t *testing.T) {
	f := &flowStub{}
	now := time.Unix(100, 0)
	h := New(f, Options{Now: func() time.Time { return now }})
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/public/flow", nil))
			if w.Code != 200 {
				t.Error(w.Code)
			}
		}()
	}
	wg.Wait()
	if f.calls != 1 {
		t.Fatalf("viewers rebuilt %d snapshots", f.calls)
	}
	now = now.Add(time.Second)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/public/flow", nil))
	if f.calls != 2 || !strings.Contains(w.Body.String(), `"samples":"9007199254740993"`) {
		t.Fatal("snapshot not refreshed losslessly")
	}
}
