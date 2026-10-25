package opc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
)

func (s *UpstreamServer) addRoots() error {
	s.devicesID = awu.NewNodeIDString(s.businessNS, "Devices")
	s.methodsID = awu.NewNodeIDString(s.businessNS, "Methods")
	s.managerRootID = awu.NewNodeIDString(s.managementNS, "RcdManager")
	s.groupsRootID = awu.NewNodeIDString(s.managementNS, "RcdManager/Groups")
	s.sourcesRootID = awu.NewNodeIDString(s.managementNS, "RcdManager/Sources")
	s.calculationsRootID = awu.NewNodeIDString(s.managementNS, "RcdManager/Calculations")
	nodes := []awserver.Node{
		newObject(s.server, s.devicesID, awu.NewQualifiedName(s.businessNS, "Devices"), "Devices", awu.ObjectIDObjectsFolder),
		newObjectWithType(s.server, s.methodsID, awu.NewQualifiedName(s.businessNS, "Methods"), "Methods", awu.ObjectIDObjectsFolder, awu.ObjectTypeIDBaseObjectType),
		newObject(s.server, s.managerRootID, awu.NewQualifiedName(s.managementNS, "RcdManager"), "RcdManager", awu.ObjectIDObjectsFolder),
		newObject(s.server, s.groupsRootID, awu.NewQualifiedName(s.managementNS, "Groups"), "Groups", s.managerRootID),
		newObject(s.server, s.sourcesRootID, awu.NewQualifiedName(s.managementNS, "Sources"), "Sources", s.managerRootID),
		newObject(s.server, s.calculationsRootID, awu.NewQualifiedName(s.managementNS, "Calculations"), "Calculations", s.managerRootID),
	}
	if err := s.server.NamespaceManager().AddNodes(nodes...); err != nil {
		return err
	}
	return s.addCalculationManagementLocked()
}

func (s *UpstreamServer) addCalculationManagementLocked() error {
	metadata := func() CalculationMetadata {
		if backend, ok := s.backend.(CalculationBackend); ok {
			return backend.CalculationMetadata()
		}
		return CalculationMetadata{RoutingMode: "DefaultSources"}
	}
	definitions := []managementVariable{
		{name: "RoutingMode", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().RoutingMode, awu.Good }},
		{name: "ActiveRevision", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().ActiveRevision, awu.Good }},
		{name: "ConfigHash", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().ConfigHash, awu.Good }},
		{name: "RuleCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().RuleCount, awu.Good }},
		{name: "GoodCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().GoodCount, awu.Good }},
		{name: "BadCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().BadCount, awu.Good }},
		{name: "DefaultRouteRevision", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().DefaultRouteRevision, awu.Good }},
		{name: "LastEvaluationAt", dataType: awu.DataTypeIDDateTime, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			value := metadata().LastEvaluationAt
			if value.IsZero() {
				return time.Time{}, awu.BadWaitingForInitialData
			}
			return value, awu.Good
		}},
		{name: "LastEvaluationDurationMs", dataType: awu.DataTypeIDDouble, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().LastEvaluationDurationMS, awu.Good }},
		{name: "PendingRestart", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return metadata().PendingRestart, awu.Good }},
		{name: "CalculationNodeIds", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankOneDimension, minimumSamplingInterval: 1000, read: func(string) (any, awu.StatusCode) { return metadata().CalculationNodeIDs, awu.Good }},
	}
	for _, definition := range definitions {
		if err := s.addManagementVariableLocked(s.calculationsRootID, "Calculations/"+definition.name, definition); err != nil {
			return err
		}
	}
	return nil
}

func newObject(srv *awserver.Server, id awu.NodeID, browseName awu.QualifiedName, display string, parent awu.NodeID) *awserver.ObjectNode {
	return newObjectWithType(srv, id, browseName, display, parent, awu.ObjectTypeIDFolderType)
}

func newObjectWithType(srv *awserver.Server, id awu.NodeID, browseName awu.QualifiedName, display string, parent, objectType awu.NodeID) *awserver.ObjectNode {
	return awserver.NewObjectNode(
		srv, id, browseName, awu.NewLocalizedText(display, ""), awu.NewLocalizedText("", ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(objectType)),
			awu.NewReference(awu.ReferenceTypeIDOrganizes, true, awu.NewExpandedNodeID(parent)),
		}, 0,
	)
}

func (s *UpstreamServer) addBusinessNodeLocked(spec domain.CatalogNode) (*businessEntry, error) {
	switch spec.Class {
	case domain.NodeClassVariable:
		return s.addBusinessVariableLocked(spec)
	case domain.NodeClassMethod:
		return s.addBusinessMethodLocked(spec)
	default:
		return nil, fmt.Errorf("unsupported node class %q", spec.Class)
	}
}

