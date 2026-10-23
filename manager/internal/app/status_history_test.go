package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func TestStatusHistoryFailuresDoNotBlockIndependentAlarms(t *testing.T) {
	var s statusHistory
	s.enqueue([]domain.SourceEvent{{SourceID: "a", Kind: "connection"}}, opc.LowerStatus{SourceID: "a"})
	s.enqueue(nil, opc.LowerStatus{SourceID: "b"})
	events, alarms := s.flush(context.Background(), func(context.Context, domain.SourceEvent) error { return errors.New("event unavailable") },
		func(_ context.Context, status opc.LowerStatus) error {
			if status.SourceID == "a" {
				return errors.New("alarm unavailable")
			}
			return nil
		})
	e, a, health := s.state()
	if events || !alarms || e != 1 || a != 1 || !strings.Contains(health, "events:") || !strings.Contains(health, "alarms/a:") {
		t.Fatalf("state=%d/%d/%s result=%v/%v", e, a, health, events, alarms)
	}
	s.flush(context.Background(), func(context.Context, domain.SourceEvent) error { return errors.New("event unavailable") }, func(context.Context, opc.LowerStatus) error { return nil })
	_, a, health = s.state()
	if a != 0 || !strings.Contains(health, "events:") || strings.Contains(health, "alarms/a:") {
		t.Fatal("alarm recovery masked event error or retained old alarm error")
	}
	s.flush(context.Background(), func(context.Context, domain.SourceEvent) error { return nil }, func(context.Context, opc.LowerStatus) error { return nil })
	if e, a, health := s.state(); e != 0 || a != 0 || health != "" {
		t.Fatalf("recovery=%d/%d/%s", e, a, health)
	}
}

func TestStatusHistoryDoesNotAcknowledgeNewEqualValuedWork(t *testing.T) {
	var s statusHistory
	status := connectedStatus("a", 1)
	s.enqueue(nil, status)
	s.flush(context.Background(), nil, func(context.Context, opc.LowerStatus) error {
		s.enqueue(nil, status)
		return nil
	})
	if _, a, _ := s.state(); a != 1 {
		t.Fatal("old completion consumed new equal-valued work")
	}
	s.flush(context.Background(), nil, func(context.Context, opc.LowerStatus) error { return nil })
	if _, a, _ := s.state(); a != 0 {
		t.Fatal("new work not acknowledged")
	}
}

func TestStatusHistoryEventBatchCannotStarveAlarms(t *testing.T) {
	var s statusHistory
	for i := 0; i < 3; i++ {
		s.enqueue([]domain.SourceEvent{{ID: int64(i + 1)}}, opc.LowerStatus{SourceID: "a"})
	}
	var ids []int64
	_, alarms := s.flush(context.Background(), func(_ context.Context, event domain.SourceEvent) error {
		ids = append(ids, event.ID)
		s.enqueue([]domain.SourceEvent{{ID: event.ID + 3}}, opc.LowerStatus{SourceID: "a"})
		return nil
	}, func(context.Context, opc.LowerStatus) error { return nil })
	if e, a, _ := s.state(); e != 3 || a != 0 || !alarms || fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("batch=%d/%d/%v ids=%v", e, a, alarms, ids)
	}
	s.flush(context.Background(), func(_ context.Context, event domain.SourceEvent) error { ids = append(ids, event.ID); return nil }, nil)
	if fmt.Sprint(ids) != "[1 2 3 4 5 6]" {
		t.Fatal(ids)
	}
}

func TestStatusHistoryCancellationAndOwnership(t *testing.T) {
	var s statusHistory
	input := []domain.SourceEvent{{Message: "original"}}
	s.enqueue(input, opc.LowerStatus{SourceID: "a"})
	input[0].Message = "changed"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fail := func() { t.Error("canceled flush called writer") }
	s.flush(ctx, func(context.Context, domain.SourceEvent) error { fail(); return nil }, func(context.Context, opc.LowerStatus) error { fail(); return nil })
	if e, a, health := s.state(); e != 1 || a != 1 || health == "" {
		t.Fatalf("lost canceled work: %d/%d/%s", e, a, health)
	}
	s.flush(context.Background(), func(_ context.Context, event domain.SourceEvent) error {
		if event.Message != "original" {
			t.Error("input aliases queue")
		}
		return nil
	}, func(context.Context, opc.LowerStatus) error { return nil })
}

