package store

import (
	"context"
	"errors"
	"rcd_manager/internal/domain"
	"testing"
)

func TestRedundancyPolicyPersistenceAndRevision(t *testing.T) {
	ctx := context.Background()
	r := openTestRepository(t)
	group, err := r.CreateGroup(ctx, domain.Group{ID: "g", Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	p := domain.DefaultRedundancyPolicy("g")
	p.Enabled = true
	p.Priority = []string{"a", "b"}
	saved, err := r.SaveRedundancy(ctx, p, 1)
	if err != nil || saved.Revision != 2 {
		t.Fatal(saved, err)
	}
	if _, err = r.SaveRedundancy(ctx, p, 1); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatal(err)
	}
	rows, err := r.LoadRedundancy(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Enabled || len(rows[0].Priority) != 2 {
		t.Fatal(rows, err)
	}
	if err = r.DeleteGroup(ctx, "g", group.Revision); err != nil {
		t.Fatal(err)
	}
	rows, err = r.LoadRedundancy(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
