package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

func TestStateOutputsWithoutForwardingRules(t *testing.T) {
	r := stateRuntime(t)
	content := strings.Split(stateConfig, "[[rules]]")[0]
	activateTestCalculation(t, r, content)
	var refreshed []string
	r.SetCalculationRefreshHandler(func(names []string) { refreshed = append(refreshed, names...) })
	r.runCalculationBatch()
	check := func(name string, want uint32, quality domain.StatusCode) {
		t.Helper()
		for _, session := range []string{"first", "second"} {
			got := r.ReadSample(session, testKey(name))
			if got.Type != domain.DataTypeUInt32 || got.Value != want || got.StatusCode != quality {
				t.Fatalf("%s / %s = %#v, want %d / %v", session, name, got, want, quality)
			}
		}
	}
	check("FIXED", 1, domain.StatusGood)
	check("DEFAULT", 1, domain.StatusGood)
	point, found := r.pointForIdentifier("FIXED")
	if !found || point.Node.Writable || point.Node.DataType != domain.DataTypeUInt32 {
		t.Fatal("state-only configuration not published", point)
	}
	group := r.routes.Groups()[0]
	if _, err := r.SetDefaultSource(context.Background(), "line", "source-b", group.Revision); err != nil {
		t.Fatal(err)
	}
	r.runCalculationBatch()
	check("DEFAULT", 2, domain.StatusGood)
	check("FIXED", 1, domain.StatusGood)
	r.OnLowerStatus(opc.LowerStatus{SourceID: "source-a", Connection: opc.ConnectionDisconnected, Session: opc.SessionInactive, Epoch: 1, Catalog: domain.CatalogReady})
	r.runCalculationBatch()
	check("FIXED", 5, domain.StatusGood)
	if !strings.Contains(strings.Join(refreshed, ","), "FIXED") {
		t.Fatal("unreferenced state output was not refreshed", refreshed)
	}
	r.OnLowerStatus(connectedStatus("source-a", 2))
	r.runCalculationBatch()
	check("FIXED", 0, domain.StatusGood)
	r.mu.Lock()
	catalog := r.catalogs["source-a"]
	catalog.Topology.Ports = nil
	r.catalogs["source-a"] = catalog
	r.mu.Unlock()
	r.requestCalculationFull()
	r.runCalculationBatch()
	check("FIXED", 0, domain.StatusBadConfigurationError)
	// Removing the binding retires the same NodeId; no stale status survives.
	activateTestCalculation(t, r, "")
	check("FIXED", 0, domain.StatusBadOutOfService)
	point, found = r.pointForIdentifier("FIXED")
	if !found || !point.Node.Retired {
		t.Fatal("removed state output was not retained as retired", point)
	}
}

func TestStateOutputsKeepObservationTimeAndRetiredRuleIdentity(t *testing.T) {
	r := stateRuntime(t)
	activateTestCalculation(t, r, "[[rules]]\nkks_calc=\"FIXED\"\ndata_type=\"UInt32\"\nexpression=\"123\"\n")
	r.runCalculationBatch()
	activateTestCalculation(t, r, "")
	activateTestCalculation(t, r, strings.Split(stateConfig, "[[rules]]")[0])
	r.runCalculationBatch()
	got := r.ReadSample("", testKey("FIXED"))
	if got.Value != uint32(1) || got.StatusCode != domain.StatusGood || got.SourceTimestamp.IsZero() {
		t.Fatal("state did not revive the old UInt32 NodeId", got)
	}
	r.mu.Lock()
	snapshot, _ := r.stations.snapshot("source-a")
	snapshot.ObservedAt = time.Now().Add(-time.Minute)
	r.stations.replace("source-a", snapshot, 1, true)
	r.mu.Unlock()
	r.refreshCalculationStateInputs("source-a")
	got = r.ReadSample("", testKey("FIXED"))
	if got.Value != uint32(0) || got.StatusCode != domain.StatusGood {
		t.Fatal("stale observation must be state 0, not cached Good/1", got)
	}
	status, _ := r.GetCalculationStatus(context.Background())
	if status.RuleCount != 0 || status.GoodCount != 0 || status.BadCount != 0 {
		t.Fatal("diagnostic state nodes must not inflate formula statistics", status)
	}
	if err := r.DeactivateCalculation(context.Background(), status.ActiveRevision); err != nil {
		t.Fatal("state-only configuration should deactivate without lower calculation nodes", err)
	}
}
