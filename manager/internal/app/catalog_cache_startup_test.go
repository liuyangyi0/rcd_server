package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
)

func TestRuntimeQuarantinesUnreadableCatalogCacheWithoutBlockingStartup(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "manager.db")
	repository, err := store.Open(ctx, databasePath, store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if _, err := repository.CreateGroup(ctx, domain.Group{ID: "line", Name: "Line"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSource(ctx, domain.Source{
		ID: "source-a", GroupID: "line", Name: "Source A", Endpoint: "opc.tcp://source-a:4840",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceSourceCatalog(ctx, domain.SourceCatalog{
		SourceID: "source-a", GroupID: "line", State: domain.CatalogCached,
		Nodes: []domain.CatalogNode{{
			Key:        domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "VALUE"},
			BrowseName: "VALUE", Class: domain.NodeClassVariable, DataType: domain.DataTypeUInt32, ValueRank: -1,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE source_topology SET manifest='{broken-json' WHERE source_id='source-a'`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_ = raw.Close()

	runtime, err := NewRuntime(ctx, repository, RuntimeOptions{CommandPendingTTL: time.Minute})
	if err != nil {
		t.Fatalf("one unreadable source cache blocked manager startup: %v", err)
	}
	assertSourceQuarantined(t, runtime, "source-a")
	if reason := runtime.quarantinedSources["source-a"]; !strings.Contains(reason, "cached catalog unreadable") {
		t.Fatalf("quarantine reason=%q", reason)
	}
	if runtime.CatalogUnion().SourceTopologies["source-a"] != nil {
		t.Fatal("unreadable cached topology was published upstream")
	}
	runtime.persistDirtyCatalogs(ctx)
	if repaired, err := repository.LoadSourceCatalog(ctx, "source-a"); err != nil || repaired.QuarantineReason == "" || repaired.Topology != nil {
		t.Fatalf("quarantined cache was not repaired atomically: catalog=%#v err=%v", repaired, err)
	}
}

func TestRuntimeDoesNotPublishSemanticallyInvalidCachedTopology(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(ctx, ":memory:", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if _, err := repository.CreateGroup(ctx, domain.Group{ID: "line", Name: "Line"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSource(ctx, domain.Source{
		ID: "source-a", GroupID: "line", Name: "Source A", Endpoint: "opc.tcp://source-a:4840",
		SecurityPolicy: "None", SecurityMode: "None", Enabled: true,
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceSourceCatalog(ctx, domain.SourceCatalog{
		SourceID: "source-a", GroupID: "line", State: domain.CatalogCached,
		Topology: &domain.SourceTopology{SchemaVersion: 99},
	}); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(ctx, repository, RuntimeOptions{})
	if err != nil {
		t.Fatalf("invalid source topology blocked manager startup: %v", err)
	}
	assertSourceQuarantined(t, runtime, "source-a")
	if runtime.catalogs["source-a"].Topology != nil || runtime.CatalogUnion().SourceTopologies["source-a"] != nil {
		t.Fatal("semantically invalid cached topology remained publishable")
	}
}

func TestCatalogConflictKeepsValidatedPhysicalTopologyForDiagnostics(t *testing.T) {
	catalogs := map[string]domain.SourceCatalog{}
	for _, item := range []struct {
		id       string
		dataType domain.DataType
		port     string
	}{{"source-a", domain.DataTypeUInt32, "serial-a"}, {"source-b", domain.DataTypeDouble, "serial-b"}} {
		catalogs[item.id] = domain.SourceCatalog{
			SourceID: item.id, GroupID: "line", State: domain.CatalogReady,
			Nodes: []domain.CatalogNode{{
				GroupID: "line", Key: domain.NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "VALUE"},
				Class: domain.NodeClassVariable, DataType: item.dataType, ValueRank: -1,
			}},
			Topology: &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{
				PortID: item.port, PortName: item.port,
				Devices: []domain.TopologyDevice{{DeviceID: "1", DeviceAddress: 1, NodeIDs: []string{"VALUE"}}},
			}}},
		}
	}
	index, states, _, err := validateCatalogs(catalogs)
	if err != nil {
		t.Fatal(err)
	}
	for _, sourceID := range []string{"source-a", "source-b"} {
		if states[sourceID] != domain.CatalogConflict || index.union.SourceTopologies[sourceID] == nil || index.union.SourceNodes[sourceID]["VALUE"].Key.Identifier != "VALUE" {
			t.Fatalf("conflicting source %s lost diagnostic snapshot: state=%q topologies=%#v nodes=%#v", sourceID, states[sourceID], index.union.SourceTopologies, index.union.SourceNodes)
		}
	}
}
