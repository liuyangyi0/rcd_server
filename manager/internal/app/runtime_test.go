package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func TestRuntimeSessionIsolationAndDataValueRules(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session-a"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.openSession("session-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.selectSource("session-b", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnLowerStatus(connectedStatus("source-b", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 11.5, StatusCode: domain.StatusGood, SourceTimestamp: now, Epoch: 1})
	runtime.OnSample("source-b", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 22.5, StatusCode: domain.StatusGood, SourceTimestamp: now, Epoch: 1})

	if got := runtime.ReadSample("session-a", testKey("KKS001")); got.Value != 11.5 || got.SourceTimestamp != now {
		t.Fatalf("session-a sample = %#v", got)
	}
	if got := runtime.ReadSample("session-b", testKey("KKS001")); got.Value != 22.5 {
		t.Fatalf("session-b sample = %#v", got)
	}
	// The union contains KKS002, but source-a does not. This is a directory
	// mismatch, not a communications failure.
	if got := runtime.ReadSample("session-a", testKey("KKS002")); got.StatusCode != domain.StatusBadConfigurationError {
		t.Fatalf("missing union member status = %#x", got.StatusCode)
	}

	runtime.OnLowerStatus(opc.LowerStatus{SourceID: "source-b", Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogReady, Epoch: 1})
	got := runtime.ReadSample("session-b", testKey("KKS001"))
	if got.Value != 22.5 || got.StatusCode != domain.StatusBadNoCommunication || got.SourceTimestamp != now {
		t.Fatalf("offline last value = %#v", got)
	}
}

func TestListPointsCanInspectDefaultSessionAndExplicitSourceRoutes(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.selectSource("session-b", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnLowerStatus(connectedStatus("source-b", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 10.0, StatusCode: domain.StatusGood, SourceTimestamp: now, Epoch: 1})
	runtime.OnSample("source-b", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 20.0, StatusCode: domain.StatusBadNoCommunication, SourceTimestamp: now, Epoch: 1})

	assertPoint := func(name string, request api.PageRequest, source string, value float64, status domain.StatusCode) {
		t.Helper()
		page, err := runtime.ListPoints(context.Background(), request)
		if err != nil || page.Total != 1 || len(page.Items) != 1 {
			t.Fatalf("%s page=%#v err=%v", name, page, err)
		}
		point := page.Items[0]
		if point.SelectedSourceID != source || point.Sample == nil || point.Sample.Value != value || point.Sample.StatusCode != status {
			t.Fatalf("%s point=%#v", name, point)
		}
	}
	base := api.PageRequest{GroupID: "line", Query: "KKS001", Limit: 10}
	assertPoint("default", base, "source-a", 10.0, domain.StatusGood)
	sessionRequest := base
	sessionRequest.SessionID = "session-b"
	assertPoint("session", sessionRequest, "source-b", 20.0, domain.StatusBadNoCommunication)
	routeRequest := base
	routeRequest.RouteSourceID = "source-b"
	assertPoint("explicit route", routeRequest, "source-b", 20.0, domain.StatusBadNoCommunication)
	routeRequest.Status = "Bad"
	assertPoint("quality filter", routeRequest, "source-b", 20.0, domain.StatusBadNoCommunication)
	routeRequest.Status = "Good"
	page, err := runtime.ListPoints(context.Background(), routeRequest)
	if err != nil || page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("good filter page=%#v err=%v", page, err)
	}
}

