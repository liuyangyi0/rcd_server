package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func acceptedTrackedWrite(t *testing.T) (*Runtime, *recoveryLower) {
	t.Helper()
	r := seededRuntime(t)
	if err := r.openSession("originating-session"); err != nil {
		t.Fatal(err)
	}
	lower := &recoveryLower{trackingLower: trackingLower{supported: true}, batch: domain.CommandEventBatch{
		NextCursor: "0", Health: domain.CommandTrackingHealth{GatewayInstanceID: "gateway"},
	}}
	attachRecoveryLower(r, lower, 1)
	status, err := r.WriteCommand(context.Background(), "originating-session", testKey("COM1_Send"), "1108")
	if err != nil || status != domain.StatusGood || lower.submitted != 1 {
		t.Fatalf("submit: %v %v, calls=%d", status, err, lower.submitted)
	}
	r.retryDirtyCommands()
	return r, lower
}

func attachRecoveryLower(r *Runtime, lower *recoveryLower, epoch uint64) lowerCommandTarget {
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: lower, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", epoch)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", epoch)
	target, _ := r.captureLowerCommandTarget("source-a", "COM1", false)
	return target
}

func receivedEvidence(v domain.TrackedCommand) domain.TrackedCommand {
	v.Phase = "AckReceived"
	v.Revision++
	v.AttemptCount, v.SentCount = 1, 1
	v.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return v
}

func TestTrackedPendingCompletesWhenSharedRepositoryCannotReadCursor(t *testing.T) {
	r, lower := acceptedTrackedWrite(t)
	if r.repo != r.tracking.audit.repo {
		t.Fatal("test must fail the shared production repository, not just the audit adapter")
	}
	if err := r.repo.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.repo.CommandTraceCursor(context.Background(), "source-a", "gateway"); err == nil {
		t.Fatal("cursor read fault was not injected")
	}
	lower.value = receivedEvidence(lower.value)
	target, _ := r.captureLowerCommandTarget("source-a", "COM1", false)
	if err := r.syncCommandEvidence(context.Background(), "source-a", target); err == nil {
		t.Fatal("cursor/storage failure must remain visible to health reporting")
	}
	if lower.reads != 1 || lower.submitted != 1 || lower.lastWrite != nil {
		t.Fatalf("read-side recovery was skipped or re-submitted: reads=%d submits=%d legacy=%v", lower.reads, lower.submitted, lower.lastWrite)
	}
	if r.routes.PendingCount("line") != 0 {
		t.Fatal("cursor failure kept the physical command and group locked")
	}
	detail, err := r.CommandDetail(context.Background(), lower.value.CommandID)
	if err != nil || detail.Command.State != domain.CommandReceived || detail.Trace == nil || detail.Trace.Snapshot == nil || detail.Trace.Snapshot.Phase != "AckReceived" {
		t.Fatalf("lost in-memory terminal evidence: %+v %v", detail, err)
	}
	if r.tracking.audit.health() == "" || !detail.Trace.HistoryGap {
		t.Fatal("storage failure was hidden after completing the control transaction")
	}
}

type independentlyBudgetedLower struct {
	trackingLower
	pendingExpired bool
	historyBudget  time.Duration
	reads          int
}

func (f *independentlyBudgetedLower) GetTrackedCommand(ctx context.Context, _ uint64, _, _ string) (*domain.TrackedCommand, error) {
	f.reads++
	<-ctx.Done()
	f.pendingExpired = errors.Is(ctx.Err(), context.DeadlineExceeded)
	return nil, ctx.Err()
}

func (f *independentlyBudgetedLower) TrackedCommandEvents(ctx context.Context, _ uint64, _ string, _ uint64) (domain.CommandEventBatch, error) {
	if deadline, ok := ctx.Deadline(); ok && ctx.Err() == nil {
		f.historyBudget = time.Until(deadline)
	}
	return domain.CommandEventBatch{}, errors.New("history path reached")
}

