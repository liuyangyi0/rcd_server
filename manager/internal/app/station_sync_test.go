package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func TestStationSyncTicketsSurviveRemovalAndConcurrentCoalescing(t *testing.T) {
	var service stationSync
	old, ok := service.request("a")
	if !ok {
		t.Fatal("first reservation rejected")
	}
	service.remove("a")
	current, ok := service.request("a")
	if !ok || old == current || service.current(old) || service.finish(old) || !service.current(current) {
		t.Fatal("deleted identity was reused or old completion removed new work")
	}
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if ticket, enqueue := service.request("a"); enqueue || ticket != current {
				t.Error("duplicate request was not coalesced")
			}
			service.support("a", 1)
			service.persistenceError()
		}()
	}
	workers.Wait()
	if !service.finish(current) || service.current(current) || service.finish(current) {
		t.Fatal("expected exactly one follow-up refresh")
	}
}

func TestStationSyncQueueSaturationDoesNotStrandReservation(t *testing.T) {
	r := &Runtime{runContext: context.Background(), statuses: make(map[string]opc.LowerStatus)}
	r.running.Store(true)
	for i := 0; i < 65; i++ {
		id := fmt.Sprint(i)
		r.statuses[id] = connectedStatus(id, 1)
		r.queueStationVersionRefresh(id)
	}
	if len(r.stationSync.queue()) != 64 {
		t.Fatal("queue capacity changed")
	}
	<-r.stationSync.queue()
	r.queueStationVersionRefresh("64")
	if len(r.stationSync.queue()) != 64 {
		t.Fatal("dropped request kept a pending marker")
	}
	r.queueStationVersionRefresh("absent")
	if _, exists := r.stationSync.pending["absent"]; exists {
		t.Fatal("deleted source admitted")
	}
}

func TestStationSyncPersistenceHealthIsSourceScoped(t *testing.T) {
	var service stationSync
	service.persistenceResult("a", errors.New("disk full"))
	service.persistenceResult("b", errors.New("read only"))
	service.persistenceResult("b", nil)
	if got := service.persistenceError(); got != "a: disk full" || !service.needsPersistence("a") {
		t.Fatalf("unrelated success cleared failure: %q", got)
	}
	service.setSupport("a", 9, true)
	if known, _ := service.support("a", 10); known {
		t.Fatal("capability crossed epochs")
	}
	service.remove("a")
	if known, _ := service.support("a", 9); known || service.persistenceError() != "" {
		t.Fatal("removed source retained synchronization state")
	}
}

type stationCapabilityLower struct {
	operationLowerStub
	capability func(context.Context, uint64) (domain.OperationsCapabilities, error)
}

func (s *stationCapabilityLower) OperationsCapabilities(ctx context.Context, epoch uint64) (domain.OperationsCapabilities, error) {
	return s.capability(ctx, epoch)
}

func TestStationSyncRejectsLateCapabilityAcrossConnectionAndTopologyChanges(t *testing.T) {
	for _, change := range []string{"epoch", "topology", "generation", "offline", "deleted"} {
		t.Run(change, func(t *testing.T) {
			r := seededRuntime(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			lower := &stationCapabilityLower{capability: func(context.Context, uint64) (domain.OperationsCapabilities, error) {
				close(entered)
				<-release
				return domain.OperationsCapabilities{Supported: true, Features: []string{}}, nil
			}}
			r.mu.Lock()
			installTestLower(r, "source-a", lowerHandle{client: lower, generation: 3})
			r.statuses["source-a"] = connectedStatus("source-a", 3<<32|7)
			r.mu.Unlock()
			done := make(chan error, 1)
			go func() { done <- r.refreshSourceStationVersions(context.Background(), "source-a") }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("capability call not entered")
			}
			r.mu.Lock()
			status := r.statuses["source-a"]
			switch change {
			case "epoch":
				status.Epoch++
			case "topology":
				catalog := r.catalogs["source-a"]
				catalog.Topology = &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "new-port"}}}
				r.catalogs["source-a"] = catalog
			case "generation":
				installTestLower(r, "source-a", lowerHandle{client: lower, generation: 4})
			case "offline":
				status.Connection = opc.ConnectionDisconnected
			}
			r.statuses["source-a"] = status
			if change == "deleted" {
				delete(r.statuses, "source-a")
				r.stationSync.remove("source-a")
			} else {
				// A newer refresh has already established support.
				r.stationSync.setSupport("source-a", status.Epoch, true)
			}
			r.mu.Unlock()
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("refresh blocked")
			}
			known, supported := r.stationSync.support("source-a", status.Epoch)
			if change == "deleted" {
				if known {
					t.Fatal("late response recreated deleted capability")
				}
			} else if !known || !supported {
				t.Fatal("late unsupported response overwrote current capability")
			}
			if lower.calls.Load() != 0 {
				t.Fatal("stale capability triggered status call")
			}
		})
	}
}

