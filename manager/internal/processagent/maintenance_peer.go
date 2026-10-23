package processagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	uaClient "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
)

// LowerFence is evidence from one explicit local gateway, never a routing choice.
type LowerFence struct {
	SchemaVersion         int    `json:"schema_version"`
	GatewayInstanceID     string `json:"gateway_instance_id"`
	DeploymentID          string `json:"deployment_id"`
	FencingToken          string `json:"fencing_token"`
	Active                bool   `json:"active"`
	AdmissionBlocked      bool   `json:"admission_blocked"`
	Durable               bool   `json:"durable"`
	PendingAdmissions     uint64 `json:"pending_admissions"`
	PendingPortOperations uint64 `json:"pending_port_operations"`
	Drained               bool   `json:"drained"`
	Error                 string `json:"error"`
	BootID                string `json:"boot_id"`
}

func (f LowerFence) validate(gateway string) error {
	token, err := strconv.ParseUint(f.FencingToken, 10, 64)
	if err != nil || strconv.FormatUint(token, 10) != f.FencingToken || f.SchemaVersion != 1 || f.GatewayInstanceID != gateway ||
		!domain.ValidOperationRevision(f.BootID) || (token == 0 && (f.Active || f.DeploymentID != "")) ||
		(token > 0 && !maintenanceID(f.DeploymentID)) || len(f.Error) > 16384 ||
		f.AdmissionBlocked != (f.Active || !f.Durable) || f.Drained != (f.Active && f.Durable && f.PendingAdmissions == 0 && f.PendingPortOperations == 0) {
		return errors.New("invalid lower maintenance identity or state")
	}
	return nil
}

type maintenancePeer interface {
	Read(context.Context) (LowerFence, error)
	Change(context.Context, string, string, string, string) (LowerFence, error)
}

type maintenanceConfigurationPeer interface {
	ValidateConfiguration(context.Context, LowerFence, map[string]string) error
}

type opcMaintenancePeer struct{ endpoint, gateway string }

