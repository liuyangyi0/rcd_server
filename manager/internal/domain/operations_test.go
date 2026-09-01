package domain

import (
	"strings"
	"testing"
)

func TestOperationRequestBoundaries(t *testing.T) {
	enabled := false
	content := "[serial]\n"
	valid := []OperationRequest{
		{Action: "status"}, {Action: "list_configs"}, {Action: "list_logs"},
		{Action: "set_port_enabled", PortID: "/dev/ttyUSB0", Enabled: &enabled, ExpectedRevision: "0"},
		{Action: "set_device_read_enabled", PortID: "COM1", DeviceID: "255", Enabled: &enabled, ExpectedRevision: "12"},
		{Action: "set_device_module_type", PortID: "COM1", DeviceID: "12", ModuleType: ModuleTypeDomestic, ExpectedRevision: "12", ExpectedStationRevision: "8"},
		{Action: "query_device_version", PortID: "COM1", DeviceID: "12", ExpectedRevision: "12", ExpectedStationRevision: "8"},
		{Action: "write_config", Name: "config.toml", Content: &content, ExpectedRevision: strings.Repeat("a", 64)},
		{Action: "read_config", Name: "rcd01.csv", BackupID: "backup-123.sha256"},
		{Action: "read_log", Name: "rcd_server_r2026-08-28.log", Limit: 65536, Level: "WARN"},
	}
	for _, request := range valid {
		if err := request.Validate(); err != nil {
			t.Errorf("valid %+v: %v", request, err)
		}
	}
	oversize := strings.Repeat("a", MaxOperationConfigBytes+1)
	invalid := []OperationRequest{
		{Action: "restart"},
		{Action: "set_port_enabled", PortID: "COM1", Enabled: &enabled},
		{Action: "set_port_enabled", PortID: "COM1", ExpectedRevision: "1"},
		{Action: "set_port_enabled", PortID: " ", Enabled: &enabled, ExpectedRevision: "1"},
		{Action: "set_device_read_enabled", PortID: "COM1", DeviceID: "01", Enabled: &enabled, ExpectedRevision: "1"},
		{Action: "set_device_read_enabled", PortID: "COM1", DeviceID: "256", Enabled: &enabled, ExpectedRevision: "1"},
		{Action: "set_device_module_type", PortID: "COM1", DeviceID: "1", ModuleType: "automatic", ExpectedRevision: "1", ExpectedStationRevision: "1"},
		{Action: "query_device_version", PortID: "COM1", DeviceID: "1", ExpectedRevision: "1"},
		{Action: "read_config", Name: "../config.toml"},
		{Action: "read_config", Name: "C:\\config.toml"},
		{Action: "read_config", Name: "config.toml", BackupID: "../backup"},
		{Action: "validate_config", Name: "config.toml"},
		{Action: "validate_config", Name: "config.toml", Content: &oversize},
		{Action: "read_log", Name: "log.txt", Limit: 65537},
		{Action: "read_log", Name: "log.txt", Level: "ANY"},
	}
	for _, request := range invalid {
		if err := request.Validate(); err == nil {
			t.Errorf("invalid action=%s name=%s accepted", request.Action, request.Name)
		}
	}
}

func TestOperationRevisionsAreExactOpaqueTokens(t *testing.T) {
	for _, value := range []string{"0", "45", "sha256:abc-._09"} {
		if !ValidOperationRevision(value) {
			t.Error(value)
		}
	}
	for _, value := range []string{"", "*", "W/1", "1,2", "a\r\nInjected", strings.Repeat("a", 129), "\"1\""} {
		if ValidOperationRevision(value) {
			t.Error(value)
		}
	}
}
