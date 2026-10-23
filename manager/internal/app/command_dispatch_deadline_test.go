package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestTrackedCommandBudgetUsesRemainingDeadline(t *testing.T) {
	r := seededRuntime(t)
	r.options.CommandTimeout = 5 * time.Second
	r.options.CommandPendingTTL = 30 * time.Second
	for _, tc := range []struct {
		name      string
		age       time.Duration
		ctxBudget time.Duration
		wantMax   time.Duration
	}{
		{"queued_near_expiry", 29800 * time.Millisecond, time.Second, 200 * time.Millisecond},
		{"short_rpc_deadline", time.Second, 200 * time.Millisecond, 200 * time.Millisecond},
		{"normal_cap", time.Second, 10 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.ctxBudget)
			defer cancel()
			budget, err := r.trackedCommandBudget(ctx, domain.PendingCommand{StartedAt: time.Now().Add(-tc.age)})
			if err != nil || budget < time.Millisecond || budget > tc.wantMax {
				t.Fatalf("budget=%s err=%v limit=%s", budget, err, tc.wantMax)
			}
		})
	}
}

func TestTrackedCommandBudgetRejectsExpiredOrCanceled(t *testing.T) {
	r := seededRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.trackedCommandBudget(ctx, domain.PendingCommand{StartedAt: time.Now()}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.trackedCommandBudget(context.Background(), domain.PendingCommand{StartedAt: time.Now().Add(-r.options.CommandPendingTTL)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

// A cached capability lets the test pin the exact remaining total budget at
// dispatch, with no network or real device involved.
func TestTrackedDispatchDoesNotRenewQueueDeadline(t *testing.T) {
	r := seededRuntime(t)
	if err := r.openSession("deadline"); err != nil {
		t.Fatal(err)
	}
	command, err := r.routes.BeginCommandWithDetails("deadline", "line", "COM1", "Write", "COM1_Send", "1108")
	if err != nil {
		t.Fatal(err)
	}
	r.commands.register(command.ID, pendingCommandMeta{})
	lower := &trackingLower{supported: true}
	target := lowerCommandTarget{client: lower, globalEpoch: 1, localEpoch: 1}
	r.tracking.capabilities[command.SourceID] = commandCapability{epoch: 1, info: domain.CommandTrackingHealth{GatewayInstanceID: "gateway", BootID: "boot"}, supported: true, checked: time.Now()}
	command.StartedAt = time.Now().Add(-r.options.CommandPendingTTL + 200*time.Millisecond)
	status, err := r.dispatchCommand(context.Background(), command, commandRoute{PhysicalPort: "COM1"}, target, command.Request)
	if err != nil || !status.IsGood() {
		t.Fatal(status, err)
	}
	if got := lower.request["timeout_ms"].(int64); got <= 0 || got > 200 {
		t.Fatalf("lower received renewed timeout %dms", got)
	}
	r.finishCommand(command.ID, domain.CommandFailed, domain.StatusBadTimeout, "test cleanup")
}

func TestExpiredTrackedDispatchNeverSubmits(t *testing.T) {
	r := seededRuntime(t)
	if err := r.openSession("deadline"); err != nil {
		t.Fatal(err)
	}
	command, err := r.routes.BeginCommandWithDetails("deadline", "line", "COM1", "Write", "COM1_Send", "1108")
	if err != nil {
		t.Fatal(err)
	}
	r.commands.register(command.ID, pendingCommandMeta{})
	lower := &trackingLower{supported: true}
	target := lowerCommandTarget{client: lower, globalEpoch: 1, localEpoch: 1}
	command.StartedAt = time.Now().Add(-r.options.CommandPendingTTL)
	status, err := r.dispatchCommand(context.Background(), command, commandRoute{PhysicalPort: "COM1"}, target, command.Request)
	if status != domain.StatusBadTimeout || !errors.Is(err, context.DeadlineExceeded) || lower.submitted != 0 {
		t.Fatalf("status=%v err=%v submits=%d", status, err, lower.submitted)
	}
	r.finishCommand(command.ID, domain.CommandTimedOut, status, "test cleanup")
}

func TestPassiveResetCannotBypassAcceptedQueue(t *testing.T) {
	r, clients := deviceRuntime(t)
	if err := r.openSession("queued"); err != nil {
		t.Fatal(err)
	}
	// Deliberately stop between route acceptance and claim, as concurrent
	// writers can while the acceptance audit is being persisted.
	command, err := r.routes.EnqueueWrite("queued", "device-one", "ttya", "ttya_Send", "01 06 00 01")
	if err != nil {
		t.Fatal(err)
	}
	status, err := r.WriteCommand(context.Background(), "queued", testKey("ttya_Send"), "0")
	if status != domain.StatusBadWaitingForResponse || !errors.Is(err, domain.ErrCommandPending) {
		t.Fatalf("queued reset admitted: status=%v err=%v", status, err)
	}
	clients["a"].recordMu.Lock()
	writes := clients["a"].writes
	clients["a"].recordMu.Unlock()
	if writes != 0 {
		t.Fatalf("reset reached lower: writes=%d", writes)
	}
	r.finishCommand(command.ID, domain.CommandFailed, domain.StatusBadTimeout, "test cleanup")
}
