package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
	"time"
)

type StateInputView struct {
	calculation.StateInput
	DataType       string                   `json:"data_type"`
	ResolvedTarget *calculation.StateTarget `json:"resolved_target,omitempty"`
	Value          uint32                   `json:"value"`
	StateText      string                   `json:"state_text"`
	StatusCode     domain.StatusCode        `json:"status_code"`
	ObservedAt     time.Time                `json:"observed_at,omitempty"`
	Error          string                   `json:"error,omitempty"`
}
type StateInputPage struct {
	Items    []StateInputView `json:"items"`
	Total    int              `json:"total"`
	Limit    int              `json:"limit"`
	Offset   int              `json:"offset"`
	Revision uint64           `json:"revision"`
}
type StateCandidate struct {
	calculation.StateTarget
	SourceName string `json:"source_name"`
	GroupID    string `json:"group_id"`
	GroupName  string `json:"group_name"`
	DeviceName string `json:"device_name"`
}
type StateCandidatePage struct {
	Items  []StateCandidate `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}
type CalculationStateService interface {
	ListCalculationStateInputs(context.Context, PageRequest, string, string, string) (StateInputPage, error)
	ListCalculationStateCandidates(context.Context, PageRequest, string, string) (StateCandidatePage, error)
}

func (h *Handler) calculationStateInputs(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, 400, "invalid_page", err.Error(), nil)
		return
	}
	request.Kind = r.URL.Query().Get("rule")
	view := r.URL.Query().Get("view")
	if view != "" && view != "active" && view != "draft" {
		writeError(w, 400, "invalid_view", "view must be active or draft", nil)
		return
	}
	service, ok := h.service.(CalculationStateService)
	if !ok {
		h.respond(w, nil, &calculationServiceError{message: "state inputs unavailable"}, 200)
		return
	}
	value, err := service.ListCalculationStateInputs(r.Context(), request, view, r.URL.Query().Get("mode"), r.URL.Query().Get("source_id"))
	h.respond(w, value, err, 200)
}
func (h *Handler) calculationStateCandidates(w http.ResponseWriter, r *http.Request) {
	request, err := parsePage(r)
	if err != nil {
		writeError(w, 400, "invalid_page", err.Error(), nil)
		return
	}
	service, ok := h.service.(CalculationStateService)
	if !ok {
		h.respond(w, nil, &calculationServiceError{message: "state inputs unavailable"}, 200)
		return
	}
	value, err := service.ListCalculationStateCandidates(r.Context(), request, r.URL.Query().Get("group_id"), r.URL.Query().Get("source_id"))
	h.respond(w, value, err, 200)
}
