package opc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"rcd_manager/internal/domain"
)

func acquisitionResultFixture() map[string]any {
	return map[string]any{
		"schema_version": 1, "revision": "4", "status": "Pending", "operation_revision": "3",
		"ports": []any{map[string]any{
			"port_id": "/dev/ttyS0", "port_name": "ttyS0", "enabled": true,
			"applied_enabled": true, "open": false, "pending_commands": 0, "control_pending": true,
			"devices": []any{map[string]any{
				"device_id": "0", "name": "设备 0", "read_enabled": false,
				"applied_read_enabled": true, "communication_ok": false,
				"last_data_at": "2026-08-28T01:02:03.123456789+08:00",
			}},
		}},
	}
}

func configResultFixture() map[string]any {
	return map[string]any{
		"name": "calc.toml", "kind": "calculation", "size_bytes": 2,
		"revision": "r2", "active_revision": "r1", "restart_required": true,
		"content": "#x", "backups": []any{},
	}
}

func logResultFixture() map[string]any {
	return map[string]any{
		"name": "rcd_current.log", "generation": "generation-1", "offset": 0,
		"next_offset": 20, "size_bytes": 1 << 40, "eof": false, "truncated": true,
		"text": "[INFO ] 中文日志\n",
	}
}

func assertOperationResult(t *testing.T, request domain.OperationRequest, value any, valid bool) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	err = validateOperationResult(request, raw)
	if valid {
		if err != nil {
			t.Fatalf("valid %s rejected: %v", request.Action, err)
		}
		return
	}
	var operationError *domain.OperationError
	if !errors.As(err, &operationError) || operationError.Code != "invalid_response" {
		t.Fatalf("bad %s result must be invalid_response, got %v", request.Action, err)
	}
}

func TestOperationResultAcquisitionSnapshotsAndEmptyArrays(t *testing.T) {
	for _, action := range []string{"status", "set_port_enabled", "set_device_read_enabled"} {
		request := domain.OperationRequest{Action: action}
		value := acquisitionResultFixture()
		value["future_extension"] = map[string]any{"accepted": true}
		assertOperationResult(t, request, value, true)
		value["ports"] = []any{}
		delete(value, "operation_revision") // Existing schema-v1 stubs may omit this extension.
		assertOperationResult(t, request, value, true)
	}
	value := acquisitionResultFixture()
	delete(value, "status")
	assertOperationResult(t, domain.OperationRequest{Action: "status"}, value, true)
	assertOperationResult(t, domain.OperationRequest{Action: "set_port_enabled"}, value, false)
}

func TestOperationResultRejectsMalformedAcquisition(t *testing.T) {
	port := func(value map[string]any) map[string]any { return value["ports"].([]any)[0].(map[string]any) }
	device := func(value map[string]any) map[string]any { return port(value)["devices"].([]any)[0].(map[string]any) }
	cases := map[string]func(map[string]any){
		"missing revision":           func(v map[string]any) { delete(v, "revision") },
		"null revision":              func(v map[string]any) { v["revision"] = nil },
		"wildcard revision":          func(v map[string]any) { v["revision"] = "*" },
		"non string revision":        func(v map[string]any) { v["revision"] = 4 },
		"wrong schema":               func(v map[string]any) { v["schema_version"] = 2 },
		"nil ports":                  func(v map[string]any) { v["ports"] = nil },
		"object ports":               func(v map[string]any) { v["ports"] = map[string]any{} },
		"null port":                  func(v map[string]any) { v["ports"] = []any{nil} },
		"missing devices":            func(v map[string]any) { delete(port(v), "devices") },
		"nil devices":                func(v map[string]any) { port(v)["devices"] = nil },
		"null device":                func(v map[string]any) { port(v)["devices"] = []any{nil} },
		"non canonical device":       func(v map[string]any) { device(v)["device_id"] = "00" },
		"device out of range":        func(v map[string]any) { device(v)["device_id"] = "256" },
		"boolean string":             func(v map[string]any) { port(v)["enabled"] = "true" },
		"null boolean":               func(v map[string]any) { device(v)["communication_ok"] = nil },
		"negative pending":           func(v map[string]any) { port(v)["pending_commands"] = -1 },
		"bad time":                   func(v map[string]any) { device(v)["last_data_at"] = "yesterday" },
		"invalid status":             func(v map[string]any) { v["status"] = "Success" },
		"null status":                func(v map[string]any) { v["status"] = nil },
		"invalid operation revision": func(v map[string]any) { v["operation_revision"] = "1,2" },
		"duplicate ports":            func(v map[string]any) { v["ports"] = []any{port(v), port(v)} },
		"duplicate devices":          func(v map[string]any) { port(v)["devices"] = []any{device(v), device(v)} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			value := acquisitionResultFixture()
			change(value)
			assertOperationResult(t, domain.OperationRequest{Action: "set_port_enabled"}, value, false)
		})
	}
}

