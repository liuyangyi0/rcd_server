package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func deliveryTestCatalog(id, node string) domain.SourceCatalog {
	return domain.SourceCatalog{SourceID: id, GroupID: "line", DiscoveredAt: time.Unix(100, 0), Nodes: []domain.CatalogNode{testNode(node)}}
}

func TestCatalogPersistenceOwnsQueuedAndWriterSnapshots(t *testing.T) {
	calls := 0
	s := newCatalogPersistence(nil, func(_ context.Context, c domain.SourceCatalog) error {
		calls++
		if c.Nodes[0].BrowseName != "original" {
			t.Fatal("pending snapshot changed", c)
		}
		c.Nodes[0].BrowseName = "writer mutation"
		if calls == 1 {
			return errors.New("disk busy")
		}
		return nil
	})
	catalog := deliveryTestCatalog("a", "original")
	s.queue(catalog)
	catalog.Nodes[0].BrowseName = "caller mutation"
	s.flush(context.Background())
	if count, message := s.status(); count != 1 || message != "disk busy" {
		t.Fatal(count, message)
	}
	s.flush(context.Background())
	if count, message := s.status(); count != 0 || message != "" || calls != 2 {
		t.Fatal(count, message, calls)
	}
}

func TestCatalogPersistenceOldWriteCannotClearSameTimestampReplacement(t *testing.T) {
	for _, oldError := range []bool{false, true} {
		for _, recreated := range []bool{false, true} {
			t.Run(map[bool]string{false: "success", true: "failure"}[oldError]+map[bool]string{false: "/replace", true: "/recreate"}[recreated], func(t *testing.T) {
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var written []string
				s := newCatalogPersistence(nil, func(_ context.Context, c domain.SourceCatalog) error {
					written = append(written, c.Nodes[0].BrowseName)
					if len(written) == 1 {
						close(entered)
						<-release
						if oldError {
							return errors.New("old failure")
						}
					}
					return nil
				})
				s.queue(deliveryTestCatalog("a", "old"))
				go func() { s.flush(context.Background()); close(done) }()
				<-entered
				// Queue and diagnostics must not wait on the slow writer's I/O.
				updated := make(chan struct{})
				go func() {
					if recreated {
						s.discard("a")
					}
					s.queue(deliveryTestCatalog("a", "new"))
					s.status()
					close(updated)
				}()
				select {
				case <-updated:
				case <-time.After(time.Second):
					close(release)
					<-done
					t.Fatal("state lock held across database write")
				}
				close(release)
				<-done
				if count, message := s.status(); count != 1 || message != "" {
					t.Fatal("stale completion changed replacement", count, message)
				}
				s.flush(context.Background())
				if count, _ := s.status(); count != 0 || len(written) != 2 || written[1] != "new" {
					t.Fatal(count, written)
				}
			})
		}
	}
}

