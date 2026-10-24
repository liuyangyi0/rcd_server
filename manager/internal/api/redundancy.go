package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/domain"
)

type RedundancyService interface {
	Redundancy(context.Context, string) (domain.RedundancyView, error)
	SaveRedundancy(context.Context, string, uint64, domain.RedundancyPolicy) (domain.RedundancyPolicy, error)
	SetSessionAutomatic(context.Context, string, string, bool, uint64) (domain.GroupRoute, error)
}

func (h *Handler) redundancyRoutes() {
	h.mux.HandleFunc("GET /api/v1/groups/{id}/redundancy", h.getRedundancy)
	h.mux.HandleFunc("PUT /api/v1/groups/{id}/redundancy", h.putRedundancy)
	h.mux.HandleFunc("PUT /api/v1/sessions/{session}/groups/{group}/automatic", h.putSessionAutomatic)
}
func (h *Handler) redundancyService(w http.ResponseWriter) (RedundancyService, bool) {
	s, ok := h.service.(RedundancyService)
	if !ok {
		writeError(w, 501, "unsupported", "redundancy unavailable", nil)
	}
	return s, ok
}
func (h *Handler) getRedundancy(w http.ResponseWriter, r *http.Request) {
	s, ok := h.redundancyService(w)
	if !ok {
		return
	}
	v, err := s.Redundancy(r.Context(), r.PathValue("id"))
	if err == nil {
		setETag(w, v.Policy.Revision)
	}
	h.respond(w, v, err, 200)
}
func (h *Handler) putRedundancy(w http.ResponseWriter, r *http.Request) {
	s, ok := h.redundancyService(w)
	if !ok {
		return
	}
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var p domain.RedundancyPolicy
	if err := decodeJSON(w, r, &p); err != nil {
		return
	}
	v, err := s.SaveRedundancy(r.Context(), r.PathValue("id"), revision, p)
	if err == nil {
		setETag(w, v.Revision)
	}
	h.respond(w, v, err, 200)
}
func (h *Handler) putSessionAutomatic(w http.ResponseWriter, r *http.Request) {
	s, ok := h.redundancyService(w)
	if !ok {
		return
	}
	revision, ok := requireRevision(w, r)
	if !ok {
		return
	}
	var body struct {
		Automatic *bool  `json:"automatic"`
		Revision  uint64 `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	if body.Automatic == nil || body.Revision != revision {
		writeError(w, 422, "invalid_request", "automatic and matching revision are required", nil)
		return
	}
	v, err := s.SetSessionAutomatic(r.Context(), r.PathValue("session"), r.PathValue("group"), *body.Automatic, revision)
	if err == nil {
		setETag(w, v.Revision)
	}
	h.respond(w, v, err, 200)
}
