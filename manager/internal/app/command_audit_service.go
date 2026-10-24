package app

import (
	"context"
	"fmt"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
	"sync"
	"time"
)

// Command evidence is not a dispatch queue. Disk failure must never retain a
// physical-port reservation or cause a command to be submitted a second time.
// This service owns bounded, immutable audit snapshots and their retry state.
type commandAuditService struct {
	repo      *store.Repository
	managerID string
	mu        sync.RWMutex
	entries   map[string]commandAuditEntry
	io        chan struct{}
	error     string
	gap       bool
	revision  uint64
}
type commandAuditEntry struct {
	detail   domain.CommandDetail
	revision uint64
	dirty    bool
}

func newCommandAuditService(repo *store.Repository, id string) *commandAuditService {
	return &commandAuditService{repo: repo, managerID: id, entries: map[string]commandAuditEntry{}, io: make(chan struct{}, 1)}
}
func (s *commandAuditService) put(detail domain.CommandDetail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[detail.Command.ID]; !ok && len(s.entries) >= 10000 {
		oldest := ""
		var at time.Time
		for id, e := range s.entries {
			if e.detail.Command.State != domain.CommandWaiting && e.detail.Command.State != domain.CommandQueued && (oldest == "" || e.detail.Command.StartedAt.Before(at)) {
				oldest = id
				at = e.detail.Command.StartedAt
			}
		}
		if oldest == "" {
			s.gap = true
			s.error = "command audit memory limit reached"
			return
		}
		if s.entries[oldest].dirty {
			s.gap = true
		}
		delete(s.entries, oldest)
	}
	old := s.entries[detail.Command.ID]
	// A delayed acceptance save must not undo a terminal outcome.
	if old.detail.Command.ID != "" && old.detail.Command.State != domain.CommandWaiting && old.detail.Command.State != domain.CommandQueued &&
		(detail.Command.State == domain.CommandWaiting || detail.Command.State == domain.CommandQueued) {
		detail.Command = old.detail.Command
	}
	if old.detail.Command.State == domain.CommandWaiting && detail.Command.State == domain.CommandQueued {
		detail.Command = old.detail.Command
	}
	if detail.Trace == nil {
		detail.Trace = old.detail.Trace
	}
	if old.detail.Trace != nil && old.detail.Trace.Snapshot != nil && detail.Trace != nil && (detail.Trace.Snapshot == nil || old.detail.Trace.Snapshot.Revision > detail.Trace.Snapshot.Revision) {
		detail.Trace = old.detail.Trace
	}
	if s.error != "" && detail.Trace != nil {
		t := *detail.Trace
		t.HistoryGap = true
		detail.Trace = &t
	}
	s.revision++
	s.entries[detail.Command.ID] = commandAuditEntry{detail: detail, revision: s.revision, dirty: true}
}
func (s *commandAuditService) detail(ctx context.Context, id string) (domain.CommandDetail, error) {
	s.mu.RLock()
	v, ok := s.entries[id]
	s.mu.RUnlock()
	if ok {
		return v.detail, nil
	}
	return s.repo.CommandDetail(ctx, id)
}
func (s *commandAuditService) record(command domain.PendingCommand) {
	s.put(domain.CommandDetail{Command: command})
	_ = s.persist(command.ID)
}
func (s *commandAuditService) trace(command domain.PendingCommand, trace domain.CommandTrace) {
	s.put(domain.CommandDetail{Command: command, Trace: &trace})
	_ = s.persist(command.ID)
}
func (s *commandAuditService) evidence(ctx context.Context, source string, v domain.TrackedCommand) error {
	if err := v.Validate(); err != nil {
		return err
	}
	detail, err := s.detail(ctx, v.CommandID)
	if err != nil {
		return err
	}
	t := detail.Trace
	if t == nil || source != detail.Command.SourceID || v.ManagerInstanceID != s.managerID || v.GatewayInstanceID != t.GatewayInstanceID || v.BootID != t.BootID || v.PortID != t.PhysicalPort || v.RequestHash != t.RequestHash {
		return fmt.Errorf("command evidence pinned target mismatch")
	}
	if t.Snapshot != nil && v.Revision <= t.Snapshot.Revision {
		return nil
	}
	copied := *t
	copied.Snapshot = &v
	detail.Trace = &copied
	s.put(detail)
	// Evidence is already validated in memory; availability does not depend on
	// its successful persistence. Health and detail explicitly expose the gap.
	return nil
}
func (s *commandAuditService) persist(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	select {
	case s.io <- struct{}{}:
		defer func() { <-s.io }()
	case <-ctx.Done():
		s.failed(id, ctx.Err())
		return ctx.Err()
	}
	s.mu.RLock()
	e, ok := s.entries[id]
	s.mu.RUnlock()
	if !ok || !e.dirty {
		return nil
	}
	err := s.repo.SaveCommand(ctx, e.detail.Command)
	if err == nil && e.detail.Trace != nil {
		err = s.repo.BeginCommandTrace(ctx, id, *e.detail.Trace)
		if err == nil && e.detail.Trace.Snapshot != nil {
			err = s.repo.SaveCommandEvidence(ctx, e.detail.Command.SourceID, e.detail.Trace.GatewayInstanceID, s.managerID, []domain.TrackedCommand{*e.detail.Trace.Snapshot}, nil, e.detail.Trace.HistoryGap)
		}
	}
	if err != nil {
		s.failed(id, err)
		return err
	}
	s.mu.Lock()
	latest := s.entries[id]
	if latest.revision == e.revision {
		latest.dirty = false
		s.entries[id] = latest
	}
	dirty := false
	for _, v := range s.entries {
		dirty = dirty || v.dirty
	}
	if !dirty {
		s.error = ""
	}
	s.mu.Unlock()
	return nil
}

