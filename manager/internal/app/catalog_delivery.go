package app

import (
	"context"
	"sync"

	"rcd_manager/internal/domain"
)

type catalogWrite struct {
	catalog   domain.SourceCatalog
	version   uint64
	errorText string
}

// catalogPersistence owns pending snapshots, not authoritative catalog state.
// Runtime's configuration transaction still orders source deletion/replacement
// with database writes. The component never calls I/O under its state mutex.
type catalogPersistence struct {
	mu       sync.RWMutex
	writerMu sync.Mutex
	sequence uint64
	pending  map[string]catalogWrite
	write    func(context.Context, domain.SourceCatalog) error
}

func newCatalogPersistence(initial map[string]domain.SourceCatalog, write func(context.Context, domain.SourceCatalog) error) *catalogPersistence {
	s := &catalogPersistence{pending: map[string]catalogWrite{}, write: write}
	for _, catalog := range initial {
		s.queue(catalog)
	}
	return s
}

func (s *catalogPersistence) queue(catalog domain.SourceCatalog) uint64 {
	owned := catalog.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	previous := s.pending[catalog.SourceID]
	s.pending[catalog.SourceID] = catalogWrite{catalog: owned, version: s.sequence, errorText: previous.errorText}
	return s.sequence
}

func (s *catalogPersistence) discard(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

func (s *catalogPersistence) contains(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pending[id]
	return ok
}

func (s *catalogPersistence) status() (int, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest uint64
	var message string
	for _, entry := range s.pending {
		if entry.errorText != "" && entry.version >= latest {
			latest, message = entry.version, entry.errorText
		}
	}
	return len(s.pending), message
}

func (s *catalogPersistence) save(ctx context.Context, catalog domain.SourceCatalog) error {
	s.queue(catalog)
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	return s.persistSource(ctx, catalog.SourceID)
}

func (s *catalogPersistence) flush(ctx context.Context) {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	s.mu.RLock()
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		_ = s.persistSource(ctx, id)
	}
}

// writerMu serializes actual database writes. A replaced snapshot stays pending
// even when DiscoveredAt is unchanged; timestamps are not revision tokens.
func (s *catalogPersistence) persistSource(ctx context.Context, id string) error {
	s.mu.RLock()
	entry, ok := s.pending[id]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	err := s.write(ctx, entry.catalog.Clone())
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, exists := s.pending[id]; exists && current.version == entry.version {
		if err == nil {
			delete(s.pending, id)
		} else {
			current.errorText = err.Error()
			s.pending[id] = current
		}
	}
	return err
}

// catalogPublication owns callback registration and delivery health/retry state.
// Runtime configMu orders catalog/route commits and all production callbacks.
// applyMu additionally serializes callbacks, while mu remains a short state lock.
type catalogPublication struct {
	mu                    sync.RWMutex
	applyMu               sync.Mutex
	validator, reconciler CatalogPublisher
	generation            uint64
	dirty                 bool
	lastError             string
}

func (s *catalogPublication) configure(validator, reconciler CatalogPublisher) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validator, s.reconciler = validator, reconciler
	s.generation++
	s.dirty, s.lastError = reconciler != nil, ""
	return s.generation
}

func (s *catalogPublication) validate(union domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
	s.mu.RLock()
	validator := s.validator
	s.mu.RUnlock()
	if validator == nil {
		return nil
	}
	return validator(union, groups, sources)
}

func (s *catalogPublication) validationFailed(generation uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return
	}
	s.dirty = false // invalid initial snapshots must not enter publish retries
	s.lastError = err.Error()
}

func (s *catalogPublication) publish(union domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.RLock()
	handler, generation := s.reconciler, s.generation
	s.mu.RUnlock()
	var err error
	if handler != nil {
		err = handler(union, groups, sources)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.generation {
		s.dirty, s.lastError = err != nil, ""
		if err != nil {
			s.lastError = err.Error()
		}
	}
	return err
}

func (s *catalogPublication) status() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty, s.lastError
}

// Caller holds configMu, including initial handler registration. Taking the
// state snapshot after entering that transaction prevents an old startup view
// from being published after a concurrent hot catalog update.
func (r *Runtime) SetCatalogHandlers(validator, reconciler CatalogPublisher) error {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return domain.ErrShuttingDown
	}
	generation := r.publication.configure(validator, reconciler)
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	groups, sources := r.routes.Groups(), r.routes.Sources()
	if err := r.validateUpstream(index, groups, sources); err != nil {
		r.publication.validationFailed(generation, err)
		return err
	}
	return r.publishCatalog(index, groups, sources)
}

func (r *Runtime) reconcileUpstream() error {
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	return r.publishCatalog(index, r.routes.Groups(), r.routes.Sources())
}

func publicationUnion(index *catalogIndex) domain.CatalogUnion {
	if index != nil {
		return cloneCatalogUnion(index.union)
	}
	return domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{}, SourceStates: map[string]domain.CatalogState{}}
}

func (r *Runtime) validateUpstream(index *catalogIndex, groups []domain.Group, sources []domain.Source) error {
	groups, sources = publicationMetadata(groups, sources)
	return r.publication.validate(publicationUnion(index), groups, sources)
}

func (r *Runtime) publishCatalog(index *catalogIndex, groups []domain.Group, sources []domain.Source) error {
	groups, sources = publicationMetadata(groups, sources)
	return r.publication.publish(publicationUnion(index), groups, sources)
}

func publicationMetadata(groups []domain.Group, sources []domain.Source) ([]domain.Group, []domain.Source) {
	owned := make([]domain.Source, len(sources))
	for i, source := range sources {
		owned[i] = source.Clone()
	}
	return append([]domain.Group(nil), groups...), owned
}

// Explicit source quarantine and transient delivery errors have independent
// lifetimes. A successful delivery clears only the latter, never a quarantine.
// Conversely, removing a quarantine cannot hide a still-pending publish failure.
// The caller holds r.mu (read or write) to read the authoritative quarantine map.
func (r *Runtime) upstreamCatalogErrorLocked() string {
	_, message := r.publication.status()
	if message != "" {
		return message
	}
	first := ""
	for id, reason := range r.quarantinedSources {
		if first == "" || id < first {
			first, message = id, reason
		}
	}
	return message
}
