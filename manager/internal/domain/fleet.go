package domain

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Shared inventory contains no business configuration or routing choices.
type FleetMachine struct {
	ID                string           `json:"id"`
	Revision          uint64           `json:"revision"`
	Name              string           `json:"name"`
	Address           string           `json:"address"`
	OPCEndpoint       string           `json:"opc_endpoint"`
	AgentURL          string           `json:"agent_url"`
	AgentInstanceID   string           `json:"agent_instance_id"`
	GatewayInstanceID string           `json:"gateway_instance_id"`
	ManagerInstanceID string           `json:"manager_instance_id"`
	WebURL            string           `json:"web_url"`
	Components        []FleetComponent `json:"components"`
	LastSuccessAt     string           `json:"last_success_at"`
	ActiveOperationID string           `json:"active_operation_id"`
}
type FleetComponent struct {
	Component     string `json:"component"`
	State         string `json:"state"`
	RunningBuild  any    `json:"running_build"`
	DiskBuild     any    `json:"disk_build"`
	RunningSHA256 string `json:"running_sha256"`
	DiskSHA256    string `json:"disk_sha256"`
}
type FleetSnapshot struct {
	SchemaVersion int            `json:"schema_version"`
	Machines      []FleetMachine `json:"machines"`
}

func (m FleetMachine) SameBinding(other FleetMachine) bool {
	return m.ID == other.ID && m.Name == other.Name && m.Address == other.Address && m.OPCEndpoint == other.OPCEndpoint && m.AgentURL == other.AgentURL && m.AgentInstanceID == other.AgentInstanceID && m.GatewayInstanceID == other.GatewayInstanceID && m.ManagerInstanceID == other.ManagerInstanceID && m.WebURL == other.WebURL
}

type FleetSourceStatus struct {
	SourceID    string        `json:"source_id"`
	DeviceID    string        `json:"device_id"`
	Machine     *FleetMachine `json:"machine,omitempty"`
	AppliedAt   time.Time     `json:"applied_at,omitempty,omitzero"`
	Error       string        `json:"error,omitempty"`
	PlatformURL string        `json:"platform_url,omitempty"`
}
type FleetStatus struct {
	Enabled       bool                `json:"enabled"`
	PlatformURL   string              `json:"platform_url"`
	LastSyncAt    time.Time           `json:"last_sync_at,omitempty,omitzero"`
	LastAttemptAt time.Time           `json:"last_attempt_at,omitempty,omitzero"`
	Error         string              `json:"error,omitempty"`
	Sources       []FleetSourceStatus `json:"sources"`
}

func (m FleetMachine) Validate() error {
	if !ValidOperationRevision(m.ID) || m.Revision == 0 || strings.TrimSpace(m.Name) == "" || len(m.Name) > 128 || net.ParseIP(m.Address) == nil ||
		!ValidOperationRevision(m.AgentInstanceID) || !ValidOperationRevision(m.GatewayInstanceID) || !ValidOperationRevision(m.ManagerInstanceID) {
		return fmt.Errorf("台账机器身份不完整")
	}
	for raw, kind := range map[string]string{m.OPCEndpoint: "opc.tcp", m.AgentURL: "http", m.WebURL: "http"} {
		u, e := url.Parse(raw)
		if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() != m.Address {
			return fmt.Errorf("台账地址必须指向同一固定 IP")
		}
		if kind == "opc.tcp" {
			if u.Scheme != kind {
				return fmt.Errorf("无效 OPC 地址")
			}
		} else if (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" {
			return fmt.Errorf("无效运维地址")
		}
	}
	return nil
}
