package domain

import (
	"net/url"
	"strings"
	"time"
)

const (
	ProcessInstanceHeader   = "X-RCD-Instance-ID"
	MaxProcessLogBytes      = 64 << 10
	MaxProcessResponseBytes = 1 << 20
)

// ProcessBinding is management configuration, independent of the OPC endpoint
// and its connection epoch. Revision zero denotes an unconfigured binding.
type ProcessBinding struct {
	SourceID       string    `json:"source_id"`
	SourceRevision uint64    `json:"source_revision"`
	AgentURL       string    `json:"agent_url"`
	InstanceID     string    `json:"instance_id"`
	Revision       uint64    `json:"revision"`
	UpdatedAt      time.Time `json:"updated_at,omitempty,omitzero"`
}

func (b ProcessBinding) Configured() bool { return b.AgentURL != "" && b.InstanceID != "" }

func (b ProcessBinding) Validate() error {
	if b.AgentURL == "" && b.InstanceID == "" {
		return nil
	}
	if !ValidOperationRevision(b.InstanceID) {
		return &FieldError{Field: "instance_id", Message: "a stable process-agent instance identifier is required"}
	}
	u, err := url.Parse(b.AgentURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || len(b.AgentURL) > 2048 {
		return &FieldError{Field: "agent_url", Message: "must be an absolute HTTP(S) agent URL without credentials, query, or path"}
	}
	return nil
}

type ProcessSnapshot struct {
	SchemaVersion    int        `json:"schema_version,omitempty"`
	InstanceID       string     `json:"instance_id"`
	Revision         string     `json:"revision"`
	State            string     `json:"state"`
	DesiredState     string     `json:"desired_state"`
	PID              int        `json:"pid,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	UptimeSeconds    int64      `json:"uptime_seconds,omitempty"`
	ExitCode         *int       `json:"exit_code,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	PendingRequestID string     `json:"pending_request_id,omitempty"`
	Executable       string     `json:"executable,omitempty"`
	WorkDir          string     `json:"work_dir,omitempty"`
	RestartPolicy    string     `json:"restart_policy,omitempty"`
	RestartCount     uint64     `json:"restart_count,omitempty"`
	LastExitAt       *time.Time `json:"last_exit_at,omitempty"`
	LastStopForced   bool       `json:"last_stop_forced,omitempty"`
}

type ProcessActionRequest struct {
	RequestID        string `json:"request_id"`
	InstanceID       string `json:"instance_id"`
	Action           string `json:"action"`
	ExpectedRevision string `json:"expected_revision"`
}

func (r ProcessActionRequest) Validate() error {
	if !ValidOperationRevision(r.RequestID) || r.RequestID == "." || r.RequestID == ".." || !ValidOperationRevision(r.InstanceID) {
		return &FieldError{Field: "request_id", Message: "request_id and instance_id must be stable nonempty identifiers"}
	}
	if r.Action != "start" && r.Action != "stop" && r.Action != "restart" {
		return &FieldError{Field: "action", Message: "must be start, stop, or restart"}
	}
	if !ValidOperationRevision(r.ExpectedRevision) {
		return &FieldError{Field: "revision", Message: "an exact process-agent revision is required"}
	}
	return nil
}

// Source identity fields are filled by the manager, never trusted from agents.
// Only succeeded/failed are terminal. Unknown keeps the maintenance fence until
// reconciliation obtains an authoritative terminal operation for this ID.
type ProcessOperation struct {
	SourceID        string           `json:"source_id,omitempty"`
	SourceRevision  uint64           `json:"source_revision,omitempty"`
	BindingRevision uint64           `json:"binding_revision,omitempty"`
	RequestID       string           `json:"request_id"`
	InstanceID      string           `json:"instance_id"`
	Action          string           `json:"action"`
	State           string           `json:"state"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	ErrorCode       string           `json:"error_code,omitempty"`
	Error           string           `json:"error,omitempty"`
	Forced          bool             `json:"forced,omitempty"`
	Snapshot        *ProcessSnapshot `json:"snapshot,omitempty"`
}

func (o ProcessOperation) Terminal() bool { return o.State == "succeeded" || o.State == "failed" }

type ProcessLog struct {
	InstanceID string `json:"instance_id"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	SizeBytes  int64  `json:"size_bytes"`
}

func (s ProcessSnapshot) Validate(instanceID string) error {
	if s.InstanceID != instanceID || !ValidOperationRevision(s.Revision) || (s.SchemaVersion != 0 && s.SchemaVersion != 1) {
		return invalidProcessResponse("进程代理身份或版本不匹配")
	}
	switch s.State {
	case "stopped", "starting", "running", "stopping", "failed", "unknown":
	default:
		return invalidProcessResponse("进程状态无效")
	}
	if (s.DesiredState != "running" && s.DesiredState != "stopped") || s.PID < 0 || s.UptimeSeconds < 0 ||
		(s.State == "running" && s.PID <= 1) || (s.PendingRequestID != "" && !ValidOperationRevision(s.PendingRequestID)) ||
		len(s.LastError) > 16384 || len(s.Executable) > 4096 || len(s.WorkDir) > 4096 {
		return invalidProcessResponse("进程状态字段无效或超限")
	}
	if s.RestartPolicy != "" && s.RestartPolicy != "never" && s.RestartPolicy != "on-failure" {
		return invalidProcessResponse("进程重启策略无效")
	}
	return nil
}

func (o ProcessOperation) Validate(request ProcessActionRequest) error {
	if o.RequestID != request.RequestID || o.InstanceID != request.InstanceID || o.Action != request.Action ||
		o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() || o.UpdatedAt.Before(o.CreatedAt) || len(o.Error) > 16384 || len(o.ErrorCode) > 128 {
		return invalidProcessResponse("进程作业身份或字段无效")
	}
	switch o.State {
	case "accepted", "running", "succeeded", "failed", "unknown":
	default:
		return invalidProcessResponse("进程作业状态无效")
	}
	if o.Snapshot != nil {
		if err := o.Snapshot.Validate(request.InstanceID); err != nil {
			return err
		}
	}
	if o.State == "succeeded" {
		want := "running"
		if request.Action == "stop" {
			want = "stopped"
		}
		if o.Snapshot == nil || o.Snapshot.State != want || o.Snapshot.DesiredState != want {
			return invalidProcessResponse("成功作业缺少已确认的目标进程状态")
		}
	}
	return nil
}

func NormalizeProcessAgentURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func invalidProcessResponse(message string) error {
	return &OperationError{Code: "invalid_response", Message: message}
}

// SourcePendingCount covers every physical port, independent of logical group
// or originating Session. Runtime serializes command/process admission around
// this snapshot with configMu so neither direction can pass the other's fence.
func (t *RouteTable) SourcePendingCount(sourceID string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	count := 0
	for _, command := range t.pendingByKey {
		if command.SourceID == sourceID {
			count++
		}
	}
	return count
}
