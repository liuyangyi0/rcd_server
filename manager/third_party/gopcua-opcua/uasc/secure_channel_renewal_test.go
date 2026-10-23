package uasc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

type renewalWirePeer struct {
	requests chan *Message
	errors   chan error
	renewals atomic.Int32
	stop     chan struct{}
}

// A strict wire peer withholds Publish responses independently of OPN. The
// optional renewal gate models a server which accepted OPN but has not replied.
func renewalFixture(t *testing.T, permitRenewal <-chan struct{}) (*SecureChannel, *channelInstance, *renewalWirePeer) {
	t.Helper()
	sc, instance, conn := sendWireFixture(t)
	p := &renewalWirePeer{requests: make(chan *Message, 32), errors: make(chan error, 1), stop: make(chan struct{})}
	t.Cleanup(func() { close(p.stop); _ = sc.Close() })
	go func() {
		var incoming, outgoing uint32
		for {
			m, err := readRenewalMessage(conn)
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					select {
					case p.errors <- err:
					default:
					}
				}
				return
			}
			incoming++
			if m.SequenceNumber != incoming {
				select {
				case p.errors <- fmt.Errorf("wire sequence %d, expected %d", m.SequenceNumber, incoming):
				default:
				}
				_ = conn.Close()
				return
			}
			if m.MessageType == MessageTypeCloseSecureChannel {
				return
			}
			p.requests <- m
			request, ok := m.Service.(*ua.OpenSecureChannelRequest)
			if !ok {
				continue
			}
			count := p.renewals.Add(1)
			if permitRenewal != nil {
				select {
				case <-permitRenewal:
				case <-p.stop:
					return
				}
			}
			outgoing++
			response := &Message{
				MessageHeader: &MessageHeader{
					Header:                   NewHeader(MessageTypeOpenSecureChannel, ChunkTypeFinal, 1),
					AsymmetricSecurityHeader: NewAsymmetricSecurityHeader(ua.SecurityPolicyURINone, nil, nil),
					SequenceHeader:           NewSequenceHeader(outgoing, m.RequestID),
				},
				TypeID: ua.NewFourByteExpandedNodeID(0, id.OpenSecureChannelResponse_Encoding_DefaultBinary),
				Service: &ua.OpenSecureChannelResponse{
					ResponseHeader: &ua.ResponseHeader{
						RequestHandle: request.Header().RequestHandle, Timestamp: time.Now(),
						ServiceDiagnostics: &ua.DiagnosticInfo{}, StringTable: []string{},
						AdditionalHeader: ua.NewExtensionObject(nil),
					},
					SecurityToken: &ua.ChannelSecurityToken{ChannelID: 1, TokenID: uint32(count + 1), CreatedAt: time.Now(), RevisedLifetime: 3600000},
				},
			}
			b, err := response.Encode()
			if err == nil {
				_, err = conn.Write(b)
			}
			if err != nil {
				select {
				case p.errors <- err:
				default:
				}
				return
			}
		}
	}()
	return sc, instance, p
}

func readRenewalMessage(conn net.Conn) (*Message, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(header[4:])
	if size < 24 || size > 1<<20 {
		return nil, fmt.Errorf("invalid message size %d", size)
	}
	b := make([]byte, size)
	copy(b, header)
	if _, err := io.ReadFull(conn, b[8:]); err != nil {
		return nil, err
	}
	m := new(Message)
	_, err := m.Decode(b)
	return m, err
}

