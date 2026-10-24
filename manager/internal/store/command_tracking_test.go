package store

import (
	"context"
	"rcd_manager/internal/domain"
	"testing"
	"time"
)

func TestCommandEvidenceIdentityCursorAndLateResult(t *testing.T) {
	r := openTestRepository(t)
	ctx := context.Background()
	id, err := r.ManagerInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := r.ManagerInstanceID(ctx)
	if id != again {
		t.Fatal("unstable identity")
	}
	c := domain.PendingCommand{ID: "cmd", SourceID: "s", Port: "COM1", State: domain.CommandTimedOut, StartedAt: time.Now(), FinishedAt: time.Now()}
	if err = r.SaveCommand(ctx, c); err != nil {
		t.Fatal(err)
	}
	hash := domain.CommandRequestHash("COM1", "1108")
	if err = r.BeginCommandTrace(ctx, c.ID, domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: "g", BootID: "b", PhysicalPort: "COM1", RequestHash: hash}); err != nil {
		t.Fatal(err)
	}
	v := domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: id, CommandID: c.ID, GatewayInstanceID: "g", BootID: "b", PortID: "COM1", RequestHash: hash, Revision: 2, Phase: "AckReceived", Accepted: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), AttemptCount: 3, SentCount: 3}
	cursor := uint64(2)
	if err = r.SaveCommandEvidence(ctx, "s", "g", id, []domain.TrackedCommand{v}, &cursor, false); err != nil {
		t.Fatal(err)
	}
	if err = r.SaveCommandEvidence(ctx, "s", "g", id, []domain.TrackedCommand{v}, &cursor, false); err != nil {
		t.Fatal(err)
	}
	detail, err := r.CommandDetail(ctx, "cmd")
	if err != nil || detail.Command.State != domain.CommandTimedOut || detail.Trace.Snapshot.SentCount != 3 {
		t.Fatalf("%+v %v", detail, err)
	}
	_, total, err := r.CommandTraceEvents(ctx, "cmd", 10, 0)
	if err != nil || total != 1 {
		t.Fatalf("dedup %d %v", total, err)
	}
	v.BootID = "wrong"
	cursor = 3
	if err = r.SaveCommandEvidence(ctx, "s", "g", id, []domain.TrackedCommand{v}, &cursor, false); err == nil {
		t.Fatal("accepted wrong boot")
	}
	got, _ := r.CommandTraceCursor(ctx, "s", "g")
	if got != 2 {
		t.Fatal("cursor committed on invalid evidence")
	}
}

func TestCommandEvidenceGapIsScopedToPageAndSnapshotRemainsUsable(t *testing.T) {
	r := openTestRepository(t)
	ctx := context.Background()
	manager, _ := r.ManagerInstanceID(ctx)
	hash := domain.CommandRequestHash("p", "1108")
	for _, id := range []string{"a", "b"} {
		if err := r.SaveCommand(ctx, domain.PendingCommand{ID: id, SourceID: "s", Port: "p", State: domain.CommandFailed, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := r.BeginCommandTrace(ctx, id, domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: "g", BootID: "b", PhysicalPort: "p", RequestHash: hash}); err != nil {
			t.Fatal(err)
		}
	}
	v := domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: manager, CommandID: "a", GatewayInstanceID: "g", BootID: "b", PortID: "p", RequestHash: hash, Revision: 2, Phase: "Failed", AttemptCount: 3, SentCount: 3, UpdatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.SaveCommandEvidence(ctx, "s", "g", manager, []domain.TrackedCommand{v}, nil, true); err != nil {
		t.Fatal(err)
	}
	b, _ := r.CommandDetail(ctx, "b")
	if b.Trace.HistoryGap {
		t.Fatal("individual persistence rewrote unrelated history")
	}
	page := []domain.PendingCommand{{ID: "a"}, {ID: "b"}}
	if err := r.AttachCommandSummaries(ctx, page); err != nil {
		t.Fatal(err)
	}
	if page[0].Tracking.AuditIncomplete || !page[0].Tracking.HistoryGap || *page[0].Tracking.SentCount != 3 {
		t.Fatalf("snapshot summary %+v", page[0].Tracking)
	}
	if !page[1].Tracking.AuditIncomplete || page[1].Tracking.SentCount != nil {
		t.Fatal("missing evidence invented counts")
	}
	ids, err := r.IncompleteCommandIDs(ctx, "g", "", 20)
	if err != nil || len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("recovery candidates %v %v", ids, err)
	}
	before, err := r.IncompleteCommandIDs(ctx, "g", "b", 20)
	if err != nil || len(before) != 0 {
		t.Fatalf("keyset failed %v %v", before, err)
	}
}

func TestCommandEvidenceSharedGatewaySkipsOtherSourceWithoutRebinding(t *testing.T) {
	r := openTestRepository(t)
	ctx := context.Background()
	manager, _ := r.ManagerInstanceID(ctx)
	hash := domain.CommandRequestHash("p", "1108")
	r.SaveCommand(ctx, domain.PendingCommand{ID: "c", SourceID: "owner", Port: "p", StartedAt: time.Now()})
	r.BeginCommandTrace(ctx, "c", domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: "g", BootID: "b", PhysicalPort: "p", RequestHash: hash})
	v := domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: manager, CommandID: "c", GatewayInstanceID: "g", BootID: "b", PortID: "p", RequestHash: hash, Revision: 2, Phase: "Failed", UpdatedAt: time.Now().Format(time.RFC3339Nano)}
	next := uint64(2)
	if err := r.SaveCommandEvidence(ctx, "alias", "g", manager, []domain.TrackedCommand{v}, &next, false); err != nil {
		t.Fatal(err)
	}
	d, _ := r.CommandDetail(ctx, "c")
	if d.Trace.Snapshot != nil || d.Command.SourceID != "owner" {
		t.Fatal("alias rebound evidence")
	}
	c, _ := r.CommandTraceCursor(ctx, "alias", "g")
	if c != 2 {
		t.Fatal("alias page cannot advance")
	}
	if err := r.SaveCommandEvidence(ctx, "owner", "g", manager, []domain.TrackedCommand{v}, &next, false); err != nil {
		t.Fatal(err)
	}
}
