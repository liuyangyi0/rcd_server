package opc

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
)

type groupMemberTreeSpec struct {
	SourceID         string                    `json:"source_id"`
	SourceName       string                    `json:"source_name"`
	Endpoint         string                    `json:"endpoint"`
	UsesDefaultGroup bool                      `json:"uses_default_group"`
	ExpectedPoints   uint32                    `json:"expected_points"`
	ProvidedPoints   uint32                    `json:"provided_points"`
	MissingPoints    uint32                    `json:"missing_points"`
	CompleteCoverage bool                      `json:"complete_coverage"`
	CoverageKnown    bool                      `json:"coverage_known"`
	CoverageHash     string                    `json:"coverage_hash"`
	ProvidedPointIDs []string                  `json:"provided_point_ids"`
	MissingPointIDs  []string                  `json:"missing_point_ids"`
	Ports            []groupMemberTreePortSpec `json:"ports"`
}

type groupMemberTreePortSpec struct {
	PortID       string                      `json:"port_id"`
	PortName     string                      `json:"port_name"`
	SendNodeID   string                      `json:"send_node_id"`
	SendMethodID string                      `json:"send_method_id"`
	Devices      []groupMemberTreeDeviceSpec `json:"devices"`
}

type groupMemberTreeDeviceSpec struct {
	DeviceID          string `json:"device_id"`
	DeviceAddress     uint32 `json:"device_address"`
	AddressKnown      bool   `json:"address_known"`
	DeviceName        string `json:"device_name"`
	KKSPrefix         string `json:"kks_prefix"`
	PointCount        uint32 `json:"point_count"`
	RawPointCount     uint32 `json:"raw_point_count"`
	CalculationPoints uint32 `json:"calculation_points"`
	Configured        bool   `json:"configured"`
	ExplicitlyBound   bool   `json:"explicitly_bound"`
	PresentInTopology bool   `json:"present_in_topology"`
}

type memberTreeTracker struct {
	nodes    map[string]awu.NodeID
	routeIDs []awu.NodeID
}

func newMemberTreeTracker() *memberTreeTracker {
	return &memberTreeTracker{nodes: make(map[string]awu.NodeID)}
}

func (t *memberTreeTracker) object(id awu.NodeID) {
	t.nodes[fmt.Sprint(id)] = id
}

func (t *memberTreeTracker) value(id awu.NodeID) {
	t.nodes[fmt.Sprint(id)] = id
}

func (t *memberTreeTracker) routeValue(id awu.NodeID) {
	t.value(id)
	t.routeIDs = append(t.routeIDs, id)
}

