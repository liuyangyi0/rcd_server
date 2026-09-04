package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

type Options struct {
	Logger *slog.Logger
	Now    func() time.Time
}

type Handler struct {
	service Service
	logger  *slog.Logger
	now     func() time.Time
	mux     *http.ServeMux
}

func New(service Service, options Options) http.Handler {
	if service == nil {
		panic("api: nil Service")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	h := &Handler{service: service, logger: logger, now: now, mux: http.NewServeMux()}
	h.routes()
	return h.middleware(h.mux)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /api/v1/overview", h.getOverview)
	h.mux.HandleFunc("GET /api/v1/groups", h.listGroups)
	h.mux.HandleFunc("POST /api/v1/groups", h.createGroup)
	h.mux.HandleFunc("GET /api/v1/groups/{id}", h.getGroup)
	h.mux.HandleFunc("PUT /api/v1/groups/{id}", h.updateGroup)
	h.mux.HandleFunc("DELETE /api/v1/groups/{id}", h.deleteGroup)
	h.mux.HandleFunc("PUT /api/v1/groups/{id}/default-source", h.setDefaultSource)

	h.mux.HandleFunc("GET /api/v1/sources", h.listSources)
	h.mux.HandleFunc("POST /api/v1/sources", h.createSource)
	h.mux.HandleFunc("GET /api/v1/sources/{id}", h.getSource)
	h.mux.HandleFunc("GET /api/v1/sources/{id}/topology", h.getSourceTopology)
	h.mux.HandleFunc("PUT /api/v1/sources/{id}", h.updateSource)
	h.mux.HandleFunc("DELETE /api/v1/sources/{id}", h.deleteSource)
	h.mux.HandleFunc("POST /api/v1/sources/{id}/probe", h.probeSource)
	h.mux.HandleFunc("POST /api/v1/sources/{id}/reconnect", h.reconnectSource)
	h.mux.HandleFunc("POST /api/v1/sources/{id}/rediscover", h.rediscoverSource)
	h.lowerOperationRoutes()
	h.deviceVersionRoutes()
	h.processControlRoutes()
	h.lowerLogRoutes()
	h.calculationRoutes()

	h.mux.HandleFunc("GET /api/v1/sessions", h.listSessions)
	h.mux.HandleFunc("PUT /api/v1/sessions/{session}/groups/{group}/selection", h.setSessionSelection)
	h.mux.HandleFunc("GET /api/v1/points", h.listPoints)
	h.mux.HandleFunc("GET /api/v1/points/export", h.exportPoints)
	h.mux.HandleFunc("GET /api/v1/commands", h.listCommands)
	h.mux.HandleFunc("GET /api/v1/commands/export", h.exportCommands)
	h.mux.HandleFunc("GET /api/v1/alarms", h.listAlarms)
	h.mux.HandleFunc("PUT /api/v1/alarms/{id}/acknowledgement", h.acknowledgeAlarm)
	h.mux.HandleFunc("PUT /api/v1/alarms/{id}/silence", h.silenceAlarm)
	h.mux.HandleFunc("GET /api/v1/source-events", h.listSourceEvents)
	h.mux.HandleFunc("GET /api/v1/management-audit", h.listManagementAudit)
	h.mux.HandleFunc("GET /api/v1/system", h.systemInfo)
	h.mux.HandleFunc("GET /api/v1/configuration", h.exportConfiguration)
	h.mux.HandleFunc("POST /api/v1/configuration/import", h.importConfiguration)
	h.mux.HandleFunc("GET /api/v1/diagnostics", h.diagnosticBundle)
	h.mux.HandleFunc("GET /api/v1/events", h.events)
	h.mux.HandleFunc("GET /metrics", h.metrics)
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("GET /readyz", h.ready)
}

func (h *Handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if isUnsafe(r.Method) {
			if err := checkSameOrigin(r); err != nil {
				writeError(w, http.StatusForbidden, "cross_origin_write", err.Error(), nil)
				return
			}
		}
		remoteAddress := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			remoteAddress = host
		}
		operator := strings.TrimSpace(r.Header.Get("X-RCD-Operator"))
		if len(operator) > 64 {
			operator = operator[:64]
		}
		actor := "web"
		if operator != "" {
			actor = operator
		}
		if remoteAddress != "" {
			actor += "@" + remoteAddress
		}
		r = r.WithContext(withRequestInfo(r.Context(), RequestInfo{Actor: actor, RemoteAddress: remoteAddress, UserAgent: r.UserAgent()}))
		defer func() {
			if recovered := recover(); recovered != nil {
				h.logger.Error("HTTP handler panic", "error", recovered, "method", r.Method, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) getOverview(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Overview(r.Context())
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.ListGroups(r.Context())
	h.respond(w, map[string]any{"items": value}, err, http.StatusOK)
}

func (h *Handler) getGroup(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.GetGroup(r.Context(), r.PathValue("id"))
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var mutation GroupMutation
	if err := decodeJSON(w, r, &mutation); err != nil {
		return
	}
	value, err := h.service.CreateGroup(r.Context(), mutation)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusCreated)
}

func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var mutation GroupMutation
	if err := decodeJSON(w, r, &mutation); err != nil {
		return
	}
	id := r.PathValue("id")
	if mutation.ID != "" && mutation.ID != id {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "body id must match path id", nil)
		return
	}
	mutation.ID = id
	value, err := h.service.UpdateGroup(r.Context(), id, revision, mutation)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	err := h.service.DeleteGroup(r.Context(), r.PathValue("id"), revision)
	if err != nil {
		h.respond(w, nil, err, http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setDefaultSource(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceID string `json:"source_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	value, err := h.service.SetDefaultSource(r.Context(), r.PathValue("id"), body.SourceID, revision)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listSources(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.ListSources(r.Context())
	h.respond(w, map[string]any{"items": value}, err, http.StatusOK)
}

func (h *Handler) getSource(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.GetSource(r.Context(), r.PathValue("id"))
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) createSource(w http.ResponseWriter, r *http.Request) {
	var mutation SourceMutation
	if err := decodeJSON(w, r, &mutation); err != nil {
		return
	}
	value, err := h.service.CreateSource(r.Context(), mutation)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusCreated)
}

func (h *Handler) updateSource(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var mutation SourceMutation
	if err := decodeJSON(w, r, &mutation); err != nil {
		return
	}
	id := r.PathValue("id")
	if mutation.ID != "" && mutation.ID != id {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "body id must match path id", nil)
		return
	}
	mutation.ID = id
	value, err := h.service.UpdateSource(r.Context(), id, revision, mutation)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) deleteSource(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	err := h.service.DeleteSource(r.Context(), r.PathValue("id"), revision)
	if err != nil {
		h.respond(w, nil, err, http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) probeSource(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.ProbeSource(r.Context(), r.PathValue("id"))
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) reconnectSource(w http.ResponseWriter, r *http.Request) {
	h.restartSource(w, r, false)
}

func (h *Handler) rediscoverSource(w http.ResponseWriter, r *http.Request) {
	h.restartSource(w, r, true)
}

func (h *Handler) restartSource(w http.ResponseWriter, r *http.Request, rediscover bool) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	value, err := h.service.RestartSource(r.Context(), r.PathValue("id"), revision, rediscover)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.ListSessions(r.Context())
	h.respond(w, map[string]any{"items": value}, err, http.StatusOK)
}

func (h *Handler) setSessionSelection(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceID string `json:"source_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	value, err := h.service.SetSessionSelection(r.Context(), r.PathValue("session"), r.PathValue("group"), body.SourceID, revision)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listPoints(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	value, err := h.service.ListPoints(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listCommands(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	value, err := h.service.ListCommands(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listAlarms(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	value, err := h.service.ListAlarms(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) acknowledgeAlarm(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Path(w, r.PathValue("id"))
	if !ok {
		return
	}
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var body struct{}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	value, err := h.service.AcknowledgeAlarm(r.Context(), id, revision)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) silenceAlarm(w http.ResponseWriter, r *http.Request) {
	id, ok := parseInt64Path(w, r.PathValue("id"))
	if !ok {
		return
	}
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		SilencedUntil string `json:"silenced_until"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	var until time.Time
	var err error
	if strings.TrimSpace(body.SilencedUntil) != "" {
		until, err = time.Parse(time.RFC3339, body.SilencedUntil)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "silenced_until must be RFC3339 or empty", nil)
			return
		}
	}
	value, err := h.service.SilenceAlarm(r.Context(), id, revision, until)
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listSourceEvents(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	value, err := h.service.ListSourceEvents(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) listManagementAudit(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	value, err := h.service.ListManagementAudit(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) systemInfo(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.SystemInfo(r.Context())
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) exportConfiguration(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.ExportConfiguration(r.Context())
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="rcd-manager-configuration.json"`)
	writeJSON(w, http.StatusOK, value)
}

func (h *Handler) importConfiguration(w http.ResponseWriter, r *http.Request) {
	var request ConfigurationImportRequest
	if err := decodeJSON(w, r, &request); err != nil {
		return
	}
	value, err := h.service.ImportConfiguration(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) diagnosticBundle(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.DiagnosticBundle(r.Context())
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="rcd-manager-diagnostics.zip"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value)
}

func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.PrometheusMetrics(r.Context())
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(value))
}

