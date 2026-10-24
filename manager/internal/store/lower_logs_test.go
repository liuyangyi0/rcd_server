package store

import (
	"context"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestLowerLogCursorOnlyMovesForward(t *testing.T) {
	repository := openTestRepository(t)
	ctx := context.Background()
	if _, err := repository.CreateGroup(ctx, domain.Group{ID: "g", Name: "Group"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSource(ctx, domain.Source{ID: "s", GroupID: "g", Name: "Source", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(-time.Minute)
	if err := repository.SaveLowerLogCursor(ctx, "s", "boot", 10, first); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveLowerLogCursor(ctx, "s", "boot", 8, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cursor, err := repository.GetLowerLogCursor(ctx, "s", "boot")
	if err != nil || cursor.AcceptedThrough != 10 || !cursor.UpdatedAt.Equal(first) {
		t.Fatalf("cursor=%#v err=%v", cursor, err)
	}
}
