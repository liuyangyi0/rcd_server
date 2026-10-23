package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

// Test fixtures install clients through the same reservation/attach boundary;
// tests may call this while holding Runtime.mu, just like production admission.
func installTestLower(r *Runtime, id string, handle lowerHandle) {
	generation, cancel := r.connections.begin(id)
	cancelLower(cancel)
	for generation < handle.generation {
		generation, _ = r.connections.begin(id)
	}
	if !r.connections.attach(id, generation, handle.client, handle.cancel) {
		panic("test lower attachment rejected")
	}
}

func TestSourceConnectionsFencesReplacementAndLateFinish(t *testing.T) {
	var s sourceConnections
	var cancels atomic.Int32
	one, previous := s.begin("a")
	if one == 0 || previous != nil || !s.attach("a", one, &fakeLowerClient{}, func() { cancels.Add(1) }) {
		t.Fatal("initial attachment failed")
	}
	two, previous := s.begin("a")
	if two <= one || s.current("a", one) || cancels.Load() != 0 {
		t.Fatal("replacement did not fence or canceled under lock")
	}
	cancelLower(previous)
	if cancels.Load() != 1 || !s.attach("a", two, &fakeLowerClient{}, nil) {
		t.Fatal("replacement failed")
	}
	if _, accepted := s.finish("a", one); accepted {
		t.Fatal("old finish accepted")
	}
	if h, ok := s.handle("a"); !ok || h.generation != two {
		t.Fatal("old worker removed replacement")
	}
	if s.attach("a", one, &fakeLowerClient{}, nil) || s.attach("a", two, &fakeLowerClient{}, nil) {
		t.Fatal("stale or duplicate attachment accepted")
	}
	terminal, ok := s.finish("a", two)
	if !ok || terminal <= two || s.current("a", two) || s.count() != 0 {
		t.Fatal("finished worker still current")
	}
	if _, ok := s.finish("a", two); ok {
		t.Fatal("duplicate finish accepted")
	}
}

func TestSourceConnectionsStopAllFencesStartingAndRecreatedIDs(t *testing.T) {
	var s sourceConnections
	starting, _ := s.begin("starting")
	attached, _ := s.begin("attached")
	var canceled bool
	s.attach("attached", attached, &fakeLowerClient{}, func() { canceled = true; s.count() })
	cancels := s.stopAll()
	if canceled || s.current("starting", starting) || s.current("attached", attached) || s.count() != 0 {
		t.Fatal("shutdown fence incomplete")
	}
	for _, cancel := range cancels {
		cancelLower(cancel)
	}
	if !canceled || s.attach("starting", starting, &fakeLowerClient{}, nil) {
		t.Fatal("late factory result attached after shutdown")
	}
	newGeneration, _ := s.begin("attached")
	if newGeneration <= attached || !s.attach("attached", newGeneration, &fakeLowerClient{}, nil) {
		t.Fatal("recreate failed")
	}
	if _, ok := s.finish("attached", attached); ok {
		t.Fatal("old generation revived across recreate")
	}
}

func TestSourceConnectionsConcurrentAccess(t *testing.T) {
	var s sourceConnections
	var wg sync.WaitGroup
	for task := 0; task < 4; task++ {
		wg.Add(1)
		go func(task int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				switch task {
				case 0:
					gen, cancel := s.begin("a")
					cancelLower(cancel)
					s.attach("a", gen, &fakeLowerClient{}, nil)
					s.finish("a", gen)
				case 1:
					cancelLower(s.stop("a"))
				case 2:
					s.handle("a")
					s.current("a", 1)
					s.count()
				case 3:
					for _, cancel := range s.stopAll() {
						cancelLower(cancel)
					}
				}
			}
		}(task)
	}
	wg.Wait()
}

type exitingLifecycleLower struct {
	fakeLowerClient
	run func(context.Context) error
}

func (c *exitingLifecycleLower) Run(ctx context.Context) error { return c.run(ctx) }

func prepareLifecycleRuntime(t *testing.T) (*Runtime, domain.Source) {
	t.Helper()
	r := seededRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.runContext, r.runCancel = ctx, cancel
	r.running.Store(true)
	t.Cleanup(func() { cancel(); r.running.Store(false); r.wg.Wait() })
	source, err := r.source("source-a")
	if err != nil {
		t.Fatal(err)
	}
	return r, source
}

func waitLifecycleWorkers(t *testing.T, r *Runtime) {
	t.Helper()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not finish")
	}
}

