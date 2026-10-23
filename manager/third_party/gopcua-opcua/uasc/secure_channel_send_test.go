package uasc

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
)

// This wire fixture owns an ephemeral loopback socket. It never connects to an
// installed OPC server or uses the deployment's port, credentials or serials.
func sendWireFixture(t *testing.T) (*SecureChannel, *channelInstance, *net.TCPConn) {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	c, err := net.DialTCP("tcp", nil, l.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	peer, err := l.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	// This chunk-level fixture starts after HEL/ACK. Supply the negotiated
	// limits explicitly; nil uses the client's pre-negotiation zero limits.
	conn, err := uacp.NewConn(c, &uacp.Acknowledge{
		ReceiveBufSize: uacp.DefaultReceiveBufSize,
		SendBufSize:    uacp.DefaultSendBufSize,
		MaxMessageSize: uacp.DefaultMaxMessageSize,
		MaxChunkCount:  uacp.DefaultMaxChunkCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := NewSecureChannel("opc.tcp://127.0.0.1:0", conn, &Config{
		SecurityPolicyURI: ua.SecurityPolicyURINone,
		SecurityMode:      ua.MessageSecurityModeNone,
		RequestTimeout:    time.Second,
		Lifetime:          3600000,
	}, make(chan error, 8))
	if err != nil {
		t.Fatal(err)
	}
	instance := newChannelInstance(sc)
	instance.state = channelActive
	instance.secureChannelID, instance.securityTokenID = 1, 1
	instance.maxBodySize = 1024
	sc.activeInstance = instance
	sc.instances[1] = []*channelInstance{instance}
	return sc, instance, peer
}

func readWireChunk(t *testing.T, peer *net.TCPConn) []byte {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(peer, header); err != nil {
		t.Fatal(err)
	}
	size := binary.LittleEndian.Uint32(header[4:])
	if size < 24 || size > 1<<20 {
		t.Fatalf("invalid chunk size %d", size)
	}
	chunk := make([]byte, size)
	copy(chunk, header)
	if _, err := io.ReadFull(peer, chunk[8:]); err != nil {
		t.Fatal(err)
	}
	return chunk
}

func requireWireSequence(t *testing.T, peer *net.TCPConn, want uint32) {
	t.Helper()
	chunk := readWireChunk(t, peer)
	if got := binary.LittleEndian.Uint32(chunk[16:]); got != want {
		t.Fatalf("wire sequence = %d, want %d (an unsent request consumed a sequence)", got, want)
	}
}

func TestSendCancelledRequestDoesNotConsumeSequence(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sc.sendRequestWithTimeout(ctx, &ua.ReadRequest{}, 1, instance, nil, time.Second, func(ua.Response) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	requireWireSequence(t, peer, 1)
	sc.handlersMu.Lock()
	defer sc.handlersMu.Unlock()
	if len(sc.handlers) != 0 {
		t.Fatalf("unsent requests leaked %d response handlers", len(sc.handlers))
	}
}

func TestSendConcurrentCancelledWaitersKeepSequenceContinuous(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	const requests = 12
	instance.Lock()
	var wg sync.WaitGroup
	var errorsSeen atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sc.SendRequest(ctx, &ua.ReadRequest{}, nil, nil); !errors.Is(err, context.Canceled) {
				errorsSeen.Add(1)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
				errorsSeen.Add(1)
			}
		}()
	}
	cancel()
	instance.Unlock()
	wg.Wait()
	if errorsSeen.Load() != 0 {
		t.Fatalf("unexpected send results: %d", errorsSeen.Load())
	}
	for n := uint32(1); n <= requests; n++ {
		requireWireSequence(t, peer, n)
	}
}

// Force cancellation only once an actual chunk has reached Write. This avoids
// timing/sleep based tests and tests a partial logical message, not a cancelled
// request which never reached the transport.
type cancelAfterChunkContext struct {
	context.Context
	instance *channelInstance
	done     chan struct{}
	once     sync.Once
}

func (c *cancelAfterChunkContext) Done() <-chan struct{} {
	if atomic.LoadUint64(&c.instance.bytesSent) > 0 {
		c.once.Do(func() { close(c.done) })
	}
	return c.done
}

func (c *cancelAfterChunkContext) Err() error {
	select {
	case <-c.Done():
		return context.Canceled
	default:
		return nil
	}
}

func TestSendCancelledChunkStreamCannotReuseChannel(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	instance.maxBodySize = 128
	ctx := &cancelAfterChunkContext{Context: context.Background(), instance: instance, done: make(chan struct{})}
	req := &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{
		NodeID: ua.NewStringNodeID(2, "fixture"), AttributeID: ua.AttributeIDValue,
		Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(strings.Repeat("x", 4096))},
	}}}
	err := sc.sendRequestWithTimeout(ctx, req, 1, instance, nil, time.Second, func(ua.Response) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("chunk cancellation: %v", err)
	}
	chunk := readWireChunk(t, peer)
	if chunk[3] != 'C' {
		t.Fatalf("want an incomplete logical message, got chunk %q", chunk[3])
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err == nil {
		t.Fatal("reused a channel after an incomplete message")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1)
	if _, err := peer.Read(b); !errors.Is(err, io.EOF) {
		t.Fatalf("partial message must close its transport, got %v", err)
	}
	sc.handlersMu.Lock()
	defer sc.handlersMu.Unlock()
	if len(sc.handlers) != 0 {
		t.Fatalf("failed stream leaked %d handlers", len(sc.handlers))
	}
}

