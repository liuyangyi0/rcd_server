package opc

import (
	"context"
	"encoding/json"
	"fmt"
	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
	"time"
)

func verifyGateway(ctx context.Context, client *gopcua.Client, expected string) error {
	ns, ok := namespaceIndex(client.Namespaces())[domain.OperationsNamespaceURI]
	if !ok {
		return fmt.Errorf("来源未提供稳定身份接口")
	}
	response, err := client.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(ns, "Operations"), MethodID: gu.NewStringNodeID(ns, "Operations/Execute"), InputArguments: []*gu.Variant{gu.MustVariant(`{"action":"command_tracking_status"}`)}})
	if err != nil {
		return err
	}
	if response == nil || response.StatusCode != gu.StatusOK || len(response.OutputArguments) != 1 || response.OutputArguments[0] == nil {
		return fmt.Errorf("来源身份响应无效")
	}
	content, ok := response.OutputArguments[0].Value().(string)
	if !ok || len(content) > 65536 {
		return fmt.Errorf("来源身份响应超限")
	}
	raw, err := decodeOperationEnvelope(content)
	if err != nil {
		return err
	}
	var info struct {
		Gateway string `json:"gateway_instance_id"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Gateway != expected {
		return fmt.Errorf("来源 gateway 身份与统一台账不符，拒绝连接")
	}
	return nil
}

// VerifyGatewayIdentity makes one read-only identity RPC, without browse,
// subscriptions, writes, or changing the live source's epoch.
func VerifyGatewayIdentity(ctx context.Context, cfg LowerConfig, expected string) error {
	if !domain.ValidOperationRevision(expected) {
		return fmt.Errorf("无效 gateway 身份")
	}
	s, err := NewLowerSupervisor(cfg, NopLowerSink{})
	if err != nil {
		return err
	}
	client, err := s.newClient(ctx, make(chan gopcua.ConnState, 8))
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Close(closeCtx)
	}()
	if err = client.Connect(ctx); err != nil {
		return err
	}
	return verifyGateway(ctx, client, expected)
}
