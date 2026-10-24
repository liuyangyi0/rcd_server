package opc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

// A malformed advertised topology is a catalog conflict, not an old server
// without grouping support. Never silently fall back to the legacy route.
var ErrInvalidTopology = errors.New("invalid lower device topology")

const maxTopologyManifestBytes = 8 << 20

func discoverTopology(ctx context.Context, client *gopcua.Client, namespaces map[string]uint16, nodes []domain.CatalogNode) (*domain.SourceTopology, error) {
	ns, exists := namespaces[TopologyNamespaceURI]
	if !exists {
		return nil, nil
	}
	response, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead:        []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, "Topology/Manifest"), AttributeID: gu.AttributeIDValue}},
	})
	if err != nil {
		return nil, fmt.Errorf("read lower topology: %w", err)
	}
	if response == nil || len(response.Results) != 1 {
		return nil, fmt.Errorf("%w: incomplete manifest read", ErrInvalidTopology)
	}
	if err := goodDataValue(response.Results[0]); err != nil {
		return nil, fmt.Errorf("%w: manifest value: %v", ErrInvalidTopology, err)
	}
	raw, ok := response.Results[0].Value.Value().(string)
	if !ok {
		return nil, fmt.Errorf("%w: manifest must be a String", ErrInvalidTopology)
	}
	return decodeTopologyManifest(raw, nodes)
}

func decodeTopologyManifest(raw string, nodes []domain.CatalogNode) (*domain.SourceTopology, error) {
	invalid := func(format string, args ...any) (*domain.SourceTopology, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidTopology, fmt.Sprintf(format, args...))
	}
	if len(raw) == 0 || len(raw) > maxTopologyManifestBytes {
		return invalid("manifest size must be between 1 and %d bytes", maxTopologyManifestBytes)
	}
	var topology domain.SourceTopology
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&topology); err != nil {
		return invalid("decode manifest: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid("manifest must contain exactly one JSON object")
	}
	if topology.SchemaVersion != 1 {
		return invalid("unsupported schema_version %d", topology.SchemaVersion)
	}
	known := make(map[string]domain.CatalogNode, len(nodes))
	for _, node := range nodes {
		if node.Key.NamespaceURI != BusinessNamespaceURI || node.Key.Identifier == "" {
			return invalid("invalid business node %v", node.Key)
		}
		if _, exists := known[node.Key.Identifier]; exists {
			return invalid("duplicate business node %q", node.Key.Identifier)
		}
		known[node.Key.Identifier] = node
	}
	claimed := make(map[string]string, len(nodes))
	claim := func(identifier, owner string, class domain.NodeClass) error {
		node, exists := known[identifier]
		if !exists || node.Class != class {
			return fmt.Errorf("%s references missing or incompatible node %q", owner, identifier)
		}
		if previous, exists := claimed[identifier]; exists {
			return fmt.Errorf("node %q belongs to both %s and %s", identifier, previous, owner)
		}
		claimed[identifier] = owner
		return nil
	}
	ports := make(map[string]struct{}, len(topology.Ports))
	devices := make(map[domain.TopologyOwner]struct{})
	for _, port := range topology.Ports {
		if strings.TrimSpace(port.PortID) == "" || port.PortName == "" {
			return invalid("port_id and port_name are required")
		}
		if _, exists := ports[port.PortID]; exists {
			return invalid("duplicate port %q", port.PortID)
		}
		ports[port.PortID] = struct{}{}
		if port.SendNodeID != port.PortName+"_Send" || port.SendMethodID != port.PortName+"_SendCommand" {
			return invalid("port %q command names do not match its port_name", port.PortID)
		}
		if err := claim(port.SendNodeID, "port "+port.PortID, domain.NodeClassVariable); err != nil {
			return invalid("%v", err)
		}
		send := known[port.SendNodeID]
		if send.DataType != domain.DataTypeString || send.ValueRank != -1 || !send.Writable {
			return invalid("port %q Send must be a writable scalar String", port.PortID)
		}
		if err := claim(port.SendMethodID, "port "+port.PortID, domain.NodeClassMethod); err != nil {
			return invalid("%v", err)
		}
		if !validProxyMethod(known[port.SendMethodID]) {
			return invalid("port %q SendCommand has an incompatible signature", port.PortID)
		}
		for _, device := range port.Devices {
			if device.DeviceAddress > 255 || device.DeviceID != strconv.FormatUint(uint64(device.DeviceAddress), 10) {
				return invalid("port %q device_id must be the decimal address in [0,255]", port.PortID)
			}
			owner := domain.TopologyOwner{PortID: port.PortID, DeviceID: device.DeviceID}
			if _, exists := devices[owner]; exists {
				return invalid("duplicate device %q/%q", port.PortID, device.DeviceID)
			}
			devices[owner] = struct{}{}
			for _, identifier := range device.NodeIDs {
				if err := claim(identifier, "device "+port.PortID+"/"+device.DeviceID, domain.NodeClassVariable); err != nil {
					return invalid("%v", err)
				}
				if known[identifier].Writable {
					return invalid("device point %q cannot be a command variable", identifier)
				}
			}
		}
	}
	for _, calculation := range topology.Calculations {
		if err := claim(calculation.NodeID, "calculation "+calculation.NodeID, domain.NodeClassVariable); err != nil {
			return invalid("%v", err)
		}
		if known[calculation.NodeID].Writable {
			return invalid("calculation %q cannot be a command variable", calculation.NodeID)
		}
		owners := make(map[domain.TopologyOwner]struct{}, len(calculation.Owners))
		for _, owner := range calculation.Owners {
			if _, exists := devices[owner]; !exists {
				return invalid("calculation %q references missing device %q/%q", calculation.NodeID, owner.PortID, owner.DeviceID)
			}
			if _, duplicate := owners[owner]; duplicate {
				return invalid("calculation %q repeats an owner", calculation.NodeID)
			}
			owners[owner] = struct{}{}
		}
	}
	for identifier := range known {
		if _, exists := claimed[identifier]; !exists {
			return invalid("business node %q has no topology ownership", identifier)
		}
	}
	// Metadata is attached only after the complete document validates; a failed
	// read must not partially alter the catalog that might later be persisted.
	decorateTopologyNodes(nodes, &topology)
	return &topology, nil
}

func decorateTopologyNodes(nodes []domain.CatalogNode, topology *domain.SourceTopology) {
	byID := make(map[string]*domain.CatalogNode, len(nodes))
	for i := range nodes {
		byID[nodes[i].Key.Identifier] = &nodes[i]
	}
	for _, port := range topology.Ports {
		for _, identifier := range []string{port.SendNodeID, port.SendMethodID} {
			node := byID[identifier]
			node.Kind, node.PortID, node.DeviceID = "command", port.PortID, ""
		}
		for _, device := range port.Devices {
			for _, identifier := range device.NodeIDs {
				node := byID[identifier]
				node.Kind, node.PortID, node.DeviceID = "point", port.PortID, device.DeviceID
			}
		}
	}
	for _, calculation := range topology.Calculations {
		node := byID[calculation.NodeID]
		node.Kind, node.PortID, node.DeviceID = "calculation", "", ""
		if len(calculation.Owners) == 1 {
			node.PortID, node.DeviceID = calculation.Owners[0].PortID, calculation.Owners[0].DeviceID
		}
	}
}