func TestSendCancelledResponseDoesNotConsumeSequence(t *testing.T) {
	sc, _, peer := sendWireFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sc.SendResponseWithContext(ctx, 1, &ua.ReadResponse{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled response: %v", err)
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	requireWireSequence(t, peer, 1)
}

type failingEncodingRequest struct{ ua.ReadRequest }

func (*failingEncodingRequest) Encode() ([]byte, error) {
	return nil, errors.New("intentional encode failure")
}

func init() { ua.RegisterService(65001, &failingEncodingRequest{}) }

func TestSendEncodingFailureDoesNotConsumeSequenceOrLeakHandler(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	err := sc.sendRequestWithTimeout(context.Background(), &failingEncodingRequest{}, 1, instance, nil, time.Second, func(ua.Response) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "intentional encode failure") {
		t.Fatalf("encode result: %v", err)
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	requireWireSequence(t, peer, 1)
	sc.handlersMu.Lock()
	defer sc.handlersMu.Unlock()
	if len(sc.handlers) != 0 {
		t.Fatalf("encoding failure leaked %d response handlers", len(sc.handlers))
	}
}

type cancelEncodingRequest struct {
	ua.ReadRequest
	cancel context.CancelFunc
}

func (r *cancelEncodingRequest) Encode() ([]byte, error) {
	r.cancel()
	return ua.Encode(&r.ReadRequest)
}

func init() { ua.RegisterService(65002, &cancelEncodingRequest{}) }

func TestSendCancelledDuringEncodingDoesNotConsumeSequence(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := sc.sendRequestWithTimeout(ctx, &cancelEncodingRequest{cancel: cancel}, 1, instance, nil, time.Second, func(ua.Response) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during encode: %v", err)
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	requireWireSequence(t, peer, 1)
	sc.handlersMu.Lock()
	defer sc.handlersMu.Unlock()
	if len(sc.handlers) != 0 {
		t.Fatalf("cancel during encode leaked %d handlers", len(sc.handlers))
	}
}

func TestSendDuplicateHandlerPreservesOriginal(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	original := make(chan *MessageBody, 1)
	sc.handlers[1] = original
	err := sc.sendRequestWithTimeout(context.Background(), &ua.ReadRequest{}, 1, instance, nil, time.Second, func(ua.Response) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "duplicate handler") {
		t.Fatalf("duplicate handler result: %v", err)
	}
	if sc.handlers[1] != original {
		t.Fatal("removed another request's handler")
	}
	if _, err := sc.sendAsyncWithTimeout(context.Background(), &ua.ReadRequest{}, 2, instance, nil, false, time.Second); err != nil {
		t.Fatal(err)
	}
	requireWireSequence(t, peer, 1)
}

func TestSendCancelledAtWrapRestoresExactSequence(t *testing.T) {
	sc, instance, peer := sendWireFixture(t)
	instance.sequenceNumber = math.MaxUint32 - 1023
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := sc.sendRequestWithTimeout(ctx, &cancelEncodingRequest{cancel: cancel}, 1, instance, nil, time.Second, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during wrap encoding: %v", err)
	}
	if instance.sequenceNumber != math.MaxUint32-1023 {
		t.Fatalf("lost pre-wrap sequence: %d", instance.sequenceNumber)
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	requireWireSequence(t, peer, 1)
}
