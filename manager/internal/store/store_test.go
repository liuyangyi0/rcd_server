package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func openTestRepository(t *testing.T) *Repository {
	t.Helper()
	repository, err := Open(context.Background(), "file:"+t.Name()+"?mode=memory&cache=shared", PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func TestRepositoryCRUDAndOptimisticRevision(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	group, err := r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"})
	if err != nil {
		t.Fatal(err)
	}
	if group.Revision != 1 {
		t.Fatalf("group revision = %d", group.Revision)
	}
	source, err := r.CreateSource(ctx, domain.Source{
		ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://127.0.0.1:4840",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
	}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !source.HasPassword {
		t.Fatal("password presence was not recorded")
	}
	username, password, err := r.SourceCredential(ctx, "s")
	if err != nil || username != "" || password != "secret" {
		t.Fatalf("credential = %q/%q, %v", username, password, err)
	}
	source.Name = "Updated"
	updated, err := r.UpdateSource(ctx, source, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Name != "Updated" {
		t.Fatalf("updated = %#v", updated)
	}
	_, err = r.UpdateSource(ctx, source, nil, 1)
	if !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("error = %v, want revision conflict", err)
	}
}

func TestRepositoryConfiguresEverySQLiteConnection(t *testing.T) {
	ctx := context.Background()
	r, err := Open(ctx, filepath.Join(t.TempDir(), "manager.db"), PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for range 4 {
		connection, err := r.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		var foreignKeys int
		var journalMode string
		if err := connection.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := connection.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || journalMode != "wal" {
			t.Fatalf("foreign_keys=%d journal_mode=%q", foreignKeys, journalMode)
		}
	}
}

func TestCreateSourceRejectsMissingGroupAsInvalid(t *testing.T) {
	r := openTestRepository(t)
	_, err := r.CreateSource(context.Background(), domain.Source{ID: "s", GroupID: "missing", Name: "Source", Endpoint: "opc.tcp://x"}, "")
	if !errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("error = %v, want invalid missing group", err)
	}
}

func TestRepositoryCatalogAndAuditRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	_, _ = r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"})
	_, _ = r.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://x", Enabled: true}, "")
	discovered := time.Now().UTC().Truncate(time.Microsecond)
	catalog := domain.SourceCatalog{SourceID: "s", GroupID: "g", State: domain.CatalogReady, DiscoveredAt: discovered,
		Nodes: []domain.CatalogNode{{Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "KKS"}, BrowseName: "KKS", Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1}}}
	if err := r.ReplaceSourceCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.LoadSourceCatalog(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].Key.Identifier != "KKS" || loaded.State != domain.CatalogReady {
		t.Fatalf("loaded catalog = %#v", loaded)
	}

	command := domain.PendingCommand{ID: "c", SessionID: "session", GroupID: "g", SourceID: "s", Port: "COM1", Kind: "write", State: domain.CommandWaiting, StartedAt: discovered}
	if err := r.SaveCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
	command.State = domain.CommandReceived
	command.FinishedAt = discovered.Add(time.Second)
	if err := r.SaveCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
	commands, total, err := r.ListCommands(ctx, 10, 0)
	if err != nil || total != 1 || commands[0].State != domain.CommandReceived {
		t.Fatalf("commands = %#v total=%d err=%v", commands, total, err)
	}
	commands, total, err = r.ListCommandsFiltered(ctx, CommandFilter{GroupID: "other", Limit: 10})
	if err != nil || total != 0 || len(commands) != 0 {
		t.Fatalf("filtered commands = %#v total=%d err=%v", commands, total, err)
	}
	records, total, err := r.ListCatalogRecordsFiltered(ctx, CatalogFilter{GroupID: "g", SourceID: "s", Query: "KK", Limit: 10})
	if err != nil || total != 1 || len(records) != 1 {
		t.Fatalf("filtered catalog = %#v total=%d err=%v", records, total, err)
	}
}

func TestPendingCommandSurvivesReopenAndRestoresGroupLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	r, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	group, err := r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group", DefaultSourceID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := r.CreateSource(ctx, domain.Source{
			ID: id, GroupID: "g", Name: id, Endpoint: "opc.tcp://" + id,
			Enabled: true, CatalogState: domain.CatalogReady,
		}, ""); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now().UTC().Add(-time.Second)
	waiting := domain.PendingCommand{
		ID: "persisted", SessionID: "old-session", GroupID: "g", SourceID: "a", Port: "COM1",
		Kind: "Write", State: domain.CommandWaiting, StatusCode: domain.StatusBadWaitingForResponse, StartedAt: started,
	}
	if err := r.SaveCommand(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sources, err := r.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending, total, err := r.ListCommandsFiltered(ctx, CommandFilter{Status: string(domain.CommandWaiting), Limit: 100})
	if err != nil || total != 1 || len(pending) != 1 {
		t.Fatalf("pending=%#v total=%d err=%v", pending, total, err)
	}
	routes, err := domain.NewRouteTable([]domain.Group{group}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if err := routes.RestorePending(pending[0]); err != nil {
		t.Fatal(err)
	}
	session, _ := routes.OpenSession("new-session")
	if _, err := routes.Select("new-session", "g", "b", session.Routes["g"].Revision); !errors.Is(err, domain.ErrGroupPending) {
		t.Fatalf("selection error = %v, want restored pending lock", err)
	}
}

func TestReplaceCatalogRollsBackOnInvalidReplacement(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	_, _ = r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"})
	_, _ = r.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://s", Enabled: true}, "")
	oldNode := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "OLD"}, BrowseName: "OLD", Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1}
	if err := r.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: "s", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{oldNode}}); err != nil {
		t.Fatal(err)
	}
	duplicate := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "DUP"}, BrowseName: "DUP", Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1}
	err := r.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: "s", GroupID: "g", State: domain.CatalogConflict, Nodes: []domain.CatalogNode{duplicate, duplicate}})
	if err == nil {
		t.Fatal("invalid replacement unexpectedly succeeded")
	}
	loaded, err := r.LoadSourceCatalog(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != domain.CatalogReady || len(loaded.Nodes) != 1 || loaded.Nodes[0].Key.Identifier != "OLD" {
		t.Fatalf("catalog was not rolled back: %#v", loaded)
	}
}

