package store

import (
	"context"
	"errors"
	"testing"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

func TestCalculationDraftAndImmutableVersionLifecycle(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)
	draft, err := repository.LoadCalculationDraft(ctx)
	if err != nil || draft.Revision != 1 || draft.Valid {
		t.Fatalf("initial draft=%#v error=%v", draft, err)
	}
	config, err := calculation.ParseConfig(`[[rules]]
kks_calc="OUT"
data_type="uint"
expression="1"`)
	if err != nil {
		t.Fatal(err)
	}
	draft, err = repository.SaveCalculationDraft(ctx, calculation.Draft{Content: config.Content, Hash: config.Hash, Valid: true}, 1)
	if err != nil || draft.Revision != 2 || draft.Hash != config.Hash {
		t.Fatalf("saved draft=%#v error=%v", draft, err)
	}
	if _, err = repository.SaveCalculationDraft(ctx, draft, 1); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale save error=%v", err)
	}
	active, err := repository.ActivateCalculation(ctx, config, "tester", "active", calculation.DiffSummary{Added: 1})
	if err != nil || active.Revision != 1 || active.State != "active" {
		t.Fatalf("active=%#v error=%v", active, err)
	}
	pendingConfig, err := calculation.ParseConfig(`[[rules]]
kks_calc="OUT"
data_type="double"
expression="2"`)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repository.ActivateCalculation(ctx, pendingConfig, "tester", "pending_restart", calculation.DiffSummary{Changed: 1})
	if err != nil || pending.Revision != 2 || pending.State != "pending_restart" {
		t.Fatalf("pending=%#v error=%v", pending, err)
	}
	if pending.ChangedCount != 1 || active.AddedCount != 1 {
		t.Fatalf("version diffs not persisted: active=%#v pending=%#v", active, pending)
	}
	if current, loadErr := repository.LoadActiveCalculation(ctx); loadErr != nil || current.ID != active.ID {
		t.Fatalf("pending replaced active: %#v error=%v", current, loadErr)
	}
	promoted, err := repository.PromotePendingCalculation(ctx, pending.ID)
	if err != nil || promoted.State != "active" || promoted.Revision != pending.Revision {
		t.Fatalf("promoted=%#v error=%v", promoted, err)
	}
	items, total, err := repository.ListCalculationVersions(ctx, 50, 0)
	if err != nil || total != 2 || len(items) != 2 || items[0].Revision != 2 || items[1].State != "archived" {
		t.Fatalf("versions=%#v total=%d error=%v", items, total, err)
	}
	if err = repository.DeactivateCalculation(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.LoadActiveCalculation(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("active remains after deactivate: %v", err)
	}
}