// reconcileGroupMemberTreeLocked publishes the physical membership behind one
// logical group without creating another routing control. OPC clients browse
// this tree, then continue to switch the current Session only through the
// group's SelectedSourceId variable.
func (s *UpstreamServer) reconcileGroupMemberTreeLocked(group domain.Group, sources []domain.Source, catalog domain.CatalogUnion) error {
	specs := buildGroupMemberTreeSpecs(group.ID, sources, catalog)
	hasher := sha256.New()
	encoder := json.NewEncoder(hasher)
	for _, spec := range specs {
		// CoverageHash already commits to both point-ID arrays. Do not scan and
		// serialize those potentially large lists a second time just to decide
		// whether the physical member tree needs reconciliation.
		spec.ProvidedPointIDs = nil
		spec.MissingPointIDs = nil
		if err := encoder.Encode(spec); err != nil {
			return fmt.Errorf("encode group %q member tree: %w", group.ID, err)
		}
	}
	hash := fmt.Sprintf("%x", hasher.Sum(nil))
	rootIdentifier := "Groups/" + group.ID + "/Members"
	rootID := awu.NewNodeIDString(s.managementNS, rootIdentifier)
	if s.memberTreeHashes[group.ID] == hash && s.memberTreeNodeIDs[group.ID] != nil {
		if _, exists := s.server.NamespaceManager().FindNode(rootID); exists {
			s.purgeMemberCoverageSnapshotsLocked(group.ID, specs)
			return nil
		}
	}

	tracker := newMemberTreeTracker()
	groupID := awu.NewNodeIDString(s.managementNS, "Groups/"+group.ID)
	if err := s.ensureMemberObjectLocked(rootID, "Members", "Members", groupID); err != nil {
		return err
	}
	tracker.object(rootID)
	for _, member := range specs {
		if err := s.addGroupMemberLocked(group.ID, rootID, member, tracker); err != nil {
			return err
		}
	}

	stale := make([]awu.NodeID, 0)
	for key, id := range s.memberTreeNodeIDs[group.ID] {
		if _, keep := tracker.nodes[key]; !keep {
			stale = append(stale, id)
		}
	}
	// Delete the shallowest stale branch first. Recursive HasComponent removal
	// then cleans its descendants, while every unchanged NodeId remains live for
	// existing monitored items throughout the reconciliation.
	sort.Slice(stale, func(i, j int) bool {
		left, right := fmt.Sprint(stale[i]), fmt.Sprint(stale[j])
		leftDepth, rightDepth := strings.Count(left, "/"), strings.Count(right, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return left < right
	})
	for _, id := range stale {
		if node, exists := s.server.NamespaceManager().FindNode(id); exists {
			if err := s.server.NamespaceManager().DeleteNode(node, true); err != nil {
				return fmt.Errorf("remove stale group %q member node %s: %w", group.ID, id, err)
			}
		}
	}
	s.purgeMissingManagementIndexLocked()
	s.memberTreeNodeIDs[group.ID] = tracker.nodes
	sort.Slice(tracker.routeIDs, func(i, j int) bool { return fmt.Sprint(tracker.routeIDs[i]) < fmt.Sprint(tracker.routeIDs[j]) })
	s.memberTreeRouteIDs[group.ID] = tracker.routeIDs
	s.memberTreeHashes[group.ID] = hash
	s.purgeMemberCoverageSnapshotsLocked(group.ID, specs)
	return nil
}

func (s *UpstreamServer) addGroupMemberLocked(groupID string, parent awu.NodeID, member groupMemberTreeSpec, tracker *memberTreeTracker) error {
	coverageSnapshot := s.updateMemberCoverageSnapshotLocked(groupID, member)
	sourcePath := "Groups/" + groupID + "/Members/" + managementPathComponent(member.SourceID)
	sourceID := awu.NewNodeIDString(s.managementNS, sourcePath)
	if err := s.ensureMemberObjectLocked(sourceID, member.SourceID, member.SourceID, parent); err != nil {
		return err
	}
	tracker.object(sourceID)

	static := []managementVariable{
		{name: "SourceId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(member.SourceID)},
		{name: "SourceName", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(member.SourceName)},
		{name: "Endpoint", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(member.Endpoint)},
		{name: "UsesDefaultGroup", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: staticManagementValue(member.UsesDefaultGroup)},
		{name: "PortCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: staticManagementValue(uint32(len(member.Ports)))},
		{name: "ExpectedPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.expectedPoints })},
		{name: "ProvidedPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.providedPoints })},
		{name: "MissingPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.missingPoints })},
		{name: "CompleteCoverage", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.completeCoverage })},
		{name: "CoverageKnown", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.coverageKnown })},
		{name: "CoverageFingerprint", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.fingerprint })},
		{name: "CoverageRevision", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.revision })},
		{name: "ProvidedPointNodeIds", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankOneDimension, minimumSamplingInterval: 1000, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.providedIDs })},
		{name: "MissingPointNodeIds", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankOneDimension, minimumSamplingInterval: 1000, read: memberCoverageValue(coverageSnapshot, func(snapshot *memberCoverageSnapshot) any { return snapshot.missingIDs })},
	}
	for _, definition := range static {
		if err := s.addMemberVariableLocked(tracker, sourceID, sourcePath+"/"+definition.name, definition); err != nil {
			return err
		}
	}
	if err := s.addMemberVariableLocked(tracker, sourceID, sourcePath+"/Selectable", managementVariable{
		name: "Selectable", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar,
		read: func(string) (any, awu.StatusCode) {
			source, ok := findSource(s.backend.Sources(), member.SourceID)
			if !ok {
				return false, awu.BadNotFound
			}
			return source.Enabled && source.CatalogState.Selectable() && source.GroupAvailable(groupID), awu.Good
		},
	}); err != nil {
		return err
	}
	if err := s.addRouteMemberVariableLocked(tracker, sourceID, sourcePath+"/IsSelected", managementVariable{
		name: "IsSelected", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar,
		read: func(sessionID string) (any, awu.StatusCode) {
			session, err := s.backend.Session(sessionID)
			if err != nil {
				return false, statusFromBackend(domain.StatusGood, err)
			}
			route, ok := session.Routes[groupID]
			if !ok {
				return false, awu.BadNotFound
			}
			return route.SourceID == member.SourceID, awu.Good
		},
	}); err != nil {
		return err
	}
	if err := s.addMemberVariableLocked(tracker, sourceID, sourcePath+"/ConnectionState", managementVariable{
		name: "ConnectionState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar,
		read: func(string) (any, awu.StatusCode) {
			status, ok := s.backend.LowerStatus(member.SourceID)
			if !ok {
				return "", awu.BadNotFound
			}
			return string(status.Connection), awu.Good
		},
	}); err != nil {
		return err
	}
	if err := s.addMemberVariableLocked(tracker, sourceID, sourcePath+"/CatalogState", managementVariable{
		name: "CatalogState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar,
		read: func(string) (any, awu.StatusCode) {
			source, ok := findSource(s.backend.Sources(), member.SourceID)
			if !ok {
				return "", awu.BadNotFound
			}
			return string(source.CatalogState), awu.Good
		},
	}); err != nil {
		return err
	}

	portsPath := sourcePath + "/Ports"
	portsID := awu.NewNodeIDString(s.managementNS, portsPath)
	if err := s.ensureMemberObjectLocked(portsID, "Ports", "Ports", sourceID); err != nil {
		return err
	}
	tracker.object(portsID)
	for _, port := range member.Ports {
		if err := s.addGroupMemberPortLocked(portsID, portsPath, port, tracker); err != nil {
			return err
		}
	}
	return nil
}

