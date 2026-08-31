package opc

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"rcd_manager/internal/domain"
)

type operationResultObject map[string]json.RawMessage

// Validate the action's result before reporting a successful write or handing
// data to the Web client. Unknown extension fields remain forward compatible.
// Validation failures are uncertain outcomes, never reasons to retry a mutation.
func validateOperationResult(request domain.OperationRequest, result json.RawMessage) error {
	if len(result) > domain.MaxOperationJSONBytes || !utf8.Valid(result) {
		return invalidOperationField("result")
	}
	object, err := operationObject(result)
	if err != nil {
		return err
	}
	switch request.Action {
	case "status", "set_port_enabled", "set_device_read_enabled":
		return validateAcquisitionResult(request, object)
	case "list_configs":
		items, err := operationArray(object, "items")
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(items))
		for _, item := range items {
			entry, err := operationObject(item)
			if err != nil {
				return err
			}
			name, _, err := validateConfigMetadata(entry)
			if err != nil {
				return err
			}
			if seen[name] {
				return invalidOperationField("items.name")
			}
			seen[name] = true
		}
		return nil
	case "read_config", "write_config":
		return validateConfigDocument(request, object)
	case "validate_config":
		return validateConfigValidation(object)
	case "list_logs":
		return validateLogList(object)
	case "read_log":
		return validateLogChunk(request, object)
	default:
		return invalidOperationField("action")
	}
}

func invalidOperationField(field string) error {
	return invalidOperationsResponse("下级运维结果字段 " + field + " 缺失、类型错误或超出范围")
}

func operationObject(raw json.RawMessage) (operationResultObject, error) {
	var object operationResultObject
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, invalidOperationField("object")
	}
	return object, nil
}

func operationField(object operationResultObject, key string, target any) error {
	raw, exists := object[key]
	if !exists || strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, target) != nil {
		return invalidOperationField(key)
	}
	return nil
}

func operationOptionalField(object operationResultObject, key string, target any) error {
	if raw, exists := object[key]; exists && strings.TrimSpace(string(raw)) != "null" {
		return operationField(object, key, target)
	}
	return nil
}

func operationArray(object operationResultObject, key string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := operationField(object, key, &items); err != nil {
		return nil, err
	}
	if items == nil {
		return nil, invalidOperationField(key)
	}
	return items, nil
}

func operationString(object operationResultObject, key string, maxBytes int, empty bool) (string, error) {
	var value string
	if err := operationField(object, key, &value); err != nil {
		return "", err
	}
	if len(value) > maxBytes || (!empty && value == "") || !utf8.ValidString(value) {
		return "", invalidOperationField(key)
	}
	return value, nil
}

func operationRevision(object operationResultObject, key string, empty bool) (string, error) {
	value, err := operationString(object, key, 128, empty)
	if err != nil {
		return "", err
	}
	if value != "" && !domain.ValidOperationRevision(value) {
		return "", invalidOperationField(key)
	}
	return value, nil
}

func operationName(object operationResultObject, key string) (string, error) {
	value, err := operationString(object, key, 255, false)
	if err != nil {
		return "", err
	}
	if !domain.ValidOperationResourceName(value) {
		return "", invalidOperationField(key)
	}
	return value, nil
}

func operationTime(object operationResultObject, key string, optional bool) error {
	if raw, exists := object[key]; optional && (!exists || strings.TrimSpace(string(raw)) == "null") {
		return nil
	}
	value, err := operationString(object, key, 64, false)
	if err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return invalidOperationField(key)
	}
	return nil
}