func TestUpdateSourceAndResetCatalogIsAtomic(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	_, _ = r.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"})
	source, _ := r.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://old", Enabled: true}, "secret")
	node := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "KKS"}, BrowseName: "KKS", Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1}
	if err := r.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: "s", GroupID: "g", State: domain.CatalogReady, Nodes: []domain.CatalogNode{node}}); err != nil {
		t.Fatal(err)
	}
	source.Endpoint = "opc.tcp://new"
	if _, err := r.UpdateSourceAndResetCatalog(ctx, source, nil, 99); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale reset error = %v, want revision conflict", err)
	}
	unchanged, err := r.LoadSourceCatalog(ctx, "s")
	if err != nil || len(unchanged.Nodes) != 1 || unchanged.Nodes[0].Key.Identifier != "KKS" {
		t.Fatalf("stale reset changed catalog: %#v err=%v", unchanged, err)
	}
	updated, err := r.UpdateSourceAndResetCatalog(ctx, source, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.CatalogState != domain.CatalogDiscovering || updated.Endpoint != "opc.tcp://new" {
		t.Fatalf("updated source = %#v", updated)
	}
	catalog, err := r.LoadSourceCatalog(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Nodes) != 0 || catalog.State != domain.CatalogDiscovering {
		t.Fatalf("catalog was not reset: %#v", catalog)
	}
	_, password, err := r.SourceCredential(ctx, "s")
	if err != nil || password != "secret" {
		t.Fatalf("preserved password = %q, err = %v", password, err)
	}
}

