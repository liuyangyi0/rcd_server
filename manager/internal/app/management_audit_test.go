package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
)

func auditFailure(context.Context, domain.AuditEntry) (domain.AuditEntry, error) {
	return domain.AuditEntry{}, errors.New("disk unavailable")
}

func auditSuccess(_ context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
	return entry, nil
}

func TestManagementAuditFreezesNestedValuesAndLargeNumbers(t *testing.T) {
	var audit managementAudit
	inner := map[string]any{"value": "original"}
	details := map[string]any{"inner": inner, "list": []string{"original"}, "large": uint64(18446744073709551615)}
	_, err := audit.save(context.Background(), domain.AuditEntry{Kind: "test", Details: details}, func(_ context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
		entry.Details["inner"].(map[string]any)["value"] = "writer mutation"
		return domain.AuditEntry{}, errors.New("failed write")
	})
	if err == nil {
		t.Fatal("failure missing")
	}
	inner["value"] = "caller mutation"
	details["list"].([]string)[0] = "caller mutation"
	delete(details, "large")
	if !audit.retry(context.Background(), func(_ context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
		if entry.OccurredAt.IsZero() {
			t.Error("time not frozen at admission")
		}
		if entry.Details["inner"].(map[string]any)["value"] != "original" || entry.Details["list"].([]any)[0] != "original" || entry.Details["large"].(json.Number).String() != "18446744073709551615" {
			t.Errorf("retry payload changed: %+v", entry.Details)
		}
		return entry, nil
	}) {
		t.Fatal("retry failed")
	}
	saved, err := audit.save(context.Background(), domain.AuditEntry{Details: map[string]any{"value": "original"}}, func(_ context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
		entry.Details["value"] = "writer mutation"
		return entry, nil
	})
	if err != nil || saved.Details["value"] != "original" {
		t.Fatal("writer mutated returned event")
	}
}

func TestManagementAuditSeparateFailuresAndSameTimestampWork(t *testing.T) {
	var audit managementAudit
	at := time.Now().UTC()
	for _, target := range []string{"a", "b"} {
		_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "same", Target: target, Revision: 1, OccurredAt: at}, auditFailure)
	}
	if n, _ := audit.state(); n != 2 {
		t.Fatal("same timestamp records collapsed")
	}
	audit.retry(context.Background(), func(ctx context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
		if entry.Target == "a" {
			return auditFailure(ctx, entry)
		}
		return entry, nil
	})
	if n, health := audit.state(); n != 1 || health != "disk unavailable" {
		t.Fatalf("success masked other failure: %d %q", n, health)
	}
	audit.retry(context.Background(), auditSuccess)
	if n, health := audit.state(); n != 0 || health != "" {
		t.Fatal("error not cleared after recovery")
	}
}

func TestManagementAuditConcurrentRetryHasOneOwner(t *testing.T) {
	var audit managementAudit
	_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "retry"}, auditFailure)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var writes atomic.Int32
	writer := func(_ context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
		writes.Add(1)
		close(entered)
		<-release
		return entry, nil
	}
	go func() { defer close(done); audit.retry(context.Background(), writer) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("retry not entered")
	}
	var attempts sync.WaitGroup
	for i := 0; i < 20; i++ {
		attempts.Add(1)
		go func() { defer attempts.Done(); audit.retry(context.Background(), writer); audit.state() }()
	}
	attempts.Wait()
	if writes.Load() != 1 {
		t.Fatal("concurrent retry duplicated save")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retry blocked")
	}
	if n, _ := audit.state(); n != 0 {
		t.Fatal("successful work retained")
	}
}

func TestManagementAuditInitialSaveCannotRaceBackgroundRetry(t *testing.T) {
	var audit managementAudit
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		defer close(done)
		_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "slow"}, func(_ context.Context, e domain.AuditEntry) (domain.AuditEntry, error) {
			close(entered)
			<-release
			return e, nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("save not entered")
	}
	responsive := make(chan struct{})
	go func() {
		defer close(responsive)
		audit.retry(context.Background(), func(context.Context, domain.AuditEntry) (domain.AuditEntry, error) {
			t.Error("background retried initial attempt")
			return domain.AuditEntry{}, nil
		})
		audit.state()
		_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "independent"}, auditSuccess)
	}()
	select {
	case <-responsive:
	case <-time.After(time.Second):
		t.Fatal("slow writer blocked state/other records")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if audit.drain(canceled, auditSuccess) {
		t.Fatal("canceled drain claimed completion")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("save blocked")
	}
}