func TestRuntimePersistsConnectionAndCatalogAlarmTransitions(t *testing.T) {
	runtime := seededRuntime(t)
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	disconnected := opc.LowerStatus{SourceID: "source-a", Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogReady,
		Epoch: 1, LastError: "connection reset", UpdatedAt: time.Now().UTC()}
	runtime.OnLowerStatus(disconnected)
	runtime.retryDirtyStatuses(context.Background())

	page, err := runtime.ListAlarms(context.Background(), api.PageRequest{SourceID: "source-a", Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Kind != "source.connection" {
		t.Fatalf("connection alarms=%#v err=%v", page, err)
	}
	events, err := runtime.ListSourceEvents(context.Background(), api.PageRequest{SourceID: "source-a", Kind: "connection", Limit: 20})
	if err != nil || events.Total < 2 {
		t.Fatalf("connection events=%#v err=%v", events, err)
	}
	runtime.OnLowerStatus(opc.LowerStatus{SourceID: "source-a", Connection: opc.ConnectionConnecting,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogReady, Epoch: 2})
	runtime.retryDirtyStatuses(context.Background())
	page, err = runtime.ListAlarms(context.Background(), api.PageRequest{SourceID: "source-a", Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil || page.Total != 1 {
		t.Fatalf("connecting unexpectedly resolved alarm: %#v err=%v", page, err)
	}
	runtime.OnLowerStatus(connectedStatus("source-a", 2))
	runtime.retryDirtyStatuses(context.Background())
	page, err = runtime.ListAlarms(context.Background(), api.PageRequest{SourceID: "source-a", Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil || page.Total != 0 {
		t.Fatalf("resolved connection alarms=%#v err=%v", page, err)
	}

	conflicting := testNode("KKS001")
	conflicting.DataType = domain.DataTypeUInt32
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-b", GroupID: "line", DiscoveredAt: time.Now().UTC(), Nodes: []domain.CatalogNode{conflicting}})
	runtime.retryDirtyStatuses(context.Background())
	page, err = runtime.ListAlarms(context.Background(), api.PageRequest{SourceID: "source-b", Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	foundCatalog := false
	for _, alarm := range page.Items {
		foundCatalog = foundCatalog || alarm.Kind == "source.catalog"
	}
	if !foundCatalog {
		t.Fatalf("catalog alarm was not persisted: %#v", page.Items)
	}
}

func TestRuntimeRetriesQueuedStatusEventsAndAlarms(t *testing.T) {
	runtime := seededRuntime(t)
	now := time.Now().UTC()
	disconnected := opc.LowerStatus{SourceID: "source-a", Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogReady,
		LastError: "temporary outage", UpdatedAt: now}
	runtime.statusPersistMu.Lock()
	runtime.dirtySourceEvents = append(runtime.dirtySourceEvents, domain.SourceEvent{SourceID: "source-a", Kind: "connection",
		Previous: string(opc.ConnectionConnected), Current: string(opc.ConnectionDisconnected), Message: disconnected.LastError, OccurredAt: now})
	runtime.dirtyAlarmStatuses["source-a"] = disconnected
	runtime.statusPersistMu.Unlock()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	runtime.retryDirtyStatuses(canceled)
	runtime.statusPersistMu.Lock()
	queuedEvents, queuedAlarms := len(runtime.dirtySourceEvents), len(runtime.dirtyAlarmStatuses)
	runtime.statusPersistMu.Unlock()
	if queuedEvents != 1 || queuedAlarms != 1 {
		t.Fatalf("canceled retry lost durable work: events=%d alarms=%d", queuedEvents, queuedAlarms)
	}
	if health := runtime.Health(context.Background()); health.OK || health.Checks["status_history_persistence"] == "ok" {
		t.Fatalf("health did not expose queued status persistence failure: %#v", health)
	}

	runtime.retryDirtyStatuses(context.Background())
	runtime.statusPersistMu.Lock()
	queuedEvents, queuedAlarms = len(runtime.dirtySourceEvents), len(runtime.dirtyAlarmStatuses)
	runtime.statusPersistMu.Unlock()
	if queuedEvents != 0 || queuedAlarms != 0 {
		t.Fatalf("successful retry retained durable work: events=%d alarms=%d", queuedEvents, queuedAlarms)
	}
	events, err := runtime.ListSourceEvents(context.Background(), api.PageRequest{SourceID: "source-a", Kind: "connection", Limit: 10})
	if err != nil || events.Total != 1 {
		t.Fatalf("retried events=%#v err=%v", events, err)
	}
	alarms, err := runtime.ListAlarms(context.Background(), api.PageRequest{SourceID: "source-a", Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil || alarms.Total != 1 || alarms.Items[0].Kind != "source.connection" {
		t.Fatalf("retried alarms=%#v err=%v", alarms, err)
	}
}

func TestRuntimeMetadataOnlySourceUpdateDoesNotRestartLower(t *testing.T) {
	runtime := seededRuntime(t)
	starts := make(map[string]int)
	var startsMu sync.Mutex
	runtime.options.LowerFactory = func(cfg opc.LowerConfig, _ opc.LowerSink) (opc.LowerClient, error) {
		startsMu.Lock()
		starts[cfg.Source.ID]++
		startsMu.Unlock()
		return &contextLowerClient{}, nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	startsMu.Lock()
	before := starts["source-b"]
	startsMu.Unlock()
	updated, err := runtime.UpdateSource(context.Background(), "source-b", 1, api.SourceMutation{
		ID: "source-b", GroupID: "line", Name: "备用来源（新名称）", Endpoint: "opc.tcp://127.0.0.1:4841",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	startsMu.Lock()
	after := starts["source-b"]
	startsMu.Unlock()
	if before != 1 || after != before {
		t.Fatalf("metadata update restarted lower: before=%d after=%d", before, after)
	}
	if updated.Name != "备用来源（新名称）" {
		t.Fatalf("updated source name=%q", updated.Name)
	}
}

func TestConfigurationImportPreflightsAndOnlyAppliesToEmptyRuntime(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(ctx, "file:"+t.Name()+"?mode=memory&cache=shared", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	runtime, err := NewRuntime(ctx, repository, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	document := api.ConfigurationDocument{SchemaVersion: 1,
		Groups:  []api.GroupMutation{{ID: "line", Name: "Line", DefaultSourceID: "source-a"}},
		Sources: []api.ExportSource{{ID: "source-a", GroupID: "line", Name: "Source A", Endpoint: "opc.tcp://127.0.0.1:4840", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}},
	}
	preflight, err := runtime.ImportConfiguration(ctx, api.ConfigurationImportRequest{Document: document, DryRun: true})
	if err != nil || !preflight.DryRun || preflight.CreatedGroups != 1 || preflight.CreatedSources != 1 {
		t.Fatalf("preflight=%#v err=%v", preflight, err)
	}
	applied, err := runtime.ImportConfiguration(ctx, api.ConfigurationImportRequest{Document: document})
	if err != nil || applied.CreatedGroups != 1 || applied.CreatedSources != 1 {
		t.Fatalf("applied=%#v err=%v", applied, err)
	}
	groups, _ := runtime.ListGroups(ctx)
	sources, _ := runtime.ListSources(ctx)
	if len(groups) != 1 || groups[0].DefaultSourceID != "" || len(sources) != 1 || sources[0].ID != "source-a" || sources[0].HasPassword || len(applied.Warnings) == 0 {
		t.Fatalf("groups=%#v sources=%#v", groups, sources)
	}
	if session, err := runtime.routes.OpenSession("imported-session"); err != nil || session.Routes["line"].SourceID != "" {
		t.Fatalf("new Session selected a Discovering imported source: session=%#v err=%v", session, err)
	}
	secondPreflight, err := runtime.ImportConfiguration(ctx, api.ConfigurationImportRequest{Document: document, DryRun: true})
	if err != nil || secondPreflight.UpdatedGroups != 1 || secondPreflight.UpdatedSources != 1 || len(secondPreflight.Warnings) == 0 {
		t.Fatalf("second preflight=%#v err=%v", secondPreflight, err)
	}
	if _, err := runtime.ImportConfiguration(ctx, api.ConfigurationImportRequest{Document: document}); !errors.Is(err, domain.ErrSourceInUse) {
		t.Fatalf("second import error=%v", err)
	}
}

func TestRuntimeHotRemovedNodeIsOutOfServiceUntilRestart(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.selectSource("session", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	runtime.OnCatalog(domain.SourceCatalog{
		SourceID: "source-b", GroupID: "line", State: domain.CatalogReady, DiscoveredAt: time.Now().UTC(),
		Nodes: []domain.CatalogNode{testNode("KKS001")},
	})
	got := runtime.ReadSample("session", testKey("KKS002"))
	if got.StatusCode != domain.StatusBadOutOfService || got.Type != domain.DataTypeDouble {
		t.Fatalf("hot removed sample = %#v", got)
	}
}

func TestRuntimeWritePinsRouteAndLocksGroupUntilAck(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 1)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 1)

	status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB")
	if err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%#x err=%v", status, err)
	}
	if client.lastWrite != "AABB" {
		t.Fatalf("lower write = %#v", client.lastWrite)
	}
	if _, err := runtime.selectSource("session", "line", "source-b", 1, "test"); !errors.Is(err, domain.ErrGroupPending) {
		t.Fatalf("selection during pending command error = %v", err)
	}
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "2", StatusCode: domain.StatusGood, Epoch: 1})
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if _, err := runtime.selectSource("session", "line", "source-b", 1, "test"); err != nil {
		t.Fatalf("selection after ACK: %v", err)
	}
	commands, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 1 || commands[0].State != domain.CommandReceived || commands[0].SourceID != "source-a" {
		t.Fatalf("command audit total=%d commands=%#v err=%v", total, commands, err)
	}
}

func TestRuntimeReconnectWaitsForCurrentEpochFirstValue(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{
		Type: domain.DataTypeDouble, Value: 42.0, StatusCode: domain.StatusGood, Epoch: 1,
	})
	runtime.OnLowerStatus(connectedStatus("source-a", 2))
	got := runtime.ReadSample("session", testKey("KKS001"))
	if got.StatusCode != domain.StatusBadWaitingInitialData || got.Value != float64(0) || got.Epoch != 2 {
		t.Fatalf("sample after reconnect = %#v", got)
	}
}

func TestReplacementLowerSupervisorPreservesProcessReconnectCount(t *testing.T) {
	runtime := seededRuntime(t)
	lastDataAt := time.Now().UTC().Add(-time.Minute)
	lastProbeAt := time.Now().UTC().Add(-time.Second)
	runtime.mu.Lock()
	status := runtime.statuses["source-a"]
	status.ReconnectCount = 5
	status.LastDataAt = lastDataAt
	status.LastProbeAt = lastProbeAt
	runtime.statuses["source-a"] = status
	runtime.mu.Unlock()
	proxy := lowerSinkProxy{runtime: runtime, sourceID: "source-a", reconnectBase: 5, lastDataAt: lastDataAt, lastProbeAt: lastProbeAt}
	proxy.OnLowerStatus(opc.LowerStatus{Connection: opc.ConnectionDisconnected, Session: opc.SessionInactive,
		Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogReady, ReconnectCount: 2, LastError: "retry failed"})
	got, ok := runtime.LowerStatus("source-a")
	if !ok || got.ReconnectCount != 7 || !got.LastDataAt.Equal(lastDataAt) || !got.LastProbeAt.Equal(lastProbeAt) {
		t.Fatalf("status=%#v ok=%v", got, ok)
	}
}

func TestRuntimeWriteWaitsForCurrentEpochAckBaseline(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 6)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 6)

	// A reconnect can report SubscriptionActive before the retained _Send
	// value reaches the sample callback.  An ACK observed in the old epoch is
	// not a valid baseline for the new connection.
	runtime.OnLowerStatus(connectedStatus("source-a", 7))
	status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "OLD-EPOCH")
	if status != domain.StatusBadNoCommunication || !errors.Is(err, opc.ErrLowerUnavailable) {
		t.Fatalf("write before current-epoch baseline status=%#x err=%v", status, err)
	}
	client.mu.Lock()
	firstWrite := client.lastWrite
	client.mu.Unlock()
	if firstWrite != nil || runtime.routes.PendingCount("line") != 0 {
		t.Fatalf("write dispatched before baseline: value=%#v pending=%d", firstWrite, runtime.routes.PendingCount("line"))
	}

	// The current epoch's retained failure value establishes only the
	// baseline.  A second 3 produced by the actual dispatch is fresh and may
	// terminate the command immediately, matching the Rust lower protocol.
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{
		Type: domain.DataTypeString, Value: "3", StatusCode: domain.StatusGood, Epoch: 7,
	})
	client.writeHook = func() {
		runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{
			Type: domain.DataTypeString, Value: "3", StatusCode: domain.StatusGood, Epoch: 7,
		})
	}
	status, err = runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "CURRENT-EPOCH")
	if err != nil || status != domain.StatusGood {
		t.Fatalf("write after current-epoch baseline status=%#x err=%v", status, err)
	}
	if pending := runtime.routes.PendingCount("line"); pending != 0 {
		t.Fatalf("pending after fresh immediate failure ACK = %d", pending)
	}
	commands, total, listErr := runtime.repo.ListCommands(context.Background(), 10, 0)
	foundImmediateFailure := false
	for _, command := range commands {
		if command.State == domain.CommandFailed && command.StatusCode == domain.StatusBadUnexpectedError {
			foundImmediateFailure = true
		}
	}
	if listErr != nil || total != 2 || !foundImmediateFailure {
		t.Fatalf("commands=%#v total=%d err=%v", commands, total, listErr)
	}
}

func TestRuntimeWriteAcceptsCollapsedTerminalAndRejectsMismatchedEpoch(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 7)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 7)
	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%#x err=%v", status, err)
	}
	// A complete sequence from another connection epoch is also stale.
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "2", StatusCode: domain.StatusGood, Epoch: 8})
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 8})
	if pending := runtime.routes.PendingCount("line"); pending != 1 {
		t.Fatalf("pending after mismatched-epoch ACKs = %d", pending)
	}
	// Waiting and Received may be collapsed by QueueSize=1 within one sampling
	// interval. A fresh terminal value in the pinned epoch is sufficient.
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 7})
	if pending := runtime.routes.PendingCount("line"); pending != 0 {
		t.Fatalf("pending after collapsed terminal ACK = %d", pending)
	}
}

