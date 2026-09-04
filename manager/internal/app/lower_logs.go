package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/lowerlogs"
)

func (r *Runtime) IngestLowerLogs(ctx context.Context, batch api.LowerLogBatch) (api.LowerLogAck, error) {
	if r.lowerLogs == nil {
		return api.LowerLogAck{}, errors.New("central lower log store is not configured")
	}
	if _, err := r.repo.GetSource(ctx, batch.SourceID); err != nil {
		return api.LowerLogAck{}, err
	}
	now := time.Now().UTC()
	r.lowerLogMu.Lock()
	defer r.lowerLogMu.Unlock()

	cursor, err := r.repo.GetLowerLogCursor(ctx, batch.SourceID, batch.BootID)
	if err != nil {
		return api.LowerLogAck{}, err
	}
	accepted := cursor.AcceptedThrough
	lastSequence := batch.Events[len(batch.Events)-1].Sequence
	if lastSequence <= accepted {
		return api.LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: accepted, ReceivedAt: cursor.UpdatedAt}, nil
	}
	expected := accepted + 1
	firstNew := 0
	for firstNew < len(batch.Events) && batch.Events[firstNew].Sequence <= accepted {
		firstNew++
	}
	if firstNew >= len(batch.Events) {
		return api.LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: accepted, ReceivedAt: cursor.UpdatedAt}, nil
	}
	if batch.Events[firstNew].Sequence != expected {
		return api.LowerLogAck{}, &api.LowerLogSequenceError{Expected: expected, Actual: batch.Events[firstNew].Sequence}
	}

	values := make([]lowerlogs.Event, 0, len(batch.Events)-firstNew)
	for _, event := range batch.Events[firstNew:] {
		values = append(values, lowerlogs.Event{
			SourceID: event.SourceID, BootID: event.BootID, Sequence: event.Sequence,
			Timestamp: event.Timestamp.UTC(), ReceivedAt: now, Level: strings.ToUpper(event.Level),
			Component: event.Component, PortID: event.PortID, DeviceID: event.DeviceID,
			Message: event.Message, Fields: event.Fields,
		})
	}
	// ACK only follows an fsync. A crash between this append and the SQLite
	// cursor commit can replay a line; the query layer deduplicates its event id.
	if err := r.lowerLogs.Append(values); err != nil {
		r.setLowerLogPersistError(err)
		return api.LowerLogAck{}, err
	}
	accepted = values[len(values)-1].Sequence
	if err := r.repo.SaveLowerLogCursor(ctx, batch.SourceID, batch.BootID, accepted, now); err != nil {
		r.setLowerLogPersistError(err)
		return api.LowerLogAck{}, err
	}
	r.setLowerLogPersistError(nil)
	r.broker.Publish(api.Event{Type: "lower_logs_changed", SourceID: batch.SourceID, Revision: accepted,
		Data: map[string]any{"source_id": batch.SourceID, "accepted_through": accepted}})
	return api.LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: accepted, ReceivedAt: now}, nil
}

func (r *Runtime) ListLowerLogs(ctx context.Context, request api.LowerLogRequest) (api.LowerLogPage, error) {
	if r.lowerLogs == nil {
		return api.LowerLogPage{}, errors.New("central lower log store is not configured")
	}
	if _, err := r.repo.GetSource(ctx, request.SourceID); err != nil {
		return api.LowerLogPage{}, err
	}
	page, err := r.lowerLogs.Query(lowerlogs.Filter{
		SourceID: request.SourceID, Level: request.Level, PortID: request.PortID,
		DeviceID: request.DeviceID, Query: request.Query, From: request.From, To: request.To,
		Limit: request.Limit, Offset: request.Offset,
	})
	if err != nil {
		r.setLowerLogPersistError(err)
		return api.LowerLogPage{}, err
	}
	lastReceivedAt, err := r.repo.LatestLowerLogReceivedAt(ctx, request.SourceID)
	if err != nil {
		return api.LowerLogPage{}, err
	}
	state := "never_received"
	if !lastReceivedAt.IsZero() {
		state = "stored"
		if time.Since(lastReceivedAt) <= 30*time.Second {
			state = "recent"
		}
	}
	items := make([]api.LowerLogItem, 0, len(page.Items))
	for _, event := range page.Items {
		items = append(items, api.LowerLogItem{LowerLogEvent: api.LowerLogEvent{
			SourceID: event.SourceID, BootID: event.BootID, Sequence: event.Sequence,
			Timestamp: event.Timestamp, Level: event.Level, Component: event.Component,
			PortID: event.PortID, DeviceID: event.DeviceID, Message: event.Message, Fields: event.Fields,
		}, ReceivedAt: event.ReceivedAt})
	}
	return api.LowerLogPage{Items: items, Total: page.Total, Limit: request.Limit, Offset: request.Offset,
		InvalidLines: page.Skipped, LastReceivedAt: lastReceivedAt, PushState: state}, nil
}

var _ api.LowerLogsService = (*Runtime)(nil)

func (r *Runtime) setLowerLogPersistError(err error) {
	r.mu.Lock()
	if err == nil {
		r.lowerLogPersistError = ""
	} else {
		r.lowerLogPersistError = err.Error()
	}
	r.mu.Unlock()
}
