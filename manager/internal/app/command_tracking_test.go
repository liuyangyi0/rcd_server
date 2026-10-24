package app

import (
	"context"
	"errors"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
	"testing"
	"time"
)

type trackingLower struct {
	fakeLowerClient
	supported bool
	submitErr error
	submitted int
	request   map[string]any
	value     domain.TrackedCommand
}

type recoveryLower struct {
	trackingLower
	batch domain.CommandEventBatch
	reads int
}

type catalogChangingTrackingLower struct {
	trackingLower
	changeCatalog func()
}

func (f *catalogChangingTrackingLower) CommandTrackingInfo(context.Context, uint64) (domain.CommandTrackingHealth, bool, error) {
	f.changeCatalog()
	return domain.CommandTrackingHealth{GatewayInstanceID: "gateway", BootID: "boot"}, true, nil
}

func (f *catalogChangingTrackingLower) SubmitTrackedCommand(ctx context.Context, epoch uint64, request map[string]any) (domain.TrackedCommand, error) {
	value, err := f.trackingLower.SubmitTrackedCommand(ctx, epoch, request)
	value.Phase, value.AttemptCount, value.SentCount = "AckReceived", 1, 1
	return value, err
}

func TestTrackedCallPinsPhysicalPortBeforeCapabilityAndAuditIO(t *testing.T) {
	r := seededRuntime(t)
	if err := r.openSession("fixed-call"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	catalog := r.catalogs["source-a"]
	catalog.Topology = &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "/dev/original", SendNodeID: "COM1_Send", SendMethodID: "COM1_SendCommand"}}}
	r.catalogs["source-a"] = catalog
	r.mu.Unlock()
	lower := &catalogChangingTrackingLower{changeCatalog: func() {
		// Capability discovery happens after the acceptance transaction releases
		// configMu. A hot catalog can now publish a different physical port.
		r.configMu.Lock()
		r.mu.Lock()
		changed := r.catalogs["source-a"].Clone()
		changed.Topology.Ports[0].PortID = "/dev/reconfigured"
		r.catalogs["source-a"] = changed
		r.mu.Unlock()
		r.configMu.Unlock()
	}}
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: lower, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", 1)
	result, status, err := r.CallCommand(context.Background(), "fixed-call", testKey("COM1_SendCommand"), "1108")
	if err != nil || status != domain.StatusGood || result != "1" {
		t.Fatalf("Call=%q status=%v err=%v", result, status, err)
	}
	if lower.submitted != 1 || lower.request["port_id"] != "/dev/original" {
		t.Fatalf("accepted Call followed the new physical port: %+v", lower.request)
	}
	detail, err := r.CommandDetail(context.Background(), lower.value.CommandID)
	if err != nil || detail.Trace == nil || detail.Trace.PhysicalPort != "/dev/original" {
		t.Fatalf("audit did not preserve the accepted physical target: %+v %v", detail, err)
	}
}

func (f *recoveryLower) TrackedCommandEvents(context.Context, uint64, string, uint64) (domain.CommandEventBatch, error) {
	return f.batch, nil
}
func (f *recoveryLower) GetTrackedCommand(_ context.Context, _ uint64, _ string, id string) (*domain.TrackedCommand, error) {
	f.reads++
	if f.value.CommandID == id {
		return &f.value, nil
	}
	return nil, nil
}

