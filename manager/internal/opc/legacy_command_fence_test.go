package opc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
)

type legacyFenceClient struct {
	response            *gu.ReadResponse
	readError           error
	readHook, writeHook func()
	writes              atomic.Int32
}

func (c *legacyFenceClient) Read(context.Context, *gu.ReadRequest) (*gu.ReadResponse, error) {
	if c.readHook != nil {
		c.readHook()
	}
	return c.response, c.readError
}
func (c *legacyFenceClient) Write(context.Context, *gu.WriteRequest) (*gu.WriteResponse, error) {
	c.writes.Add(1)
	if c.writeHook != nil {
		c.writeHook()
	}
	return &gu.WriteResponse{Results: []gu.StatusCode{gu.StatusOK}}, nil
}
func legacyFenceFixture(t *testing.T) (*LowerSupervisor, *ackProbeSink, domain.NodeKey, *gu.NodeID, time.Time) {
	t.Helper()
	sink := &ackProbeSink{samples: make(chan domain.Sample, 10)}
	s := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "isolated"}}, sink: sink}
	s.epoch.Store(7)
	key := domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}
	id := gu.NewStringNodeID(2, key.Identifier)
	at := time.Now().UTC()
	s.emitCommandSample(id.String(), key, domain.DataTypeString, ackReadResponse("0", at.Add(-time.Millisecond)).Results[0], 7)
	<-sink.samples
	return s, sink, key, id, at
}

func TestLegacyCommandFenceRejectsOldAckAndRetainsFastFreshTerminal(t *testing.T) {
	for _, fast := range []bool{false, true} {
		t.Run(map[bool]string{false: "delayed-old-only", true: "coalesced-fast-ack"}[fast], func(t *testing.T) {
			s, sink, key, id, at := legacyFenceFixture(t)
			client := &legacyFenceClient{response: ackReadResponse("1", at)}
			client.readHook = func() {
				// Old terminal arrives after the coordinator reserved a new command.
				s.emitCommandSample(id.String(), key, domain.DataTypeString, ackReadResponse("1", at).Results[0], 7)
			}
			client.writeHook = func() {
				if fast {
					s.emitCommandSample(id.String(), key, domain.DataTypeString, ackReadResponse("2", at.Add(time.Millisecond)).Results[0], 7)
					s.emitCommandSample(id.String(), key, domain.DataTypeString, ackReadResponse("1", at.Add(2*time.Millisecond)).Results[0], 7)
				}
			}
			status, err := s.writeFencedLegacyCommand(context.Background(), client, id, key, domain.DataTypeString, "1108", 7, func() bool {
				if len(sink.samples) != 0 {
					t.Fatal("old ACK escaped before baseline acceptance")
				}
				return true
			})
			if err != nil || status != domain.StatusGood || client.writes.Load() != 1 {
				t.Fatal(status, err, client.writes.Load())
			}
			if fast {
				if len(sink.samples) != 1 || (<-sink.samples).Value != "1" {
					t.Fatal("fast terminal was lost or old ACK leaked")
				}
			} else if len(sink.samples) != 0 {
				t.Fatal("old ACK completed new Write")
			}
			for _, stamp := range []time.Time{at, at.Add(-time.Millisecond), {}} {
				if s.emitCommandSample(id.String(), key, domain.DataTypeString, ackReadResponse("1", stamp).Results[0], 7) {
					t.Fatal("stale/zero timestamp escaped strict fence", stamp)
				}
			}
		})
	}
}

func TestLegacyCommandFenceMissingBaselineOrReservationNeverWrites(t *testing.T) {
	for _, scenario := range []string{"read-error", "nil-response", "zero-time", "bad-quality", "previous-waiting", "expired-reservation", "clock-backwards", "epoch-change"} {
		t.Run(scenario, func(t *testing.T) {
			s, sink, key, id, at := legacyFenceFixture(t)
			client := &legacyFenceClient{response: ackReadResponse("1", at)}
			switch scenario {
			case "read-error":
				client.readError = errors.New("reply lost")
			case "nil-response":
				client.response = nil
			case "zero-time":
				client.response.Results[0].ServerTimestamp = time.Time{}
			case "bad-quality":
				client.response.Results[0].Status = gu.StatusBadNoCommunication
			case "previous-waiting":
				client.response = ackReadResponse("2", at)
			case "clock-backwards":
				client.response = ackReadResponse("1", at.Add(-time.Second))
			case "epoch-change":
				client.readHook = func() { s.epoch.Store(8) }
			}
			status, err := s.writeFencedLegacyCommand(context.Background(), client, id, key, domain.DataTypeString, "1108", 7, func() bool { return scenario != "expired-reservation" })
			if err == nil || status.IsGood() || client.writes.Load() != 0 || len(sink.samples) != 0 {
				t.Fatal("unconfirmed Write sent", status, err, client.writes.Load())
			}
			if len(s.commandFences) != 0 {
				t.Fatal("failed baseline retained gate")
			}
		})
	}
}

func TestLegacyCommandFenceDoesNotBlockOtherNodesAndRejectsDuplicateDispatch(t *testing.T) {
	s, sink, key, id, at := legacyFenceFixture(t)
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	client := &legacyFenceClient{response: ackReadResponse("1", at), readHook: func() { close(entered); <-resume }}
	go func() {
		_, err := s.writeFencedLegacyCommand(context.Background(), client, id, key, domain.DataTypeString, "1108", 7, func() bool { return true })
		finished <- err
	}()
	<-entered
	otherDone := make(chan struct{})
	go func() {
		s.emitCommandSample("ns=2;s=COM2_Send", domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM2_Send"}, domain.DataTypeString, ackReadResponse("1", at).Results[0], 7)
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		close(resume)
		t.Fatal("network read blocked other command samples")
	}
	<-sink.samples
	status, err := s.writeFencedLegacyCommand(context.Background(), &legacyFenceClient{response: ackReadResponse("1", at)}, id, key, domain.DataTypeString, "2208", 7, nil)
	if err == nil || status != domain.StatusBadWaitingForResponse {
		close(resume)
		t.Fatal("duplicate dispatch admitted", status, err)
	}
	close(resume)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	s.epoch.Store(8)
	if !s.emitCommandSample(id.String(), key, domain.DataTypeString, ackReadResponse("0", time.Time{}).Results[0], 8) {
		t.Fatal("new epoch inherited old strict floor")
	}
}
