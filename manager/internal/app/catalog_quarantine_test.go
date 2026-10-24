package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

func assertSourceQuarantined(t *testing.T, runtime *Runtime, sourceID string) {
	t.Helper()
	source, err := runtime.source(sourceID)
	if err != nil || source.CatalogState != domain.CatalogConflict || source.CatalogState.Selectable() {
		t.Fatalf("source %s: state=%q err=%v; want unselectable CatalogConflict", sourceID, source.CatalogState, err)
	}
	runtime.mu.RLock()
	catalog, status := runtime.catalogs[sourceID], runtime.statuses[sourceID]
	reason := runtime.quarantinedSources[sourceID]
	indexState := runtime.index.union.SourceStates[sourceID]
	runtime.mu.RUnlock()
	if catalog.QuarantineReason == "" || catalog.QuarantineReason != reason || status.Catalog != domain.CatalogConflict || indexState != domain.CatalogConflict {
		t.Fatalf("source %s quarantine is inconsistent: reason=%q saved_reason=%q status=%q index=%q", sourceID, reason, catalog.QuarantineReason, status.Catalog, indexState)
	}
}

func reopenRuntimeFromCache(t *testing.T, runtime *Runtime) *Runtime {
	t.Helper()
	restarted, err := NewRuntime(context.Background(), runtime.repo, RuntimeOptions{CommandPendingTTL: time.Minute})
	if err != nil {
		t.Fatalf("restart from persisted cache: %v", err)
	}
	return restarted
}

func TestTopologyQuarantineSurvivesRestartUntilValidRediscovery(t *testing.T) {
	ctx := context.Background()
	runtime, _ := deviceRuntime(t)
	status := connectedStatus("b", 1)
	status.Catalog = domain.CatalogConflict
	status.LastError = "invalid lower device topology: missing declared business point"
	runtime.OnLowerStatus(status)
	assertSourceQuarantined(t, runtime, "b")
	runtime.persistDirtyCatalogs(ctx)
	persisted, err := runtime.repo.LoadSourceCatalog(ctx, "b")
	if err != nil || persisted.State != domain.CatalogConflict || persisted.QuarantineReason != status.LastError || persisted.Topology == nil || len(persisted.Nodes) == 0 {
		t.Fatalf("persisted quarantine: catalog=%+v err=%v; want rejection plus the last valid cache", persisted, err)
	}
	source, _ := runtime.source("b")
	if _, err := domain.AssignCatalogGroups(source, persisted); err != nil {
		t.Fatalf("the preserved last successful manifest must be valid: %v", err)
	}

	// No Start: neither lower is connected or rediscovered after restart.
	restarted := reopenRuntimeFromCache(t, runtime)
	assertSourceQuarantined(t, restarted, "b")
	if healthy, err := restarted.source("a"); err != nil || !healthy.CatalogState.Selectable() {
		t.Fatalf("unrelated valid cached source a must remain selectable: state=%q err=%v", healthy.CatalogState, err)
	}
	const sessionID = "quarantine-restart"
	if err := restarted.openSession(sessionID); err != nil {
		t.Fatal(err)
	}
	if route, err := restarted.selectSource(sessionID, "device-one", "b", 1, "test"); !errors.Is(err, domain.ErrSourceNotSelectable) {
		t.Fatalf("select rejected source after restart: route=%+v err=%v; want %v", route, err, domain.ErrSourceNotSelectable)
	}

	// Merely reconnecting is insufficient. The actual validated catalog
	// publication must clear the reason and catalog atomically in SQLite.
	restarted.OnLowerStatus(connectedStatus("b", 2))
	assertSourceQuarantined(t, restarted, "b")
	restarted.OnCatalog(persisted)
	if _, err := restarted.selectSource(sessionID, "device-one", "b", 1, "test"); err != nil {
		t.Fatalf("valid rediscovery did not recover source b: %v", err)
	}
	restarted.persistDirtyCatalogs(ctx)
	recovered, err := restarted.repo.LoadSourceCatalog(ctx, "b")
	if err != nil || recovered.QuarantineReason != "" || recovered.State != domain.CatalogReady {
		t.Fatalf("successful discovery did not durably clear quarantine: reason=%q state=%q err=%v", recovered.QuarantineReason, recovered.State, err)
	}
	afterRecovery := reopenRuntimeFromCache(t, restarted)
	if source, err := afterRecovery.source("b"); err != nil || !source.CatalogState.Selectable() {
		t.Fatalf("cleared quarantine returned after another restart: state=%q err=%v", source.CatalogState, err)
	}
}