func (s *UpstreamServer) addBusinessVariableLocked(spec domain.CatalogNode) (*businessEntry, error) {
	if spec.BrowseName == "" {
		spec.BrowseName = spec.Key.Identifier
	}
	id := awu.NewNodeIDString(s.businessNS, spec.Key.Identifier)
	dataType, err := awcullenDataType(spec.DataType)
	if err != nil {
		return nil, err
	}
	access := awu.AccessLevelsCurrentRead
	if spec.Writable {
		access |= awu.AccessLevelsCurrentWrite
	}
	entry := &businessEntry{spec: spec}
	description := ""
	if strings.HasPrefix(spec.Key.Identifier, "DATA.Buildings.") && strings.HasSuffix(spec.Key.Identifier, ".Fire") && spec.DataType == domain.DataTypeUInt32 {
		description = SynchronizationFireDescription
	}
	node := awserver.NewVariableNode(
		s.server, id, awu.NewQualifiedName(s.businessNS, spec.BrowseName),
		awu.NewLocalizedText(spec.BrowseName, ""), awu.NewLocalizedText(description, ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.VariableTypeIDBaseDataVariableType)),
			awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(s.devicesID)),
		},
		awu.NewDataValue(zeroValue(spec.DataType, spec.ValueRank), awu.BadWaitingForInitialData, time.Time{}, 0, time.Now(), 0),
		dataType, spec.ValueRank, nil, access, upstreamMinimumSamplingIntervalMS, false, nil,
	)
	node.SetReadValueHandler(func(session *awserver.Session, _ awu.ReadValueID) awu.DataValue {
		return readBusinessValue(s.backend, sessionID(session), spec, entry.retired.Load(), time.Now())
	})
	if spec.Writable {
		node.SetWriteValueHandler(func(session *awserver.Session, req awu.WriteValue) (awu.DataValue, awu.StatusCode) {
			if IsSynchronizationInput(spec.Key) && req.IndexRange != "" {
				return awu.DataValue{}, awu.BadIndexRangeInvalid
			}
			if IsSynchronizationInput(spec.Key) && uint32(req.Value.StatusCode)&0xC0000000 != 0 {
				return awu.DataValue{}, awu.BadInvalidArgument
			}
			if entry.retired.Load() {
				return awu.NewDataValue(req.Value.Value, awu.BadOutOfService, time.Time{}, 0, time.Now(), 0), awu.BadOutOfService
			}
			ctx, cancel := s.commandContext()
			defer cancel()
			return writeBusinessCommand(ctx, s.backend, sessionID(session), spec.Key, req.Value.Value, time.Now())
		})
	}
	if err := s.server.NamespaceManager().AddNode(node); err != nil {
		return nil, err
	}
	entry.node = node
	return entry, nil
}

func (s *UpstreamServer) addBusinessMethodLocked(spec domain.CatalogNode) (*businessEntry, error) {
	if spec.BrowseName == "" {
		spec.BrowseName = spec.Key.Identifier
	}
	if !validProxyMethod(spec) {
		return nil, fmt.Errorf("method %q does not match HexData String -> Result String", spec.Key.Identifier)
	}
	id := awu.NewNodeIDString(s.businessNS, spec.Key.Identifier)
	entry := &businessEntry{spec: spec}
	node := awserver.NewMethodNode(
		s.server, id, awu.NewQualifiedName(s.businessNS, spec.BrowseName),
		awu.NewLocalizedText(spec.BrowseName, ""), awu.NewLocalizedText("", ""), nil,
		[]awu.Reference{awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(s.methodsID))}, true,
	)
	node.SetCallMethodHandler(func(session *awserver.Session, req awu.CallMethodRequest) awu.CallMethodResult {
		if entry.retired.Load() {
			return awu.CallMethodResult{StatusCode: awu.BadOutOfService}
		}
		if len(req.InputArguments) == 0 {
			return awu.CallMethodResult{StatusCode: awu.BadArgumentsMissing}
		}
		if len(req.InputArguments) > 1 {
			return awu.CallMethodResult{StatusCode: awu.BadTooManyArguments}
		}
		hexData, ok := req.InputArguments[0].(string)
		if !ok {
			return awu.CallMethodResult{StatusCode: awu.BadTypeMismatch, InputArgumentResults: []awu.StatusCode{awu.BadTypeMismatch}}
		}
		ctx, cancel := s.commandContext()
		defer cancel()
		return callBusinessCommand(ctx, s.backend, sessionID(session), spec.Key, hexData)
	})
	inputID := awu.NewNodeIDString(s.businessNS, spec.Key.Identifier+"_Input")
	outputID := awu.NewNodeIDString(s.businessNS, spec.Key.Identifier+"_Output")
	input := methodArgumentNode(s.server, inputID, id, "InputArguments", []awu.Argument{{
		Name: "HexData", DataType: awu.DataTypeIDString, ValueRank: awu.ValueRankScalar,
		Description: awu.NewLocalizedText("HexData", ""),
	}})
	output := methodArgumentNode(s.server, outputID, id, "OutputArguments", []awu.Argument{{
		Name: "Result", DataType: awu.DataTypeIDString, ValueRank: awu.ValueRankScalar,
		Description: awu.NewLocalizedText("Result", ""),
	}})
	if err := s.server.NamespaceManager().AddNodes(node, input, output); err != nil {
		return nil, err
	}
	entry.node = node
	return entry, nil
}

func methodArgumentNode(srv *awserver.Server, id, methodID awu.NodeID, browseName string, value []awu.Argument) *awserver.VariableNode {
	extensions := make([]awu.ExtensionObject, len(value))
	for i := range value {
		extensions[i] = value[i]
	}
	return awserver.NewVariableNode(
		srv, id, awu.NewQualifiedName(0, browseName), awu.NewLocalizedText(browseName, ""), awu.NewLocalizedText("", ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.VariableTypeIDPropertyType)),
			awu.NewReference(awu.ReferenceTypeIDHasProperty, true, awu.NewExpandedNodeID(methodID)),
		},
		awu.NewDataValue(extensions, awu.Good, time.Now(), 0, time.Now(), 0),
		awu.DataTypeIDArgument, awu.ValueRankOneDimension, []uint32{uint32(len(value))},
		awu.AccessLevelsCurrentRead, 0, false, nil,
	)
}