// Each bounded connection disables reconnect: writes are never transparently
// replayed and every mutation names the exact observed boot identity.
func (p opcMaintenancePeer) exchange(ctx context.Context, action, deployment, token, boot string, expected ...map[string]string) (LowerFence, error) {
	endpoints, err := uaClient.GetEndpoints(ctx, p.endpoint, uaClient.RequestTimeout(2*time.Second), uaClient.AutoReconnect(false))
	if err != nil {
		return LowerFence{}, err
	}
	endpoint, err := uaClient.SelectEndpoint(endpoints, ua.SecurityPolicyURINone, ua.MessageSecurityModeNone)
	if err != nil {
		return LowerFence{}, err
	}
	client, err := uaClient.NewClient(p.endpoint, uaClient.SecurityFromEndpoint(endpoint, ua.UserTokenTypeAnonymous),
		uaClient.AuthAnonymous(), uaClient.AutoReconnect(false),
		uaClient.RequestTimeout(2*time.Second), uaClient.SessionTimeout(10*time.Second))
	if err != nil {
		return LowerFence{}, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Close(closeCtx)
	}()
	if err = client.Connect(ctx); err != nil {
		return LowerFence{}, err
	}
	response, err := client.Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{{NodeID: ua.NewNumericNodeID(0, 2255), AttributeID: ua.AttributeIDValue}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil || response.Results[0].Status != ua.StatusOK || response.Results[0].Value == nil {
		return LowerFence{}, fmt.Errorf("read lower namespaces: %v", err)
	}
	namespaces, ok := response.Results[0].Value.Value().([]string)
	if !ok {
		return LowerFence{}, errors.New("invalid lower namespace array")
	}
	var ns uint16
	for i, value := range namespaces {
		if value == domain.OperationsNamespaceURI && i > 0 && i <= 65535 {
			ns = uint16(i)
		}
	}
	if ns == 0 {
		return LowerFence{}, &domain.OperationError{Code: "unsupported", Message: "下级未提供维护运维命名空间"}
	}
	response, err = client.Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{{NodeID: ua.NewStringNodeID(ns, "Operations/Capabilities"), AttributeID: ua.AttributeIDValue}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil || response.Results[0].Status != ua.StatusOK || response.Results[0].Value == nil {
		return LowerFence{}, errors.New("lower capabilities unavailable")
	}
	content, ok := response.Results[0].Value.Value().(string)
	var capabilities struct {
		SchemaVersion int      `json:"schema_version"`
		Features      []string `json:"features"`
	}
	if !ok || len(content) > 16384 || json.Unmarshal([]byte(content), &capabilities) != nil || capabilities.SchemaVersion != 1 {
		return LowerFence{}, errors.New("invalid lower capabilities")
	}
	supported := false
	for _, feature := range capabilities.Features {
		if feature == "maintenance_gate_v1" {
			supported = true
		}
	}
	if !supported {
		return LowerFence{}, &domain.OperationError{Code: "unsupported", Message: "下级不支持软件维护栅栏；不得自动降级为停机操作"}
	}
	if len(expected) > 0 {
		guard := false
		for _, feature := range capabilities.Features {
			guard = guard || feature == "maintenance_config_guard_v1"
		}
		if !guard {
			return LowerFence{}, &domain.OperationError{Code: "unsupported", Message: "下级未提供维护内配置版本检查，未停止程序"}
		}
	}
	call := func(request any) (json.RawMessage, error) {
		payload, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		out, err := client.Call(ctx, &ua.CallMethodRequest{ObjectID: ua.NewStringNodeID(ns, "Operations"), MethodID: ua.NewStringNodeID(ns, "Operations/Execute"), InputArguments: []*ua.Variant{ua.MustVariant(string(payload))}})
		if err != nil {
			return nil, err
		}
		if out == nil || out.StatusCode != ua.StatusOK || len(out.OutputArguments) != 1 || out.OutputArguments[0] == nil {
			return nil, errors.New("invalid lower maintenance method response")
		}
		value, ok := out.OutputArguments[0].Value().(string)
		if !ok || len(value) > 65536 {
			return nil, errors.New("invalid or oversized lower maintenance result")
		}
		var envelope struct {
			OK     *bool                  `json:"ok"`
			Result json.RawMessage        `json:"result"`
			Error  *domain.OperationError `json:"error"`
		}
		if json.Unmarshal([]byte(value), &envelope) != nil || envelope.OK == nil {
			return nil, errors.New("invalid lower maintenance envelope")
		}
		if !*envelope.OK {
			if envelope.Error == nil {
				return nil, errors.New("missing lower maintenance error")
			}
			return nil, envelope.Error
		}
		if envelope.Error != nil || len(envelope.Result) == 0 {
			return nil, errors.New("inconsistent lower maintenance envelope")
		}
		return envelope.Result, nil
	}
	identityJSON, err := call(map[string]any{"action": "command_tracking_status", "schema_version": 1})
	if err != nil {
		return LowerFence{}, err
	}
	var identity struct {
		Gateway string `json:"gateway_instance_id"`
		Boot    string `json:"boot_id"`
	}
	if json.Unmarshal(identityJSON, &identity) != nil || identity.Gateway != p.gateway || !domain.ValidOperationRevision(identity.Boot) {
		return LowerFence{}, errors.New("configured gateway identity does not match local OPC")
	}
	request := map[string]any{"action": action, "schema_version": 1}
	if action != "maintenance_status" {
		if boot != identity.Boot {
			return LowerFence{}, errors.New("lower restarted before maintenance mutation")
		}
		request["deployment_id"], request["fencing_token"] = deployment, token
		request["expected_gateway_instance_id"], request["expected_boot_id"] = p.gateway, boot
		if len(expected) > 0 {
			request["expected_config_revisions"] = expected[0]
		}
	}
	raw, err := call(request)
	if err != nil {
		return LowerFence{}, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return LowerFence{}, errors.New("invalid lower fence")
	}
	for _, key := range []string{"schema_version", "gateway_instance_id", "deployment_id", "fencing_token", "active", "admission_blocked", "durable", "pending_admissions", "pending_port_operations", "drained", "error"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return LowerFence{}, fmt.Errorf("missing lower fence field: %s", key)
		}
	}
	var fence LowerFence
	if err = json.Unmarshal(raw, &fence); err != nil {
		return LowerFence{}, err
	}
	fence.BootID = identity.Boot
	return fence, fence.validate(p.gateway)
}

func (p opcMaintenancePeer) Read(ctx context.Context) (LowerFence, error) {
	return p.exchange(ctx, "maintenance_status", "", "", "")
}
func (p opcMaintenancePeer) Change(ctx context.Context, action, deployment, token, boot string) (LowerFence, error) {
	if action != "enter_maintenance" && action != "leave_maintenance" {
		return LowerFence{}, errors.New("invalid maintenance action")
	}
	return p.exchange(ctx, action, deployment, token, boot)
}

func (p opcMaintenancePeer) ValidateConfiguration(ctx context.Context, fence LowerFence, expected map[string]string) error {
	if err := fence.validate(p.gateway); err != nil || !fence.Active || !fence.Drained {
		return errors.New("configuration check requires a confirmed drained lower fence")
	}
	after, err := p.exchange(ctx, "validate_maintenance_configs", fence.DeploymentID, fence.FencingToken, fence.BootID, expected)
	if err != nil {
		return err
	}
	if !after.Active || !after.Drained || after.DeploymentID != fence.DeploymentID || after.FencingToken != fence.FencingToken || after.BootID != fence.BootID {
		return errors.New("configuration fence changed during validation")
	}
	return nil
}