func TestCatalogPersistenceErrorsAreSourceScopedAndCancelKeepsPending(t *testing.T) {
	failA, failB := true, true
	calls := 0
	s := newCatalogPersistence(nil, func(_ context.Context, c domain.SourceCatalog) error {
		calls++
		if c.SourceID == "a" && failA || c.SourceID == "b" && failB {
			return errors.New("failed " + c.SourceID)
		}
		return nil
	})
	s.queue(deliveryTestCatalog("a", "A"))
	s.queue(deliveryTestCatalog("b", "B"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.flush(ctx)
	if calls != 0 {
		t.Fatal("cancelled flush performed writes")
	}
	s.flush(context.Background())
	if count, message := s.status(); count != 2 || message != "failed b" {
		t.Fatal(count, message)
	}
	failB = false
	s.flush(context.Background())
	if count, message := s.status(); count != 1 || message != "failed a" {
		t.Fatal("other source success hid failure", count, message)
	}
	failA = false
	s.flush(context.Background())
	if count, message := s.status(); count != 0 || message != "" {
		t.Fatal(count, message)
	}
}

func TestCatalogPersistenceSerializesWriters(t *testing.T) {
	var active, peak atomic.Int32
	s := newCatalogPersistence(nil, func(_ context.Context, c domain.SourceCatalog) error {
		current := active.Add(1)
		if current > 1 {
			peak.Store(current)
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return nil
	})
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := s.save(context.Background(), deliveryTestCatalog(id, "n")); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if peak.Load() > 1 {
		t.Fatal("concurrent database writes")
	}
	if count, _ := s.status(); count != 0 {
		t.Fatal("writes left pending", count)
	}
}

func TestCatalogPublicationFailureAndHandlerGeneration(t *testing.T) {
	var s catalogPublication
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	old := s.configure(nil, func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
		close(entered)
		<-release
		return errors.New("obsolete failure")
	})
	go func() { _ = s.publish(domain.CatalogUnion{}, nil, nil); close(done) }()
	<-entered
	s.configure(nil, func(domain.CatalogUnion, []domain.Group, []domain.Source) error { return errors.New("current failure") })
	s.validationFailed(old, errors.New("obsolete validation"))
	if dirty, message := s.status(); !dirty || message != "" {
		t.Fatal("old validation changed new handlers", dirty, message)
	}
	close(release)
	<-done
	if dirty, message := s.status(); !dirty || message != "" {
		t.Fatal("old publication changed new handlers", dirty, message)
	}
	if err := s.publish(domain.CatalogUnion{}, nil, nil); err == nil {
		t.Fatal("failure hidden")
	}
	if dirty, message := s.status(); !dirty || message != "current failure" {
		t.Fatal(dirty, message)
	}
	s.configure(nil, func(domain.CatalogUnion, []domain.Group, []domain.Source) error { return nil })
	if err := s.publish(domain.CatalogUnion{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if dirty, message := s.status(); dirty || message != "" {
		t.Fatal(dirty, message)
	}
}

func TestRuntimeInitialCatalogValidationFailureDoesNotRetryPublication(t *testing.T) {
	r := seededRuntime(t)
	calls := 0
	err := r.SetCatalogHandlers(func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
		return errors.New("invalid initial catalog")
	},
		func(domain.CatalogUnion, []domain.Group, []domain.Source) error { calls++; return nil })
	if err == nil {
		t.Fatal("preflight failure hidden")
	}
	r.retryDirtyUpstream()
	if dirty, message := r.publication.status(); dirty || message != "invalid initial catalog" || calls != 0 {
		t.Fatal(dirty, message, calls)
	}
}

func TestRuntimeCatalogHandlerRegistrationCannotPublishStaleStartupSnapshot(t *testing.T) {
	r := seededRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	installed, discovered := make(chan error, 1), make(chan struct{})
	var first sync.Once
	var published []bool
	go func() {
		installed <- r.SetCatalogHandlers(func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
			first.Do(func() { close(entered); <-release })
			return nil
		}, func(union domain.CatalogUnion, _ []domain.Group, _ []domain.Source) error {
			_, exists := union.Groups["line"]["NEW_POINT"]
			published = append(published, exists)
			return nil
		})
	}()
	<-entered
	go func() { r.OnCatalog(deliveryTestCatalog("source-b", "NEW_POINT")); close(discovered) }()
	select {
	case <-discovered:
		close(release)
		<-installed
		t.Fatal("hot discovery bypassed initial publication transaction")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-installed; err != nil {
		t.Fatal(err)
	}
	<-discovered
	if len(published) != 2 || published[0] || !published[1] {
		t.Fatal("startup snapshot overwrote hot discovery", published)
	}
}

func TestRuntimeCatalogValidationCannotMutatePublicationInput(t *testing.T) {
	r := seededRuntime(t)
	err := r.SetCatalogHandlers(func(union domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
		delete(union.Groups["line"], "KKS001")
		groups[0].Name = "mutated"
		sources[0].Name = "mutated"
		return nil
	}, func(union domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
		if _, exists := union.Groups["line"]["KKS001"]; !exists || groups[0].Name == "mutated" || sources[0].Name == "mutated" {
			t.Fatal("validation mutated later publication")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := r.CatalogUnion().Groups["line"]["KKS001"]; !exists {
		t.Fatal("validation mutated runtime index")
	}
}

func TestRuntimeCatalogPublicationHealthSeparatesQuarantineAndDelivery(t *testing.T) {
	r := seededRuntime(t)
	fail := false
	if err := r.SetCatalogHandlers(nil, func(domain.CatalogUnion, []domain.Group, []domain.Source) error {
		if fail {
			return errors.New("delivery failed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.quarantinedSources["source-b"] = "bad shape"
	r.mu.Unlock()
	if got := r.Health(context.Background()).Checks["upstream_catalog"]; got != "bad shape" {
		t.Fatal(got)
	}
	fail = true
	r.configMu.Lock()
	_ = r.reconcileUpstream()
	r.configMu.Unlock()
	if got := r.Health(context.Background()).Checks["upstream_catalog"]; got != "delivery failed" {
		t.Fatal(got)
	}
	fail = false
	r.retryDirtyUpstream()
	if got := r.Health(context.Background()).Checks["upstream_catalog"]; got != "bad shape" {
		t.Fatal("successful delivery hid quarantine", got)
	}
	r.mu.Lock()
	delete(r.quarantinedSources, "source-b")
	r.mu.Unlock()
	if got := r.Health(context.Background()).Checks["upstream_catalog"]; got != "ok" {
		t.Fatal("resolved errors remain sticky", got)
	}
}

func TestRuntimeCalculationDeactivationPublicationFailureRetries(t *testing.T) {
	r := seededRuntime(t)
	lower, err := r.repo.LoadSourceCatalog(context.Background(), "source-a")
	if err != nil {
		t.Fatal(err)
	}
	node := testNode("CENTRAL")
	node.Kind = "calculation"
	lower.Nodes = append(lower.Nodes, node)
	lower.Topology = &domain.SourceTopology{SchemaVersion: 1,
		Ports: []domain.TopologyPort{{PortID: "COM1", PortName: "COM1", SendNodeID: "COM1_Send", SendMethodID: "COM1_SendCommand",
			Devices: []domain.TopologyDevice{{DeviceID: "1", DeviceAddress: 1, NodeIDs: []string{"KKS001"}}}}},
		Calculations: []domain.TopologyCalculation{{NodeID: "CENTRAL", DependencyNodeIDs: []string{"KKS001"}, Owners: []domain.TopologyOwner{{PortID: "COM1", DeviceID: "1"}}}},
	}
	r.OnCatalog(lower)
	version := activateTestCalculation(t, r, "[[rules]]\nkks_calc = \"CENTRAL\"\ndata_type = \"double\"\nexpression = \"KKS001 + 1\"\n")
	fail := false
	calls := 0
	var lastKind string
	if err := r.SetCatalogHandlers(nil, func(union domain.CatalogUnion, _ []domain.Group, _ []domain.Source) error {
		calls++
		lastKind = union.Groups["line"]["CENTRAL"].Kind
		if fail {
			return errors.New("temporary deactivation publication failure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := r.DeactivateCalculation(context.Background(), version.Revision); err == nil {
		t.Fatal("publication failure hidden")
	}
	if plan, _ := r.calculations.definition(); plan != nil {
		t.Fatal("deactivation was not committed")
	}
	if dirty, message := r.publication.status(); !dirty || message == "" {
		t.Fatal("deactivation failure not scheduled for retry", dirty, message)
	}
	fail = false
	r.retryDirtyUpstream()
	if dirty, message := r.publication.status(); dirty || message != "" || calls != 3 || lastKind != "calculation" {
		t.Fatal("retry did not deliver lower calculation catalog", dirty, message, calls, lastKind)
	}
}