// Commit a bounded event page without waiting for unrelated sources' dirty
// entries. Persist its command identities first, then evidence and cursor in
// one transaction. Concurrent newer state remains dirty for a later flush.
func (s *commandAuditService) commitPage(ctx context.Context, source, gateway string, values []domain.TrackedCommand, cursor *uint64, gap bool) error {
	select {
	case s.io <- struct{}{}:
		defer func() { <-s.io }()
	case <-ctx.Done():
		return ctx.Err()
	}
	entries := map[string]commandAuditEntry{}
	s.mu.RLock()
	for _, v := range values {
		if e, ok := s.entries[v.CommandID]; ok && e.detail.Command.SourceID == source && e.dirty {
			entries[v.CommandID] = e
		}
	}
	s.mu.RUnlock()
	for id, e := range entries {
		if err := s.repo.SaveCommand(ctx, e.detail.Command); err != nil {
			s.failed(id, err)
			return err
		}
		if e.detail.Trace != nil {
			if err := s.repo.BeginCommandTrace(ctx, id, *e.detail.Trace); err != nil {
				s.failed(id, err)
				return err
			}
		}
	}
	if err := s.repo.SaveCommandEvidence(ctx, source, gateway, s.managerID, values, cursor, gap); err != nil {
		for id := range entries {
			s.failed(id, err)
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range entries {
		latest := s.entries[id]
		// The page can contain an older revision than the live cache. Never
		// acknowledge persistence of a newer snapshot merely by saving this page.
		covered := e.detail.Trace == nil || e.detail.Trace.Snapshot == nil
		for _, v := range values {
			if v.CommandID == id && e.detail.Trace != nil && e.detail.Trace.Snapshot != nil && v.Revision >= e.detail.Trace.Snapshot.Revision {
				covered = true
			}
		}
		if latest.revision == e.revision && covered {
			latest.dirty = false
			s.entries[id] = latest
		}
	}
	dirty := false
	for _, e := range s.entries {
		dirty = dirty || e.dirty
	}
	if !dirty {
		s.error = ""
	}
	return nil
}
func (s *commandAuditService) failed(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.error = err.Error()
	if e, ok := s.entries[id]; ok {
		if e.detail.Trace != nil {
			t := *e.detail.Trace
			t.HistoryGap = true
			e.detail.Trace = &t
		}
		s.revision++
		e.revision = s.revision
		e.dirty = true
		s.entries[id] = e
	}
}
func (s *commandAuditService) flush() {
	deadline := time.Now().Add(50 * time.Millisecond)
	s.mu.RLock()
	ids := make([]string, 0)
	for id, e := range s.entries {
		if e.dirty {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	// Stop after the first storage error: a failed disk must not cost 100 ms for
	// each buffered record or prevent the worker from querying pending commands.
	for _, id := range ids {
		if time.Now().After(deadline) {
			return
		}
		if s.persist(id) != nil {
			return
		}
	}
}
func (s *commandAuditService) health() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.gap {
		return "audit records evicted; history incomplete; " + s.error
	}
	return s.error
}
func (s *commandAuditService) pending(source string) []domain.CommandDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var values []domain.CommandDetail
	for _, e := range s.entries {
		if e.detail.Command.SourceID == source && e.detail.Command.State == domain.CommandWaiting {
			values = append(values, e.detail)
		}
	}
	return values
}
func (s *commandAuditService) dirtyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.entries {
		if e.dirty {
			n++
		}
	}
	return n
}

func (s *commandAuditService) markHistoryGap(source, gateway string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		if e.detail.Command.SourceID != source || e.detail.Trace == nil || e.detail.Trace.GatewayInstanceID != gateway || e.detail.Trace.HistoryGap {
			continue
		}
		t := *e.detail.Trace
		t.HistoryGap = true
		e.detail.Trace = &t
		s.revision++
		e.revision = s.revision
		e.dirty = true
		s.entries[id] = e
	}
}