func TestOperationalHistoryAlarmLifecycleAndRevision(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	r.clock = func() time.Time { return now }

	event, err := r.AppendSourceEvent(ctx, domain.SourceEvent{SourceID: "source-a", Kind: "connection", Previous: "Connected", Current: "Disconnected", Message: "connection lost"})
	if err != nil || event.ID == 0 || !event.OccurredAt.Equal(now) {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	events, total, err := r.ListSourceEvents(ctx, SourceEventFilter{SourceID: "source-a", Query: "lost", Limit: 10})
	if err != nil || total != 1 || len(events) != 1 || events[0].Current != "Disconnected" {
		t.Fatalf("events=%#v total=%d err=%v", events, total, err)
	}

	alarm, changed, err := r.ReconcileAlarm(ctx, "source-a", "source.connection", domain.AlarmSeverityCritical, "connection lost", true)
	if err != nil || !changed || alarm.ID == 0 || alarm.State != domain.AlarmOpen || alarm.Occurrences != 1 || alarm.Revision != 1 {
		t.Fatalf("opened alarm=%#v changed=%v err=%v", alarm, changed, err)
	}
	now = now.Add(time.Minute)
	updated, changed, err := r.ReconcileAlarm(ctx, "source-a", "source.connection", domain.AlarmSeverityCritical, "retry failed", true)
	if err != nil || !changed || updated.ID != alarm.ID || updated.Occurrences != 2 || updated.Revision != 2 || updated.Message != "retry failed" {
		t.Fatalf("updated alarm=%#v changed=%v err=%v", updated, changed, err)
	}

	acknowledged, err := r.AcknowledgeAlarm(ctx, alarm.ID, updated.Revision, "operator-a")
	if err != nil || acknowledged.AcknowledgedBy != "operator-a" || acknowledged.Revision != 3 {
		t.Fatalf("acknowledged=%#v err=%v", acknowledged, err)
	}
	if _, err := r.AcknowledgeAlarm(ctx, alarm.ID, updated.Revision, "stale"); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale acknowledgement error=%v", err)
	}

	silenced, err := r.SilenceAlarm(ctx, alarm.ID, acknowledged.Revision, now.Add(time.Hour))
	if err != nil || silenced.Revision != 4 || silenced.SilencedUntil.IsZero() {
		t.Fatalf("silenced=%#v err=%v", silenced, err)
	}
	open, unacknowledged, err := r.AlarmCounts(ctx)
	if err != nil || open != 0 || unacknowledged != 0 {
		t.Fatalf("silenced counts open=%d unacknowledged=%d err=%v", open, unacknowledged, err)
	}

	resolved, changed, err := r.ReconcileAlarm(ctx, "source-a", "source.connection", domain.AlarmSeverityCritical, "", false)
	if err != nil || !changed || resolved.State != domain.AlarmResolved || resolved.Revision != 5 || resolved.ResolvedAt.IsZero() {
		t.Fatalf("resolved=%#v changed=%v err=%v", resolved, changed, err)
	}
	reopened, changed, err := r.ReconcileAlarm(ctx, "source-a", "source.connection", domain.AlarmSeverityCritical, "lost again", true)
	if err != nil || !changed || reopened.ID == alarm.ID || reopened.State != domain.AlarmOpen {
		t.Fatalf("reopened=%#v changed=%v err=%v", reopened, changed, err)
	}
	alarms, total, err := r.ListAlarms(ctx, AlarmFilter{SourceID: "source-a", Limit: 10})
	if err != nil || total != 2 || len(alarms) != 2 || alarms[0].ID != reopened.ID {
		t.Fatalf("alarms=%#v total=%d err=%v", alarms, total, err)
	}
}

func TestPruneOperationalHistoryRetainsOpenAndWaitingRecords(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	r.clock = func() time.Time { return now }
	old := now.Add(-100 * 24 * time.Hour)
	current := now

	_, _ = r.AppendSourceEvent(ctx, domain.SourceEvent{SourceID: "source-a", Kind: "connection", Current: "Disconnected", OccurredAt: old})
	_, _ = r.AppendAudit(ctx, domain.AuditEntry{Kind: "source.update", Actor: "test", Target: "source-a", Revision: 1, OccurredAt: old})
	if err := r.SaveCommand(ctx, domain.PendingCommand{ID: "waiting", SourceID: "source-a", GroupID: "g", Port: "COM1", Kind: "Write", State: domain.CommandWaiting, StartedAt: old}); err != nil {
		t.Fatal(err)
	}
	now = old
	resolved, _, err := r.ReconcileAlarm(ctx, "source-a", "source.connection", domain.AlarmSeverityCritical, "lost", true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, _, err := r.ReconcileAlarm(ctx, "source-a", "source.connection", domain.AlarmSeverityCritical, "", false); err != nil {
		t.Fatal(err)
	}
	if resolved.ID == 0 {
		t.Fatal("alarm was not created")
	}

	now = current
	if err := r.PruneOperationalHistory(ctx, now.Add(-90*24*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	events, total, _ := r.ListSourceEvents(ctx, SourceEventFilter{Limit: 10})
	if total != 0 || len(events) != 0 {
		t.Fatalf("old source events retained: %#v", events)
	}
	audits, total, _ := r.ListAudit(ctx, 10, 0)
	if total != 0 || len(audits) != 0 {
		t.Fatalf("old audits retained: %#v", audits)
	}
	waiting, total, _ := r.ListCommandsFiltered(ctx, CommandFilter{Status: string(domain.CommandWaiting), Limit: 10})
	if total != 1 || len(waiting) != 1 {
		t.Fatalf("waiting commands were pruned: %#v", waiting)
	}
	alarms, total, _ := r.ListAlarms(ctx, AlarmFilter{Limit: 10})
	if total != 0 || len(alarms) != 0 {
		t.Fatalf("old resolved alarms retained: %#v", alarms)
	}
}
