package app

import (
	"sync"
	"time"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

// calculationService owns every mutable calculation object, including Runner.
// mu protects short state snapshots; worker serializes evaluation without
// blocking readers. No Runtime/config lock is held during expression evaluation.
// A generation fence discards results computed against replaced definitions,
// routes or invalidated source/catalog state. It never performs persistence or
// protocol callbacks while holding mu.
type calculationService struct {
	mu                                    sync.RWMutex
	worker                                sync.Mutex
	plan                                  *calculation.Plan
	version, pending                      calculation.Version
	runner                                *calculation.Runner
	results, retired                      map[string]calculation.Result
	retiredPoints                         map[string]indexedPoint
	dirty                                 map[string]struct{}
	stateSeen                             map[string]string
	stateEventPending, full, eventPending bool
	routeRevision, generation             uint64
	lastEvaluationAt, eventAt             time.Time
	lastDuration                          time.Duration
	lastError                             string
	wake                                  chan struct{}
}

type calculationSnapshot struct {
	plan             *calculation.Plan
	version          calculation.Version
	results, retired map[string]calculation.Result
	retiredPoints    map[string]indexedPoint
}

func newCalculationService(plan *calculation.Plan, version, pending calculation.Version) *calculationService {
	s := &calculationService{wake: make(chan struct{}, 1)}
	s.install(plan, version, nil, nil)
	s.pending = pending
	s.stateEventPending = false
	return s
}

func (s *calculationService) definition() (*calculation.Plan, calculation.Version) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.plan, s.version
}

func (s *calculationService) catalogDefinition() (*calculation.Plan, map[string]indexedPoint) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.plan, cloneIndexedPoints(s.retiredPoints)
}

func (s *calculationService) snapshot() calculationSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return calculationSnapshot{s.plan, s.version, cloneCalculationResults(s.results), cloneCalculationResults(s.retired), cloneIndexedPoints(s.retiredPoints)}
}

func (s *calculationService) setPending(version calculation.Version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = version
}

func (s *calculationService) install(plan *calculation.Plan, version calculation.Version, retired map[string]calculation.Result, points map[string]indexedPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	s.plan, s.version, s.pending = plan, version, calculation.Version{}
	s.runner = nil
	if plan != nil {
		s.runner = calculation.NewRunner(plan)
		if s.routeRevision == 0 {
			s.routeRevision = 1
		}
	}
	s.results = make(map[string]calculation.Result)
	s.retired, s.retiredPoints = cloneCalculationResults(retired), cloneIndexedPoints(points)
	s.dirty, s.stateSeen = make(map[string]struct{}), make(map[string]string)
	s.stateEventPending, s.full, s.eventPending = true, plan != nil, false
	s.lastError = ""
}

func (s *calculationService) status() calculation.Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statusLocked()
}

func (s *calculationService) metadata() (calculation.Status, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	if s.plan != nil {
		names = s.plan.OutputNames()
	}
	return s.statusLocked(), names
}

func (s *calculationService) statusLocked() calculation.Status {
	status := calculation.Status{RoutingMode: "DefaultSources", ActiveRevision: s.version.Revision,
		DefaultRouteRevision: s.routeRevision, LastEvaluationAt: s.lastEvaluationAt,
		LastEvaluationDurationMS: float64(s.lastDuration) / float64(time.Millisecond), LastError: s.lastError,
		PendingRestart: s.pending.Revision != 0, PendingRevision: s.pending.Revision, PendingHash: s.pending.Hash}
	if s.plan != nil {
		status.ConfigHash, status.RuleCount = s.plan.Hash(), s.plan.RuleCount()
	}
	for _, result := range s.results {
		if result.StatusCode.IsGood() {
			status.GoodCount++
		} else {
			status.BadCount++
		}
	}
	return status
}

func (s *calculationService) resultList() []calculation.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]calculation.Result, 0, len(s.results)+len(s.retired))
	for _, value := range s.results {
		items = append(items, cloneCalculationResult(value))
	}
	for _, value := range s.retired {
		items = append(items, cloneCalculationResult(value))
	}
	return items
}

func (s *calculationService) result(identifier string) (calculation.Result, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, exists := s.results[identifier]
	if !exists {
		result, exists = s.retired[identifier]
	}
	return cloneCalculationResult(result), exists
}

