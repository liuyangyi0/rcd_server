package domain

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	OperationsNamespaceURI  = "urn:SerialServer:operations"
	MaxOperationConfigBytes = 2 << 20
	MaxOperationLogBytes    = 64 << 10
	// A JSON string may expand one input byte to six escaped bytes.
	MaxOperationJSONBytes = 13 << 20
)

// OperationRequest is a bounded, library-independent lower operations request.
// SourceID is intentionally absent: Runtime pins that physical target before I/O.
type OperationRequest struct {
	Action                  string          `json:"action"`
	PortID                  string          `json:"port_id,omitempty"`
	DeviceID                string          `json:"device_id,omitempty"`
	ModuleType              string          `json:"module_type,omitempty"`
	Mode                    string          `json:"mode,omitempty"`
	LogLevel                string          `json:"log_level,omitempty"`
	Enabled                 *bool           `json:"enabled,omitempty"`
	ExpectedRevision        string          `json:"expected_revision,omitempty"`
	ExpectedStationRevision string          `json:"expected_station_revision,omitempty"`
	Name                    string          `json:"name,omitempty"`
	BackupID                string          `json:"backup_id,omitempty"`
	Content                 *string         `json:"content,omitempty"`
	Edits                   json.RawMessage `json:"edits,omitempty"`
	Generation              string          `json:"generation,omitempty"`
	Offset                  *int64          `json:"offset,omitempty"`
	Limit                   int64           `json:"limit,omitempty"`
	Query                   string          `json:"query,omitempty"`
	Level                   string          `json:"level,omitempty"`
	Tail                    bool            `json:"tail,omitempty"`
}

func (r OperationRequest) Mutates() bool {
	return r.Action == "set_port_enabled" || r.Action == "set_device_read_enabled" ||
		r.Action == "set_device_module_type" || r.Action == "query_device_version" ||
		r.Action == "set_calculation_mode" || r.Action == "set_logging_level" || r.Action == "write_config"
}

func (r OperationRequest) Feature() string {
	switch r.Action {
	case "status", "set_port_enabled", "set_device_read_enabled":
		return "acquisition"
	case "set_device_module_type", "query_device_version":
		return "station_versions"
	case "set_calculation_mode":
		return "calculation_mode"
	case "get_logging", "set_logging_level":
		return "logging_configuration"
	case "list_configs", "read_config", "validate_config", "write_config":
		return "configuration"
	case "prepare_config":
		return "configuration_forms"
	case "list_logs", "read_log":
		return "logs"
	default:
		return ""
	}
}

