package app

import (
	"context"
	"testing"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

func TestDeletingConflictingSourceImmediatelyRestoresPeerData(t *testing.T) {
	ctx := context.Background()
	runtime := seededRuntime(t)
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnLowerStatus(connectedStatus("source-b", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{
		Type: domain.DataTypeDouble, Value: float64(42), StatusCode: domain.StatusGood, Epoch: 1,
	})
	mismatch := testNode("KKS001")
	mismatch.DataType = domain.DataTypeUInt32
	runtime.OnCatalog(domain.SourceCatalog{
		SourceID: "source-b", GroupID: "line", State: domain.CatalogReady,
		Nodes: []domain.CatalogNode{mismatch, testNode("KKS002"), testStringNode("COM1_Send"), testMethodNode("COM1_SendCommand")},
	})
	if source, err := runtime.source("source-a"); err != nil || source.CatalogState != domain.CatalogConflict {
		t.Fatalf("type conflict precondition: state=%q err=%v", source.CatalogState, err)
	}
	runtime.retryDirtyStatuses(ctx)
	if err := runtime.DeleteSource(ctx, "source-b", 1); err != nil {
		t.Fatalf("delete non-default conflicting source: %v", err)
	}
	assertPeerRecovered(t, runtime, "after-source-delete")
}

func TestDeletingConflictingGroupImmediatelyRestoresPeerData(t *testing.T) {
	ctx := context.Background()
	runtime := seededRuntime(t)
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{
		Type: domain.DataTypeDouble, Value: float64(42), StatusCode: domain.StatusGood, Epoch: 1,
	})
	if _, err := runtime.CreateGroup(ctx, api.GroupMutation{ID: "other", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.CreateSource(ctx, api.SourceMutation{ID: "source-c", GroupID: "other", Name: "Other source", Endpoint: "opc.tcp://other:4840", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-c", GroupID: "other", Nodes: []domain.CatalogNode{testNode("KKS001")}})
	if source, err := runtime.source("source-a"); err != nil || source.CatalogState != domain.CatalogConflict {
		t.Fatalf("cross-group conflict precondition: state=%q err=%v", source.CatalogState, err)
	}
	runtime.retryDirtyStatuses(ctx)
	if err := runtime.DeleteGroup(ctx, "other", 1); err != nil {
		t.Fatalf("delete conflicting group without Sessions: %v", err)
	}
	assertPeerRecovered(t, runtime, "after-group-delete")
}

func assertPeerRecovered(t *testing.T, runtime *Runtime, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if source, err := runtime.source("source-a"); err != nil || !source.CatalogState.Selectable() {
		t.Fatalf("remaining source must immediately recover: state=%q err=%v", source.CatalogState, err)
	}
	runtime.mu.RLock()
	status := runtime.statuses["source-a"]
	runtime.mu.RUnlock()
	if status.Catalog == domain.CatalogConflict || status.LastError != "" {
		t.Errorf("remaining source status still reports a removed conflict: catalog=%q last_error=%q", status.Catalog, status.LastError)
	}
	if err := runtime.openSession(sessionID); err != nil {
		t.Fatal(err)
	}
	route, err := runtime.routes.Route(sessionID, "line")
	if err != nil || route.SourceID != "source-a" {
		t.Fatalf("new Session default: route=%+v err=%v; want source-a", route, err)
	}
	if sample := runtime.ReadSample(sessionID, testKey("KKS001")); sample.StatusCode != domain.StatusGood || sample.Value != float64(42) {
		t.Errorf("peer data after conflict removal: sample=%+v; want Good value 42 without another lower heartbeat", sample)
	}
	runtime.retryDirtyStatuses(ctx)
	alarms, err := runtime.ListAlarms(ctx, api.PageRequest{SourceID: "source-a", Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil || alarms.Total != 0 {
		t.Errorf("recovered source still has open alarms: page=%+v err=%v", alarms, err)
	}
	runtime.persistDirtyCatalogs(ctx)
	catalog, err := runtime.repo.LoadSourceCatalog(ctx, "source-a")
	if err != nil || !catalog.State.Selectable() || catalog.QuarantineReason != "" {
		t.Fatalf("recovered peer cache was not persisted: state=%q reason=%q err=%v", catalog.State, catalog.QuarantineReason, err)
	}
	restarted := reopenRuntimeFromCache(t, runtime)
	if source, err := restarted.source("source-a"); err != nil || !source.CatalogState.Selectable() {
		t.Fatalf("derived conflict returned after restart: state=%q err=%v", source.CatalogState, err)
	}
}

func TestDeletingSourceRefreshesOnlyAffectedLiveSessionGroups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := seededRuntime(t)
	if _, err := runtime.CreateGroup(ctx, api.GroupMutation{ID: "unaffected", Name: "Unaffected"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	before, _ := runtime.routes.Route("session", "line")
	// Exercise the real enqueue path without starting lower network workers.
	runtime.mu.Lock()
	runtime.runContext = ctx
	runtime.refreshSession = func(string, string) {}
	runtime.mu.Unlock()
	runtime.running.Store(true)
	defer runtime.running.Store(false)
	if err := runtime.DeleteSource(ctx, "source-b", 1); err != nil {
		t.Fatal(err)
	}
	request, ok := runtime.refreshes.take()
	if !ok {
		t.Fatal("hot source deletion did not refresh the affected existing Session group")
	}
	if request.sessionID != "session" || request.groupID != "line" {
		t.Fatalf("wrong refresh target: %+v", request)
	}
	if runtime.refreshes.depth() != 0 {
		t.Fatal("unaffected or duplicate Session groups were refreshed")
	}
	after, err := runtime.routes.Route("session", "line")
	if err != nil || after != before {
		t.Fatalf("deleting an unselected peer changed selection: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestCorrectedTypeConflictRemainsRecoverableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	runtime := seededRuntime(t)
	valid, err := runtime.repo.LoadSourceCatalog(ctx, "source-b")
	if err != nil {
		t.Fatal(err)
	}
	mismatch := testNode("KKS001")
	mismatch.DataType = domain.DataTypeUInt32
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-b", GroupID: "line", Nodes: []domain.CatalogNode{mismatch}})
	conflicting := reopenRuntimeFromCache(t, runtime)
	for _, sourceID := range []string{"source-a", "source-b"} {
		source, _ := conflicting.source(sourceID)
		catalog, err := conflicting.repo.LoadSourceCatalog(ctx, sourceID)
		if source.CatalogState != domain.CatalogConflict || err != nil || catalog.QuarantineReason != "" {
			t.Fatalf("ordinary conflict became explicit quarantine: source=%s state=%q reason=%q err=%v", sourceID, source.CatalogState, catalog.QuarantineReason, err)
		}
	}
	conflicting.OnCatalog(valid)
	recovered := reopenRuntimeFromCache(t, conflicting)
	for _, source := range recovered.routes.Sources() {
		if !source.CatalogState.Selectable() {
			t.Fatalf("corrected peer remains locked after restart: source=%s state=%q", source.ID, source.CatalogState)
		}
	}
}
