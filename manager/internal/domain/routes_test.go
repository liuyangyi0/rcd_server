package domain

import (
	"errors"
	"testing"
	"time"
)

func testRouteTable(t *testing.T) *RouteTable {
	t.Helper()
	table, err := NewRouteTable(
		[]Group{{ID: "g", DefaultSourceID: "a", Revision: 2}},
		[]Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogCached},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestRouteTableKeepsSelectionsPerSession(t *testing.T) {
	table := testRouteTable(t)
	left, _ := table.OpenSession("left")
	_, _ = table.OpenSession("right")

	selected, err := table.Select("left", "g", "b", left.Routes["g"].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if selected.SourceID != "b" {
		t.Fatalf("selected = %q", selected.SourceID)
	}
	rightAfter, _ := table.Session("right")
	if rightAfter.Routes["g"].SourceID != "a" {
		t.Fatalf("other session changed: %#v", rightAfter.Routes["g"])
	}

	reattached, _ := table.OpenSession("left")
	if reattached.Routes["g"].SourceID != "b" {
		t.Fatalf("reattach lost selection: %#v", reattached.Routes["g"])
	}
	table.CloseSession("left")
	newSession, _ := table.OpenSession("left")
	if newSession.Routes["g"].SourceID != "a" {
		t.Fatalf("new session did not copy default: %#v", newSession.Routes["g"])
	}
}

func TestRouteReturnsOneSessionRoute(t *testing.T) {
	table, err := NewRouteTable([]Group{{ID: "g", DefaultSourceID: "a"}}, []Source{{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.OpenSession("s"); err != nil {
		t.Fatal(err)
	}
	route, err := table.Route("s", "g")
	if err != nil || route.SourceID != "a" || route.Revision != 1 {
		t.Fatalf("route=%#v err=%v", route, err)
	}
}

func TestPendingCommandLocksSelectionButPinsSource(t *testing.T) {
	table := testRouteTable(t)
	session, _ := table.OpenSession("s")
	command, err := table.BeginCommandWithDetails("s", "g", "COM1", "write", "COM1_Send", "440102")
	if err != nil {
		t.Fatal(err)
	}
	if command.SourceID != "a" || command.NodeID != "COM1_Send" || command.Request != "440102" {
		t.Fatalf("command source = %q", command.SourceID)
	}
	_, err = table.Select("s", "g", "b", session.Routes["g"].Revision)
	if !errors.Is(err, ErrGroupPending) {
		t.Fatalf("select error = %v, want pending", err)
	}
	_, err = table.SetDefault("g", "b", 2)
	if !errors.Is(err, ErrGroupPending) {
		t.Fatalf("default error = %v, want pending", err)
	}
	finished, err := table.FinishCommand(command.ID, CommandReceived, StatusGood, "")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Response != "1" {
		t.Fatalf("command response = %q, want ACK 1", finished.Response)
	}
	if _, err := table.Select("s", "g", "b", session.Routes["g"].Revision); err != nil {
		t.Fatalf("select after finish: %v", err)
	}
}

func TestRouteTableChecksRevision(t *testing.T) {
	table := testRouteTable(t)
	table.OpenSession("s")
	_, err := table.Select("s", "g", "b", 99)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("error = %v, want revision conflict", err)
	}
}

func TestDefaultChangeOnlyAffectsNewSessions(t *testing.T) {
	table := testRouteTable(t)
	existing, _ := table.OpenSession("existing")
	if _, err := table.SetDefault("g", "b", 2); err != nil {
		t.Fatal(err)
	}
	stillExisting, _ := table.Session("existing")
	if stillExisting.Routes["g"].SourceID != existing.Routes["g"].SourceID {
		t.Fatalf("existing route changed: before=%#v after=%#v", existing.Routes["g"], stillExisting.Routes["g"])
	}
	created, _ := table.OpenSession("new")
	if created.Routes["g"].SourceID != "b" {
		t.Fatalf("new session route = %#v", created.Routes["g"])
	}
}

func TestConfiguredUnselectableDefaultIsNotCopiedToSession(t *testing.T) {
	table, err := NewRouteTable(
		[]Group{{ID: "g", DefaultSourceID: "a", Revision: 1}},
		[]Source{{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogConflict}, {ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady}},
	)
	if err != nil {
		t.Fatalf("construct with conflicted configured default: %v", err)
	}
	session, err := table.OpenSession("s")
	if err != nil || session.Routes["g"].SourceID != "" {
		t.Fatalf("session=%#v err=%v", session, err)
	}
	if _, err := table.Select("s", "g", "a", session.Routes["g"].Revision); !errors.Is(err, ErrSourceNotSelectable) {
		t.Fatalf("select conflicted source error = %v", err)
	}
	if _, err := table.SetDefault("g", "a", 1); !errors.Is(err, ErrSourceNotSelectable) {
		t.Fatalf("set conflicted default error = %v", err)
	}
}

func TestApplyConfigurationInvalidatesRouteThatBecomesUnselectable(t *testing.T) {
	table := testRouteTable(t)
	session, _ := table.OpenSession("s")
	before := session.Routes["g"]
	err := table.ApplyConfiguration(
		[]Group{{ID: "g", DefaultSourceID: "a", Revision: 2}},
		[]Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogConflict},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := table.Session("s")
	if after.Routes["g"].SourceID != "" || after.Routes["g"].Revision != before.Revision+1 {
		t.Fatalf("route after source conflict = %#v", after.Routes["g"])
	}
}

func TestApplyConfigurationKeepsExistingRoutesAndAddsNewGroups(t *testing.T) {
	table := testRouteTable(t)
	session, _ := table.OpenSession("s")
	if _, err := table.Select("s", "g", "b", session.Routes["g"].Revision); err != nil {
		t.Fatal(err)
	}
	err := table.ApplyConfiguration(
		[]Group{{ID: "g", DefaultSourceID: "a", Revision: 3}, {ID: "new", DefaultSourceID: "c", Revision: 1}},
		[]Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
			{ID: "c", GroupID: "new", Enabled: true, CatalogState: CatalogReady},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := table.Session("s")
	if updated.Routes["g"].SourceID != "b" || updated.Routes["new"].SourceID != "c" {
		t.Fatalf("routes = %#v", updated.Routes)
	}
}

func TestApplyConfigurationRejectsRemovingSelectedSource(t *testing.T) {
	table := testRouteTable(t)
	table.OpenSession("s")
	err := table.ApplyConfiguration(
		[]Group{{ID: "g", DefaultSourceID: "b", Revision: 3}},
		[]Source{{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady}},
	)
	if !errors.Is(err, ErrSourceInUse) {
		t.Fatalf("error = %v, want source in use", err)
	}
}

func TestApplyConfigurationCannotBypassPendingDefaultLock(t *testing.T) {
	table := testRouteTable(t)
	table.OpenSession("s")
	if _, err := table.BeginCommand("s", "g", "COM1", "write"); err != nil {
		t.Fatal(err)
	}
	err := table.ApplyConfiguration(
		[]Group{{ID: "g", DefaultSourceID: "b", Revision: 3}},
		[]Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
		},
	)
	if !errors.Is(err, ErrGroupPending) {
		t.Fatalf("error = %v, want pending lock", err)
	}
}

func TestApplyConfigurationDoesNotRewriteSelectionWhileCommandPending(t *testing.T) {
	table := testRouteTable(t)
	session, err := table.OpenSession("s")
	if err != nil {
		t.Fatal(err)
	}
	command, err := table.BeginCommand("s", "g", "COM1", "write")
	if err != nil {
		t.Fatal(err)
	}
	err = table.ApplyConfiguration(
		[]Group{{ID: "g", DefaultSourceID: "a", Revision: 2}},
		[]Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogConflict},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := table.Session("s")
	if err != nil {
		t.Fatal(err)
	}
	if after.Routes["g"] != session.Routes["g"] {
		t.Fatalf("pending command changed route: before=%#v after=%#v", session.Routes["g"], after.Routes["g"])
	}
	if _, err := table.PrepareCommandFinish(command.ID, CommandFailed, StatusBadUnexpectedError, "catalog conflict", time.Now()); err != nil {
		t.Fatal(err)
	}
	changes, err := table.CommitCommandFinish(command.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].SessionID != "s" || changes[0].Route.SourceID != "" ||
		changes[0].Route.Revision != session.Routes["g"].Revision+1 {
		t.Fatalf("deferred route invalidation = %#v", changes)
	}
	final, err := table.Session("s")
	if err != nil || final.Routes["g"] != changes[0].Route {
		t.Fatalf("final session = %#v, err=%v", final, err)
	}
}

func TestCommitConfigurationPublishesOnlyAfterPersistence(t *testing.T) {
	table := testRouteTable(t)
	session, err := table.OpenSession("s")
	if err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("database unavailable")
	err = table.CommitConfiguration(
		[]Group{{ID: "g", DefaultSourceID: "a", Revision: 2}, {ID: "ghost", Revision: 1}},
		[]Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogCached},
		},
		func() error { return persistErr },
	)
	if !errors.Is(err, persistErr) {
		t.Fatalf("commit error = %v", err)
	}
	if len(table.Groups()) != 1 {
		t.Fatalf("failed persistence changed groups: %#v", table.Groups())
	}
	after, err := table.Session("s")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := after.Routes["ghost"]; exists || after.Routes["g"] != session.Routes["g"] {
		t.Fatalf("failed persistence changed session routes: %#v", after.Routes)
	}
}

func TestCommitConfigurationDoesNotBlockHotSessionReads(t *testing.T) {
	table := testRouteTable(t)
	if _, err := table.OpenSession("s"); err != nil {
		t.Fatal(err)
	}
	persisting := make(chan struct{})
	release := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- table.CommitConfiguration(
			[]Group{{ID: "g", DefaultSourceID: "a", Revision: 3}},
			[]Source{
				{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady},
				{ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogCached},
			},
			func() error {
				close(persisting)
				<-release
				return nil
			},
		)
	}()
	<-persisting
	readDone := make(chan error, 1)
	go func() {
		_, err := table.Session("s")
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("hot Session read blocked behind persistence")
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
}

func TestCommitConfigurationKeepsValidatedCatalogStateOverStalePersistence(t *testing.T) {
	table := testRouteTable(t)
	groups := table.Groups()
	sources := table.Sources()
	for index := range sources {
		if sources[index].ID == "a" {
			sources[index].CatalogState = CatalogReady
		}
	}
	if err := table.CommitConfiguration(groups, sources, func() error {
		// Simulate a metadata update read back while a prior dirty catalog write
		// has not yet updated SQLite's catalog_state column.
		for index := range sources {
			if sources[index].ID == "a" {
				sources[index].CatalogState = CatalogDiscovering
				sources[index].Revision++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	updated, ok := sourceForTest(table.Sources(), "a")
	if !ok || updated.CatalogState != CatalogReady || updated.Revision != 1 {
		t.Fatalf("updated source = %#v, exists=%v", updated, ok)
	}
}

func sourceForTest(sources []Source, id string) (Source, bool) {
	for _, source := range sources {
		if source.ID == id {
			return source, true
		}
	}
	return Source{}, false
}

func TestFinishBySourcePortAndSweepTimeout(t *testing.T) {
	table := testRouteTable(t)
	table.OpenSession("s")
	first, _ := table.BeginCommand("s", "g", "COM1", "write")
	finished, err := table.FinishCommandBySourcePort(first.SourceID, first.Port, CommandReceived, StatusGood, "")
	if err != nil || finished.ID != first.ID {
		t.Fatalf("finished = %#v, err = %v", finished, err)
	}
	table.clock = func() time.Time { return time.Unix(100, 0) }
	_, _ = table.BeginCommand("s", "g", "COM2", "write")
	expired := table.SweepExpired(time.Unix(111, 0), 10*time.Second)
	if len(expired) != 1 || expired[0].State != CommandTimedOut || table.PendingCount("g") != 0 {
		t.Fatalf("expired = %#v pending=%d", expired, table.PendingCount("g"))
	}
}

func TestRestorePendingDoesNotRequireOriginalSession(t *testing.T) {
	table := testRouteTable(t)
	command := PendingCommand{
		ID: "persisted", SessionID: "closed", GroupID: "g", SourceID: "a", Port: "COM1",
		Kind: "write", State: CommandWaiting, StartedAt: time.Unix(10, 0),
	}
	if err := table.RestorePending(command); err != nil {
		t.Fatal(err)
	}
	if table.PendingCount("g") != 1 {
		t.Fatalf("pending = %d", table.PendingCount("g"))
	}
	finished, err := table.FinishCommandBySourcePort("a", "COM1", CommandReceived, StatusGood, "")
	if err != nil || finished.ID != "persisted" {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
}

func TestInvalidTerminalStateDoesNotReleasePendingLock(t *testing.T) {
	table := testRouteTable(t)
	table.OpenSession("s")
	command, err := table.BeginCommand("s", "g", "COM1", "Write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.FinishCommand(command.ID, CommandWaiting, StatusBadWaitingForResponse, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("finish error = %v, want invalid", err)
	}
	if table.PendingCount("g") != 1 {
		t.Fatal("invalid finish released the group lock")
	}
}