func (h *Handler) exportPoints(w http.ResponseWriter, r *http.Request) {
	h.exportCSV(w, r, true)
}

func (h *Handler) exportCommands(w http.ResponseWriter, r *http.Request) {
	h.exportCSV(w, r, false)
}

func (h *Handler) exportCSV(w http.ResponseWriter, r *http.Request, points bool) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	request.Limit, request.Offset = 1000, 0
	filename := "rcd-manager-commands.csv"
	if points {
		filename = "rcd-manager-points.csv"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(w)
	if points {
		_ = writer.Write([]string{"NodeId", "BrowseName", "GroupId", "SourceId", "NodeClass", "DataType", "Value", "StatusCode", "SourceTimestamp"})
		for {
			page, pageErr := h.service.ListPoints(r.Context(), request)
			if pageErr != nil {
				return
			}
			for _, point := range page.Items {
				var value, status, sourceTimestamp string
				if point.Sample != nil {
					encoded, _ := json.Marshal(point.Sample.Value)
					value = string(encoded)
					status = fmt.Sprintf("0x%08X", uint32(point.Sample.StatusCode))
					sourceTimestamp = point.Sample.SourceTimestamp.Format(time.RFC3339Nano)
				}
				_ = writer.Write([]string{point.Node.Key.Identifier, point.Node.BrowseName, point.GroupID, point.SelectedSourceID,
					string(point.Node.Class), string(point.Node.DataType), value, status, sourceTimestamp})
			}
			request.Offset += len(page.Items)
			if len(page.Items) == 0 || request.Offset >= page.Total {
				break
			}
		}
	} else {
		_ = writer.Write([]string{"Id", "StartedAt", "FinishedAt", "Kind", "State", "GroupId", "SourceId", "Port", "SessionId", "StatusCode", "Error"})
		for {
			page, pageErr := h.service.ListCommands(r.Context(), request)
			if pageErr != nil {
				return
			}
			for _, command := range page.Items {
				finishedAt := ""
				if !command.FinishedAt.IsZero() {
					finishedAt = command.FinishedAt.Format(time.RFC3339Nano)
				}
				_ = writer.Write([]string{command.ID, command.StartedAt.Format(time.RFC3339Nano), finishedAt,
					command.Kind, string(command.State), command.GroupID, command.SourceID, command.Port, command.SessionID,
					fmt.Sprintf("0x%08X", uint32(command.StatusCode)), command.Error})
			}
			request.Offset += len(page.Items)
			if len(page.Items) == 0 || request.Offset >= page.Total {
				break
			}
		}
	}
	writer.Flush()
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	status := h.service.Health(r.Context())
	code := http.StatusOK
	if !status.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	status := h.service.Ready(r.Context())
	code := http.StatusOK
	if !status.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream_unsupported", "streaming is not supported", nil)
		return
	}
	subscription, err := h.service.Subscribe(r.Context())
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	if subscription.Close != nil {
		defer subscription.Close()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			flusher.Flush()
		case event, open := <-subscription.Events:
			if !open {
				return
			}
			if event.Time.IsZero() {
				event.Time = h.now().UTC()
			}
			payload, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				h.logger.Error("encode SSE event", "error", marshalErr, "type", event.Type)
				continue
			}
			if event.ID != "" {
				_, _ = fmt.Fprintf(w, "id: %s\n", sanitizeSSEField(event.ID))
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sanitizeSSEField(event.Type), payload)
			flusher.Flush()
		}
	}
}