func TestStaleConflictStatusDoesNotQuarantineNewEpoch(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	delayedStatus := connectedStatus("b", 1)
	delayedStatus.Catalog = domain.CatalogConflict
	delayedStatus.LastError = "invalid lower device topology in old connection"
	runtime.OnLowerStatus(delayedStatus)
	runtime.OnLowerStatus(connectedStatus("b", 2))
	runtime.mu.RLock()
	validCatalog := runtime.catalogs["b"]
	runtime.mu.RUnlock()
	runtime.OnCatalog(validCatalog)
	runtime.OnSample("b", testKey("P1"), domain.Sample{Type: domain.DataTypeDouble, Value: float64(20), StatusCode: domain.StatusGood, Epoch: 2})
	const sessionID = "stale-epoch-session"
	if err := runtime.openSession(sessionID); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.selectSource(sessionID, "device-one", "b", 1, "test")
	if err != nil {
		t.Fatalf("select recovered source before late event: %v", err)
	}
	runtime.OnLowerStatus(delayedStatus)
	if source, err := runtime.source("b"); err != nil || source.CatalogState != domain.CatalogReady {
		t.Errorf("late epoch 1 Conflict changed epoch 2 source: state=%q err=%v", source.CatalogState, err)
	}
	runtime.mu.RLock()
	status, catalog := runtime.statuses["b"], runtime.catalogs["b"]
	runtime.mu.RUnlock()
	if status.Epoch != 2 || status.Catalog != domain.CatalogReady || catalog.QuarantineReason != "" {
		t.Errorf("late status changed recovered state: epoch=%d catalog=%q reason=%q", status.Epoch, status.Catalog, catalog.QuarantineReason)
	}
	after, err := runtime.routes.Route(sessionID, "device-one")
	if err != nil || after.SourceID != before.SourceID || after.Revision != before.Revision {
		t.Errorf("late status changed explicit selection: before=%+v after=%+v err=%v", before, after, err)
	}
	if sample := runtime.ReadSample(sessionID, testKey("P1")); sample.StatusCode != domain.StatusGood || sample.Value != float64(20) {
		t.Errorf("late status changed recovered data: sample=%+v; want Good value 20", sample)
	}
}

func TestConflictCallbackRechecksEpochGenerationAndClosingAfterWaiting(t *testing.T) {
	for _, advance := range []string{"epoch", "generation", "closing"} {
		t.Run(advance, func(t *testing.T) {
			runtime := seededRuntime(t)
			if err := runtime.openSession("session"); err != nil {
				t.Fatal(err)
			}
			before, err := runtime.selectSource("session", "line", "source-b", 1, "test")
			if err != nil {
				t.Fatal(err)
			}
			runtime.running.Store(true)
			defer runtime.running.Store(false)
			runtime.mu.Lock()
			runtime.connections.begin("source-b")
			runtime.statuses["source-b"] = connectedStatus("source-b", 1<<32|1)
			runtime.mu.Unlock()
			conflict := connectedStatus("source-b", 1<<32|1)
			conflict.Catalog = domain.CatalogConflict
			conflict.LastError = "late invalid manifest"

			// The callback is queued behind configuration work. Advance the
			// connection/closing state before letting it perform any mutation.
			runtime.configMu.Lock()
			entered, done := make(chan struct{}), make(chan struct{})
			go func() {
				close(entered)
				runtime.onLowerStatus(conflict, 1)
				close(done)
			}()
			<-entered
			runtime.mu.Lock()
			switch advance {
			case "epoch":
				runtime.statuses["source-b"] = connectedStatus("source-b", 1<<32|2)
			case "generation":
				runtime.connections.stop("source-b")
				runtime.statuses["source-b"] = connectedStatus("source-b", 2<<32|1)
			case "closing":
				runtime.closing.Store(true)
			}
			wantStatus := runtime.statuses["source-b"]
			runtime.mu.Unlock()
			runtime.configMu.Unlock()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("rejected callback did not return")
			}
			runtime.mu.RLock()
			catalog, status := runtime.catalogs["source-b"], runtime.statuses["source-b"]
			dirty := runtime.catalogPersistence.contains("source-b")
			runtime.mu.RUnlock()
			after, err := runtime.routes.Route("session", "line")
			if catalog.QuarantineReason != "" || catalog.State == domain.CatalogConflict || dirty || status.Epoch != wantStatus.Epoch || status.Catalog != wantStatus.Catalog || err != nil || after != before {
				t.Fatalf("late callback had side effects: catalog=%q reason=%q dirty=%t status=%+v route=%+v err=%v", catalog.State, catalog.QuarantineReason, dirty, status, after, err)
			}
		})
	}
}

func TestConcurrentOldConflictStatusesCannotClearCurrentSelection(t *testing.T) {
	runtime := seededRuntime(t)
	runtime.OnLowerStatus(connectedStatus("source-a", 2))
	if err := runtime.openSession("session"); err != nil {
		t.Fatal(err)
	}
	before, _ := runtime.routes.Route("session", "line")
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			old := connectedStatus("source-a", 1)
			old.Catalog, old.LastError = domain.CatalogConflict, "obsolete rejection"
			runtime.OnLowerStatus(old)
			runtime.OnLowerStatus(connectedStatus("source-a", 2))
		}()
	}
	workers.Wait()
	after, err := runtime.routes.Route("session", "line")
	if err != nil || after != before {
		t.Fatalf("concurrent stale statuses changed route: before=%+v after=%+v err=%v", before, after, err)
	}
	runtime.mu.RLock()
	reason := runtime.catalogs["source-a"].QuarantineReason
	runtime.mu.RUnlock()
	if reason != "" {
		t.Fatalf("stale rejection was retained: %q", reason)
	}
}

