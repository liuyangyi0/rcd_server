package opc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

// LowerOperationsClient is optional. Older/fake lower clients keep their
// existing data/command contract and need not implement maintenance methods.
type LowerOperationsClient interface {
	OperationsCapabilities(context.Context, uint64) (domain.OperationsCapabilities, error)
	ExecuteOperation(context.Context, domain.OperationRequest, uint64) (json.RawMessage, error)
}

func (s *LowerSupervisor) operationsTarget(epoch uint64) (*gopcua.Client, uint16, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.status.Connection != ConnectionConnected || epoch != s.status.Epoch {
		return nil, 0, ErrLowerUnavailable
	}
	ns, ok := s.namespaces[domain.OperationsNamespaceURI]
	if !ok {
		return nil, 0, &domain.OperationError{Code: "unsupported", Message: "下级未提供运维能力，请升级 Rust 下级程序"}
	}
	return s.client, ns, nil
}

func (s *LowerSupervisor) OperationsCapabilities(ctx context.Context, epoch uint64) (domain.OperationsCapabilities, error) {
	client, ns, err := s.operationsTarget(epoch)
	var operationErr *domain.OperationError
	if errors.As(err, &operationErr) && operationErr.Code == "unsupported" {
		return domain.OperationsCapabilities{Reason: operationErr.Message, Features: []string{}}, nil
	}
	if err != nil {
		return domain.OperationsCapabilities{}, err
	}
	if err := s.acquireOperationSlot(); err != nil {
		return domain.OperationsCapabilities{}, err
	}
	defer s.operationCalls.Add(-1)
	response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, "Operations/Capabilities"), AttributeID: gu.AttributeIDValue}}})
	if err != nil {
		return domain.OperationsCapabilities{}, operationTransportError(err)
	}
	if response == nil || len(response.Results) != 1 || response.Results[0] == nil || response.Results[0].Status != gu.StatusOK || response.Results[0].Value == nil {
		return domain.OperationsCapabilities{}, invalidOperationsResponse("下级运维能力节点无有效值")
	}
	content, ok := response.Results[0].Value.Value().(string)
	if !ok || len(content) > 16384 {
		return domain.OperationsCapabilities{}, invalidOperationsResponse("下级运维能力格式无效")
	}
	var capabilities domain.OperationsCapabilities
	if err := json.Unmarshal([]byte(content), &capabilities); err != nil {
		return capabilities, invalidOperationsResponse("下级运维能力 JSON 无效")
	}
	capabilities.SourceID, capabilities.SourceRevision = "", 0
	capabilities.Supported = capabilities.SchemaVersion == 1
	if !capabilities.Supported {
		capabilities.Reason = "下级运维协议版本不受支持"
	}
	if capabilities.Features == nil {
		capabilities.Features = []string{}
	}
	return capabilities, nil
}

func (s *LowerSupervisor) ExecuteOperation(ctx context.Context, request domain.OperationRequest, epoch uint64) (json.RawMessage, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	client, ns, err := s.operationsTarget(epoch)
	if err != nil {
		return nil, err
	}
	if err := s.acquireOperationSlot(); err != nil {
		return nil, err
	}
	defer s.operationCalls.Add(-1)
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(payload) > domain.MaxOperationJSONBytes {
		return nil, &domain.OperationError{Code: "too_large", Message: "运维请求超限"}
	}
	response, err := client.Call(ctx, &gu.CallMethodRequest{
		ObjectID: gu.NewStringNodeID(ns, "Operations"), MethodID: gu.NewStringNodeID(ns, "Operations/Execute"),
		InputArguments: []*gu.Variant{gu.MustVariant(string(payload))},
	})
	if err != nil {
		return nil, operationTransportError(err)
	}
	if response != nil && response.StatusCode == gu.StatusBadTimeout {
		return nil, operationTransportError(response.StatusCode)
	}
	if response == nil || response.StatusCode != gu.StatusOK || len(response.OutputArguments) != 1 || response.OutputArguments[0] == nil {
		return nil, invalidOperationsResponse("下级运维方法调用失败或返回参数无效")
	}
	content, ok := response.OutputArguments[0].Value().(string)
	if !ok || len(content) > domain.MaxOperationJSONBytes {
		return nil, invalidOperationsResponse("下级运维返回文本无效或超限")
	}
	result, err := decodeOperationEnvelope(content)
	if err != nil {
		return nil, err
	}
	if err := validateOperationResult(request, result); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeOperationEnvelope(content string) (json.RawMessage, error) {
	var envelope struct {
		OK     *bool                  `json:"ok"`
		Result json.RawMessage        `json:"result"`
		Error  *domain.OperationError `json:"error"`
	}
	if err := json.Unmarshal([]byte(content), &envelope); err != nil || envelope.OK == nil {
		return nil, invalidOperationsResponse("下级运维返回 JSON 无效")
	}
	if !*envelope.OK {
		if envelope.Error == nil || envelope.Error.Code == "" || len(envelope.Error.Code) > 128 || len(envelope.Error.CurrentRevision) > 512 || len(envelope.Error.Message) > 16384 {
			return nil, invalidOperationsResponse("下级运维错误格式无效")
		}
		return nil, envelope.Error
	}
	var result map[string]json.RawMessage
	if envelope.Error != nil || json.Unmarshal(envelope.Result, &result) != nil || result == nil {
		return nil, invalidOperationsResponse("下级运维结果不是有效对象")
	}
	return envelope.Result, nil
}

func invalidOperationsResponse(message string) error {
	return &domain.OperationError{Code: "invalid_response", Message: message}
}

func (s *LowerSupervisor) acquireOperationSlot() error {
	if s.operationCalls.Add(1) > 4 {
		s.operationCalls.Add(-1)
		return &domain.OperationError{Code: "busy", Message: "该下级运维请求过多，请稍后重试"}
	}
	return nil
}

func operationTransportError(err error) error {
	// The UA client's own shorter RequestTimeout returns a StatusCode rather
	// than ctx.Err(). Preserve timeout semantics across the library boundary.
	if errors.Is(err, gu.StatusBadTimeout) {
		return fmt.Errorf("lower operations request timeout: %w", context.DeadlineExceeded)
	}
	return err
}
