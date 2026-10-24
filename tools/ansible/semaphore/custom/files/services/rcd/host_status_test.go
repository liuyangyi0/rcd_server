// SPDX-License-Identifier: MIT
package rcd

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestRCDHostIPv4InterfaceAddressContract(t *testing.T) {
	for _, value := range []string{"192.168.10.46", "192.168.10.46/24", "128.128.0.1/16", "0.0.0.0/0", "255.255.255.255/32"} {
		if !validHostIPv4(value) {
			t.Errorf("valid IPv4 interface address rejected: %q", value)
		}
	}
	for _, value := range []string{"", "not-an-ip", "192.168.10.46/33", "192.168.10.46/-1", "192.168.10.46/abc", "192.168.10.46/24/1", "256.168.10.46/24", "::1", "::ffff:192.168.10.46", "::ffff:192.168.10.46/120", "2001:db8::1/64", " 192.168.10.46/24"} {
		if validHostIPv4(value) {
			t.Errorf("invalid IPv4 interface address accepted: %q", value)
		}
	}
}

func TestRCDHostMetricsProducerGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/host_status_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	h := (InspectionReport{HostStatus: raw}).HostObservation()
	if h == nil || !h.Available || !h.CPU.Available || !h.Memory.Available || len(h.Disks) != 2 || len(h.Network) != 1 {
		t.Fatalf("producer report rejected or metrics lost: %+v", h)
	}
	if len(h.Network[0].IPv4Addresses) != 2 || h.Network[0].IPv4Addresses[0] != "192.0.2.46/24" || h.Network[0].IPv4Addresses[1] != "198.51.100.46/32" {
		t.Fatalf("interface prefix information lost: %+v", h.Network)
	}
}

func TestRCDHostMetricsAreOptionalAndValidated(t *testing.T) {
	if (InspectionReport{}).HostObservation() != nil {
		t.Fatal("legacy report invented metrics")
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	report := InspectionReport{HostStatus: json.RawMessage(`{"schema_version":1,"observed_at":"` + at + `","available":true,"cpu":{"available":true,"utilization_percent":0},"memory":{"available":false},"network":[{"name":"eth0","available":false,"ipv4_addresses":["192.168.10.46"],"rx_bytes_per_second":null}]}`)}
	h := report.HostObservation()
	if h == nil || !h.Available || h.CPU.UtilizationPercent == nil || *h.CPU.UtilizationPercent != 0 || h.Network[0].RXBytesPerSecond != nil {
		t.Fatal(h)
	}
	for _, raw := range []string{
		`{"schema_version":1,"observed_at":"` + at + `","available":true,"cpu":{"utilization_percent":101}}`,
		`{"schema_version":1,"observed_at":"` + at + `","network":[{"name":"eth0","ipv4_addresses":["not-an-ip"]}]}`,
		`{"schema_version":1,"observed_at":"` + at + `","network":[{"name":"eth0"},{"name":"eth0"}]}`,
		`{"schema_version":1,"observed_at":"` + at + `","memory":{"available":true,"total_bytes":-1}}`,
		`{"schema_version":2,"observed_at":"` + at + `"}`, `{"cpu":`,
	} {
		h = (InspectionReport{HostStatus: json.RawMessage(raw)}).HostObservation()
		if h == nil || h.Available || h.Error == "" {
			t.Fatalf("invalid metrics accepted: %s", raw)
		}
	}
}

func TestRCDFourthAndRegisteredComponentEvidence(t *testing.T) {
	no := false
	d := Device{ID: "device", DeviceBinding: DeviceBinding{HostAlias: "host"}}
	report := InspectionReport{SchemaVersion: 1, DeviceID: d.ID, TaskID: 9, HostAlias: d.HostAlias, Probes: []RuntimeProbe{
		{SchemaVersion: 1, Component: "manager", Processes: []RuntimeProcess{{Component: "manager", Running: &no}}},
		{SchemaVersion: 1, Component: "lower-stack", Processes: []RuntimeProcess{{Component: "agent", Running: &no}, {Component: "lower"}}},
		{SchemaVersion: 1, Component: "communicate", Processes: []RuntimeProcess{{Component: "communicate", Running: &no, Installed: &no}}},
		{SchemaVersion: 1, Component: "sample-app", Processes: []RuntimeProcess{{Component: "sample-app", Running: &no, Installed: &no}}},
	}}
	report.Probes[2].Service.LoadState = "not-found"
	report.Probes[3].Service.LoadState = "not-found"
	result, err := report.Normalize(d, 9)
	if err != nil || len(result) != 5 || result[3].State != "not_installed" || result[4].State != "not_installed" {
		t.Fatal(result, err)
	}
	if report.ValidateApplications(nil) == nil {
		t.Fatal("unregistered app accepted")
	}
	if err = report.ValidateApplications([]Application{{Component: "sample-app"}}); err != nil {
		t.Fatal(err)
	}
	report.Probes[2].Processes[0].EntryError = "permission denied"
	result, err = report.Normalize(d, 9)
	if err != nil || result[3].State == "not_installed" {
		t.Fatal("permission denied treated as absent", result, err)
	}
	report.Probes[3].Processes = nil
	if _, err = report.Normalize(d, 9); err == nil {
		t.Fatal("empty app evidence accepted")
	}
}
