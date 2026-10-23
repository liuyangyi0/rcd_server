package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"rcd_manager/internal/domain"
)

type ProcessActionInput = domain.ProcessActionInput
type ProcessStatusView = domain.ProcessStatusView
type ProcessLogView = domain.ProcessLogView
type ProcessOperationPage = domain.ProcessOperationPage

// Independent process control remains available while lower OPC is offline.
// Kept optional to preserve the base Service implementations and test doubles.
type ProcessControlService interface {
	SourceProcessBinding(context.Context, string) (domain.ProcessBinding, error)
	SaveSourceProcessBinding(context.Context, domain.ProcessBinding, uint64) (domain.ProcessBinding, error)
	SourceProcessStatus(context.Context, string) (ProcessStatusView, error)
	ExecuteSourceProcess(context.Context, string, string, ProcessActionInput) (domain.ProcessOperation, error)
	ResolveSourceProcess(context.Context, string, string, ProcessActionInput) (domain.ProcessOperation, error)
	SourceProcessOperation(context.Context, string, string) (domain.ProcessOperation, error)
	ListSourceProcessOperations(context.Context, string, int, int) (ProcessOperationPage, error)
	SourceProcessLog(context.Context, string, int) (ProcessLogView, error)
}

func (h *Handler) processControlRoutes() {
	const base = "/api/v1/sources/{id}/process"
	h.mux.HandleFunc("GET "+base+"/binding", h.getProcessBinding)
	h.mux.HandleFunc("PUT "+base+"/binding", h.putProcessBinding)
	h.mux.HandleFunc("GET "+base+"/status", h.getProcessStatus)
	h.mux.HandleFunc("GET "+base+"/log", h.getProcessLog)
	h.mux.HandleFunc("POST "+base+"/actions", h.processAction(false))
	h.mux.HandleFunc("POST "+base+"/operations/{request}/resolve", h.processAction(true))
	h.mux.HandleFunc("GET "+base+"/operations", h.listProcessOperations)
	h.mux.HandleFunc("GET "+base+"/operations/{request}", h.getProcessOperation)
}

func (h *Handler) processService(w http.ResponseWriter) (ProcessControlService, bool) {
	w.Header().Set("Cache-Control", "no-store")
	service, ok := h.service.(ProcessControlService)
	if !ok {
		h.respond(w, nil, &domain.OperationError{Code: "unsupported", Message: "未启用独立进程管理"}, http.StatusOK)
	}
	return service, ok
}

func (h *Handler) getProcessBinding(w http.ResponseWriter, r *http.Request) {
	service, ok := h.processService(w)
	if !ok {
		return
	}
	value, err := service.SourceProcessBinding(r.Context(), r.PathValue("id"))
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) putProcessBinding(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireProcessBindingRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceRevision uint64 `json:"source_revision"`
		AgentURL       string `json:"agent_url"`
		InstanceID     string `json:"instance_id"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if body.SourceRevision == 0 {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "source_revision is required", nil)
		return
	}
	service, ok := h.processService(w)
	if !ok {
		return
	}
	value, err := service.SaveSourceProcessBinding(r.Context(), domain.ProcessBinding{SourceID: r.PathValue("id"), SourceRevision: body.SourceRevision,
		AgentURL: body.AgentURL, InstanceID: body.InstanceID}, revision)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) getProcessStatus(w http.ResponseWriter, r *http.Request) {
	service, ok := h.processService(w)
	if !ok {
		return
	}
	value, err := service.SourceProcessStatus(r.Context(), r.PathValue("id"))
	if err == nil {
		w.Header().Set("ETag", strconv.Quote(value.Revision))
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) processAction(resolve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		revision, ok := requireOperationRevision(w, r)
		if !ok {
			return
		}
		var body ProcessActionInput
		if decodeJSON(w, r, &body) != nil {
			return
		}
		if body.SourceRevision == 0 || body.BindingRevision == 0 {
			writeError(w, http.StatusPreconditionRequired, "revision_required", "source_revision and binding_revision are required", nil)
			return
		}
		if resolve && body.RequestID != r.PathValue("request") {
			writeError(w, http.StatusBadRequest, "invalid_request", "request_id must match the original request and path", nil)
			return
		}
		service, ok := h.processService(w)
		if !ok {
			return
		}
		var value domain.ProcessOperation
		var err error
		if resolve {
			value, err = service.ResolveSourceProcess(r.Context(), r.PathValue("id"), revision, body)
		} else {
			value, err = service.ExecuteSourceProcess(r.Context(), r.PathValue("id"), revision, body)
		}
		status := http.StatusAccepted
		if value.Terminal() {
			status = http.StatusOK
		}
		h.respond(w, value, err, status)
	}
}

func (h *Handler) getProcessOperation(w http.ResponseWriter, r *http.Request) {
	service, ok := h.processService(w)
	if !ok {
		return
	}
	value, err := service.SourceProcessOperation(r.Context(), r.PathValue("id"), r.PathValue("request"))
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listProcessOperations(w http.ResponseWriter, r *http.Request) {
	limit, offset := 50, 0
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100", nil)
			return
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "offset must be non-negative", nil)
			return
		}
	}
	service, ok := h.processService(w)
	if !ok {
		return
	}
	value, err := service.ListSourceProcessOperations(r.Context(), r.PathValue("id"), limit, offset)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) getProcessLog(w http.ResponseWriter, r *http.Request) {
	limit := domain.MaxProcessLogBytes
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 4 || parsed > domain.MaxProcessLogBytes {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 4 and 65536 bytes", nil)
			return
		}
		limit = parsed
	}
	service, ok := h.processService(w)
	if !ok {
		return
	}
	value, err := service.SourceProcessLog(r.Context(), r.PathValue("id"), limit)
	h.respond(w, value, err, http.StatusOK)
}

func requireProcessBindingRevision(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "If-Match binding revision is required", nil)
		return 0, false
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	revision, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || raw != strconv.FormatUint(revision, 10) {
		writeError(w, http.StatusBadRequest, "invalid_revision", "If-Match must contain one exact numeric binding revision (0 for unconfigured)", nil)
		return 0, false
	}
	return revision, true
}