func TestDeletingPeerCannotClearExplicitCatalogQuarantine(t *testing.T) {
	ctx := context.Background()
	runtime := seededRuntime(t)
	mismatch := testNode("KKS001")
	mismatch.DataType = domain.DataTypeUInt32
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-b", GroupID: "line", Nodes: []domain.CatalogNode{mismatch}})
	status := connectedStatus("source-b", 1)
	status.Catalog, status.LastError = domain.CatalogConflict, "invalid lower device topology"
	runtime.OnLowerStatus(status)
	runtime.persistDirtyCatalogs(ctx)
	if _, err := runtime.SetDefaultSource(ctx, "line", "", 2); err != nil {
		t.Fatal(err)
	}
	if err := runtime.DeleteSource(ctx, "source-a", 1); err != nil {
		t.Fatal(err)
	}
	assertSourceQuarantined(t, runtime, "source-b")
	runtime.persistDirtyCatalogs(ctx)
	assertSourceQuarantined(t, reopenRuntimeFromCache(t, runtime), "source-b")
}

func TestEndpointResetKeepsQuarantineUntilValidRediscovery(t *testing.T) {
	ctx := context.Background()
	runtime := seededRuntime(t)
	status := connectedStatus("source-b", 1)
	status.Catalog, status.LastError = domain.CatalogConflict, "invalid lower device topology"
	runtime.OnLowerStatus(status)
	runtime.persistDirtyCatalogs(ctx)
	source, _ := runtime.source("source-b")
	_, err := runtime.UpdateSource(ctx, source.ID, source.Revision, api.SourceMutation{
		ID: source.ID, GroupID: source.GroupID, Name: source.Name, Enabled: true,
		Endpoint: "opc.tcp://replacement:4840", SecurityMode: "None", SecurityPolicy: "None",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSourceQuarantined(t, runtime, source.ID)
	// The source-update transaction itself must preserve an already durable
	// rejection, even before the retired in-memory catalog retry is flushed.
	assertSourceQuarantined(t, reopenRuntimeFromCache(t, runtime), source.ID)
}

func TestFactoryFailureDoesNotTurnPeerConflictIntoExplicitQuarantine(t *testing.T) {
	runtime := seededRuntime(t)
	mismatch := testNode("KKS001")
	mismatch.DataType = domain.DataTypeUInt32
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-b", GroupID: "line", Nodes: []domain.CatalogNode{mismatch}})
	source, _ := runtime.source("source-b")
	done := make(chan struct{})
	go func() {
		runtime.configMu.Lock()
		runtime.recordLowerStartFailure(source, errors.New("factory could not open certificate"))
		runtime.configMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("factory failure recursively locked the configuration gate")
	}
	runtime.mu.RLock()
	reason, status := runtime.catalogs[source.ID].QuarantineReason, runtime.statuses[source.ID]
	runtime.mu.RUnlock()
	if reason != "" || !strings.Contains(status.LastError, "factory could not") || status.Catalog != domain.CatalogConflict {
		t.Fatalf("connection failure changed catalog rejection semantics: reason=%q status=%+v", reason, status)
	}
}

func TestClosedRuntimeRejectsLateCatalogPreflightFailure(t *testing.T) {
	runtime := seededRuntime(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	runtime.mu.Lock()
	runtime.connections.begin("source-b")
	runtime.publication.configure(func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
		close(entered)
		<-release
		return errors.New("obsolete preflight rejection")
	}, nil)
	runtime.mu.Unlock()
	runtime.running.Store(true)
	defer runtime.running.Store(false)
	go func() {
		runtime.onCatalog("source-b", 1, domain.SourceCatalog{SourceID: "source-b", GroupID: "line", Nodes: []domain.CatalogNode{testNode("KKS001")}})
		close(done)
	}()
	<-entered
	runtime.mu.Lock()
	runtime.closing.Store(true)
	runtime.running.Store(false)
	runtime.connections.stop("source-b")
	runtime.mu.Unlock()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("late preflight callback did not return")
	}
	runtime.mu.RLock()
	catalog, reconcileError := runtime.catalogs["source-b"], runtime.upstreamCatalogErrorLocked()
	runtime.mu.RUnlock()
	if catalog.QuarantineReason != "" || catalog.State == domain.CatalogConflict || reconcileError != "" {
		t.Fatalf("late preflight failure mutated a closed runtime: reason=%q state=%q reconcile_error=%q", catalog.QuarantineReason, catalog.State, reconcileError)
	}
}