func validateAcquisitionResult(request domain.OperationRequest, object operationResultObject) error {
	var version uint32
	if err := operationField(object, "schema_version", &version); err != nil {
		return err
	}
	if version != 1 {
		return invalidOperationField("schema_version")
	}
	if _, err := operationRevision(object, "revision", false); err != nil {
		return err
	}
	if request.Mutates() {
		status, err := operationString(object, "status", 16, false)
		if err != nil {
			return err
		}
		if status != "Applied" && status != "Pending" {
			return invalidOperationField("status")
		}
		if _, exists := object["operation_revision"]; exists {
			if _, err := operationRevision(object, "operation_revision", false); err != nil {
				return err
			}
		}
	}
	ports, err := operationArray(object, "ports")
	if err != nil {
		return err
	}
	seenPorts := make(map[string]bool, len(ports))
	for _, rawPort := range ports {
		port, err := operationObject(rawPort)
		if err != nil {
			return err
		}
		id, err := operationString(port, "port_id", 1024, false)
		if err != nil {
			return err
		}
		if strings.TrimSpace(id) == "" || strings.IndexFunc(id, unicode.IsControl) >= 0 || seenPorts[id] {
			return invalidOperationField("port_id")
		}
		seenPorts[id] = true
		if _, err := operationString(port, "port_name", 1024, true); err != nil {
			return err
		}
		for _, field := range []string{"enabled", "applied_enabled", "open", "control_pending"} {
			var value bool
			if err := operationField(port, field, &value); err != nil {
				return err
			}
		}
		var pending uint64
		if err := operationField(port, "pending_commands", &pending); err != nil {
			return err
		}
		var controlError string
		if err := operationOptionalField(port, "last_control_error", &controlError); err != nil {
			return err
		}
		if len(controlError) > 16384 {
			return invalidOperationField("last_control_error")
		}
		devices, err := operationArray(port, "devices")
		if err != nil {
			return err
		}
		seenDevices := make(map[string]bool, len(devices))
		for _, rawDevice := range devices {
			device, err := operationObject(rawDevice)
			if err != nil {
				return err
			}
			id, err := operationString(device, "device_id", 3, false)
			if err != nil {
				return err
			}
			address, parseErr := strconv.ParseUint(id, 10, 8)
			if parseErr != nil || strconv.FormatUint(address, 10) != id || seenDevices[id] {
				return invalidOperationField("device_id")
			}
			seenDevices[id] = true
			if _, err := operationString(device, "name", 1024, true); err != nil {
				return err
			}
			for _, field := range []string{"read_enabled", "applied_read_enabled", "communication_ok"} {
				var value bool
				if err := operationField(device, field, &value); err != nil {
					return err
				}
			}
			if err := operationTime(device, "last_data_at", true); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateConfigMetadata(object operationResultObject) (string, uint64, error) {
	name, err := operationName(object, "name")
	if err != nil {
		return "", 0, err
	}
	if _, err := operationString(object, "kind", 128, false); err != nil {
		return "", 0, err
	}
	var size uint64
	if err := operationField(object, "size_bytes", &size); err != nil {
		return "", 0, err
	}
	if size > domain.MaxOperationConfigBytes {
		return "", 0, invalidOperationField("size_bytes")
	}
	revision, err := operationRevision(object, "revision", false)
	if err != nil {
		return "", 0, err
	}
	active, err := operationRevision(object, "active_revision", true)
	if err != nil {
		return "", 0, err
	}
	var restart bool
	if err := operationField(object, "restart_required", &restart); err != nil {
		return "", 0, err
	}
	if restart != (active != revision) {
		return "", 0, invalidOperationField("restart_required")
	}
	return name, size, nil
}

func validateConfigDocument(request domain.OperationRequest, object operationResultObject) error {
	name, size, err := validateConfigMetadata(object)
	if err != nil {
		return err
	}
	if name != request.Name {
		return invalidOperationField("name")
	}
	content, err := operationString(object, "content", domain.MaxOperationConfigBytes, true)
	if err != nil {
		return err
	}
	var preview string
	if err := operationOptionalField(object, "viewing_backup_id", &preview); err != nil {
		return err
	}
	if preview != request.BackupID {
		return invalidOperationField("viewing_backup_id")
	}
	if preview != "" {
		if !domain.ValidOperationResourceName(preview) {
			return invalidOperationField("viewing_backup_id")
		}
		if _, err := operationRevision(object, "backup_revision", false); err != nil {
			return err
		}
	} else if size != uint64(len(content)) {
		return invalidOperationField("size_bytes")
	}
	if request.Action == "write_config" && request.Content != nil && content != *request.Content {
		return invalidOperationField("content")
	}
	backups, err := operationArray(object, "backups")
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(backups))
	for _, raw := range backups {
		backup, err := operationObject(raw)
		if err != nil {
			return err
		}
		id, err := operationName(backup, "id")
		if err != nil {
			return err
		}
		if seen[id] {
			return invalidOperationField("backups.id")
		}
		seen[id] = true
		var bytes uint64
		if err := operationField(backup, "size_bytes", &bytes); err != nil {
			return err
		}
		if bytes > domain.MaxOperationConfigBytes {
			return invalidOperationField("backups.size_bytes")
		}
		if err := operationTime(backup, "created_at", false); err != nil {
			return err
		}
	}
	var truncated bool
	return operationOptionalField(object, "backups_truncated", &truncated)
}

func validateConfigValidation(object operationResultObject) error {
	var valid, restart bool
	if err := operationField(object, "valid", &valid); err != nil {
		return err
	}
	if err := operationField(object, "restart_required", &restart); err != nil {
		return err
	}
	if !restart {
		return invalidOperationField("restart_required")
	}
	for _, key := range []string{"errors", "warnings"} {
		messages, err := operationArray(object, key)
		if err != nil {
			return err
		}
		total := 0
		for _, message := range messages {
			var text string
			if strings.TrimSpace(string(message)) == "null" || json.Unmarshal(message, &text) != nil {
				return invalidOperationField(key)
			}
			total += len(text)
			if total > 16384 {
				return invalidOperationField(key)
			}
		}
		if key == "errors" && valid != (len(messages) == 0) {
			return invalidOperationField("valid")
		}
	}
	for _, key := range []string{"errors_truncated", "warnings_truncated"} {
		var truncated bool
		if err := operationOptionalField(object, key, &truncated); err != nil {
			return err
		}
	}
	return nil
}

func operationGeneration(object operationResultObject) (string, error) {
	value, err := operationString(object, "generation", 512, false)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", invalidOperationField("generation")
	}
	return value, nil
}

func validateLogList(object operationResultObject) error {
	items, err := operationArray(object, "items")
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		item, err := operationObject(raw)
		if err != nil {
			return err
		}
		name, err := operationName(item, "name")
		if err != nil {
			return err
		}
		if seen[name] {
			return invalidOperationField("items.name")
		}
		seen[name] = true
		var size uint64
		if err := operationField(item, "size_bytes", &size); err != nil {
			return err
		}
		if size > math.MaxInt64 {
			return invalidOperationField("size_bytes")
		}
		if err := operationTime(item, "modified_at", false); err != nil {
			return err
		}
		if _, err := operationGeneration(item); err != nil {
			return err
		}
	}
	return nil
}

