package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func processStoreFixture(t *testing.T) (*Repository, domain.ProcessBinding) {
	t.Helper()
	r := openTestRepository(t)
	ctx := context.Background()
	if _, err := r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := r.CreateSource(ctx, domain.Source{ID: id, GroupID: "g", Name: id, Endpoint: "opc.tcp://test:4840", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
			t.Fatal(err)
		}
	}
	binding, err := r.SaveProcessBinding(ctx, domain.ProcessBinding{SourceID: "a", SourceRevision: 1, AgentURL: "http://agent:8086", InstanceID: "instance-a"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return r, binding
}

func processStoreRecord(binding domain.ProcessBinding, id string) ProcessControlRecord {
	now := time.Now().UTC()
	return ProcessControlRecord{Binding: binding, Request: domain.ProcessActionRequest{RequestID: id, InstanceID: binding.InstanceID, Action: "restart", ExpectedRevision: "agent:1"},
		Operation: domain.ProcessOperation{SourceID: binding.SourceID, SourceRevision: binding.SourceRevision, BindingRevision: binding.Revision,
			RequestID: id, InstanceID: binding.InstanceID, Action: "restart", State: "accepted", CreatedAt: now, UpdatedAt: now}, Actor: "test"}
}

func TestProcessBindingsUseSourceAndBindingCASAndRetainUnbindRevision(t *testing.T) {
	r, binding := processStoreFixture(t)
	ctx := context.Background()
	unconfigured, err := r.GetProcessBinding(ctx, "b")
	if err != nil || unconfigured.Revision != 0 || unconfigured.SourceRevision != 1 || unconfigured.Configured() {
		t.Fatalf("binding=%+v err=%v", unconfigured, err)
	}
	if _, err := r.SaveProcessBinding(ctx, binding, 0); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale binding=%v", err)
	}
	wrongSource := binding
	wrongSource.SourceRevision = 2
	if _, err := r.SaveProcessBinding(ctx, wrongSource, 1); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale source=%v", err)
	}
	duplicate := binding
	duplicate.SourceID = "b"
	if _, err := r.SaveProcessBinding(ctx, duplicate, 0); err == nil {
		t.Fatal("duplicate agent target accepted")
	}
	duplicate.AgentURL = "http://another-address:8086"
	if _, err := r.SaveProcessBinding(ctx, duplicate, 0); err == nil {
		t.Fatal("address alias bypassed unique physical instance")
	}
	binding.AgentURL, binding.InstanceID = "", ""
	unbound, err := r.SaveProcessBinding(ctx, binding, 1)
	if err != nil || unbound.Revision != 2 || unbound.Configured() {
		t.Fatalf("unbind=%+v %v", unbound, err)
	}
	if _, err := r.SaveProcessBinding(ctx, binding, 0); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("unbind reset CAS: %v", err)
	}
}

