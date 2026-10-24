package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/awcullen/opcua/ua"
)

// The transport implements net.Conn but opens no sockets. Responses still go
// through the real UA encoder and channel writer, including the failure path.
type publishTestConn struct {
	sync.Mutex
	writes      [][]byte
	writeErr    error
	beforeWrite func()
	closed      bool
}

func (c *publishTestConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *publishTestConn) Write(p []byte) (int, error) {
	c.Lock()
	hook := c.beforeWrite
	c.beforeWrite = nil
	c.Unlock()
	if hook != nil {
		hook()
	}
	c.Lock()
	defer c.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (c *publishTestConn) Close() error {
	c.Lock()
	c.closed = true
	c.Unlock()
	return nil
}
func (c *publishTestConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *publishTestConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *publishTestConn) SetDeadline(time.Time) error      { return nil }
func (c *publishTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *publishTestConn) SetWriteDeadline(time.Time) error { return nil }

func newPublishTestChannel(srv *Server) (*serverSecureChannel, *publishTestConn) {
	srv.maxBufferSize = defaultMaxBufferSize
	conn := &publishTestConn{}
	ch := newServerSecureChannel(srv, conn, false)
	ch.securityMode = ua.MessageSecurityModeNone
	ch.sendBuffer = make([]byte, defaultMaxBufferSize)
	ch.tokenExpiration = time.Now().Add(time.Hour)
	return ch, conn
}

func newPublishTestRequest(handle uint32) *ua.PublishRequest {
	return &ua.PublishRequest{RequestHeader: ua.RequestHeader{
		Timestamp: time.Now(), RequestHandle: handle, TimeoutHint: 60_000,
	}}
}

func newPublishTestSubscription(srv *Server, session *Session) (*Subscription, *DataChangeMonitoredItem) {
	sub := NewSubscription(srv.subscriptionManager, session, 250, 100, 3, 1, true, 0)
	item := &DataChangeMonitoredItem{
		id: 1, clientHandle: 123, monitoringMode: ua.MonitoringModeReporting,
		queueSize: 16, discardOldest: true, timestampsToReturn: ua.TimestampsToReturnBoth,
		sub: sub, srv: srv, session: session,
	}
	sub.AppendItem(item)
	return sub, item
}

func queuePublishTestValue(item *DataChangeMonitoredItem, value int32) {
	item.Lock()
	defer item.Unlock()
	item.queue.PushBack(ua.NewDataValue(value, ua.Good, time.Now(), 0, time.Now(), 0))
}

func sendTestPublish(t *testing.T, path string, sub *Subscription, ch *serverSecureChannel, requestID uint32, results []ua.StatusCode) error {
	t.Helper()
	req := newPublishTestRequest(requestID)
	req.SubscriptionAcknowledgements = make([]ua.SubscriptionAcknowledgement, len(results))
	if path == "late" {
		sub.Lock()
		sub.isLate = true
		sub.Unlock()
		handled, err := sub.handleLatePublishRequest(ch, requestID, req, results)
		if !handled && err == nil {
			t.Fatal("late Publish was not handled")
		}
		return err
	}
	if err := sub.session.addPublishRequest(ch, requestID, req, results); err != nil {
		return err
	}
	return sub.publish(time.Now())
}

func readTestPublishResponse(t *testing.T, conn *publishTestConn, requestID uint32) ua.PublishResponse {
	t.Helper()
	conn.Lock()
	writes := append([][]byte(nil), conn.writes...)
	conn.Unlock()
	if len(writes) != 1 || len(writes[0]) < 24 {
		t.Fatalf("wire chunks = %d, want one complete small UA response", len(writes))
	}
	chunk := writes[0]
	if got := binary.LittleEndian.Uint32(chunk[20:24]); got != requestID {
		t.Fatalf("response request id = %d, want %d", got, requestID)
	}
	decoder := ua.NewBinaryDecoder(bytes.NewReader(chunk[24:]), ua.NewEncodingContext())
	var typeID ua.NodeID
	if err := decoder.ReadNodeID(&typeID); err != nil || typeID != ua.ObjectIDPublishResponseEncodingDefaultBinary {
		t.Fatalf("response type = %v, decode error = %v", typeID, err)
	}
	var response ua.PublishResponse
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func requireRepublishValue(t *testing.T, sub *Subscription, seq uint32, want int32) {
	t.Helper()
	found, err := sub.republish(seq, func(nm ua.NotificationMessage) error {
		if nm.SequenceNumber != seq || len(nm.NotificationData) != 1 {
			t.Fatalf("republished message = %#v", nm)
		}
		change, ok := nm.NotificationData[0].(ua.DataChangeNotification)
		if !ok || len(change.MonitoredItems) != 1 || change.MonitoredItems[0].Value.Value != want {
			t.Fatalf("republished value for %d = %#v, want %d", seq, nm.NotificationData, want)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("Republish(%d) found=%v error=%v", seq, found, err)
	}
}

func TestPublishWriteFailureNeverReusesNotificationSequence(t *testing.T) {
	for _, path := range []string{"scheduled", "late"} {
		for _, first := range []uint32{2, math.MaxUint32} {
			t.Run(fmt.Sprintf("%s/sequence-%d", path, first), func(t *testing.T) {
				srv := newForkUnitServer()
				session := newForkSession(srv, "publish-write-failure")
				old, failed := newPublishTestChannel(srv)
				session.SetSecureChannelId(old.ChannelID())
				sub, item := newPublishTestSubscription(srv, session)
				sub.nextSequenceNumber = first
				sub.lifetimeCount = sub.maxLifetimeCount - 1
				queuePublishTestValue(item, 11)
				writeErr := errors.New("injected transport write failure")
				failed.writeErr = writeErr
				if err := sendTestPublish(t, path, sub, old, 1, nil); !errors.Is(err, writeErr) {
					t.Fatalf("failed Publish error = %v", err)
				}
				if !old.IsClosed() {
					t.Fatal("failed transport stayed eligible for queued Publish")
				}
				next := first + 1
				if next == 0 {
					next = 1
				}
				if got := sub.diagnosticsSnapshot(); got.NextSequenceNumber != next || got.UnacknowledgedMessageCount != 1 || got.CurrentLifetimeCount != 0 {
					t.Fatalf("diagnostics after failed write = %#v", got)
				}
				requireRepublishValue(t, sub, first, 11)
				// Republish is not ACK: retrying still returns the same old value.
				requireRepublishValue(t, sub, first, 11)

				current, wire := newPublishTestChannel(srv)
				session.SetSecureChannelId(current.ChannelID())
				queuePublishTestValue(item, 101)
				if err := sendTestPublish(t, path, sub, current, 2, nil); err != nil {
					t.Fatal(err)
				}
				response := readTestPublishResponse(t, wire, 2)
				if response.NotificationMessage.SequenceNumber != next || !reflect.DeepEqual(response.AvailableSequenceNumbers, []uint32{first, next}) {
					t.Fatalf("replacement-channel response = %#v", response)
				}
				requireRepublishValue(t, sub, next, 101)
				if !sub.acknowledge(first) || sub.acknowledge(first) {
					t.Fatal("ACK must remove the failed-write message exactly once")
				}
				if found, err := sub.republish(first, func(ua.NotificationMessage) error { return nil }); found || err != nil {
					t.Fatalf("ACKed message remained available: found=%v error=%v", found, err)
				}
				requireRepublishValue(t, sub, next, 101)
				if !sub.acknowledge(next) {
					t.Fatal("old ACK removed the distinct new notification")
				}
			})
		}
	}
}

func TestPublishKeepAliveDoesNotConsumeSequenceOrReplaceAckResults(t *testing.T) {
	for _, path := range []string{"scheduled", "late"} {
		t.Run(path, func(t *testing.T) {
			srv := newForkUnitServer()
			session := newForkSession(srv, "keepalive-failure")
			old, failed := newPublishTestChannel(srv)
			session.SetSecureChannelId(old.ChannelID())
			sub, _ := newPublishTestSubscription(srv, session)
			sub.nextSequenceNumber = 37
			sub.lifetimeCount = sub.maxLifetimeCount - 1
			failed.writeErr = errors.New("keepalive transport failure")
			if err := sendTestPublish(t, path, sub, old, 1, nil); err == nil {
				t.Fatal("expected keepalive write failure")
			}
			if sub.lifetimeCount != 0 {
				t.Fatal("failed keepalive response did not reset the valid request's lifetime timer")
			}
			current, wire := newPublishTestChannel(srv)
			session.SetSecureChannelId(current.ChannelID())
			results := []ua.StatusCode{ua.BadSequenceNumberUnknown, ua.BadSubscriptionIDInvalid}
			if err := sendTestPublish(t, path, sub, current, 2, results); err != nil {
				t.Fatal(err)
			}
			response := readTestPublishResponse(t, wire, 2)
			if response.NotificationMessage.SequenceNumber != 37 || len(response.NotificationMessage.NotificationData) != 0 || !reflect.DeepEqual(response.Results, results) {
				t.Fatalf("keepalive response = %#v", response)
			}
			if sub.nextSequenceNumber != 37 || sub.retransmissionQueue.Len() != 0 {
				t.Fatal("keepalive consumed a sequence or entered the retransmission queue")
			}
		})
	}
}

func TestClosedPublishChannelDoesNotDrainMonitoredItems(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "closed-channel")
	old, _ := newPublishTestChannel(srv)
	session.SetSecureChannelId(old.ChannelID())
	sub, item := newPublishTestSubscription(srv, session)
	queuePublishTestValue(item, 11)
	if err := session.addPublishRequest(old, 1, newPublishTestRequest(1), nil); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if err := sub.publish(time.Now()); err != nil {
		t.Fatal(err)
	}
	if item.queue.Len() != 1 || sub.retransmissionQueue.Len() != 0 || sub.nextSequenceNumber != 1 {
		t.Fatal("closed-channel request consumed the queued value or a sequence")
	}
	current, wire := newPublishTestChannel(srv)
	session.SetSecureChannelId(current.ChannelID())
	if err := sendTestPublish(t, "scheduled", sub, current, 2, nil); err != nil {
		t.Fatal(err)
	}
	if response := readTestPublishResponse(t, wire, 2); response.NotificationMessage.SequenceNumber != 1 {
		t.Fatalf("first healthy Publish response = %#v", response)
	}
	requireRepublishValue(t, sub, 1, 11)
}

func TestLatePublishRejectsReplacedChannelWithoutDrainingItems(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "late-replaced-channel")
	old, _ := newPublishTestChannel(srv)
	session.SetSecureChannelId(old.ChannelID())
	sub, item := newPublishTestSubscription(srv, session)
	sub.isLate = true
	queuePublishTestValue(item, 11)
	current, _ := newPublishTestChannel(srv)
	session.SetSecureChannelId(current.ChannelID())
	handled, err := sub.handleLatePublishRequest(old, 1, newPublishTestRequest(1), nil)
	if handled || err != ua.BadSecureChannelIDInvalid || item.queue.Len() != 1 || sub.retransmissionQueue.Len() != 0 {
		t.Fatalf("stale late Publish: handled=%v error=%v queue=%d retransmission=%d", handled, err, item.queue.Len(), sub.retransmissionQueue.Len())
	}
}

func TestPublishQueueSkipsStaleRequestsBeforeHealthyRequest(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "skip-stale")
	old, oldWire := newPublishTestChannel(srv)
	current, _ := newPublishTestChannel(srv)
	session.SetSecureChannelId(current.ChannelID())
	// Model a stale entry already queued before a worker observes rebinding.
	session.publishRequests <- &publishOp{ch: old, requestId: 1, req: newPublishTestRequest(1)}
	session.publishRequests <- &publishOp{ch: nil, requestId: 2, req: newPublishTestRequest(2)}
	if err := session.addPublishRequest(current, 3, newPublishTestRequest(3), nil); err != nil {
		t.Fatal(err)
	}
	ch, id, _, _, ok, err := session.removePublishRequest()
	if err != nil || !ok || ch != current || id != 3 || len(oldWire.writes) != 0 {
		t.Fatalf("remove after stale entries: channel=%p id=%d ok=%v error=%v", ch, id, ok, err)
	}
}

func TestPublishTimeoutWriteFailureDoesNotLoseReplacementRequest(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "expired-publish-rebind")
	old, failed := newPublishTestChannel(srv)
	current, _ := newPublishTestChannel(srv)
	session.SetSecureChannelId(old.ChannelID())
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	failed.beforeWrite = func() { close(entered); <-release }
	failed.writeErr = errors.New("old timeout response failed after reactivation")
	req := newPublishTestRequest(1)
	req.Timestamp = time.Now().Add(-time.Minute)
	req.TimeoutHint = 1
	if err := session.addPublishRequest(old, 1, req, nil); err != nil {
		t.Fatal(err)
	}
	type removal struct {
		ch  *serverSecureChannel
		id  uint32
		ok  bool
		err error
	}
	done := make(chan removal, 1)
	go func() {
		ch, id, _, _, ok, err := session.removePublishRequest()
		done <- removal{ch, id, ok, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timeout response was not attempted")
	}
	// Network I/O must not hold the Session lock or block its reactivation.
	activated := make(chan error, 1)
	go func() {
		session.SetSecureChannelId(current.ChannelID())
		activated <- session.addPublishRequest(current, 2, newPublishTestRequest(2), nil)
	}()
	select {
	case err := <-activated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old timeout write held the Session lock during reactivation")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.err != nil || !result.ok || result.ch != current || result.id != 2 {
			t.Fatalf("replacement request lost behind failed timeout response: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("removing a Publish request remained blocked after old write failed")
	}
}

func TestPublishReactivationPreservesConcurrentNewChannelRequests(t *testing.T) {
	srv := newForkUnitServer()
	for iteration := 0; iteration < 100; iteration++ {
		session := newForkSession(srv, "concurrent-reactivation")
		old, _ := newPublishTestChannel(srv)
		current, _ := newPublishTestChannel(srv)
		session.SetSecureChannelId(old.ChannelID())
		if err := session.addPublishRequest(old, 999, newPublishTestRequest(999), nil); err != nil {
			t.Fatal(err)
		}
		session.SetSecureChannelId(current.ChannelID())
		start := make(chan struct{})
		var workers sync.WaitGroup
		for id := uint32(1); id <= 32; id++ {
			workers.Add(1)
			go func(id uint32) {
				defer workers.Done()
				<-start
				if err := session.addPublishRequest(current, id, newPublishTestRequest(id), nil); err != nil {
					t.Errorf("new-channel admission failed: %v", err)
				}
			}(id)
		}
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			for i := 0; i < 64; i++ {
				session.SetSecureChannelId(current.ChannelID())
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			old.Close()
			if err := session.addPublishRequest(old, 1000, newPublishTestRequest(1000), nil); err != ua.BadSecureChannelClosed {
				t.Errorf("closed old-channel admission = %v", err)
			}
		}()
		close(start)
		workers.Wait()
		seen := make(map[uint32]bool)
		for {
			ch, id, _, _, ok, err := session.removePublishRequest()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if ch != current || id < 1 || id > 32 || seen[id] {
				t.Fatalf("stale/duplicate request survived: id=%d channel=%p", id, ch)
			}
			seen[id] = true
		}
		if len(seen) != 32 {
			t.Fatalf("reactivation discarded new requests: got %d, want 32", len(seen))
		}
	}
}

func TestPublishZeroTimeoutHintRemainsEligible(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "unlimited-publish-timeout")
	ch, wire := newPublishTestChannel(srv)
	session.SetSecureChannelId(ch.ChannelID())
	req := newPublishTestRequest(1)
	req.Timestamp = time.Now().Add(-time.Hour)
	req.TimeoutHint = 0
	if err := session.addPublishRequest(ch, 1, req, nil); err != nil {
		t.Fatal(err)
	}
	_, _, got, _, ok, err := session.removePublishRequest()
	if err != nil || !ok || got != req || len(wire.writes) != 0 {
		t.Fatalf("TimeoutHint=0 treated as expired: ok=%v error=%v", ok, err)
	}
}
