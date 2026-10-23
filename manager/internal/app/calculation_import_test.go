package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

func TestCalculationImportPinsSourceRevision(t *testing.T) {
	r := seededRuntime(t)
	content := "[[rules]]\nkks_calc = \"IMPORTED\"\ndata_type = \"double\"\nexpression = \"KKS001 + 1\"\n"
	f := &operationLowerStub{execute: func(_ context.Context, request domain.OperationRequest, _ uint64) (json.RawMessage, error) {
		if request.Action != "read_config" || request.Name != "calc.toml" {
			t.Fatalf("unexpected import request: %+v", request)
		}
		result, err := json.Marshal(map[string]string{"content": content})
		return result, err
	}}
	revision := installOperationsLower(t, r, f)
	if _, err := r.ImportCalculationFromLower(context.Background(), "source-a", 0); err == nil || f.calls.Load() != 0 {
		t.Fatalf("missing source revision reached lower: err=%v calls=%d", err, f.calls.Load())
	}
	if _, err := r.ImportCalculationFromLower(context.Background(), "source-a", revision+1); !errors.Is(err, domain.ErrRevisionConflict) || f.calls.Load() != 0 {
		t.Fatalf("stale source revision reached lower: err=%v calls=%d", err, f.calls.Load())
	}
	draft, err := r.ImportCalculationFromLower(context.Background(), "source-a", revision)
	if err != nil || !draft.Valid || draft.Content != content || f.calls.Load() != 1 {
		t.Fatalf("valid import draft=%+v err=%v calls=%d", draft, err, f.calls.Load())
	}
}

func TestCalculationImportRejectsSourceChangeDuringLowerRead(t *testing.T) {
	r := seededRuntime(t)
	before, err := r.GetCalculationDraft(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := &operationLowerStub{execute: func(ctx context.Context, _ domain.OperationRequest, _ uint64) (json.RawMessage, error) {
		source, err := r.source("source-a")
		if err != nil {
			return nil, err
		}
		_, err = r.UpdateSource(ctx, source.ID, source.Revision, api.SourceMutation{
			ID: source.ID, GroupID: source.GroupID, GroupBindings: source.GroupBindings,
			Name: source.Name + " changed during import", Endpoint: source.Endpoint,
			SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode,
			Username: source.Username, Enabled: source.Enabled,
		})
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"content":"[[rules]]\nkks_calc = \"STALE_IMPORT\"\ndata_type = \"double\"\nexpression = \"KKS001\"\n"}`), nil
	}}
	revision := installOperationsLower(t, r, f)
	if _, err := r.ImportCalculationFromLower(context.Background(), "source-a", revision); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("late import did not reject a changed source: %v", err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("import should read once and never retry: %d", f.calls.Load())
	}
	after, err := r.GetCalculationDraft(context.Background())
	if err != nil || after.Revision != before.Revision || after.Content != before.Content {
		t.Fatalf("rejected import changed saved draft: before=%+v after=%+v err=%v", before, after, err)
	}
}
