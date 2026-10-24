package processagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
)

// Handler exposes only this supervisor's fixed instance and fixed resources.
// Authentication and network isolation remain the operator's existing intranet
// policy; the instance check is target validation, not authentication.
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/lifecycle/actions", func(w http.ResponseWriter, r *http.Request) {
		req, err := readAction(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		if !req.Lifecycle || r.Header.Get("If-Match") != strconv.Quote(req.ExpectedRevision) {
			writeError(w, maintenanceError("conflict", "安全操作必须携带 lifecycle 及精确 If-Match"))
			return
		}
		value, err := s.SubmitLifecycle(req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOperation(w, value)
	})
	mux.HandleFunc("GET /api/v1/lifecycle/operations/{request_id}", func(w http.ResponseWriter, r *http.Request) {
		value, ok := s.Operation(r.PathValue("request_id"))
		if !ok {
			writeError(w, maintenanceError("not_found", "操作尚未找到；不能据此认定未执行"))
			return
		}
		writeJSON(w, http.StatusOK, value)
	})
	mux.HandleFunc("POST /api/v1/lifecycle/operations/{request_id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		req, err := readAction(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		if !req.Lifecycle || req.RequestID != r.PathValue("request_id") || r.Header.Get("If-Match") != strconv.Quote(req.ExpectedRevision) {
			writeError(w, maintenanceError("invalid_request", "操作目标或 If-Match 不一致"))
			return
		}
		value, err := s.Resolve(req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOperation(w, value)
	})
	mux.HandleFunc("GET /api/v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		value := s.Maintenance()
		w.Header().Set("ETag", strconv.Quote(value.Revision))
		writeJSON(w, http.StatusOK, value)
	})
	mux.HandleFunc("POST /api/v1/maintenance/actions", func(w http.ResponseWriter, r *http.Request) {
		var req MaintenanceRequest
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		defer r.Body.Close()
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, maintenanceError("invalid_request", "需要有界维护 JSON 请求"))
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			writeError(w, maintenanceError("invalid_request", "只允许一个 JSON 对象"))
			return
		}
		if header := r.Header.Get("If-Match"); header == "" || (header != req.ExpectedRevision && header != strconv.Quote(req.ExpectedRevision)) {
			writeError(w, maintenanceError("conflict", "If-Match 必须与请求版本一致"))
			return
		}
		value, err := s.MaintenanceAction(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("ETag", strconv.Quote(value.Revision))
		writeJSON(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, buildinfo.ForComponent("agent"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": s.cfg.InstanceID})
	})
	mux.HandleFunc("GET /api/v1/process", func(w http.ResponseWriter, r *http.Request) {
		snapshot := s.Snapshot()
		w.Header().Set("ETag", strconv.Quote(snapshot.Revision))
		writeJSON(w, http.StatusOK, snapshot)
	})
	mux.HandleFunc("POST /api/v1/process/actions", func(w http.ResponseWriter, r *http.Request) {
		req, err := readAction(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		if header := r.Header.Get("If-Match"); header != "" && header != req.ExpectedRevision && header != strconv.Quote(req.ExpectedRevision) {
			writeError(w, &domain.OperationError{Code: "conflict", Message: "请求体与 If-Match 版本不一致"})
			return
		}
		if req.DeploymentID != "" && r.Header.Get("If-Match") == "" {
			writeError(w, maintenanceError("conflict", "维护进程操作必须携带 If-Match"))
			return
		}
		operation, err := s.Submit(req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOperation(w, operation)
	})
	mux.HandleFunc("GET /api/v1/process/operations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": s.History()})
	})
	mux.HandleFunc("GET /api/v1/process/operations/{request_id}", func(w http.ResponseWriter, r *http.Request) {
		operation, ok := s.Operation(r.PathValue("request_id"))
		if !ok {
			writeError(w, &domain.OperationError{Code: "not_found", Message: "尚未找到作业；不能据此认定操作未发生"})
			return
		}
		// A GET is a successful read even while the retrieved operation runs.
		writeJSON(w, http.StatusOK, operation)
	})
	mux.HandleFunc("POST /api/v1/process/operations/{request_id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		req, err := readAction(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		if req.RequestID != r.PathValue("request_id") {
			writeError(w, &domain.OperationError{Code: "invalid_request", Message: "作业路径与请求编号不一致"})
			return
		}
		operation, err := s.Resolve(req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeOperation(w, operation)
	})
	mux.HandleFunc("GET /api/v1/process/log", func(w http.ResponseWriter, r *http.Request) {
		limit := domain.MaxProcessLogBytes
		if value := r.URL.Query().Get("limit"); value != "" {
			var err error
			limit, err = strconv.Atoi(value)
			if err != nil {
				writeError(w, &domain.OperationError{Code: "invalid_request", Message: "日志上限必须为整数"})
				return
			}
		}
		for key := range r.URL.Query() {
			if key != "limit" {
				writeError(w, &domain.OperationError{Code: "invalid_request", Message: "只能查看固定的最近进程输出，不能指定路径"})
				return
			}
		}
		value, err := s.Log(limit)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.cfg.RemoteManagers != "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(host)
			allowed := err == nil && ip != nil && ip.IsLoopback()
			for _, value := range strings.Split(s.cfg.RemoteManagers, ",") {
				if ip != nil && ip.Equal(net.ParseIP(strings.TrimSpace(value))) {
					allowed = true
				}
			}
			if !allowed {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "manager address is not allowed"})
				return
			}
			if ip != nil && !ip.IsLoopback() && r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.URL.Path, "/api/v1/lifecycle/") {
				writeError(w, maintenanceError("unsupported", "远程写入只允许安全生命周期接口；发布接口仅供本机使用"))
				return
			}
		}
		if r.URL.Path != "/healthz" && r.Header.Get(domain.ProcessInstanceHeader) != s.cfg.InstanceID {
			writeError(w, &domain.OperationError{Code: "not_found", Message: "守护实例标识不匹配；未执行操作"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": domain.OperationError{Code: "invalid_request", Message: "跨域写请求已拒绝"}})
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func readAction(w http.ResponseWriter, r *http.Request) (domain.ProcessActionRequest, error) {
	var value domain.ProcessActionRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return value, &domain.OperationError{Code: "invalid_request", Message: "需要有界 JSON 操作请求，不能包含额外执行参数"}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return value, &domain.OperationError{Code: "invalid_request", Message: "请求只能包含一个 JSON 对象"}
	}
	return value, value.Validate()
}

func writeOperation(w http.ResponseWriter, value domain.ProcessOperation) {
	code := http.StatusAccepted
	if value.Terminal() {
		code = http.StatusOK
	}
	writeJSON(w, code, value)
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	value := &domain.OperationError{Code: "io_error", Message: boundedError(err)}
	var operationError *domain.OperationError
	var fieldError *domain.FieldError
	if errors.As(err, &operationError) {
		value = operationError
	} else if errors.As(err, &fieldError) {
		value = &domain.OperationError{Code: "invalid_request", Message: fieldError.Error()}
	}
	code := http.StatusInternalServerError
	switch value.Code {
	case "conflict", "busy":
		code = http.StatusConflict
	case "not_found":
		code = http.StatusNotFound
	case "invalid_request":
		code = http.StatusBadRequest
	case "too_large":
		code = http.StatusRequestEntityTooLarge
	case "unsupported":
		code = http.StatusNotImplemented
	case "operation_failed", "persistence_failed":
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"error": value})
}

func Run(ctx context.Context, cfg Config) error {
	if err := platformSupported(); err != nil {
		return err
	}
	var err error
	cfg, err = cfg.normalized()
	if err != nil {
		return err
	}
	// Own the control listener before starting a lower. A port collision must
	// not leave an inaccessible child running in the background.
	listener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("listen for process control: %w", err)
	}
	defer listener.Close()
	s, err := New(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	s.logger.Info("process agent listening", "instance_id", cfg.InstanceID, "listen", listener.Addr().String(), "restart_policy", cfg.RestartPolicy)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
