package opc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	awu "github.com/awcullen/opcua/ua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

func TestDecodeStationVersionStatusAcceptsNullableRawVersions(t *testing.T) {
	result := json.RawMessage(`{
		"schema_version":1,"revision":"4","station_revision":"7","software_version":"1.2.3","build_commit":"abc",
		"ports":[{"port_id":"/dev/ttyWCH0","port_name":"ttyWCH0","enabled":true,"applied_enabled":true,
		"open":true,"control_pending":false,"pending_commands":0,"devices":[{
		"device_id":"12","name":"station-12","read_enabled":true,"applied_read_enabled":true,"communication_ok":true,
		"configured_module_type":"domestic","detected_module_type":"unknown","type_mismatch":false,
		"communication_module_version":"","communication_module_version_raw":null,"plc_version":"","plc_version_raw":null,
		"version_payload":"","version_state":"never_queried","last_version_attempt_at":null,"last_version_success_at":null,
		"last_version_error":"","consecutive_failures":0,"stale":false}]}]}`)
	snapshot, present, err := DecodeStationVersionStatus(domain.OperationRequest{Action: "status"}, result, "source-a", "fingerprint", time.Now())
	if err != nil || !present {
		t.Fatalf("decode present=%v err=%v", present, err)
	}
	_, device, ok := snapshot.Device("/dev/ttyWCH0", "12")
	if !ok || device.ConfiguredModuleType != domain.ModuleTypeDomestic || device.CommunicationModuleVersionRaw != nil {
		t.Fatalf("device = %#v", device)
	}
}

type stationRevisionProbeReader struct {
	responses []*gu.ReadResponse
	index     int
}

func (r *stationRevisionProbeReader) Read(context.Context, *gu.ReadRequest) (*gu.ReadResponse, error) {
	response := r.responses[r.index]
	r.index++
	return response, nil
}

func TestStationRevisionProbeToleratesMissingNodeFromOlderLower(t *testing.T) {
	capabilities := `{"schema_version":1,"features":["acquisition","station_versions"]}`
	reader := &stationRevisionProbeReader{responses: []*gu.ReadResponse{
		{Results: []*gu.DataValue{{Status: gu.StatusOK, Value: gu.MustVariant(capabilities)}}},
		{Results: []*gu.DataValue{{Status: gu.StatusBadNodeIDUnknown}}},
	}}
	supervisor := &LowerSupervisor{cfg: LowerConfig{OperationTimeout: time.Second}}
	if nodeID := supervisor.discoverStationVersionRevisionNode(context.Background(), reader,
		map[string]uint16{domain.OperationsNamespaceURI: 4}); nodeID != "" {
		t.Fatalf("missing optional node discovered as %q", nodeID)
	}
}

type stationMetadataBackend struct {
	*fakeBackend
	snapshot     domain.SourceStationVersions
	status       LowerStatus
	supportKnown bool
	supported    bool
}

func (b *stationMetadataBackend) StationVersionSupport(string) (bool, bool) {
	return b.supportKnown, b.supported
}

func (b *stationMetadataBackend) StationVersions(string) (domain.SourceStationVersions, bool) {
	return b.snapshot.Clone(), true
}

func (b *stationMetadataBackend) LowerStatus(string) (LowerStatus, bool) {
	return b.status, true
}

func stationDefinition(t *testing.T, definitions []managementVariable, name string) managementVariable {
	t.Helper()
	for _, definition := range definitions {
		if definition.name == name {
			return definition
		}
	}
	t.Fatalf("management variable %s not found", name)
	return managementVariable{}
}