func awaitRenewalRequest(t *testing.T, p *renewalWirePeer, typ string) *Message {
	t.Helper()
	select {
	case err := <-p.errors:
		t.Fatal(err)
	case m := <-p.requests:
		if m.MessageType != typ {
			t.Fatalf("message type %s, want %s", m.MessageType, typ)
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("wire peer did not receive request")
	}
	return nil
}

func TestRenewQueuedOldTokenDoesNotReuseSequence(t *testing.T) {
	sc, original, p := renewalFixture(t, nil)
	if err := sc.renew(original); err != nil {
		t.Fatal(err)
	}
	awaitRenewalRequest(t, p, MessageTypeOpenSecureChannel)
	// A scheduled/manual renewal can have captured this token before another
	// renewal completed. It must not copy its obsolete sequence counter.
	if err := sc.renew(original); err != nil {
		t.Fatalf("stale queued renewal: %v", err)
	}
	if got := p.renewals.Load(); got != 1 {
		t.Fatalf("issued %d renewals for one token", got)
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	m := awaitRenewalRequest(t, p, MessageTypeMessage)
	if m.SequenceNumber != 2 {
		t.Fatalf("post-renew sequence %d, want 2", m.SequenceNumber)
	}
}

func TestRenewDoesNotWaitForOutstandingPublishResponse(t *testing.T) {
	sc, original, p := renewalFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- sc.SendRequest(ctx, &ua.PublishRequest{}, nil, func(ua.Response) error { return nil })
	}()
	awaitRenewalRequest(t, p, MessageTypeMessage)
	if err := sc.renew(original); err != nil {
		t.Fatalf("renew behind unanswered Publish: %v", err)
	}
	awaitRenewalRequest(t, p, MessageTypeOpenSecureChannel)
	select {
	case err := <-publishDone:
		t.Fatalf("Publish unexpectedly completed: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-publishDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish waiter leaked")
	}
}

func TestCloseInterruptsRenewalWithoutWaitingForSendGate(t *testing.T) {
	permit := make(chan struct{})
	sc, original, p := renewalFixture(t, permit)
	renewDone := make(chan error, 1)
	go func() { renewDone <- sc.renew(original) }()
	awaitRenewalRequest(t, p, MessageTypeOpenSecureChannel)
	closed := make(chan struct{})
	go func() { _ = sc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Close blocked behind unanswered OPN")
	}
	select {
	case err := <-renewDone:
		if err == nil {
			t.Fatal("unanswered renewal succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt OPN response wait")
	}
}

func TestCancelledSendWaitingForRenewalNeverReachesWire(t *testing.T) {
	permit := make(chan struct{})
	sc, original, p := renewalFixture(t, permit)
	renewDone := make(chan error, 1)
	go func() { renewDone <- sc.renew(original) }()
	awaitRenewalRequest(t, p, MessageTypeOpenSecureChannel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requestDone := make(chan error, 1)
	go func() { requestDone <- sc.SendRequest(ctx, &ua.ReadRequest{}, nil, nil) }()
	select {
	case err := <-requestDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled gate wait: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("cancelled request still waits behind renewal")
	}
	close(permit)
	select {
	case err := <-renewDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("renewal did not complete")
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	m := awaitRenewalRequest(t, p, MessageTypeMessage)
	if m.SequenceNumber != 2 {
		t.Fatalf("queued cancellation consumed a sequence: %d", m.SequenceNumber)
	}
}

func TestRenewalReplyLostCannotReuseOldToken(t *testing.T) {
	permit := make(chan struct{})
	sc, _, p := renewalFixture(t, permit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sc.Renew(ctx) }()
	awaitRenewalRequest(t, p, MessageTypeOpenSecureChannel)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost OPN reply: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Renew ignored cancellation")
	}
	if err := sc.SendRequest(context.Background(), &ua.ReadRequest{}, nil, nil); err == nil {
		t.Fatal("reused old token after an ambiguous OPN result")
	}
}

func TestInitialOpenReplyLostCannotReuseTransport(t *testing.T) {
	permit := make(chan struct{})
	sc, _, p := renewalFixture(t, permit)
	sc.activeInstance = nil
	sc.instances = make(map[uint32][]*channelInstance)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sc.Open(ctx) }()
	awaitRenewalRequest(t, p, MessageTypeOpenSecureChannel)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost initial OPN reply: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Open ignored cancellation")
	}
	if err := sc.Open(context.Background()); err == nil {
		t.Fatal("reissued OPN on an ambiguous initial connection")
	}
	if got := p.renewals.Load(); got != 1 {
		t.Fatalf("ambiguous OPN was sent again: %d", got)
	}
}