func TestCommandPageAdvancesDespiteUnrelatedDirtyAuditAndRecoversFinishedSnapshot(t *testing.T) {
	r := seededRuntime(t)
	ctx := context.Background()
	hash := domain.CommandRequestHash("COM1", "1108")
	command := domain.PendingCommand{ID: "historical", SourceID: "source-a", Port: "COM1", State: domain.CommandTimedOut, StartedAt: time.Now(), FinishedAt: time.Now()}
	if err := r.repo.SaveCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
	if err := r.repo.BeginCommandTrace(ctx, command.ID, domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: "gateway", BootID: "boot", PhysicalPort: "COM1", RequestHash: hash, HistoryGap: true}); err != nil {
		t.Fatal(err)
	}
	lower := &recoveryLower{trackingLower: trackingLower{supported: true, value: domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: r.tracking.managerID, CommandID: command.ID, GatewayInstanceID: "gateway", BootID: "boot", PortID: "COM1", RequestHash: hash, Phase: "AckReceived", Revision: 50, AttemptCount: 3, SentCount: 3, UpdatedAt: time.Now().Format(time.RFC3339Nano)}}, batch: domain.CommandEventBatch{NextCursor: "50", Health: domain.CommandTrackingHealth{GatewayInstanceID: "gateway"}}}
	r.tracking.audit.put(domain.CommandDetail{Command: domain.PendingCommand{ID: "unrelated", SourceID: "source-b", StartedAt: time.Now()}})
	target := lowerCommandTarget{client: lower, localEpoch: 1, globalEpoch: 1}
	if err := r.syncCommandEvidence(ctx, "source-a", target); err != nil {
		t.Fatal(err)
	}
	c, err := r.repo.CommandTraceCursor(ctx, "source-a", "gateway")
	if err != nil || c != 50 {
		t.Fatalf("stuck cursor %d %v", c, err)
	}
	d, err := r.repo.CommandDetail(ctx, command.ID)
	if err != nil || d.Trace.Snapshot == nil || d.Trace.Snapshot.SentCount != 3 || d.Command.State != domain.CommandTimedOut {
		t.Fatalf("recovery changed verdict or lost evidence %+v %v", d, err)
	}
	if lower.submitted != 0 || lower.reads != 1 {
		t.Fatalf("recovery sent command or failed to read: %+v", lower)
	}
	if r.tracking.audit.dirtyCount() != 1 {
		t.Fatal("unrelated dirty entry was acknowledged")
	}
}

func TestHistoricalEvidenceFollowsPinnedGatewayNotEditedSourceEndpoint(t *testing.T) {
	r := seededRuntime(t)
	ctx := context.Background()
	hash := domain.CommandRequestHash("p", "1108")
	c := domain.PendingCommand{ID: "before-endpoint-edit", SourceID: "source-a", Port: "p", State: domain.CommandFailed, StartedAt: time.Now()}
	r.repo.SaveCommand(ctx, c)
	r.repo.BeginCommandTrace(ctx, c.ID, domain.CommandTrace{Protocol: "tracked", GatewayInstanceID: "gateway", BootID: "old-boot", PhysicalPort: "p", RequestHash: hash})
	lower := &recoveryLower{trackingLower: trackingLower{value: domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: r.tracking.managerID, CommandID: c.ID, GatewayInstanceID: "gateway", BootID: "old-boot", PortID: "p", RequestHash: hash, Phase: "Failed", Revision: 20, AttemptCount: 3, SentCount: 3, UpdatedAt: time.Now().Format(time.RFC3339Nano)}}}
	changed, err := r.backfillCommandEvidence(ctx, "source-b", lowerCommandTarget{client: lower}, "gateway")
	if err != nil || !changed {
		t.Fatalf("pinned recovery %v %v", changed, err)
	}
	d, _ := r.repo.CommandDetail(ctx, c.ID)
	if d.Command.SourceID != "source-a" || d.Trace.Snapshot == nil || d.Trace.Snapshot.SentCount != 3 || d.Trace.BootID != "old-boot" {
		t.Fatalf("historical target changed: %+v", d)
	}
	if lower.submitted != 0 {
		t.Fatal("recovery submitted a command")
	}
}

func TestTrackingSchedulesAreIsolatedAndIdleSourcesDoNotPollAtCommandRate(t *testing.T) {
	r := seededRuntime(t)
	s := r.tracking
	s.capabilities["a"] = commandCapability{epoch: 1}
	s.capabilities["b"] = commandCapability{epoch: 1}
	if !s.beginSync("a", 1) || s.beginSync("a", 1) {
		t.Fatal("duplicate concurrent source query")
	}
	if !s.beginSync("b", 1) {
		t.Fatal("busy source blocked another source")
	}
	delete(s.inFlight, "a")
	if s.beginSync("a", 1) {
		t.Fatal("idle source polled before its fallback interval")
	}
	s.notify("a")
	if !s.beginSync("a", 1) {
		t.Fatal("revision notification did not schedule source")
	}
}

