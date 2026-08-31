package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"rcd_manager/internal/domain"
)

// LowerOperationsService is optional. Operations address a physical source,
// never a Session route, and pin its configuration revision on every write.
type LowerOperationsService interface {
	SourceOperationsCapabilities(context.Context, string) (domain.OperationsCapabilities, error)
	ExecuteSourceOperation(context.Context, string, uint64, domain.OperationRequest) (json.RawMessage, error)
}

func (h *Handler) lowerOperationRoutes() {
	const base = "/api/v1/sources/{id}/operations"
	h.mux.HandleFunc("GET "+base+"/capabilities", h.getSourceOperationsCapabilities)
	h.mux.HandleFunc("GET "+base+"/status", h.readSourceOperation("status"))
	h.mux.HandleFunc("GET "+base+"/configs", h.readSourceOperation("list_configs"))
	h.mux.HandleFunc("GET "+base+"/config", h.readSourceOperation("read_config"))
	h.mux.HandleFunc("GET "+base+"/logs", h.readSourceOperation("list_logs"))
	h.mux.HandleFunc("GET "+base+"/log", h.readSourceOperation("read_log"))
	h.mux.HandleFunc("PUT "+base+"/port", h.setSourceAcquisition("set_port_enabled"))
	h.mux.HandleFunc("PUT "+base+"/device-read", h.setSourceAcquisition("set_device_read_enabled"))
	h.mux.HandleFunc("POST "+base+"/config/validate", h.validateSourceConfig)
	h.mux.HandleFunc("PUT "+base+"/config", h.writeSourceConfig)
}

func (h *Handler) getSourceOperationsCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	service, ok := h.service.(LowerOperationsService)
	if !ok {
		h.respond(w, nil, unsupportedOperations(), http.StatusOK)
		return
	}
	result, err := service.SourceOperationsCapabilities(r.Context(), r.PathValue("id"))
	h.respond(w, result, err, http.StatusOK)
}

func (h *Handler) readSourceOperation(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		request := domain.OperationRequest{Action: action, Name: query.Get("name"), BackupID: query.Get("backup_id")}
		if action == "read_log" {
			request.Generation, request.Query, request.Level = query.Get("generation"), query.Get("q"), strings.ToUpper(query.Get("level"))
			if value, exists := query["offset"]; exists {
				offset, err := strconv.ParseInt(value[0], 10, 64)
				if err != nil || offset < 0 {
					writeError(w, http.StatusBadRequest, "invalid_request", "offset must be a non-negative integer", nil)
					return
				}
				request.Offset = &offset
			}
			if value, exists := query["limit"]; exists {
				limit, err := strconv.ParseInt(value[0], 10, 64)
				if err != nil || limit < 4 || limit > domain.MaxOperationLogBytes {
					writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 4 and 65536 bytes", nil)
					return
				}
				request.Limit = limit
			}
			if value, exists := query["tail"]; exists {
				if value[0] != "true" && value[0] != "false" {
					writeError(w, http.StatusBadRequest, "invalid_request", "tail must be true or false", nil)
					return
				}
				request.Tail = value[0] == "true"
			}
		}
		h.executeSourceOperation(w, r, 0, request)
	}
}

func (h *Handler) setSourceAcquisition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		revision, ok := requireOperationRevision(w, r)
		if !ok {
			return
		}
		var body struct {
			SourceRevision uint64 `json:"source_revision"`
			PortID         string `json:"port_id"`
			DeviceID       string `json:"device_id,omitempty"`
			Enabled        *bool  `json:"enabled"`
		}
		if decodeJSON(w, r, &body) != nil {
			return
		}
		if action == "set_port_enabled" && body.DeviceID != "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "a port operation must not contain device_id", nil)
			return
		}
		h.executeSourceOperation(w, r, body.SourceRevision, domain.OperationRequest{
			Action: action, PortID: body.PortID, DeviceID: body.DeviceID, Enabled: body.Enabled, ExpectedRevision: revision,
		})
	}
}

func (h *Handler) validateSourceConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string  `json:"name"`
		Content *string `json:"content"`
	}
	if decodeOperationConfig(w, r, &body) != nil {
		return
	}
	h.executeSourceOperation(w, r, 0, domain.OperationRequest{Action: "validate_config", Name: body.Name, Content: body.Content})
}

func (h *Handler) writeSourceConfig(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireOperationRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceRevision uint64  `json:"source_revision"`
		Name           string  `json:"name"`
		Content        *string `json:"content"`
	}
	if decodeOperationConfig(w, r, &body) != nil {
		return
	}
	h.executeSourceOperation(w, r, body.SourceRevision, domain.OperationRequest{
		Action: "write_config", Name: body.Name, Content: body.Content, ExpectedRevision: revision,
	})
}

func (h *Handler) executeSourceOperation(w http.ResponseWriter, r *http.Request, sourceRevision uint64, request domain.OperationRequest) {
	w.Header().Set("Cache-Control", "no-store")
	if request.Mutates() && sourceRevision == 0 {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "source_revision is required", nil)
		return
	}
	if err := request.Validate(); err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	service, ok := h.service.(LowerOperationsService)
	if !ok {
		h.respond(w, nil, unsupportedOperations(), http.StatusOK)
		return
	}
	result, err := service.ExecuteSourceOperation(r.Context(), r.PathValue("id"), sourceRevision, request)
	if err == nil {
		var version struct {
			Revision string `json:"revision"`
		}
		if json.Unmarshal(result, &version) == nil && domain.ValidOperationRevision(version.Revision) {
			w.Header().Set("ETag", strconv.Quote(version.Revision))
		}
	}
	h.respond(w, result, err, http.StatusOK)
}

func requireOperationRevision(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "If-Match lower revision is required", nil)
		return "", false
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	if !domain.ValidOperationRevision(raw) {
		writeError(w, http.StatusBadRequest, "invalid_revision", "If-Match must contain one exact lower revision", nil)
		return "", false
	}
	return raw, true
}

func decodeOperationConfig(w http.ResponseWriter, r *http.Request, target any) error {
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required", "Content-Type must be application/json", nil)
		return errors.New("JSON content type required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, domain.MaxOperationJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(target)
	if err == nil {
		if extraErr := decoder.Decode(&struct{}{}); !errors.Is(extraErr, io.EOF) {
			err = extraErr
			if err == nil {
				err = errors.New("request body must contain one JSON value")
			}
		}
	}
	if err != nil {
		var limitErr *http.MaxBytesError
		if errors.As(err, &limitErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "configuration request exceeds 13 MiB", nil)
		} else {
			// Do not echo invalid configuration content in a decoding error.
			writeError(w, http.StatusBadRequest, "invalid_json", "configuration request must be one valid JSON object", nil)
		}
	}
	return err
}

func unsupportedOperations() error {
	return &domain.OperationError{Code: "unsupported", Message: "该下级尚未提供运维能力"}
}