func readBusinessValue(backend UpstreamBackend, sid string, spec domain.CatalogNode, retired bool, now time.Time) awu.DataValue {
	if retired {
		return awu.NewDataValue(zeroValue(spec.DataType, spec.ValueRank), awu.BadOutOfService, time.Time{}, 0, now, 0)
	}
	sample := backend.ReadSample(sid, spec.Key)
	return sampleToDataValue(sample, spec.DataType, spec.ValueRank, now)
}

func sampleToDataValue(sample domain.Sample, fallback domain.DataType, valueRank int32, now time.Time) awu.DataValue {
	value := sample.Value
	if value == nil {
		value = zeroValue(fallback, valueRank)
	}
	return awu.NewDataValue(value, awu.StatusCode(sample.StatusCode), sample.SourceTimestamp, 0, now, 0)
}

func writeBusinessCommand(ctx context.Context, backend UpstreamBackend, sid string, key domain.NodeKey, value any, now time.Time) (awu.DataValue, awu.StatusCode) {
	status, err := backend.WriteCommand(ctx, sid, key, value)
	uaStatus := statusFromBackend(status, err)
	return awu.NewDataValue(value, uaStatus, time.Time{}, 0, now, 0), uaStatus
}

func callBusinessCommand(ctx context.Context, backend UpstreamBackend, sid string, key domain.NodeKey, hexData string) awu.CallMethodResult {
	result, status, err := backend.CallCommand(ctx, sid, key, hexData)
	uaStatus := statusFromBackend(status, err)
	if uaStatus != awu.Good {
		return awu.CallMethodResult{StatusCode: uaStatus}
	}
	return awu.CallMethodResult{
		StatusCode: awu.Good, InputArgumentResults: []awu.StatusCode{awu.Good},
		OutputArguments: []awu.Variant{result},
	}
}

func sessionID(session *awserver.Session) string {
	if session == nil || session.SessionId() == nil {
		return ""
	}
	return fmt.Sprint(session.SessionId())
}

func statusFromBackend(status domain.StatusCode, err error) awu.StatusCode {
	if err == nil {
		return awu.StatusCode(status)
	}
	switch {
	case errors.Is(err, domain.ErrGroupPending), errors.Is(err, domain.ErrCommandPending):
		return awu.BadWaitingForResponse
	case errors.Is(err, domain.ErrRevisionConflict):
		return awu.BadInvalidState
	case errors.Is(err, domain.ErrSessionClosed):
		return awu.BadSessionIDInvalid
	case errors.Is(err, domain.ErrNodeRetired):
		return awu.BadOutOfService
	case errors.Is(err, domain.ErrShuttingDown):
		return awu.BadShutdown
	case errors.Is(err, domain.ErrSourceNotSelectable), errors.Is(err, domain.ErrSourceNotInGroup), errors.Is(err, domain.ErrNodeNotConfigured):
		return awu.BadConfigurationError
	case errors.Is(err, domain.ErrNotFound):
		return awu.BadNotFound
	case errors.Is(err, context.DeadlineExceeded):
		return awu.BadTimeout
	default:
		if status != domain.StatusGood {
			return awu.StatusCode(status)
		}
		return awu.BadUnexpectedError
	}
}

func awcullenDataType(dataType domain.DataType) (awu.NodeID, error) {
	switch dataType {
	case domain.DataTypeBoolean:
		return awu.DataTypeIDBoolean, nil
	case domain.DataTypeUInt32:
		return awu.DataTypeIDUInt32, nil
	case domain.DataTypeFloat:
		return awu.DataTypeIDFloat, nil
	case domain.DataTypeDouble:
		return awu.DataTypeIDDouble, nil
	case domain.DataTypeString:
		return awu.DataTypeIDString, nil
	default:
		return nil, fmt.Errorf("unsupported data type %q", dataType)
	}
}

func zeroValue(dataType domain.DataType, valueRank int32) any {
	if valueRank >= awu.ValueRankOneOrMoreDimensions {
		switch dataType {
		case domain.DataTypeBoolean:
			return []bool{}
		case domain.DataTypeUInt32:
			return []uint32{}
		case domain.DataTypeFloat:
			return []float32{}
		case domain.DataTypeDouble:
			return []float64{}
		case domain.DataTypeString:
			return []string{}
		default:
			return []any{}
		}
	}
	switch dataType {
	case domain.DataTypeBoolean:
		return false
	case domain.DataTypeUInt32:
		return uint32(0)
	case domain.DataTypeFloat:
		return float32(0)
	case domain.DataTypeDouble:
		return float64(0)
	case domain.DataTypeString:
		return ""
	default:
		return nil
	}
}

