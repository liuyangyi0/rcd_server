package app

import (
	"context"
	"sort"
	"strings"
	"sync"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

type alarmStatusWork struct {
	status  opc.LowerStatus
	version uint64
}

// statusHistory owns the event FIFO, coalesced desired alarms and independent
// persistence failures. A single flusher performs I/O outside the state lock;
// status callbacks enqueue without waiting for that I/O. Zero value is usable.
type statusHistory struct {
	mu          sync.Mutex
	writerMu    sync.Mutex
	sequence    uint64
	events      []domain.SourceEvent
	alarms      map[string]alarmStatusWork
	eventError  string
	alarmErrors map[string]string
}

func (s *statusHistory) enqueue(events []domain.SourceEvent, status opc.LowerStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	if s.alarms == nil {
		s.alarms = make(map[string]alarmStatusWork)
	}
	s.sequence++
	s.alarms[status.SourceID] = alarmStatusWork{status: status, version: s.sequence}
}

func (s *statusHistory) state() (events, alarms int, health string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := make([]string, 0, len(s.alarmErrors)+1)
	if s.eventError != "" {
		parts = append(parts, "events: "+s.eventError)
	}
	for id, message := range s.alarmErrors {
		parts = append(parts, "alarms/"+id+": "+message)
	}
	sort.Strings(parts)
	return len(s.events), len(s.alarms), strings.Join(parts, "; ")
}

func (s *statusHistory) flush(ctx context.Context, appendEvent func(context.Context, domain.SourceEvent) error, reconcile func(context.Context, opc.LowerStatus) error) (eventsWritten, alarmsWritten bool) {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	s.mu.Lock()
	// New arrivals belong to the next pass; a continuous event stream must not
	// postpone alarm recovery forever. Shutdown (after workers stop) drains all.
	batch := len(s.events)
	s.mu.Unlock()
	for i := 0; i < batch; i++ {
		s.mu.Lock()
		event := s.events[0]
		s.mu.Unlock()
		err := ctx.Err()
		if err == nil {
			err = appendEvent(ctx, event)
		}
		s.mu.Lock()
		if err != nil {
			s.eventError = err.Error()
			s.mu.Unlock()
			break
		}
		s.events[0] = domain.SourceEvent{}
		s.events = s.events[1:]
		if len(s.events) == 0 {
			s.events = nil
		}
		s.eventError = ""
		s.mu.Unlock()
		eventsWritten = true
	}
	// Event-table failure must not suppress independent current-condition writes.
	s.mu.Lock()
	ids := make([]string, 0, len(s.alarms))
	for id := range s.alarms {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		s.mu.Lock()
		work, exists := s.alarms[id]
		s.mu.Unlock()
		if !exists {
			continue
		}
		err := ctx.Err()
		if err == nil {
			err = reconcile(ctx, work.status)
		}
		s.mu.Lock()
		if err != nil {
			if s.alarmErrors == nil {
				s.alarmErrors = make(map[string]string)
			}
			s.alarmErrors[id] = err.Error()
		} else {
			delete(s.alarmErrors, id)
			// Content (including timestamps/epoch) can repeat. Only this exact
			// work item may be acknowledged, never a newer equal-valued one.
			if current, ok := s.alarms[id]; ok && current.version == work.version {
				delete(s.alarms, id)
			}
			alarmsWritten = true
		}
		s.mu.Unlock()
		if ctx.Err() != nil {
			break
		}
	}
	return eventsWritten, alarmsWritten
}
