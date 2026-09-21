package api

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/calculation"
)

type CalculationStatus struct {
	RoutingMode              string    `json:"routing_mode"`
	ActiveRevision           uint64    `json:"active_revision"`
	ConfigHash               string    `json:"config_hash,omitempty"`
	RuleCount                int       `json:"rule_count"`
	GoodCount                int       `json:"good_count"`
	BadCount                 int       `json:"bad_count"`
	DefaultRouteRevision     uint64    `json:"default_route_revision"`
	LastEvaluationAt         time.Time `json:"last_evaluation_at,omitempty,omitzero"`
	LastEvaluationDurationMS float64   `json:"last_evaluation_duration_ms"`
	LastError                string    `json:"last_error,omitempty"`
	PendingRestart           bool      `json:"pending_restart"`
	PendingRevision          uint64    `json:"pending_revision,omitempty"`
	PendingHash              string    `json:"pending_hash,omitempty"`
}

type CalculationResultPage struct {
	Items  []calculation.Result `json:"items"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

type CalculationRulePage struct {
	Items  []calculation.RuleView `json:"items"`
	Total  int                    `json:"total"`
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

type CalculationVersionPage struct {
	Items  []calculation.Version `json:"items"`
	Total  int                   `json:"total"`
	Limit  int                   `json:"limit"`
	Offset int                   `json:"offset"`
}

type CalculationValidation struct {
	Valid     bool                  `json:"valid"`
	Hash      string                `json:"hash,omitempty"`
	RuleCount int                   `json:"rule_count"`
	Problems  []calculation.Problem `json:"problems"`
}

// CalculationService is optional so compact test services and deployments
// without the calculation migration remain source compatible.
type CalculationService interface {
	GetCalculationStatus(context.Context) (CalculationStatus, error)
	ListCalculationResults(context.Context, PageRequest) (CalculationResultPage, error)
	ListCalculationRules(context.Context, PageRequest) (CalculationRulePage, error)
	GetCalculationDraft(context.Context) (calculation.Draft, error)
	SaveCalculationDraft(context.Context, uint64, string) (calculation.Draft, error)
	ImportCalculationFromLower(context.Context, string, uint64) (calculation.Draft, error)
	ValidateCalculation(context.Context, *string) (CalculationValidation, error)
	ActivateCalculation(context.Context, uint64) (calculation.Version, error)
	DeactivateCalculation(context.Context, uint64) error
	ListCalculationVersions(context.Context, PageRequest) (CalculationVersionPage, error)
	RestoreCalculationVersion(context.Context, int64, uint64) (calculation.Version, error)
}

func (h *Handler) calculationRoutes() {
	const base = "/api/v1/calculations"
	h.mux.HandleFunc("GET "+base+"/state-inputs", h.calculationStateInputs)
	h.mux.HandleFunc("GET "+base+"/state-input-candidates", h.calculationStateCandidates)
	h.mux.HandleFunc("GET "+base+"/status", h.calculationStatus)
	h.mux.HandleFunc("GET "+base+"/results", h.calculationResults)
	h.mux.HandleFunc("GET "+base+"/results/export", h.exportCalculationResults)
	h.mux.HandleFunc("GET "+base+"/rules", h.calculationRules)
	h.mux.HandleFunc("GET "+base+"/config/draft", h.calculationDraft)
	h.mux.HandleFunc("PUT "+base+"/config/draft", h.saveCalculationDraft)
	h.mux.HandleFunc("POST "+base+"/config/import-file", h.importCalculationFile)
	h.mux.HandleFunc("POST "+base+"/config/import-lower", h.importCalculationLower)
	h.mux.HandleFunc("POST "+base+"/config/validate", h.validateCalculation)
	h.mux.HandleFunc("POST "+base+"/config/activate", h.activateCalculation)
	h.mux.HandleFunc("POST "+base+"/config/deactivate", h.deactivateCalculation)
	h.mux.HandleFunc("GET "+base+"/config/versions", h.calculationVersions)
	h.mux.HandleFunc("POST "+base+"/config/versions/{id}/restore", h.restoreCalculationVersion)
}

func (h *Handler) calculations() (CalculationService, error) {
	service, ok := h.service.(CalculationService)
	if !ok {
		return nil, &calculationServiceError{message: "calculation center is not available"}
	}
	return service, nil
}

type calculationServiceError struct{ message string }

func (e *calculationServiceError) Error() string { return e.message }

func (h *Handler) calculationStatus(w http.ResponseWriter, r *http.Request) {
	service, err := h.calculations()
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	value, err := service.GetCalculationStatus(r.Context())
	if err == nil && value.ActiveRevision != 0 {
		setETag(w, value.ActiveRevision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) calculationResults(w http.ResponseWriter, r *http.Request) {
	h.calculationPage(w, r, false)
}

func (h *Handler) calculationRules(w http.ResponseWriter, r *http.Request) {
	h.calculationPage(w, r, true)
}

func (h *Handler) calculationPage(w http.ResponseWriter, r *http.Request, rules bool) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	service, err := h.calculations()
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	if rules {
		value, listErr := service.ListCalculationRules(r.Context(), request)
		h.respond(w, value, listErr, http.StatusOK)
		return
	}
	value, listErr := service.ListCalculationResults(r.Context(), request)
	h.respond(w, value, listErr, http.StatusOK)
}

func (h *Handler) calculationDraft(w http.ResponseWriter, r *http.Request) {
	service, err := h.calculations()
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	value, err := service.GetCalculationDraft(r.Context())
	if err == nil {
		setETag(w, value.Revision)
	}
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) saveCalculationDraft(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	content, ok := decodeCalculationContent(w, r)
	if !ok {
		return
	}
	service, err := h.calculations()
	if err == nil {
		var value calculation.Draft
		value, err = service.SaveCalculationDraft(r.Context(), revision, content)
		if err == nil {
			setETag(w, value.Revision)
		}
		h.respond(w, value, err, http.StatusOK)
		return
	}
	h.respond(w, nil, err, http.StatusOK)
}

func (h *Handler) importCalculationFile(w http.ResponseWriter, r *http.Request) {
	// The browser reads the selected local file and sends UTF-8 text. Keeping
	// this endpoint JSON-only avoids multipart copies for a bounded 4 MiB file.
	h.saveCalculationDraft(w, r)
}

func (h *Handler) importCalculationLower(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceID       string `json:"source_id"`
		SourceRevision uint64 `json:"source_revision"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	service, err := h.calculations()
	if err == nil {
		var value calculation.Draft
		value, err = service.ImportCalculationFromLower(r.Context(), body.SourceID, body.SourceRevision)
		if err == nil {
			// Import uses the same optimistic draft revision supplied by If-Match.
			value, err = service.SaveCalculationDraft(r.Context(), revision, value.Content)
		}
		if err == nil {
			setETag(w, value.Revision)
		}
		h.respond(w, value, err, http.StatusOK)
		return
	}
	h.respond(w, nil, err, http.StatusOK)
}