func (s *UpstreamServer) reconcileManagementLocked(catalog domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
	sourcesByGroup := make(map[string][]string)
	membersByGroup := make(map[string][]domain.Source)
	desiredGroups := make(map[string]struct{}, len(groups))
	desiredSources := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		for _, group := range groups {
			if source.HasGroup(group.ID) {
				sourcesByGroup[group.ID] = append(sourcesByGroup[group.ID], source.ID)
				membersByGroup[group.ID] = append(membersByGroup[group.ID], source.Clone())
			}
		}
		desiredSources[source.ID] = struct{}{}
	}
	for groupID := range sourcesByGroup {
		sort.Strings(sourcesByGroup[groupID])
		sort.Slice(membersByGroup[groupID], func(i, j int) bool { return membersByGroup[groupID][i].ID < membersByGroup[groupID][j].ID })
	}
	for _, group := range groups {
		desiredGroups[group.ID] = struct{}{}
		catalogRevision := s.beginGroupCatalogUpdateLocked(group.ID)
		if err := s.addGroupManagementLocked(group, sourcesByGroup[group.ID], groupPointNodeIDs(group.ID, catalog), catalogRevision); err != nil {
			return err
		}
		if err := s.reconcileGroupMemberTreeLocked(group, membersByGroup[group.ID], catalog); err != nil {
			return err
		}
		s.finishGroupCatalogUpdateLocked(catalogRevision)
	}
	for _, source := range sources {
		if err := s.addSourceManagementLocked(source, catalog.SourceTopologies[source.ID]); err != nil {
			return err
		}
	}
	if err := s.removeStaleManagementLocked(s.managementGroups, desiredGroups); err != nil {
		return fmt.Errorf("remove stale group management nodes: %w", err)
	}
	if err := s.removeStaleManagementLocked(s.managementSources, desiredSources); err != nil {
		return fmt.Errorf("remove stale source management nodes: %w", err)
	}
	for groupID := range s.memberTreeHashes {
		if _, keep := desiredGroups[groupID]; !keep {
			delete(s.memberTreeHashes, groupID)
			delete(s.memberTreeNodeIDs, groupID)
			delete(s.memberTreeRouteIDs, groupID)
		}
	}
	for groupID := range s.groupPointSnapshots {
		if _, keep := desiredGroups[groupID]; !keep {
			delete(s.groupPointSnapshots, groupID)
		}
	}
	for groupID := range s.groupCatalogRevisions {
		if _, keep := desiredGroups[groupID]; !keep {
			delete(s.groupCatalogRevisions, groupID)
		}
	}
	for key := range s.memberCoverageSnapshots {
		if _, keep := desiredGroups[key.groupID]; !keep {
			delete(s.memberCoverageSnapshots, key)
		}
	}
	for sourceID := range s.sourceVersionTreeHashes {
		if _, keep := desiredSources[sourceID]; !keep {
			delete(s.sourceVersionTreeHashes, sourceID)
			delete(s.sourceVersionTreeRoots, sourceID)
		}
	}
	return nil
}

func (s *UpstreamServer) removeStaleManagementLocked(objects map[string]awserver.Node, desired map[string]struct{}) error {
	removed := false
	for id, object := range objects {
		if _, keep := desired[id]; keep {
			continue
		}
		if err := s.server.NamespaceManager().DeleteNode(object, true); err != nil {
			return err
		}
		delete(objects, id)
		removed = true
	}
	if !removed {
		return nil
	}

	// DeleteNode removes the object and all HasComponent descendants. Keep the
	// adapter's lookup index in lock-step so that a later CRUD re-add can create
	// the same NodeIDs again.
	namespace := s.server.NamespaceManager()
	for key, node := range s.management {
		if _, exists := namespace.FindNode(node.NodeID()); !exists {
			delete(s.management, key)
		}
	}
	return nil
}

