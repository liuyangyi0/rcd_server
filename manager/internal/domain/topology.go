package domain

import (
	"fmt"
	"sort"
	"strings"
)

type GroupBinding struct {
	GroupID  string `json:"group_id"`
	PortID   string `json:"port_id"`
	DeviceID string `json:"device_id"`
}

type TopologyOwner struct {
	PortID   string `json:"port_id"`
	DeviceID string `json:"device_id"`
}

type TopologyDevice struct {
	DeviceID      string   `json:"device_id"`
	DeviceAddress uint32   `json:"device_address"`
	Name          string   `json:"name"`
	KKSPrefix     string   `json:"kks_prefix"`
	NodeIDs       []string `json:"node_ids"`
}

type TopologyPort struct {
	PortID       string           `json:"port_id"`
	PortName     string           `json:"port_name"`
	SendNodeID   string           `json:"send_node_id"`
	SendMethodID string           `json:"send_method_id"`
	Devices      []TopologyDevice `json:"devices"`
}

type TopologyCalculation struct {
	NodeID            string          `json:"node_id"`
	DependencyNodeIDs []string        `json:"dependency_node_ids"`
	Owners            []TopologyOwner `json:"owners"`
}

type SourceTopology struct {
	SchemaVersion int                   `json:"schema_version"`
	Ports         []TopologyPort        `json:"ports"`
	Calculations  []TopologyCalculation `json:"calculations"`
}

func (s Source) HasGroup(groupID string) bool {
	if s.GroupID == groupID {
		return true
	}
	for _, binding := range s.GroupBindings {
		if binding.GroupID == groupID {
			return true
		}
	}
	return false
}

func (s Source) GroupIDs() []string {
	ids := map[string]struct{}{s.GroupID: {}}
	for _, binding := range s.GroupBindings {
		ids[binding.GroupID] = struct{}{}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s Source) GroupAvailable(groupID string) bool {
	if !s.HasGroup(groupID) {
		return false
	}
	if groupID == s.GroupID {
		return true
	}
	for _, available := range s.AvailableGroupIDs {
		if available == groupID {
			return true
		}
	}
	return false
}

func (s Source) Clone() Source {
	s.GroupBindings = append([]GroupBinding(nil), s.GroupBindings...)
	s.AvailableGroupIDs = append([]string(nil), s.AvailableGroupIDs...)
	return s
}

func (s Source) GroupForDevice(portID, deviceID string) string {
	for _, binding := range s.GroupBindings {
		if binding.PortID == portID && binding.DeviceID == deviceID {
			return binding.GroupID
		}
	}
	return s.GroupID
}

func ValidateGroupBindings(bindings []GroupBinding) error {
	seen := make(map[TopologyOwner]struct{}, len(bindings))
	for _, binding := range bindings {
		if strings.TrimSpace(binding.GroupID) == "" || strings.TrimSpace(binding.PortID) == "" || strings.TrimSpace(binding.DeviceID) == "" {
			return &FieldError{Field: "group_bindings", Message: "group_id, port_id and device_id are required"}
		}
		key := TopologyOwner{PortID: binding.PortID, DeviceID: binding.DeviceID}
		if _, exists := seen[key]; exists {
			return &FieldError{Field: "group_bindings", Message: "a device can only be bound once"}
		}
		seen[key] = struct{}{}
	}
	return nil
}

func SameGroupBindings(a, b []GroupBinding) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[GroupBinding]struct{}, len(a))
	for _, binding := range a {
		set[binding] = struct{}{}
	}
	for _, binding := range b {
		if _, exists := set[binding]; !exists {
			return false
		}
	}
	return true
}

func (t *SourceTopology) Clone() *SourceTopology {
	if t == nil {
		return nil
	}
	clone := *t
	clone.Ports = append([]TopologyPort(nil), t.Ports...)
	for i := range clone.Ports {
		clone.Ports[i].Devices = append([]TopologyDevice(nil), t.Ports[i].Devices...)
		for j := range clone.Ports[i].Devices {
			clone.Ports[i].Devices[j].NodeIDs = append([]string(nil), t.Ports[i].Devices[j].NodeIDs...)
		}
	}
	clone.Calculations = append([]TopologyCalculation(nil), t.Calculations...)
	for i := range clone.Calculations {
		clone.Calculations[i].Owners = append([]TopologyOwner(nil), t.Calculations[i].Owners...)
		clone.Calculations[i].DependencyNodeIDs = append([]string(nil), t.Calculations[i].DependencyNodeIDs...)
	}
	return &clone
}

