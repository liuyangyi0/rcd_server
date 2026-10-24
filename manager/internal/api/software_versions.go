package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/domain"
)

type SoftwareVersionService interface {
	SoftwareVersions(context.Context, bool) ([]domain.SoftwareVersion, error)
}

func (h *Handler) softwareVersions(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(SoftwareVersionService)
	if !ok {
		writeError(w, 501, "unsupported", "software versions unavailable", nil)
		return
	}
	values, err := service.SoftwareVersions(r.Context(), r.URL.Query().Get("refresh") == "1")
	h.respond(w, map[string]any{"items": values}, err, http.StatusOK)
}