func TestRuntimePassiveSendValuesDoNotCreatePendingCommands(t *testing.T) {
	for _, payload := range []string{"", "IDLE", "0", "1", "2", "3"} {
		t.Run(fmt.Sprintf("value_%q", payload), func(t *testing.T) {
			runtime := seededRuntime(t)
			if err := runtime.openSession("session"); err != nil {
				t.Fatal(err)
			}
			client := &fakeLowerClient{writeStatus: domain.StatusGood}
			runtime.mu.Lock()
			runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
			runtime.statuses["source-a"] = connectedStatus("source-a", 1)
			runtime.mu.Unlock()
			if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), payload); err != nil || status != domain.StatusGood {
				t.Fatalf("passive write status=%#x err=%v", status, err)
			}
			if pending := runtime.routes.PendingCount("line"); pending != 0 {
				t.Fatalf("passive write created pending=%d", pending)
			}
			commands, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
			if err != nil || total != 0 || len(commands) != 0 {
				t.Fatalf("passive write audit commands=%#v total=%d err=%v", commands, total, err)
			}
		})
	}
}

func TestRuntimeWriteAcceptsAckThatArrivesBeforeWriteReturns(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	client.writeHook = func() {
		runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "2", StatusCode: domain.StatusGood, Epoch: 3})
		runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 3})
	}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 3)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 3)
	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%#x err=%v", status, err)
	}
	if pending := runtime.routes.PendingCount("line"); pending != 0 {
		t.Fatalf("pending after inline ACK = %d", pending)
	}
	commands, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 1 || commands[0].State != domain.CommandReceived {
		t.Fatalf("commands=%#v total=%d err=%v", commands, total, err)
	}
}

func TestRuntimeWriteAcceptsImmediateFailureAckWithoutWaiting(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	client.writeHook = func() {
		runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "3", StatusCode: domain.StatusGood, Epoch: 4})
	}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 4)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 4)
	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%#x err=%v", status, err)
	}
	if pending := runtime.routes.PendingCount("line"); pending != 0 {
		t.Fatalf("pending after immediate failure ACK = %d", pending)
	}
	commands, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 1 || len(commands) != 1 || commands[0].State != domain.CommandFailed || commands[0].StatusCode != domain.StatusBadUnexpectedError {
		t.Fatalf("commands=%#v total=%d err=%v", commands, total, err)
	}
}

func TestRuntimeWriteTimeoutPersistsTerminalStateAndUnlocksGroup(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 5)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 5)
	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%#x err=%v", status, err)
	}
	runtime.sweepExpiredCommands(time.Now().UTC().Add(runtime.options.CommandPendingTTL + time.Second))
	if pending := runtime.routes.PendingCount("line"); pending != 0 {
		t.Fatalf("pending after timeout = %d", pending)
	}
	commands, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 1 || len(commands) != 1 || commands[0].State != domain.CommandTimedOut || commands[0].StatusCode != domain.StatusBadTimeout {
		t.Fatalf("commands=%#v total=%d err=%v", commands, total, err)
	}
}

func TestRuntimePinnedWriteAcceptsTerminalAckAfterCatalogConflict(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 6)
	runtime.mu.Unlock()
	primeWriteAck(runtime, "source-a", "COM1", 6)
	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%#x err=%v", status, err)
	}
	groups, sources := runtime.routes.Groups(), runtime.routes.Sources()
	for index := range sources {
		if sources[index].ID == "source-a" {
			sources[index].CatalogState = domain.CatalogConflict
		}
	}
	if err := runtime.routes.ApplyConfiguration(groups, sources); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	catalog := runtime.catalogs["source-a"]
	catalog.State = domain.CatalogConflict
	runtime.catalogs["source-a"] = catalog
	runtime.mu.Unlock()
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "3", StatusCode: domain.StatusGood, Epoch: 6})
	if pending := runtime.routes.PendingCount("line"); pending != 0 {
		t.Fatalf("pending after pinned terminal ACK = %d", pending)
	}
	session, err := runtime.routes.Session("session")
	if err != nil || session.Routes["line"].SourceID != "" || session.Routes["line"].Revision != 2 {
		t.Fatalf("session after terminal ACK = %#v, err=%v", session, err)
	}
}

