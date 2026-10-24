package api

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rcd_manager/internal/store"
)

const webCookie = "rcd_web_session"
const webSessionTTL = 8 * time.Hour
const passwordRounds = 600000

type webSession struct {
	username   string
	mustChange bool
	deadline   time.Time
	context    context.Context
	cancel     context.CancelFunc
}
type loginLimit struct {
	count int
	until time.Time
}
type WebAuth struct {
	mu            sync.Mutex
	repo          *store.Repository
	user          store.WebAdmin
	sessions      map[[32]byte]*webSession
	attempts      map[string]loginLimit
	slots         chan struct{}
	bootstrapPath string
	now           func() time.Time
}

// Existing credentials always win. Environment credentials are first-install only.
func NewWebAuth(ctx context.Context, repo *store.Repository, dataDir, username, password string) (*WebAuth, error) {
	user, exists, err := repo.WebAdmin(ctx)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "web-initial-password.txt")
	if !exists {
		if username == "" {
			username = "admin"
		}
		if len(username) > 64 || strings.TrimSpace(username) != username || strings.ContainsAny(username, "\r\n\t") {
			return nil, errors.New("invalid web administrator username")
		}
		generated := password == ""
		if generated {
			// Reuse a recovery file if a previous first-start was interrupted before DB commit.
			bytes, readErr := os.ReadFile(path)
			if readErr == nil {
				password = strings.TrimSpace(string(bytes))
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			} else {
				password = randomToken()
				file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if openErr != nil {
					return nil, openErr
				}
				_, err = file.WriteString(password + "\n")
				if err == nil {
					err = file.Sync()
				}
				closeErr := file.Close()
				if err != nil {
					return nil, err
				}
				if closeErr != nil {
					return nil, closeErr
				}
			}
		}
		if len(password) == 0 || len(password) > 128 {
			return nil, errors.New("web password must contain 1..128 bytes")
		}
		hash, hashErr := hashPassword(password)
		if hashErr != nil {
			return nil, hashErr
		}
		user = store.WebAdmin{Username: username, PasswordHash: hash, MustChange: generated}
		if err = repo.CreateWebAdmin(ctx, user); err != nil {
			return nil, err
		}
	}
	return &WebAuth{repo: repo, user: user, sessions: map[[32]byte]*webSession{}, attempts: map[string]loginLimit{}, slots: make(chan struct{}, 2), bootstrapPath: path, now: time.Now}, nil
}

func randomToken() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordRounds, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$600000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func matchPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "600000" || len(password) > 128 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) != 32 {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordRounds, 32)
	return err == nil && subtle.ConstantTimeCompare(key, expected) == 1
}
func (a *WebAuth) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, s := range a.sessions {
		s.cancel()
		delete(a.sessions, key)
	}
}
func (a *WebAuth) session(r *http.Request) (*webSession, [32]byte) {
	cookie, err := r.Cookie(webCookie)
	if err != nil {
		return nil, [32]byte{}
	}
	key := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[key]
	if s != nil && !a.now().Before(s.deadline) {
		s.cancel()
		delete(a.sessions, key)
		return nil, key
	}
	return s, key
}
func (a *WebAuth) allowLogin(r *http.Request) bool {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for key, value := range a.attempts {
		if !now.Before(value.until) {
			delete(a.attempts, key)
		}
	}
	limit := a.attempts[ip]
	if limit.count >= 10 || (limit.count == 0 && len(a.attempts) >= 1024) {
		return false
	}
	if limit.count == 0 {
		limit.until = now.Add(time.Minute)
	}
	limit.count++
	a.attempts[ip] = limit
	return true
}
func (a *WebAuth) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
func sessionJSON(s *webSession) map[string]any {
	if s == nil {
		return map[string]any{"authenticated": false}
	}
	return map[string]any{"authenticated": true, "username": s.username, "password_change_required": s.mustChange, "expires_at": s.deadline}
}

