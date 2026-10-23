package opc

import (
	"context"
	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
	"testing"
	"time"
)

type baselineCountingReader struct{ calls int }

func (r *baselineCountingReader) Read(context.Context, *gu.ReadRequest) (*gu.ReadResponse, error) {
	r.calls++
	return &gu.ReadResponse{Results: []*gu.DataValue{{ServerTimestamp: time.Now()}}}, nil
}
func TestSynchronizationWritesDoNotReadAckBaseline(t *testing.T) {
	reader := &baselineCountingReader{}
	for _, name := range []string{"RcdSync.sync.TX.Snapshot", "RcdSync.sync.TX.Header.detectorTemp3", "TransmitSnapshot", "DATA.Buildings.11UBE.Fire", "DATA.PumpStationCtrl[0]"} {
		stamp, ok := commandWriteBaseline(context.Background(), reader, gu.NewStringNodeID(2, name), domain.NodeKey{Identifier: name})
		if ok || !stamp.IsZero() || reader.calls != 0 {
			t.Fatal("synchronization must not wait for ACK baseline")
		}
	}
	_, ok := commandWriteBaseline(context.Background(), reader, gu.NewStringNodeID(2, "tty_Send"), domain.NodeKey{Identifier: "tty_Send"})
	if !ok || reader.calls != 1 {
		t.Fatal("legacy command baseline removed")
	}
}