func TestProcessOperationJournalFreezesFingerprintAndTerminalState(t *testing.T) {
	r, binding := processStoreFixture(t)
	ctx := context.Background()
	record := processStoreRecord(binding, "req-1")
	record.ExpectedConfigRevisions = map[string]string{"config.toml": strings.Repeat("a", 64)}
	if err := r.CreateProcessOperation(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveProcessBinding(ctx, binding, binding.Revision); err == nil {
		t.Fatal("binding changed with pending process job")
	}
	if err := r.CreateProcessOperation(ctx, processStoreRecord(binding, "req-2")); err == nil {
		t.Fatal("second pending job admitted")
	}
	loaded, err := r.GetProcessOperation(ctx, "a", "req-1")
	if err != nil || !loaded.SameRequest(1, 1, record.Request, record.ExpectedConfigRevisions) {
		t.Fatalf("load %+v %v", loaded, err)
	}
	wrong := loaded
	wrong.Request.Action = "stop"
	wrong.Operation.Action = "stop"
	if _, _, err := r.UpdateProcessOperation(ctx, wrong); err == nil {
		t.Fatal("journal request changed")
	}
	wrong = loaded
	wrong.ExpectedConfigRevisions = map[string]string{"config.toml": strings.Repeat("b", 64)}
	if _, _, err := r.UpdateProcessOperation(ctx, wrong); err == nil {
		t.Fatal("journal configuration precondition changed")
	}
	record.Dispatched = true
	record, _, err = r.UpdateProcessOperation(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	stale := record
	stale.Dispatched = false
	stale, _, err = r.UpdateProcessOperation(ctx, stale)
	if err != nil || !stale.Dispatched {
		t.Fatalf("write-ahead marker regressed: %+v %v", stale, err)
	}
	record.Operation.State = "failed"
	record.Operation.ErrorCode = "unconfirmed_cancelled"
	finished, changed, err := r.UpdateProcessOperation(ctx, record)
	if err != nil || !changed || !finished.Operation.Terminal() {
		t.Fatalf("finish %+v %v", finished, err)
	}
	stale.Operation.State = "unknown"
	sticky, changed, err := r.UpdateProcessOperation(ctx, stale)
	if err != nil || changed || sticky.Operation.State != "failed" {
		t.Fatalf("terminal regressed %+v %v", sticky, err)
	}
	if _, err := r.SaveProcessBinding(ctx, binding, binding.Revision); err != nil {
		t.Fatal(err)
	}
	items, total, err := r.ListProcessOperations(ctx, "a", 50, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].Binding.Revision != 1 {
		t.Fatalf("history %+v %d %v", items, total, err)
	}
	pending, err := r.ListPendingProcessOperations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending %+v %v", pending, err)
	}
}

func TestProcessTerminalPersistenceFailureDoesNotErasePendingJournal(t *testing.T) {
	r, binding := processStoreFixture(t)
	ctx := context.Background()
	record := processStoreRecord(binding, "failure")
	record.Dispatched = true
	if err := r.CreateProcessOperation(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`CREATE TRIGGER reject_process_terminal BEFORE UPDATE ON process_control_operations WHEN NEW.state='failed' BEGIN SELECT RAISE(ABORT,'test unavailable storage'); END`); err != nil {
		t.Fatal(err)
	}
	record.Operation.State = "failed"
	if _, _, err := r.UpdateProcessOperation(ctx, record); err == nil {
		t.Fatal("expected persistence failure")
	}
	loaded, err := r.GetProcessOperation(ctx, "a", "failure")
	if err != nil || loaded.Operation.Terminal() || !loaded.Dispatched {
		t.Fatalf("journal lost %+v %v", loaded, err)
	}
}

func TestProcessMigrationFromSchemaFiveRetainsExistingSources(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:5] {
		if _, err := db.ExecContext(ctx, migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	const at = "2026-08-28T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO groups(id,name,default_source_id,revision,created_at,updated_at) VALUES('g','Group','',1,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(id,group_id,name,endpoint,security_policy,security_mode,enabled,catalog_state,revision,created_at,updated_at) VALUES('old','g','Old','opc.tcp://test:4840','None','None',1,'Ready',9,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	binding, err := r.GetProcessBinding(ctx, "old")
	if err != nil || binding.SourceRevision != 9 || binding.Revision != 0 {
		t.Fatalf("legacy binding %+v %v", binding, err)
	}
	var version int
	if err := r.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("schema %d %v", version, err)
	}
}

func TestProcessJournalConcurrentCASCannotRegressTerminal(t *testing.T) {
	ctx := context.Background()
	r, err := Open(ctx, filepath.Join(t.TempDir(), "concurrent.db"), PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateSource(ctx, domain.Source{ID: "a", GroupID: "g", Name: "A", Endpoint: "opc.tcp://test:4840", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	binding, err := r.SaveProcessBinding(ctx, domain.ProcessBinding{SourceID: "a", SourceRevision: 1, AgentURL: "http://agent", InstanceID: "a"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	record := processStoreRecord(binding, "race")
	record.Dispatched = true
	if err := r.CreateProcessOperation(ctx, record); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 16)
	var workers sync.WaitGroup
	for index := 0; index < 16; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			candidate := record
			candidate.Operation.State = "unknown"
			if index == 0 {
				candidate.Operation.State = "failed"
			}
			_, _, err := r.UpdateProcessOperation(ctx, candidate)
			errs <- err
		}(index)
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	final, err := r.GetProcessOperation(ctx, "a", "race")
	if err != nil || final.Operation.State != "failed" || !final.Dispatched {
		t.Fatalf("terminal lost %+v %v", final, err)
	}
}
