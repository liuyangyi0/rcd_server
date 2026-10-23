package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestPassiveResetRejectsReleaseAndSourceMaintenance(t *testing.T) {
	for _, scope := range []string{"manager", "source"} {
		t.Run(scope, func(t *testing.T) {
			r, clients := deviceRuntime(t)
			_ = r.openSession("s")
			if scope == "manager" {
				r.release.Active = true
			} else if err := r.maintenance.beginProcess("a", "restart-test"); err != nil {
				t.Fatal(err)
			}
			status, err := r.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "0")
			if status != domain.StatusBadWaitingForResponse || err == nil {
				t.Fatalf("maintenance reset admitted: status=%x error=%v", status, err)
			}
			clients["a"].recordMu.Lock()
			writes := clients["a"].writes
			clients["a"].recordMu.Unlock()
			if writes != 0 || r.routes.TotalPending() != 0 {
				t.Fatal("rejected reset reached lower or leaked a reservation")
			}
		})
	}
}

func TestPassiveResetInFlightFencesFastCommandsAndSelection(t *testing.T) {
	r := seededRuntime(t)
	lower := &deviceRecordingLower{}
	installTestLower(r, "source-a", lowerHandle{client: lower})
	r.OnLowerStatus(connectedStatus("source-a", 1))
	primeWriteAck(r, "source-a", "COM1", 1)
	_ = r.openSession("sender")
	_ = r.openSession("other")
	entered, release := make(chan struct{}), make(chan struct{})
	lower.writeHook = func() { close(entered); <-release }
	finished := make(chan error, 1)
	go func() {
		status, err := r.WriteCommand(context.Background(), "sender", testKey("COM1_Send"), "0")
		if status != domain.StatusGood && err == nil {
			err = fmt.Errorf("unexpected passive status %x", status)
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("passive write did not enter lower")
	}
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Errorf("passive completion: %v", err)
		}
		if r.routes.TotalPending() != 0 || r.releaseDispatches.Load() != 0 {
			t.Error("successful reset leaked a physical-port or maintenance reservation")
		}
		if _, err := r.selectSource("sender", "line", "source-b", 1, "test"); err != nil {
			t.Errorf("successful reset left selection locked: %v", err)
		}
	}()
	for _, payload := range []string{"1108", "0"} {
		status, err := r.WriteCommand(context.Background(), "sender", testKey("COM1_Send"), payload)
		if status != domain.StatusBadWaitingForResponse || !errors.Is(err, domain.ErrCommandPending) {
			t.Fatalf("write crossed reset: status=%x error=%v", status, err)
		}
	}
	if _, status, err := r.CallCommand(context.Background(), "other", testKey("COM1_SendCommand"), "1108"); status != domain.StatusBadWaitingForResponse || !errors.Is(err, domain.ErrCommandPending) {
		t.Fatalf("method crossed reset: status=%x error=%v", status, err)
	}
	if _, err := r.selectSource("sender", "line", "source-b", 1, "test"); !errors.Is(err, domain.ErrGroupPending) {
		t.Fatalf("selection crossed reset: %v", err)
	}
	if snapshot := r.releaseSnapshot(); snapshot.PendingCommands != 1 || snapshot.ActiveDispatches != 1 {
		t.Fatalf("passive RPC missing from maintenance drain: %+v", snapshot)
	}
	r.sweepExpiredCommands(time.Now().Add(time.Hour))
	if r.routes.TotalPending() != 1 || len(r.routes.QueuedCommands()) != 0 {
		t.Fatal("passive reset was expired or treated as a hardware queue entry")
	}
	if _, total, err := r.repo.ListCommands(context.Background(), 10, 0); err != nil || total != 0 {
		t.Fatalf("passive reset generated hardware audit: total=%d error=%v", total, err)
	}
	lower.recordMu.Lock()
	writes := lower.writes
	lower.recordMu.Unlock()
	if writes != 1 {
		t.Fatalf("fast command reached lower during reset: writes=%d", writes)
	}
}

type passiveResetResultLower struct {
	fakeLowerClient
	status domain.StatusCode
	err    error
}

func (f *passiveResetResultLower) Write(ctx context.Context, _ domain.NodeKey, _ any) (domain.StatusCode, error) {
	if f.err == context.Canceled {
		return domain.StatusGood, ctx.Err()
	}
	return f.status, f.err
}

func TestPassiveResetReleasesReservationOnFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status domain.StatusCode
		err    error
	}{
		{"rejected", domain.StatusBadOutOfService, nil},
		{"transport_error", domain.StatusGood, errors.New("write connection closed")},
		{"cancelled", domain.StatusGood, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := seededRuntime(t)
			lower := &passiveResetResultLower{status: tc.status, err: tc.err}
			installTestLower(r, "source-a", lowerHandle{client: lower})
			r.OnLowerStatus(connectedStatus("source-a", 1))
			_ = r.openSession("sender")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.err == context.Canceled {
				cancel()
			}
			status, err := r.WriteCommand(ctx, "sender", testKey("COM1_Send"), "0")
			if status.IsGood() || (tc.err != nil && err == nil) {
				t.Fatalf("failed reset reported Good: status=%x error=%v", status, err)
			}
			if r.routes.TotalPending() != 0 || r.releaseDispatches.Load() != 0 {
				t.Fatal("failed reset leaked maintenance or physical-port reservation")
			}
			if _, err := r.selectSource("sender", "line", "source-b", 1, "test"); err != nil {
				t.Fatalf("failed reset left selection locked: %v", err)
			}
			if _, total, err := r.repo.ListCommands(context.Background(), 10, 0); err != nil || total != 0 {
				t.Fatalf("failed passive reset generated hardware audit: total=%d error=%v", total, err)
			}
		})
	}
}
