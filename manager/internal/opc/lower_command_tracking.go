package opc

import (
	"context"
	"encoding/json"
	"fmt"
	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
)

type TrackedCommandClient interface {
	CommandTrackingInfo(context.Context, uint64) (domain.CommandTrackingHealth, bool, error)
	SubmitTrackedCommand(context.Context, uint64, map[string]any) (domain.TrackedCommand, error)
	TrackedCommandEvents(context.Context, uint64, string, uint64) (domain.CommandEventBatch, error)
	GetTrackedCommand(context.Context, uint64, string, string) (*domain.TrackedCommand, error)
}

func (s *LowerSupervisor) trackedRPC(ctx context.Context, epoch uint64, request map[string]any, output any) error {
	client, ns, err := s.operationsTarget(epoch)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(raw) > 131072 {
		return fmt.Errorf("command request too large")
	}
	response, err := client.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(ns, "Operations"), MethodID: gu.NewStringNodeID(ns, "Operations/Execute"), InputArguments: []*gu.Variant{gu.MustVariant(string(raw))}})
	if err != nil {
		return operationTransportError(err)
	}
	if response == nil || response.StatusCode != gu.StatusOK || len(response.OutputArguments) != 1 || response.OutputArguments[0] == nil {
		return fmt.Errorf("invalid command RPC response")
	}
	current, _, err := s.operationsTarget(epoch)
	if err != nil || current != client {
		return ErrLowerUnavailable
	}
	content, ok := response.OutputArguments[0].Value().(string)
	if !ok || len(content) > 4<<20 {
		return fmt.Errorf("invalid command RPC payload")
	}
	result, err := decodeOperationEnvelope(content)
	if err != nil {
		return err
	}
	return json.Unmarshal(result, output)
}
func (s *LowerSupervisor) CommandTrackingInfo(ctx context.Context, epoch uint64) (domain.CommandTrackingHealth, bool, error) {
	capabilities, err := s.OperationsCapabilities(ctx, epoch)
	if err != nil {
		return domain.CommandTrackingHealth{}, false, err
	}
	if !capabilities.HasFeature("command_tracking") {
		return domain.CommandTrackingHealth{}, false, nil
	}
	var info domain.CommandTrackingHealth
	err = s.trackedRPC(ctx, epoch, map[string]any{"action": "command_tracking_status"}, &info)
	if err == nil && (info.GatewayInstanceID == "" || info.BootID == "") {
		err = fmt.Errorf("missing command tracking identity")
	}
	return info, true, err
}
func (s *LowerSupervisor) SubmitTrackedCommand(ctx context.Context, epoch uint64, request map[string]any) (domain.TrackedCommand, error) {
	var reply struct {
		Command domain.TrackedCommand `json:"command"`
	}
	err := s.trackedRPC(ctx, epoch, request, &reply)
	if err == nil {
		err = reply.Command.Validate()
	}
	return reply.Command, err
}
func (s *LowerSupervisor) TrackedCommandEvents(ctx context.Context, epoch uint64, manager string, after uint64) (domain.CommandEventBatch, error) {
	var reply domain.CommandEventBatch
	err := s.trackedRPC(ctx, epoch, map[string]any{"action": "list_command_events", "manager_instance_id": manager, "after": fmt.Sprint(after), "limit": 200}, &reply)
	if err == nil {
		for _, v := range reply.Items {
			if err = v.Validate(); err != nil {
				break
			}
		}
	}
	return reply, err
}
func (s *LowerSupervisor) GetTrackedCommand(ctx context.Context, epoch uint64, manager, id string) (*domain.TrackedCommand, error) {
	var reply struct {
		Command *domain.TrackedCommand `json:"command"`
	}
	err := s.trackedRPC(ctx, epoch, map[string]any{"action": "get_command", "manager_instance_id": manager, "command_id": id}, &reply)
	if err == nil && reply.Command != nil {
		err = reply.Command.Validate()
	}
	return reply.Command, err
}