func TestValidateCatalogsMarksAllCrossGroupDuplicates(t *testing.T) {
	catalogs := map[string]domain.SourceCatalog{
		"a": {SourceID: "a", GroupID: "g1", State: domain.CatalogReady, Nodes: []domain.CatalogNode{testNode("DUP1")}},
		"b": {SourceID: "b", GroupID: "g2", State: domain.CatalogReady, Nodes: []domain.CatalogNode{testNode("DUP1")}},
		"c": {SourceID: "c", GroupID: "g3", State: domain.CatalogReady, Nodes: []domain.CatalogNode{testNode("DUP2")}},
		"d": {SourceID: "d", GroupID: "g4", State: domain.CatalogReady, Nodes: []domain.CatalogNode{testNode("DUP2")}},
	}
	index, states, conflicts, err := validateCatalogs(catalogs)
	if err != nil {
		t.Fatal(err)
	}
	if conflicts != 2 || len(index.points) != 0 {
		t.Fatalf("conflicts=%d points=%d", conflicts, len(index.points))
	}
	if len(index.union.Conflicts) != 2 || index.union.Conflicts[0].Kind != domain.ConflictCrossGroup || index.union.Conflicts[0].Identifier != "DUP1" {
		t.Fatalf("conflict details = %#v", index.union.Conflicts)
	}
	for _, sourceID := range []string{"a", "b", "c", "d"} {
		if states[sourceID] != domain.CatalogConflict {
			t.Fatalf("state[%s]=%s", sourceID, states[sourceID])
		}
	}
}

func TestRuntimeExposesCatalogConflictDetailsForEveryImplicatedSource(t *testing.T) {
	runtime := seededRuntime(t)
	mismatch := testNode("KKS001")
	mismatch.DataType = domain.DataTypeUInt32
	runtime.OnCatalog(domain.SourceCatalog{
		SourceID: "source-b", GroupID: "line", State: domain.CatalogReady, DiscoveredAt: time.Now().UTC(),
		Nodes: []domain.CatalogNode{mismatch, testNode("KKS002"), testStringNode("COM1_Send"), testMethodNode("COM1_SendCommand")},
	})
	for _, sourceID := range []string{"source-a", "source-b"} {
		source, err := runtime.GetSource(context.Background(), sourceID)
		if err != nil || source.CatalogState != domain.CatalogConflict || len(source.CatalogConflicts) != 1 ||
			source.CatalogConflicts[0].Identifier != "KKS001" || !strings.Contains(source.LastError, "KKS001") {
			t.Fatalf("source %s conflict view=%#v err=%v", sourceID, source, err)
		}
	}
}

func TestRuntimeDefaultOnlyAffectsNewSessions(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("old"); err != nil {
		t.Fatal(err)
	}
	group, err := runtime.setDefault(context.Background(), "line", "source-b", 2, "test")
	if err != nil || group.Revision != 3 {
		t.Fatalf("set default group=%#v err=%v", group, err)
	}
	if err := runtime.openSession("new"); err != nil {
		t.Fatal(err)
	}
	oldSession, _ := runtime.routes.Session("old")
	newSession, _ := runtime.routes.Session("new")
	if oldSession.Routes["line"].SourceID != "source-a" || newSession.Routes["line"].SourceID != "source-b" {
		t.Fatalf("old=%#v new=%#v", oldSession.Routes, newSession.Routes)
	}
}

func TestRuntimeCanClearDefaultThenDeleteFormerDefaultSource(t *testing.T) {
	runtime := seededRuntime(t)
	group, err := runtime.setDefault(context.Background(), "line", "", 2, "test")
	if err != nil || group.DefaultSourceID != "" || group.Revision != 3 {
		t.Fatalf("clear default group=%#v err=%v", group, err)
	}
	if err := runtime.DeleteSource(context.Background(), "source-a", 1); err != nil {
		t.Fatalf("delete former default source: %v", err)
	}
	if err := runtime.openSession("new"); err != nil {
		t.Fatal(err)
	}
	session, err := runtime.routes.Session("new")
	if err != nil || session.Routes["line"].SourceID != "" {
		t.Fatalf("new session after clearing default = %#v, err=%v", session, err)
	}
}