func TestTrackedSyncUsesIndependentPendingAndHistoryBudgets(t *testing.T) {
	r, original := acceptedTrackedWrite(t)
	lower := &independentlyBudgetedLower{trackingLower: trackingLower{supported: true, value: original.value}}
	err := r.syncCommandEvidence(context.Background(), "source-a", lowerCommandTarget{client: lower, globalEpoch: 1, localEpoch: 1})
	if !errors.Is(err, context.DeadlineExceeded) || !lower.pendingExpired || lower.historyBudget < commandSyncTimeout/2 {
		t.Fatalf("pending consumed history deadline: err=%v expired=%v history_budget=%s", err, lower.pendingExpired, lower.historyBudget)
	}
	if lower.reads != 1 || lower.submitted != 0 {
		t.Fatalf("unexpected dispatch or read retry: reads=%d submits=%d", lower.reads, lower.submitted)
	}
	if r.routes.PendingCount("line") != 1 {
		t.Fatal("read timeout incorrectly completed or released a command")
	}
}

// This lower deliberately holds a history request open before any command is
// submitted. Live snapshots use their own channel, as the real lower's read-only
// get_command operation does; no timed sleep decides when the ACK appears.
type blockedHistoryLower struct {
	trackingLower
	trackingMu     sync.Mutex
	historyStarted chan struct{}
	historyExited  chan struct{}
	historyOnce    sync.Once
	exitOnce       sync.Once
	pendingRead    chan struct{}
	pendingOnce    sync.Once
}

func (f *blockedHistoryLower) SubmitTrackedCommand(ctx context.Context, epoch uint64, request map[string]any) (domain.TrackedCommand, error) {
	f.trackingMu.Lock()
	defer f.trackingMu.Unlock()
	return f.trackingLower.SubmitTrackedCommand(ctx, epoch, request)
}

func (f *blockedHistoryLower) GetTrackedCommand(_ context.Context, _ uint64, _, id string) (*domain.TrackedCommand, error) {
	f.trackingMu.Lock()
	defer f.trackingMu.Unlock()
	if f.value.CommandID != id {
		return nil, nil
	}
	value := f.value
	if value.Phase == "Executing" {
		f.pendingOnce.Do(func() { close(f.pendingRead) })
	}
	return &value, nil
}

func (f *blockedHistoryLower) TrackedCommandEvents(ctx context.Context, _ uint64, _ string, _ uint64) (domain.CommandEventBatch, error) {
	f.historyOnce.Do(func() { close(f.historyStarted) })
	<-ctx.Done()
	f.exitOnce.Do(func() { close(f.historyExited) })
	return domain.CommandEventBatch{}, ctx.Err()
}