func TestStationSyncRetriesIdenticalSnapshotAfterStorageRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	repository, err := store.Open(ctx, path, store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateGroup(ctx, domain.Group{ID: "g", Name: "g"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := repository.CreateSource(ctx, domain.Source{ID: id, GroupID: "g", Name: id,
			Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewRuntime(ctx, repository, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	topology := &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "COM1"}}}
	fingerprint := domain.SourceTopologyFingerprint(topology)
	for _, id := range []string{"a", "b"} {
		r.catalogs[id] = domain.SourceCatalog{SourceID: id, Topology: topology}
		r.statuses[id] = connectedStatus(id, 1)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(statement string) {
		t.Helper()
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE station_test_writes (source_id TEXT)`)
	exec(`CREATE TRIGGER count_station_writes AFTER INSERT ON source_station_version_state BEGIN INSERT INTO station_test_writes VALUES(NEW.source_id); END`)
	exec(`CREATE TRIGGER count_station_updates AFTER UPDATE ON source_station_version_state BEGIN INSERT INTO station_test_writes VALUES(NEW.source_id); END`)
	exec(`CREATE TRIGGER reject_station_write BEFORE INSERT ON source_station_version_state WHEN NEW.source_id='a' BEGIN SELECT RAISE(FAIL,'injected station storage failure'); END`)
	cache := func(id string, open bool) error {
		return r.cacheStationVersionResult(domain.OperationRequest{Action: "status"}, stationStatusResult(t, 1, open), domain.Source{ID: id}, 1, fingerprint)
	}
	if err := cache("a", true); err == nil {
		t.Fatal("injected storage failure not reported")
	}
	if snapshot, ok := r.StationVersions("a"); !ok || snapshot.StationRevision != "1" {
		t.Fatal("storage failure suppressed live snapshot")
	}
	if err := cache("b", true); err != nil {
		t.Fatal(err)
	}
	if !r.stationSync.needsPersistence("a") || !strings.Contains(r.stationSync.persistenceError(), "a:") {
		t.Fatal("other source masked failure")
	}
	exec(`DROP TRIGGER reject_station_write`)
	// Same station revision and same content: this must retry, not deduplicate.
	if err := cache("a", true); err != nil {
		t.Fatal(err)
	}
	if persisted, err := repository.LoadSourceStationVersions(ctx, "a"); err != nil || persisted.StationRevision != "1" {
		t.Fatalf("retry not persisted: %+v %v", persisted, err)
	}
	if r.stationSync.persistenceError() != "" {
		t.Fatal("recovered failure remains visible")
	}
	for i := 0; i < 5; i++ {
		if err := cache("a", i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM station_test_writes WHERE source_id='a'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runtime health was persisted repeatedly: %d %v", count, err)
	}
}

func TestStationSyncSourceAndGroupDeletionClearAllStationState(t *testing.T) {
	for _, cascade := range []bool{false, true} {
		t.Run(fmt.Sprint(cascade), func(t *testing.T) {
			r := seededRuntime(t)
			id := "source-b" // not the default source
			r.stations.replace(id, domain.SourceStationVersions{SourceID: id}, 4, true)
			r.stationSync.setSupport(id, 4, true)
			r.stationSync.persistenceResult(id, errors.New("old error"))
			old, _ := r.stationSync.request(id)
			var err error
			if cascade {
				group := r.routes.Groups()[0]
				err = r.DeleteGroup(context.Background(), group.ID, group.Revision)
			} else {
				source, _ := r.source(id)
				err = r.DeleteSource(context.Background(), id, source.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := r.StationVersions(id); exists {
				t.Fatal("deleted source retained station snapshot")
			}
			if _, _, live, _ := r.stations.read(id); live {
				t.Fatal("deleted source retained snapshot epoch")
			}
			if known, _ := r.stationSync.support(id, 4); known || r.stationSync.needsPersistence(id) || r.stationSync.current(old) {
				t.Fatal("deleted source retained synchronization state")
			}
		})
	}
}