func (c SourceCatalog) NodeGroup(node CatalogNode) string {
	if node.GroupID != "" {
		return node.GroupID
	}
	return c.GroupID
}

// AssignCatalogGroups only annotates business nodes; their public NodeIds and
// legacy browse parents remain unchanged. No sample value is copied or stored.
func AssignCatalogGroups(source Source, catalog SourceCatalog) (SourceCatalog, error) {
	catalog.GroupID = source.GroupID
	nodes := make([]CatalogNode, len(catalog.Nodes))
	for i := range catalog.Nodes {
		nodes[i] = catalog.Nodes[i].Clone()
	}
	catalog.Nodes = nodes
	catalog.Topology = catalog.Topology.Clone()
	nodeIndexes := make(map[string]int, len(catalog.Nodes))
	for i := range catalog.Nodes {
		if _, exists := nodeIndexes[catalog.Nodes[i].Key.Identifier]; exists {
			return SourceCatalog{}, &FieldError{Field: "topology.nodes", Message: "duplicate business node"}
		}
		nodeIndexes[catalog.Nodes[i].Key.Identifier] = i
		if catalog.Nodes[i].Retired {
			continue
		}
		catalog.Nodes[i].GroupID = source.GroupID
		catalog.Nodes[i].PortID, catalog.Nodes[i].DeviceID = "", ""
		catalog.Nodes[i].Kind = "point"
		if catalog.Nodes[i].Class == NodeClassMethod || (catalog.Topology == nil && strings.HasSuffix(catalog.Nodes[i].Key.Identifier, "_Send") && catalog.Nodes[i].Class == NodeClassVariable && validCommandVariable(catalog.Nodes[i])) {
			catalog.Nodes[i].Kind = "command"
		}
	}
	if catalog.Topology == nil {
		return catalog, nil
	}
	if catalog.Topology.SchemaVersion != 1 {
		return SourceCatalog{}, &FieldError{Field: "topology.schema_version", Message: "unsupported version"}
	}
	owners := make(map[string]TopologyOwner)
	devices := make(map[TopologyOwner]struct{})
	ports := make(map[string]struct{})
	commands := make(map[string]struct{})
	for _, port := range catalog.Topology.Ports {
		if strings.TrimSpace(port.PortID) == "" || port.PortName == "" || port.SendNodeID != port.PortName+"_Send" || port.SendMethodID != port.PortName+"_SendCommand" {
			return SourceCatalog{}, &FieldError{Field: "topology.ports", Message: "port and matching command node identifiers are required"}
		}
		if _, exists := ports[port.PortID]; exists {
			return SourceCatalog{}, &FieldError{Field: "topology.ports", Message: "duplicate port"}
		}
		ports[port.PortID] = struct{}{}
		for _, commandID := range []string{port.SendNodeID, port.SendMethodID} {
			i, exists := nodeIndexes[commandID]
			if !exists {
				return SourceCatalog{}, &FieldError{Field: "topology.commands", Message: "missing command " + commandID}
			}
			if _, duplicate := commands[commandID]; duplicate {
				return SourceCatalog{}, &FieldError{Field: "topology.commands", Message: "duplicate command ownership"}
			}
			commands[commandID] = struct{}{}
			node := catalog.Nodes[i]
			if commandID == port.SendNodeID && (node.Class != NodeClassVariable || !validCommandVariable(node)) {
				return SourceCatalog{}, &FieldError{Field: "topology.commands", Message: "Send must be writable scalar String"}
			}
			if commandID == port.SendMethodID && (node.Class != NodeClassMethod || !validCommandMethod(node)) {
				return SourceCatalog{}, &FieldError{Field: "topology.commands", Message: "invalid SendCommand signature"}
			}
			catalog.Nodes[i].PortID = port.PortID
			catalog.Nodes[i].Kind = "command"
		}
		for _, device := range port.Devices {
			owner := TopologyOwner{PortID: port.PortID, DeviceID: device.DeviceID}
			if device.DeviceID != fmt.Sprint(device.DeviceAddress) || device.DeviceAddress > 255 {
				return SourceCatalog{}, &FieldError{Field: "topology.device", Message: "device ID must match byte-sized address"}
			}
			if _, exists := devices[owner]; exists {
				return SourceCatalog{}, &FieldError{Field: "topology.devices", Message: "duplicate device"}
			}
			devices[owner] = struct{}{}
			for _, identifier := range device.NodeIDs {
				if _, exists := owners[identifier]; exists {
					return SourceCatalog{}, &FieldError{Field: "topology.node_ids", Message: "duplicate point ownership"}
				}
				owners[identifier] = owner
				i, exists := nodeIndexes[identifier]
				if !exists {
					return SourceCatalog{}, &FieldError{Field: "topology.node_ids", Message: "unknown point " + identifier}
				}
				if catalog.Nodes[i].Class != NodeClassVariable || catalog.Nodes[i].Writable || catalog.Nodes[i].Kind == "command" {
					return SourceCatalog{}, &FieldError{Field: "topology.node_ids", Message: "device points must be read-only variables"}
				}
				catalog.Nodes[i].PortID, catalog.Nodes[i].DeviceID = port.PortID, device.DeviceID
				catalog.Nodes[i].GroupID = source.GroupForDevice(port.PortID, device.DeviceID)
			}
		}
	}
	for _, calculation := range catalog.Topology.Calculations {
		i, exists := nodeIndexes[calculation.NodeID]
		if !exists {
			return SourceCatalog{}, &FieldError{Field: "topology.calculations", Message: "unknown calculation " + calculation.NodeID}
		}
		if catalog.Nodes[i].Class != NodeClassVariable || catalog.Nodes[i].Writable || catalog.Nodes[i].Kind == "command" {
			return SourceCatalog{}, &FieldError{Field: "topology.calculations", Message: "calculation must be a read-only variable"}
		}
		if _, duplicate := owners[calculation.NodeID]; duplicate {
			return SourceCatalog{}, &FieldError{Field: "topology.calculations", Message: "duplicate calculation ownership"}
		}
		owners[calculation.NodeID] = TopologyOwner{}
		catalog.Nodes[i].Kind = "calculation"
		seenOwners := make(map[TopologyOwner]bool)
		for _, owner := range calculation.Owners {
			if _, exists := devices[owner]; !exists || seenOwners[owner] {
				return SourceCatalog{}, &FieldError{Field: "topology.calculations.owners", Message: "unknown or duplicate device owner"}
			}
			seenOwners[owner] = true
		}
		if len(calculation.Owners) == 1 {
			owner := calculation.Owners[0]
			catalog.Nodes[i].PortID, catalog.Nodes[i].DeviceID = owner.PortID, owner.DeviceID
			catalog.Nodes[i].GroupID = source.GroupForDevice(owner.PortID, owner.DeviceID)
		}
	}
	for _, node := range catalog.Nodes {
		if node.Retired {
			continue
		}
		if _, owned := owners[node.Key.Identifier]; owned {
			continue
		}
		if _, owned := commands[node.Key.Identifier]; !owned {
			return SourceCatalog{}, &FieldError{Field: "topology.nodes", Message: "business node has no topology ownership: " + node.Key.Identifier}
		}
	}
	return catalog, nil
}

func AvailableGroups(source Source, catalog SourceCatalog) []string {
	groups := map[string]struct{}{source.GroupID: {}}
	if catalog.Topology != nil {
		for _, port := range catalog.Topology.Ports {
			for _, device := range port.Devices {
				groups[source.GroupForDevice(port.PortID, device.DeviceID)] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(groups))
	for group := range groups {
		out = append(out, group)
	}
	sort.Strings(out)
	return out
}
