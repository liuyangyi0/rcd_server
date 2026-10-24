package opc

import (
	"context"
	"errors"
	gu "github.com/gopcua/opcua/ua"
	"testing"

	"rcd_manager/internal/domain"
)

func TestOperationEnvelopeRejectsMalformedResults(t *testing.T) {
	for _, value := range []string{"", "null", "{}", `{"ok":true}`, `{"ok":true,"result":[]}`, `{"ok":true,"result":null}`, `{"ok":false}`, `{"ok":false,"error":{}}`, `{"ok":true,"result":{},"error":{"code":"bad"}}`} {
		if _, err := decodeOperationEnvelope(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	result, err := decodeOperationEnvelope(`{"ok":true,"result":{"revision":"3","ports":[]}}`)
	if err != nil || string(result) != `{"revision":"3","ports":[]}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

func TestOperationAdmissionAndUATimeout(t *testing.T) {
	supervisor := &LowerSupervisor{}
	for range 4 {
		if err := supervisor.acquireOperationSlot(); err != nil {
			t.Fatal(err)
		}
	}
	var busy *domain.OperationError
	if err := supervisor.acquireOperationSlot(); !errors.As(err, &busy) || busy.Code != "busy" {
		t.Fatalf("admission=%v", err)
	}
	if supervisor.operationCalls.Load() != 4 {
		t.Fatal("rejected call leaked an admission slot")
	}
	if !errors.Is(operationTransportError(gu.StatusBadTimeout), context.DeadlineExceeded) {
		t.Fatal("UA timeout did not map to a deadline")
	}
}

func TestOperationBusinessConflictPreservesRevision(t *testing.T) {
	_, err := decodeOperationEnvelope(`{"ok":false,"error":{"code":"conflict","message":"changed","current_revision":"42"}}`)
	var conflict *domain.OperationError
	if !errors.As(err, &conflict) || conflict.Code != "conflict" || conflict.CurrentRevision != "42" {
		t.Fatalf("err=%v", err)
	}
}

func TestOperationsTargetNeverUsesAnotherEpoch(t *testing.T) {
	supervisor := &LowerSupervisor{status: LowerStatus{Connection: ConnectionConnected, Epoch: 7}, namespaces: map[string]uint16{domain.OperationsNamespaceURI: 11}}
	if _, _, err := supervisor.operationsTarget(7); !errors.Is(err, ErrLowerUnavailable) {
		t.Fatalf("nil client=%v", err)
	}
	// No live connection is fabricated in a unit test; the integration suite
	// verifies URI resolution and pinned dispatch against the Rust instance.
	if _, _, err := supervisor.operationsTarget(0); !errors.Is(err, ErrLowerUnavailable) {
		t.Fatalf("wildcard=%v", err)
	}
}