func (s *UpstreamServer) addGroupManagementLocked(group domain.Group, members, pointNodeIDs []string, catalogRevision *atomic.Uint64) error {
	pointSnapshot := s.updateGroupPointSnapshotLocked(group.ID, pointNodeIDs)
	objectID := awu.NewNodeIDString(s.managementNS, "Groups/"+group.ID)
	object, exists := s.management[objectID.String()]
	if !exists {
		object = newObject(s.server, objectID, awu.NewQualifiedName(s.managementNS, group.ID), group.ID, s.groupsRootID)
		if err := s.server.NamespaceManager().AddNode(object); err != nil {
			return err
		}
		s.management[objectID.String()] = object
	}
	s.managementGroups[group.ID] = object
	definitions := []managementVariable{
		{name: "DefaultSourceId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, writable: true,
			read: func(_ string) (any, awu.StatusCode) {
				current, ok := findGroup(s.backend.Groups(), group.ID)
				if !ok {
					return "", awu.BadNotFound
				}
				return current.DefaultSourceID, awu.Good
			},
			write: func(_ string, value any) awu.StatusCode {
				selected, ok := value.(string)
				if !ok {
					return awu.BadTypeMismatch
				}
				current, ok := findGroup(s.backend.Groups(), group.ID)
				if !ok {
					return awu.BadNotFound
				}
				_, err := s.backend.SetDefaultSource(group.ID, selected, current.Revision)
				return statusFromBackend(domain.StatusGood, err)
			}},
		{name: "SelectedSourceId", description: "Current Session's selected source. Legacy String write has no caller-supplied revision; use SelectSource for compare-and-set.", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, writable: true,
			read: func(sid string) (any, awu.StatusCode) {
				session, err := s.backend.Session(sid)
				if err != nil {
					return "", statusFromBackend(domain.StatusGood, err)
				}
				route, ok := session.Routes[group.ID]
				if !ok {
					return "", awu.BadNotFound
				}
				return route.SourceID, awu.Good
			},
			write: func(sid string, value any) awu.StatusCode {
				selected, ok := value.(string)
				if !ok {
					return awu.BadTypeMismatch
				}
				session, err := s.backend.Session(sid)
				if err != nil {
					return statusFromBackend(domain.StatusGood, err)
				}
				route, ok := session.Routes[group.ID]
				if !ok {
					return awu.BadNotFound
				}
				_, err = s.backend.SelectSource(sid, group.ID, selected, route.Revision)
				return statusFromBackend(domain.StatusGood, err)
			}},
		{name: "SelectionRevision", description: "Current Session/group route revision. Pass as ExpectedRevision to SelectSource; not the lower TransmitStatus revision.", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar,
			read: func(sid string) (any, awu.StatusCode) {
				session, err := s.backend.Session(sid)
				if err != nil {
					return uint64(0), statusFromBackend(domain.StatusGood, err)
				}
				route, ok := session.Routes[group.ID]
				if !ok {
					return uint64(0), awu.BadNotFound
				}
				return route.Revision, awu.Good
			}},
		{name: "MemberSourceIds", description: "Configured group member IDs, including offline, disabled or unselectable members. Inspect Members/<source>/Selectable, ConnectionState and CompleteCoverage.", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankOneDimension,
			read: func(_ string) (any, awu.StatusCode) {
				current := make([]string, 0, len(members))
				for _, source := range s.backend.Sources() {
					if source.HasGroup(group.ID) {
						current = append(current, source.ID)
					}
				}
				sort.Strings(current)
				return current, awu.Good
			}},
		{name: "CatalogRevision", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar,
			read: func(_ string) (any, awu.StatusCode) { return catalogRevision.Load(), awu.Good }},
		{name: "PointNodeIds", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankOneDimension,
			minimumSamplingInterval: 1000,
			read: func(_ string) (any, awu.StatusCode) {
				return pointSnapshot.current.Load().identifiers, awu.Good
			}},
		{name: "PointNodeCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
			read: func(_ string) (any, awu.StatusCode) {
				return uint32(len(pointSnapshot.current.Load().identifiers)), awu.Good
			}},
		{name: "PointSetFingerprint", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar,
			read: func(_ string) (any, awu.StatusCode) { return pointSnapshot.current.Load().fingerprint, awu.Good }},
		{name: "PointSetRevision", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar,
			read: func(_ string) (any, awu.StatusCode) { return pointSnapshot.current.Load().revision, awu.Good }},
		{name: "PendingCommandCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
			read: func(_ string) (any, awu.StatusCode) { return uint32(s.backend.PendingCount(group.ID)), awu.Good }},
	}
	if backend, ok := s.backend.(interface{ CommandQueueCounts(string) (uint32, uint32) }); ok {
		definitions = append(definitions,
			managementVariable{name: "QueuedCommandCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
				read: func(_ string) (any, awu.StatusCode) {
					queued, _ := backend.CommandQueueCounts(group.ID)
					return queued, awu.Good
				}},
			managementVariable{name: "ExecutingCommandCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
				read: func(_ string) (any, awu.StatusCode) {
					_, executing := backend.CommandQueueCounts(group.ID)
					return executing, awu.Good
				}},
			managementVariable{name: "CommandQueueCapacityPerPort", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
				read: func(_ string) (any, awu.StatusCode) { return uint32(domain.CommandQueueCapacity), awu.Good }},
		)
	}
	definitions = append(definitions, s.redundancyVariables(group.ID)...)
	for _, definition := range definitions {
		if err := s.addManagementVariableLocked(objectID, "Groups/"+group.ID+"/"+definition.name, definition); err != nil {
			return err
		}
	}
	if err := s.addGroupSelectionMethodLocked(group.ID, objectID); err != nil {
		return err
	}
	return s.addGroupCommandNodesLocked(group.ID, objectID)
}

func (s *UpstreamServer) beginGroupCatalogUpdateLocked(groupID string) *atomic.Uint64 {
	revision := s.groupCatalogRevisions[groupID]
	if revision == nil {
		revision = &atomic.Uint64{}
		s.groupCatalogRevisions[groupID] = revision
	}
	// An odd value is a seqlock write marker. If a prior reconciliation failed,
	// leave its odd marker in place until this attempt completes successfully.
	if revision.Load()%2 == 0 {
		revision.Add(1)
	}
	return revision
}

func (s *UpstreamServer) finishGroupCatalogUpdateLocked(revision *atomic.Uint64) {
	if revision != nil && revision.Load()%2 == 1 {
		revision.Add(1)
	}
}

func (s *UpstreamServer) updateGroupPointSnapshotLocked(groupID string, identifiers []string) *groupPointSnapshotStore {
	fingerprint := pointListFingerprint(identifiers)
	store := s.groupPointSnapshots[groupID]
	if store == nil {
		store = &groupPointSnapshotStore{}
		s.groupPointSnapshots[groupID] = store
	}
	current := store.current.Load()
	if current != nil && current.fingerprint == fingerprint {
		return store
	}
	revision := uint64(1)
	if current != nil {
		revision = current.revision + 1
	}
	store.current.Store(&groupPointSnapshot{
		revision: revision, fingerprint: fingerprint, identifiers: append([]string(nil), identifiers...),
	})
	return store
}

