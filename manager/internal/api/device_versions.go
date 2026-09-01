package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/domain"
)

type DeviceVersionRequest struct {
	Page       int
	PageSize   int
	SourceID   string
	PortID     string
	ModuleType string
	Status     string
	Query      string
}

type DeviceVersionItem struct {
	SourceID                      string    `json:"source_id"`
	SourceName                    string    `json:"source_name"`
	SourceRevision                uint64    `json:"source_revision"`
	RCDSoftwareVersion            string    `json:"rcd_software_version"`
	RCDBuildCommit                string    `json:"rcd_build_commit"`
	PortID                        string    `json:"port_id"`
	PortName                      string    `json:"port_name"`
	DeviceID                      string    `json:"device_id"`
	DeviceName                    string    `json:"device_name"`
	ConfiguredModuleType          string    `json:"configured_module_type"`
	DetectedModuleType            string    `json:"detected_module_type"`
	TypeMismatch                  bool      `json:"type_mismatch"`
	CommunicationModuleVersion    string    `json:"communication_module_version"`
	CommunicationModuleVersionRaw *uint16   `json:"communication_module_version_raw"`
	PLCVersion                    string    `json:"plc_version"`
	PLCVersionRaw                 *uint16   `json:"plc_version_raw"`
	VersionPayload                string    `json:"version_payload"`
	VersionState                  string    `json:"version_state"`
	LastVersionAttemptAt          time.Time `json:"last_version_attempt_at,omitempty,omitzero"`
	LastVersionSuccessAt          time.Time `json:"last_version_success_at,omitempty,omitzero"`
	LastVersionError              string    `json:"last_version_error,omitempty"`
	ConsecutiveFailures           uint32    `json:"consecutive_failures"`
	StationRevision               string    `json:"station_revision"`
	VersionSupported              bool      `json:"version_supported"`
	SourceOnline                  bool      `json:"source_online"`
	Cached                        bool      `json:"cached"`
	Stale                         bool      `json:"stale"`
	PortEnabled                   bool      `json:"port_enabled"`
	PortAppliedEnabled            bool      `json:"port_applied_enabled"`
	PortOpen                      bool      `json:"port_open"`
	DeviceReadEnabled             bool      `json:"device_read_enabled"`
	AppliedDeviceReadEnabled      bool      `json:"applied_device_read_enabled"`
	CommunicationOK               bool      `json:"communication_ok"`
	ControlPending                bool      `json:"control_pending"`
	PendingCommands               uint32    `json:"pending_commands"`
	ObservedAt                    time.Time `json:"observed_at,omitempty,omitzero"`
}

type DeviceVersionSummary struct {
	Total                 int                           `json:"total"`
	Domestic              int                           `json:"domestic"`
	LegacyRussian         int                           `json:"legacy_russian"`
	Unknown               int                           `json:"unknown"`
	Mismatch              int                           `json:"mismatch"`
	Failed                int                           `json:"failed"`
	OfflineCached         int                           `json:"offline_cached"`
	LowerSoftwareVersions []LowerSoftwareVersionSummary `json:"lower_software_versions"`
}

type LowerSoftwareVersionSummary struct {
	SoftwareVersion string `json:"software_version"`
	BuildCommit     string `json:"build_commit"`
	SourceCount     int    `json:"source_count"`
}