func (s *UpstreamServer) addGroupMemberPortLocked(parent awu.NodeID, parentPath string, port groupMemberTreePortSpec, tracker *memberTreeTracker) error {
	portPath := parentPath + "/" + managementPathComponent(port.PortID)
	portID := awu.NewNodeIDString(s.managementNS, portPath)
	if err := s.ensureMemberObjectLocked(portID, port.PortID, port.PortID, parent); err != nil {
		return err
	}
	tracker.object(portID)
	definitions := []managementVariable{
		{name: "PortId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(port.PortID)},
		{name: "PortName", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(port.PortName)},
		{name: "SendNodeId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(port.SendNodeID)},
		{name: "SendMethodId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(port.SendMethodID)},
		{name: "DeviceCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: staticManagementValue(uint32(len(port.Devices)))},
	}
	for _, definition := range definitions {
		if err := s.addMemberVariableLocked(tracker, portID, portPath+"/"+definition.name, definition); err != nil {
			return err
		}
	}

	devicesPath := portPath + "/Devices"
	devicesID := awu.NewNodeIDString(s.managementNS, devicesPath)
	if err := s.ensureMemberObjectLocked(devicesID, "Devices", "Devices", portID); err != nil {
		return err
	}
	tracker.object(devicesID)
	for _, device := range port.Devices {
		devicePath := devicesPath + "/" + managementPathComponent(device.DeviceID)
		deviceID := awu.NewNodeIDString(s.managementNS, devicePath)
		if err := s.ensureMemberObjectLocked(deviceID, device.DeviceID, device.DeviceID, devicesID); err != nil {
			return err
		}
		tracker.object(deviceID)
		deviceDefinitions := []managementVariable{
			{name: "DeviceId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.DeviceID)},
			{name: "DeviceAddress", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: deviceAddressManagementValue(device)},
			{name: "AddressKnown", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.AddressKnown)},
			{name: "DeviceName", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.DeviceName)},
			{name: "KKSPrefix", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.KKSPrefix)},
			{name: "PointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.PointCount)},
			{name: "RawPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.RawPointCount)},
			{name: "CalculationPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.CalculationPoints)},
			{name: "Configured", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.Configured)},
			{name: "ExplicitlyBound", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.ExplicitlyBound)},
			{name: "PresentInTopology", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.PresentInTopology)},
			// Retain the first-release alias while documenting its precise meaning.
			{name: "Discovered", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: staticManagementValue(device.PresentInTopology)},
		}
		for _, definition := range deviceDefinitions {
			if err := s.addMemberVariableLocked(tracker, deviceID, devicePath+"/"+definition.name, definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildGroupMemberTreeSpecs(groupID string, sources []domain.Source, catalog domain.CatalogUnion) []groupMemberTreeSpec {
	result := make([]groupMemberTreeSpec, 0, len(sources))
	groupPointIDs := groupPointNodeIDs(groupID, catalog)
	for _, source := range sources {
		providedIDs, missingIDs := memberPointNodeIDsFromGroup(groupID, source.ID, groupPointIDs, catalog)
		expected, provided, missing := uint32(len(providedIDs)+len(missingIDs)), uint32(len(providedIDs)), uint32(len(missingIDs))
		catalogState := catalog.SourceStates[source.ID]
		if catalogState == "" {
			catalogState = source.CatalogState
		}
		// Only a validated live or cached catalog is authoritative. Conflict
		// sources may still expose best-effort lists for diagnosis, but a damaged
		// or quarantined cache must never masquerade as known coverage.
		coverageKnown := catalogState.Selectable()
		member := groupMemberTreeSpec{
			SourceID: source.ID, SourceName: source.Name, Endpoint: source.Endpoint,
			UsesDefaultGroup: source.GroupID == groupID, ExpectedPoints: expected,
			ProvidedPoints: provided, MissingPoints: missing, CompleteCoverage: coverageKnown && missing == 0,
			CoverageKnown: coverageKnown, CoverageHash: pointListFingerprint([]string{string(catalogState)}, providedIDs, missingIDs),
			ProvidedPointIDs: providedIDs, MissingPointIDs: missingIDs,
		}
		configured := make(map[domain.TopologyOwner]bool)
		for _, binding := range source.GroupBindings {
			if binding.GroupID == groupID {
				configured[domain.TopologyOwner{PortID: binding.PortID, DeviceID: binding.DeviceID}] = true
			}
		}
		ports := make(map[string]*groupMemberTreePortSpec)
		if topology := catalog.SourceTopologies[source.ID]; topology != nil {
			calculationCounts := make(map[domain.TopologyOwner]uint32)
			for _, calculation := range topology.Calculations {
				// A cross-device calculation is a group-level point, not a point
				// owned independently by every participating physical station.
				if len(calculation.Owners) == 1 {
					calculationCounts[calculation.Owners[0]]++
				}
			}
			for _, topologyPort := range topology.Ports {
				if len(topologyPort.Devices) == 0 && source.GroupID == groupID {
					ports[topologyPort.PortID] = &groupMemberTreePortSpec{
						PortID: topologyPort.PortID, PortName: topologyPort.PortName,
						SendNodeID: topologyPort.SendNodeID, SendMethodID: topologyPort.SendMethodID,
					}
				}
				for _, topologyDevice := range topologyPort.Devices {
					if source.GroupForDevice(topologyPort.PortID, topologyDevice.DeviceID) != groupID {
						continue
					}
					port := ports[topologyPort.PortID]
					if port == nil {
						port = &groupMemberTreePortSpec{PortID: topologyPort.PortID, PortName: topologyPort.PortName, SendNodeID: topologyPort.SendNodeID, SendMethodID: topologyPort.SendMethodID}
						ports[topologyPort.PortID] = port
					}
					owner := domain.TopologyOwner{PortID: topologyPort.PortID, DeviceID: topologyDevice.DeviceID}
					explicitlyBound := configured[owner]
					rawCount, calculationCount := uint32(len(topologyDevice.NodeIDs)), calculationCounts[owner]
					port.Devices = append(port.Devices, groupMemberTreeDeviceSpec{
						DeviceID: topologyDevice.DeviceID, DeviceAddress: topologyDevice.DeviceAddress, AddressKnown: true,
						DeviceName: topologyDevice.Name, KKSPrefix: topologyDevice.KKSPrefix,
						PointCount: rawCount + calculationCount, RawPointCount: rawCount, CalculationPoints: calculationCount, Configured: true,
						ExplicitlyBound: explicitlyBound, PresentInTopology: true,
					})
					delete(configured, owner)
				}
			}
		}
		for owner := range configured {
			port := ports[owner.PortID]
			if port == nil {
				port = &groupMemberTreePortSpec{PortID: owner.PortID, PortName: owner.PortID}
				ports[owner.PortID] = port
			}
			address, known := parseDeviceAddress(owner.DeviceID)
			port.Devices = append(port.Devices, groupMemberTreeDeviceSpec{
				DeviceID: owner.DeviceID, DeviceAddress: address, AddressKnown: known,
				Configured: true, ExplicitlyBound: true,
			})
		}
		portIDs := make([]string, 0, len(ports))
		for portID := range ports {
			portIDs = append(portIDs, portID)
		}
		sort.Strings(portIDs)
		for _, portID := range portIDs {
			port := ports[portID]
			sort.Slice(port.Devices, func(i, j int) bool { return port.Devices[i].DeviceID < port.Devices[j].DeviceID })
			member.Ports = append(member.Ports, *port)
		}
		result = append(result, member)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SourceID < result[j].SourceID })
	return result
}

// groupPointNodeIDs is the authoritative, sorted list of ns=2 data point
// identifiers routed by one logical group. Command variables and methods are
// intentionally excluded because their physical target is resolved from the
// command payload instead of from a static KKS-to-group lookup.
func groupPointNodeIDs(groupID string, catalog domain.CatalogUnion) []string {
	identifiers := make([]string, 0, len(catalog.Groups[groupID]))
	for identifier, node := range catalog.Groups[groupID] {
		if memberCoveragePoint(node) {
			identifiers = append(identifiers, identifier)
		}
	}
	sort.Strings(identifiers)
	return identifiers
}

func memberPointNodeIDs(groupID, sourceID string, catalog domain.CatalogUnion) (provided, missing []string) {
	return memberPointNodeIDsFromGroup(groupID, sourceID, groupPointNodeIDs(groupID, catalog), catalog)
}

func memberPointNodeIDsFromGroup(groupID, sourceID string, groupPointIDs []string, catalog domain.CatalogUnion) (provided, missing []string) {
	sourceNodes := catalog.SourceNodes[sourceID]
	for _, identifier := range groupPointIDs {
		unionNode := catalog.Groups[groupID][identifier]
		sourceNode, exists := sourceNodes[identifier]
		sameGroup := sourceNode.GroupID == "" || sourceNode.GroupID == groupID
		if exists && sameGroup && memberCoveragePoint(sourceNode) && sourceNode.Class == unionNode.Class && sourceNode.DataType == unionNode.DataType && sourceNode.ValueRank == unionNode.ValueRank {
			provided = append(provided, identifier)
		} else {
			missing = append(missing, identifier)
		}
	}
	return provided, missing
}

func pointListFingerprint(lists ...[]string) string {
	hasher := sha256.New()
	_ = json.NewEncoder(hasher).Encode(lists)
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

func (s *UpstreamServer) updateMemberCoverageSnapshotLocked(groupID string, member groupMemberTreeSpec) *memberCoverageSnapshotStore {
	key := memberCoverageSnapshotKey{groupID: groupID, sourceID: member.SourceID}
	store := s.memberCoverageSnapshots[key]
	if store == nil {
		store = &memberCoverageSnapshotStore{}
		s.memberCoverageSnapshots[key] = store
	}
	current := store.current.Load()
	if current != nil && current.fingerprint == member.CoverageHash {
		return store
	}
	revision := uint64(1)
	if current != nil {
		revision = current.revision + 1
	}
	store.current.Store(&memberCoverageSnapshot{
		revision: revision, expectedPoints: member.ExpectedPoints,
		providedPoints: member.ProvidedPoints, missingPoints: member.MissingPoints,
		completeCoverage: member.CompleteCoverage, coverageKnown: member.CoverageKnown,
		fingerprint: member.CoverageHash,
		providedIDs: append([]string(nil), member.ProvidedPointIDs...),
		missingIDs:  append([]string(nil), member.MissingPointIDs...),
	})
	return store
}

func (s *UpstreamServer) purgeMemberCoverageSnapshotsLocked(groupID string, members []groupMemberTreeSpec) {
	desired := make(map[string]struct{}, len(members))
	for _, member := range members {
		desired[member.SourceID] = struct{}{}
	}
	for key := range s.memberCoverageSnapshots {
		if key.groupID == groupID {
			if _, keep := desired[key.sourceID]; !keep {
				delete(s.memberCoverageSnapshots, key)
			}
		}
	}
}

func memberCoverageValue(store *memberCoverageSnapshotStore, selectValue func(*memberCoverageSnapshot) any) func(string) (any, awu.StatusCode) {
	return func(string) (any, awu.StatusCode) {
		snapshot := store.current.Load()
		if snapshot == nil {
			return nil, awu.BadWaitingForInitialData
		}
		return selectValue(snapshot), awu.Good
	}
}

func memberCoveragePoint(node domain.CatalogNode) bool {
	if node.Retired || node.Class != domain.NodeClassVariable || node.Kind == "command" {
		return false
	}
	return !(node.Writable && node.DataType == domain.DataTypeString && strings.HasSuffix(node.Key.Identifier, "_Send"))
}

func (s *UpstreamServer) ensureMemberObjectLocked(id awu.NodeID, browseName, display string, parent awu.NodeID) error {
	if existing, exists := s.server.NamespaceManager().FindNode(id); exists {
		if _, ok := existing.(*awserver.ObjectNode); !ok {
			return fmt.Errorf("management node %s has class %T, want Object", id, existing)
		}
		s.management[fmt.Sprint(id)] = existing
		return nil
	}
	object := newComponentObject(s.server, s.managementNS, id, browseName, display, parent)
	if err := s.server.NamespaceManager().AddNode(object); err != nil {
		return err
	}
	s.management[fmt.Sprint(id)] = object
	return nil
}

func (s *UpstreamServer) addMemberVariableLocked(tracker *memberTreeTracker, parent awu.NodeID, identifier string, definition managementVariable) error {
	if err := s.addManagementVariableLocked(parent, identifier, definition); err != nil {
		return err
	}
	tracker.value(awu.NewNodeIDString(s.managementNS, identifier))
	return nil
}

func (s *UpstreamServer) addRouteMemberVariableLocked(tracker *memberTreeTracker, parent awu.NodeID, identifier string, definition managementVariable) error {
	if err := s.addManagementVariableLocked(parent, identifier, definition); err != nil {
		return err
	}
	tracker.routeValue(awu.NewNodeIDString(s.managementNS, identifier))
	return nil
}

func deviceAddressManagementValue(device groupMemberTreeDeviceSpec) func(string) (any, awu.StatusCode) {
	return func(string) (any, awu.StatusCode) {
		if !device.AddressKnown {
			return uint32(0), awu.BadWaitingForInitialData
		}
		return device.DeviceAddress, awu.Good
	}
}

func parseDeviceAddress(deviceID string) (uint32, bool) {
	value, err := strconv.ParseUint(strings.TrimSpace(deviceID), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

func newComponentObject(srv *awserver.Server, namespace uint16, id awu.NodeID, browseName, display string, parent awu.NodeID) *awserver.ObjectNode {
	return awserver.NewObjectNode(
		srv, id, awu.NewQualifiedName(namespace, browseName), awu.NewLocalizedText(display, ""), awu.NewLocalizedText("", ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.ObjectTypeIDFolderType)),
			awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(parent)),
		}, 0,
	)
}

func staticManagementValue(value any) func(string) (any, awu.StatusCode) {
	return func(string) (any, awu.StatusCode) { return value, awu.Good }
}

func staticStringSliceManagementValue(value []string) func(string) (any, awu.StatusCode) {
	snapshot := append([]string(nil), value...)
	return func(string) (any, awu.StatusCode) {
		return snapshot, awu.Good
	}
}

func managementPathComponent(value string) string {
	return url.PathEscape(value)
}

func findSource(sources []domain.Source, id string) (domain.Source, bool) {
	for _, source := range sources {
		if source.ID == id {
			return source, true
		}
	}
	return domain.Source{}, false
}

func (s *UpstreamServer) purgeMissingManagementIndexLocked() {
	namespace := s.server.NamespaceManager()
	for key, node := range s.management {
		if _, exists := namespace.FindNode(node.NodeID()); !exists {
			delete(s.management, key)
		}
	}
}