func (h *Handler) respond(w http.ResponseWriter, value any, err error, success int) {
	if err == nil {
		writeJSON(w, success, value)
		return
	}
	status, code, details := mapError(err)
	// Browser navigation, query invalidation and SSE teardown routinely cancel
	// in-flight requests. The repository correctly returns the wrapped context
	// error, but recording that client lifecycle as a server fault hides real
	// database/OPC failures in the operations log.
	if status >= 500 && !errors.Is(err, context.Canceled) {
		h.logger.Error("HTTP service error", "error", err)
	}
	writeError(w, status, code, publicMessage(err, status), details)
}

func mapError(err error) (int, string, any) {
	var unavailable *calculationServiceError
	if errors.As(err, &unavailable) {
		return http.StatusNotImplemented, "calculation_unavailable", nil
	}
	if errors.Is(err, calculation.ErrConfigTooLarge) {
		return http.StatusRequestEntityTooLarge, "calculation_too_large", nil
	}
	var validation *calculation.ValidationError
	if errors.As(err, &validation) {
		return http.StatusUnprocessableEntity, "calculation_invalid", validation.Problems
	}
	var operation *domain.OperationError
	if errors.As(err, &operation) {
		status := http.StatusBadGateway
		switch operation.Code {
		case "invalid_request":
			status = http.StatusUnprocessableEntity
		case "not_found":
			status = http.StatusNotFound
		case "conflict", "busy":
			status = http.StatusConflict
		case "unsupported":
			status = http.StatusNotImplemented
		case "too_large":
			status = http.StatusRequestEntityTooLarge
		case "unavailable":
			status = http.StatusServiceUnavailable
		case "timeout":
			status = http.StatusGatewayTimeout
		}
		var details any
		if operation.CurrentRevision != "" {
			details = map[string]string{"current_revision": operation.CurrentRevision}
		}
		return status, operation.Code, details
	}
	var revision *domain.RevisionConflictError
	switch {
	case errors.As(err, &revision):
		return http.StatusConflict, "revision_conflict", map[string]uint64{"expected": revision.Expected, "actual": revision.Actual}
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrSessionClosed):
		return http.StatusNotFound, "not_found", nil
	case errors.Is(err, domain.ErrAlreadyExists):
		return http.StatusConflict, "already_exists", nil
	case errors.Is(err, domain.ErrGroupPending), errors.Is(err, domain.ErrCommandPending):
		return http.StatusConflict, "pending_command", nil
	case errors.Is(err, domain.ErrGroupInUse), errors.Is(err, domain.ErrSourceInUse):
		return http.StatusConflict, "resource_in_use", nil
	case errors.Is(err, domain.ErrSourceNotSelectable):
		return http.StatusConflict, "source_not_selectable", nil
	case errors.Is(err, domain.ErrSourceNotInGroup), errors.Is(err, domain.ErrInvalid):
		return http.StatusUnprocessableEntity, "invalid_request", nil
	case errors.Is(err, domain.ErrShuttingDown):
		return http.StatusServiceUnavailable, "shutting_down", nil
	default:
		return http.StatusInternalServerError, "internal_error", nil
	}
}

