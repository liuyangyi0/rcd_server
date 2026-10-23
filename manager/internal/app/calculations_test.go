package app

import (
	"context"
	"testing"
	"time"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

func activateTestCalculation(t *testing.T, runtime *Runtime, content string) calculation.Version {
	t.Helper()
	ctx := context.Background()
	draft, err := runtime.GetCalculationDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	draft, err = runtime.SaveCalculationDraft(ctx, draft.Revision, content)
	if err != nil {
		t.Fatal(err)
	}
	version, err := runtime.ActivateCalculation(ctx, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestCentralCalculationUsesGroupDefaultAndIgnoresSessionSelections(t *testing.T) {
	runtime := seededRuntime(t)
	version := activateTestCalculation(t, runtime, `[[rules]]
kks_calc = "CENTRAL"
data_type = "double"
expression = "KKS001 * 2 + 5"
`)
	if version.State != "active" {
		t.Fatalf("initial version state=%q", version.State)
	}
	for _, sessionID := range []string{"default-session", "selected-session"} {
		if err := runtime.openSession(sessionID); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := runtime.routes.Session("selected-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.selectSource("selected-session", "line", "source-b", selected.Routes["line"].Revision, "test"); err != nil {
		t.Fatal(err)
	}

	firstAt := time.Date(2026, time.September, 1, 8, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Second)
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnLowerStatus(connectedStatus("source-b", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 10.0, StatusCode: domain.StatusGood, SourceTimestamp: firstAt, Epoch: 1})
	runtime.OnSample("source-b", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 25.0, StatusCode: domain.StatusGood, SourceTimestamp: secondAt, Epoch: 1})
	runtime.runCalculationBatch()

	for _, sessionID := range []string{"default-session", "selected-session"} {
		got := runtime.ReadSample(sessionID, testKey("CENTRAL"))
		if got.Value != 25.0 || got.StatusCode != domain.StatusGood || !got.SourceTimestamp.Equal(firstAt) {
			t.Fatalf("%s central result=%#v; session route leaked into calculation", sessionID, got)
		}
	}
	runtime.mu.RLock()
	routeRevisionBefore := runtime.calculations.status().DefaultRouteRevision
	runtime.mu.RUnlock()
	selected, _ = runtime.routes.Session("selected-session")
	if _, err = runtime.selectSource("selected-session", "line", "source-a", selected.Routes["line"].Revision, "test"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.RLock()
	routeRevisionAfterSessionSelection := runtime.calculations.status().DefaultRouteRevision
	runtime.mu.RUnlock()
	if routeRevisionAfterSessionSelection != routeRevisionBefore {
		t.Fatalf("Session selection changed calculation route revision: %d -> %d", routeRevisionBefore, routeRevisionAfterSessionSelection)
	}

	group := runtime.routes.Groups()[0]
	if _, err = runtime.SetDefaultSource(context.Background(), "line", "source-b", group.Revision); err != nil {
		t.Fatal(err)
	}
	runtime.runCalculationBatch()
	for _, sessionID := range []string{"default-session", "selected-session"} {
		got := runtime.ReadSample(sessionID, testKey("CENTRAL"))
		if got.Value != 55.0 || got.StatusCode != domain.StatusGood || !got.SourceTimestamp.Equal(secondAt) {
			t.Fatalf("%s result after default switch=%#v", sessionID, got)
		}
	}

	runtime.OnLowerStatus(opc.LowerStatus{SourceID: "source-b", Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogReady, Epoch: 1})
	runtime.runCalculationBatch()
	got := runtime.ReadSample("selected-session", testKey("CENTRAL"))
	if got.Value != 5.0 || got.StatusCode != domain.StatusBadNoCommunication || !got.SourceTimestamp.Equal(secondAt) {
		t.Fatalf("offline default source was not recalculated from a zero input: %#v", got)
	}
}

func TestCentralCalculationHotRemovalAndTypeChangeLifecycle(t *testing.T) {
	runtime := seededRuntime(t)
	first := activateTestCalculation(t, runtime, `[[rules]]
kks_calc = "KEEP"
data_type = "double"
expression = "KKS001 + 1"
[[rules]]
kks_calc = "REMOVE"
data_type = "double"
expression = "KKS001 + 2"
`)
	if first.State != "active" {
		t.Fatalf("first state=%q", first.State)
	}
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 4.0, StatusCode: domain.StatusGood, Epoch: 1})
	runtime.runCalculationBatch()

	second := activateTestCalculation(t, runtime, `[[rules]]
kks_calc = "KEEP"
data_type = "double"
expression = "KKS001 + 10"
`)
	if second.State != "active" || second.RemovedCount != 1 || second.ChangedCount != 1 {
		t.Fatalf("hot activation=%#v", second)
	}
	runtime.runCalculationBatch()
	if got := runtime.ReadSample("", testKey("KEEP")); got.Value != 14.0 || got.StatusCode != domain.StatusGood {
		t.Fatalf("hot expression result=%#v", got)
	}
	if got := runtime.ReadSample("", testKey("REMOVE")); got.StatusCode != domain.StatusBadOutOfService {
		t.Fatalf("removed output=%#v", got)
	}

	pending := activateTestCalculation(t, runtime, `[[rules]]
kks_calc = "KEEP"
data_type = "uint"
expression = "KKS001 + 20"
`)
	if pending.State != "pending_restart" {
		t.Fatalf("type-changing version state=%q", pending.State)
	}
	status, err := runtime.GetCalculationStatus(context.Background())
	if err != nil || !status.PendingRestart || status.PendingRevision != pending.Revision || status.ActiveRevision != second.Revision {
		t.Fatalf("pending status=%#v err=%v", status, err)
	}
	if got := runtime.ReadSample("", testKey("KEEP")); got.Type != domain.DataTypeDouble || got.Value != 14.0 {
		t.Fatalf("pending restart changed current node shape/value: %#v", got)
	}
}
