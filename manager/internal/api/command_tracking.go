package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/domain"
	"strconv"
)

type CommandTrackingService interface {
	CommandDetail(context.Context, string) (domain.CommandDetail, error)
	CommandEvents(context.Context, string, int, int) ([]domain.TrackedCommand, int, error)
}

func (h *Handler) commandTrackingRoutes() {
	h.mux.HandleFunc("GET /api/v1/commands/{id}", func(w http.ResponseWriter, r *http.Request) {
		service, ok := h.service.(CommandTrackingService)
		if !ok {
			writeError(w, 501, "unsupported", "command details unavailable", nil)
			return
		}
		value, err := service.CommandDetail(r.Context(), r.PathValue("id"))
		h.respond(w, value, err, 200)
	})
	h.mux.HandleFunc("GET /api/v1/commands/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		service, ok := h.service.(CommandTrackingService)
		if !ok {
			writeError(w, 501, "unsupported", "command details unavailable", nil)
			return
		}
		limit, offset := 100, 0
		var err error
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
		}
		if err == nil {
			if raw := r.URL.Query().Get("offset"); raw != "" {
				offset, err = strconv.Atoi(raw)
			}
		}
		if err != nil || limit < 1 || limit > 1000 || offset < 0 {
			writeError(w, 400, "invalid_request", "invalid pagination", nil)
			return
		}
		values, total, err := service.CommandEvents(r.Context(), r.PathValue("id"), limit, offset)
		h.respond(w, map[string]any{"items": values, "total": total, "limit": limit, "offset": offset}, err, 200)
	})
}