func (s *calculationService) sample(identifier string) (domain.Sample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, exists := s.results[identifier]
	if !exists {
		result, exists = s.retired[identifier]
	}
	if exists {
		return result.Sample(), true
	}
	if s.plan != nil {
		if dataType, exists := s.plan.OutputDataType(identifier); exists {
			return domain.Sample{Type: dataType, Value: zeroValue(dataType, -1), StatusCode: domain.StatusBadWaitingInitialData}, true
		}
	}
	return domain.Sample{}, false
}

func (s *calculationService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *calculationService) invalidate(routeChanged bool) {
	s.mu.Lock()
	if s.plan != nil {
		s.generation++
		s.full = true
		if routeChanged {
			s.routeRevision++
			if s.routeRevision == 0 {
				s.routeRevision = 1
			}
		}
	}
	s.mu.Unlock()
	s.signal()
}

func (s *calculationService) inputChanged(identifier string) {
	s.mu.Lock()
	if s.plan != nil && s.plan.HasBaseVariable(identifier) {
		s.dirty[identifier] = struct{}{}
	}
	s.mu.Unlock()
	s.signal()
}

func (s *calculationService) stateChanged(plan *calculation.Plan, name, signature string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan != plan {
		return false
	}
	if s.stateSeen[name] != signature {
		s.stateSeen[name] = signature
		s.stateEventPending = true
		if plan.HasBaseVariable(name) {
			s.dirty[name] = struct{}{}
		}
		// Unreferenced state inputs are public OPC outputs too. Their
		// subscribers must be refreshed even when no formula uses the name.
		return true
	}
	return false
}

func (s *calculationService) takeStateEvent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.stateEventPending
	s.stateEventPending = false
	return pending
}

func (s *calculationService) pendingWork() (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.dirty), s.stateEventPending
}

// resolve snapshots the selected inputs under Runtime's short configuration
// read transaction. Computation starts only after resolve releases that lock.
func (s *calculationService) evaluate(resolve func(*calculation.Plan, []string) (map[string]calculation.Input, bool)) ([]calculation.Result, bool, uint64) {
	s.worker.Lock()
	defer s.worker.Unlock()
	s.mu.Lock()
	plan, runner, version, route, generation := s.plan, s.runner, s.version.Revision, s.routeRevision, s.generation
	full := s.full
	if plan == nil || runner == nil || (!full && len(s.dirty) == 0) {
		s.mu.Unlock()
		return nil, false, version
	}
	dirty := make([]string, 0, len(s.dirty))
	for name := range s.dirty {
		dirty = append(dirty, name)
	}
	s.full, s.dirty = false, make(map[string]struct{})
	s.mu.Unlock()
	if full {
		dirty = plan.BaseVariables()
	}
	inputs, valid := resolve(plan, dirty)
	if !valid {
		s.mu.Lock()
		if s.generation == generation {
			s.full = true
		}
		s.mu.Unlock()
		s.signal()
		return nil, false, version
	}
	started := time.Now()
	var changed []calculation.Result
	if full {
		changed = runner.Reset(inputs, version, route)
	} else {
		changed = runner.Update(inputs, version, route)
	}
	duration, now := time.Since(started), time.Now().UTC()
	// Runner state is worker-only. Publish an independent snapshot; readers
	// never touch the mutable runner, and stale generations never leak results.
	results := runner.Results()
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return nil, false, version
	}
	for _, result := range results {
		s.results[result.NodeID] = result
	}
	s.lastEvaluationAt, s.lastDuration, s.lastError = now, duration, ""
	publish := len(changed) != 0 && now.Sub(s.eventAt) >= time.Second
	if len(changed) != 0 {
		s.eventPending = !publish
		if publish {
			s.eventAt = now
		}
	}
	for i := range changed {
		changed[i] = cloneCalculationResult(changed[i])
	}
	return changed, publish, version
}

func (s *calculationService) takeResultEvent() (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.eventPending || time.Since(s.eventAt) < time.Second {
		return 0, false
	}
	s.eventPending, s.eventAt = false, time.Now().UTC()
	return s.version.Revision, true
}

// Caller holds configMu (and may hold Runtime.mu). The service never acquires
// either Runtime lock, keeping catalog/calculation publication order explicit.
func (r *Runtime) buildCatalogWithCalculations(catalogs map[string]domain.SourceCatalog, groups []domain.Group) (*catalogIndex, map[string]domain.CatalogState, int, error) {
	plan, retired := r.calculations.catalogDefinition()
	return buildCatalogIndexWithCalculations(catalogs, plan, retired, groups)
}
