package lowerlogs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
)

func (r *Service) IngestLowerLogs(ctx context.Context, batch domain.LowerLogBatch) (domain.LowerLogAck, error) {
	if len(batch.Events) == 0 {
		return domain.LowerLogAck{}, domain.ErrInvalid
	}
	if r.lowerLogs == nil {
		return domain.LowerLogAck{}, errors.New("central lower log store is not configured")
	}
	if _, err := r.repo.GetSource(ctx, batch.SourceID); err != nil {
		return domain.LowerLogAck{}, err
	}
	now := time.Now().UTC()
	lock, _ := r.lowerLogLocks.LoadOrStore(batch.SourceID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	cursor, err := r.repo.GetLowerLogCursor(ctx, batch.SourceID, batch.BootID)
	if err != nil {
		return domain.LowerLogAck{}, err
	}
	accepted := cursor.AcceptedThrough
	lastSequence := batch.Events[len(batch.Events)-1].Sequence
	if lastSequence <= accepted {
		return domain.LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: accepted, ReceivedAt: cursor.UpdatedAt}, nil
	}
	expected := accepted + 1
	firstNew := 0
	for firstNew < len(batch.Events) && batch.Events[firstNew].Sequence <= accepted {
		firstNew++
	}
	if firstNew >= len(batch.Events) {
		return domain.LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: accepted, ReceivedAt: cursor.UpdatedAt}, nil
	}
	if batch.Events[firstNew].Sequence != expected {
		return domain.LowerLogAck{}, &domain.LowerLogSequenceError{Expected: expected, Actual: batch.Events[firstNew].Sequence}
	}

	values := make([]Event, 0, len(batch.Events)-firstNew)
	for _, event := range batch.Events[firstNew:] {
		values = append(values, Event{
			SourceID: event.SourceID, BootID: event.BootID, Sequence: event.Sequence,
			Timestamp: event.Timestamp.UTC(), ReceivedAt: now, Level: strings.ToUpper(event.Level),
			Component: event.Component, PortID: event.PortID, DeviceID: event.DeviceID,
			Message: event.Message, Fields: event.Fields,
		})
	}
	// ACK only follows an fsync. A crash between this append and the SQLite
	// cursor commit can replay a line; the query layer deduplicates its event id.
	if err := r.lowerLogs.Append(values); err != nil {
		r.setLowerLogPersistError(batch.SourceID, err)
		return domain.LowerLogAck{}, err
	}
	accepted = values[len(values)-1].Sequence
	if err := r.repo.SaveLowerLogCursor(ctx, batch.SourceID, batch.BootID, accepted, now); err != nil {
		r.setLowerLogPersistError(batch.SourceID, err)
		return domain.LowerLogAck{}, err
	}
	r.setLowerLogPersistError(batch.SourceID, nil)
	if r.notify != nil {
		r.notify(batch.SourceID, accepted)
	}
	return domain.LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: accepted, ReceivedAt: now}, nil
}

func (r *Service) ListLowerLogs(ctx context.Context, request domain.LowerLogRequest) (domain.LowerLogPage, error) {
	if r.lowerLogs == nil {
		return domain.LowerLogPage{}, errors.New("central lower log store is not configured")
	}
	if _, err := r.repo.GetSource(ctx, request.SourceID); err != nil {
		return domain.LowerLogPage{}, err
	}
	page, err := r.lowerLogs.QueryContext(ctx, Filter{
		CommandAttempts: request.CommandAttempts,
		SourceID:        request.SourceID, Level: request.Level, PortID: request.PortID,
		DeviceID: request.DeviceID, Query: request.Query, From: request.From, To: request.To,
		Limit: request.Limit, Offset: request.Offset,
	})
	if err != nil {
		return domain.LowerLogPage{}, err
	}
	lastReceivedAt, err := r.repo.LatestLowerLogReceivedAt(ctx, request.SourceID)
	if err != nil {
		return domain.LowerLogPage{}, err
	}
	state := "never_received"
	if !lastReceivedAt.IsZero() {
		state = "stored"
		if time.Since(lastReceivedAt) <= 30*time.Second {
			state = "recent"
		}
	}
	items := make([]domain.LowerLogItem, 0, len(page.Items))
	for _, event := range page.Items {
		items = append(items, domain.LowerLogItem{LowerLogEvent: domain.LowerLogEvent{
			SourceID: event.SourceID, BootID: event.BootID, Sequence: event.Sequence,
			Timestamp: event.Timestamp, Level: event.Level, Component: event.Component,
			PortID: event.PortID, DeviceID: event.DeviceID, Message: event.Message, Fields: event.Fields,
		}, ReceivedAt: event.ReceivedAt})
	}
	return domain.LowerLogPage{Items: items, Total: page.Total, Limit: request.Limit, Offset: request.Offset,
		InvalidLines: page.Skipped, LastReceivedAt: lastReceivedAt, PushState: state}, nil
}

func (r *Service) setLowerLogPersistError(source string, err error) {
	r.mu.Lock()
	if err == nil {
		delete(r.persistErrors, source)
	} else {
		r.persistErrors[source] = err.Error()
	}
	r.mu.Unlock()
}

// Service owns ingestion ordering and health independently of manager Runtime.
type Service struct {
	repo          *store.Repository
	lowerLogs     *Store
	lowerLogLocks sync.Map
	mu            sync.RWMutex
	persistErrors map[string]string
	notify        func(string, uint64)
}

func NewService(repo *store.Repository, files *Store, notify func(string, uint64)) *Service {
	return &Service{repo: repo, lowerLogs: files, notify: notify, persistErrors: map[string]string{}}
}
func (r *Service) HealthError() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var messages []string
	for source, message := range r.persistErrors {
		messages = append(messages, fmt.Sprintf("%s: %s", source, message))
	}
	sort.Strings(messages)
	return strings.Join(messages, "; ")
}
