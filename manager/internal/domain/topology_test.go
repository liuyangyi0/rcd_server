package domain

import (
	"errors"
	"testing"
)

func topologyFixture() *SourceTopology {
	return &SourceTopology{SchemaVersion: 1, Ports: []TopologyPort{{PortID: "/dev/ttyA", PortName: "ttyA", SendNodeID: "ttyA_Send", SendMethodID: "ttyA_SendCommand", Devices: []TopologyDevice{{DeviceID: "1", DeviceAddress: 1, NodeIDs: []string{"P1"}}, {DeviceID: "2", DeviceAddress: 2, NodeIDs: []string{"P2"}}}}}, Calculations: []TopologyCalculation{{NodeID: "C1", Owners: []TopologyOwner{{PortID: "/dev/ttyA", DeviceID: "1"}}}, {NodeID: "CROSS", Owners: []TopologyOwner{{PortID: "/dev/ttyA", DeviceID: "1"}, {PortID: "/dev/ttyA", DeviceID: "2"}}}}}
}

func TestAssignCatalogGroupsKeepsOnePhysicalSourceWithMultipleGroups(t *testing.T) {
	source := Source{ID: "source", GroupID: "fallback", GroupBindings: []GroupBinding{{GroupID: "one", PortID: "/dev/ttyA", DeviceID: "1"}, {GroupID: "two", PortID: "/dev/ttyA", DeviceID: "2"}}}
	catalog := SourceCatalog{SourceID: "source", GroupID: "fallback", State: CatalogReady, Topology: topologyFixture(), Nodes: []CatalogNode{variable("P1", DataTypeDouble), variable("P2", DataTypeDouble), variable("C1", DataTypeDouble), variable("CROSS", DataTypeDouble)}}
	send := variable("ttyA_Send", DataTypeString)
	send.Writable = true
	method := CatalogNode{Key: NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "ttyA_SendCommand"}, Class: NodeClassMethod, ValueRank: -1, InputArguments: []Argument{{Name: "HexData", DataType: DataTypeString, ValueRank: -1}}, OutputArguments: []Argument{{Name: "Result", DataType: DataTypeString, ValueRank: -1}}}
	catalog.Nodes = append(catalog.Nodes, send, method)
	assigned, err := AssignCatalogGroups(source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"P1": "one", "P2": "two", "C1": "one", "CROSS": "fallback", "ttyA_Send": "fallback", "ttyA_SendCommand": "fallback"}
	for _, node := range assigned.Nodes {
		if node.GroupID != want[node.Key.Identifier] {
			t.Fatalf("node=%#v", node)
		}
	}
	if catalog.Nodes[0].GroupID != "" {
		t.Fatal("input catalog mutated")
	}
	union, err := BuildCatalogUnion([]SourceCatalog{assigned})
	if err != nil || len(union.Groups["one"]) != 2 || len(union.Groups["two"]) != 1 {
		t.Fatalf("union=%#v err=%v", union, err)
	}
	assigned.Topology.Ports[0].Devices[0].NodeIDs[0] = "mutated"
	if catalog.Topology.Ports[0].Devices[0].NodeIDs[0] != "P1" {
		t.Fatal("topology clone shares node slice")
	}
}

func TestDeviceGroupRoutesRemainIndependentAndRequireDiscoveredMembership(t *testing.T) {
	groups := []Group{{ID: "fallback"}, {ID: "one", DefaultSourceID: "a"}, {ID: "two", DefaultSourceID: "a"}, {ID: "missing"}}
	source := Source{ID: "a", GroupID: "fallback", Enabled: true, CatalogState: CatalogReady, GroupBindings: []GroupBinding{{GroupID: "one", PortID: "p", DeviceID: "1"}, {GroupID: "two", PortID: "p", DeviceID: "2"}, {GroupID: "missing", PortID: "p", DeviceID: "3"}}, AvailableGroupIDs: []string{"one", "two"}}
	other := source.Clone()
	other.ID = "b"
	table, err := NewRouteTable(groups, []Source{source, other})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = table.OpenSession("left")
	_, _ = table.OpenSession("right")
	if _, err := table.Select("left", "one", "b", 1); err != nil {
		t.Fatal(err)
	}
	left, _ := table.Session("left")
	right, _ := table.Session("right")
	if left.Routes["one"].SourceID != "b" || left.Routes["two"].SourceID != "a" || right.Routes["one"].SourceID != "a" {
		t.Fatalf("left=%#v right=%#v", left, right)
	}
	if _, err := table.Select("left", "missing", "a", 1); !errors.Is(err, ErrSourceNotSelectable) {
		t.Fatalf("missing device err=%v", err)
	}
	source.GroupBindings[0].GroupID = "mutated"
	copySources := table.Sources()
	copySources[0].GroupBindings[0].GroupID = "mutated-again"
	if !table.Sources()[0].HasGroup("one") {
		t.Fatal("source slice was not copied")
	}
}