func (s *UpstreamServer) addSourceManagementLocked(source domain.Source, topology *domain.SourceTopology) error {
	objectID := awu.NewNodeIDString(s.managementNS, "Sources/"+source.ID)
	object, exists := s.management[objectID.String()]
	if !exists {
		object = newObject(s.server, objectID, awu.NewQualifiedName(s.managementNS, source.ID), source.ID, s.sourcesRootID)
		if err := s.server.NamespaceManager().AddNode(object); err != nil {
			return err
		}
		s.management[objectID.String()] = object
	}
	s.managementSources[source.ID] = object
	status := func() LowerStatus { value, _ := s.backend.LowerStatus(source.ID); return value }
	pointStats := func() PointStatistics { return s.backend.SourcePointStatistics(source.ID) }
	definitions := []managementVariable{
		{name: "RcdSoftwareVersion", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			if snapshot, ok := sourceStationVersions(s.backend, source.ID); ok {
				return snapshot.SoftwareVersion, awu.Good
			}
			return "", sourceStationMissingStatus(s.backend, source.ID)
		}},
		{name: "RcdBuildCommit", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			if snapshot, ok := sourceStationVersions(s.backend, source.ID); ok {
				return snapshot.BuildCommit, awu.Good
			}
			return "", sourceStationMissingStatus(s.backend, source.ID)
		}},
		{name: "StationVersionRevision", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			if snapshot, ok := sourceStationVersions(s.backend, source.ID); ok {
				return snapshot.StationRevision, awu.Good
			}
			return "", sourceStationMissingStatus(s.backend, source.ID)
		}},
		{name: "ConnectionState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return string(status().Connection), awu.Good }},
		{name: "SessionState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return string(status().Session), awu.Good }},
		{name: "SubscriptionState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return string(status().Subscription), awu.Good }},
		{name: "CatalogState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			for _, current := range s.backend.Sources() {
				if current.ID == source.ID && current.CatalogState != "" {
					return string(current.CatalogState), awu.Good
				}
			}
			return string(status().Catalog), awu.Good
		}},
		{name: "LastDataAt", dataType: awu.DataTypeIDDateTime, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			value := status().LastDataAt
			if value.IsZero() {
				return time.Time{}, awu.BadWaitingForInitialData
			}
			return value, awu.Good
		}},
		{name: "LastProbeAt", dataType: awu.DataTypeIDDateTime, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) {
			value := status().LastProbeAt
			if value.IsZero() {
				return time.Time{}, awu.BadWaitingForInitialData
			}
			return value, awu.Good
		}},
		{name: "LastError", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return status().LastError, awu.Good }},
		{name: "ReconnectCount", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return status().ReconnectCount, awu.Good }},
		{name: "PointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return uint32(status().PointCount), awu.Good }},
		{name: "MonitoredPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return uint32(status().PointCount), awu.Good }},
		{name: "SubscriptionCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return uint32(status().SubscriptionCount), awu.Good }},
		// Point statistics scan every node of the source, so sampling is capped like other computed diagnostics.
		{name: "TotalPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, minimumSamplingInterval: 1000, read: func(string) (any, awu.StatusCode) { return uint32(pointStats().Total), awu.Good }},
		{name: "GoodPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, minimumSamplingInterval: 1000, read: func(string) (any, awu.StatusCode) { return uint32(pointStats().Good), awu.Good }},
		{name: "BadPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, minimumSamplingInterval: 1000, read: func(string) (any, awu.StatusCode) { return uint32(pointStats().Bad), awu.Good }},
	}
	for _, definition := range definitions {
		if err := s.addManagementVariableLocked(objectID, "Sources/"+source.ID+"/"+definition.name, definition); err != nil {
			return err
		}
	}
	return s.reconcileSourceStationVersionTreeLocked(source, topology)
}

func sourceStationVersions(backend UpstreamBackend, sourceID string) (domain.SourceStationVersions, bool) {
	if known, supported := sourceStationVersionSupport(backend, sourceID); known && !supported {
		return domain.SourceStationVersions{}, false
	}
	provider, ok := backend.(StationVersionBackend)
	if !ok {
		return domain.SourceStationVersions{}, false
	}
	return provider.StationVersions(sourceID)
}

func sourceStationVersionSupport(backend UpstreamBackend, sourceID string) (known, supported bool) {
	provider, ok := backend.(StationVersionCapabilityBackend)
	if !ok {
		return false, false
	}
	return provider.StationVersionSupport(sourceID)
}

func sourceStationOnline(backend UpstreamBackend, sourceID string) bool {
	status, ok := backend.LowerStatus(sourceID)
	return ok && status.Connection == ConnectionConnected && status.Session == SessionActive
}

func sourceStationMissingStatus(backend UpstreamBackend, sourceID string) awu.StatusCode {
	if known, supported := sourceStationVersionSupport(backend, sourceID); known && !supported {
		return awu.BadNotSupported
	}
	if sourceStationOnline(backend, sourceID) {
		return awu.BadWaitingForInitialData
	}
	return awu.BadNoCommunication
}

func (s *UpstreamServer) reconcileSourceStationVersionTreeLocked(source domain.Source, topology *domain.SourceTopology) error {
	fingerprint := domain.SourceTopologyFingerprint(topology)
	if current, exists := s.sourceVersionTreeHashes[source.ID]; exists && current == fingerprint {
		return nil
	}
	if root := s.sourceVersionTreeRoots[source.ID]; root != nil {
		if err := s.server.NamespaceManager().DeleteNode(root, true); err != nil {
			return fmt.Errorf("remove source station version tree: %w", err)
		}
		delete(s.sourceVersionTreeRoots, source.ID)
		for key, node := range s.management {
			if _, exists := s.server.NamespaceManager().FindNode(node.NodeID()); !exists {
				delete(s.management, key)
			}
		}
	}
	s.sourceVersionTreeHashes[source.ID] = fingerprint
	if topology == nil {
		return nil
	}
	rootID := awu.NewNodeIDString(s.managementNS, "Sources/"+source.ID+"/Ports")
	root := newObject(s.server, rootID, awu.NewQualifiedName(s.managementNS, "Ports"), "Ports", awu.NewNodeIDString(s.managementNS, "Sources/"+source.ID))
	if err := s.server.NamespaceManager().AddNode(root); err != nil {
		return err
	}
	s.management[rootID.String()] = root
	s.sourceVersionTreeRoots[source.ID] = root
	for _, topologyPort := range topology.Ports {
		port := topologyPort
		portToken := url.PathEscape(port.PortID)
		portPath := "Sources/" + source.ID + "/Ports/" + portToken
		portID := awu.NewNodeIDString(s.managementNS, portPath)
		portObject := newObject(s.server, portID, awu.NewQualifiedName(s.managementNS, port.PortName), port.PortName, rootID)
		if err := s.server.NamespaceManager().AddNode(portObject); err != nil {
			return err
		}
		s.management[portID.String()] = portObject
		for _, definition := range []managementVariable{
			{name: "PortId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return port.PortID, awu.Good }},
			{name: "PortName", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return port.PortName, awu.Good }},
		} {
			if err := s.addManagementVariableLocked(portID, portPath+"/"+definition.name, definition); err != nil {
				return err
			}
		}
		devicesPath := portPath + "/Devices"
		devicesID := awu.NewNodeIDString(s.managementNS, devicesPath)
		devicesObject := newObject(s.server, devicesID, awu.NewQualifiedName(s.managementNS, "Devices"), "Devices", portID)
		if err := s.server.NamespaceManager().AddNode(devicesObject); err != nil {
			return err
		}
		s.management[devicesID.String()] = devicesObject
		for _, topologyDevice := range port.Devices {
			device := topologyDevice
			devicePath := devicesPath + "/" + device.DeviceID
			deviceID := awu.NewNodeIDString(s.managementNS, devicePath)
			deviceObject := newObject(s.server, deviceID, awu.NewQualifiedName(s.managementNS, device.DeviceID), device.Name, devicesID)
			if err := s.server.NamespaceManager().AddNode(deviceObject); err != nil {
				return err
			}
			s.management[deviceID.String()] = deviceObject
			if err := s.addSourceStationDeviceVariablesLocked(source.ID, port.PortID, device, deviceID, devicePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *UpstreamServer) addSourceStationDeviceVariablesLocked(sourceID, portID string, topologyDevice domain.TopologyDevice, parent awu.NodeID, path string) error {
	definitions := sourceStationDeviceVariables(s.backend, sourceID, portID, topologyDevice)
	definitions = append(definitions, stationCommunicationVariables(s.backend, sourceID, portID, topologyDevice.DeviceID)...)
	for _, definition := range definitions {
		if err := s.addManagementVariableLocked(parent, path+"/"+definition.name, definition); err != nil {
			return err
		}
	}
	return nil
}

func sourceStationDeviceVariables(backend UpstreamBackend, sourceID, portID string, topologyDevice domain.TopologyDevice) []managementVariable {
	lookup := func() (domain.StationVersion, bool) {
		snapshot, ok := sourceStationVersions(backend, sourceID)
		if !ok {
			return domain.StationVersion{}, false
		}
		_, device, ok := snapshot.Device(portID, topologyDevice.DeviceID)
		return device, ok
	}
	metadata := func(value func(domain.StationVersion) any, zero any) func(string) (any, awu.StatusCode) {
		return func(string) (any, awu.StatusCode) {
			device, ok := lookup()
			if !ok {
				return zero, sourceStationMissingStatus(backend, sourceID)
			}
			return value(device), awu.Good
		}
	}
	version := func(value func(domain.StationVersion) any, zero any) func(string) (any, awu.StatusCode) {
		return func(string) (any, awu.StatusCode) {
			device, ok := lookup()
			if !ok {
				return zero, sourceStationMissingStatus(backend, sourceID)
			}
			return value(device), stationVersionStatus(backend, sourceID, device)
		}
	}
	return []managementVariable{
		{name: "PortId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return portID, awu.Good }},
		{name: "DeviceId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return topologyDevice.DeviceID, awu.Good }},
		{name: "DeviceName", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return topologyDevice.Name, awu.Good }},
		{name: "ConfiguredModuleType", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.ConfiguredModuleType }, "")},
		{name: "DetectedModuleType", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.DetectedModuleType }, "")},
		{name: "TypeMismatch", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.TypeMismatch }, false)},
		{name: "CommunicationModuleVersion", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: version(func(v domain.StationVersion) any { return v.CommunicationModuleVersion }, "")},
		{name: "CommunicationModuleVersionRaw", dataType: awu.DataTypeIDUInt16, valueRank: awu.ValueRankScalar, read: version(func(v domain.StationVersion) any { return stationRawValue(v.CommunicationModuleVersionRaw) }, uint16(0))},
		{name: "PLCVersion", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: version(func(v domain.StationVersion) any { return v.PLCVersion }, "")},
		{name: "PLCVersionRaw", dataType: awu.DataTypeIDUInt16, valueRank: awu.ValueRankScalar, read: version(func(v domain.StationVersion) any { return stationRawValue(v.PLCVersionRaw) }, uint16(0))},
		{name: "VersionPayload", dataType: awu.DataTypeIDByteString, valueRank: awu.ValueRankScalar, read: version(func(v domain.StationVersion) any {
			payload, _ := hex.DecodeString(v.VersionPayload)
			return awu.ByteString(string(payload))
		}, awu.ByteString(""))},
		{name: "VersionState", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.VersionState }, "")},
		{name: "LastVersionAttemptAt", dataType: awu.DataTypeIDDateTime, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.LastVersionAttemptAt }, time.Time{})},
		{name: "LastVersionSuccessAt", dataType: awu.DataTypeIDDateTime, valueRank: awu.ValueRankScalar, read: version(func(v domain.StationVersion) any { return v.LastVersionSuccessAt }, time.Time{})},
		{name: "LastVersionError", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.LastVersionError }, "")},
		{name: "ConsecutiveFailures", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.ConsecutiveFailures }, uint32(0))},
		{name: "Stale", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, read: metadata(func(v domain.StationVersion) any { return v.Stale || !sourceStationOnline(backend, sourceID) }, true)},
	}
}

