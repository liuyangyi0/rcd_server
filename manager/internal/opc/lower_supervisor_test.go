package opc

import (
	"context"
	"sync"
	"testing"
	"time"

	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

type ackProbeSink struct{ samples chan domain.Sample }

func (s *ackProbeSink) OnLowerStatus(LowerStatus)      {}
func (s *ackProbeSink) OnCatalog(domain.SourceCatalog) {}
func (s *ackProbeSink) OnSample(_ string, _ domain.NodeKey, sample domain.Sample) {
	s.samples <- sample
}

type sequenceValueReader struct {
	mu        sync.Mutex
	responses []*gu.ReadResponse
	index     int
}

func (r *sequenceValueReader) Read(context.Context, *gu.ReadRequest) (*gu.ReadResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.index
	if index >= len(r.responses) {
		index = len(r.responses) - 1
	} else {
		r.index++
	}
	return r.responses[index], nil
}

func ackReadResponse(value string, timestamp time.Time) *gu.ReadResponse {
	return &gu.ReadResponse{Results: []*gu.DataValue{{
		EncodingMask: gu.DataValueValue | gu.DataValueStatusCode | gu.DataValueServerTimestamp,
		Value:        gu.MustVariant(value), Status: gu.StatusOK, ServerTimestamp: timestamp,
	}}}
}

func TestLowerSupervisorDefaultsProbeInsideRustSessionTimeout(t *testing.T) {
	supervisor, err := NewLowerSupervisor(LowerConfig{Source: domain.Source{
		ID: "source-a", GroupID: "group-a", Endpoint: "opc.tcp://127.0.0.1:4840",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if supervisor.cfg.SessionProbeInterval != DefaultSessionProbeInterval || supervisor.cfg.SessionProbeInterval >= time.Minute {
		t.Fatalf("session probe interval = %s", supervisor.cfg.SessionProbeInterval)
	}
}

func TestLowerSupervisorRejectsProbeAtOrBeyondRustSessionTimeout(t *testing.T) {
	_, err := NewLowerSupervisor(LowerConfig{
		Source:               domain.Source{ID: "source-a", GroupID: "group-a", Endpoint: "opc.tcp://127.0.0.1:4840"},
		SessionProbeInterval: time.Minute,
	}, nil)
	if err == nil {
		t.Fatal("expected the incompatible probe interval to be rejected")
	}
}

func TestLowerDialEndpointDoesNotFollowStaleServerAdvertisement(t *testing.T) {
	configured := "opc.tcp://192.168.10.46:4840"
	advertised := "opc.tcp://192.168.10.40:4840/"
	if got := lowerDialEndpoint(configured, advertised); got != configured {
		t.Fatalf("dial endpoint = %q, want configured endpoint %q", got, configured)
	}
	if got := lowerDialEndpoint("", advertised); got != advertised {
		t.Fatalf("fallback dial endpoint = %q, want advertised endpoint %q", got, advertised)
	}
}

func TestCommandAckProbeEmitsOnlyTimestampedTransitionsAndStopsAtTerminal(t *testing.T) {
	baseline := time.Now().UTC()
	sink := &ackProbeSink{samples: make(chan domain.Sample, 4)}
	supervisor := &LowerSupervisor{
		cfg:  LowerConfig{Source: domain.Source{ID: "source-a"}, OperationTimeout: time.Second},
		sink: sink,
	}
	supervisor.epoch.Store(7)
	reader := &sequenceValueReader{responses: []*gu.ReadResponse{
		ackReadResponse("3", baseline),
		ackReadResponse("2", baseline.Add(time.Millisecond)),
		ackReadResponse("3", baseline.Add(2*time.Millisecond)),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	supervisor.pollCommandAck(ctx, reader, gu.NewStringNodeID(2, "COM1_Send"),
		domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}, domain.DataTypeString, 7, baseline)

	first := <-sink.samples
	second := <-sink.samples
	if first.Value != "2" || second.Value != "3" || first.Epoch != 7 || second.Epoch != 7 {
		t.Fatalf("probed samples = %#v, %#v", first, second)
	}
	select {
	case unexpected := <-sink.samples:
		t.Fatalf("unexpected duplicate sample %#v", unexpected)
	default:
	}
}

func TestCommandAckProbeDoesNotReuseUnchangedTerminalValue(t *testing.T) {
	baseline := time.Now().UTC()
	sink := &ackProbeSink{samples: make(chan domain.Sample, 1)}
	supervisor := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink}
	supervisor.epoch.Store(9)
	reader := &sequenceValueReader{responses: []*gu.ReadResponse{ackReadResponse("3", baseline)}}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	supervisor.pollCommandAck(ctx, reader, gu.NewStringNodeID(2, "COM1_Send"),
		domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}, domain.DataTypeString, 9, baseline)
	select {
	case unexpected := <-sink.samples:
		t.Fatalf("stale terminal was reused: %#v", unexpected)
	default:
	}
}

func TestCommandAckProbeCannotOverwriteNewerSubscriptionValue(t *testing.T) {
	baseline := time.Now().UTC()
	sink := &ackProbeSink{samples: make(chan domain.Sample, 3)}
	supervisor := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink}
	supervisor.epoch.Store(7)
	key := domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}
	node := observedNode{key: key, dataType: domain.DataTypeString, command: true}
	if !supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("2", baseline.Add(2*time.Millisecond)).Results[0], 7) {
		t.Fatal("subscription Waiting was not accepted")
	}
	reader := &sequenceValueReader{responses: []*gu.ReadResponse{ackReadResponse("1", baseline.Add(time.Millisecond))}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	supervisor.pollCommandAck(ctx, reader, gu.NewStringNodeID(2, "COM1_Send"), key, domain.DataTypeString, 7, baseline)
	if first := <-sink.samples; first.Value != "2" {
		t.Fatalf("initial sample=%#v", first)
	}
	select {
	case stale := <-sink.samples:
		t.Fatalf("older probe ACK replaced newer subscription state: %#v", stale)
	default:
	}
	if !supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("1", baseline.Add(3*time.Millisecond)).Results[0], 7) {
		t.Fatal("fresh subscription terminal was not accepted")
	}
	if final := <-sink.samples; final.Value != "1" {
		t.Fatalf("fresh terminal=%#v", final)
	}
}

type epochChangingValueReader struct {
	supervisor *LowerSupervisor
	response   *gu.ReadResponse
}

func (r *epochChangingValueReader) Read(context.Context, *gu.ReadRequest) (*gu.ReadResponse, error) {
	r.supervisor.epoch.Add(1)
	return r.response, nil
}

func TestCommandAckProbeRejectsResponseAfterEpochChanges(t *testing.T) {
	baseline := time.Now().UTC()
	sink := &ackProbeSink{samples: make(chan domain.Sample, 1)}
	supervisor := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink}
	supervisor.epoch.Store(7)
	reader := &epochChangingValueReader{supervisor: supervisor, response: ackReadResponse("1", baseline.Add(time.Millisecond))}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	supervisor.pollCommandAck(ctx, reader, gu.NewStringNodeID(2, "COM1_Send"),
		domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}, domain.DataTypeString, 7, baseline)
	select {
	case late := <-sink.samples:
		t.Fatalf("probe response from a closed epoch was accepted: %#v", late)
	default:
	}
}