func TestOperationResultConfigDocumentAndBackupUseCurrentMetadata(t *testing.T) {
	request := domain.OperationRequest{Action: "read_config", Name: "calc.toml"}
	value := configResultFixture()
	assertOperationResult(t, request, value, true)
	value["content"] = strings.Repeat("x", domain.MaxOperationConfigBytes)
	value["size_bytes"] = domain.MaxOperationConfigBytes
	value["active_revision"] = "" // File first appeared after this process started.
	assertOperationResult(t, request, value, true)
	value["content"] = "a differently sized backup draft"
	value["viewing_backup_id"], value["backup_revision"] = "backup-1", "r0"
	value["backups"] = []any{map[string]any{"id": "backup-1", "created_at": "2026-08-28T00:00:00Z", "size_bytes": 32}}
	value["backups_truncated"] = true
	request.BackupID = "backup-1"
	assertOperationResult(t, request, value, true)
	// The current metadata's size/revision must not be confused with backup content.
	value["revision"], value["active_revision"], value["restart_required"] = "r2", "r2", false
	assertOperationResult(t, request, value, true)
}

func TestOperationResultCalculationModeValidatesPinnedConfigRevision(t *testing.T) {
	config := configResultFixture()
	config["name"], config["kind"] = "config.toml", "software"
	value := map[string]any{
		"status": "Saved", "revision": "r2", "active_mode": "local",
		"configured_mode": "manager", "restart_required": true, "config": config,
	}
	request := domain.OperationRequest{Action: "set_calculation_mode", Mode: "manager"}
	assertOperationResult(t, request, value, true)

	for name, change := range map[string]func(map[string]any){
		"wrong configured mode": func(v map[string]any) { v["configured_mode"] = "local" },
		"wrong restart state":   func(v map[string]any) { v["restart_required"] = false },
		"wrong top revision":    func(v map[string]any) { v["revision"] = "r3" },
		"missing config":        func(v map[string]any) { delete(v, "config") },
	} {
		t.Run(name, func(t *testing.T) {
			copy := map[string]any{}
			for key, item := range value {
				copy[key] = item
			}
			change(copy)
			assertOperationResult(t, request, copy, false)
		})
	}
}

func TestOperationResultRejectsMalformedConfigAndUnconfirmedWrites(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong name":          func(v map[string]any) { v["name"] = "config.toml" },
		"path name":           func(v map[string]any) { v["name"] = "../calc.toml" },
		"oversize content":    func(v map[string]any) { v["content"] = strings.Repeat("x", domain.MaxOperationConfigBytes+1) },
		"oversize metadata":   func(v map[string]any) { v["size_bytes"] = domain.MaxOperationConfigBytes + 1 },
		"null content":        func(v map[string]any) { v["content"] = nil },
		"missing content":     func(v map[string]any) { delete(v, "content") },
		"length mismatch":     func(v map[string]any) { v["size_bytes"] = 1 },
		"nil backups":         func(v map[string]any) { v["backups"] = nil },
		"null backup":         func(v map[string]any) { v["backups"] = []any{nil} },
		"false restart":       func(v map[string]any) { v["restart_required"] = false },
		"bad active revision": func(v map[string]any) { v["active_revision"] = "bad\nrevision" },
		"unexpected preview":  func(v map[string]any) { v["viewing_backup_id"] = "backup-1" },
		"bad truncation flag": func(v map[string]any) { v["backups_truncated"] = "true" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			value := configResultFixture()
			change(value)
			assertOperationResult(t, domain.OperationRequest{Action: "read_config", Name: "calc.toml"}, value, false)
		})
	}
	content := "#x"
	request := domain.OperationRequest{Action: "write_config", Name: "calc.toml", Content: &content}
	assertOperationResult(t, request, configResultFixture(), true)
	content = "#different draft"
	assertOperationResult(t, request, configResultFixture(), false)
	request = domain.OperationRequest{Action: "read_config", Name: "calc.toml", BackupID: "backup-1"}
	assertOperationResult(t, request, configResultFixture(), false)
	value := configResultFixture()
	value["viewing_backup_id"] = "backup-1"
	assertOperationResult(t, request, value, false) // A preview must identify its own revision too.
}