func (h *Handler) validateCalculation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content *string `json:"content,omitempty"`
	}
	if decodeCalculationJSON(w, r, &body) != nil {
		return
	}
	service, err := h.calculations()
	if err == nil {
		var value CalculationValidation
		value, err = service.ValidateCalculation(r.Context(), body.Content)
		h.respond(w, value, err, http.StatusOK)
		return
	}
	h.respond(w, nil, err, http.StatusOK)
}

func (h *Handler) activateCalculation(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	service, err := h.calculations()
	if err == nil {
		var value calculation.Version
		value, err = service.ActivateCalculation(r.Context(), revision)
		if err == nil {
			setETag(w, value.Revision)
		}
		h.respond(w, value, err, http.StatusOK)
		return
	}
	h.respond(w, nil, err, http.StatusOK)
}

func (h *Handler) deactivateCalculation(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	service, err := h.calculations()
	if err == nil {
		err = service.DeactivateCalculation(r.Context(), revision)
		h.respond(w, nil, err, http.StatusNoContent)
		return
	}
	h.respond(w, nil, err, http.StatusNoContent)
}

func (h *Handler) calculationVersions(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	service, err := h.calculations()
	if err == nil {
		var value CalculationVersionPage
		value, err = service.ListCalculationVersions(r.Context(), request)
		h.respond(w, value, err, http.StatusOK)
		return
	}
	h.respond(w, nil, err, http.StatusOK)
}

func (h *Handler) restoreCalculationVersion(w http.ResponseWriter, r *http.Request) {
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	id, ok := parseInt64Path(w, r.PathValue("id"))
	if !ok {
		return
	}
	service, err := h.calculations()
	if err == nil {
		var value calculation.Version
		value, err = service.RestoreCalculationVersion(r.Context(), id, revision)
		if err == nil {
			setETag(w, value.Revision)
		}
		h.respond(w, value, err, http.StatusOK)
		return
	}
	h.respond(w, nil, err, http.StatusOK)
}

func (h *Handler) exportCalculationResults(w http.ResponseWriter, r *http.Request) {
	service, err := h.calculations()
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	request, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_page", err.Error(), nil)
		return
	}
	request.Limit, request.Offset = calculation.MaxRules, 0
	page, err := service.ListCalculationResults(r.Context(), request)
	if err != nil {
		h.respond(w, nil, err, http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="rcd-manager-calculations.csv"`)
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"KKS", "DataType", "Expression", "Value", "StatusCode", "EvaluatedAt", "Error", "Dependencies", "ConfigRevision", "DefaultRouteRevision"})
	for _, item := range page.Items {
		_ = writer.Write([]string{item.NodeID, string(item.DataType), item.Expression, stringifyCSVValue(item.Value),
			strconv.FormatUint(uint64(item.StatusCode), 10), item.EvaluatedAt.Format(time.RFC3339Nano), item.Error,
			strconv.Itoa(item.DependencyCount), strconv.FormatUint(item.ConfigRevision, 10), strconv.FormatUint(item.DefaultRouteRevision, 10)})
	}
	writer.Flush()
}

func decodeCalculationContent(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Content string `json:"content"`
		StateInputs *[]calculation.StateInput `json:"state_inputs,omitempty"`
	}
	if decodeCalculationJSON(w, r, &body) != nil {
		return "", false
	}
	if body.StateInputs != nil {
		content, err := calculation.ReplaceStateInputs(body.Content, *body.StateInputs)
		if err != nil { status:=http.StatusUnprocessableEntity;if errors.Is(err,calculation.ErrConfigTooLarge) {status=http.StatusRequestEntityTooLarge};writeError(w, status, "invalid_configuration", err.Error(), nil); return "", false }
		return content, true
	}
	return body.Content, true
}

func decodeCalculationJSON(w http.ResponseWriter, r *http.Request, target any) error {
	// Reuse the hardened operations decoder because it allows the configured
	// 4 MiB calculation ceiling plus JSON escaping overhead.
	return decodeOperationConfig(w, r, target)
}

func stringifyCSVValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(fmt.Sprint(value), "\r", " "), "\n", " "))
}
