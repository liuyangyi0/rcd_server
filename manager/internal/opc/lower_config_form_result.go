package opc

import (
	"encoding/json"
	"strings"

	"rcd_manager/internal/domain"
)

// A lower is a protocol peer, not trusted React props. Reject malformed field
// schemas before they enter the Web cache; extension fields remain compatible.
func validateConfigForm(object operationResultObject) error {
	if _, err := operationString(object, "content", domain.MaxOperationConfigBytes, true); err != nil {
		return err
	}
	fields, err := operationArray(object, "fields")
	if err != nil || len(fields) > 4096 {
		return invalidOperationField("fields")
	}
	keys, paths := map[string]bool{}, map[string]bool{}
	for _, raw := range fields {
		var field struct {
			Path      []json.RawMessage `json:"path"`
			Kind      string            `json:"kind"`
			Value     json.RawMessage   `json:"value"`
			Options   []json.RawMessage `json:"options"`
			Label     string            `json:"label"`
			Key       string            `json:"key"`
			Hint      string            `json:"hint"`
			Readonly  *bool             `json:"readonly"`
			Inherited *bool             `json:"inherited"`
		}
		if json.Unmarshal(raw, &field) != nil || len(field.Path) == 0 || len(field.Path) > 32 || field.Label == "" || len(field.Label) > 2048 || field.Key == "" || len(field.Key) > 2048 || len(field.Hint) > 4096 || field.Options == nil || len(field.Options) > 100 || field.Readonly == nil || field.Inherited == nil {
			return invalidOperationField("fields")
		}
		if keys[field.Key] {
			return invalidOperationField("fields.key")
		}
		keys[field.Key] = true
		var canonical []any
		for index, segment := range field.Path {
			var key string
			if json.Unmarshal(segment, &key) == nil && string(segment) != "null" {
				if key == "" || len(key) > 1024 {
					return invalidOperationField("fields.path")
				}
				canonical = append(canonical, key)
			} else {
				var ordinal uint32
				if index == 0 || string(segment) == "null" || json.Unmarshal(segment, &ordinal) != nil {
					return invalidOperationField("fields.path")
				}
				canonical = append(canonical, ordinal)
			}
		}
		encoded, _ := json.Marshal(canonical)
		if paths[string(encoded)] {
			return invalidOperationField("fields.path")
		}
		paths[string(encoded)] = true
		values := append([]json.RawMessage{field.Value}, field.Options...)
		for _, value := range values {
			if len(value) == 0 || strings.TrimSpace(string(value)) == "null" {
				return invalidOperationField("fields.value")
			}
			switch field.Kind {
			case "string":
				var text string
				if json.Unmarshal(value, &text) != nil || len(text) > 16384 {
					return invalidOperationField("fields.value")
				}
			case "integer":
				var number int64
				if json.Unmarshal(value, &number) != nil {
					return invalidOperationField("fields.value")
				}
			case "boolean":
				var flag bool
				if json.Unmarshal(value, &flag) != nil {
					return invalidOperationField("fields.value")
				}
			default:
				return invalidOperationField("fields.kind")
			}
		}
	}
	return nil
}
