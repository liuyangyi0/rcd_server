package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestCatalogQuarantineRoundTripAndAtomicClear(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if _, err := repo.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://test", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	node := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "P"}, BrowseName: "P", Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1}
	quarantined := domain.SourceCatalog{SourceID: "s", GroupID: "g", State: domain.CatalogReady,
		Nodes: []domain.CatalogNode{node}, QuarantineReason: "invalid lower topology"}
	if err := repo.ReplaceSourceCatalog(ctx, quarantined); err != nil {
		t.Fatal(err)
	}
	saved, err := repo.LoadSourceCatalog(ctx, "s")
	if err != nil || saved.State != domain.CatalogConflict || saved.QuarantineReason != quarantined.QuarantineReason {
		t.Fatalf("quarantine did not persist as a conflict: state=%q reason=%q err=%v", saved.State, saved.QuarantineReason, err)
	}

	// A failed replacement may not clear the durable reason while leaving
	// the old cache. The marker, nodes, manifest, and state share a transaction.
	replacement := quarantined
	replacement.QuarantineReason = ""
	replacement.Nodes = []domain.CatalogNode{node, node}
	if err := repo.ReplaceSourceCatalog(ctx, replacement); err == nil {
		t.Fatal("duplicate replacement unexpectedly succeeded")
	}
	saved, err = repo.LoadSourceCatalog(ctx, "s")
	if err != nil || saved.QuarantineReason != quarantined.QuarantineReason || saved.State != domain.CatalogConflict || len(saved.Nodes) != 1 {
		t.Fatalf("failed discovery partially cleared quarantine: catalog=%+v err=%v", saved, err)
	}
	replacement.Nodes = []domain.CatalogNode{node}
	if err := repo.ReplaceSourceCatalog(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	saved, err = repo.LoadSourceCatalog(ctx, "s")
	if err != nil || saved.QuarantineReason != "" || saved.State != domain.CatalogReady || len(saved.Nodes) != 1 {
		t.Fatalf("successful discovery did not atomically replace quarantine: catalog=%+v err=%v", saved, err)
	}
}

func TestCatalogResetPreservesExplicitQuarantine(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	if _, err := repo.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"}); err != nil {
		t.Fatal(err)
	}
	source, err := repo.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://old", Enabled: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: "s", GroupID: "g", State: domain.CatalogConflict,
		QuarantineReason: "invalid manifest", Topology: &domain.SourceTopology{SchemaVersion: 99}}); err != nil {
		t.Fatal(err)
	}
	source.Endpoint = "opc.tcp://replacement"
	updated, err := repo.UpdateSourceAndResetCatalog(ctx, source, nil, 1)
	if err != nil || updated.CatalogState != domain.CatalogConflict {
		t.Fatalf("endpoint edit cleared rejection: state=%q err=%v", updated.CatalogState, err)
	}
	catalog, err := repo.LoadSourceCatalog(ctx, "s")
	if err != nil || catalog.QuarantineReason != "invalid manifest" || catalog.Topology != nil || len(catalog.Nodes) != 0 || catalog.State != domain.CatalogConflict {
		t.Fatalf("reset cache must drop old metadata but retain quarantine: catalog=%+v err=%v", catalog, err)
	}
}

func TestSchemaFourMigratesOnlyExplicitLatestQuarantineEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, migration := range migrations[:4] {
		if _, err := db.ExecContext(ctx, migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	const at = "2026-01-01T00:00:00Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO groups (id,name,default_source_id,revision,created_at,updated_at) VALUES ('g','Group','',1,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id       string
		state    domain.CatalogState
		messages []string
		want     string
		noOldRow bool
	}{
		{id: "rejected", state: domain.CatalogConflict, messages: []string{"catalog rejected: bad manifest"}, want: "bad manifest"},
		{id: "unknown", state: domain.CatalogConflict},
		{id: "peer-conflict", state: domain.CatalogConflict, messages: []string{"catalog conflict: TypeMismatch P (a,b)"}},
		{id: "stale-rejection", state: domain.CatalogConflict, messages: []string{"catalog rejected: old error", "catalog conflict: TypeMismatch P (a,b)"}},
		{id: "healthy", state: domain.CatalogCached, messages: []string{"catalog rejected: historical error"}},
		{id: "uncertain-history", state: domain.CatalogConflict, messages: []string{"catalog rejected: old error", "connection reset"}},
		{id: "empty-reason", state: domain.CatalogConflict, messages: []string{"catalog rejected: "}},
		{id: "first-discovery-rejected", state: domain.CatalogConflict, messages: []string{"catalog rejected: first manifest invalid"}, want: "first manifest invalid", noOldRow: true},
	}
	for _, test := range cases {
		if _, err := db.ExecContext(ctx, `INSERT INTO sources
			(id,group_id,name,endpoint,security_policy,security_mode,enabled,catalog_state,revision,created_at,updated_at)
			VALUES (?,'g',?,'opc.tcp://test','None','None',1,?,1,?,?)`, test.id, test.id, test.state, at, at); err != nil {
			t.Fatal(err)
		}
		if !test.noOldRow {
			if _, err := db.ExecContext(ctx, `INSERT INTO source_topology (source_id,manifest,discovered_at) VALUES (?, ?, ?)`,
				test.id, `{"schema_version":1,"ports":[],"calculations":[]}`, at); err != nil {
				t.Fatal(err)
			}
		}
		for index, message := range test.messages {
			occurred := time.Date(2026, 1, 1, 0, 0, index+1, 0, time.UTC).Format(time.RFC3339Nano)
			if _, err := db.ExecContext(ctx, `INSERT INTO source_events
				(source_id,kind,current_value,message,occurred_at) VALUES (?,'error',?,?,?)`, test.id, message, message, occurred); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, test := range cases {
		catalog, err := repo.LoadSourceCatalog(ctx, test.id)
		if err != nil || catalog.QuarantineReason != test.want || catalog.State != test.state {
			t.Errorf("migration %s: state=%q reason=%q err=%v; want state=%q reason=%q", test.id, catalog.State, catalog.QuarantineReason, err, test.state, test.want)
		}
		if !test.noOldRow && (catalog.Topology == nil || catalog.Topology.SchemaVersion != 1) {
			t.Errorf("migration replaced existing manifest for %s: %+v", test.id, catalog.Topology)
		}
	}
	var version int
	if err := repo.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("migrated version=%d err=%v; want %d", version, err, len(migrations))
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	catalog, err := reopened.LoadSourceCatalog(ctx, "rejected")
	if err != nil || catalog.QuarantineReason != "bad manifest" {
		t.Fatalf("quarantine changed on second open: reason=%q err=%v", catalog.QuarantineReason, err)
	}
}
