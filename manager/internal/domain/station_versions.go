package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	ModuleTypeUnknown       = "unknown"
	ModuleTypeDomestic      = "domestic"
	ModuleTypeLegacyRussian = "legacy_russian"
)

func ValidModuleType(value string) bool {
	switch value {
	case ModuleTypeUnknown, ModuleTypeDomestic, ModuleTypeLegacyRussian:
		return true
	default:
		return false
	}
}

// ParseStationRevision accepts the lower's canonical decimal u64 revision.
// Station revisions are monotonic within one lower connection epoch, so a
// numeric representation is required for rejecting late status responses.
func ParseStationRevision(value string) (uint64, bool) {
	revision, err := strconv.ParseUint(value, 10, 64)
	return revision, err == nil && strconv.FormatUint(revision, 10) == value
}

func ValidStationRevision(value string) bool {
	_, ok := ParseStationRevision(value)
	return ok
}

// StationVersion is version and acquisition metadata for one physical station.
// It is source-scoped and never participates in business KKS merging or routing.
type StationVersion struct {
	DeviceID                      string    `json:"device_id"`
	DeviceName                    string    `json:"device_name,omitempty"`
	ReadEnabled                   bool      `json:"read_enabled"`
	AppliedReadEnabled            bool      `json:"applied_read_enabled"`
	CommunicationOK               bool      `json:"communication_ok"`
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
	Stale                         bool      `json:"stale"`
}

type StationVersionPort struct {
	PortID          string           `json:"port_id"`
	PortName        string           `json:"port_name,omitempty"`
	Enabled         bool             `json:"enabled"`
	AppliedEnabled  bool             `json:"applied_enabled"`
	Open            bool             `json:"open"`
	ControlPending  bool             `json:"control_pending"`
	PendingCommands uint32           `json:"pending_commands"`
	Devices         []StationVersion `json:"devices"`
}

type SourceStationVersions struct {
	SourceID            string               `json:"source_id"`
	TopologyFingerprint string               `json:"topology_fingerprint"`
	Revision            string               `json:"revision"`
	StationRevision     string               `json:"station_revision"`
	SoftwareVersion     string               `json:"software_version"`
	BuildCommit         string               `json:"build_commit"`
	Ports               []StationVersionPort `json:"ports"`
	ObservedAt          time.Time            `json:"observed_at,omitempty,omitzero"`
	Cached              bool                 `json:"cached"`
}

func (s SourceStationVersions) Clone() SourceStationVersions {
	clone := s
	clone.Ports = append([]StationVersionPort(nil), s.Ports...)
	for i := range clone.Ports {
		clone.Ports[i].Devices = append([]StationVersion(nil), s.Ports[i].Devices...)
		for j := range clone.Ports[i].Devices {
			if value := clone.Ports[i].Devices[j].CommunicationModuleVersionRaw; value != nil {
				copyValue := *value
				clone.Ports[i].Devices[j].CommunicationModuleVersionRaw = &copyValue
			}
			if value := clone.Ports[i].Devices[j].PLCVersionRaw; value != nil {
				copyValue := *value
				clone.Ports[i].Devices[j].PLCVersionRaw = &copyValue
			}
		}
	}
	return clone
}

func (s SourceStationVersions) Device(portID, deviceID string) (StationVersionPort, StationVersion, bool) {
	for _, port := range s.Ports {
		if port.PortID != portID {
			continue
		}
		for _, device := range port.Devices {
			if device.DeviceID == deviceID {
				return port, device, true
			}
		}
	}
	return StationVersionPort{}, StationVersion{}, false
}

// SourceTopologyFingerprint binds an offline cache to the exact discovered
// physical topology. JSON field order is fixed by the typed structure.
func SourceTopologyFingerprint(topology *SourceTopology) string {
	if topology == nil {
		return ""
	}
	payload, err := json.Marshal(topology)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func ValidVersionPayload(value string) bool {
	if len(value) != 0 && len(value) != 8 {
		return false
	}
	if value != strings.ToUpper(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