func TestTrackedWorkerCompletesNewPendingWhileHistoryIsBlocked(t *testing.T) {
	r := seededRuntime(t)
	r.options.CommandTimeout = 1500 * time.Millisecond
	if err := r.openSession("live-command"); err != nil {
		t.Fatal(err)
	}
	lower := &blockedHistoryLower{
		trackingLower: trackingLower{supported: true}, historyStarted: make(chan struct{}),
		historyExited: make(chan struct{}), pendingRead: make(chan struct{}),
	}
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: lower, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", 1)
	runCtx, cancel := context.WithCancel(context.Background())
	r.runContext = runCtx
	r.wg.Add(1)
	go r.commandTrackingWorker()
	t.Cleanup(func() {
		cancel()
		r.wg.Wait()
	})
	r.tracking.notify("source-a")
	select {
	case <-lower.historyStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("history worker did not reach the barrier")
	}
	type callResult struct {
		value  string
		status domain.StatusCode
		err    error
	}
	finished := make(chan callResult, 1)
	go func() {
		value, status, err := r.CallCommand(runCtx, "live-command", testKey("COM1_SendCommand"), "1108")
		finished <- callResult{value, status, err}
	}()
	select {
	case <-lower.pendingRead:
	case result := <-finished:
		t.Fatalf("Call ended before an independent live read: %+v", result)
	case <-time.After(3 * time.Second):
		t.Fatal("history blocked the live pending snapshot")
	}
	lower.trackingMu.Lock()
	lower.value = receivedEvidence(lower.value)
	lower.trackingMu.Unlock()
	r.tracking.notify("source-a")
	select {
	case result := <-finished:
		if result.err != nil || result.status != domain.StatusGood || result.value != "1" {
			t.Fatalf("ACK was delayed past the Call deadline by history: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live ACK did not complete Call")
	}
	select {
	case <-lower.historyExited:
		t.Fatal("command only completed after history unblocked")
	default:
	}
	lower.trackingMu.Lock()
	submits, commandID := lower.submitted, lower.value.CommandID
	lower.trackingMu.Unlock()
	if submits != 1 || r.routes.PendingCount("line") != 0 {
		t.Fatalf("completion re-submitted or retained the physical reservation: submits=%d", submits)
	}
	detail, err := r.CommandDetail(context.Background(), commandID)
	if err != nil || detail.Command.State != domain.CommandReceived || detail.Trace == nil || detail.Trace.Snapshot == nil || detail.Trace.Snapshot.Phase != "AckReceived" {
		t.Fatalf("missing live terminal evidence: %+v %v", detail, err)
	}
}

func TestTrackedSyncLanesPreserveIndependentErrors(t *testing.T) {
	r := seededRuntime(t)
	historyErr := errors.New("history cursor unavailable")
	r.tracking.endSync("source-a", false, historyErr)
	r.tracking.endSync("source-a", true, nil)
	if r.tracking.errors["source-a"] != historyErr.Error() {
		t.Fatal("a successful live lookup hid the history error")
	}
	r.tracking.endSync("source-a", true, errors.New("live lookup unavailable"))
	r.tracking.endSync("source-a", false, nil)
	if r.tracking.errors["source-a"] != "live lookup unavailable" {
		t.Fatal("a successful history refresh hid the live error")
	}
	if r.tracking.beginPendingSync("source-a") || r.tracking.errors["source-a"] != "" {
		t.Fatal("idle source kept a stale live-lookup error or scheduled live I/O")
	}
}

func TestRestartedTrackedPendingCompletesFromEvidenceWithoutResubmission(t *testing.T) {
	before, original := acceptedTrackedWrite(t)
	after, err := NewRuntime(context.Background(), before.repo, RuntimeOptions{CommandPendingTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.routes.Sessions()) != 0 || after.routes.PendingCount("line") != 1 {
		t.Fatal("restart must restore the physical reservation without the old Session")
	}
	meta, exists := after.commands.meta[original.value.CommandID]
	if !exists || !meta.tracked || !meta.armed || meta.done != nil {
		t.Fatalf("invalid recovered completion coordinator: %+v exists=%v", meta, exists)
	}
	lower := &recoveryLower{trackingLower: trackingLower{supported: true, value: receivedEvidence(original.value)}, batch: original.batch}
	target := attachRecoveryLower(after, lower, 2)
	after.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 2})
	if after.routes.PendingCount("line") != 1 {
		t.Fatal("new connection's uncorrelated ACK completed a restored command")
	}
	if err := after.syncCommandEvidence(context.Background(), "source-a", target); err != nil {
		t.Fatal(err)
	}
	if after.routes.PendingCount("line") != 0 || lower.submitted != 0 || lower.lastWrite != nil {
		t.Fatalf("restored completion retained a lock or re-dispatched: submits=%d legacy=%v", lower.submitted, lower.lastWrite)
	}
	detail, err := after.repo.CommandDetail(context.Background(), original.value.CommandID)
	if err != nil || detail.Command.State != domain.CommandReceived || detail.Command.Response != "1" || detail.Trace.Snapshot == nil || detail.Trace.Snapshot.Phase != "AckReceived" {
		t.Fatalf("restored command was not durably completed: %+v %v", detail, err)
	}
}

