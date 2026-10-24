package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"rcd_manager/internal/domain"
)

type managementAuditWriter func(context.Context, domain.AuditEntry) (domain.AuditEntry, error)

type managementAuditWork struct {
	entry   domain.AuditEntry // metadata only; Details lives in immutable JSON
	details []byte
	invalid bool
	failure string
	done    chan struct{} // non-nil while precisely one attempt owns this record
}

// managementAudit owns immutable pending management records and their failures.
// These are audit saves, never operation/command retries. No I/O under mu.
// Existing SQLite deduplication/retention rules remain authoritative.
type managementAudit struct {
	mu       sync.Mutex
	sequence uint64
	entries  map[uint64]managementAuditWork
}

func (s *managementAudit) save(ctx context.Context, entry domain.AuditEntry, write managementAuditWriter) (domain.AuditEntry, error) {
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = time.Now().UTC()
	}
	details, err := json.Marshal(entry.Details)
	entry.Details = nil
	work := managementAuditWork{entry: entry, details: details}
	if err != nil {
		err = fmt.Errorf("encode audit details: %w", err)
		// Do not retain an unsafe caller-owned map, silently omit its details or
		// pretend this record was saved. Invalid records stay visible as failed.
		work.invalid, work.failure = true, err.Error()
	} else {
		work.done = make(chan struct{})
	}
	s.mu.Lock()
	if s.entries == nil {
		s.entries = make(map[uint64]managementAuditWork)
	}
	s.sequence++
	id := s.sequence
	s.entries[id] = work
	s.mu.Unlock()
	if err != nil {
		return entry, err
	}
	return s.persist(ctx, id, work, write)
}

func decodeManagementAudit(work managementAuditWork) (domain.AuditEntry, error) {
	entry := work.entry
	decoder := json.NewDecoder(bytes.NewReader(work.details))
	// JSON round trips must not round uint64 values above 2^53 through float64.
	decoder.UseNumber()
	if err := decoder.Decode(&entry.Details); err != nil {
		return entry, err
	}
	return entry, nil
}

func (s *managementAudit) persist(ctx context.Context, id uint64, work managementAuditWork, write managementAuditWriter) (domain.AuditEntry, error) {
	defer close(work.done)
	entry, err := decodeManagementAudit(work)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		_, err = write(ctx, entry)
	}
	s.mu.Lock()
	if err != nil {
		work.failure, work.done = err.Error(), nil
		s.entries[id] = work
	} else {
		delete(s.entries, id)
	}
	s.mu.Unlock()
	// Return a separate decoded snapshot: a writer cannot mutate later retries
	// or the event sent to Web by changing the map it received.
	returnEntry, decodeErr := decodeManagementAudit(work)
	if err == nil {
		err = decodeErr
	}
	return returnEntry, err
}

func (s *managementAudit) state() (dirty int, health string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first uint64
	for id, work := range s.entries {
		if work.failure == "" {
			continue
		}
		dirty++
		if first == 0 || id < first {
			first, health = id, work.failure
		}
	}
	return dirty, health
}

func (s *managementAudit) retry(ctx context.Context, write managementAuditWriter) bool {
	s.mu.Lock()
	ids := make([]uint64, 0, len(s.entries))
	for id, work := range s.entries {
		if work.done == nil && !work.invalid {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	persisted := false
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		s.mu.Lock()
		work, exists := s.entries[id]
		if !exists || work.done != nil || work.invalid {
			s.mu.Unlock()
			continue
		}
		work.done = make(chan struct{})
		s.entries[id] = work
		s.mu.Unlock()
		if _, err := s.persist(ctx, id, work, write); err == nil {
			persisted = true
		}
	}
	return persisted
}

// drain waits for the attempts already admitted at the shutdown boundary, then
// retries failures within the caller's deadline. It does not seal admission;
// the application must quiesce producers before shutdown, as for other queues.
func (s *managementAudit) drain(ctx context.Context, write managementAuditWriter) bool {
	s.mu.Lock()
	waiting := make([]chan struct{}, 0, len(s.entries))
	for _, work := range s.entries {
		if work.done != nil {
			waiting = append(waiting, work.done)
		}
	}
	s.mu.Unlock()
	for _, done := range waiting {
		select {
		case <-done:
		case <-ctx.Done():
			return false
		}
	}
	return s.retry(ctx, write)
}
