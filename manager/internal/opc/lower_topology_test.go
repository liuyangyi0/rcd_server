package opc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

func topologyFixture() (domain.SourceTopology, []domain.CatalogNode) {
	port := domain.TopologyPort{
		PortID: "/dev/ttyS0", PortName: "ttyS0", SendNodeID: "ttyS0_Send", SendMethodID: "ttyS0_SendCommand",
		Devices: []domain.TopologyDevice{
			{DeviceID: "1", DeviceAddress: 1, Name: "设备 A", NodeIDs: []string{"A"}},
			{DeviceID: "2", DeviceAddress: 2, Name: "设备 B", NodeIDs: []string{"B"}},
		},
	}
	ownerA := domain.TopologyOwner{PortID: port.PortID, DeviceID: "1"}
	ownerB := domain.TopologyOwner{PortID: port.PortID, DeviceID: "2"}
	topology := domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{port}, Calculations: []domain.TopologyCalculation{
		{NodeID: "CALC_A", DependencyNodeIDs: []string{"A"}, Owners: []domain.TopologyOwner{ownerA}},
		{NodeID: "CALC_AB", DependencyNodeIDs: []string{"CALC_A", "B"}, Owners: []domain.TopologyOwner{ownerA, ownerB}},
		{NodeID: "CONSTANT", DependencyNodeIDs: []string{}, Owners: []domain.TopologyOwner{}},
	}}
	nodes := []domain.CatalogNode{
		legacyVariable("A", domain.DataTypeUInt32, false), legacyVariable("B", domain.DataTypeUInt32, false),
		legacyVariable("CALC_A", domain.DataTypeDouble, false), legacyVariable("CALC_AB", domain.DataTypeDouble, false),
		legacyVariable("CONSTANT", domain.DataTypeDouble, false), legacyVariable("ttyS0_Send", domain.DataTypeString, true),
		{Key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "ttyS0_SendCommand"}, BrowseName: "ttyS0_SendCommand",
			ParentPath: "Objects/Methods", Class: domain.NodeClassMethod, ValueRank: -1,
			InputArguments:  []domain.Argument{{Name: "HexData", DataType: domain.DataTypeString, ValueRank: -1}},
			OutputArguments: []domain.Argument{{Name: "Result", DataType: domain.DataTypeString, ValueRank: -1}}},
	}
	return topology, nodes
}

func topologyJSON(t *testing.T, value domain.SourceTopology) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTopologyManifestDecoratesDevicesAndCalculationsWithoutChangingLegacyShape(t *testing.T) {
	topology, nodes := topologyFixture()
	before := append([]domain.CatalogNode(nil), nodes...)
	decoded, err := decodeTopologyManifest(topologyJSON(t, topology), nodes)
	if err != nil || !reflect.DeepEqual(decoded, &topology) {
		t.Fatalf("topology=%#v err=%v", decoded, err)
	}
	for i, node := range nodes {
		legacy := node
		legacy.Kind, legacy.PortID, legacy.DeviceID = "", "", ""
		if !reflect.DeepEqual(legacy, before[i]) {
			t.Fatalf("legacy node attributes changed: %#v", node)
		}
	}
	if nodes[0].PortID != "/dev/ttyS0" || nodes[0].DeviceID != "1" || nodes[0].Kind != "point" || nodes[1].DeviceID != "2" {
		t.Fatalf("device ownership: %#v", nodes[:2])
	}
	if nodes[2].DeviceID != "1" || nodes[2].Kind != "calculation" || nodes[3].DeviceID != "" || nodes[4].DeviceID != "" {
		t.Fatalf("calculation ownership: %#v", nodes[2:5])
	}
	if nodes[5].Kind != "command" || nodes[5].DeviceID != "" || nodes[6].PortID != "/dev/ttyS0" {
		t.Fatalf("command ownership: %#v", nodes[5:])
	}
}