func (f *trackingLower) CommandTrackingInfo(context.Context, uint64) (domain.CommandTrackingHealth, bool, error) {
	return domain.CommandTrackingHealth{GatewayInstanceID: "gateway", BootID: "boot"}, f.supported, nil
}
func (f *trackingLower) SubmitTrackedCommand(_ context.Context, _ uint64, request map[string]any) (domain.TrackedCommand, error) {
	f.submitted++
	f.request = request
	f.value = domain.TrackedCommand{SchemaVersion: 1, ManagerInstanceID: request["manager_instance_id"].(string), CommandID: request["command_id"].(string), GatewayInstanceID: "gateway", BootID: "boot", PortID: request["port_id"].(string), RequestHash: domain.CommandRequestHash(request["port_id"].(string), request["hex_data"].(string)), Accepted: true, Phase: "Executing", Revision: 1, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	return f.value, f.submitErr
}

func TestCommandAuditFailureDoesNotBlockDispatchOrKeepPortLocked(t *testing.T) {
	r := seededRuntime(t)
	if err := r.openSession("s"); err != nil {
		t.Fatal(err)
	}
	lower := &trackingLower{fakeLowerClient: fakeLowerClient{writeStatus: domain.StatusGood}, supported: true}
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: lower, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", 1)
	broken, err := store.Open(context.Background(), ":memory:", store.PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	_ = broken.Close()
	r.tracking.audit.repo = broken
	status, err := r.WriteCommand(context.Background(), "s", testKey("COM1_Send"), "1108")
	if err != nil || status != domain.StatusGood || lower.submitted != 1 {
		t.Fatalf("audit fault blocked control: %v %v", status, err)
	}
	lower.value.Phase = "AckReceived"
	lower.value.Revision = 2
	lower.value.AttemptCount = 1
	lower.value.SentCount = 1
	target, _ := r.captureLowerCommandTarget("source-a", "COM1", false)
	_ = r.syncCommandEvidence(context.Background(), "source-a", target)
	if _, ok := r.routes.PendingCommand("source-a", "COM1"); ok {
		t.Fatal("disk failure retained physical command lock")
	}
	detail, err := r.CommandDetail(context.Background(), lower.value.CommandID)
	if err != nil || detail.Command.State != domain.CommandReceived || !detail.Trace.HistoryGap {
		t.Fatalf("volatile audit %v %+v", err, detail)
	}
	if r.tracking.audit.health() == "" {
		t.Fatal("missing degradation warning")
	}
	r.tracking.audit.repo = r.repo
	r.retryDirtyCommands()
	persisted, err := r.repo.CommandDetail(context.Background(), lower.value.CommandID)
	if err != nil || persisted.Command.State != domain.CommandReceived || persisted.Trace.Snapshot.Phase != "AckReceived" || !persisted.Trace.HistoryGap {
		t.Fatalf("recovery lost evidence %v %+v", err, persisted)
	}
	if lower.submitted != 1 {
		t.Fatal("audit recovery replayed control")
	}
}
func (f *trackingLower) TrackedCommandEvents(context.Context, uint64, string, uint64) (domain.CommandEventBatch, error) {
	return domain.CommandEventBatch{}, errors.New("audit disk full")
}
func (f *trackingLower) GetTrackedCommand(context.Context, uint64, string, string) (*domain.TrackedCommand, error) {
	return &f.value, nil
}

func TestTrackedDispatchNeverFallsBackAndLegacyAckCannotFinish(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "lost_response"}[ambiguous], func(t *testing.T) {
			r := seededRuntime(t)
			if err := r.openSession("s"); err != nil {
				t.Fatal(err)
			}
			lower := &trackingLower{fakeLowerClient: fakeLowerClient{writeStatus: domain.StatusGood}, supported: true}
			if ambiguous {
				lower.submitErr = context.DeadlineExceeded
			}
			r.mu.Lock()
			installTestLower(r, "source-a", lowerHandle{client: lower, generation: 1, cancel: func() {}})
			r.statuses["source-a"] = connectedStatus("source-a", 1)
			r.mu.Unlock()
			primeWriteAck(r, "source-a", "COM1", 1)
			status, err := r.WriteCommand(context.Background(), "s", testKey("COM1_Send"), "1108")
			if !ambiguous && (err != nil || status != domain.StatusGood) {
				t.Fatalf("%x %v", status, err)
			}
			if lower.submitted != 1 || lower.lastWrite != nil {
				t.Fatalf("duplicate legacy dispatch: %d %v", lower.submitted, lower.lastWrite)
			}
			if ambiguous {
				return
			}
			r.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
			if _, ok := r.routes.PendingCommand("source-a", "COM1"); !ok {
				t.Fatal("uncorrelated ACK completed tracked command")
			}
			lower.value.Phase = "AckReceived"
			lower.value.Revision = 2
			lower.value.AttemptCount = 1
			lower.value.SentCount = 1
			target, _ := r.captureLowerCommandTarget("source-a", "COM1", false)
			_ = r.syncCommandEvidence(context.Background(), "source-a", target)
			if _, ok := r.routes.PendingCommand("source-a", "COM1"); ok {
				t.Fatal("volatile ACK did not finish during audit failure")
			}
		})
	}
}
