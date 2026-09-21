package api

import (
	"context"
	"net/http"
	"rcd_manager/internal/calculation"
	"testing"
)

func (f *calculationServiceStub) ListCalculationStateInputs(_ context.Context, p PageRequest, view, mode, source string) (StateInputPage, error) {
	f.page = p
	return StateInputPage{Items: []StateInputView{{StateInput: calculation.StateInput{Name: "STATE", Mode: "fixed"}, Value: 5, StateText: "下级 OPC 离线"}}, Total: 1, Revision: 2}, nil
}
func (f *calculationServiceStub) ListCalculationStateCandidates(_ context.Context, p PageRequest, group, source string) (StateCandidatePage, error) {
	f.page = p
	return StateCandidatePage{Items: []StateCandidate{}, Total: 0}, nil
}
func TestStateInputsHTTPAndStructuredDraft(t *testing.T) {
	f := &calculationServiceStub{}
	for _, url := range []string{"/api/v1/calculations/state-inputs?view=active&limit=10", "/api/v1/calculations/state-input-candidates?limit=10"} {
		got := calculationHTTP(f, http.MethodGet, url, "", "")
		if got.Code != 200 || f.page.Limit != 10 {
			t.Fatal(got.Code, got.Body.String())
		}
	}
	got := calculationHTTP(f, http.MethodGet, "/api/v1/calculations/state-inputs?view=invalid", "", "")
	if got.Code != 400 {
		t.Fatal(got.Code)
	}
	body := `{"content":"","state_inputs":[{"name":"STATE","mode":"fixed","targets":[{"source_id":"a","port_id":"COM1","device_id":"11"}]}]}`
	got = calculationHTTP(f, http.MethodPut, "/api/v1/calculations/config/draft", body, `"2"`)
	if got.Code != 200 || f.savedExpected != 2 {
		t.Fatal(got.Code, got.Body.String())
	}
	parsed, err := calculation.ParseConfig(f.savedContent)
	if err != nil || len(parsed.StateInputs) != 1 {
		t.Fatal(parsed, err)
	}
	got = calculationHTTP(f, http.MethodPut, "/api/v1/calculations/config/draft", body, "")
	if got.Code != 428 {
		t.Fatal(got.Code)
	}
	got = calculationHTTP(f, http.MethodPut, "/api/v1/calculations/config/draft", `{"content":"","state_inputs":[{"name":"BAD","mode":"fixed","targets":[]}]}`, `"2"`)
	if got.Code != 422 {
		t.Fatal(got.Code, got.Body.String())
	}
}
