package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/domain"
)

const (
	maxLowerLogCompressedBody   = 1 << 20
	maxLowerLogDecompressedBody = 10 << 20
	maxLowerLogEvents           = 500
	maxLowerLogMessageBytes     = 16 << 10
)

type LowerLogEvent = domain.LowerLogEvent
type LowerLogBatch = domain.LowerLogBatch
type LowerLogAck = domain.LowerLogAck
type LowerLogRequest = domain.LowerLogRequest
type LowerLogItem = domain.LowerLogItem
type LowerLogPage = domain.LowerLogPage
type LowerLogSequenceError = domain.LowerLogSequenceError

type LowerLogsService interface {
	IngestLowerLogs(context.Context, LowerLogBatch) (LowerLogAck, error)
	ListLowerLogs(context.Context, LowerLogRequest) (LowerLogPage, error)
}

func (h *Handler) lowerLogRoutes() {
	h.mux.HandleFunc("POST /api/v1/lower-ingest/logs", h.ingestLowerLogs)
	h.mux.HandleFunc("GET /api/v1/lower-logs", h.listLowerLogs)
}

func (h *Handler) ingestLowerLogs(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(LowerLogsService)
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "central lower log ingestion is unavailable", nil)
		return
	}
	var batch LowerLogBatch
	if err := decodeLowerLogBatch(w, r, &batch); err != nil {
		return
	}
	if err := validateLowerLogBatch(batch); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_log_batch", err.Error(), nil)
		return
	}
	value, err := service.IngestLowerLogs(r.Context(), batch)
	if err != nil {
		var sequence *LowerLogSequenceError
		if errors.As(err, &sequence) {
			writeError(w, http.StatusConflict, "log_sequence_gap", sequence.Error(), map[string]uint64{
				"expected_sequence": sequence.Expected, "actual_sequence": sequence.Actual,
			})
			return
		}
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.respond(w, value, nil, http.StatusOK)
}

func (h *Handler) listLowerLogs(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(LowerLogsService)
	if !ok {
		writeError(w, http.StatusNotImplemented, "unsupported", "central lower log storage is unavailable", nil)
		return
	}
	request := LowerLogRequest{
		CommandAttempts: r.URL.Query().Get("command_attempts"),
		SourceID:        strings.TrimSpace(r.URL.Query().Get("source_id")),
		Level:           strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("level"))),
		PortID:          r.URL.Query().Get("port_id"), DeviceID: r.URL.Query().Get("device_id"),
		Query: r.URL.Query().Get("q"), Limit: 100,
	}
	if request.SourceID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "source_id is required", nil)
		return
	}
	if request.CommandAttempts != "" && request.CommandAttempts != "all" && request.CommandAttempts != "retried" && request.CommandAttempts != "three" {
		writeError(w, http.StatusBadRequest, "invalid_request", "command_attempts must be all, retried or three", nil)
		return
	}
	if request.Level != "" && !validLowerLogLevel(request.Level) {
		writeError(w, http.StatusBadRequest, "invalid_request", "level must be TRACE, DEBUG, INFO, WARN or ERROR", nil)
		return
	}
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		request.Limit, err = strconv.Atoi(raw)
		if err != nil || request.Limit < 1 || request.Limit > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 1000", nil)
			return
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		request.Offset, err = strconv.Atoi(raw)
		if err != nil || request.Offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "offset must be non-negative", nil)
			return
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		request.From, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "from must be an RFC3339 timestamp", nil)
			return
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		request.To, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "to must be an RFC3339 timestamp", nil)
			return
		}
	}
	if !request.From.IsZero() && !request.To.IsZero() && request.From.After(request.To) {
		writeError(w, http.StatusBadRequest, "invalid_request", "from must not be after to", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	value, err := service.ListLowerLogs(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func decodeLowerLogBatch(w http.ResponseWriter, r *http.Request, target *LowerLogBatch) error {
	if mediaType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); mediaType != "application/json" {
		err := errors.New("Content-Type must be application/json")
		writeError(w, http.StatusUnsupportedMediaType, "json_required", err.Error(), nil)
		return err
	}
	compressed := http.MaxBytesReader(w, r.Body, maxLowerLogCompressedBody)
	var reader io.Reader = compressed
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		err := errors.New("Content-Encoding must be identity or gzip")
		writeError(w, http.StatusUnsupportedMediaType, "encoding_unsupported", err.Error(), nil)
		return err
	}
	if encoding == "gzip" {
		gzipReader, err := gzip.NewReader(compressed)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_gzip", err.Error(), nil)
			return err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	reader = io.LimitReader(reader, maxLowerLogDecompressedBody+1)
	decoder := json.NewDecoder(reader)
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

func validateLowerLogBatch(batch LowerLogBatch) error {
	if batch.SourceID == "" || len(batch.SourceID) > 128 || strings.IndexFunc(batch.SourceID, func(r rune) bool { return r < 0x20 }) >= 0 {
		return errors.New("source_id must contain 1..128 printable characters")
	}
	if batch.BootID == "" || len(batch.BootID) > 128 || strings.IndexFunc(batch.BootID, func(r rune) bool { return r < 0x20 }) >= 0 {
		return errors.New("boot_id must contain 1..128 printable characters")
	}
	if batch.FirstSequence == 0 || len(batch.Events) == 0 || len(batch.Events) > maxLowerLogEvents {
		return errors.New("events must contain 1..500 records and first_sequence must be positive")
	}
	for index, event := range batch.Events {
		expected := batch.FirstSequence + uint64(index)
		if event.SourceID != batch.SourceID || event.BootID != batch.BootID || event.Sequence != expected {
			return fmt.Errorf("event %d identity or sequence does not match batch", index)
		}
		if event.Timestamp.IsZero() || !validLowerLogLevel(strings.ToUpper(event.Level)) {
			return fmt.Errorf("event %d has invalid timestamp or level", index)
		}
		if len(event.Component) > 256 || len(event.PortID) > 256 || len(event.DeviceID) > 128 || len(event.Message) > maxLowerLogMessageBytes {
			return fmt.Errorf("event %d exceeds field length limits", index)
		}
		if len(event.Fields) > 64 {
			return fmt.Errorf("event %d has too many structured fields", index)
		}
	}
	return nil
}

func validLowerLogLevel(value string) bool {
	switch strings.ToUpper(value) {
	case "TRACE", "DEBUG", "INFO", "WARN", "ERROR":
		return true
	default:
		return false
	}
}