// Called inside the existing same-origin middleware, before any management route.
func (a *WebAuth) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	w.Header().Set("Cache-Control", "no-store")
	path := r.URL.Path
	// Machine log ingestion and liveness probes are intentionally not browser login endpoints.
	if isPublicFlowRead(r) || (r.Method == "POST" && path == "/api/v1/lower-ingest/logs") || ((r.Method == "GET" || r.Method == "HEAD") && (path == "/healthz" || path == "/readyz")) {
		next.ServeHTTP(w, r)
		return
	}
	if r.Method == "POST" && path == "/api/v1/auth/login" {
		a.login(w, r)
		return
	}
	s, key := a.session(r)
	if r.Method == "GET" && path == "/api/v1/auth/session" {
		writeJSON(w, 200, sessionJSON(s))
		return
	}
	if r.Method == "POST" && path == "/api/v1/auth/logout" {
		a.mu.Lock()
		if existing := a.sessions[key]; existing != nil {
			existing.cancel()
			delete(a.sessions, key)
		}
		a.mu.Unlock()
		a.setCookie(w, r, "", -1)
		w.WriteHeader(204)
		return
	}
	if s == nil {
		writeError(w, 401, "login_required", "请先登录管理系统", nil)
		return
	}
	if r.Method == "POST" && path == "/api/v1/auth/password" {
		a.changePassword(w, r, s)
		return
	}
	if s.mustChange {
		writeError(w, 403, "password_change_required", "请先修改初始密码", nil)
		return
	}
	info := RequestInfoFromContext(r.Context())
	info.Actor = s.username + "@" + info.RemoteAddress
	ctx, cancel := context.WithCancel(withRequestInfo(r.Context(), info))
	defer cancel()
	stop := context.AfterFunc(s.context, cancel)
	defer stop()
	next.ServeHTTP(w, r.WithContext(ctx))
}
func (a *WebAuth) login(w http.ResponseWriter, r *http.Request) {
	if !a.allowLogin(r) {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "login_rate_limited", "登录尝试过多，请一分钟后重试", nil)
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		writeError(w, 429, "login_busy", "登录服务忙，请稍后重试", nil)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if decodeJSON(w, r, &body) != nil {
		return
	}
	a.mu.Lock()
	user := a.user
	a.mu.Unlock()
	valid := matchPassword(user.PasswordHash, body.Password)
	if body.Username != user.Username || !valid {
		writeError(w, 401, "invalid_credentials", "账号或密码错误", nil)
		return
	}
	token := randomToken()
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.user.PasswordHash != user.PasswordHash {
		writeError(w, 401, "invalid_credentials", "密码已更新，请重新登录", nil)
		return
	}
	for key, s := range a.sessions {
		if !now.Before(s.deadline) {
			s.cancel()
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 128 {
		writeError(w, 429, "session_limit", "登录会话数量已达上限", nil)
		return
	}
	if cookie, err := r.Cookie(webCookie); err == nil {
		oldKey := sha256.Sum256([]byte(cookie.Value))
		if old := a.sessions[oldKey]; old != nil {
			old.cancel()
			delete(a.sessions, oldKey)
		}
	}
	deadline := now.Add(webSessionTTL)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	s := &webSession{username: user.Username, mustChange: user.MustChange, deadline: deadline, context: ctx, cancel: cancel}
	a.sessions[key] = s
	a.setCookie(w, r, token, int(webSessionTTL.Seconds()))
	writeJSON(w, 200, sessionJSON(s))
}
func (a *WebAuth) changePassword(w http.ResponseWriter, r *http.Request, s *webSession) {
	if !a.allowLogin(r) {
		writeError(w, 429, "login_rate_limited", "操作过于频繁，请一分钟后重试", nil)
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		writeError(w, 429, "login_busy", "服务忙，请稍后重试", nil)
		return
	}
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if len(body.New) < 8 || len(body.New) > 128 {
		writeError(w, 422, "invalid_password", "新密码长度须为 8～128 字节", nil)
		return
	}
	a.mu.Lock()
	user := a.user
	a.mu.Unlock()
	if !matchPassword(user.PasswordHash, body.Current) {
		writeError(w, 401, "invalid_credentials", "原密码错误", nil)
		return
	}
	if body.New == body.Current {
		writeError(w, 422, "invalid_password", "新密码不能与原密码相同", nil)
		return
	}
	hash, err := hashPassword(body.New)
	if err != nil {
		writeError(w, 500, "password_error", "密码更新失败", nil)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.user.PasswordHash != user.PasswordHash {
		writeError(w, 409, "password_changed", "密码已被修改，请重新登录", nil)
		return
	}
	changed, err := a.repo.UpdateWebPassword(r.Context(), user.PasswordHash, hash)
	if err != nil || !changed {
		writeError(w, 500, "password_error", "密码更新失败", nil)
		return
	}
	a.user.PasswordHash = hash
	a.user.MustChange = false
	for key, session := range a.sessions {
		session.cancel()
		delete(a.sessions, key)
	}
	// The generated bootstrap secret is no longer valid. Removing it is best-effort.
	_ = os.Remove(a.bootstrapPath)
	a.setCookie(w, r, "", -1)
	writeJSON(w, 200, map[string]string{"message": "密码已修改，请重新登录"})
}

// Local maintenance recovery; caller must stop the manager first and supply a password.
func ResetWebPassword(ctx context.Context, repo *store.Repository, password string) error {
	if len(password) < 8 || len(password) > 128 {
		return errors.New("new web password must contain 8..128 bytes")
	}
	user, exists, err := repo.WebAdmin(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("web administrator is not initialized")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	ok, err := repo.UpdateWebPassword(ctx, user.PasswordHash, hash)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("administrator changed concurrently")
	}
	return nil
}
