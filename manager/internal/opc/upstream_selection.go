package opc

import (
	"fmt"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
)

// SelectSource is an additive CAS interface. Keep the legacy String variable
// unchanged for clients that cannot call Methods. The expected revision must
// come from the caller, not a fresh server-side read that hides stale intent.
func (s *UpstreamServer) addGroupSelectionMethodLocked(groupID string, parent awu.NodeID) error {
	identifier := "Groups/" + groupID + "/SelectSource"
	methodID := awu.NewNodeIDString(s.managementNS, identifier)
	if _, exists := s.management[methodID.String()]; exists {
		return nil
	}
	method := awserver.NewMethodNode(
		s.server, methodID, awu.NewQualifiedName(s.managementNS, "SelectSource"),
		awu.NewLocalizedText("SelectSource", ""),
		awu.NewLocalizedText("Select this Session's group source only when ExpectedRevision matches its current route revision. Does not revoke previously accepted lower snapshots.", ""), nil,
		[]awu.Reference{awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(parent))}, true,
	)
	method.SetCallMethodHandler(func(session *awserver.Session, req awu.CallMethodRequest) awu.CallMethodResult {
		if req.ObjectID != parent {
			return awu.CallMethodResult{StatusCode: awu.BadMethodInvalid}
		}
		return callSelectSource(s.backend, sessionID(session), groupID, req.InputArguments)
	})
	input := methodArgumentNode(s.server, awu.NewNodeIDString(s.managementNS, identifier+"_Input"), methodID, "InputArguments", []awu.Argument{
		{Name: "SourceId", DataType: awu.DataTypeIDString, ValueRank: awu.ValueRankScalar,
			Description: awu.NewLocalizedText("Stable source ID in this group; not an IP address or display name.", "")},
		{Name: "ExpectedRevision", DataType: awu.DataTypeIDUInt64, ValueRank: awu.ValueRankScalar,
			Description: awu.NewLocalizedText("SelectionRevision read by this same Session. Stale values fail without changing its route.", "")},
	})
	output := methodArgumentNode(s.server, awu.NewNodeIDString(s.managementNS, identifier+"_Output"), methodID, "OutputArguments", []awu.Argument{
		{Name: "SelectedSourceId", DataType: awu.DataTypeIDString, ValueRank: awu.ValueRankScalar,
			Description: awu.NewLocalizedText("Source accepted by this operation.", "")},
		{Name: "SelectionRevision", DataType: awu.DataTypeIDUInt64, ValueRank: awu.ValueRankScalar,
			Description: awu.NewLocalizedText("Route revision committed by this operation; later operations may change it again.", "")},
	})
	if err := s.server.NamespaceManager().AddNodes(method, input, output); err != nil {
		return err
	}
	for _, node := range []awserver.Node{method, input, output} {
		s.management[fmt.Sprint(node.NodeID())] = node
	}
	return nil
}

func callSelectSource(backend UpstreamBackend, sid, groupID string, arguments []awu.Variant) awu.CallMethodResult {
	if len(arguments) < 2 {
		return awu.CallMethodResult{StatusCode: awu.BadArgumentsMissing}
	}
	if len(arguments) > 2 {
		return awu.CallMethodResult{StatusCode: awu.BadTooManyArguments}
	}
	results := []awu.StatusCode{awu.Good, awu.Good}
	sourceID, sourceOK := arguments[0].(string)
	revision, revisionOK := arguments[1].(uint64)
	if !sourceOK {
		results[0] = awu.BadTypeMismatch
	}
	if !revisionOK {
		results[1] = awu.BadTypeMismatch
	}
	if !sourceOK || !revisionOK {
		return awu.CallMethodResult{StatusCode: awu.BadTypeMismatch, InputArgumentResults: results}
	}
	if sourceID == "" {
		results[0] = awu.BadInvalidArgument
		return awu.CallMethodResult{StatusCode: awu.BadInvalidArgument, InputArgumentResults: results}
	}
	// The backend's one route transaction checks Session, revision, group
	// membership, selectability and pending commands before committing. Reuse it
	// so Web, legacy Write and this Method share the same lock and semantics.
	route, err := backend.SelectSource(sid, groupID, sourceID, revision)
	status := statusFromBackend(domain.StatusGood, err)
	if status != awu.Good {
		return awu.CallMethodResult{StatusCode: status, InputArgumentResults: results}
	}
	return awu.CallMethodResult{
		StatusCode: awu.Good, InputArgumentResults: results,
		OutputArguments: []awu.Variant{route.SourceID, route.Revision},
	}
}
