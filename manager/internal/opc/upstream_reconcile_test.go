package opc

import (
	"reflect"
	"sync/atomic"
	"testing"

	awserver "github.com/awcullen/opcua/server"
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
		"Groups/group-a/SelectSource",
		"Groups/group-a/SelectSource_Input",
		"Groups/group-a/SelectSource_Output",
		"Groups/group-a/MemberSourceIds",
		"Groups/group-a/CatalogRevision",
		"Groups/group-a/PointNodeIds",
		"Groups/group-a/PointNodeCount",
		"Groups/group-a/PointSetFingerprint",
		"Groups/group-a/PointSetRevision",
		"Groups/group-a/PendingCommandCount",
		"Groups/group-a/Send",
		"Groups/group-a/SendCommand",
		"Groups/group-a/SendCommand_Input",
		"Groups/group-a/SendCommand_Output",
		"Groups/group-a/Members",
		"Groups/group-a/Members/source-a",
		"Groups/group-a/Members/source-a/SourceId",
		"Groups/group-a/Members/source-a/SourceName",
		"Groups/group-a/Members/source-a/Endpoint",
		"Groups/group-a/Members/source-a/UsesDefaultGroup",
		"Groups/group-a/Members/source-a/PortCount",
		"Groups/group-a/Members/source-a/ExpectedPointCount",
		"Groups/group-a/Members/source-a/ProvidedPointCount",
		"Groups/group-a/Members/source-a/MissingPointCount",
		"Groups/group-a/Members/source-a/CompleteCoverage",
		"Groups/group-a/Members/source-a/CoverageKnown",
		"Groups/group-a/Members/source-a/CoverageFingerprint",
		"Groups/group-a/Members/source-a/CoverageRevision",
		"Groups/group-a/Members/source-a/ProvidedPointNodeIds",
		"Groups/group-a/Members/source-a/MissingPointNodeIds",
		"Groups/group-a/Members/source-a/Selectable",
		"Groups/group-a/Members/source-a/IsSelected",
		"Groups/group-a/Members/source-a/ConnectionState",
		"Groups/group-a/Members/source-a/CatalogState",
		"Groups/group-a/Members/source-a/Ports",
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

func TestReconcileCatalogRefreshesPhysicalGroupMemberTree(t *testing.T) {
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	group := domain.Group{ID: "device-a", DefaultSourceID: "source-a", Revision: 1}
	source := domain.Source{
		ID: "source-a", GroupID: group.ID, Name: "Lower A", Endpoint: "opc.tcp://lower-a:4840",
		Enabled: true, CatalogState: domain.CatalogReady, AvailableGroupIDs: []string{group.ID},
		GroupBindings: []domain.GroupBinding{{GroupID: group.ID, PortID: "/dev/ttyUSB0", DeviceID: "7"}},
	}
	backend := &fakeBackend{groups: []domain.Group{group}, sources: []domain.Source{source}}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: "opc.tcp://127.0.0.1:4840", CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	makeCatalog := func(portID, portName string) domain.CatalogUnion {
		return domain.CatalogUnion{
			Groups:      map[string]map[string]domain.CatalogNode{group.ID: {}},
			SourceNodes: map[string]map[string]domain.CatalogNode{source.ID: {}},
			SourceTopologies: map[string]*domain.SourceTopology{source.ID: {
				SchemaVersion: 1,
				Ports: []domain.TopologyPort{{
					PortID: portID, PortName: portName, SendNodeID: portName + "_Send", SendMethodID: portName + "_SendCommand",
					Devices: []domain.TopologyDevice{{DeviceID: "7", DeviceAddress: 7, Name: "Meter 7", KKSPrefix: "M7", NodeIDs: []string{"M7_VALUE"}}},
				}},
			}},
		}
	}
	assertTree := func(portID string, present bool) {
		t.Helper()
		prefix := "Groups/device-a/Members/source-a/Ports/" + managementPathComponent(portID)
		identifiers := []string{
			prefix,
			prefix + "/PortId",
			prefix + "/PortName",
			prefix + "/SendNodeId",
			prefix + "/SendMethodId",
			prefix + "/DeviceCount",
			prefix + "/Devices",
			prefix + "/Devices/7",
			prefix + "/Devices/7/DeviceId",
			prefix + "/Devices/7/DeviceAddress",
			prefix + "/Devices/7/AddressKnown",
			prefix + "/Devices/7/DeviceName",
			prefix + "/Devices/7/KKSPrefix",
			prefix + "/Devices/7/PointCount",
			prefix + "/Devices/7/RawPointCount",
			prefix + "/Devices/7/CalculationPointCount",
			prefix + "/Devices/7/Configured",
			prefix + "/Devices/7/ExplicitlyBound",
			prefix + "/Devices/7/PresentInTopology",
			prefix + "/Devices/7/Discovered",
			prefix + "/Devices/7/CommunicationState",
			prefix + "/Devices/7/CommunicationStateText",
			prefix + "/Devices/7/CommunicationObservedAt",
		}
		for _, identifier := range identifiers {
			_, exists := server.server.NamespaceManager().FindNode(awu.NewNodeIDString(server.managementNS, identifier))
			if exists != present {
				t.Fatalf("management node %q exists=%v want=%v", identifier, exists, present)
			}
		}
	}

	if err := server.ReconcileCatalog(makeCatalog("/dev/ttyUSB0", "ttyUSB0"), backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	assertTree("/dev/ttyUSB0", true)
	stableID := awu.NewNodeIDString(server.managementNS, "Groups/device-a/Members/source-a/Ports/"+managementPathComponent("/dev/ttyUSB0")+"/PortName")
	stableBefore, ok := server.server.NamespaceManager().FindNode(stableID)
	if !ok {
		t.Fatal("stable PortName node missing before metadata refresh")
	}
	if err := server.ReconcileCatalog(makeCatalog("/dev/ttyUSB0", "renamed-port"), backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	stableAfter, ok := server.server.NamespaceManager().FindNode(stableID)
	if !ok || stableBefore != stableAfter {
		t.Fatal("metadata refresh replaced a stable member variable and would break monitored items")
	}
	foundStaticRefreshID := false
	for _, id := range server.groupNodeIDs[group.ID] {
		if id == stableID {
			foundStaticRefreshID = true
			break
		}
	}
	if foundStaticRefreshID {
		t.Fatal("static member metadata must not be resent on every Session route change")
	}
	isSelectedID := awu.NewNodeIDString(server.managementNS, "Groups/device-a/Members/source-a/IsSelected")
	foundRouteRefreshID := false
	for _, id := range server.groupNodeIDs[group.ID] {
		if id == isSelectedID {
			foundRouteRefreshID = true
			break
		}
	}
	if !foundRouteRefreshID {
		t.Fatal("Session-specific IsSelected is absent from forced route refresh index")
	}

	source.GroupBindings = []domain.GroupBinding{{GroupID: group.ID, PortID: "/dev/ttyUSB1", DeviceID: "7"}}
	backend.sources = []domain.Source{source}
	if err := server.ReconcileCatalog(makeCatalog("/dev/ttyUSB1", "ttyUSB1"), backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	assertTree("/dev/ttyUSB0", false)
	assertTree("/dev/ttyUSB1", true)
}

func TestBuildGroupMemberTreeSpecsDistinguishesDefaultAndExplicitBindings(t *testing.T) {
	point := legacyVariable("M7_VALUE", domain.DataTypeUInt32, false)
	point.GroupID = "device-a"
	catalog := domain.CatalogUnion{
		Groups:      map[string]map[string]domain.CatalogNode{"device-a": {"M7_VALUE": point}},
		SourceNodes: map[string]map[string]domain.CatalogNode{"source-a": {"M7_VALUE": point}},
		SourceTopologies: map[string]*domain.SourceTopology{"source-a": {
			SchemaVersion: 1,
			Ports: []domain.TopologyPort{{
				PortID: "serial-a", PortName: "serial-a",
				Devices: []domain.TopologyDevice{{DeviceID: "7", DeviceAddress: 7, NodeIDs: []string{"M7_VALUE"}}},
			}},
		}},
	}
	source := domain.Source{ID: "source-a", GroupID: "device-a", CatalogState: domain.CatalogReady}
	specs := buildGroupMemberTreeSpecs("device-a", []domain.Source{source}, catalog)
	if len(specs) != 1 || len(specs[0].Ports) != 1 || len(specs[0].Ports[0].Devices) != 1 {
		t.Fatalf("unexpected default member tree: %#v", specs)
	}
	device := specs[0].Ports[0].Devices[0]
	if !device.Configured || device.ExplicitlyBound || !device.AddressKnown || !device.PresentInTopology {
		t.Fatalf("default-group membership flags are misleading: %#v", device)
	}
	if !specs[0].CoverageKnown || !specs[0].CompleteCoverage || specs[0].ProvidedPoints != 1 || specs[0].MissingPoints != 0 || len(specs[0].CoverageHash) != 64 {
		t.Fatalf("coverage=%#v", specs[0])
	}
	if !reflect.DeepEqual(specs[0].ProvidedPointIDs, []string{"M7_VALUE"}) || len(specs[0].MissingPointIDs) != 0 {
		t.Fatalf("point coverage identifiers=%#v", specs[0])
	}

	source.GroupBindings = []domain.GroupBinding{{GroupID: "device-b", PortID: "serial-b", DeviceID: "station-x"}}
	placeholders := buildGroupMemberTreeSpecs("device-b", []domain.Source{source}, domain.CatalogUnion{})
	placeholder := placeholders[0].Ports[0].Devices[0]
	if !placeholder.Configured || !placeholder.ExplicitlyBound || placeholder.AddressKnown || placeholder.PresentInTopology {
		t.Fatalf("configured undiscovered placeholder flags are misleading: %#v", placeholder)
	}
	if value, status := deviceAddressManagementValue(placeholder)(""); value != uint32(0) || status != awu.BadWaitingForInitialData {
		t.Fatalf("unknown address value=%#v status=%s; want typed zero with BadWaitingForInitialData", value, status)
	}
}

func TestDiscoveringMemberNeverClaimsCoverageIsComplete(t *testing.T) {
	point := legacyVariable("KKS", domain.DataTypeUInt32, false)
	point.GroupID = "g"
	catalog := domain.CatalogUnion{
		Groups:       map[string]map[string]domain.CatalogNode{"g": {"KKS": point}},
		SourceNodes:  map[string]map[string]domain.CatalogNode{"source": {"KKS": point}},
		SourceStates: map[string]domain.CatalogState{"source": domain.CatalogDiscovering},
	}
	specs := buildGroupMemberTreeSpecs("g", []domain.Source{{ID: "source", GroupID: "g", CatalogState: domain.CatalogDiscovering}}, catalog)
	if len(specs) != 1 || specs[0].CoverageKnown || specs[0].CompleteCoverage || specs[0].ProvidedPoints != 1 {
		t.Fatalf("discovering coverage=%#v", specs)
	}
}

func TestConflictMemberNeverClaimsCoverageIsKnown(t *testing.T) {
	point := legacyVariable("KKS", domain.DataTypeUInt32, false)
	point.GroupID = "g"
	catalog := domain.CatalogUnion{
		Groups:       map[string]map[string]domain.CatalogNode{"g": {"KKS": point}},
		SourceNodes:  map[string]map[string]domain.CatalogNode{"source": {"KKS": point}},
		SourceStates: map[string]domain.CatalogState{"source": domain.CatalogConflict},
	}
	specs := buildGroupMemberTreeSpecs("g", []domain.Source{{ID: "source", GroupID: "g", CatalogState: domain.CatalogConflict}}, catalog)
	if len(specs) != 1 || specs[0].CoverageKnown || specs[0].CompleteCoverage || specs[0].ProvidedPoints != 1 {
		t.Fatalf("conflict coverage=%#v", specs)
	}
}

func TestDiscoveryListHandlersShareAtomicVersionedSnapshots(t *testing.T) {
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	group := domain.Group{ID: "g", DefaultSourceID: "source", Revision: 1}
	source := domain.Source{ID: "source", GroupID: "g", Enabled: true, CatalogState: domain.CatalogReady}
	backend := &fakeBackend{groups: []domain.Group{group}, sources: []domain.Source{source}}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: "opc.tcp://127.0.0.1:4840", CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	makeCatalog := func(identifiers ...string) domain.CatalogUnion {
		groupNodes := make(map[string]domain.CatalogNode, len(identifiers))
		sourceNodes := make(map[string]domain.CatalogNode, len(identifiers))
		for _, identifier := range identifiers {
			point := legacyVariable(identifier, domain.DataTypeUInt32, false)
			point.GroupID = group.ID
			groupNodes[identifier] = point
			sourceNodes[identifier] = point
		}
		return domain.CatalogUnion{
			Groups:       map[string]map[string]domain.CatalogNode{group.ID: groupNodes},
			SourceNodes:  map[string]map[string]domain.CatalogNode{source.ID: sourceNodes},
			SourceStates: map[string]domain.CatalogState{source.ID: domain.CatalogReady},
		}
	}
	if err := server.ReconcileCatalog(makeCatalog("A"), backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	readHandler := func(identifier string) func(*awserver.Session, awu.ReadValueID) awu.DataValue {
		t.Helper()
		node, ok := server.server.NamespaceManager().FindNode(awu.NewNodeIDString(server.managementNS, identifier))
		if !ok {
			t.Fatalf("missing management node %q", identifier)
		}
		variable, ok := node.(*awserver.VariableNode)
		if !ok || variable.ReadValueHandler() == nil {
			t.Fatalf("management node %q has no read handler", identifier)
		}
		return variable.ReadValueHandler()
	}
	pointIDs := readHandler("Groups/g/PointNodeIds")
	pointRevision := readHandler("Groups/g/PointSetRevision")
	catalogRevision := readHandler("Groups/g/CatalogRevision")
	providedIDs := readHandler("Groups/g/Members/source/ProvidedPointNodeIds")
	coverageRevision := readHandler("Groups/g/Members/source/CoverageRevision")
	groupStore := server.groupPointSnapshots[group.ID]
	coverageStore := server.memberCoverageSnapshots[memberCoverageSnapshotKey{groupID: group.ID, sourceID: source.ID}]

	if err := server.ReconcileCatalog(makeCatalog("A", "B"), backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	if server.groupPointSnapshots[group.ID] != groupStore || server.memberCoverageSnapshots[memberCoverageSnapshotKey{groupID: group.ID, sourceID: source.ID}] != coverageStore {
		t.Fatal("hot reconciliation replaced a snapshot holder captured by existing handlers")
	}
	if got := pointIDs(nil, awu.ReadValueID{}).Value.([]string); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("old PointNodeIds handler=%#v", got)
	}
	if got := pointRevision(nil, awu.ReadValueID{}).Value; got != uint64(2) {
		t.Fatalf("old PointSetRevision handler=%#v, want 2", got)
	}
	if got := catalogRevision(nil, awu.ReadValueID{}).Value; got != uint64(4) {
		t.Fatalf("old CatalogRevision handler=%#v, want second completed generation 4", got)
	}
	if got := providedIDs(nil, awu.ReadValueID{}).Value.([]string); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("old ProvidedPointNodeIds handler=%#v", got)
	}
	if got := coverageRevision(nil, awu.ReadValueID{}).Value; got != uint64(2) {
		t.Fatalf("old CoverageRevision handler=%#v, want 2", got)
	}
}

func TestMemberPointNodeIDsRequireACompatiblePointInTheSameGroup(t *testing.T) {
	point := legacyVariable("PRESENT", domain.DataTypeUInt32, false)
	point.GroupID = "device-a"
	wrongGroup := legacyVariable("WRONG_GROUP", domain.DataTypeUInt32, false)
	wrongGroup.GroupID = "device-b"
	command := legacyVariable("COMMAND_SHAPED_AS_POINT", domain.DataTypeUInt32, false)
	command.GroupID = "device-a"
	command.Kind = "command"
	catalog := domain.CatalogUnion{
		Groups: map[string]map[string]domain.CatalogNode{"device-a": {
			"PRESENT": point, "WRONG_GROUP": wrongGroup, "COMMAND_SHAPED_AS_POINT": legacyVariable("COMMAND_SHAPED_AS_POINT", domain.DataTypeUInt32, false),
		}},
		SourceNodes: map[string]map[string]domain.CatalogNode{"source-a": {
			"PRESENT": point, "WRONG_GROUP": wrongGroup, "COMMAND_SHAPED_AS_POINT": command,
		}},
	}
	provided, missing := memberPointNodeIDs("device-a", "source-a", catalog)
	if !reflect.DeepEqual(provided, []string{"PRESENT"}) || !reflect.DeepEqual(missing, []string{"COMMAND_SHAPED_AS_POINT", "WRONG_GROUP"}) {
		t.Fatalf("provided=%#v missing=%#v", provided, missing)
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

func TestUpstreamManagementArrayCapacityCoversMaximumDeployment(t *testing.T) {
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: "opc.tcp://127.0.0.1:4840", CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
	}, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if got := server.server.ServerCapabilities().MaxArrayLength; got != managementMaxArrayLength || got < 20*5300 {
		t.Fatalf("MaxArrayLength=%d, want %d and enough for 20x5300", got, managementMaxArrayLength)
	}
	if got := server.server.ServerCapabilities().MinSupportedSampleRate; got != upstreamMinimumSamplingIntervalMS {
		t.Fatalf("MinSupportedSampleRate=%v, want %v", got, upstreamMinimumSamplingIntervalMS)
	}
}

func TestGroupCatalogRevisionIsARecoverableSeqlock(t *testing.T) {
	server := &UpstreamServer{groupCatalogRevisions: make(map[string]*atomic.Uint64)}
	revision := server.beginGroupCatalogUpdateLocked("g")
	if got := revision.Load(); got != 1 {
		t.Fatalf("first in-progress revision=%d, want odd 1", got)
	}
	if retry := server.beginGroupCatalogUpdateLocked("g"); retry != revision || retry.Load() != 1 {
		t.Fatalf("retry replaced or advanced failed odd revision: %#v/%d", retry, retry.Load())
	}
	server.finishGroupCatalogUpdateLocked(revision)
	if got := revision.Load(); got != 2 {
		t.Fatalf("completed revision=%d, want even 2", got)
	}
}
