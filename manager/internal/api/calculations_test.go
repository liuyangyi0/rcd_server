package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

type calculationServiceStub struct {
	fakeService
	draft           calculation.Draft
	page            PageRequest
	savedExpected   uint64
	savedContent    string
	importSource    string
	importRevision  uint64
	activateErr     error
	statusRevision  *uint64
	restoreExpected uint64
	restoreCalls    int
	restoreErr      error
}

func (f *calculationServiceStub) GetCalculationStatus(context.Context) (CalculationStatus, error) {
	revision := uint64(4)
	if f.statusRevision != nil {
		revision = *f.statusRevision
	}
	return CalculationStatus{RoutingMode: "DefaultSources", ActiveRevision: revision, RuleCount: 6683}, nil
}
func (f *calculationServiceStub) ListCalculationResults(_ context.Context, request PageRequest) (CalculationResultPage, error) {
	f.page = request
	return CalculationResultPage{Items: []calculation.Result{}, Total: 0, Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *calculationServiceStub) ListCalculationRules(_ context.Context, request PageRequest) (CalculationRulePage, error) {
	f.page = request
	return CalculationRulePage{Items: []calculation.RuleView{}, Total: 0, Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *calculationServiceStub) GetCalculationDraft(context.Context) (calculation.Draft, error) {
	if f.draft.Revision == 0 {
		f.draft.Revision = 2
	}
	return f.draft, nil
}
func (f *calculationServiceStub) SaveCalculationDraft(_ context.Context, expected uint64, content string) (calculation.Draft, error) {
	f.savedExpected, f.savedContent = expected, content
	if len(content) > calculation.MaxConfigBytes {
		return calculation.Draft{}, calculation.ErrConfigTooLarge
	}
	return calculation.Draft{Revision: expected + 1, Content: content}, nil
}
func (f *calculationServiceStub) ImportCalculationFromLower(_ context.Context, source string, revision uint64) (calculation.Draft, error) {
	f.importSource, f.importRevision = source, revision
	return calculation.Draft{Content: "[[rules]]"}, nil
}
func (f *calculationServiceStub) ValidateCalculation(context.Context, *string) (CalculationValidation, error) {
	return CalculationValidation{Valid: true, RuleCount: 1, Problems: []calculation.Problem{}}, nil
}
func (f *calculationServiceStub) ActivateCalculation(context.Context, uint64) (calculation.Version, error) {
	return calculation.Version{Revision: 4, State: "active"}, f.activateErr
}
func (f *calculationServiceStub) DeactivateCalculation(context.Context, uint64) error { return nil }
func (f *calculationServiceStub) ListCalculationVersions(_ context.Context, request PageRequest) (CalculationVersionPage, error) {
	return CalculationVersionPage{Items: []calculation.Version{}, Limit: request.Limit, Offset: request.Offset}, nil
}
func (f *calculationServiceStub) RestoreCalculationVersion(_ context.Context, _ int64, expected uint64) (calculation.Version, error) {
	f.restoreExpected = expected
	f.restoreCalls++
	return calculation.Version{Revision: 5, State: "active"}, f.restoreErr
}

func calculationHTTP(service *calculationServiceStub, method, path, body, revision string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if revision != "" {
		request.Header.Set("If-Match", revision)
	}
	response := httptest.NewRecorder()
	New(service, Options{}).ServeHTTP(response, request)
	return response
}

func TestCalculationHTTPPinsDraftAndLowerRevisions(t *testing.T) {
	service := &calculationServiceStub{}
	withoutRevision := calculationHTTP(service, http.MethodPut, "/api/v1/calculations/config/draft", `{"content":"x"}`, "")
	if withoutRevision.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing revision status=%d", withoutRevision.Code)
	}

	saved := calculationHTTP(service, http.MethodPut, "/api/v1/calculations/config/draft", `{"content":"[[rules]]"}`, `"2"`)
	if saved.Code != http.StatusOK || service.savedExpected != 2 || service.savedContent != "[[rules]]" || saved.Header().Get("ETag") != `"3"` {
		t.Fatalf("save status=%d headers=%v service=%+v body=%s", saved.Code, saved.Header(), service, saved.Body.String())
	}

	imported := calculationHTTP(service, http.MethodPost, "/api/v1/calculations/config/import-lower", `{"source_id":"lower-a","source_revision":9}`, `"3"`)
	if imported.Code != http.StatusOK || service.importSource != "lower-a" || service.importRevision != 9 || service.savedExpected != 3 {
		t.Fatalf("import status=%d service=%+v body=%s", imported.Code, service, imported.Body.String())
	}
}

func TestCalculationHTTPMapsValidationSizeAndRevisionErrors(t *testing.T) {
	service := &calculationServiceStub{activateErr: &calculation.ValidationError{Problems: []calculation.Problem{{Rule: "X", Message: "invalid"}}}}
	invalid := calculationHTTP(service, http.MethodPost, "/api/v1/calculations/config/activate", `{}`, `"2"`)
	if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), "calculation_invalid") {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	service.activateErr = &domain.RevisionConflictError{Expected: 2, Actual: 3}
	conflict := calculationHTTP(service, http.MethodPost, "/api/v1/calculations/config/activate", `{}`, `"2"`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"actual":3`) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	if status, code, _ := mapError(calculation.ErrConfigTooLarge); status != http.StatusRequestEntityTooLarge || code != "calculation_too_large" {
		t.Fatalf("size mapping status=%d code=%s", status, code)
	}
}

func TestCalculationHTTPParsesResultFilters(t *testing.T) {
	service := &calculationServiceStub{}
	response := calculationHTTP(service, http.MethodGet, "/api/v1/calculations/results?q=KKS&status=bad&kind=Double&limit=25&offset=50", "", "")
	want := PageRequest{Query: "KKS", Status: "bad", Kind: "Double", Limit: 25, Offset: 50}
	if response.Code != http.StatusOK || service.page != want {
		t.Fatalf("status=%d request=%#v want=%#v", response.Code, service.page, want)
	}
}

func TestCalculationRestoreAcceptsObservedNoActiveRevision(t *testing.T) {
	zero := uint64(0)
	service := &calculationServiceStub{statusRevision: &zero}
	status := calculationHTTP(service, http.MethodGet, "/api/v1/calculations/status", "", "")
	if status.Code != http.StatusOK || status.Header().Get("ETag") != `"0"` {
		t.Fatalf("status=%d etag=%q", status.Code, status.Header().Get("ETag"))
	}
	restored := calculationHTTP(service, http.MethodPost, "/api/v1/calculations/config/versions/3/restore", "", `"0"`)
	if restored.Code != http.StatusOK || service.restoreCalls != 1 || service.restoreExpected != 0 || restored.Header().Get("ETag") != `"5"` {
		t.Fatalf("restore status=%d calls=%d expected=%d body=%s", restored.Code, service.restoreCalls, service.restoreExpected, restored.Body.String())
	}
	service.restoreErr = &domain.RevisionConflictError{Expected: 0, Actual: 6}
	conflict := calculationHTTP(service, http.MethodPost, "/api/v1/calculations/config/versions/3/restore", "", `"0"`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("stale no-active observation status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestCalculationRestoreRequiresExplicitPreconditionAndKeepsOtherWritesPositive(t *testing.T) {
	for _, test := range []struct {
		header string
		status int
	}{
		{"", http.StatusPreconditionRequired},
		{"0", http.StatusBadRequest},
		{`W/"0"`, http.StatusBadRequest},
		{`"00"`, http.StatusBadRequest},
		{"*", http.StatusBadRequest},
		{`"-1"`, http.StatusBadRequest},
	} {
		t.Run(test.header, func(t *testing.T) {
			service := &calculationServiceStub{}
			response := calculationHTTP(service, http.MethodPost, "/api/v1/calculations/config/versions/3/restore", "", test.header)
			if response.Code != test.status || service.restoreCalls != 0 {
				t.Fatalf("status=%d want=%d calls=%d", response.Code, test.status, service.restoreCalls)
			}
		})
	}
	for _, path := range []string{"activate", "deactivate", "import-lower"} {
		response := calculationHTTP(&calculationServiceStub{}, http.MethodPost, "/api/v1/calculations/config/"+path, `{}`, `"0"`)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s unexpectedly accepts zero: %d", path, response.Code)
		}
	}
	response := calculationHTTP(&calculationServiceStub{}, http.MethodPut, "/api/v1/calculations/config/draft", `{"content":""}`, `"0"`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("save unexpectedly accepts zero: %d", response.Code)
	}
}

var _ CalculationService = (*calculationServiceStub)(nil)