func TestManagementAuditInvalidDetailsStayVisibleWithoutUnsafePointers(t *testing.T) {
	var audit managementAudit
	_, err := audit.save(context.Background(), domain.AuditEntry{Kind: "invalid", Details: map[string]any{"bad": func() {}}}, func(context.Context, domain.AuditEntry) (domain.AuditEntry, error) {
		t.Fatal("invalid details were silently discarded")
		return domain.AuditEntry{}, nil
	})
	if err == nil {
		t.Fatal("invalid details accepted")
	}
	_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "valid"}, auditSuccess)
	audit.retry(context.Background(), func(context.Context, domain.AuditEntry) (domain.AuditEntry, error) {
		t.Error("invalid snapshot retried as valid")
		return domain.AuditEntry{}, nil
	})
	if n, health := audit.state(); n != 1 || !strings.Contains(health, "encode audit details") {
		t.Fatalf("invalid record hidden: %d %s", n, health)
	}
}

func TestManagementAuditDrainWaitsForAdmittedFailureThenRetries(t *testing.T) {
	var audit managementAudit
	entered, release, saved := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		defer close(saved)
		_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "shutdown"}, func(ctx context.Context, e domain.AuditEntry) (domain.AuditEntry, error) {
			close(entered)
			<-release
			return auditFailure(ctx, e)
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initial attempt not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drained := make(chan bool, 1)
	go func() { drained <- audit.drain(ctx, auditSuccess) }()
	select {
	case <-drained:
		t.Fatal("drain skipped admitted save")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case ok := <-drained:
		if !ok {
			t.Fatal("failed initial save not retried")
		}
	case <-ctx.Done():
		t.Fatal("drain blocked")
	}
	<-saved
	if n, health := audit.state(); n != 0 || health != "" {
		t.Fatalf("drain left work: %d %s", n, health)
	}
}

func TestManagementAuditCanceledRetryAndDrainPreserveWork(t *testing.T) {
	var audit managementAudit
	_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "canceled"}, auditFailure)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := func(context.Context, domain.AuditEntry) (domain.AuditEntry, error) {
		t.Error("canceled flush called writer")
		return domain.AuditEntry{}, nil
	}
	audit.retry(ctx, writer)
	audit.drain(ctx, writer)
	if n, health := audit.state(); n != 1 || health != "disk unavailable" {
		t.Fatal("cancellation lost failure")
	}
	audit.retry(context.Background(), auditSuccess)
}

func TestManagementAuditRecordAdmittedDuringRetryRemainsPending(t *testing.T) {
	var audit managementAudit
	_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "old"}, auditFailure)
	audit.retry(context.Background(), func(_ context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
		_, _ = audit.save(context.Background(), domain.AuditEntry{Kind: "new"}, auditFailure)
		return entry, nil
	})
	if n, _ := audit.state(); n != 1 {
		t.Fatal("old pass consumed new failure")
	}
	if !audit.retry(context.Background(), auditSuccess) {
		t.Fatal("new record lost")
	}
}

func TestRuntimeManagementAuditSQLiteRecoveryUsesOriginalDetails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	repo, err := store.Open(ctx, path, store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	r, err := NewRuntime(ctx, repo, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_management_audit BEFORE INSERT ON management_audit BEGIN SELECT RAISE(FAIL,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	details := map[string]any{"nested": map[string]any{"original": true}, "large": uint64(18446744073709551615)}
	r.audit(ctx, "test.snapshot", "test", 1, details)
	details["nested"].(map[string]any)["original"] = false
	details["large"] = 0
	if r.Health(ctx).Checks["management_audit_persistence"] == "ok" {
		t.Fatal("save failure hidden")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_management_audit`); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); r.retryDirtyAudits(ctx) }()
	}
	workers.Wait()
	var body string
	var count int
	if err := db.QueryRow(`SELECT COUNT(*), details FROM management_audit WHERE kind='test.snapshot'`).Scan(&count, &body); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !strings.Contains(body, `"original":true`) || !strings.Contains(body, "18446744073709551615") {
		t.Fatalf("wrong persisted snapshot: %d %s", count, body)
	}
	if n, health := r.managementAudit.state(); n != 0 || health != "" {
		t.Fatalf("recovery incomplete: %d %s", n, health)
	}
}