func TestRuntimeExposesLiveSubscriptionSelectionAndSessionMetadata(t *testing.T) {
	runtime := seededRuntime(t)
	runtime.SetSubscriptionCountHandler(func() int { return 7 })
	if _, err := runtime.setDefault(context.Background(), "line", "source-b", 2, "test"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.selectSource("session", "line", "source-a", 1, "test"); err != nil {
		t.Fatal(err)
	}
	serverLastAccess := time.Now().UTC().Add(time.Minute)
	runtime.SetSessionViewHandler(func() map[string]api.SessionView {
		return map[string]api.SessionView{"session": {
			Session: domain.Session{LastSeenAt: serverLastAccess}, ClientName: "SCADA", RemoteAddress: "127.0.0.1:50000",
		}}
	})

	overview, err := runtime.Overview(context.Background())
	if err != nil || overview.SubscriptionCount != 7 || overview.RecentSelectionCount != 2 || overview.RecentSelectionAt.IsZero() {
		t.Fatalf("overview=%#v err=%v", overview, err)
	}
	group, err := runtime.GetGroup(context.Background(), "line")
	if err != nil || group.LastSelectedAt.IsZero() {
		t.Fatalf("group=%#v err=%v", group, err)
	}
	sessions, err := runtime.ListSessions(context.Background())
	if err != nil || len(sessions) != 1 || sessions[0].RemoteAddress != "127.0.0.1:50000" || sessions[0].LastSeenAt != serverLastAccess {
		t.Fatalf("sessions=%#v err=%v", sessions, err)
	}

	count, latest, groups, err := loadSelectionMetrics(context.Background(), runtime.repo)
	if err != nil || count != 2 || latest.IsZero() || groups["line"].IsZero() {
		t.Fatalf("persisted selection metrics count=%d latest=%s groups=%#v err=%v", count, latest, groups, err)
	}
}

func TestRuntimeHealthStatisticsExcludeMethods(t *testing.T) {
	runtime := seededRuntime(t)
	if _, err := runtime.setDefault(context.Background(), "line", "source-b", 2, "test"); err != nil {
		t.Fatal(err)
	}
	runtime.OnLowerStatus(connectedStatus("source-b", 1))
	for identifier, value := range map[string]any{
		"KKS001": float64(1), "KKS002": float64(2), "COM1_Send": "1",
	} {
		dataType := domain.DataTypeDouble
		if identifier == "COM1_Send" {
			dataType = domain.DataTypeString
		}
		runtime.OnSample("source-b", testKey(identifier), domain.Sample{Type: dataType, Value: value, StatusCode: domain.StatusGood, Epoch: 1})
	}
	overview, err := runtime.Overview(context.Background())
	if err != nil || overview.PointCount != 3 || overview.GoodPoints != 3 || overview.BadPoints != 0 {
		t.Fatalf("overview=%#v err=%v", overview, err)
	}
	group, err := runtime.GetGroup(context.Background(), "line")
	if err != nil || group.TotalPoints != 3 || group.GoodPoints != 3 || group.BadPoints != 0 || group.Status != "Healthy" {
		t.Fatalf("group=%#v err=%v", group, err)
	}
	source, err := runtime.GetSource(context.Background(), "source-b")
	if err != nil || source.TotalPoints != 3 || source.GoodPoints != 3 || source.BadPoints != 0 {
		t.Fatalf("source=%#v err=%v", source, err)
	}
	if stats := runtime.SourcePointStatistics("source-b"); stats.Total != 3 || stats.Good != 3 || stats.Bad != 0 {
		t.Fatalf("source point statistics = %#v", stats)
	}
}

func TestRuntimeDeleteGroupCascadesSourcesOnlyWithoutSessions(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.DeleteGroup(context.Background(), "line", 2); err != nil {
		t.Fatal(err)
	}
	if len(runtime.routes.Groups()) != 0 || len(runtime.routes.Sources()) != 0 {
		t.Fatalf("routes after delete: groups=%#v sources=%#v", runtime.routes.Groups(), runtime.routes.Sources())
	}
	groups, err := runtime.repo.ListGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := runtime.repo.ListSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 || len(sources) != 0 {
		t.Fatalf("persisted after delete: groups=%#v sources=%#v", groups, sources)
	}
}

func TestRuntimeDeleteGroupResolvesDisconnectedSourceAlarms(t *testing.T) {
	runtime := seededRuntime(t)
	disconnected := opc.LowerStatus{SourceID: "source-a", Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogConflict,
		LastError: "connection lost"}
	runtime.statusPersistMu.Lock()
	runtime.dirtyAlarmStatuses["source-a"] = disconnected
	runtime.statusPersistMu.Unlock()
	runtime.retryDirtyStatuses(context.Background())
	if err := runtime.DeleteGroup(context.Background(), "line", 2); err != nil {
		t.Fatal(err)
	}
	runtime.retryDirtyStatuses(context.Background())
	open, _, err := runtime.repo.AlarmCounts(context.Background())
	if err != nil || open != 0 {
		t.Fatalf("open alarms after group delete=%d err=%v", open, err)
	}
}

func TestRuntimeDeleteGroupRejectsOnlineSession(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.DeleteGroup(context.Background(), "line", 2); !errors.Is(err, domain.ErrGroupInUse) {
		t.Fatalf("delete error = %v, want group in use", err)
	}
}

func TestRuntimeFailedCreateGroupDoesNotLeakLiveConfiguration(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.CreateGroup(ctx, api.GroupMutation{ID: "ghost", Name: "Ghost"}); err == nil {
		t.Fatal("create group with canceled context unexpectedly succeeded")
	}
	if _, ok := groupByID(runtime.routes.Groups(), "ghost"); ok {
		t.Fatalf("failed create leaked group into routes: %#v", runtime.routes.Groups())
	}
	session, err := runtime.routes.Session("session")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := session.Routes["ghost"]; ok {
		t.Fatalf("failed create leaked route into session: %#v", session.Routes)
	}
	if _, err := runtime.repo.GetGroup(context.Background(), "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("persisted ghost group error = %v", err)
	}
}

func TestRuntimeFailedSourceDisablePreservesSessionSelection(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.selectSource("session", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runtime.UpdateSource(ctx, "source-b", 1, api.SourceMutation{
		ID: "source-b", GroupID: "line", Name: "source-b", Endpoint: "opc.tcp://127.0.0.1:4841",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: false,
	})
	if err == nil {
		t.Fatal("update source with canceled context unexpectedly succeeded")
	}
	session, sessionErr := runtime.routes.Session("session")
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if route := session.Routes["line"]; route.SourceID != "source-b" || route.Revision != 2 {
		t.Fatalf("failed update changed session route: %#v", route)
	}
	source, sourceErr := runtime.repo.GetSource(context.Background(), "source-b")
	if sourceErr != nil || !source.Enabled {
		t.Fatalf("persisted source after failed update = %#v, err=%v", source, sourceErr)
	}
}

func TestRuntimeSourceResetDiscardsDirtyCatalogFromPreviousEndpoint(t *testing.T) {
	runtime := seededRuntime(t)
	stale, err := runtime.repo.LoadSourceCatalog(context.Background(), "source-b")
	if err != nil || len(stale.Nodes) == 0 {
		t.Fatalf("load stale catalog nodes=%d err=%v", len(stale.Nodes), err)
	}
	runtime.mu.Lock()
	runtime.dirtyCatalogs["source-b"] = stale
	runtime.catalogPersistError = "simulated prior write failure"
	runtime.mu.Unlock()
	_, err = runtime.UpdateSource(context.Background(), "source-b", 1, api.SourceMutation{
		ID: "source-b", GroupID: "line", Name: "source-b", Endpoint: "opc.tcp://127.0.0.1:5841",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.RLock()
	_, stillDirty := runtime.dirtyCatalogs["source-b"]
	persistError := runtime.catalogPersistError
	runtime.mu.RUnlock()
	if stillDirty || persistError != "" {
		t.Fatalf("reset retained stale dirty catalog: dirty=%v error=%q", stillDirty, persistError)
	}
	runtime.retryDirtyCatalogs(context.Background())
	persisted, err := runtime.repo.LoadSourceCatalog(context.Background(), "source-b")
	if err != nil || len(persisted.Nodes) != 0 || persisted.State != domain.CatalogDiscovering {
		t.Fatalf("catalog after reset/retry = %#v err=%v", persisted, err)
	}
}

func TestRuntimeMovingUnusedSourceDoesNotConflictWithOldGroup(t *testing.T) {
	runtime := seededRuntime(t)
	if _, err := runtime.CreateGroup(context.Background(), api.GroupMutation{ID: "line-2", Name: "Line 2"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.routes.Session("session")
	if err != nil || before.Routes["line"].SourceID != "source-a" {
		t.Fatalf("session before move = %#v err=%v", before, err)
	}
	updated, err := runtime.UpdateSource(context.Background(), "source-b", 1, api.SourceMutation{
		ID: "source-b", GroupID: "line-2", Name: "source-b", Endpoint: "opc.tcp://127.0.0.1:4841",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.GroupID != "line-2" || updated.CatalogState != domain.CatalogDiscovering {
		t.Fatalf("moved source = %#v", updated)
	}
	after, err := runtime.routes.Session("session")
	if err != nil || after.Routes["line"] != before.Routes["line"] {
		t.Fatalf("healthy route changed by unused source move: before=%#v after=%#v err=%v", before.Routes, after.Routes, err)
	}
	sourceA, ok := sourceByID(runtime.routes.Sources(), "source-a")
	if !ok || !sourceA.CatalogState.Selectable() {
		t.Fatalf("healthy source conflicted after move: %#v", sourceA)
	}
	point, ok := runtime.pointForIdentifier("KKS001")
	if !ok || point.GroupID != "line" || point.Node.Retired {
		t.Fatalf("shared point after move = %#v, exists=%v", point, ok)
	}
}

func TestRuntimeRetiredCommandNodesAreNeverDispatched(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 1)
	for _, identifier := range []string{"COM1_Send", "COM1_SendCommand"} {
		point := runtime.index.pointByNode[identifier]
		point.Node.Retired = true
		runtime.index.pointByNode[identifier] = point
	}
	runtime.mu.Unlock()

	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); status != domain.StatusBadOutOfService || !errors.Is(err, domain.ErrNodeRetired) {
		t.Fatalf("retired write status=%#x err=%v", status, err)
	}
	if result, status, err := runtime.CallCommand(context.Background(), "session", testKey("COM1_SendCommand"), "AABB"); result != "" || status != domain.StatusBadOutOfService || !errors.Is(err, domain.ErrNodeRetired) {
		t.Fatalf("retired call result=%q status=%#x err=%v", result, status, err)
	}
	if client.lastWrite != nil || runtime.routes.PendingCount("line") != 0 {
		t.Fatalf("retired command dispatched: lastWrite=%#v pending=%d", client.lastWrite, runtime.routes.PendingCount("line"))
	}
	_, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 0 {
		t.Fatalf("retired command audit total=%d err=%v", total, err)
	}
}

func TestRuntimeMissingSelectedSourceCommandNodesAreNeverDispatched(t *testing.T) {
	runtime := seededRuntime(t)
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	runtime.mu.Lock()
	runtime.lowers["source-a"] = lowerHandle{client: client, generation: 1, cancel: func() {}}
	runtime.statuses["source-a"] = connectedStatus("source-a", 1)
	delete(runtime.index.sourceNodes["source-a"], "COM1_Send")
	delete(runtime.index.sourceNodes["source-a"], "COM1_SendCommand")
	runtime.mu.Unlock()

	if status, err := runtime.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "AABB"); status != domain.StatusBadConfigurationError || !errors.Is(err, domain.ErrNodeNotConfigured) {
		t.Fatalf("missing-source write status=%#x err=%v", status, err)
	}
	if result, status, err := runtime.CallCommand(context.Background(), "session", testKey("COM1_SendCommand"), "AABB"); result != "" || status != domain.StatusBadConfigurationError || !errors.Is(err, domain.ErrNodeNotConfigured) {
		t.Fatalf("missing-source call result=%q status=%#x err=%v", result, status, err)
	}
	if client.lastWrite != nil || runtime.routes.PendingCount("line") != 0 {
		t.Fatalf("missing-source command dispatched: lastWrite=%#v pending=%d", client.lastWrite, runtime.routes.PendingCount("line"))
	}
	_, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 0 {
		t.Fatalf("missing-source command audit total=%d err=%v", total, err)
	}
}

func TestRuntimeCloseSerializesWithDynamicLowerStart(t *testing.T) {
	runtime := seededRuntime(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	runtime.options.LowerFactory = func(cfg opc.LowerConfig, _ opc.LowerSink) (opc.LowerClient, error) {
		if cfg.Source.ID == "source-c" {
			close(entered)
			<-release
		}
		return &fakeLowerClient{writeStatus: domain.StatusGood}, nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	created := make(chan error, 1)
	go func() {
		_, err := runtime.CreateSource(context.Background(), api.SourceMutation{
			ID: "source-c", GroupID: "line", Name: "source-c", Endpoint: "opc.tcp://127.0.0.1:4843",
			SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
		})
		created <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- runtime.CloseContext(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("CloseContext bypassed in-flight lower registration: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if runtime.running.Load() {
		t.Fatal("runtime still running after close")
	}
	runtime.mu.RLock()
	lowerCount := len(runtime.lowers)
	runtime.mu.RUnlock()
	if lowerCount != 0 {
		t.Fatalf("lower handles after close = %d", lowerCount)
	}
}

func TestRuntimeCloseHonorsDeadlineWhileConfigurationGateIsBusy(t *testing.T) {
	runtime := seededRuntime(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	runtime.options.LowerFactory = func(cfg opc.LowerConfig, _ opc.LowerSink) (opc.LowerClient, error) {
		if cfg.Source.ID == "source-c" {
			close(entered)
			<-release
		}
		return &fakeLowerClient{writeStatus: domain.StatusGood}, nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	created := make(chan error, 1)
	go func() {
		_, err := runtime.CreateSource(context.Background(), api.SourceMutation{
			ID: "source-c", GroupID: "line", Name: "source-c", Endpoint: "opc.tcp://127.0.0.1:4843",
			SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
		})
		created <- err
	}()
	<-entered
	closeContext, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := runtime.CloseContext(closeContext)
	cancelClose()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close while config gate busy = %v, want deadline", err)
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	retryContext, cancelRetry := context.WithTimeout(context.Background(), time.Second)
	defer cancelRetry()
	if err := runtime.CloseContext(retryContext); err != nil {
		t.Fatalf("retry close after config release: %v", err)
	}
	if err := runtime.Start(context.Background()); err == nil {
		t.Fatal("closed runtime unexpectedly restarted")
	}
}

func TestRuntimeStartCannotPublishReadyAfterConcurrentClose(t *testing.T) {
	runtime := seededRuntime(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runtime.options.LowerFactory = func(opc.LowerConfig, opc.LowerSink) (opc.LowerClient, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return &fakeLowerClient{writeStatus: domain.StatusGood}, nil
	}
	started := make(chan error, 1)
	go func() { started <- runtime.Start(context.Background()) }()
	<-entered
	closeContext, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := runtime.CloseContext(closeContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close while Start factory blocked = %v, want deadline", err)
	}
	cancelClose()
	close(release)
	if err := <-started; !errors.Is(err, domain.ErrShuttingDown) {
		t.Fatalf("Start after concurrent close = %v, want shutting down", err)
	}
	retryContext, cancelRetry := context.WithTimeout(context.Background(), time.Second)
	defer cancelRetry()
	if err := runtime.CloseContext(retryContext); err != nil {
		t.Fatal(err)
	}
	if runtime.ready.Load() || runtime.running.Load() {
		t.Fatalf("runtime state after concurrent close: ready=%v running=%v", runtime.ready.Load(), runtime.running.Load())
	}
}

func TestRuntimeCloseCanResumeWaitingAfterTimeout(t *testing.T) {
	runtime := seededRuntime(t)
	release := make(chan struct{})
	runtime.options.LowerFactory = func(opc.LowerConfig, opc.LowerSink) (opc.LowerClient, error) {
		return &blockingLowerClient{release: release}, nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runtime.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first close error = %v, want deadline", err)
	}
	close(release)
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	if err := runtime.CloseContext(secondCtx); err != nil {
		t.Fatalf("second close did not resume wait: %v", err)
	}
}

func TestRuntimeGracefulCloseFlushesDirtyDurableState(t *testing.T) {
	runtime := seededRuntime(t)
	runtime.options.LowerFactory = func(opc.LowerConfig, opc.LowerSink) (opc.LowerClient, error) {
		return &contextLowerClient{}, nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	catalog := runtime.catalogs["source-a"]
	catalog.DiscoveredAt = now
	command := domain.PendingCommand{ID: "close-flush-command", SessionID: "session", GroupID: "line", SourceID: "source-a",
		Port: "COM1", Kind: "Write", State: domain.CommandReceived, StatusCode: domain.StatusGood,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	audit := domain.AuditEntry{Kind: "close.flush", Actor: "test", Target: "runtime", Revision: 1, OccurredAt: now}
	runtime.mu.Lock()
	runtime.dirtyCatalogs[catalog.SourceID] = catalog
	runtime.dirtyAudits["close-flush"] = audit
	runtime.mu.Unlock()
	runtime.commandMu.Lock()
	runtime.dirtyCommands[command.ID] = command
	runtime.commandMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	persistedCatalog, err := runtime.repo.LoadSourceCatalog(context.Background(), "source-a")
	if err != nil || !persistedCatalog.DiscoveredAt.Equal(now) {
		t.Fatalf("catalog after close = %#v, err=%v", persistedCatalog, err)
	}
	commands, total, err := runtime.repo.ListCommands(context.Background(), 10, 0)
	if err != nil || total != 1 || len(commands) != 1 || commands[0].State != domain.CommandReceived {
		t.Fatalf("commands after close = %#v, total=%d, err=%v", commands, total, err)
	}
	audits, total, err := runtime.repo.ListAudit(context.Background(), 10, 0)
	if err != nil || total != 1 || len(audits) != 1 || audits[0].Kind != "close.flush" {
		t.Fatalf("audits after close = %#v, total=%d, err=%v", audits, total, err)
	}
}

func TestRuntimeRetriesLowerFactoryFailure(t *testing.T) {
	runtime := seededRuntime(t)
	failSourceA := true
	runtime.options.LowerFactory = func(cfg opc.LowerConfig, _ opc.LowerSink) (opc.LowerClient, error) {
		if cfg.Source.ID == "source-a" && failSourceA {
			return nil, errors.New("temporary credential provider failure")
		}
		return &contextLowerClient{}, nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, ok := runtime.LowerStatus("source-a")
	if !ok || status.LastError == "" {
		t.Fatalf("source-a status after factory failure = %#v, exists=%v", status, ok)
	}
	runtime.mu.RLock()
	_, exists := runtime.lowers["source-a"]
	runtime.mu.RUnlock()
	if exists {
		t.Fatal("failed lower factory left a registered handle")
	}
	failSourceA = false
	runtime.retryMissingLowers()
	runtime.mu.RLock()
	_, exists = runtime.lowers["source-a"]
	runtime.mu.RUnlock()
	if !exists {
		t.Fatal("missing lower was not restarted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCoalescesLastDataStatusWithSampleInvalidations(t *testing.T) {
	runtime := seededRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription, err := runtime.broker.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	stable := connectedStatus("source-a", 1)
	runtime.OnLowerStatus(stable)
	select {
	case <-subscription.Events:
	case <-time.After(time.Second):
		t.Fatal("missing immediate connection-state event")
	}
	runtime.mu.Lock()
	runtime.lastSampleEventAt = time.Now().UTC()
	runtime.mu.Unlock()
	for index := 0; index < 20; index++ {
		tick := stable
		tick.LastDataAt = time.Now().UTC().Add(time.Duration(index) * time.Millisecond)
		tick.UpdatedAt = tick.LastDataAt
		runtime.OnLowerStatus(tick)
	}
	select {
	case event := <-subscription.Events:
		t.Fatalf("LastDataAt-only burst was not coalesced: %#v", event)
	case <-time.After(30 * time.Millisecond):
	}
	runtime.flushPendingSampleEvent(time.Now().UTC().Add(2 * time.Second))
	select {
	case event := <-subscription.Events:
		if event.Type != "source.sample" {
			t.Fatalf("event type = %q", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("missing trailing coalesced status event")
	}
}

func TestRuntimeManagementAuditUsesInternalContextAndAffectsHealth(t *testing.T) {
	runtime := seededRuntime(t)
	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	runtime.audit(requestContext, "group.update", "line", 3, map[string]any{"test": true})
	entries, total, err := runtime.repo.ListAudit(context.Background(), 10, 0)
	if err != nil || total != 1 || len(entries) != 1 || entries[0].Target != "line" {
		t.Fatalf("audit entries=%#v total=%d err=%v", entries, total, err)
	}
	runtime.mu.Lock()
	runtime.auditPersistError = "disk unavailable"
	runtime.mu.Unlock()
	health := runtime.Health(context.Background())
	if health.OK || health.Checks["management_audit_persistence"] != "disk unavailable" {
		t.Fatalf("health with audit failure = %#v", health)
	}
}

func TestRuntimeCatalogPreflightRejectsHotShapeChange(t *testing.T) {
	runtime := seededRuntime(t)
	reconciles := 0
	err := runtime.SetCatalogHandlers(
		func(union domain.CatalogUnion, _ []domain.Group, _ []domain.Source) error {
			if node, ok := union.Groups["line"]["HOT"]; ok && node.DataType == domain.DataTypeString {
				return errors.New("hot shape change rejected")
			}
			return nil
		},
		func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
			reconciles++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	base := []domain.CatalogNode{testNode("KKS001"), testStringNode("COM1_Send"), testMethodNode("COM1_SendCommand")}
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-a", GroupID: "line", State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC(), Nodes: append(append([]domain.CatalogNode(nil), base...), testNode("HOT"))})
	point, ok := runtime.pointForIdentifier("HOT")
	if !ok || point.Node.DataType != domain.DataTypeDouble {
		t.Fatalf("initial hot point = %#v, exists=%v", point, ok)
	}
	runtime.OnSample("source-a", testKey("HOT"), domain.Sample{Type: domain.DataTypeDouble, Value: 42.0, StatusCode: domain.StatusGood})
	beforeReject := reconciles
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-a", GroupID: "line", State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC().Add(time.Second), Nodes: append(append([]domain.CatalogNode(nil), base...),
			domain.CatalogNode{Key: testKey("HOT"), BrowseName: "HOT", ParentPath: "Objects/Devices", Class: domain.NodeClassVariable, DataType: domain.DataTypeString, ValueRank: -1})})
	point, ok = runtime.pointForIdentifier("HOT")
	if !ok || point.Node.DataType != domain.DataTypeDouble || !point.Node.Retired || reconciles != beforeReject+1 {
		t.Fatalf("catalog changed after rejected preflight: point=%#v reconciles=%d/%d", point, reconciles, beforeReject)
	}
	source, ok := sourceByID(runtime.routes.Sources(), "source-a")
	if !ok || source.CatalogState != domain.CatalogConflict {
		t.Fatalf("rejected source was not isolated: %#v", source)
	}
	runtime.OnSample("source-a", testKey("HOT"), domain.Sample{Type: domain.DataTypeString, Value: "wrong", StatusCode: domain.StatusGood})
	if sample, exists := runtime.cache.Get("source-a", testKey("HOT")); !exists || sample.Type != domain.DataTypeDouble || sample.Value != 42.0 {
		t.Fatalf("rejected schema polluted cache: sample=%#v exists=%v", sample, exists)
	}
	persisted, err := runtime.repo.LoadSourceCatalog(context.Background(), "source-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range persisted.Nodes {
		if node.Key.Identifier == "HOT" && (node.DataType != domain.DataTypeDouble || !node.Retired) {
			t.Fatalf("persisted rejected shape = %#v", node)
		}
	}
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-b", GroupID: "line", State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC().Add(2 * time.Second), Nodes: []domain.CatalogNode{
			testNode("KKS001"), testNode("KKS002"), testStringNode("COM1_Send"), testMethodNode("COM1_SendCommand"),
		}})
	source, ok = sourceByID(runtime.routes.Sources(), "source-a")
	if !ok || source.CatalogState != domain.CatalogConflict {
		t.Fatalf("unrelated source discovery cleared quarantine: %#v", source)
	}
	session, err := runtime.routes.OpenSession("after-rejection")
	if err != nil {
		t.Fatal(err)
	}
	if session.Routes["line"].SourceID != "" {
		t.Fatalf("new session selected quarantined default: %#v", session.Routes["line"])
	}
	if _, err := runtime.routes.Select("after-rejection", "line", "source-a", session.Routes["line"].Revision); !errors.Is(err, domain.ErrSourceNotSelectable) {
		t.Fatalf("quarantined source selection error = %v", err)
	}
	if health := runtime.Health(context.Background()); health.OK || health.Checks["upstream_catalog"] == "ok" {
		t.Fatalf("health after rejected catalog = %#v", health)
	}
	overview, err := runtime.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conflictsAfterFirstRejection := overview.CatalogConflicts
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-a", GroupID: "line", State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC().Add(3 * time.Second), Nodes: append(append([]domain.CatalogNode(nil), base...),
			domain.CatalogNode{Key: testKey("HOT"), BrowseName: "HOT", ParentPath: "Objects/Devices", Class: domain.NodeClassVariable, DataType: domain.DataTypeString, ValueRank: -1})})
	overview, err = runtime.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := overview.CatalogConflicts; got != conflictsAfterFirstRejection {
		t.Fatalf("repeated rejection changed conflict count: got %d, want %d", got, conflictsAfterFirstRejection)
	}
}

func TestNewRuntimeRejectsPendingTimeoutShorterThanOperationTimeout(t *testing.T) {
	repository, err := store.Open(context.Background(), ":memory:", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if _, err := NewRuntime(context.Background(), repository, RuntimeOptions{
		CommandTimeout:    10 * time.Second,
		CommandPendingTTL: 5 * time.Second,
	}); err == nil {
		t.Fatal("NewRuntime unexpectedly accepted a pending timeout shorter than the operation timeout")
	}
}

func TestSampleMatchesCatalogNodeChecksActualValueTypeAndRank(t *testing.T) {
	node := testNode("typed")
	if !sampleMatchesCatalogNode(domain.Sample{Type: domain.DataTypeDouble, Value: float64(1)}, node) {
		t.Fatal("valid scalar Double was rejected")
	}
	if sampleMatchesCatalogNode(domain.Sample{Type: domain.DataTypeDouble, Value: "not-a-double"}, node) {
		t.Fatal("String value was accepted for Double catalog node")
	}
	node.ValueRank = 1
	if !sampleMatchesCatalogNode(domain.Sample{Type: domain.DataTypeDouble, Value: []float64{1}}, node) {
		t.Fatal("valid Double array was rejected")
	}
	if sampleMatchesCatalogNode(domain.Sample{Type: domain.DataTypeDouble, Value: []string{"wrong"}}, node) {
		t.Fatal("String array was accepted for Double catalog node")
	}
}

func TestRuntimeRetriesFailedUpstreamReconciliation(t *testing.T) {
	runtime := seededRuntime(t)
	fail := false
	calls := 0
	if err := runtime.SetCatalogHandlers(nil, func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
		calls++
		if fail {
			return errors.New("temporary namespace failure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := runtime.CreateGroup(context.Background(), api.GroupMutation{ID: "retry-group", Name: "Retry Group"}); err != nil {
		t.Fatalf("CreateGroup() error = %v", err)
	}
	if health := runtime.Health(context.Background()); health.OK || health.Checks["upstream_catalog"] == "ok" {
		t.Fatalf("health after failed reconciliation = %#v", health)
	}
	runtime.mu.RLock()
	dirty := runtime.upstreamReconcileDirty
	runtime.mu.RUnlock()
	if !dirty {
		t.Fatal("failed reconciliation was not marked dirty")
	}
	fail = false
	runtime.retryDirtyUpstream()
	if calls != 3 {
		t.Fatalf("reconciler calls = %d, want 3", calls)
	}
	if health := runtime.Health(context.Background()); !health.OK || health.Checks["upstream_catalog"] != "ok" {
		t.Fatalf("health after successful retry = %#v", health)
	}
	runtime.mu.RLock()
	dirty = runtime.upstreamReconcileDirty
	runtime.mu.RUnlock()
	if dirty {
		t.Fatal("successful reconciliation did not clear dirty state")
	}
}

func seededRuntime(t *testing.T) *Runtime {
	t.Helper()
	ctx := context.Background()
	repository, err := store.Open(ctx, ":memory:", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	group, err := repository.CreateGroup(ctx, domain.Group{ID: "line", Name: "Line"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-a", "source-b"} {
		if _, err := repository.CreateSource(ctx, domain.Source{ID: id, GroupID: "line", Name: id,
			Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None",
			Enabled: true, CatalogState: domain.CatalogReady}, ""); err != nil {
			t.Fatal(err)
		}
	}
	group.DefaultSourceID = "source-a"
	if _, err := repository.UpdateGroup(ctx, group, 1); err != nil {
		t.Fatal(err)
	}
	common := []domain.CatalogNode{testNode("KKS001"), testStringNode("COM1_Send"), testMethodNode("COM1_SendCommand")}
	if err := repository.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: "source-a", GroupID: "line", State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC(), Nodes: common}); err != nil {
		t.Fatal(err)
	}
	second := append(append([]domain.CatalogNode(nil), common...), testNode("KKS002"))
	if err := repository.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: "source-b", GroupID: "line", State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC(), Nodes: second}); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(ctx, repository, RuntimeOptions{CommandPendingTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func testKey(identifier string) domain.NodeKey {
	return domain.NodeKey{NamespaceURI: opc.BusinessNamespaceURI, Identifier: identifier}
}

func testNode(identifier string) domain.CatalogNode {
	return domain.CatalogNode{Key: testKey(identifier), BrowseName: identifier, ParentPath: "Objects/Devices",
		Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1}
}

func testStringNode(identifier string) domain.CatalogNode {
	return domain.CatalogNode{Key: testKey(identifier), BrowseName: identifier, ParentPath: "Objects/Devices",
		Class: domain.NodeClassVariable, DataType: domain.DataTypeString, ValueRank: -1, Writable: true}
}

func testMethodNode(identifier string) domain.CatalogNode {
	return domain.CatalogNode{Key: testKey(identifier), BrowseName: identifier, ParentPath: "Objects/Methods",
		Class: domain.NodeClassMethod, ValueRank: -1,
		InputArguments:  []domain.Argument{{Name: "HexData", DataType: domain.DataTypeString, ValueRank: -1}},
		OutputArguments: []domain.Argument{{Name: "Result", DataType: domain.DataTypeString, ValueRank: -1}}}
}

func connectedStatus(sourceID string, epoch uint64) opc.LowerStatus {
	return opc.LowerStatus{SourceID: sourceID, Connection: opc.ConnectionConnected, Session: opc.SessionActive,
		Subscription: opc.SubscriptionActive, Catalog: domain.CatalogReady, Epoch: epoch, UpdatedAt: time.Now().UTC()}
}

func primeWriteAck(runtime *Runtime, sourceID, port string, epoch uint64) {
	runtime.OnSample(sourceID, testKey(port+"_Send"), domain.Sample{
		Type: domain.DataTypeString, Value: "0", StatusCode: domain.StatusGood, Epoch: epoch,
	})
}

type fakeLowerClient struct {
	mu          sync.Mutex
	lastWrite   any
	writeStatus domain.StatusCode
	writeHook   func()
}

type blockingLowerClient struct {
	fakeLowerClient
	release <-chan struct{}
}

type contextLowerClient struct{ fakeLowerClient }

func (*contextLowerClient) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (f *blockingLowerClient) Run(context.Context) error {
	<-f.release
	return nil
}

func (*fakeLowerClient) Run(context.Context) error { return nil }
func (*fakeLowerClient) Read(context.Context, domain.NodeKey) (domain.Sample, error) {
	return domain.Sample{}, nil
}
func (f *fakeLowerClient) Write(_ context.Context, _ domain.NodeKey, value any) (domain.StatusCode, error) {
	f.mu.Lock()
	f.lastWrite = value
	hook := f.writeHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f.writeStatus, nil
}
func (*fakeLowerClient) Call(context.Context, domain.NodeKey, string) (string, domain.StatusCode, error) {
	return "OK", domain.StatusGood, nil
}
func (*fakeLowerClient) Status() opc.LowerStatus { return opc.LowerStatus{} }
