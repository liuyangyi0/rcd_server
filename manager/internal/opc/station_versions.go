package opc

import (
	"encoding/json"
	"time"

	"rcd_manager/internal/domain"
)

type stationVersionStatusWire struct {
	SchemaVersion   uint32                   `json:"schema_version"`
	Revision        string                   `json:"revision"`
	StationRevision string                   `json:"station_revision"`
	SoftwareVersion string                   `json:"software_version"`
	BuildCommit     string                   `json:"build_commit"`
	Ports           []stationVersionPortWire `json:"ports"`
}

type stationVersionPortWire struct {
	PortID          string                     `json:"port_id"`
	PortName        string                     `json:"port_name"`
	Enabled         bool                       `json:"enabled"`
	AppliedEnabled  bool                       `json:"applied_enabled"`
	Open            bool                       `json:"open"`
	ControlPending  bool                       `json:"control_pending"`
	PendingCommands uint32                     `json:"pending_commands"`
	Devices         []stationVersionDeviceWire `json:"devices"`
}

type stationVersionDeviceWire struct {
	DeviceID                      string                       `json:"device_id"`
	Name                          string                       `json:"name"`
	ReadEnabled                   bool                         `json:"read_enabled"`
	AppliedReadEnabled            bool                         `json:"applied_read_enabled"`
	CommunicationOK               bool                         `json:"communication_ok"`
	ConfiguredModuleType          string                       `json:"configured_module_type"`
	DetectedModuleType            string                       `json:"detected_module_type"`
	TypeMismatch                  bool                         `json:"type_mismatch"`
	CommunicationModuleVersion    string                       `json:"communication_module_version"`
	CommunicationModuleVersionRaw *uint16                      `json:"communication_module_version_raw"`
	PLCVersion                    string                       `json:"plc_version"`
	PLCVersionRaw                 *uint16                      `json:"plc_version_raw"`
	VersionPayload                string                       `json:"version_payload"`
	VersionState                  string                       `json:"version_state"`
	LastVersionAttemptAt          time.Time                    `json:"last_version_attempt_at"`
	LastVersionSuccessAt          time.Time                    `json:"last_version_success_at"`
	LastVersionError              string                       `json:"last_version_error"`
	ConsecutiveFailures           uint32                       `json:"consecutive_failures"`
	Stale                         bool                         `json:"stale"`
	StationVersion                *stationVersionDeviceOverlay `json:"station_version,omitempty"`
}

// stationVersionDeviceOverlay keeps the decoder compatible with early lower
// builds that nested the additive fields under station_version.
type stationVersionDeviceOverlay struct {
	ConfiguredModuleType          string    `json:"configured_module_type"`
	DetectedModuleType            string    `json:"detected_module_type"`
	TypeMismatch                  bool      `json:"type_mismatch"`
	CommunicationModuleVersion    string    `json:"communication_module_version"`
	CommunicationModuleVersionRaw *uint16   `json:"communication_module_version_raw"`
	PLCVersion                    string    `json:"plc_version"`
	PLCVersionRaw                 *uint16   `json:"plc_version_raw"`
	VersionPayload                string    `json:"version_payload"`
	VersionState                  string    `json:"version_state"`
	LastVersionAttemptAt          time.Time `json:"last_version_attempt_at"`
	LastVersionSuccessAt          time.Time `json:"last_version_success_at"`
	LastVersionError              string    `json:"last_version_error"`
	ConsecutiveFailures           uint32    `json:"consecutive_failures"`
	Stale                         bool      `json:"stale"`
}

func DecodeStationVersionStatus(request domain.OperationRequest, result json.RawMessage, sourceID, topologyFingerprint string, observedAt time.Time) (domain.SourceStationVersions, bool, error) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(result, &probe) != nil {
		return domain.SourceStationVersions{}, false, invalidOperationField("result")
	}
	if _, present := probe["station_revision"]; !present {
		return domain.SourceStationVersions{}, false, nil
	}
	if err := validateOperationResult(request, result); err != nil {
		return domain.SourceStationVersions{}, true, err
	}
	var wire stationVersionStatusWire
	if json.Unmarshal(result, &wire) != nil {
		return domain.SourceStationVersions{}, true, invalidOperationField("station_versions")
	}
	snapshot := domain.SourceStationVersions{
		SourceID: sourceID, TopologyFingerprint: topologyFingerprint, Revision: wire.Revision,
		StationRevision: wire.StationRevision, SoftwareVersion: wire.SoftwareVersion,
		BuildCommit: wire.BuildCommit, ObservedAt: observedAt,
	}
	for _, rawPort := range wire.Ports {
		port := domain.StationVersionPort{
			PortID: rawPort.PortID, PortName: rawPort.PortName, Enabled: rawPort.Enabled,
			AppliedEnabled: rawPort.AppliedEnabled, Open: rawPort.Open,
			ControlPending: rawPort.ControlPending, PendingCommands: rawPort.PendingCommands,
		}
		for _, rawDevice := range rawPort.Devices {
			if nested := rawDevice.StationVersion; nested != nil && rawDevice.ConfiguredModuleType == "" {
				rawDevice.ConfiguredModuleType = nested.ConfiguredModuleType
				rawDevice.DetectedModuleType = nested.DetectedModuleType
				rawDevice.TypeMismatch = nested.TypeMismatch
				rawDevice.CommunicationModuleVersion = nested.CommunicationModuleVersion
				rawDevice.CommunicationModuleVersionRaw = nested.CommunicationModuleVersionRaw
				rawDevice.PLCVersion = nested.PLCVersion
				rawDevice.PLCVersionRaw = nested.PLCVersionRaw
				rawDevice.VersionPayload = nested.VersionPayload
				rawDevice.VersionState = nested.VersionState
				rawDevice.LastVersionAttemptAt = nested.LastVersionAttemptAt
				rawDevice.LastVersionSuccessAt = nested.LastVersionSuccessAt
				rawDevice.LastVersionError = nested.LastVersionError
				rawDevice.ConsecutiveFailures = nested.ConsecutiveFailures
				rawDevice.Stale = nested.Stale
			}
			port.Devices = append(port.Devices, domain.StationVersion{
				DeviceID: rawDevice.DeviceID, DeviceName: rawDevice.Name,
				ReadEnabled: rawDevice.ReadEnabled, AppliedReadEnabled: rawDevice.AppliedReadEnabled,
				CommunicationOK:      rawDevice.CommunicationOK,
				ConfiguredModuleType: rawDevice.ConfiguredModuleType, DetectedModuleType: rawDevice.DetectedModuleType,
				TypeMismatch: rawDevice.TypeMismatch, CommunicationModuleVersion: rawDevice.CommunicationModuleVersion,
				CommunicationModuleVersionRaw: rawDevice.CommunicationModuleVersionRaw,
				PLCVersion:                    rawDevice.PLCVersion, PLCVersionRaw: rawDevice.PLCVersionRaw,
				VersionPayload: rawDevice.VersionPayload, VersionState: rawDevice.VersionState,
				LastVersionAttemptAt: rawDevice.LastVersionAttemptAt, LastVersionSuccessAt: rawDevice.LastVersionSuccessAt,
				LastVersionError: rawDevice.LastVersionError, ConsecutiveFailures: rawDevice.ConsecutiveFailures,
				Stale: rawDevice.Stale,
			})
		}
		snapshot.Ports = append(snapshot.Ports, port)
	}
	return snapshot, true, nil
}
