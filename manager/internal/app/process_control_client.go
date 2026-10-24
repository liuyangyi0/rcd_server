package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
)

const processRequestTimeout = 5 * time.Second

// ProcessAgentClient is independent of the lower OPC connection. A stopped or
// manager-disabled source still has a usable process-agent management channel.
type ProcessAgentClient interface {
	Status(context.Context, domain.ProcessBinding) (domain.ProcessSnapshot, error)
	Action(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error)
	Operation(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error)
	Resolve(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessOperation, error)
	Log(context.Context, domain.ProcessBinding, int) (domain.ProcessLog, error)
}

type processAgentHTTPClient struct{ client *http.Client }

func (c *processAgentHTTPClient) SoftwareVersion(ctx context.Context, binding domain.ProcessBinding) (buildinfo.Info, error) {
	var info buildinfo.Info
	err := c.exchange(ctx, binding, http.MethodGet, "/api/v1/version", nil, &info)
	if err == nil {
		err = domain.ValidateSoftwareBuild(info, "agent")
	}
	return info, err
}

func newProcessAgentHTTPClient() ProcessAgentClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 4
	return &processAgentHTTPClient{client: &http.Client{Timeout: processRequestTimeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("process agent redirects are not supported")
		}}}
}

func (c *processAgentHTTPClient) exchange(ctx context.Context, binding domain.ProcessBinding, method, path string, body any, result any) error {
	if !binding.Configured() {
		return processUnsupported()
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, domain.NormalizeProcessAgentURL(binding.AgentURL)+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set(domain.ProcessInstanceHeader, binding.InstanceID)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if action, ok := body.(domain.ProcessActionRequest); ok {
		request.Header.Set("If-Match", strconv.Quote(action.ExpectedRevision))
	}
	response, err := c.client.Do(request)
	if err != nil {
		code, message := "unavailable", "进程代理不可达，请检查代理状态；进程是否运行尚未确认"
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			code, message = "timeout", "进程代理请求超时，请按原 request_id 核对结果，不要重复操作"
		}
		return &domain.OperationError{Code: code, Message: message}
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, domain.MaxProcessResponseBytes+1))
	if err != nil {
		return &domain.OperationError{Code: "unavailable", Message: "进程代理响应中断，结果尚未确认"}
	}
	if len(content) > domain.MaxProcessResponseBytes || !utf8.Valid(content) {
		return processInvalidResponse("进程代理响应超限或编码无效")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		var envelope struct {
			Error *domain.OperationError `json:"error"`
		}
		if json.Unmarshal(content, &envelope) != nil || envelope.Error == nil || envelope.Error.Code == "" ||
			len(envelope.Error.Code) > 128 || len(envelope.Error.Message) > 16384 || len(envelope.Error.CurrentRevision) > 128 {
			return processInvalidResponse(fmt.Sprintf("进程代理返回无效错误（HTTP %d）", response.StatusCode))
		}
		return envelope.Error
	}
	if json.Unmarshal(content, result) != nil {
		return processInvalidResponse("进程代理返回 JSON 无效")
	}
	return nil
}

func (c *processAgentHTTPClient) Status(ctx context.Context, binding domain.ProcessBinding) (domain.ProcessSnapshot, error) {
	var result domain.ProcessSnapshot
	if err := c.exchange(ctx, binding, http.MethodGet, "/api/v1/process", nil, &result); err != nil {
		return result, err
	}
	return result, result.Validate(binding.InstanceID)
}

func (c *processAgentHTTPClient) Action(ctx context.Context, binding domain.ProcessBinding, request domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	return c.operation(ctx, binding, request, http.MethodPost, processAgentRoute(request)+"/actions", request)
}

func processAgentRoute(request domain.ProcessActionRequest) string {
	if request.Lifecycle {
		return "/api/v1/lifecycle"
	}
	return "/api/v1/process"
}

func (c *processAgentHTTPClient) Prepare(ctx context.Context, binding domain.ProcessBinding, request domain.ProcessActionRequest) (domain.ProcessActionRequest, error) {
	status, err := c.Status(ctx, binding)
	if err != nil {
		return request, err
	}
	if status.LifecycleSupported {
		if request.ConfigurationGuard != "" && !status.ConfigurationGuardSupported {
			return request, &domain.OperationError{Code: "unsupported", Message: "Agent 未提供维护后的配置版本复核，未派发重启"}
		}
		request.Lifecycle, request.GatewayInstanceID = true, status.GatewayInstanceID
		return request, request.Validate()
	}
	return request, &domain.OperationError{Code: "unsupported", Message: "代理未提供安全生命周期能力，请先升级并配置 Agent；未退回无保护操作"}
}

func (c *processAgentHTTPClient) Operation(ctx context.Context, binding domain.ProcessBinding, request domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	return c.operation(ctx, binding, request, http.MethodGet, processAgentRoute(request)+"/operations/"+url.PathEscape(request.RequestID), nil)
}

// Resolve is not a repeated process action. The agent atomically reads an
// existing operation or stores a failed tombstone that fences any late Action.
func (c *processAgentHTTPClient) Resolve(ctx context.Context, binding domain.ProcessBinding, request domain.ProcessActionRequest) (domain.ProcessOperation, error) {
	return c.operation(ctx, binding, request, http.MethodPost, processAgentRoute(request)+"/operations/"+url.PathEscape(request.RequestID)+"/resolve", request)
}

func (c *processAgentHTTPClient) operation(ctx context.Context, binding domain.ProcessBinding, request domain.ProcessActionRequest, method, path string, body any) (domain.ProcessOperation, error) {
	var result domain.ProcessOperation
	if request.InstanceID != binding.InstanceID {
		return result, processInvalidResponse("固定进程实例不匹配")
	}
	if err := c.exchange(ctx, binding, method, path, body, &result); err != nil {
		return result, err
	}
	return result, result.Validate(request)
}

func (c *processAgentHTTPClient) Log(ctx context.Context, binding domain.ProcessBinding, limit int) (domain.ProcessLog, error) {
	var wire struct {
		InstanceID string  `json:"instance_id"`
		Text       *string `json:"text"`
		Truncated  *bool   `json:"truncated"`
		SizeBytes  *int64  `json:"size_bytes"`
	}
	if err := c.exchange(ctx, binding, http.MethodGet, "/api/v1/process/log?limit="+strconv.Itoa(limit), nil, &wire); err != nil {
		return domain.ProcessLog{}, err
	}
	if wire.Text == nil || wire.SizeBytes == nil || wire.Truncated == nil || wire.InstanceID != binding.InstanceID ||
		*wire.SizeBytes < 0 || len(*wire.Text) > limit || !utf8.ValidString(*wire.Text) {
		return domain.ProcessLog{}, processInvalidResponse("进程日志身份、长度或游标无效")
	}
	return domain.ProcessLog{InstanceID: wire.InstanceID, Text: *wire.Text, Truncated: *wire.Truncated, SizeBytes: *wire.SizeBytes}, nil
}

func processUnsupported() error {
	return &domain.OperationError{Code: "unsupported", Message: "此来源尚未绑定独立进程代理"}
}

func processInvalidResponse(message string) error {
	return &domain.OperationError{Code: "invalid_response", Message: message}
}