func stationVersionStatus(backend UpstreamBackend, sourceID string, device domain.StationVersion) awu.StatusCode {
	if known, supported := sourceStationVersionSupport(backend, sourceID); known && !supported {
		return awu.BadNotSupported
	}
	if device.ConfiguredModuleType == domain.ModuleTypeLegacyRussian {
		return awu.BadNotSupported
	}
	if device.TypeMismatch || (device.ConfiguredModuleType == domain.ModuleTypeDomestic && device.DetectedModuleType == domain.ModuleTypeLegacyRussian) {
		return awu.BadConfigurationError
	}
	hasValue := device.CommunicationModuleVersionRaw != nil || device.PLCVersionRaw != nil || device.VersionPayload != "" || !device.LastVersionSuccessAt.IsZero()
	if !sourceStationOnline(backend, sourceID) {
		if hasValue {
			return awu.UncertainLastUsableValue
		}
		return awu.BadNoCommunication
	}
	state := strings.ToLower(device.VersionState)
	if (state == "success" || state == "ready") && device.CommunicationOK && !device.Stale && device.ConsecutiveFailures == 0 {
		return awu.Good
	}
	if hasValue {
		return awu.UncertainLastUsableValue
	}
	if !device.CommunicationOK || device.Stale || device.ConsecutiveFailures > 0 || state == "failed" || state == "timeout" || state == "error" {
		return awu.BadNoCommunication
	}
	return awu.BadWaitingForInitialData
}

