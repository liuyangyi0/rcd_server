package app

import (
	"context"
	"errors"
	"rcd_manager/internal/domain"
	"testing"
	"time"
)

type syncInputLower struct {
	fakeLowerClient
	epoch uint64
	fail  error
}

func (c *syncInputLower) WriteAtEpoch(ctx context.Context, key domain.NodeKey, value any, epoch uint64) (domain.StatusCode, error) {
	c.epoch = epoch
	if c.fail != nil {
		return domain.StatusBadNoCommunication, c.fail
	}
	return c.Write(ctx, key, value)
}
func (c *syncInputLower) CallAtEpoch(ctx context.Context, key domain.NodeKey, value string, _ uint64) (string, domain.StatusCode, error) {
	return c.Call(ctx, key, value)
}

func syncInputRuntime(t *testing.T) (*Runtime, *syncInputLower, *syncInputLower, domain.NodeKey) {
	t.Helper()
	r := seededRuntime(t)
	key := testKey("DATA.Buildings.11UBE.Fire")
	node := testNode(key.Identifier)
	node.Writable = true
	node.DataType = domain.DataTypeUInt32
	a, b := &syncInputLower{fakeLowerClient: fakeLowerClient{writeStatus: domain.StatusGood}}, &syncInputLower{fakeLowerClient: fakeLowerClient{writeStatus: domain.StatusGood}}
	for id, c := range map[string]*syncInputLower{"source-a": a, "source-b": b} {
		r.OnCatalog(domain.SourceCatalog{SourceID: id, GroupID: "line", State: domain.CatalogReady, DiscoveredAt: time.Now(), Nodes: []domain.CatalogNode{node}})
		r.mu.Lock()
		installTestLower(r, id, lowerHandle{client: c, generation: 1, cancel: func() {}})
		r.statuses[id] = connectedStatus(id, 7)
		r.mu.Unlock()
	}
	if err := r.openSession("sender"); err != nil {
		t.Fatal(err)
	}
	return r, a, b, key
}

func TestSynchronizationInputsForwardEqualWritesAndPinSelectedEpoch(t *testing.T) {
	r, a, b, key := syncInputRuntime(t)
	a.writeHook = func() {
		if _, err := r.selectSource("sender", "line", "source-b", 1, "test"); !errors.Is(err, domain.ErrGroupPending) {
			t.Fatalf("selection not fenced: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		status, err := r.WriteCommand(context.Background(), "sender", key, uint32(7))
		if err != nil || !status.IsGood() {
			t.Fatalf("write=%x %v", status, err)
		}
		if r.routes.PendingCount("line") != 0 {
			t.Fatal("state refresh awaiting nonexistent ACK")
		}
	}
	if a.epoch != 7 || a.lastWrite != uint32(7) || b.lastWrite != nil {
		t.Fatal("wrong target/epoch")
	}
	if _, err := r.selectSource("sender", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	if s, e := r.WriteCommand(context.Background(), "sender", key, uint32(9)); e != nil || !s.IsGood() {
		t.Fatal(s, e)
	}
	if b.lastWrite != uint32(9) || a.lastWrite != uint32(7) {
		t.Fatal("route not switched")
	}
}

func TestSynchronizationInputsRejectBadTypeMissingNodeAndNoReplay(t *testing.T) {
	r, a, b, key := syncInputRuntime(t)
	if s, _ := r.WriteCommand(context.Background(), "sender", key, "7"); s.IsGood() {
		t.Fatal("wrong type")
	}
	wrong := key
	wrong.NamespaceURI = "other"
	if s, _ := r.WriteCommand(context.Background(), "sender", wrong, uint32(7)); s.IsGood() {
		t.Fatal("wrong namespace")
	}
	wrong = key
	wrong.Identifier += "missing"
	if s, _ := r.WriteCommand(context.Background(), "sender", wrong, uint32(7)); s.IsGood() {
		t.Fatal("missing point")
	}
	a.fail = errors.New("reply lost")
	if s, _ := r.WriteCommand(context.Background(), "sender", key, uint32(7)); s.IsGood() {
		t.Fatal("ambiguous result pretends success")
	}
	if b.lastWrite != nil || r.routes.PendingCount("line") != 0 {
		t.Fatal("fallback/retry or leaked reservation")
	}
	r.closing.Store(true)
	if s, _ := r.WriteCommand(context.Background(), "sender", key, uint32(7)); s != domain.StatusBadOutOfService {
		t.Fatal(s)
	}
}

func TestSynchronizationInputsRespectMaintenanceAndExcludeDiagnosticDifferences(t *testing.T) {
	r, a, _, key := syncInputRuntime(t)
	r.configMu.Lock()
	r.release.Active = true
	r.configMu.Unlock()
	if s, _ := r.WriteCommand(context.Background(), "sender", key, uint32(7)); s != domain.StatusBadWaitingForResponse {
		t.Fatal(s)
	}
	if a.lastWrite != nil || r.routes.TotalPending() != 0 {
		t.Fatal("maintenance dispatch")
	}
	r.configMu.Lock()
	r.release.Active = false
	r.configMu.Unlock()
	if err := r.maintenance.restoreFence("source-a", "unknown-job"); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.WriteCommand(context.Background(), "sender", key, uint32(7)); s != domain.StatusBadWaitingForResponse {
		t.Fatal(s)
	}
	if rawRedundancyPoint(indexedPoint{Node: testNode("RcdSync.sync.TX.Status")}) {
		t.Fatal("input diagnostic entered source comparison")
	}
}
