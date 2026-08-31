package opc

import (
	"sync"
	"testing"
	"time"

	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
)

type recordingSampleSink struct {
	NopLowerSink
	values []domain.Sample
}

func (s *recordingSampleSink) OnSample(_ string, _ domain.NodeKey, sample domain.Sample) {
	s.values = append(s.values, sample)
}

func TestOrdinarySamplesPreserveValueAndQualityWithNonmonotonicServerClock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delta  time.Duration
		value  uint32
		status gu.StatusCode
	}{
		{"clock_steps_back", -time.Minute, 12, gu.StatusOK},
		{"different_value_same_timestamp", 0, 12, gu.StatusOK},
		{"bad_quality_same_timestamp", 0, 11, gu.StatusBadNoCommunication},
		{"bad_quality_clock_steps_back", -time.Minute, 11, gu.StatusBadNoCommunication},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSampleSink{}
			supervisor, err := NewLowerSupervisor(LowerConfig{Source: domain.Source{
				ID: "source-a", GroupID: "group-a", Endpoint: "opc.tcp://127.0.0.1:4845",
			}}, sink)
			if err != nil {
				t.Fatal(err)
			}
			const epoch = 7
			supervisor.epoch.Store(epoch)
			key := domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "A_VALUE"}
			stamp := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
			first := &gu.DataValue{Value: gu.MustVariant(uint32(11)), Status: gu.StatusOK, ServerTimestamp: stamp, SourceTimestamp: stamp}
			second := &gu.DataValue{Value: gu.MustVariant(tc.value), Status: tc.status, ServerTimestamp: stamp.Add(tc.delta), SourceTimestamp: stamp.Add(tc.delta)}
			if !supervisor.emitObservedSample("ns=2;s=A_VALUE", key, domain.DataTypeUInt32, first, epoch) {
				t.Fatal("initial ordinary point was not delivered")
			}
			accepted := supervisor.emitObservedSample("ns=2;s=A_VALUE", key, domain.DataTypeUInt32, second, epoch)
			if !accepted || len(sink.values) != 2 {
				t.Fatalf("changed ordinary point dropped: accepted=%t delivered=%d retained=%#v expected_value=%d expected_status=%s clock_delta=%s", accepted, len(sink.values), sink.values[len(sink.values)-1], tc.value, tc.status, tc.delta)
			}
			last := sink.values[1]
			if last.Value != tc.value || last.StatusCode != domain.StatusCode(tc.status) || !last.SourceTimestamp.Equal(second.SourceTimestamp) || last.Epoch != epoch {
				t.Fatalf("ordinary sample changed during forwarding: %#v", last)
			}
			if len(supervisor.versions) != 0 {
				t.Fatal("ordinary points must not allocate command ACK version state")
			}
		})
	}
}

func TestCommandVariableClassificationDoesNotTreatReadOnlySendNamesAsCommands(t *testing.T) {
	for _, test := range []struct {
		name     string
		dataType domain.DataType
		writable bool
		want     bool
	}{
		{"COM1_Send", domain.DataTypeString, true, true},
		{"FLOW_Send", domain.DataTypeString, false, false},
		{"FLOW_Send", domain.DataTypeUInt32, false, false},
		{"FLOW_Send", domain.DataTypeDouble, false, false},
		{"FLOW_Send", domain.DataTypeUInt32, true, false},
		{"OTHER", domain.DataTypeString, true, false},
	} {
		node := legacyVariable(test.name, test.dataType, test.writable)
		if got := isCommandVariable(node); got != test.want {
			t.Errorf("%s type=%s writable=%v: command=%v want=%v", test.name, test.dataType, test.writable, got, test.want)
		}
	}
}

func TestReadOnlyStringSendPointPreservesEqualTimestampChanges(t *testing.T) {
	sink := &recordingSampleSink{}
	supervisor := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink}
	supervisor.epoch.Store(7)
	node := observedNode{key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "FLOW_Send"}, dataType: domain.DataTypeString}
	stamp := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
	for _, text := range []string{"1", "3"} {
		value := &gu.DataValue{Value: gu.MustVariant(text), Status: gu.StatusOK, ServerTimestamp: stamp}
		if !supervisor.emitMonitoredSample("ns=2;s=FLOW_Send", node, value, 7) {
			t.Fatalf("read-only point %q was incorrectly filtered as an ACK", text)
		}
	}
	if len(sink.values) != 2 || sink.values[1].Value != "3" {
		t.Fatalf("samples=%#v", sink.values)
	}
}