func stationRawValue(value *uint16) uint16 {
	if value == nil {
		return 0
	}
	return *value
}

type managementVariable struct {
	name                    string
	description             string
	dataType                awu.NodeID
	valueRank               int32
	minimumSamplingInterval float64
	writable                bool
	read                    func(sessionID string) (any, awu.StatusCode)
	write                   func(sessionID string, value any) awu.StatusCode
}

func (s *UpstreamServer) addManagementVariableLocked(parent awu.NodeID, id string, definition managementVariable) error {
	nodeID := awu.NewNodeIDString(s.managementNS, id)
	access := awu.AccessLevelsCurrentRead
	if definition.writable {
		access |= awu.AccessLevelsCurrentWrite
	}
	minimumSamplingInterval := definition.minimumSamplingInterval
	if minimumSamplingInterval <= 0 {
		minimumSamplingInterval = upstreamMinimumSamplingIntervalMS
	}
	if existing, exists := s.server.NamespaceManager().FindNode(nodeID); exists {
		node, ok := existing.(*awserver.VariableNode)
		if !ok {
			return fmt.Errorf("management node %q has class %T, want Variable", id, existing)
		}
		if node.DataType() != definition.dataType || node.ValueRank() != definition.valueRank || node.AccessLevel() != access || node.MinimumSamplingInterval() != minimumSamplingInterval {
			return fmt.Errorf("management variable %q changed shape", id)
		}
		configureManagementVariable(node, definition)
		s.management[nodeID.String()] = node
		return nil
	}
	node := awserver.NewVariableNode(
		s.server, nodeID, awu.NewQualifiedName(s.managementNS, definition.name), awu.NewLocalizedText(definition.name, ""), awu.NewLocalizedText(definition.description, ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.VariableTypeIDBaseDataVariableType)),
			awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(parent)),
		}, awu.NewDataValue(nil, awu.BadWaitingForInitialData, time.Time{}, 0, time.Now(), 0),
		definition.dataType, definition.valueRank, nil, access, minimumSamplingInterval, false, nil,
	)
	configureManagementVariable(node, definition)
	if err := s.server.NamespaceManager().AddNode(node); err != nil {
		return err
	}
	s.management[nodeID.String()] = node
	return nil
}

func configureManagementVariable(node *awserver.VariableNode, definition managementVariable) {
	node.SetReadValueHandler(func(session *awserver.Session, _ awu.ReadValueID) awu.DataValue {
		value, status := definition.read(sessionID(session))
		return awu.NewDataValue(value, status, time.Time{}, 0, time.Now(), 0)
	})
	if definition.write == nil {
		node.SetWriteValueHandler(nil)
		return
	}
	node.SetWriteValueHandler(func(session *awserver.Session, req awu.WriteValue) (awu.DataValue, awu.StatusCode) {
		status := definition.write(sessionID(session), req.Value.Value)
		return awu.NewDataValue(req.Value.Value, status, time.Time{}, 0, time.Now(), 0), status
	})
}

func findGroup(groups []domain.Group, id string) (domain.Group, bool) {
	for _, group := range groups {
		if group.ID == id {
			return group, true
		}
	}
	return domain.Group{}, false
}

func portFromCommandIdentifier(identifier string) string {
	return strings.TrimSuffix(identifier, "_SendCommand")
}

func validProxyMethod(spec domain.CatalogNode) bool {
	if !strings.HasSuffix(spec.Key.Identifier, "_SendCommand") || len(spec.InputArguments) != 1 || len(spec.OutputArguments) != 1 {
		return false
	}
	in, out := spec.InputArguments[0], spec.OutputArguments[0]
	return in.Name == "HexData" && in.DataType == domain.DataTypeString && in.ValueRank == awu.ValueRankScalar &&
		out.Name == "Result" && out.DataType == domain.DataTypeString && out.ValueRank == awu.ValueRankScalar
}