func TestStationDeviceVariablesExposePortAndTypedNullableRawVersions(t *testing.T) {
	backend := &stationMetadataBackend{
		fakeBackend: &fakeBackend{},
		status:      LowerStatus{Connection: ConnectionConnected, Session: SessionActive},
		snapshot: domain.SourceStationVersions{SourceID: "source-a", Ports: []domain.StationVersionPort{{
			PortID: "COM1", Devices: []domain.StationVersion{{DeviceID: "12", CommunicationOK: true, ConfiguredModuleType: domain.ModuleTypeDomestic,
				DetectedModuleType: domain.ModuleTypeUnknown, VersionState: "never_queried"}},
		}}},
	}
	definitions := sourceStationDeviceVariables(backend, "source-a", "COM1", domain.TopologyDevice{DeviceID: "12", Name: "station-12"})
	if value, status := stationDefinition(t, definitions, "PortId").read(""); value != "COM1" || status != awu.Good {
		t.Fatalf("PortId = %#v/%#x", value, status)
	}
	raw := stationDefinition(t, definitions, "CommunicationModuleVersionRaw")
	if value, status := raw.read(""); value != uint16(0) || status != awu.BadWaitingForInitialData {
		t.Fatalf("nullable raw = %#v (%T)/%#x", value, value, status)
	}

	zero := uint16(0)
	backend.snapshot.Ports[0].Devices[0].CommunicationModuleVersionRaw = &zero
	backend.snapshot.Ports[0].Devices[0].PLCVersionRaw = &zero
	backend.snapshot.Ports[0].Devices[0].VersionPayload = "00000000"
	backend.snapshot.Ports[0].Devices[0].VersionState = "ready"
	if value, status := raw.read(""); value != uint16(0) || status != awu.Good {
		t.Fatalf("valid V0.0 raw = %#v (%T)/%#x", value, value, status)
	}
	backend.supportKnown, backend.supported = true, false
	if value, status := raw.read(""); value != uint16(0) || status != awu.BadNotSupported {
		t.Fatalf("cached value from unsupported current lower = %#v (%T)/%#x", value, value, status)
	}
}

func TestStationVersionQualityRules(t *testing.T) {
	value := uint16(0x0104)
	online := &stationMetadataBackend{fakeBackend: &fakeBackend{},
		status: LowerStatus{Connection: ConnectionConnected, Session: SessionActive}}
	tests := []struct {
		name   string
		device domain.StationVersion
		want   awu.StatusCode
	}{
		{name: "not queried", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, CommunicationOK: true, VersionState: "never_queried"}, want: awu.BadWaitingForInitialData},
		{name: "failed without history", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, VersionState: "failed", ConsecutiveFailures: 1}, want: awu.BadNoCommunication},
		{name: "configured Russian", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeLegacyRussian}, want: awu.BadNotSupported},
		{name: "detected mismatch", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, DetectedModuleType: domain.ModuleTypeLegacyRussian, TypeMismatch: true}, want: awu.BadConfigurationError},
		{name: "good", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, CommunicationOK: true, CommunicationModuleVersionRaw: &value, VersionState: "ready"}, want: awu.Good},
		{name: "station communication failed with history", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, CommunicationModuleVersionRaw: &value, VersionState: "success"}, want: awu.UncertainLastUsableValue},
		{name: "stale history", device: domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, CommunicationOK: true, CommunicationModuleVersionRaw: &value, VersionState: "failed", Stale: true}, want: awu.UncertainLastUsableValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stationVersionStatus(online, "source-a", test.device); got != test.want {
				t.Fatalf("status = %#x, want %#x", got, test.want)
			}
		})
	}
	online.status = LowerStatus{Connection: ConnectionDisconnected, Session: SessionInactive}
	if got := stationVersionStatus(online, "source-a", domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic, CommunicationModuleVersionRaw: &value}); got != awu.UncertainLastUsableValue {
		t.Fatalf("offline history status = %#x", got)
	}
	if got := stationVersionStatus(online, "source-a", domain.StationVersion{ConfiguredModuleType: domain.ModuleTypeDomestic}); got != awu.BadNoCommunication {
		t.Fatalf("offline empty status = %#x", got)
	}
}
