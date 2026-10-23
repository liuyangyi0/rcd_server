// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// A native Semaphore instance owns the RCD repository. Serialize identity
// binding checks across devices, in addition to per-device SQL revision CAS.
var machineBindingMu sync.Mutex

type MachineBinding struct {
	Name              string `json:"name,omitempty"`
	OPCEndpoint       string `json:"opc_endpoint"`
	AgentURL          string `json:"agent_url"`
	AgentInstanceID   string `json:"agent_instance_id"`
	GatewayInstanceID string `json:"gateway_instance_id"`
	ManagerInstanceID string `json:"manager_instance_id"`
}

var machineIdentifier = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,128}$`)

func (m MachineBinding) Validate(address string) error {
	if m.Name != "" && (strings.TrimSpace(m.Name) == "" || len(m.Name) > 128 || strings.ContainsAny(m.Name, "\n\r\x00")) {
		return ErrInvalid
	}
	if !machineIdentifier.MatchString(m.AgentInstanceID) || !machineIdentifier.MatchString(m.GatewayInstanceID) || !machineIdentifier.MatchString(m.ManagerInstanceID) {
		return ErrInvalid
	}
	for raw, scheme := range map[string]string{m.OPCEndpoint: "opc.tcp", m.AgentURL: "http"} {
		u, e := url.Parse(raw)
		if e != nil || u.User != nil || u.Hostname() != address || u.RawQuery != "" || u.Fragment != "" {
			return ErrInvalid
		}
		if scheme == "opc.tcp" {
			if u.Scheme != scheme {
				return ErrInvalid
			}
		} else if (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" {
			return ErrInvalid
		}
	}
	return nil
}
func (r *Repository) BindMachine(ctx context.Context, project int, id string, revision int, m MachineBinding) (Device, error) {
	machineBindingMu.Lock()
	defer machineBindingMu.Unlock()
	d, e := r.Device(ctx, project, id)
	if e != nil {
		return d, e
	}
	if e = m.Validate(d.Address); e != nil {
		return d, e
	}
	if d.Revision != revision || d.ActiveOperationID != "" {
		return d, ErrConflict
	}
	if (d.AgentInstanceID != "" && d.AgentInstanceID != m.AgentInstanceID) || (d.GatewayInstanceID != "" && d.GatewayInstanceID != m.GatewayInstanceID) || (d.ManagerInstanceID != "" && d.ManagerInstanceID != m.ManagerInstanceID) {
		return d, fmt.Errorf("%w: 稳定身份不能随地址修改", ErrInvalid)
	}
	var count int
	items, e := r.Devices(ctx, project)
	if e != nil {
		return d, e
	}
	for _, other := range items {
		if other.ID != id && (other.AgentInstanceID == m.AgentInstanceID || other.GatewayInstanceID == m.GatewayInstanceID || other.ManagerInstanceID == m.ManagerInstanceID) {
			count++
		}
	}
	if count > 0 {
		return d, ErrConflict
	}
	d.OPCEndpoint, d.AgentURL, d.AgentInstanceID, d.GatewayInstanceID, d.ManagerInstanceID = m.OPCEndpoint, m.AgentURL, m.AgentInstanceID, m.GatewayInstanceID, m.ManagerInstanceID
	if m.Name != "" {
		d.Name = m.Name
	}
	return r.updateDeviceBinding(ctx, project, d, revision)
}
