package opc

import (
	"reflect"
	"testing"

	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

func TestReconcileCatalogRemovesStaleManagementNodesOnly(t *testing.T) {
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	group := domain.Group{ID: "group-a", DefaultSourceID: "source-a", Revision: 1}
	source := domain.Source{ID: "source-a", GroupID: group.ID, Enabled: true, CatalogState: domain.CatalogReady}
	backend := &fakeBackend{groups: []domain.Group{group}, sources: []domain.Source{source}}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: "opc.tcp://127.0.0.1:4840", CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	catalog := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{
		group.ID: {
			"KKS": legacyVariable("KKS", domain.DataTypeUInt32, false),
		},
	}}
	if err := server.ReconcileCatalog(catalog, backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}

	managementIdentifiers := []string{
		"Groups/group-a",
		"Groups/group-a/DefaultSourceId",
		"Groups/group-a/SelectedSourceId",
		"Groups/group-a/SelectionRevision",
		"Groups/group-a/MemberSourceIds",
		"Groups/group-a/PendingCommandCount",
		"Sources/source-a",
		"Sources/source-a/ConnectionState",
		"Sources/source-a/SessionState",
		"Sources/source-a/SubscriptionState",
		"Sources/source-a/CatalogState",
		"Sources/source-a/LastDataAt",
		"Sources/source-a/LastProbeAt",
		"Sources/source-a/LastError",
		"Sources/source-a/ReconnectCount",
		"Sources/source-a/PointCount",
		"Sources/source-a/MonitoredPointCount",
		"Sources/source-a/SubscriptionCount",
		"Sources/source-a/TotalPointCount",
		"Sources/source-a/GoodPointCount",
		"Sources/source-a/BadPointCount",
	}
	namespace := server.server.NamespaceManager()
	for _, identifier := range managementIdentifiers {
		if _, ok := namespace.FindNode(awu.NewNodeIDString(server.managementNS, identifier)); !ok {
			t.Fatalf("management node %q was not added", identifier)
		}
	}

	backend.groups = nil
	backend.sources = nil
	if err := server.ReconcileCatalog(domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, identifier := range managementIdentifiers {
		if _, ok := namespace.FindNode(awu.NewNodeIDString(server.managementNS, identifier)); ok {
			t.Fatalf("stale management node %q remains addressable", identifier)
		}
	}
	assertNoForwardOrganizesChildren(t, server, server.groupsRootID)
	assertNoForwardOrganizesChildren(t, server, server.sourcesRootID)

	if _, ok := namespace.FindNode(awu.NewNodeIDString(server.businessNS, "KKS")); !ok {
		t.Fatal("hot-deleted ns=2 business node was physically removed")
	}
	if entry := server.business["KKS"]; entry == nil || !entry.retired.Load() {
		t.Fatal("hot-deleted ns=2 business node was not retained as retired")
	}

	// Deletion must also clear the adapter index so a later CRUD re-add of the
	// same stable IDs can recreate every address-space node.
	backend.groups = []domain.Group{group}
	backend.sources = []domain.Source{source}
	if err := server.ReconcileCatalog(catalog, backend.groups, backend.sources); err != nil {
		t.Fatalf("re-add deleted management nodes: %v", err)
	}
	for _, identifier := range managementIdentifiers {
		if _, ok := namespace.FindNode(awu.NewNodeIDString(server.managementNS, identifier)); !ok {
			t.Fatalf("management node %q was not recreated", identifier)
		}
	}
}

func assertNoForwardOrganizesChildren(t *testing.T, server *UpstreamServer, id awu.NodeID) {
	t.Helper()
	namespace := server.server.NamespaceManager()
	root, ok := namespace.FindNode(id)
	if !ok {
		t.Fatalf("missing management root %v", id)
	}
	for _, reference := range root.References() {
		if !reference.IsInverse && reference.ReferenceTypeID == awu.ReferenceTypeIDOrganizes {
			t.Fatalf("management root %v retains stale Organizes reference to %v", id, reference.TargetID)
		}
	}
}

func TestForceSessionRefreshBatchesAtFiveHundredNodes(t *testing.T) {
	nodes := make([]awu.NodeID, 1201)
	for i := range nodes {
		nodes[i] = awu.NewNodeIDNumeric(2, uint32(i+1))
	}
	var batchSizes []int
	total := forceSessionRefreshBatches(nodes, func(batch []awu.NodeID) int {
		batchSizes = append(batchSizes, len(batch))
		return len(batch)
	})
	if total != len(nodes) {
		t.Fatalf("total refreshed = %d, want %d", total, len(nodes))
	}
	if want := []int{500, 500, 201}; !reflect.DeepEqual(batchSizes, want) {
		t.Fatalf("batch sizes = %v, want %v", batchSizes, want)
	}
}
