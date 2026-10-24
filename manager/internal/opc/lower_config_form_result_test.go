package opc

import (
	"encoding/json"
	"testing"

	"rcd_manager/internal/domain"
)

func TestPreparedConfigurationRejectsInvalidFieldSchemas(t *testing.T) {
	field := func() map[string]any {
		return map[string]any{"path": []any{"settings", "command_retry_count"}, "kind": "integer", "key": "settings.command_retry_count", "label": "最大下发次数", "hint": "含首次", "value": 3, "options": []any{}, "readonly": false, "inherited": false}
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"valid", func(map[string]any) {}, true},
		{"wrong_value", func(f map[string]any) { f["value"] = "three" }, false},
		{"missing_options", func(f map[string]any) { delete(f, "options") }, false},
		{"negative_index", func(f map[string]any) { f["path"] = []any{"ports", -1, "enabled"} }, false},
		{"missing_readonly", func(f map[string]any) { delete(f, "readonly") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := field()
			tc.change(f)
			raw, _ := json.Marshal(map[string]any{"content": "[settings]", "fields": []any{f}})
			err := validateOperationResult(domain.OperationRequest{Action: "prepare_config"}, raw)
			if (err == nil) != tc.valid {
				t.Fatalf("error=%v", err)
			}
		})
	}
	raw, _ := json.Marshal(map[string]any{"content": "[settings]", "fields": []any{field(), field()}})
	if validateOperationResult(domain.OperationRequest{Action: "prepare_config"}, raw) == nil {
		t.Fatal("duplicate fields accepted")
	}
}