func TestRestartedTrackedPendingRejectsPinnedIdentityMismatch(t *testing.T) {
	for _, field := range []string{"manager", "gateway", "boot", "port", "hash"} {
		t.Run(field, func(t *testing.T) {
			before, original := acceptedTrackedWrite(t)
			after, err := NewRuntime(context.Background(), before.repo, RuntimeOptions{CommandPendingTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			value := receivedEvidence(original.value)
			switch field {
			case "manager":
				value.ManagerInstanceID = "other-manager"
			case "gateway":
				value.GatewayInstanceID = "other-gateway"
			case "boot":
				value.BootID = "other-boot"
			case "port":
				value.PortID = "other-port"
			case "hash":
				value.RequestHash = domain.CommandRequestHash(value.PortID, "1208")
			}
			lower := &recoveryLower{trackingLower: trackingLower{supported: true, value: value}, batch: original.batch}
			target := attachRecoveryLower(after, lower, 2)
			if err := after.syncCommandEvidence(context.Background(), "source-a", target); err == nil {
				t.Fatal("wrong pinned identity was accepted")
			}
			detail, err := after.CommandDetail(context.Background(), original.value.CommandID)
			if err != nil || detail.Command.State != domain.CommandWaiting || after.routes.PendingCount("line") != 1 || lower.submitted != 0 || lower.lastWrite != nil {
				t.Fatalf("mismatched evidence changed control state: %+v %v", detail, err)
			}
		})
	}
}

func TestRestartDoesNotArmLegacyExpiredOrUnpinnedCommands(t *testing.T) {
	for _, scenario := range []string{"legacy", "missing-boot", "wrong-hash", "expired", "already-timed-out"} {
		t.Run(scenario, func(t *testing.T) {
			before := seededRuntime(t)
			command := domain.PendingCommand{ID: "recovery-boundary", SessionID: "closed-session", GroupID: "line", SourceID: "source-a", Port: "COM1", Kind: "Write", Request: "1108", State: domain.CommandWaiting, StartedAt: time.Now().UTC()}
			trace := domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: "gateway", BootID: "boot", PhysicalPort: "COM1", RequestHash: domain.CommandRequestHash("COM1", command.Request)}
			switch scenario {
			case "legacy":
				trace.Protocol = "legacy"
			case "missing-boot":
				trace.BootID = ""
			case "wrong-hash":
				trace.RequestHash = domain.CommandRequestHash("COM1", "1208")
			case "expired":
				command.StartedAt = command.StartedAt.Add(-2 * time.Minute)
			case "already-timed-out":
				command.State = domain.CommandTimedOut
				command.FinishedAt = time.Now().UTC()
			}
			if err := before.repo.SaveCommand(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			if err := before.repo.BeginCommandTrace(context.Background(), command.ID, trace); err != nil {
				t.Fatal(err)
			}
			after, err := NewRuntime(context.Background(), before.repo, RuntimeOptions{CommandPendingTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := after.commands.meta[command.ID]; exists {
				t.Fatal("unrecoverable/finished command was armed for new evidence")
			}
			value := domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: after.tracking.managerID, CommandID: command.ID, GatewayInstanceID: "gateway", BootID: "boot", PortID: "COM1", RequestHash: domain.CommandRequestHash("COM1", command.Request), Accepted: true, Phase: "AckReceived", Revision: 2, AttemptCount: 1, SentCount: 1, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			lower := &recoveryLower{trackingLower: trackingLower{supported: true, value: value}, batch: domain.CommandEventBatch{NextCursor: "0", Health: domain.CommandTrackingHealth{GatewayInstanceID: "gateway"}}}
			target := attachRecoveryLower(after, lower, 2)
			after.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 2})
			_ = after.syncCommandEvidence(context.Background(), "source-a", target)
			detail, err := after.CommandDetail(context.Background(), command.ID)
			wantState := domain.CommandWaiting
			wantPending := 1
			if scenario == "expired" || scenario == "already-timed-out" {
				wantState, wantPending = domain.CommandTimedOut, 0
			}
			if err != nil || detail.Command.State != wantState || after.routes.PendingCount("line") != wantPending || lower.submitted != 0 || lower.lastWrite != nil {
				t.Fatalf("unsafe recovery of %s: %+v %v", scenario, detail, err)
			}
		})
	}
}