func TestCommandSamplesRejectRetainedAndOutOfOrderAcks(t *testing.T) {
	sink := &recordingSampleSink{}
	supervisor := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink}
	supervisor.epoch.Store(7)
	node := observedNode{key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}, dataType: domain.DataTypeString, command: true}
	stamp := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		ack      string
		delta    time.Duration
		accepted bool
	}{
		{"3", 0, true},
		{"2", 2 * time.Millisecond, true},
		{"1", time.Millisecond, false},
		{"2", 2 * time.Millisecond, false},
		{"1", 3 * time.Millisecond, true},
		{"1", 3 * time.Millisecond, false},
	} {
		value := ackReadResponse(test.ack, stamp.Add(test.delta)).Results[0]
		if got := supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, value, 7); got != test.accepted {
			t.Fatalf("ACK %q delta=%s accepted=%v want=%v", test.ack, test.delta, got, test.accepted)
		}
	}
	if len(sink.values) != 3 || sink.values[0].Value != "3" || sink.values[1].Value != "2" || sink.values[2].Value != "1" {
		t.Fatalf("accepted ACKs=%#v", sink.values)
	}
}

func TestSamplesRejectOldEpochButAcceptNewEpochWithEarlierClock(t *testing.T) {
	for _, command := range []bool{false, true} {
		sink := &recordingSampleSink{}
		supervisor := &LowerSupervisor{cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink}
		supervisor.epoch.Store(7)
		node := observedNode{key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}, dataType: domain.DataTypeString, command: command}
		stamp := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
		if !supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("1", stamp).Results[0], 7) {
			t.Fatal("initial epoch sample was not accepted")
		}
		supervisor.epoch.Store(8)
		if supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("3", stamp.Add(time.Minute)).Results[0], 7) {
			t.Fatalf("late epoch was accepted; command=%v", command)
		}
		if !supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("2", stamp.Add(-time.Hour)).Results[0], 8) {
			t.Fatalf("new epoch retained old clock floor; command=%v", command)
		}
		if len(sink.values) != 2 || sink.values[1].Epoch != 8 || sink.values[1].Value != "2" {
			t.Fatalf("epoch samples; command=%v values=%#v", command, sink.values)
		}
	}
}

type blockingSampleSink struct {
	NopLowerSink
	entered  chan struct{}
	release  chan struct{}
	statuses chan LowerStatus
	first    sync.Once
}

func (s *blockingSampleSink) OnSample(string, domain.NodeKey, domain.Sample) {
	s.first.Do(func() {
		close(s.entered)
		<-s.release
	})
}

func (s *blockingSampleSink) OnLowerStatus(status LowerStatus) {
	s.statuses <- status
}

func TestSampleFromPreviousEpochCannotPublishCurrentEpochStatus(t *testing.T) {
	for _, test := range []struct {
		name    string
		command bool
	}{
		{name: "ordinary sample"},
		{name: "command ACK", command: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &blockingSampleSink{entered: make(chan struct{}), release: make(chan struct{}), statuses: make(chan LowerStatus, 4)}
			supervisor := &LowerSupervisor{
				cfg: LowerConfig{Source: domain.Source{ID: "source-a"}}, sink: sink,
				status: LowerStatus{SourceID: "source-a", Epoch: 7, Catalog: domain.CatalogReady},
			}
			supervisor.epoch.Store(7)
			node := observedNode{key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}, dataType: domain.DataTypeString, command: test.command}
			stamp := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
			done := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(sink.release) })
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("blocked old-epoch sample did not finish")
				}
			})
			go func() {
				defer close(done)
				supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("1", stamp).Results[0], 7)
			}()
			select {
			case <-sink.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("old-epoch sample did not enter the sink")
			}
			// The replacement connection has its own status before the old sink
			// finishes. Its catalog may still be recovering from an earlier error.
			supervisor.mu.Lock()
			supervisor.epoch.Store(8)
			supervisor.status = LowerStatus{SourceID: "source-a", Epoch: 8, Catalog: domain.CatalogConflict, LastDataAt: stamp, UpdatedAt: stamp}
			supervisor.mu.Unlock()
			releaseOnce.Do(func() { close(sink.release) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("old-epoch sample did not return")
			}
			status := supervisor.Status()
			if !status.LastDataAt.Equal(stamp) || !status.UpdatedAt.Equal(stamp) || status.Epoch != 8 {
				t.Fatalf("old sample modified the new epoch: %#v", status)
			}
			select {
			case stale := <-sink.statuses:
				t.Fatalf("old sample emitted a relabeled current-epoch status: %#v", stale)
			default:
			}
			supervisor.mu.Lock()
			supervisor.status.Catalog = domain.CatalogReady
			supervisor.mu.Unlock()
			if !supervisor.emitMonitoredSample("ns=2;s=COM1_Send", node, ackReadResponse("2", stamp.Add(time.Second)).Results[0], 8) {
				t.Fatal("current-epoch sample was not accepted")
			}
			select {
			case current := <-sink.statuses:
				if current.Epoch != 8 || current.Catalog != domain.CatalogReady || current.LastDataAt.Equal(stamp) {
					t.Fatalf("current-epoch status=%#v", current)
				}
			default:
				t.Fatal("the old callback consumed the new epoch's status emission budget")
			}
		})
	}
}