func TestDeviceGroupCommandsSharePhysicalPortLock(t *testing.T) {
	source := Source{ID: "a", GroupID: "fallback", Enabled: true, CatalogState: CatalogReady, GroupBindings: []GroupBinding{{GroupID: "one", PortID: "p", DeviceID: "1"}, {GroupID: "two", PortID: "p", DeviceID: "2"}}, AvailableGroupIDs: []string{"one", "two"}}
	table, err := NewRouteTable([]Group{{ID: "fallback"}, {ID: "one", DefaultSourceID: "a"}, {ID: "two", DefaultSourceID: "a"}}, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = table.OpenSession("s")
	first, err := table.BeginCommand("s", "one", "physical", "Write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.BeginCommand("s", "two", "physical", "Write"); !errors.Is(err, ErrCommandPending) {
		t.Fatalf("same port accepted: %v", err)
	}
	if table.PendingCount("one") != 1 || table.PendingCount("two") != 0 {
		t.Fatal("command locked wrong logical group")
	}
	if _, err := table.Select("s", "two", "a", 1); err != nil {
		t.Fatalf("unrelated group locked: %v", err)
	}
	if _, err := table.Select("s", "one", "a", 1); !errors.Is(err, ErrGroupPending) {
		t.Fatalf("target group unlocked: %v", err)
	}
	_, _ = table.FinishCommand(first.ID, CommandReceived, StatusGood, "")
}

func TestBindingsChangeRequiresNoSessionButAllowsUnchangedFallbackDefault(t *testing.T) {
	groups := []Group{{ID: "fallback", DefaultSourceID: "a"}, {ID: "one"}}
	source := Source{ID: "a", GroupID: "fallback", Enabled: true, CatalogState: CatalogReady}
	table, err := NewRouteTable(groups, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = table.OpenSession("s")
	source.GroupBindings = []GroupBinding{{GroupID: "one", PortID: "p", DeviceID: "1"}}
	if err := table.ApplyConfiguration(groups, []Source{source}); !errors.Is(err, ErrGroupInUse) {
		t.Fatalf("binding changed with session: %v", err)
	}
	table.CloseSession("s")
	if err := table.ApplyConfiguration(groups, []Source{source}); err != nil {
		t.Fatalf("unused binding change with fallback default: %v", err)
	}
}

func TestInvalidTopologyNeverFallsBackToWholeSourceGrouping(t *testing.T) {
	catalog := SourceCatalog{SourceID: "a", Topology: &SourceTopology{SchemaVersion: 99}}
	if _, err := AssignCatalogGroups(Source{ID: "a", GroupID: "fallback"}, catalog); err == nil {
		t.Fatal("invalid topology accepted")
	}
	if err := ValidateGroupBindings([]GroupBinding{{GroupID: "a", PortID: "p", DeviceID: "1"}, {GroupID: "b", PortID: "p", DeviceID: "1"}}); err == nil {
		t.Fatal("duplicate device binding accepted")
	}
}

func TestReadonlySendSuffixIsNotACommandWithOrWithoutTopology(t *testing.T) {
	for _, withTopology := range []bool{false, true} {
		name := "legacy"
		if withTopology {
			name = "topology"
		}
		t.Run(name, func(t *testing.T) {
			source := Source{ID: "a", GroupID: "fallback", GroupBindings: []GroupBinding{{GroupID: "device", PortID: "COM1", DeviceID: "1"}}}
			send := variable("COM1_Send", DataTypeString)
			send.Writable = true
			method := CatalogNode{Key: NodeKey{NamespaceURI: "urn:SerialServer", Identifier: "COM1_SendCommand"}, Class: NodeClassMethod, ValueRank: -1, InputArguments: []Argument{{Name: "HexData", DataType: DataTypeString, ValueRank: -1}}, OutputArguments: []Argument{{Name: "Result", DataType: DataTypeString, ValueRank: -1}}}
			catalog := SourceCatalog{SourceID: "a", GroupID: "fallback", State: CatalogReady, Nodes: []CatalogNode{variable("FLOW_Send", DataTypeDouble), variable("TOTAL_Send", DataTypeDouble), variable("STATE_Send", DataTypeString), send, method}}
			if withTopology {
				catalog.Topology = &SourceTopology{SchemaVersion: 1, Ports: []TopologyPort{{PortID: "COM1", PortName: "COM1", SendNodeID: "COM1_Send", SendMethodID: "COM1_SendCommand", Devices: []TopologyDevice{{DeviceID: "1", DeviceAddress: 1, NodeIDs: []string{"FLOW_Send", "STATE_Send"}}}}}, Calculations: []TopologyCalculation{{NodeID: "TOTAL_Send", Owners: []TopologyOwner{{PortID: "COM1", DeviceID: "1"}}}}}
			}
			assigned, err := AssignCatalogGroups(source, catalog)
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range assigned.Nodes {
				if node.Key.Identifier == "FLOW_Send" || node.Key.Identifier == "STATE_Send" {
					if node.Kind != "point" {
						t.Fatalf("measurement classified as command: %#v", node)
					}
				}
				if withTopology && node.Key.Identifier == "TOTAL_Send" && (node.Kind != "calculation" || node.GroupID != "device") {
					t.Fatalf("calculation classified incorrectly: %#v", node)
				}
			}
			union, err := BuildCatalogUnion([]SourceCatalog{assigned})
			if err != nil || union.SourceStates["a"] == CatalogConflict {
				t.Fatalf("valid KKS suffix rejected: union=%#v err=%v", union, err)
			}
		})
	}
}