type DeviceVersionPage struct {
	Items    []DeviceVersionItem  `json:"items"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
	Total    int                  `json:"total"`
	Summary  DeviceVersionSummary `json:"summary"`
}

type DeviceVersionMutation struct {
	SourceRevision  uint64 `json:"source_revision"`
	StationRevision string `json:"station_revision"`
	PortID          string `json:"port_id"`
	DeviceID        string `json:"device_id"`
	ModuleType      string `json:"module_type,omitempty"`
}

type DeviceVersionsService interface {
	ListDeviceVersions(context.Context, DeviceVersionRequest) (DeviceVersionPage, error)
	SetDeviceModuleType(context.Context, string, DeviceVersionMutation) (json.RawMessage, error)
	QueryDeviceVersion(context.Context, string, DeviceVersionMutation) (json.RawMessage, error)
}

func (h *Handler) deviceVersionRoutes() {
	h.mux.HandleFunc("GET /api/v1/device-versions", h.listDeviceVersions)
	h.mux.HandleFunc("PUT /api/v1/sources/{id}/operations/device-module-type", h.setDeviceModuleType)
	h.mux.HandleFunc("POST /api/v1/sources/{id}/operations/device-version/query", h.queryDeviceVersion)
}

func (h *Handler) listDeviceVersions(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(DeviceVersionsService)
	if !ok {
		h.respond(w, nil, unsupportedOperations(), http.StatusOK)
		return
	}
	page, pageSize := 1, 100
	var err error
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			writeError(w, http.StatusBadRequest, "invalid_page", "page must be a positive integer", nil)
			return
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_page", "page_size must be between 1 and 1000", nil)
			return
		}
	}
	request := DeviceVersionRequest{
		Page: page, PageSize: pageSize, SourceID: r.URL.Query().Get("source_id"),
		PortID: r.URL.Query().Get("port_id"), ModuleType: r.URL.Query().Get("module_type"),
		Status: r.URL.Query().Get("status"), Query: r.URL.Query().Get("q"),
	}
	if request.ModuleType != "" && !domain.ValidModuleType(request.ModuleType) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid module_type", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	value, err := service.ListDeviceVersions(r.Context(), request)
	h.respond(w, value, err, http.StatusOK)
}

func (h *Handler) setDeviceModuleType(w http.ResponseWriter, r *http.Request) {
	h.mutateDeviceVersion(w, r, true)
}

func (h *Handler) queryDeviceVersion(w http.ResponseWriter, r *http.Request) {
	h.mutateDeviceVersion(w, r, false)
}

func (h *Handler) mutateDeviceVersion(w http.ResponseWriter, r *http.Request, setType bool) {
	expected, ok := requireOperationRevision(w, r)
	if !ok {
		return
	}
	var mutation DeviceVersionMutation
	if decodeJSON(w, r, &mutation) != nil {
		return
	}
	if mutation.SourceRevision == 0 || mutation.StationRevision == "" {
		writeError(w, http.StatusPreconditionRequired, "revision_required", "source_revision and station_revision are required", nil)
		return
	}
	if !domain.ValidStationRevision(expected) || !domain.ValidStationRevision(mutation.StationRevision) {
		writeError(w, http.StatusBadRequest, "invalid_revision", "If-Match and station_revision must be canonical decimal revisions", nil)
		return
	}
	if expected != mutation.StationRevision {
		writeError(w, http.StatusConflict, "revision_conflict", "If-Match must equal station_revision", map[string]string{"expected": expected, "actual": mutation.StationRevision})
		return
	}
	if setType {
		if !domain.ValidModuleType(mutation.ModuleType) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "invalid module_type", nil)
			return
		}
	} else if strings.TrimSpace(mutation.ModuleType) != "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "module_type is not accepted for a version query", nil)
		return
	}
	service, ok := h.service.(DeviceVersionsService)
	if !ok {
		h.respond(w, nil, unsupportedOperations(), http.StatusOK)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var result json.RawMessage
	var err error
	if setType {
		result, err = service.SetDeviceModuleType(r.Context(), r.PathValue("id"), mutation)
	} else {
		result, err = service.QueryDeviceVersion(r.Context(), r.PathValue("id"), mutation)
	}
	if err == nil {
		var revision struct {
			StationRevision string `json:"station_revision"`
		}
		if json.Unmarshal(result, &revision) == nil && domain.ValidStationRevision(revision.StationRevision) {
			w.Header().Set("ETag", strconv.Quote(revision.StationRevision))
		}
	}
	h.respond(w, result, err, http.StatusOK)
}