func publicMessage(err error, status int) string {
	var operation *domain.OperationError
	if errors.As(err, &operation) {
		return operation.Message
	}
	if status >= 500 {
		return "internal server error"
	}
	return err.Error()
}

func requireRevision(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "If-Match revision is required", nil)
		return 0, false
	}
	raw = strings.TrimPrefix(raw, "W/")
	raw = strings.Trim(raw, `"`)
	revision, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || revision == 0 {
		writeError(w, http.StatusBadRequest, "invalid_revision", "If-Match must contain a positive numeric revision", nil)
		return 0, false
	}
	return revision, true
}

func setETag(w http.ResponseWriter, revision uint64) {
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, revision))
}

func parsePage(r *http.Request) (PageRequest, error) {
	request := PageRequest{
		PortID: r.URL.Query().Get("port_id"), DeviceID: r.URL.Query().Get("device_id"),
		Limit: 100, GroupID: r.URL.Query().Get("group_id"), SourceID: r.URL.Query().Get("source_id"),
		RouteSourceID: r.URL.Query().Get("route_source_id"), SessionID: r.URL.Query().Get("session_id"),
		Status: r.URL.Query().Get("status"), Kind: r.URL.Query().Get("kind"), Query: r.URL.Query().Get("q"),
	}
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		request.Limit, err = strconv.Atoi(raw)
		if err != nil || request.Limit < 1 || request.Limit > 1000 {
			return PageRequest{}, errors.New("limit must be between 1 and 1000")
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		request.Offset, err = strconv.Atoi(raw)
		if err != nil || request.Offset < 0 {
			return PageRequest{}, errors.New("offset must be non-negative")
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		request.From, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return PageRequest{}, errors.New("from must be an RFC3339 timestamp")
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		request.To, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return PageRequest{}, errors.New("to must be an RFC3339 timestamp")
		}
	}
	if !request.From.IsZero() && !request.To.IsZero() && request.From.After(request.To) {
		return PageRequest{}, errors.New("from must not be after to")
	}
	return request, nil
}

func parseInt64Path(w http.ResponseWriter, value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "path id must be a positive integer", nil)
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	contentType := r.Header.Get("Content-Type")
	if mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0]); mediaType != "application/json" {
		err := errors.New("Content-Type must be application/json")
		writeError(w, http.StatusUnsupportedMediaType, "json_required", err.Error(), nil)
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain one JSON value")
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "details": details}})
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func checkSameOrigin(r *http.Request) error {
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
		return errors.New("cross-site browser writes are not allowed")
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid Origin header")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !strings.EqualFold(parsed.Scheme, scheme) || !strings.EqualFold(parsed.Host, r.Host) {
		return errors.New("write Origin does not match request origin")
	}
	return nil
}

func sanitizeSSEField(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	return strings.ReplaceAll(value, "\n", "")
}
