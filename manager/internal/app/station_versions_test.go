package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func stationStatusResult(t *testing.T, revision uint64, open bool) json.RawMessage {
	t.Helper()
	result := map[string]any{
		"schema_version": 1, "revision": fmt.Sprint(revision), "station_revision": fmt.Sprint(revision),
		"software_version": "test", "build_commit": "test", "ports": []any{map[string]any{
			"port_id": "COM1", "port_name": "COM1", "enabled": true, "applied_enabled": true,
			"open": open, "control_pending": false, "pending_commands": 0, "devices": []any{map[string]any{
				"device_id": "1", "name": "station-1", "read_enabled": true, "applied_read_enabled": true,
				"communication_ok": open, "configured_module_type": "domestic", "detected_module_type": "unknown",
				"type_mismatch": false, "communication_module_version": "", "communication_module_version_raw": nil,
				"plc_version": "", "plc_version_raw": nil, "version_payload": "", "version_state": "never_queried",
				"last_version_attempt_at": nil, "last_version_success_at": nil, "last_version_error": "",
				"consecutive_failures": 0, "stale": false,
			}},
		}},
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestStationVersionCacheRejectsOutOfOrderWithinEpochAndAcceptsResetAfterReconnect(t *testing.T) {
	const sourceID = "source-a"
	runtime := &Runtime{
		broker:                  api.NewBroker(),
		catalogs:                map[string]domain.SourceCatalog{sourceID: {SourceID: sourceID}},
		statuses:                map[string]opc.LowerStatus{sourceID: {SourceID: sourceID, Epoch: 9}},
		stationVersions:         make(map[string]domain.SourceStationVersions),
		stationVersionEpoch:     make(map[string]uint64),
		stationRefreshDirty:     make(map[string]bool),
		stationFeatureEpoch:     make(map[string]uint64),
		stationFeatureSupported: make(map[string]bool),
	}
	source := domain.Source{ID: sourceID}

	// Calls can complete in any order. The serialized cache commit must retain
	// the greatest station revision observed in this connection epoch.
	var workers sync.WaitGroup
	for revision := uint64(1); revision <= 32; revision++ {
		workers.Add(1)
		go func(revision uint64) {
			defer workers.Done()
			if err := runtime.cacheStationVersionResult(domain.OperationRequest{Action: "status"},
				stationStatusResult(t, revision, true), source, 9, unverifiedStationTopology); err != nil {
				t.Errorf("cache revision %d: %v", revision, err)
			}
		}(revision)
	}
	workers.Wait()
	if snapshot, ok := runtime.StationVersions(sourceID); !ok || snapshot.StationRevision != "32" {
		t.Fatalf("same-epoch snapshot = %#v, present=%v", snapshot, ok)
	}

	// Equal station revisions may carry changed runtime port/device state and
	// must replace the content, even though identical content is de-duplicated.
	if err := runtime.cacheStationVersionResult(domain.OperationRequest{Action: "status"},
		stationStatusResult(t, 32, false), source, 9, unverifiedStationTopology); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := runtime.StationVersions(sourceID)
	if len(snapshot.Ports) != 1 || snapshot.Ports[0].Open {
		t.Fatalf("equal-revision runtime state was not refreshed: %#v", snapshot)
	}

	// A new downstream Session may legitimately start at a lower revision.
	runtime.mu.Lock()
	runtime.statuses[sourceID] = opc.LowerStatus{SourceID: sourceID, Epoch: 10}
	runtime.mu.Unlock()
	if err := runtime.cacheStationVersionResult(domain.OperationRequest{Action: "status"},
		stationStatusResult(t, 1, true), source, 10, unverifiedStationTopology); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = runtime.StationVersions(sourceID)
	if snapshot.StationRevision != "1" || runtime.stationVersionEpoch[sourceID] != 10 {
		t.Fatalf("new-epoch snapshot = %#v epoch=%d", snapshot, runtime.stationVersionEpoch[sourceID])
	}

	// A successful old-Session Call arriving after reconnect is discarded.
	if err := runtime.cacheStationVersionResult(domain.OperationRequest{Action: "status"},
		stationStatusResult(t, 99, true), source, 9, unverifiedStationTopology); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = runtime.StationVersions(sourceID)
	if snapshot.StationRevision != "1" {
		t.Fatalf("late old-epoch response replaced current snapshot: %#v", snapshot)
	}
}

func TestStationVersionRefreshKeepsInvalidationArrivingInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &Runtime{
		runContext: ctx, stationRefreshQueue: make(chan string, 1),
		stationRefreshPending: make(map[string]bool), stationRefreshDirty: make(map[string]bool),
	}
	runtime.running.Store(true)
	runtime.queueStationVersionRefresh("source-a")
	if sourceID := <-runtime.stationRefreshQueue; sourceID != "source-a" {
		t.Fatalf("queued source = %q", sourceID)
	}
	runtime.queueStationVersionRefresh("source-a")
	if !runtime.finishStationVersionRefresh("source-a") || !runtime.stationRefreshPending["source-a"] {
		t.Fatal("in-flight invalidation was not retained for a second refresh")
	}
	if runtime.finishStationVersionRefresh("source-a") || runtime.stationRefreshPending["source-a"] {
		t.Fatal("pending marker was not released after the clean refresh")
	}
}

func TestDeviceVersionSummarySeparatesMismatchFromFailureAndDeduplicatesBuilds(t *testing.T) {
	items := []api.DeviceVersionItem{
		{SourceID: "a", ConfiguredModuleType: domain.ModuleTypeDomestic, TypeMismatch: true,
			VersionState: "type_mismatch", LastVersionError: "diagnostic", RCDSoftwareVersion: "1.2.3", RCDBuildCommit: "abc"},
		{SourceID: "a", ConfiguredModuleType: domain.ModuleTypeDomestic,
			VersionState: "success", RCDSoftwareVersion: "1.2.3", RCDBuildCommit: "abc"},
		{SourceID: "b", ConfiguredModuleType: domain.ModuleTypeUnknown,
			VersionState: "failed", LastVersionError: "timeout", RCDSoftwareVersion: "1.2.3", RCDBuildCommit: "abc"},
	}
	summary := summarizeDeviceVersions(items)
	if summary.Mismatch != 1 || summary.Failed != 1 || len(summary.LowerSoftwareVersions) != 1 ||
		summary.LowerSoftwareVersions[0].SourceCount != 2 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestLateStationVersionPersistenceRestoresPreviousOrClears(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(ctx, ":memory:", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateGroup(ctx, domain.Group{ID: "g", Name: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSource(ctx, domain.Source{ID: "source-a", GroupID: "g", Name: "a",
		Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	previous := domain.SourceStationVersions{SourceID: "source-a", TopologyFingerprint: "fingerprint", Revision: "2", StationRevision: "2"}
	late := domain.SourceStationVersions{SourceID: "source-a", TopologyFingerprint: "fingerprint", Revision: "9", StationRevision: "9"}
	if err := repository.SaveSourceStationVersions(ctx, late); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{repo: repository}
	if err := runtime.restoreStationVersionPersistence("source-a", previous, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.LoadSourceStationVersions(ctx, "source-a")
	if err != nil || loaded.StationRevision != "2" {
		t.Fatalf("restored snapshot = %#v, err=%v", loaded, err)
	}
	if err := repository.SaveSourceStationVersions(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := runtime.restoreStationVersionPersistence("source-a", domain.SourceStationVersions{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.LoadSourceStationVersions(ctx, "source-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("late snapshot was not cleared: %v", err)
	}
}
