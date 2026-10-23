package app

import (
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

func newTestCalculationService() *calculationService {
	return newCalculationService(nil, calculation.Version{}, calculation.Version{})
}

func servicePlan(t *testing.T, expression string) *calculation.Plan {
	t.Helper()
	cfg, err := calculation.ParseConfig("[[rules]]\nkks_calc = \"OUT\"\ndata_type = \"double\"\nexpression = \"" + expression + "\"\n")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := calculation.Compile(cfg, map[string]calculation.InputDefinition{"X": {DataType: domain.DataTypeDouble, ValueRank: -1}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func serviceInput(value float64) map[string]calculation.Input {
	return map[string]calculation.Input{"X": {Type: domain.DataTypeDouble, Value: value, StatusCode: domain.StatusGood}}
}

func TestCalculationServiceDiscardsInvalidatedBatchWithoutBlockingReaders(t *testing.T) {
	for _, change := range []string{"route", "catalog", "activate", "deactivate"} {
		t.Run(change, func(t *testing.T) {
			s := newCalculationService(servicePlan(t, "X * 2"), calculation.Version{Revision: 1}, calculation.Version{})
			entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan []calculation.Result, 1)
			go func() {
				changed, _, _ := s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) {
					close(entered)
					<-resume
					return serviceInput(4), true
				})
				finished <- changed
			}()
			<-entered
			read := make(chan struct{})
			go func() { _ = s.status(); _, _ = s.sample("OUT"); close(read) }()
			select {
			case <-read:
			case <-time.After(time.Second):
				close(resume)
				t.Fatal("snapshot reader blocked by evaluation")
			}
			switch change {
			case "route":
				s.invalidate(true)
			case "catalog":
				s.invalidate(false)
			case "activate":
				s.install(servicePlan(t, "X + 3"), calculation.Version{Revision: 2}, nil, nil)
			case "deactivate":
				s.install(nil, calculation.Version{}, nil, nil)
			}
			close(resume)
			if changed := <-finished; len(changed) != 0 {
				t.Fatal("stale batch was published", changed)
			}
			if _, exists := s.result("OUT"); exists {
				t.Fatal("stale result entered cache")
			}
			s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) { return serviceInput(10), true })
			result, exists := s.result("OUT")
			if change == "deactivate" {
				if exists {
					t.Fatal("deactivated output resurrected")
				}
				return
			}
			want := float64(20)
			if change == "activate" {
				want = 13
			}
			if !exists || result.Value != want {
				t.Fatalf("replacement batch = %+v", result)
			}
			if change == "route" && result.DefaultRouteRevision != 2 {
				t.Fatal("route generation missing", result)
			}
		})
	}
}

func TestCalculationServiceDoesNotLoseDirtyInputsDuringBatch(t *testing.T) {
	s := newCalculationService(servicePlan(t, "X * 2"), calculation.Version{Revision: 1}, calculation.Version{})
	s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) {
		s.inputChanged("X") // arrives after batch claimed its input snapshot
		return serviceInput(1), true
	})
	if dirty, _ := s.pendingWork(); dirty != 1 {
		t.Fatal("input update lost", dirty)
	}
	s.evaluate(func(_ *calculation.Plan, names []string) (map[string]calculation.Input, bool) {
		if len(names) != 1 || names[0] != "X" {
			t.Fatal(names)
		}
		return serviceInput(7), true
	})
	result, _ := s.result("OUT")
	if result.Value != float64(14) {
		t.Fatal(result)
	}
}

func TestCalculationServiceFailedSnapshotRequeuesFullBatch(t *testing.T) {
	s := newCalculationService(servicePlan(t, "X * 2"), calculation.Version{Revision: 1}, calculation.Version{})
	s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) { return nil, false })
	changed, _, _ := s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) { return serviceInput(2), true })
	if len(changed) != 1 || changed[0].Value != float64(4) {
		t.Fatal(changed)
	}
}

func TestCalculationServiceOwnsReturnedCollections(t *testing.T) {
	s := newCalculationService(servicePlan(t, "X * 2"), calculation.Version{Revision: 1}, calculation.Version{})
	changed, _, _ := s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) {
		inputs := serviceInput(0)
		input := inputs["X"]
		input.StatusCode = domain.StatusBadNoCommunication
		inputs["X"] = input
		return inputs, true
	})
	if len(changed) != 1 || len(changed[0].CommunicationInputs) != 1 {
		t.Fatal(changed)
	}
	changed[0].CommunicationInputs[0] = "changed-result-corruption"
	items := s.resultList()
	if len(items) != 1 || len(items[0].CommunicationInputs) != 1 {
		t.Fatal(items)
	}
	items[0].CommunicationInputs[0] = "corrupted"
	snapshot := s.snapshot()
	item := snapshot.results["OUT"]
	item.CommunicationInputs[0] = "also corrupted"
	actual, _ := s.result("OUT")
	if actual.CommunicationInputs[0] != "X" {
		t.Fatal("mutable slice exposed", actual)
	}
}

func TestCalculationServiceSerializesRunnerAndConcurrentReaders(t *testing.T) {
	s := newCalculationService(servicePlan(t, "X * 2"), calculation.Version{Revision: 1}, calculation.Version{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s.inputChanged("X")
				s.evaluate(func(*calculation.Plan, []string) (map[string]calculation.Input, bool) { return serviceInput(5), true })
				_ = s.status()
				_ = s.resultList()
				_, _ = s.sample("OUT")
			}
		}()
	}
	wg.Wait()
	actual, _ := s.result("OUT")
	if actual.Value != float64(10) {
		t.Fatal(actual)
	}
}
