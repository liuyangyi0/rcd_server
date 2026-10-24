package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"rcd_manager/internal/domain"
	"testing"
)

func TestDeviceTopologyAndBindingsSurviveRepositoryReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	repo, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fallback", "device"} {
		if _, err := repo.CreateGroup(ctx, domain.Group{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	source, err := repo.CreateSource(ctx, domain.Source{ID: "s", GroupID: "fallback", Name: "source", Endpoint: "opc.tcp://test", Enabled: true, GroupBindings: []domain.GroupBinding{{GroupID: "device", PortID: "/dev/ttyA", DeviceID: "1"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "P"}, GroupID: "device", PortID: "/dev/ttyA", DeviceID: "1", Kind: "point", Class: domain.NodeClassVariable, DataType: domain.DataTypeDouble, ValueRank: -1}
	topology := &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "/dev/ttyA", PortName: "ttyA", SendNodeID: "ttyA_Send", SendMethodID: "ttyA_SendCommand", Devices: []domain.TopologyDevice{{DeviceID: "1", DeviceAddress: 1, NodeIDs: []string{"P"}}}}}}
	if err := repo.ReplaceSourceCatalog(ctx, domain.SourceCatalog{SourceID: source.ID, GroupID: source.GroupID, State: domain.CatalogReady, Nodes: []domain.CatalogNode{node}, Topology: topology}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	loaded, err := repo.GetSource(ctx, "s")
	if err != nil || !loaded.HasGroup("device") {
		t.Fatalf("source=%#v err=%v", loaded, err)
	}
	catalog, err := repo.LoadSourceCatalog(ctx, "s")
	if err != nil || catalog.Topology == nil || catalog.Topology.Ports[0].Devices[0].NodeIDs[0] != "P" || catalog.Nodes[0].GroupID != "device" {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
	records, total, err := repo.ListCatalogRecords(ctx, "device", "", 10, 0)
	if err != nil || total != 1 || records[0].Node.PortID != "/dev/ttyA" {
		t.Fatalf("records=%#v total=%d err=%v", records, total, err)
	}
}

func TestSchemaThreeMigratesLegacySourceWithoutChangingGrouping(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:3] {
		if _, err := db.ExecContext(ctx, migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO groups VALUES('g','legacy','',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sources VALUES('s','g','legacy','opc.tcp://x','None','None','',NULL,1,'Cached',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	source, err := repo.GetSource(ctx, "s")
	if err != nil || source.GroupID != "g" || len(source.GroupBindings) != 0 {
		t.Fatalf("source=%#v err=%v", source, err)
	}
	var version int
	if err := repo.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
