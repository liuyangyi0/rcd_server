package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"rcd_manager/internal/store"
	"strings"
	"testing"
	"time"
)

func TestWebAuthLifecycle(t *testing.T) {
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
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(RequestInfoFromContext(r.Context()).Actor))
	})
	h := (&Handler{auth: auth}).middleware(next)
	call := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-RCD-Operator", "forged")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/v1/overview", "/api/v1/events", "/api/v1/configuration", "/api/v1/points/export", "/metrics"} {
		if w := call("GET", path, "", nil); w.Code != 401 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w := call("POST", "/api/v1/lower-ingest/logs", "{}", nil); w.Code != 200 {
		t.Fatalf("push blocked: %d", w.Code)
	}
	if w := call("GET", "/healthz", "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call("POST", "/api/v1/auth/login", `{"username":"admin","password":"wrong"}`, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	login := func() *http.Cookie {
		w := call("POST", "/api/v1/auth/login", `{"username":"admin","password":"admin"}`, nil)
		if w.Code != 200 {
			t.Fatalf("login: %d %s", w.Code, w.Body)
		}
		return w.Result().Cookies()[0]
	}
	first, second := login(), login()
	if !first.HttpOnly || first.SameSite != http.SameSiteStrictMode || first.MaxAge != 28800 {
		t.Fatal("invalid cookie flags")
	}
	if w := call("GET", "/api/v1/overview", "", first); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "admin@") {
		t.Fatalf("actor: %d %s", w.Code, w.Body)
	}
	s, _ := auth.session(func() *http.Request { r := httptest.NewRequest("GET", "/", nil); r.AddCookie(first); return r }())
	if w := call("POST", "/api/v1/auth/logout", "", first); w.Code != 204 {
		t.Fatal(w.Code)
	}
	select {
	case <-s.context.Done():
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel SSE context")
	}
	if w := call("GET", "/api/v1/overview", "", first); w.Code != 401 {
		t.Fatal("old token valid")
	}
	if w := call("POST", "/api/v1/auth/password", `{"current_password":"admin","new_password":"updated-password"}`, second); w.Code != 200 {
		t.Fatalf("change: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/api/v1/overview", "", second); w.Code != 401 {
		t.Fatal("password change did not revoke sessions")
	}
	restored, err := NewWebAuth(context.Background(), repo, dir, "overwrite", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.user.Username != "admin" || !matchPassword(restored.user.PasswordHash, "updated-password") {
		t.Fatal("credentials overwritten by bootstrap environment")
	}
	if matchPassword(restored.user.PasswordHash, "admin") {
		t.Fatal("old password accepted")
	}
}

func TestWebAuthBootstrapAndRateLimit(t *testing.T) {
	dir := t.TempDir()
	repo, err := store.Open(context.Background(), filepath.Join(dir, "auth.db"), store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	auth, err := NewWebAuth(context.Background(), repo, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if !auth.user.MustChange || matchPassword(auth.user.PasswordHash, "admin") {
		t.Fatal("empty bootstrap did not generate secret")
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	for n := 0; n < 10; n++ {
		if !auth.allowLogin(r) {
			t.Fatal("limited early")
		}
	}
	if auth.allowLogin(r) {
		t.Fatal("missing limit")
	}
	auth.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if !auth.allowLogin(r) {
		t.Fatal("rate limit did not expire")
	}
}
