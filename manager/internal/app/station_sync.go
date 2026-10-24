package app

import (
	"sort"
	"strings"
	"sync"
)

type stationRefreshTicket struct {
	sourceID string
	sequence uint64
}

type stationRefreshLease struct {
	ticket stationRefreshTicket
	dirty  bool
}

type stationFeature struct {
	epoch     uint64
	supported bool
}

// stationSync owns refresh admission, capability knowledge and persistence
// health. Runtime still coordinates live snapshots with connection/catalog
// state under r.mu. Lock order is writerMu -> r.mu -> mu; no I/O under mu.
// The zero value is usable. Tickets never reuse a deleted source's identity.
type stationSync struct {
	writerMu      sync.Mutex
	mu            sync.Mutex
	sequence      uint64
	pending       map[string]stationRefreshLease
	features      map[string]stationFeature
	persistErrors map[string]string
	refreshQueue  chan stationRefreshTicket
}

func (s *stationSync) queue() chan stationRefreshTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshQueue == nil {
		s.refreshQueue = make(chan stationRefreshTicket, 64)
	}
	return s.refreshQueue
}

func (s *stationSync) request(sourceID string) (stationRefreshTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease, ok := s.pending[sourceID]; ok {
		lease.dirty = true
		s.pending[sourceID] = lease
		return lease.ticket, false
	}
	if s.pending == nil {
		s.pending = make(map[string]stationRefreshLease)
	}
	s.sequence++
	ticket := stationRefreshTicket{sourceID: sourceID, sequence: s.sequence}
	s.pending[sourceID] = stationRefreshLease{ticket: ticket}
	return ticket, true
}

func (s *stationSync) current(ticket stationRefreshTicket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.pending[ticket.sourceID]
	return ok && lease.ticket == ticket
}

// finish releases the exact lease, returning whether a coalesced invalidation
// needs another attempt. An old worker cannot release a replacement's lease.
func (s *stationSync) finish(ticket stationRefreshTicket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.pending[ticket.sourceID]
	if !ok || lease.ticket != ticket {
		return false
	}
	delete(s.pending, ticket.sourceID)
	return lease.dirty
}

func (s *stationSync) support(sourceID string, epoch uint64) (known, supported bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	feature, exists := s.features[sourceID]
	return exists && feature.epoch == epoch, exists && feature.epoch == epoch && feature.supported
}

// Caller checks the current source/connection while holding Runtime.mu.
func (s *stationSync) setSupport(sourceID string, epoch uint64, supported bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.features == nil {
		s.features = make(map[string]stationFeature)
	}
	s.features[sourceID] = stationFeature{epoch: epoch, supported: supported}
}

func (s *stationSync) persistenceResult(sourceID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.persistErrors, sourceID)
		return
	}
	if s.persistErrors == nil {
		s.persistErrors = make(map[string]string)
	}
	s.persistErrors[sourceID] = err.Error()
}

func (s *stationSync) needsPersistence(sourceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistErrors[sourceID] != ""
}

func (s *stationSync) persistenceError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := make([]string, 0, len(s.persistErrors))
	for id, message := range s.persistErrors {
		parts = append(parts, id+": "+message)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// Removal/reset is coordinated with snapshot removal under Runtime.mu and
// writerMu, so late database writes cannot restore a deleted source's cache.
func (s *stationSync) remove(sourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, sourceID)
	delete(s.features, sourceID)
	delete(s.persistErrors, sourceID)
}
