package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/domain"
)

type FleetService interface {
	FleetStatus(context.Context) (domain.FleetStatus, error)
}

func (h *Handler) fleetStatus(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(FleetService)
	if !ok {
		h.respond(w, domain.FleetStatus{Sources: []domain.FleetSourceStatus{}}, nil, 200)
		return
	}
	value, err := service.FleetStatus(r.Context())
	h.respond(w, value, err, 200)
}
