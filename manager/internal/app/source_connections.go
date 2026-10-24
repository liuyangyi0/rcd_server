package app

import (
	"context"
	"sync"

	"rcd_manager/internal/opc"
)

type lowerHandle struct {
	client     opc.LowerClient
	cancel     context.CancelFunc
	generation uint64
}

// sourceConnections owns supervisor handles and non-reused generation fences.
// Zero value is ready to use. Cancellation is returned to the caller, never
// invoked while holding mu. Credentials, factories and network I/O stay outside.
// Runtime also holds r.mu for every production mutation and callback acceptance
// so a successful fence check and its status/catalog/cache mutation stay atomic.
type sourceConnections struct {
	mu          sync.RWMutex
	handles     map[string]lowerHandle
	generations map[string]uint64
}

func (s *sourceConnections) advanceLocked(id string) (uint64, context.CancelFunc) {
	if s.generations == nil {
		s.generations = map[string]uint64{}
		s.handles = map[string]lowerHandle{}
	}
	old := s.handles[id]
	delete(s.handles, id)
	s.generations[id]++
	return s.generations[id], old.cancel
}

func (s *sourceConnections) begin(id string) (uint64, context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advanceLocked(id)
}

func (s *sourceConnections) attach(id string, generation uint64, client opc.LowerClient, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || s.generations[id] != generation || client == nil {
		return false
	}
	if _, exists := s.handles[id]; exists {
		return false
	}
	s.handles[id] = lowerHandle{client: client, cancel: cancel, generation: generation}
	return true
}

// finish fences both a failed factory reservation and an exited Run. It never
// invalidates a replacement connection, even after deletion/recreation of an ID.
func (s *sourceConnections) finish(id string, generation uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || s.generations[id] != generation {
		return 0, false
	}
	delete(s.handles, id)
	s.generations[id]++
	return s.generations[id], true
}

func (s *sourceConnections) stop(id string) context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cancel := s.advanceLocked(id)
	return cancel
}

func (s *sourceConnections) stopAll() []context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	cancels := make([]context.CancelFunc, 0, len(s.handles))
	// Fence starting reservations too, not just already-attached supervisors.
	for id := range s.generations {
		s.generations[id]++
	}
	for id, handle := range s.handles {
		if handle.cancel != nil {
			cancels = append(cancels, handle.cancel)
		}
		delete(s.handles, id)
	}
	return cancels
}

func (s *sourceConnections) current(id string, generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return generation != 0 && s.generations[id] == generation
}

func (s *sourceConnections) handle(id string) (lowerHandle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	handle, ok := s.handles[id]
	return handle, ok
}

func (s *sourceConnections) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.handles)
}

func cancelLower(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}