func TestOperationResultMetadataListsAndValidationDiagnostics(t *testing.T) {
	assertOperationResult(t, domain.OperationRequest{Action: "list_configs"}, map[string]any{"items": []any{configResultFixture()}}, true)
	assertOperationResult(t, domain.OperationRequest{Action: "list_configs"}, map[string]any{"items": []any{}}, true)
	assertOperationResult(t, domain.OperationRequest{Action: "list_configs"}, map[string]any{"items": []any{configResultFixture(), configResultFixture()}}, false)
	log := map[string]any{"name": "rcd.log", "size_bytes": 1 << 40, "modified_at": "2026-08-28T00:00:00Z", "generation": "generation-1"}
	assertOperationResult(t, domain.OperationRequest{Action: "list_logs"}, map[string]any{"items": []any{log}}, true)
	for _, action := range []string{"list_configs", "list_logs"} {
		for _, items := range []any{nil, map[string]any{}, []any{nil}} {
			assertOperationResult(t, domain.OperationRequest{Action: action}, map[string]any{"items": items}, false)
		}
	}
	log["size_bytes"] = -1
	assertOperationResult(t, domain.OperationRequest{Action: "list_logs"}, map[string]any{"items": []any{log}}, false)
	request := domain.OperationRequest{Action: "validate_config"}
	valid := map[string]any{"valid": true, "errors": []any{}, "warnings": []any{}, "restart_required": true}
	assertOperationResult(t, request, valid, true)
	valid["valid"], valid["errors"] = false, []any{"错误"}
	valid["errors_truncated"] = true
	assertOperationResult(t, request, valid, true)
	valid["valid"] = true
	assertOperationResult(t, request, valid, false)
	valid["valid"], valid["errors"] = false, []any{nil}
	assertOperationResult(t, request, valid, false)
	valid["errors"] = []any{strings.Repeat("x", 16385)}
	assertOperationResult(t, request, valid, false)
	valid["errors"] = nil
	assertOperationResult(t, request, valid, false)
}

func TestOperationResultLogPaginationAllowsLargeFilesTailAndUTF8Boundaries(t *testing.T) {
	request := domain.OperationRequest{Action: "read_log", Name: "rcd_current.log", Generation: "generation-1", Limit: 64}
	value := logResultFixture()
	assertOperationResult(t, request, value, true)
	offset := int64(10)
	request.Offset = &offset
	value["offset"], value["next_offset"] = 12, 32
	assertOperationResult(t, request, value, true)
	request.Tail = true
	value["offset"], value["next_offset"], value["size_bytes"], value["eof"] = 9000, 9010, 9010, true
	assertOperationResult(t, request, value, true)
	value["offset"], value["next_offset"], value["size_bytes"], value["text"] = 0, 0, 0, ""
	assertOperationResult(t, request, value, true)
	request.Offset, request.Tail, request.Generation = nil, false, ""
	assertOperationResult(t, request, value, true)
	// Filtering can consume a whole page without returning any matching lines.
	value = logResultFixture()
	value["text"], value["next_offset"] = "", 256<<10
	assertOperationResult(t, request, value, true)
}

func TestOperationResultRejectsInvalidLogCursorsAndOversizeText(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong name":            func(v map[string]any) { v["name"] = "other.log" },
		"wrong generation":      func(v map[string]any) { v["generation"] = "generation-2" },
		"empty generation":      func(v map[string]any) { v["generation"] = "" },
		"backward cursor":       func(v map[string]any) { v["offset"], v["next_offset"] = 2, 1 },
		"negative offset":       func(v map[string]any) { v["offset"] = -1 },
		"arbitrary offset jump": func(v map[string]any) { v["offset"] = 10 },
		"cursor beyond file":    func(v map[string]any) { v["size_bytes"] = 10 },
		"non advancing page":    func(v map[string]any) { v["next_offset"] = 0 },
		"unbounded scan":        func(v map[string]any) { v["next_offset"] = (256 << 10) + 1 },
		"false EOF":             func(v map[string]any) { v["eof"] = true },
		"EOF must be set":       func(v map[string]any) { v["size_bytes"] = 20 },
		"string numeric":        func(v map[string]any) { v["next_offset"] = "20" },
		"null text":             func(v map[string]any) { v["text"] = nil },
		"missing text":          func(v map[string]any) { delete(v, "text") },
		"text exceeds request":  func(v map[string]any) { v["text"] = strings.Repeat("x", 65) },
		"wrong EOF type":        func(v map[string]any) { v["eof"] = "false" },
		"missing truncated":     func(v map[string]any) { delete(v, "truncated") },
	}
	request := domain.OperationRequest{Action: "read_log", Name: "rcd_current.log", Generation: "generation-1", Limit: 64}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			value := logResultFixture()
			change(value)
			assertOperationResult(t, request, value, false)
		})
	}
	value := logResultFixture()
	value["text"] = strings.Repeat("x", domain.MaxOperationLogBytes+1)
	request.Limit = 0
	assertOperationResult(t, request, value, false)
	value["text"] = strings.Repeat("x", domain.MaxOperationLogBytes)
	assertOperationResult(t, request, value, true)
}

func TestOperationResultRejectsBadOuterJSONAndUnrecognizedAction(t *testing.T) {
	for _, value := range []string{"", "null", "[]", "{}", `{"ports":[]}`, "{\"revision\":\"\xff\"}"} {
		err := validateOperationResult(domain.OperationRequest{Action: "status"}, json.RawMessage(value))
		var operationError *domain.OperationError
		if !errors.As(err, &operationError) || operationError.Code != "invalid_response" {
			t.Fatalf("accepted %q: %v", value, err)
		}
	}
	assertOperationResult(t, domain.OperationRequest{Action: "unknown"}, map[string]any{}, false)
}
