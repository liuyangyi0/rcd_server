package opc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	nodes := []awserver.Node{
		newObject(s.server, s.devicesID, awu.NewQualifiedName(s.businessNS, "Devices"), "Devices", awu.ObjectIDObjectsFolder),
		newObjectWithType(s.server, s.methodsID, awu.NewQualifiedName(s.businessNS, "Methods"), "Methods", awu.ObjectIDObjectsFolder, awu.ObjectTypeIDBaseObjectType),
		newObject(s.server, s.managerRootID, awu.NewQualifiedName(s.managementNS, "RcdManager"), "RcdManager", awu.ObjectIDObjectsFolder),
		newObject(s.server, s.groupsRootID, awu.NewQualifiedName(s.managementNS, "Groups"), "Groups", s.managerRootID),
		newObject(s.server, s.sourcesRootID, awu.NewQualifiedName(s.managementNS, "Sources"), "Sources", s.managerRootID),
	}
	return s.server.NamespaceManager().AddNodes(nodes...)
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
	node := awserver.NewVariableNode(
		s.server, id, awu.NewQualifiedName(s.businessNS, spec.BrowseName),
		awu.NewLocalizedText(spec.BrowseName, ""), awu.NewLocalizedText("", ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.VariableTypeIDBaseDataVariableType)),
			awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(s.devicesID)),
		},
		awu.NewDataValue(zeroValue(spec.DataType, spec.ValueRank), awu.BadWaitingForInitialData, time.Time{}, 0, time.Now(), 0),
		dataType, spec.ValueRank, nil, access, 125, false, nil,
	)
	node.SetReadValueHandler(func(session *awserver.Session, _ awu.ReadValueID) awu.DataValue {
		return readBusinessValue(s.backend, sessionID(session), spec, entry.retired.Load(), time.Now())
	})
	if spec.Writable {
		node.SetWriteValueHandler(func(session *awserver.Session, req awu.WriteValue) (awu.DataValue, awu.StatusCode) {
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

func (s *UpstreamServer) reconcileManagementLocked(groups []domain.Group, sources []domain.Source) error {
	sourcesByGroup := make(map[string][]string)
	desiredGroups := make(map[string]struct{}, len(groups))
	desiredSources := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		for _, group := range groups {
			if source.HasGroup(group.ID) {
				sourcesByGroup[group.ID] = append(sourcesByGroup[group.ID], source.ID)
			}
		}
		desiredSources[source.ID] = struct{}{}
	}
	for groupID := range sourcesByGroup {
		sort.Strings(sourcesByGroup[groupID])
	}
	for _, group := range groups {
		desiredGroups[group.ID] = struct{}{}
		if err := s.addGroupManagementLocked(group, sourcesByGroup[group.ID]); err != nil {
			return err
		}
	}
	for _, source := range sources {
		if err := s.addSourceManagementLocked(source); err != nil {
			return err
		}
	}
	if err := s.removeStaleManagementLocked(s.managementGroups, desiredGroups); err != nil {
		return fmt.Errorf("remove stale group management nodes: %w", err)
	}
	if err := s.removeStaleManagementLocked(s.managementSources, desiredSources); err != nil {
		return fmt.Errorf("remove stale source management nodes: %w", err)
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

func (s *UpstreamServer) addGroupManagementLocked(group domain.Group, members []string) error {
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
		{name: "SelectedSourceId", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar, writable: true,
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
		{name: "SelectionRevision", dataType: awu.DataTypeIDUInt64, valueRank: awu.ValueRankScalar,
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
		{name: "MemberSourceIds", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankOneDimension,
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
		{name: "PendingCommandCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
			read: func(_ string) (any, awu.StatusCode) { return uint32(s.backend.PendingCount(group.ID)), awu.Good }},
	}
	for _, definition := range definitions {
		if err := s.addManagementVariableLocked(objectID, "Groups/"+group.ID+"/"+definition.name, definition); err != nil {
			return err
		}
	}
	return s.addGroupCommandNodesLocked(group.ID, objectID)
}

func (s *UpstreamServer) addSourceManagementLocked(source domain.Source) error {
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
		{name: "TotalPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return uint32(pointStats().Total), awu.Good }},
		{name: "GoodPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return uint32(pointStats().Good), awu.Good }},
		{name: "BadPointCount", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, read: func(string) (any, awu.StatusCode) { return uint32(pointStats().Bad), awu.Good }},
	}
	for _, definition := range definitions {
		if err := s.addManagementVariableLocked(objectID, "Sources/"+source.ID+"/"+definition.name, definition); err != nil {
			return err
		}
	}
	return nil
}

type managementVariable struct {
	name      string
	dataType  awu.NodeID
	valueRank int32
	writable  bool
	read      func(sessionID string) (any, awu.StatusCode)
	write     func(sessionID string, value any) awu.StatusCode
}

func (s *UpstreamServer) addManagementVariableLocked(parent awu.NodeID, id string, definition managementVariable) error {
	nodeID := awu.NewNodeIDString(s.managementNS, id)
	if _, exists := s.management[nodeID.String()]; exists {
		return nil
	}
	access := awu.AccessLevelsCurrentRead
	if definition.writable {
		access |= awu.AccessLevelsCurrentWrite
	}
	node := awserver.NewVariableNode(
		s.server, nodeID, awu.NewQualifiedName(s.managementNS, definition.name), awu.NewLocalizedText(definition.name, ""), awu.NewLocalizedText("", ""), nil,
		[]awu.Reference{
			awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.VariableTypeIDBaseDataVariableType)),
			awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(parent)),
		}, awu.NewDataValue(nil, awu.BadWaitingForInitialData, time.Time{}, 0, time.Now(), 0),
		definition.dataType, definition.valueRank, nil, access, 125, false, nil,
	)
	node.SetReadValueHandler(func(session *awserver.Session, _ awu.ReadValueID) awu.DataValue {
		value, status := definition.read(sessionID(session))
		return awu.NewDataValue(value, status, time.Time{}, 0, time.Now(), 0)
	})
	if definition.write != nil {
		node.SetWriteValueHandler(func(session *awserver.Session, req awu.WriteValue) (awu.DataValue, awu.StatusCode) {
			status := definition.write(sessionID(session), req.Value.Value)
			return awu.NewDataValue(req.Value.Value, status, time.Time{}, 0, time.Now(), 0), status
		})
	}
	if err := s.server.NamespaceManager().AddNode(node); err != nil {
		return err
	}
	s.management[nodeID.String()] = node
	return nil
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
