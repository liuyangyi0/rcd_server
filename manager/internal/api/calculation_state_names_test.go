package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"rcd_manager/internal/calculation"
)

func TestCalculationDraftDigitLeadingStateInputNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{
		{"9CYE91GH201_COM1", http.StatusOK},
		{"11_STATE", http.StatusOK},
		{"STATION", http.StatusOK},
		{"11", http.StatusUnprocessableEntity},
		{"IF", http.StatusUnprocessableEntity},
		{"false", http.StatusUnprocessableEntity},
		{"9CYE.STATE", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := calculation.StateInput{Name: tc.name, Mode: "fixed", Targets: []calculation.StateTarget{{SourceID: "rcd92-58", PortID: "/dev/ttyWCH0", DeviceID: "11"}}}
			body, err := json.Marshal(map[string]any{
				"content":      "[[rules]]\nkks_calc=\"ALARM\"\ndata_type=\"UInt32\"\nexpression=\"1\"\n",
				"state_inputs": []calculation.StateInput{input},
			})
			if err != nil {
				t.Fatal(err)
			}
			service := &calculationServiceStub{}
			response := calculationHTTP(service, http.MethodPut, "/api/v1/calculations/config/draft", string(body), `"2"`)
			if response.Code != tc.code {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.code, response.Body.String())
			}
			if tc.code != http.StatusOK {
				if service.savedExpected != 0 {
					t.Fatal("invalid state input reached draft persistence")
				}
				return
			}
			config, err := calculation.ParseConfig(service.savedContent)
			if err != nil || len(config.StateInputs) != 1 || config.StateInputs[0].Name != tc.name || config.StateInputs[0].Targets[0] != input.Targets[0] {
				t.Fatalf("state binding changed: config=%#v err=%v", config, err)
			}
			if service.savedExpected != 2 || response.Header().Get("ETag") != `"3"` {
				t.Fatal("draft revision guard changed")
			}
		})
	}
}