func TestRuntimeSupervisorExitMarksOfflineAndRejectsLateCallbacks(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil_return", true: "error_return"}[failed], func(t *testing.T) {
			r, source := prepareLifecycleRuntime(t)
			var sink opc.LowerSink
			var runContext context.Context
			r.options.LowerFactory = func(_ opc.LowerConfig, s opc.LowerSink) (opc.LowerClient, error) {
				sink = s
				return &exitingLifecycleLower{run: func(ctx context.Context) error {
					runContext = ctx
					s.OnLowerStatus(connectedStatus(source.ID, 7))
					s.OnSample(source.ID, testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 42.0, StatusCode: domain.StatusGood, Epoch: 7})
					if failed {
						return errors.New("run failed")
					}
					return nil
				}}, nil
			}
			r.configMu.Lock()
			err := r.startLower(source)
			r.configMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			waitLifecycleWorkers(t, r)
			status, _ := r.LowerStatus(source.ID)
			if status.Connection != opc.ConnectionDisconnected || status.Epoch != uint64(1)<<32|7 || status.LastError == "" {
				t.Fatal("exit status discarded as old epoch", status)
			}
			if runContext.Err() == nil {
				t.Fatal("finished supervisor context not released")
			}
			sink.OnLowerStatus(connectedStatus(source.ID, 8))
			sink.OnCatalog(deliveryTestCatalog(source.ID, "LATE_NODE"))
			sink.OnSample(source.ID, testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 99.0, StatusCode: domain.StatusGood, Epoch: 7})
			got, _ := r.LowerStatus(source.ID)
			if got.Connection != opc.ConnectionDisconnected || got.Epoch != status.Epoch {
				t.Fatal("late status revived exited supervisor", got)
			}
			if _, exists := r.CatalogUnion().Groups["line"]["LATE_NODE"]; exists {
				t.Fatal("late catalog was accepted")
			}
			if sample, _ := r.cache.Get(source.ID, testKey("KKS001")); sample.Value != 42.0 {
				t.Fatal("late sample overwrote last sample", sample)
			}
			if r.connections.count() != 0 {
				t.Fatal("exited handle retained")
			}
		})
	}
}

func TestRuntimeFactoryFailureFencesCapturedSink(t *testing.T) {
	for _, nilClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "nil_client"}[nilClient], func(t *testing.T) {
			r, source := prepareLifecycleRuntime(t)
			var sink opc.LowerSink
			r.options.LowerFactory = func(_ opc.LowerConfig, s opc.LowerSink) (opc.LowerClient, error) {
				sink = s
				if nilClient {
					return nil, nil
				}
				return nil, errors.New("factory failure")
			}
			r.configMu.Lock()
			err := r.startLower(source)
			r.configMu.Unlock()
			if err == nil {
				t.Fatal("factory failure hidden")
			}
			sink.OnLowerStatus(connectedStatus(source.ID, 1))
			sink.OnSample(source.ID, testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 99.0, Epoch: 1})
			status, _ := r.LowerStatus(source.ID)
			if status.Connection == opc.ConnectionConnected || r.connections.count() != 0 {
				t.Fatal("failed factory accepted callbacks", status)
			}
			if _, exists := r.cache.Get(source.ID, testKey("KKS001")); exists {
				t.Fatal("failed factory polluted sample cache")
			}
		})
	}
}

func TestRuntimeLowerCancellationRunsOutsideStateLock(t *testing.T) {
	r, source := prepareLifecycleRuntime(t)
	canceled := make(chan struct{})
	installTestLower(r, source.ID, lowerHandle{client: &fakeLowerClient{}, cancel: func() { r.mu.RLock(); r.mu.RUnlock(); close(canceled) }})
	done := make(chan struct{})
	go func() { r.stopLower(source.ID); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel ran under Runtime state lock")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("cancel not invoked")
	}
}

func TestRuntimeExitFromConflictedCatalogDoesNotCreateTopologyRejection(t *testing.T) {
	r, source := prepareLifecycleRuntime(t)
	r.mu.Lock()
	catalog := r.catalogs[source.ID]
	catalog.State = domain.CatalogConflict
	r.catalogs[source.ID] = catalog
	r.mu.Unlock()
	r.options.LowerFactory = func(_ opc.LowerConfig, sink opc.LowerSink) (opc.LowerClient, error) {
		return &exitingLifecycleLower{run: func(context.Context) error {
			sink.OnLowerStatus(connectedStatus(source.ID, 2))
			return errors.New("supervisor failed")
		}}, nil
	}
	r.configMu.Lock()
	err := r.startLower(source)
	r.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitLifecycleWorkers(t, r)
	r.mu.RLock()
	reason := r.catalogs[source.ID].QuarantineReason
	r.mu.RUnlock()
	status, _ := r.LowerStatus(source.ID)
	if reason != "" || !strings.Contains(status.LastError, "supervisor failed") || status.Connection != opc.ConnectionDisconnected {
		t.Fatal("transport error became topology quarantine", reason, status)
	}
}