func (r OperationRequest) Validate() error {
	invalid := func(field, message string) error { return &FieldError{Field: field, Message: message} }
	if r.Feature() == "" {
		return invalid("action", "unsupported lower operation")
	}
	if r.Mutates() && !ValidOperationRevision(r.ExpectedRevision) {
		return invalid("revision", "a lower revision is required")
	}
	if r.Action == "set_port_enabled" || r.Action == "set_device_read_enabled" ||
		r.Action == "set_device_module_type" || r.Action == "query_device_version" {
		if strings.TrimSpace(r.PortID) == "" || len(r.PortID) > 1024 || strings.ContainsAny(r.PortID, "\x00\r\n") {
			return invalid("port_id", "invalid physical port identifier")
		}
	}
	if r.Action == "set_port_enabled" || r.Action == "set_device_read_enabled" {
		if r.Enabled == nil {
			return invalid("enabled", "a Boolean value is required")
		}
	}
	if r.Action == "set_device_read_enabled" || r.Action == "set_device_module_type" || r.Action == "query_device_version" {
		value, err := strconv.ParseUint(r.DeviceID, 10, 8)
		if err != nil || strconv.FormatUint(value, 10) != r.DeviceID {
			return invalid("device_id", "must be a canonical station address in 0..255")
		}
	}
	if r.Action == "set_device_module_type" || r.Action == "query_device_version" {
		if !ValidStationRevision(r.ExpectedStationRevision) {
			return invalid("station_revision", "an exact station version revision is required")
		}
	}
	if r.Action == "set_device_module_type" {
		if !ValidModuleType(r.ModuleType) {
			return invalid("module_type", "must be unknown, domestic or legacy_russian")
		}
	} else if r.ModuleType != "" {
		return invalid("module_type", "is only valid for set_device_module_type")
	}
	if r.Action == "set_calculation_mode" {
		if r.Mode != "local" && r.Mode != "manager" {
			return invalid("mode", "must be local or manager")
		}
	} else if r.Mode != "" {
		return invalid("mode", "is only valid for set_calculation_mode")
	}
	if r.Action == "set_logging_level" {
		switch strings.ToLower(r.LogLevel) {
		case "error", "warn", "info", "debug", "trace":
		default:
			return invalid("log_level", "must be error, warn, info, debug or trace")
		}
	} else if r.LogLevel != "" {
		return invalid("log_level", "is only valid for set_logging_level")
	}
	switch r.Action {
	case "read_config", "validate_config", "write_config", "prepare_config", "read_log":
		if !ValidOperationResourceName(r.Name) {
			return invalid("name", "select a listed resource name, not a path")
		}
	}
	if r.BackupID != "" && !ValidOperationResourceName(r.BackupID) {
		return invalid("backup_id", "invalid backup resource identifier")
	}
	if r.Action == "validate_config" || r.Action == "write_config" || r.Action == "prepare_config" {
		if r.Content == nil {
			return invalid("content", "configuration text is required")
		}
		if len(*r.Content) > MaxOperationConfigBytes {
			return &OperationError{Code: "too_large", Message: "配置文件不能超过 2 MiB"}
		}
		if !utf8.ValidString(*r.Content) || strings.ContainsRune(*r.Content, '\x00') {
			return invalid("content", "configuration must be UTF-8 text without NUL")
		}
	}
	if r.Action == "read_log" {
		if len(r.Edits) > 0 {
			return invalid("edits", "only supported for draft preparation")
		}
		if r.Offset != nil && *r.Offset < 0 {
			return invalid("offset", "must not be negative")
		}
		if r.Limit != 0 && (r.Limit < 4 || r.Limit > MaxOperationLogBytes) {
			return invalid("limit", "must be between 4 and 65536 bytes")
		}
		if len(r.Query) > 256 {
			return invalid("q", "must not exceed 256 bytes")
		}
		if len(r.Generation) > 512 || strings.ContainsAny(r.Generation, "\x00\r\n") {
			return invalid("generation", "invalid log generation")
		}
		switch strings.ToUpper(r.Level) {
		case "", "TRACE", "DEBUG", "INFO", "WARN", "ERROR":
		default:
			return invalid("level", "unsupported log level")
		}
	}
	if len(r.Edits) > 0 {
		var edits []json.RawMessage
		if r.Action != "prepare_config" || len(r.Edits) > MaxOperationConfigBytes || json.Unmarshal(r.Edits, &edits) != nil || len(edits) > 4096 {
			return invalid("edits", "expected a bounded configuration draft edit array")
		}
	}
	return nil
}

func ValidOperationResourceName(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 && path.Base(value) == value && !strings.ContainsAny(value, "/\\:\x00") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func ValidOperationRevision(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ':' || c == '_' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

type OperationsCapabilities struct {
	SourceID           string   `json:"source_id"`
	SourceRevision     uint64   `json:"source_revision"`
	Supported          bool     `json:"supported"`
	Reason             string   `json:"reason,omitempty"`
	SchemaVersion      uint32   `json:"schema_version"`
	Features           []string `json:"features"`
	MaxConfigBytes     int64    `json:"max_config_bytes"`
	MaxLogBytes        int64    `json:"max_log_bytes"`
	ConfigurationApply string   `json:"configuration_apply"`
}

func (c OperationsCapabilities) HasFeature(feature string) bool {
	for _, item := range c.Features {
		if item == feature {
			return true
		}
	}
	return false
}

// OperationError preserves a lower business failure without treating it as a
// connection failure. No config/log body belongs in this error or its audit.
type OperationError struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	CurrentRevision string `json:"current_revision,omitempty"`
}

func (e *OperationError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