func validateLogChunk(request domain.OperationRequest, object operationResultObject) error {
	name, err := operationName(object, "name")
	if err != nil {
		return err
	}
	if name != request.Name {
		return invalidOperationField("name")
	}
	generation, err := operationGeneration(object)
	if err != nil {
		return err
	}
	if request.Generation != "" && request.Generation != generation {
		return invalidOperationField("generation")
	}
	var offset, next, size uint64
	for key, target := range map[string]*uint64{"offset": &offset, "next_offset": &next, "size_bytes": &size} {
		if err := operationField(object, key, target); err != nil {
			return err
		}
	}
	var eof, truncated bool
	if err := operationField(object, "eof", &eof); err != nil {
		return err
	}
	if err := operationField(object, "truncated", &truncated); err != nil {
		return err
	}
	if size > math.MaxInt64 || next < offset || next > size || next-offset > 256<<10 || eof != (next == size) || (!eof && next == offset) {
		return invalidOperationField("offset/next_offset/size_bytes/eof")
	}
	if !request.Tail {
		requested := uint64(0)
		if request.Offset != nil {
			if *request.Offset < 0 {
				return invalidOperationField("offset")
			}
			requested = uint64(*request.Offset)
		}
		// An explicit byte cursor can point into a UTF-8 continuation sequence.
		// Rust skips at most three bytes to start at a complete character.
		if offset < requested || offset-requested > 3 {
			return invalidOperationField("offset")
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = domain.MaxOperationLogBytes
	}
	if limit < 4 || limit > domain.MaxOperationLogBytes {
		return invalidOperationField("limit")
	}
	_, err = operationString(object, "text", int(limit), true)
	return err
}
