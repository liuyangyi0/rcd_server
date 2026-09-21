package opc

import (
	"testing"
	"time"

	awu "github.com/awcullen/opcua/ua"
	"rcd_manager/internal/domain"
)

type communicationBackend struct {
	*fakeBackend
	state                            domain.StationCommunication
	seenSource, seenPort, seenDevice string
}

func (b *communicationBackend) StationCommunication(source, port, device string) domain.StationCommunication {
	b.seenSource, b.seenPort, b.seenDevice = source, port, device
	return b.state
}

func TestCommunicationVariablesAreReadOnlyNumericAndSessionIndependent(t *testing.T) {
	b := &communicationBackend{fakeBackend: &fakeBackend{}}
	defs := stationCommunicationVariables(b, "source-a", "/dev/ttyWCH0", "11")
	node := stationDefinition(t, defs, "CommunicationState")
	if node.writable || node.write != nil || node.dataType != awu.DataTypeIDUInt32 || node.valueRank != awu.ValueRankScalar {
		t.Fatal("incorrect public variable shape")
	}
	for state := uint32(0); state <= 5; state++ {
		b.state = domain.StationCommunication{State: state, ObservedAt: time.Now().UTC()}
		for _, session := range []string{"session-a", "session-b"} {
			value, quality := node.read(session)
			if value != state || quality != awu.Good {
				t.Fatalf("state=%v quality=%v", value, quality)
			}
		}
		if b.seenSource != "source-a" || b.seenPort != "/dev/ttyWCH0" || b.seenDevice != "11" {
			t.Fatal("physical identity changed")
		}
	}
}
