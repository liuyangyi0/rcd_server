// SPDX-License-Identifier: MIT
package rcd

import (
	"encoding/json"
	"math"
	"net/netip"
	"path/filepath"
	"strings"
	"time"
)

// Host readings belong to one successful native inspection, not to an OPC
// source. Nil metrics are unknown; a valid zero must remain distinguishable.
type HostStatus struct {
	SchemaVersion         int           `json:"schema_version"`
	ObservedAt            string        `json:"observed_at"`
	LastAttemptAt         string        `json:"last_attempt_at,omitempty"`
	Available             bool          `json:"available"`
	Error                 string        `json:"error"`
	SampleIntervalSeconds *float64      `json:"sample_interval_seconds"`
	UptimeSeconds         *float64      `json:"uptime_seconds"`
	CPU                   HostCPU       `json:"cpu"`
	Memory                HostMemory    `json:"memory"`
	Disks                 []HostDisk    `json:"disks"`
	Network               []HostNetwork `json:"network"`
}
type HostCPU struct {
	Available          bool     `json:"available"`
	Error              string   `json:"error"`
	UtilizationPercent *float64 `json:"utilization_percent"`
	LogicalCount       *int     `json:"logical_count"`
}
type HostMemory struct {
	Available      bool     `json:"available"`
	Error          string   `json:"error"`
	TotalBytes     *uint64  `json:"total_bytes"`
	AvailableBytes *uint64  `json:"available_bytes"`
	UsedBytes      *uint64  `json:"used_bytes"`
	UsedPercent    *float64 `json:"used_percent"`
}
type HostDisk struct {
	HostMemory
	Path       string `json:"path"`
	MountPoint string `json:"mount_point"`
	Filesystem string `json:"filesystem"`
}
type HostNetwork struct {
	Name             string   `json:"name"`
	Operstate        string   `json:"operstate"`
	LinkUp           *bool    `json:"link_up"`
	IPv4Addresses    []string `json:"ipv4_addresses"`
	AddressesError   string   `json:"addresses_error"`
	Available        bool     `json:"available"`
	Error            string   `json:"error"`
	RXBytes          *uint64  `json:"rx_bytes"`
	TXBytes          *uint64  `json:"tx_bytes"`
	RXBytesPerSecond *float64 `json:"rx_bytes_per_second"`
	TXBytesPerSecond *float64 `json:"tx_bytes_per_second"`
}

func metricNumber(v *float64, maximum float64) bool {
	return v == nil || (!math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 && *v <= maximum)
}
func validHostMemory(v HostMemory) bool {
	if len(v.Error) > 1024 || !metricNumber(v.UsedPercent, 100) {
		return false
	}
	if v.TotalBytes != nil {
		if v.AvailableBytes != nil && *v.AvailableBytes > *v.TotalBytes {
			return false
		}
		if v.UsedBytes != nil && *v.UsedBytes > *v.TotalBytes {
			return false
		}
	}
	return !v.Available || (v.TotalBytes != nil && v.AvailableBytes != nil && v.UsedBytes != nil && v.UsedPercent != nil)
}

// Inventory reports interface addresses with their prefix length. Retain that
// information while accepting older bare IPv4 reports; do not accept IPv6 or
// invalid masks as IPv4 merely because the address portion looks familiar.
func validHostIPv4(value string) bool {
	if len(value) > 64 {
		return false
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Is4()
	}
	prefix, err := netip.ParsePrefix(value)
	return err == nil && prefix.Addr().Is4()
}

func (h *HostStatus) valid() bool {
	if h == nil || h.SchemaVersion != 1 || len(h.Error) > 2048 || len(h.CPU.Error) > 1024 || len(h.Disks) > 32 || len(h.Network) > 128 {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, h.ObservedAt)
	if err != nil || at.After(time.Now().Add(5*time.Minute)) {
		return false
	}
	if !metricNumber(h.SampleIntervalSeconds, 60) || !metricNumber(h.UptimeSeconds, 1e12) || !metricNumber(h.CPU.UtilizationPercent, 100) || !validHostMemory(h.Memory) {
		return false
	}
	if h.CPU.LogicalCount != nil && (*h.CPU.LogicalCount < 1 || *h.CPU.LogicalCount > 1048576) {
		return false
	}
	if h.CPU.Available && h.CPU.UtilizationPercent == nil {
		return false
	}
	for _, d := range h.Disks {
		if !filepath.IsAbs(d.Path) || len(d.Path) > 4096 || len(d.MountPoint) > 4096 || len(d.Filesystem) > 128 || !validHostMemory(d.HostMemory) {
			return false
		}
	}
	seen := map[string]bool{}
	for _, n := range h.Network {
		if len(n.Name) < 1 || len(n.Name) > 128 || strings.ContainsAny(n.Name, "/\x00\r\n") || seen[n.Name] || len(n.Operstate) > 32 || len(n.Error) > 1024 || len(n.AddressesError) > 1024 || len(n.IPv4Addresses) > 32 || !metricNumber(n.RXBytesPerSecond, math.MaxFloat64) || !metricNumber(n.TXBytesPerSecond, math.MaxFloat64) {
			return false
		}
		seen[n.Name] = true
		if n.Available && (n.RXBytes == nil || n.TXBytes == nil) {
			return false
		}
		for _, ip := range n.IPv4Addresses {
			if !validHostIPv4(ip) {
				return false
			}
		}
	}
	return true
}

// Malformed optional metrics do not discard trustworthy software observations.
func (report InspectionReport) HostObservation() *HostStatus {
	raw := report.HostStatus
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var h HostStatus
	if len(raw) > 128<<10 || json.Unmarshal(raw, &h) != nil || !h.valid() {
		return &HostStatus{SchemaVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: "主机指标报告无效，未将未知数值当作0"}
	}
	return &h
}
