package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func awaitQueueCondition(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(until) {
			t.Fatal("queue condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestQueuedWritesPreserveEveryPayloadUntilACK(t *testing.T) {
	r, clients := deviceRuntime(t)
	_ = r.openSession("sender")
	for i := 0; i < 8; i++ {
		status, err := r.WriteCommand(context.Background(), "sender", testKey("ttya_Send"), fmt.Sprintf("01 06 00 %02X", i))
		if err != nil || !status.IsGood() {
			t.Fatalf("%v %v", status, err)
		}
	}
	if r.routes.TotalPending() != 8 {
		t.Fatal("commands not reserved")
	}
	r.closeSession("sender")
	for i := 0; i < 8; i++ {
		want := i + 1
		awaitQueueCondition(t, func() bool {
			clients["a"].recordMu.Lock()
			defer clients["a"].recordMu.Unlock()
			return clients["a"].writes == want
		})
		clients["a"].recordMu.Lock()
		payload := clients["a"].payload
		clients["a"].recordMu.Unlock()
		if payload != fmt.Sprintf("01 06 00 %02X", i) {
			t.Fatalf("mixed/reordered payload=%s", payload)
		}
		// Complete this command only after its immutable payload was dispatched.
		r.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	}
	awaitQueueCondition(t, func() bool { return r.routes.TotalPending() == 0 })
	r.wg.Wait()
	commands, total, err := r.repo.ListCommands(context.Background(), 20, 0)
	if err != nil || total != 8 {
		t.Fatalf("audit %d %v", total, err)
	}
	for _, c := range commands {
		if c.State != domain.CommandReceived {
			t.Fatalf("%+v", c)
		}
	}
}

func TestQueuedWritesDisconnectCancelsWithoutReplay(t *testing.T) {
	r, clients := deviceRuntime(t)
	_ = r.openSession("s")
	for i := 0; i < 3; i++ {
		if status, err := r.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 01"); err != nil || !status.IsGood() {
			t.Fatal(status, err)
		}
	}
	status := connectedStatus("a", 1)
	status.Connection = opc.ConnectionDisconnected
	r.OnLowerStatus(status)
	if len(r.routes.QueuedCommands()) != 0 {
		t.Fatal("offline queue retained")
	}
	r.OnLowerStatus(connectedStatus("a", 2))
	r.sweepExpiredCommands(time.Now().Add(time.Minute))
	clients["a"].recordMu.Lock()
	defer clients["a"].recordMu.Unlock()
	if clients["a"].writes != 1 {
		t.Fatal("replayed queued controls")
	}
}

func TestQueuedWritesUnconfirmedTimeoutCancelsTail(t *testing.T) {
	r, clients := deviceRuntime(t)
	_ = r.openSession("s")
	for i := 0; i < 3; i++ {
		_, _ = r.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 01")
	}
	r.sweepExpiredCommands(time.Now().Add(time.Minute))
	if r.routes.TotalPending() != 0 {
		t.Fatal("timeout leaked queue")
	}
	clients["a"].recordMu.Lock()
	defer clients["a"].recordMu.Unlock()
	if clients["a"].writes != 1 {
		t.Fatal("unknown outcome dispatched tail")
	}
}

func TestQueuedCommandsAreRetiredAfterRestart(t *testing.T) {
	r := seededRuntime(t)
	ctx := context.Background()
	if err := r.repo.SaveCommand(ctx, domain.PendingCommand{ID: "not-sent", SourceID: "source-a", GroupID: "line", Port: "COM1", Kind: "Write", State: domain.CommandQueued, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	after, err := NewRuntime(ctx, r.repo, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items, total, err := r.repo.ListCommandsFiltered(ctx, store.CommandFilter{Status: "Queued", Limit: 10})
	if err != nil || total != 0 || len(items) != 0 || after.routes.TotalPending() != 0 {
		t.Fatalf("queued restored: %+v %v", items, err)
	}
	detail, err := r.repo.CommandDetail(ctx, "not-sent")
	if err != nil || detail.Command.State != domain.CommandFailed {
		t.Fatal(detail, err)
	}
}

func TestQueuedWritesConfirmedTimeoutContinuesButUnknownStops(t *testing.T) {
	for _, phase := range []string{"Failed", "Unknown", "Expired"} {
		t.Run(phase, func(t *testing.T) {
			r, clients := deviceRuntime(t)
			_ = r.openSession("s")
			for i := 0; i < 2; i++ {
				_, _ = r.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 01")
			}
			first, _ := r.routes.PendingCommand("a", "ttya")
			if !r.commands.markTracked(first.ID) {
				t.Fatal("missing active command")
			}
			r.acceptCommandEvidence("a", domain.TrackedCommand{CommandID: first.ID, Phase: phase, Error: "ack_timeout"})
			if phase != "Failed" {
				if r.routes.TotalPending() != 0 {
					t.Fatal("unknown or watchdog-expired result left replayable queue")
				}
			} else {
				awaitQueueCondition(t, func() bool {
					clients["a"].recordMu.Lock()
					defer clients["a"].recordMu.Unlock()
					return clients["a"].writes == 2
				})
				r.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
			}
			r.wg.Wait()
			if phase == "Expired" {
				detail, err := r.repo.CommandDetail(context.Background(), first.ID)
				if err != nil || detail.Command.State != domain.CommandTimedOut || detail.Command.StatusCode != domain.StatusBadTimeout {
					t.Fatalf("expiration lost timeout verdict: %+v %v", detail.Command, err)
				}
			}
			clients["a"].recordMu.Lock()
			defer clients["a"].recordMu.Unlock()
			want := 1
			if phase == "Failed" {
				want = 2
			}
			if clients["a"].writes != want {
				t.Fatalf("writes=%d", clients["a"].writes)
			}
		})
	}
}

func TestQueuedWritesCapacityAndMaintenanceCounts(t *testing.T) {
	r, clients := deviceRuntime(t)
	_ = r.openSession("s")
	for i := 0; i < domain.CommandQueueCapacity; i++ {
		status, err := r.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 01")
		if err != nil || !status.IsGood() {
			t.Fatal(status, err)
		}
	}
	status, _ := r.WriteCommand(context.Background(), "s", testKey("ttya_Send"), "01 06 00 02")
	if status != domain.StatusBadWaitingForResponse {
		t.Fatal("overflow accepted")
	}
	if snapshot := r.releaseSnapshot(); snapshot.PendingCommands != 64 || snapshot.Drained {
		t.Fatalf("%+v", snapshot)
	}
	clients["a"].recordMu.Lock()
	if clients["a"].writes != 1 {
		t.Fatal("parallel same-port writes")
	}
	clients["a"].recordMu.Unlock()
	r.sweepExpiredCommands(time.Now().Add(time.Minute))
	if r.routes.TotalPending() != 0 {
		t.Fatal("capacity leaked after expiration")
	}
}

func TestQueuedWriterCannotDispatchAnotherWritersHead(t *testing.T) {
	r, _ := deviceRuntime(t)
	_ = r.openSession("s")
	first, err := r.routes.EnqueueWrite("s", "device-one", "ttya", "ttya_Send", "01 06 00 01")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.routes.EnqueueWrite("s", "device-one", "ttya", "ttya_Send", "01 06 00 02")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []domain.PendingCommand{first, second} {
		r.commands.register(c.ID, pendingCommandMeta{queued: &queuedCommandDispatch{}, ready: true})
	}
	if _, _, ok := r.commands.claimQueued(first.SourceID, first.Port, second.ID); ok {
		t.Fatal("second writer stole first command and would receive its result")
	}
	active, _, ok := r.commands.claimQueued(first.SourceID, first.Port, first.ID)
	if !ok || active.ID != first.ID {
		t.Fatal("first writer lost its queue position")
	}
	r.sweepExpiredCommands(time.Now().Add(time.Minute))
}
