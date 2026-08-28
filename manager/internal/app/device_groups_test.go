package app

import (
	"context"
	"errors"
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
	"sync"
	"testing"
	"time"
)

type deviceRecordingLower struct {
	fakeLowerClient
	recordMu sync.Mutex
	writeKey domain.NodeKey
	callKey  domain.NodeKey
	payload  string
	writes   int
}

func (f *deviceRecordingLower) Write(_ context.Context, key domain.NodeKey, value any) (domain.StatusCode, error) {
	f.recordMu.Lock()
	defer f.recordMu.Unlock()
	f.writeKey = key
	f.payload = value.(string)
	f.writes++
	return domain.StatusGood, nil
}
func (f *deviceRecordingLower) Call(_ context.Context, key domain.NodeKey, payload string) (string, domain.StatusCode, error) {
	f.recordMu.Lock()
	defer f.recordMu.Unlock()
	f.callKey = key
	f.payload = payload
	return "OK", domain.StatusGood, nil
}

func deviceRuntime(t *testing.T) (*Runtime, map[string]*deviceRecordingLower) {
	return deviceRuntimeMapped(t, true)
}

func deviceRuntimeMapped(t *testing.T, mapped bool) (*Runtime, map[string]*deviceRecordingLower) {
	t.Helper()
	ctx := context.Background()
	repo, err := store.Open(ctx, ":memory:", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	for _, id := range []string{"fallback", "device-one", "device-two"} {
		defaultSource := ""
		if id == "fallback" || mapped {
			defaultSource = "a"
		}
		if _, err := repo.CreateGroup(ctx, domain.Group{ID: id, Name: id, DefaultSourceID: defaultSource}); err != nil {
			t.Fatal(err)
		}
	}
	clients := make(map[string]*deviceRecordingLower)
	for _, id := range []string{"a", "b"} {
		port := "tty" + id
		source := domain.Source{ID: id, Name: id, GroupID: "fallback", Enabled: true, Endpoint: "opc.tcp://" + id, SecurityPolicy: "None", SecurityMode: "None", GroupBindings: []domain.GroupBinding{{GroupID: "device-one", PortID: "/dev/" + port, DeviceID: "1"}, {GroupID: "device-two", PortID: "/dev/" + port, DeviceID: "2"}}}
		if !mapped {
			source.GroupBindings = nil
		}
		if _, err := repo.CreateSource(ctx, source, ""); err != nil {
			t.Fatal(err)
		}
		catalog := domain.SourceCatalog{SourceID: id, GroupID: "fallback", State: domain.CatalogReady, Nodes: []domain.CatalogNode{testNode("P1"), testNode("P2"), testNode("CALC"), testStringNode(port + "_Send"), testMethodNode(port + "_SendCommand")}, Topology: &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "/dev/" + port, PortName: port, SendNodeID: port + "_Send", SendMethodID: port + "_SendCommand", Devices: []domain.TopologyDevice{{DeviceID: "1", DeviceAddress: 1, Name: "设备1", NodeIDs: []string{"P1"}}, {DeviceID: "2", DeviceAddress: 2, Name: "设备2", NodeIDs: []string{"P2"}}}}}, Calculations: []domain.TopologyCalculation{{NodeID: "CALC", DependencyNodeIDs: []string{"P1"}, Owners: []domain.TopologyOwner{{PortID: "/dev/" + port, DeviceID: "1"}}}}}}
		if err := repo.ReplaceSourceCatalog(ctx, catalog); err != nil {
			t.Fatal(err)
		}
		clients[id] = &deviceRecordingLower{}
	}
	runtime, err := NewRuntime(ctx, repo, RuntimeOptions{CommandPendingTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for id, client := range clients {
		runtime.lowers[id] = lowerHandle{client: client}
		runtime.OnLowerStatus(connectedStatus(id, 1))
		primeWriteAck(runtime, id, "tty"+id, 1)
		value := 10.0
		if id == "b" {
			value = 20.0
		}
		for _, node := range []string{"P1", "P2", "CALC"} {
			runtime.OnSample(id, testKey(node), domain.Sample{Type: domain.DataTypeDouble, Value: value, StatusCode: domain.StatusGood, Epoch: 1})
		}
	}
	return runtime, clients
}

func TestDeviceGroupsSplitTwoSessionsWithoutExtraConnections(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	if err := runtime.openSession("left"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.openSession("right"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.selectSource("left", "device-one", "b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		session, node string
		value         float64
	}{{"left", "P1", 20}, {"left", "CALC", 20}, {"left", "P2", 10}, {"right", "P1", 10}, {"right", "P2", 10}} {
		if sample := runtime.ReadSample(check.session, testKey(check.node)); sample.Value != check.value || !sample.StatusCode.IsGood() {
			t.Fatalf("%s/%s=%#v", check.session, check.node, sample)
		}
	}
	if len(runtime.lowers) != 2 {
		t.Fatalf("lower connections multiplied by device count: %d", len(runtime.lowers))
	}
	if sample := runtime.ReadSample("left", testKey("ttya_Send")); sample.StatusCode != domain.StatusBadConfigurationError {
		t.Fatalf("split-port status must be ambiguous: %#v", sample)
	}
	view, err := runtime.GetSourceTopology(context.Background(), "b")
	if err != nil || !view.Supported || len(view.Ports) != 1 || view.Ports[0].Devices[0].GroupID != "device-one" || view.Ports[0].Devices[0].CalculationCount != 1 {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	page, err := runtime.ListPoints(context.Background(), api.PageRequest{SourceID: "a", RouteSourceID: "b", PortID: "/dev/ttya", DeviceID: "1", Limit: 20})
	if err != nil || page.Total != 2 {
		t.Fatalf("filtered page=%#v err=%v", page, err)
	}
	for _, point := range page.Items {
		if point.Node.PortID != "/dev/ttyb" || point.SelectedSourceID != "b" {
			t.Fatalf("target ownership=%#v", point)
		}
	}
}

func TestDeviceCommandRoutesPayloadToSelectedPhysicalPortAndLocksOnlyTargetGroup(t *testing.T) {
	runtime, clients := deviceRuntime(t)
	_ = runtime.openSession("s")
	_, _ = runtime.selectSource("s", "device-one", "b", 1, "test")
	_, _ = runtime.selectSource("s", "device-two", "b", 1, "test")
	status, err := runtime.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 01")
	if err != nil || status != domain.StatusGood {
		t.Fatalf("write status=%x err=%v", status, err)
	}
	if clients["b"].writeKey.Identifier != "ttyb_Send" || clients["b"].payload != "01 06 00 01" || clients["a"].writes != 0 {
		t.Fatalf("wrong command route: a=%#v b=%#v", clients["a"], clients["b"])
	}
	if runtime.routes.PendingCount("device-one") != 1 || runtime.routes.PendingCount("device-two") != 0 {
		t.Fatal("wrong group locked")
	}
	if _, err := runtime.selectSource("s", "device-one", "a", 2, "test"); !errors.Is(err, domain.ErrGroupPending) {
		t.Fatalf("target selection not locked: %v", err)
	}
	_, status, err = runtime.CallCommand(context.Background(), "s", testKey("ttya_SendCommand"), "02 06 00 01")
	if !errors.Is(err, domain.ErrCommandPending) || status != domain.StatusBadWaitingForResponse {
		t.Fatalf("shared physical port accepted second command: %x %v", status, err)
	}
	if _, err := runtime.selectSource("s", "device-two", "a", 2, "test"); err != nil {
		t.Fatalf("unrelated group cannot switch: %v", err)
	}
	runtime.closeSession("s")
	runtime.OnSample("b", testKey("ttyb_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if runtime.routes.PendingCount("device-one") != 0 {
		t.Fatal("closing Session lost pinned ACK")
	}
	if len(runtime.commandHistory) != 0 {
		t.Fatal("Session command history leaked after close")
	}
}

func TestGroupCommandAndRecentLegacyStatusFollowActualTarget(t *testing.T) {
	runtime, clients := deviceRuntime(t)
	_ = runtime.openSession("s")
	_, _ = runtime.selectSource("s", "device-one", "b", 1, "test")
	backend := OPCBackend{Runtime: runtime}
	result, status, err := backend.CallGroupCommand(context.Background(), "s", "device-one", "01 03 00 01")
	if err != nil || status != domain.StatusGood || result != "OK" || clients["b"].callKey.Identifier != "ttyb_SendCommand" {
		t.Fatalf("group method result=%q status=%x err=%v", result, status, err)
	}
	_, status, err = backend.CallGroupCommand(context.Background(), "s", "device-one", "02 03 00 01")
	if err == nil || status != domain.StatusBadConfigurationError {
		t.Fatalf("wrong device address accepted: %x %v", status, err)
	}
	status, err = runtime.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 01")
	if err != nil || status != domain.StatusGood {
		t.Fatal(err)
	}
	runtime.OnSample("b", testKey("ttyb_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if sample := runtime.ReadSample("s", testKey("ttya_Send")); sample.Value != "1" || sample.StatusCode != domain.StatusGood {
		t.Fatalf("recent target ACK=%#v", sample)
	}
	if sample := backend.ReadGroupCommand("s", "device-one"); sample.Value != "1" || sample.StatusCode != domain.StatusGood {
		t.Fatalf("group ACK=%#v", sample)
	}
}

func TestTopologyConflictCannotKeepCachedSourceSelectable(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	_ = runtime.openSession("s")
	status := connectedStatus("b", 1)
	status.Catalog = domain.CatalogConflict
	status.LastError = "invalid topology manifest"
	runtime.OnLowerStatus(status)
	if _, err := runtime.selectSource("s", "device-one", "b", 1, "test"); !errors.Is(err, domain.ErrSourceNotSelectable) {
		t.Fatalf("malformed lower remained selectable: %v", err)
	}
	source, _ := runtime.GetSource(context.Background(), "b")
	if source.CatalogState != domain.CatalogConflict {
		t.Fatalf("source state=%#v", source)
	}
}

func TestDeviceBindingsCannotChangeDuringSessionOrCascadeDeleteSharedSource(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	_ = runtime.openSession("s")
	source, _ := runtime.source("a")
	mutation := api.SourceMutation{ID: source.ID, GroupID: source.GroupID, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: source.Enabled, GroupBindings: []domain.GroupBinding{}}
	if _, err := runtime.UpdateSource(context.Background(), source.ID, source.Revision, mutation); !errors.Is(err, domain.ErrGroupInUse) {
		t.Fatalf("binding edit with Session: %v", err)
	}
	runtime.closeSession("s")
	group, _ := runtime.GetGroup(context.Background(), "device-one")
	if err := runtime.DeleteGroup(context.Background(), group.ID, group.Revision); !errors.Is(err, domain.ErrGroupInUse) {
		t.Fatalf("shared-source group deletion: %v", err)
	}
	if _, err := runtime.source("a"); err != nil {
		t.Fatal("shared physical source removed")
	}
	document, err := runtime.ExportConfiguration(context.Background())
	if err != nil || document.SchemaVersion != 2 || len(document.Sources[0].GroupBindings) != 2 {
		t.Fatalf("export=%#v err=%v", document, err)
	}
}

func TestDeviceBindingsWithoutTopologyRemainUnselectable(t *testing.T) {
	runtime := seededRuntime(t)
	if _, err := runtime.CreateGroup(context.Background(), api.GroupMutation{ID: "new-device", Name: "New device"}); err != nil {
		t.Fatal(err)
	}
	source, _ := runtime.source("source-b")
	mutation := api.SourceMutation{ID: source.ID, GroupID: source.GroupID, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: source.Enabled, GroupBindings: []domain.GroupBinding{{GroupID: "new-device", PortID: "COM1", DeviceID: "1"}}}
	if _, err := runtime.UpdateSource(context.Background(), source.ID, source.Revision, mutation); err != nil {
		t.Fatal(err)
	}
	_ = runtime.openSession("s")
	if _, err := runtime.selectSource("s", "new-device", source.ID, 1, "test"); !errors.Is(err, domain.ErrSourceNotSelectable) {
		t.Fatalf("unseen device was selectable: %v", err)
	}
}

func TestSequentialDeviceMappingMigratesRedundantSourcesWithoutConfigurationDeadlock(t *testing.T) {
	runtime, _ := deviceRuntimeMapped(t, false)
	for _, id := range []string{"a", "b"} {
		source, _ := runtime.source(id)
		mutation := api.SourceMutation{ID: id, GroupID: source.GroupID, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: true, GroupBindings: []domain.GroupBinding{{GroupID: "device-one", PortID: "/dev/tty" + id, DeviceID: "1"}}}
		updated, err := runtime.UpdateSource(context.Background(), id, source.Revision, mutation)
		if err != nil {
			t.Fatalf("mapping %s could not commit: %v", id, err)
		}
		if id == "a" && updated.CatalogState != domain.CatalogConflict {
			t.Fatalf("intermediate mismatch was not quarantined: %#v", updated)
		}
		if id == "b" && !updated.CatalogState.Selectable() {
			t.Fatalf("completed mapping did not recover: %#v", updated)
		}
	}
	for _, id := range []string{"a", "b"} {
		source, _ := runtime.source(id)
		if !source.CatalogState.Selectable() || !source.GroupAvailable("device-one") {
			t.Fatalf("source not recovered=%#v", source)
		}
	}
	group, err := runtime.GetGroup(context.Background(), "device-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SetDefaultSource(context.Background(), group.ID, "a", group.Revision); err != nil {
		t.Fatal(err)
	}
	if err := runtime.openSession("s"); err != nil {
		t.Fatal(err)
	}
	if value := runtime.ReadSample("s", testKey("P1")); value.StatusCode != domain.StatusGood || value.Value != 10.0 {
		t.Fatalf("mapped sample=%#v", value)
	}
}

func TestNewSourceCanDeclareDeviceBindingsBeforeDiscovery(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	created, err := runtime.CreateSource(context.Background(), api.SourceMutation{ID: "new", GroupID: "fallback", Name: "New", Endpoint: "opc.tcp://new:4840", SecurityPolicy: "None", SecurityMode: "None", Enabled: true, GroupBindings: []domain.GroupBinding{{GroupID: "device-one", PortID: "COM1", DeviceID: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !created.HasGroup("device-one") || created.GroupAvailable("device-one") || created.CatalogState != domain.CatalogDiscovering {
		t.Fatalf("new source=%#v", created)
	}
	_ = runtime.openSession("s")
	if _, err := runtime.selectSource("s", "device-one", "new", 1, "test"); !errors.Is(err, domain.ErrSourceNotSelectable) {
		t.Fatalf("undiscovered source selectable: %v", err)
	}
}

func TestDirectorySourceFilterDoesNotOverrideDefaultRoute(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	group, _ := runtime.GetGroup(context.Background(), "device-one")
	if _, err := runtime.SetDefaultSource(context.Background(), group.ID, "b", group.Revision); err != nil {
		t.Fatal(err)
	}
	page, err := runtime.ListPoints(context.Background(), api.PageRequest{GroupID: group.ID, SourceID: "a", PortID: "/dev/ttya", DeviceID: "1", Query: "P1", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].SelectedSourceID != "b" || page.Items[0].Sample.Value != 20.0 {
		t.Fatalf("directory filter overrode route: page=%#v err=%v", page, err)
	}
}

func TestCommandResultIsolatedFromOtherSessionAndEquipmentOnSamePort(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	_ = runtime.openSession("first")
	_ = runtime.openSession("second")
	backend := OPCBackend{Runtime: runtime}
	if status, err := backend.WriteGroupCommand(context.Background(), "first", "device-one", "01 06 00 01"); err != nil || status != domain.StatusGood {
		t.Fatalf("first=%x %v", status, err)
	}
	if sample := backend.ReadGroupCommand("first", "device-one"); sample.Value != "2" {
		t.Fatalf("initial status=%#v", sample)
	}
	runtime.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if status, err := runtime.WriteCommand(context.Background(), "second", testKey("ttya_Send"), "02 06 00 01"); err != nil || status != domain.StatusGood {
		t.Fatalf("second=%x %v", status, err)
	}
	runtime.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "3", StatusCode: domain.StatusGood, Epoch: 1})
	if sample := backend.ReadGroupCommand("first", "device-one"); sample.Value != "1" || sample.StatusCode != domain.StatusGood {
		t.Fatalf("first command overwritten by another device: %#v", sample)
	}
	if sample := backend.ReadGroupCommand("second", "device-two"); sample.Value != "3" || sample.StatusCode != domain.StatusBadUnexpectedError {
		t.Fatalf("second result=%#v", sample)
	}
	if sample := runtime.ReadSample("second", testKey("ttya_Send")); sample.Value != "3" {
		t.Fatalf("legacy ACK history=%#v", sample)
	}
	runtime.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if sample := runtime.ReadSample("second", testKey("ttya_Send")); sample.Value != "3" {
		t.Fatalf("unsolicited later ACK overwrote legacy result: %#v", sample)
	}
}

func TestHotRetiredPointKeepsDeviceGroupWhenAnotherBindingChanges(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	runtime.mu.RLock()
	catalog := runtime.catalogs["a"]
	runtime.mu.RUnlock()
	catalog.Topology = catalog.Topology.Clone()
	catalog.Topology.Ports[0].Devices[0].NodeIDs = nil
	catalog.Topology.Calculations = nil
	remaining := make([]domain.CatalogNode, 0)
	for _, node := range catalog.Nodes {
		if node.Key.Identifier != "P1" && node.Key.Identifier != "CALC" {
			remaining = append(remaining, node)
		}
	}
	catalog.Nodes = remaining
	runtime.OnCatalog(catalog)
	if _, err := runtime.CreateGroup(context.Background(), api.GroupMutation{ID: "unused", Name: "Unused"}); err != nil {
		t.Fatal(err)
	}
	source, _ := runtime.source("a")
	bindings := append([]domain.GroupBinding(nil), source.GroupBindings...)
	bindings = append(bindings, domain.GroupBinding{GroupID: "unused", PortID: "missing-port", DeviceID: "99"})
	_, err := runtime.UpdateSource(context.Background(), source.ID, source.Revision, api.SourceMutation{ID: source.ID, GroupID: source.GroupID, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: source.Enabled, GroupBindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.RLock()
	node, exists := runtime.index.sourceNode("a", "P1")
	runtime.mu.RUnlock()
	if !exists || !node.Retired || node.GroupID != "device-one" || node.PortID != "/dev/ttya" || node.DeviceID != "1" {
		t.Fatalf("retired ownership lost: %#v exists=%v", node, exists)
	}
	peer, _ := runtime.source("b")
	if !peer.CatalogState.Selectable() {
		t.Fatalf("unrelated peer quarantined: %#v", peer)
	}
}

func TestTopologyFallbackGroupChangeRejectsOnlineSessions(t *testing.T) {
	runtime, _ := deviceRuntimeMapped(t, false)
	if _, err := runtime.CreateGroup(context.Background(), api.GroupMutation{ID: "different", Name: "Different"}); err != nil {
		t.Fatal(err)
	}
	_ = runtime.openSession("s")
	source, _ := runtime.source("b")
	_, err := runtime.UpdateSource(context.Background(), source.ID, source.Revision, api.SourceMutation{ID: source.ID, GroupID: "different", Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: true})
	if !errors.Is(err, domain.ErrGroupInUse) {
		t.Fatalf("topology fallback moved while Session online: %v", err)
	}
}

func TestInvalidCachedTopologyQuarantinesOnlyItsSource(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	catalog, err := runtime.repo.LoadSourceCatalog(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	catalog.Topology.SchemaVersion = 99
	if err := runtime.repo.ReplaceSourceCatalog(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewRuntime(context.Background(), runtime.repo, RuntimeOptions{CommandPendingTTL: time.Minute})
	if err != nil {
		t.Fatalf("one invalid cached topology stopped whole manager: %v", err)
	}
	bad, _ := restarted.source("a")
	healthy, _ := restarted.source("b")
	if bad.CatalogState != domain.CatalogConflict || !healthy.CatalogState.Selectable() {
		t.Fatalf("bad=%#v healthy=%#v", bad, healthy)
	}
	if err := restarted.openSession("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.selectSource("s", "device-one", "b", 1, "test"); err != nil {
		t.Fatalf("healthy source was isolated: %v", err)
	}
}

func TestBindingEditCannotClearMalformedManifestQuarantine(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	status := connectedStatus("b", 1)
	status.Catalog = domain.CatalogConflict
	status.LastError = "invalid lower device topology"
	runtime.OnLowerStatus(status)
	if _, err := runtime.CreateGroup(context.Background(), api.GroupMutation{ID: "future", Name: "Future"}); err != nil {
		t.Fatal(err)
	}
	source, _ := runtime.source("b")
	bindings := append(source.GroupBindings, domain.GroupBinding{GroupID: "future", PortID: "unseen", DeviceID: "3"})
	updated, err := runtime.UpdateSource(context.Background(), source.ID, source.Revision, api.SourceMutation{ID: source.ID, GroupID: source.GroupID, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Enabled: true, GroupBindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CatalogState != domain.CatalogConflict {
		t.Fatalf("binding edit bypassed invalid manifest quarantine: %#v", updated)
	}
}

func TestPassiveSendResetOnlyUpdatesInitiatingSessionAndControlNode(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	_ = runtime.openSession("first")
	_ = runtime.openSession("second")
	backend := OPCBackend{Runtime: runtime}
	if status, err := backend.WriteGroupCommand(context.Background(), "first", "device-one", "01 06 00 01"); err != nil || status != domain.StatusGood {
		t.Fatalf("first=%x %v", status, err)
	}
	runtime.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if status, err := runtime.WriteCommand(context.Background(), "second", testKey("ttya_Send"), "02 06 00 01"); err != nil || status != domain.StatusGood {
		t.Fatalf("second=%x %v", status, err)
	}
	runtime.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "3", StatusCode: domain.StatusGood, Epoch: 1})
	if status, err := runtime.WriteCommand(context.Background(), "second", testKey("ttya_Send"), "0"); err != nil || status != domain.StatusGood {
		t.Fatalf("reset=%x %v", status, err)
	}
	if sample := runtime.ReadSample("second", testKey("ttya_Send")); sample.Value != "0" || sample.StatusCode != domain.StatusGood {
		t.Fatalf("legacy reset not reflected: %#v", sample)
	}
	if sample := backend.ReadGroupCommand("second", "device-two"); sample.Value != "3" {
		t.Fatalf("legacy reset cleared distinct group control: %#v", sample)
	}
	if sample := backend.ReadGroupCommand("first", "device-one"); sample.Value != "1" {
		t.Fatalf("reset cleared another Session: %#v", sample)
	}
	if status, err := backend.WriteGroupCommand(context.Background(), "first", "device-one", "IDLE"); err != nil || status != domain.StatusGood {
		t.Fatalf("group reset=%x %v", status, err)
	}
	runtime.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if sample := backend.ReadGroupCommand("first", "device-one"); sample.Value != "IDLE" || sample.StatusCode != domain.StatusGood {
		t.Fatalf("group reset overwritten: %#v", sample)
	}
	if sample := runtime.ReadSample("second", testKey("ttya_Send")); sample.Value != "0" {
		t.Fatalf("unsolicited ACK overwrote passive result: %#v", sample)
	}
}

func TestPassiveSendHistoryCannotDispatchToHotRetiredTarget(t *testing.T) {
	runtime := seededRuntime(t)
	lower := &deviceRecordingLower{}
	runtime.lowers["source-a"] = lowerHandle{client: lower}
	runtime.OnLowerStatus(connectedStatus("source-a", 1))
	primeWriteAck(runtime, "source-a", "COM1", 1)
	_ = runtime.openSession("s")
	if status, err := runtime.WriteCommand(context.Background(), "s", testKey("COM1_Send"), "01 06 00 01"); err != nil || status != domain.StatusGood {
		t.Fatalf("write=%x %v", status, err)
	}
	runtime.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	runtime.OnCatalog(domain.SourceCatalog{SourceID: "source-a", GroupID: "line", State: domain.CatalogReady, Nodes: []domain.CatalogNode{testNode("KKS001")}})
	if status, err := runtime.WriteCommand(context.Background(), "s", testKey("COM1_Send"), "0"); !errors.Is(err, domain.ErrNodeRetired) || status != domain.StatusBadOutOfService {
		t.Fatalf("retired target accepted reset=%x %v", status, err)
	}
	if lower.writes != 1 {
		t.Fatalf("reset dispatched to retired target: %d writes", lower.writes)
	}
	if sample := runtime.ReadSample("s", testKey("COM1_Send")); sample.StatusCode != domain.StatusBadOutOfService {
		t.Fatalf("retired history quality=%#v", sample)
	}
}