func TestTopologyManifestRejectsConflictsAtomically(t *testing.T) {
	tests := []struct {
		name   string
		change func(*domain.SourceTopology, []domain.CatalogNode)
	}{
		{"schema", func(top *domain.SourceTopology, _ []domain.CatalogNode) { top.SchemaVersion = 2 }},
		{"duplicate port", func(top *domain.SourceTopology, _ []domain.CatalogNode) { top.Ports = append(top.Ports, top.Ports[0]) }},
		{"duplicate device", func(top *domain.SourceTopology, _ []domain.CatalogNode) {
			top.Ports[0].Devices = append(top.Ports[0].Devices, top.Ports[0].Devices[0])
		}},
		{"address mismatch", func(top *domain.SourceTopology, _ []domain.CatalogNode) { top.Ports[0].Devices[0].DeviceID = "2" }},
		{"address overflow", func(top *domain.SourceTopology, _ []domain.CatalogNode) { top.Ports[0].Devices[0].DeviceAddress = 256 }},
		{"duplicate ownership", func(top *domain.SourceTopology, _ []domain.CatalogNode) {
			top.Ports[0].Devices[1].NodeIDs = append(top.Ports[0].Devices[1].NodeIDs, "A")
		}},
		{"missing point", func(top *domain.SourceTopology, _ []domain.CatalogNode) {
			top.Ports[0].Devices[0].NodeIDs = []string{"MISSING"}
		}},
		{"missing owner", func(top *domain.SourceTopology, _ []domain.CatalogNode) {
			top.Calculations[0].Owners[0].DeviceID = "99"
		}},
		{"duplicate calculation owner", func(top *domain.SourceTopology, _ []domain.CatalogNode) {
			top.Calculations[0].Owners = append(top.Calculations[0].Owners, top.Calculations[0].Owners[0])
		}},
		{"unowned calculation", func(top *domain.SourceTopology, _ []domain.CatalogNode) { top.Calculations = top.Calculations[:2] }},
		{"command name", func(top *domain.SourceTopology, _ []domain.CatalogNode) { top.Ports[0].SendNodeID = "A" }},
		{"command shape", func(_ *domain.SourceTopology, nodes []domain.CatalogNode) { nodes[5].Writable = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			topology, nodes := topologyFixture()
			test.change(&topology, nodes)
			before := append([]domain.CatalogNode(nil), nodes...)
			if _, err := decodeTopologyManifest(topologyJSON(t, topology), nodes); !errors.Is(err, ErrInvalidTopology) {
				t.Fatalf("want catalog conflict, got %v", err)
			}
			if !reflect.DeepEqual(nodes, before) {
				t.Fatal("invalid topology partially modified catalog")
			}
		})
	}
}

func TestTopologyManifestRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"schema_version":1,"unexpected":true}`, `{"schema_version":1} {}`, strings.Repeat(" ", maxTopologyManifestBytes+1)} {
		if _, err := decodeTopologyManifest(raw, nil); !errors.Is(err, ErrInvalidTopology) {
			t.Fatalf("malformed JSON length=%d got %v", len(raw), err)
		}
	}
}

func TestTopologyDiscoveryResolvesNamespaceURIAndKeepsFlatBusinessNodes(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "opc.tcp://" + reservation.Addr().String()
	_ = reservation.Close()
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewUpstreamServer(UpstreamConfig{EndpointURL: endpoint, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath}, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	topology, nodes := topologyFixture()
	group := domain.Group{ID: "fallback"}
	source := domain.Source{ID: "lower", GroupID: group.ID}
	union := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{group.ID: {}}}
	for _, node := range nodes {
		union.Groups[group.ID][node.Key.Identifier] = node
	}
	if err := server.ReconcileCatalog(union, []domain.Group{group}, []domain.Source{source}); err != nil {
		t.Fatal(err)
	}
	// The topology namespace is intentionally not the Rust default index.
	server.server.NamespaceManager().Add("urn:test:extra")
	topologyNS := server.server.NamespaceManager().Add(TopologyNamespaceURI)
	manifest := awserver.NewVariableNode(server.server, awu.NewNodeIDString(topologyNS, "Topology/Manifest"),
		awu.NewQualifiedName(topologyNS, "Manifest"), awu.NewLocalizedText("Manifest", ""), awu.NewLocalizedText("", ""), nil, nil,
		awu.NewDataValue(topologyJSON(t, topology), awu.Good, time.Now(), 0, time.Now(), 0),
		awu.DataTypeIDString, awu.ValueRankScalar, nil, awu.AccessLevelsCurrentRead, 0, false, nil)
	if err := server.server.NamespaceManager().AddNode(manifest); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("topology test server failed to stop")
		}
	})
	client := connectAnonymousClient(t, endpoint)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	catalog, observed, err := discoverCatalog(ctx, client, source, namespaceIndex(client.Namespaces()))
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Topology == nil || len(catalog.Topology.Ports) != 1 || len(catalog.Nodes) != len(nodes) || len(observed) != len(nodes)-1 {
		t.Fatalf("catalog topology=%#v nodes=%d observed=%d", catalog.Topology, len(catalog.Nodes), len(observed))
	}
	for _, node := range catalog.Nodes {
		if node.Key.Identifier == "A" && (node.PortID != "/dev/ttyS0" || node.DeviceID != "1") {
			t.Fatalf("namespace-resolved ownership missing: %#v", node)
		}
	}
}