func TestStatusHistoryCancellationStopsRemainingAlarmAttempts(t *testing.T) {
	var s statusHistory
	for _, id := range []string{"a", "b", "c"} {
		s.enqueue(nil, opc.LowerStatus{SourceID: id})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	s.flush(ctx, nil, func(ctx context.Context, _ opc.LowerStatus) error {
		attempts++
		cancel()
		return ctx.Err()
	})
	if _, a, health := s.state(); attempts != 1 || a != 3 || health == "" {
		t.Fatalf("canceled pass attempted=%d pending=%d health=%s", attempts, a, health)
	}
	s.flush(context.Background(), nil, func(context.Context, opc.LowerStatus) error { attempts++; return nil })
	if _, a, health := s.state(); attempts != 4 || a != 0 || health != "" {
		t.Fatalf("retry attempted=%d pending=%d health=%s", attempts, a, health)
	}
}

func TestStatusHistorySlowWriterDoesNotBlockEnqueueOrHealth(t *testing.T) {
	var s statusHistory
	s.enqueue([]domain.SourceEvent{{ID: 1}}, opc.LowerStatus{SourceID: "a"})
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		defer close(done)
		s.flush(context.Background(), func(context.Context, domain.SourceEvent) error { close(entered); <-release; return nil }, func(context.Context, opc.LowerStatus) error { return nil })
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not enter")
	}
	responsive := make(chan struct{})
	go func() {
		s.enqueue([]domain.SourceEvent{{ID: 2}}, opc.LowerStatus{SourceID: "b"})
		s.state()
		close(responsive)
	}()
	select {
	case <-responsive:
	case <-time.After(time.Second):
		t.Fatal("writer held state lock")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flush did not exit")
	}
	if e, _, _ := s.state(); e != 1 {
		t.Fatal("new event was drained by old batch")
	}
}

func TestStatusHistoryConcurrentFlushersSerializeWithoutEventLoss(t *testing.T) {
	var s statusHistory
	var active, writes atomic.Int32
	appendEvent := func(context.Context, domain.SourceEvent) error {
		if active.Add(1) != 1 {
			t.Error("concurrent writers")
		}
		writes.Add(1)
		active.Add(-1)
		return nil
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			for i := 0; i < 50; i++ {
				if id < 4 {
					s.enqueue([]domain.SourceEvent{{SourceID: fmt.Sprint(id)}}, opc.LowerStatus{SourceID: fmt.Sprint(id)})
				} else {
					s.flush(context.Background(), appendEvent, func(context.Context, opc.LowerStatus) error { return nil })
				}
			}
		}(worker)
	}
	workers.Wait()
	s.flush(context.Background(), appendEvent, func(context.Context, opc.LowerStatus) error { return nil })
	if e, a, _ := s.state(); e != 0 || a != 0 || writes.Load() != 200 {
		t.Fatalf("lost/duplicated events: %d/%d written=%d", e, a, writes.Load())
	}
}

func TestRuntimeLateStatusNotificationKeepsHistoryButUsesCurrentAlarm(t *testing.T) {
	r := seededRuntime(t)
	current := connectedStatus("source-a", 2)
	r.mu.Lock()
	r.statuses[current.SourceID] = current
	r.mu.Unlock()
	old := current
	old.Epoch, old.Connection, old.LastError = 1, opc.ConnectionDisconnected, "old outage"
	r.persistLowerStatusTransition(current, old, true)
	r.retryDirtyStatuses(context.Background())
	alarms, err := r.ListAlarms(context.Background(), api.PageRequest{Status: string(domain.AlarmOpen), Limit: 10})
	if err != nil || alarms.Total != 0 {
		t.Fatalf("late notification restored obsolete alarm: %+v %v", alarms, err)
	}
	events, err := r.ListSourceEvents(context.Background(), api.PageRequest{SourceID: "source-a", Kind: "connection", Limit: 10})
	if err != nil || events.Total != 1 || events.Items[0].Current != string(opc.ConnectionDisconnected) {
		t.Fatalf("old transition history lost: %+v %v", events, err)
	}
}

func TestRuntimeEventTableFailureStillResolvesRecoveredAlarm(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	repo, err := store.Open(ctx, path, store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if _, err := repo.CreateGroup(ctx, domain.Group{ID: "g", Name: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSource(ctx, domain.Source{ID: "a", GroupID: "g", Name: "a", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(ctx, repo, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	disconnected := connectedStatus("a", 1)
	disconnected.Connection, disconnected.LastError = opc.ConnectionDisconnected, "outage"
	r.OnLowerStatus(disconnected)
	r.retryDirtyStatuses(ctx)
	if open, _, err := repo.AlarmCounts(ctx); err != nil || open != 1 {
		t.Fatalf("initial alarm=%d %v", open, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_source_events BEFORE INSERT ON source_events BEGIN SELECT RAISE(FAIL,'injected event failure'); END`); err != nil {
		t.Fatal(err)
	}
	r.OnLowerStatus(connectedStatus("a", 2))
	r.retryDirtyStatuses(ctx)
	if open, _, err := repo.AlarmCounts(ctx); err != nil || open != 0 {
		t.Fatalf("event failure blocked alarm recovery=%d %v", open, err)
	}
	if e, a, health := r.statusHistory.state(); e == 0 || a != 0 || !strings.Contains(health, "injected event failure") {
		t.Fatalf("pending health=%d/%d/%s", e, a, health)
	}
	if r.Health(ctx).Checks["status_history_persistence"] == "ok" {
		t.Fatal("history failure hidden by alarm success")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_source_events`); err != nil {
		t.Fatal(err)
	}
	r.retryDirtyStatuses(ctx)
	if e, a, health := r.statusHistory.state(); e != 0 || a != 0 || health != "" {
		t.Fatalf("retry=%d/%d/%s", e, a, health)
	}
}
