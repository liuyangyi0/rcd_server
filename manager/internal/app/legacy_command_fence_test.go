package app

import (
	"context"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"testing"
)

func TestLegacyDispatchDiscardsProvisionalAckBeforeReadBaseline(t *testing.T) {
	r := seededRuntime(t)
	if err := r.openSession("session"); err != nil {
		t.Fatal(err)
	}
	client := &fakeLowerClient{writeStatus: domain.StatusGood}
	client.legacyBaselineHook = func() {
		r.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	}
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: client, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", 1)
	status, err := r.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "1108")
	if err != nil || !status.IsGood() {
		t.Fatal(status, err)
	}
	if r.routes.PendingCount("line") != 1 {
		t.Fatal("old provisional ACK completed new command")
	}
	r.OnSample("source-a", testKey("COM1_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
	if r.routes.PendingCount("line") != 0 {
		t.Fatal("fresh command terminal did not release reservation")
	}
}

type unfencedReviewLower struct{ opc.LowerClient }

func TestLegacyDispatchWithoutDeliveryFenceDoesNotFallbackToPlainWrite(t *testing.T) {
	r := seededRuntime(t)
	if err := r.openSession("session"); err != nil {
		t.Fatal(err)
	}
	plain := &fakeLowerClient{writeStatus: domain.StatusGood}
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: &unfencedReviewLower{LowerClient: plain}, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", 1)
	status, err := r.WriteCommand(context.Background(), "session", testKey("COM1_Send"), "1108")
	if err == nil || status != domain.StatusBadNotSupported || plain.lastWrite != nil {
		t.Fatal("unsafe adapter dispatched", status, err, plain.lastWrite)
	}
}
