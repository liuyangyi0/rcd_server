package opc

import (
	"fmt"
	"time"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
)

// Group commands deliberately live in ns=3. The old ns=2 physical-port
// interfaces remain unchanged, including their NodeIds and method arguments.
func (s *UpstreamServer) addGroupCommandNodesLocked(groupID string, parent awu.NodeID) error {
	backend, supported := s.backend.(GroupCommandBackend)
	sendID := awu.NewNodeIDString(s.managementNS, "Groups/"+groupID+"/Send")
	if _, exists := s.management[sendID.String()]; !exists {
		node := awserver.NewVariableNode(
			s.server, sendID, awu.NewQualifiedName(s.managementNS, "Send"),
			awu.NewLocalizedText("Send", ""), awu.NewLocalizedText("Session-scoped logical device command and ACK", ""), nil,
			[]awu.Reference{
				awu.NewReference(awu.ReferenceTypeIDHasTypeDefinition, false, awu.NewExpandedNodeID(awu.VariableTypeIDBaseDataVariableType)),
				awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(parent)),
			}, awu.NewDataValue("", awu.BadWaitingForInitialData, time.Time{}, 0, time.Now(), 0),
			awu.DataTypeIDString, awu.ValueRankScalar, nil,
			awu.AccessLevelsCurrentRead|awu.AccessLevelsCurrentWrite, upstreamMinimumSamplingIntervalMS, false, nil,
		)
		node.SetReadValueHandler(func(session *awserver.Session, _ awu.ReadValueID) awu.DataValue {
			if !supported {
				return awu.NewDataValue("", awu.BadNotSupported, time.Time{}, 0, time.Now(), 0)
			}
			return sampleToDataValue(backend.ReadGroupCommand(sessionID(session), groupID), domain.DataTypeString, awu.ValueRankScalar, time.Now())
		})
		node.SetWriteValueHandler(func(session *awserver.Session, req awu.WriteValue) (awu.DataValue, awu.StatusCode) {
			if !supported {
				return awu.NewDataValue("", awu.BadNotSupported, time.Time{}, 0, time.Now(), 0), awu.BadNotSupported
			}
			if _, ok := req.Value.Value.(string); !ok {
				return awu.NewDataValue("", awu.BadTypeMismatch, time.Time{}, 0, time.Now(), 0), awu.BadTypeMismatch
			}
			ctx, cancel := s.commandContext()
			defer cancel()
			status, err := backend.WriteGroupCommand(ctx, sessionID(session), groupID, req.Value.Value)
			uaStatus := statusFromBackend(status, err)
			return awu.NewDataValue(req.Value.Value, uaStatus, time.Time{}, 0, time.Now(), 0), uaStatus
		})
		if err := s.server.NamespaceManager().AddNode(node); err != nil {
			return err
		}
		s.management[sendID.String()] = node
	}

	methodID := awu.NewNodeIDString(s.managementNS, "Groups/"+groupID+"/SendCommand")
	if _, exists := s.management[methodID.String()]; exists {
		return nil
	}
	method := awserver.NewMethodNode(
		s.server, methodID, awu.NewQualifiedName(s.managementNS, "SendCommand"),
		awu.NewLocalizedText("SendCommand", ""), awu.NewLocalizedText("Route HexData using this Session's logical device selection", ""), nil,
		[]awu.Reference{awu.NewReference(awu.ReferenceTypeIDHasComponent, true, awu.NewExpandedNodeID(parent))}, true,
	)
	method.SetCallMethodHandler(func(session *awserver.Session, req awu.CallMethodRequest) awu.CallMethodResult {
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
		if !supported {
			return awu.CallMethodResult{StatusCode: awu.BadNotSupported}
		}
		ctx, cancel := s.commandContext()
		defer cancel()
		result, status, err := backend.CallGroupCommand(ctx, sessionID(session), groupID, hexData)
		uaStatus := statusFromBackend(status, err)
		if uaStatus != awu.Good {
			return awu.CallMethodResult{StatusCode: uaStatus}
		}
		return awu.CallMethodResult{StatusCode: awu.Good, InputArgumentResults: []awu.StatusCode{awu.Good}, OutputArguments: []awu.Variant{result}}
	})
	inputID := awu.NewNodeIDString(s.managementNS, "Groups/"+groupID+"/SendCommand_Input")
	outputID := awu.NewNodeIDString(s.managementNS, "Groups/"+groupID+"/SendCommand_Output")
	input := methodArgumentNode(s.server, inputID, methodID, "InputArguments", []awu.Argument{{
		Name: "HexData", DataType: awu.DataTypeIDString, ValueRank: awu.ValueRankScalar,
		Description: awu.NewLocalizedText("HexData", ""),
	}})
	output := methodArgumentNode(s.server, outputID, methodID, "OutputArguments", []awu.Argument{{
		Name: "Result", DataType: awu.DataTypeIDString, ValueRank: awu.ValueRankScalar,
		Description: awu.NewLocalizedText("Result", ""),
	}})
	if err := s.server.NamespaceManager().AddNodes(method, input, output); err != nil {
		return err
	}
	for _, node := range []awserver.Node{method, input, output} {
		s.management[fmt.Sprint(node.NodeID())] = node
	}
	return nil
}
