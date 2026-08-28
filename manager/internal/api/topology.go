package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/domain"
)

type TopologyDeviceView struct {
	DeviceID         string `json:"device_id"`
	DeviceAddress    uint32 `json:"device_address"`
	Name             string `json:"name"`
	KKSPrefix        string `json:"kks_prefix"`
	PointCount       int    `json:"point_count"`
	CalculationCount int    `json:"calculation_count"`
	GroupID          string `json:"group_id"`
	Bound            bool   `json:"bound"`
}

type TopologyPortView struct {
	PortID       string               `json:"port_id"`
	PortName     string               `json:"port_name"`
	SendNodeID   string               `json:"send_node_id"`
	SendMethodID string               `json:"send_method_id"`
	PointCount   int                  `json:"point_count"`
	Devices      []TopologyDeviceView `json:"devices"`
}

type SourceTopologyView struct {
	SourceID                    string              `json:"source_id"`
	GroupID                     string              `json:"group_id"`
	Revision                    uint64              `json:"revision"`
	Supported                   bool                `json:"supported"`
	CatalogState                domain.CatalogState `json:"catalog_state"`
	SchemaVersion               int                 `json:"schema_version"`
	Ports                       []TopologyPortView  `json:"ports"`
	CalculationCount            int                 `json:"calculation_count"`
	UnassignedPointCount        int                 `json:"unassigned_point_count"`
	CrossDeviceCalculationCount int                 `json:"cross_device_calculation_count"`
}

type TopologyService interface {
	GetSourceTopology(context.Context, string) (SourceTopologyView, error)
}

func (h *Handler) getSourceTopology(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(TopologyService)
	if !ok {
		h.respond(w, nil, domain.ErrNotFound, http.StatusOK)
		return
	}
	view, err := service.GetSourceTopology(r.Context(), r.PathValue("id"))
	if err == nil {
		setETag(w, view.Revision)
	}
	h.respond(w, view, err, http.StatusOK)
}
