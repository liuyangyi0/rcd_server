package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestStationVersionCacheRoundTripAndCascade(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	if _, err := r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source",
		Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	moduleRaw, plcRaw := uint16(0x0104), uint16(5)
	zeroRaw := uint16(0)
	snapshot := domain.SourceStationVersions{
		SourceID: "s", TopologyFingerprint: "abc", Revision: "4", StationRevision: "7",
		SoftwareVersion: "1.2.3", BuildCommit: "deadbeef", ObservedAt: now,
		Ports: []domain.StationVersionPort{{PortID: "/dev/ttyWCH0", PortName: "ttyWCH0", Enabled: true,
			AppliedEnabled: true, Open: true, PendingCommands: 1, Devices: []domain.StationVersion{{
				DeviceID: "12", DeviceName: "station-12", ReadEnabled: true, AppliedReadEnabled: true,
				CommunicationOK: true, ConfiguredModuleType: domain.ModuleTypeDomestic,
				DetectedModuleType: domain.ModuleTypeDomestic, CommunicationModuleVersion: "V1.4",
				CommunicationModuleVersionRaw: &moduleRaw, PLCVersion: "V0.5", PLCVersionRaw: &plcRaw,
				VersionPayload: "01040005", VersionState: "ready", LastVersionAttemptAt: now,
				LastVersionSuccessAt: now,
			}, {
				DeviceID: "13", DeviceName: "station-13", ConfiguredModuleType: domain.ModuleTypeUnknown,
				DetectedModuleType: domain.ModuleTypeUnknown, VersionState: "never_queried",
			}, {
				DeviceID: "14", DeviceName: "station-14", ConfiguredModuleType: domain.ModuleTypeDomestic,
				DetectedModuleType: domain.ModuleTypeDomestic, CommunicationModuleVersion: "V0.0",
				CommunicationModuleVersionRaw: &zeroRaw, PLCVersion: "V0.0", PLCVersionRaw: &zeroRaw,
				VersionPayload: "00000000", VersionState: "ready", LastVersionSuccessAt: now,
			}}}},
	}
	if err := r.SaveSourceStationVersions(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.LoadSourceStationVersions(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	_, device, ok := loaded.Device("/dev/ttyWCH0", "12")
	if !ok || !loaded.Cached || loaded.StationRevision != "7" || device.VersionPayload != "01040005" ||
		device.CommunicationModuleVersionRaw == nil || *device.CommunicationModuleVersionRaw != 0x0104 {
		t.Fatalf("loaded cache = %#v / %#v", loaded, device)
	}
	_, unknown, ok := loaded.Device("/dev/ttyWCH0", "13")
	if !ok || unknown.CommunicationModuleVersionRaw != nil || unknown.PLCVersionRaw != nil {
		t.Fatalf("nullable raw versions changed during round trip: %#v", unknown)
	}
	_, zero, ok := loaded.Device("/dev/ttyWCH0", "14")
	if !ok || zero.CommunicationModuleVersionRaw == nil || *zero.CommunicationModuleVersionRaw != 0 ||
		zero.PLCVersionRaw == nil || *zero.PLCVersionRaw != 0 {
		t.Fatalf("valid V0.0 raw versions were lost: %#v", zero)
	}
	if err := r.ClearSourceStationVersions(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadSourceStationVersions(ctx, "s"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("load after clear error = %v", err)
	}
}
