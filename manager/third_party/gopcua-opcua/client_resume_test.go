package opcua

import (
	"context"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

// This is a protocol peer, not a second Client or a replacement subscription.
// It drops the TCP connection after accepting Publish(2), keeps the Session and
// Subscription alive, replays notification 2, and requires another real
// Publish on the new SecureChannel before sending notification 3.
func TestReconnectOriginalSessionResumesOriginalSubscription(t *testing.T) {
	testResumeScenario(t, resumeNoFailure)
}

func TestReconnectRetainsIdentityAcrossSecondDisconnect(t *testing.T) {
	for _, tt := range []struct {
		name    string
		failure resumeFailure
	}{
		{"activate response lost", resumeLoseActivateResponse},
		{"transfer not applied", resumeLoseTransferRequest},
		{"transfer applied but response lost", resumeLoseTransferResponse},
	} {
		t.Run(tt.name, func(t *testing.T) { testResumeScenario(t, tt.failure) })
	}
}

func testResumeScenario(t *testing.T, failure resumeFailure) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peer := newResumePeer(t, ctx, resumePeerConfig{failure: failure})
	c, err := NewClient(peer.endpoint,
		SecurityPolicy("None"), SecurityMode(ua.MessageSecurityModeNone),
		AutoReconnect(true), ReconnectInterval(10*time.Millisecond), RequestTimeout(time.Second))
	require.NoError(t, err)
	require.NoError(t, c.Connect(ctx))
	defer c.Close(context.Background())
	session := c.Session()
	notifs := make(chan *PublishNotificationData, 8)
	sub, err := c.Subscribe(ctx, &SubscriptionParameters{
		Interval: 10 * time.Millisecond, MaxKeepAliveCount: 10, LifetimeCount: 100,
	}, notifs)
	require.NoError(t, err)
	_, err = sub.Monitor(ctx, ua.TimestampsToReturnBoth,
		NewMonitoredItemCreateRequestWithDefaults(ua.NewNumericNodeID(1, 1), ua.AttributeIDValue, 42))
	require.NoError(t, err)
	subID := sub.SubscriptionID
	require.Equal(t, int32(11), resumeValue(t, ctx, notifs))
	close(peer.cut)
	require.Equal(t, int32(22), resumeValue(t, ctx, notifs), "lost notification must be republished first")
	require.Equal(t, int32(33), resumeValue(t, ctx, notifs), "Publish must restart without a new subscription")
	if failure == resumeLoseTransferRequest || failure == resumeLoseTransferResponse {
		require.NotSame(t, session, c.Session(), "only an explicit invalid Session allows replacement")
		require.EqualValues(t, 201, c.Session().resp.AuthenticationToken.IntID())
	} else {
		require.Same(t, session, c.Session())
	}
	require.Equal(t, subID, sub.SubscriptionID)
	require.Equal(t, []uint32{subID}, c.SubscriptionIDs())
	require.Equal(t, Connected, c.State())
	require.NoError(t, c.UpdateNamespaces(ctx), "Read must still work after Republish's normal ServiceFault")
	peer.mu.Lock()
	defer peer.mu.Unlock()
	wantSessions, wantActivations, wantTransfers, wantChannels := 1, 2, 0, 2
	wantReplay := []uint32{2, 3}
	wantTokens := []uint32{200, 200}
	switch failure {
	case resumeLoseActivateResponse:
		wantActivations, wantChannels = 3, 3
		wantTokens = []uint32{200, 200, 200}
	case resumeLoseTransferRequest, resumeLoseTransferResponse:
		wantSessions, wantActivations, wantTransfers, wantChannels = 2, 4, 2, 3
		wantTokens = []uint32{200, 200, 201, 201}
		require.Equal(t, []uint32{201, 201}, peer.transferTokens, "retry must use the same target Session")
		if failure == resumeLoseTransferRequest {
			wantReplay = []uint32{2} // a successful transfer supplies the available sequence list
		}
	}
	require.Equal(t, wantSessions, peer.sessionCreates)
	require.Equal(t, wantActivations, peer.sessionActivations)
	require.Equal(t, wantTokens, peer.activationTokens)
	require.Equal(t, wantChannels, peer.channels)
	require.Equal(t, 1, peer.subscriptionCreates)
	require.Zero(t, peer.subscriptionDeletes, "uncertain recovery must never delete a valid subscription")
	require.Equal(t, wantReplay, peer.republished)
	require.True(t, peer.replayedAck, "replayed notification must be acknowledged by the resumed Publish")
	require.Equal(t, wantTransfers, peer.transfers)
}

func TestRepublishMissDoesNotReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer := newResumePeer(t, ctx)
	c, err := NewClient(peer.endpoint, AutoReconnect(true), RequestTimeout(time.Second))
	require.NoError(t, err)
	require.NoError(t, c.Connect(ctx))
	defer c.Close(context.Background())
	session, channel := c.Session(), c.SecureChannel()
	err = c.Send(ctx, &ua.RepublishRequest{SubscriptionID: 300, RetransmitSequenceNumber: 99}, func(ua.Response) error { return nil })
	require.ErrorIs(t, err, ua.StatusBadMessageNotAvailable)
	// Allow the connection monitor to consume the actual ServiceFault, not
	// merely the synchronous request result. A normal replay terminator must
	// not initiate a new channel even when no subscription is registered.
	require.Never(t, func() bool { return c.State() != Connected || c.SecureChannel() != channel }, 100*time.Millisecond, time.Millisecond)
	require.NoError(t, c.UpdateNamespaces(ctx))
	require.Same(t, session, c.Session())
	peer.mu.Lock()
	defer peer.mu.Unlock()
	require.Equal(t, 1, peer.channels)
	require.Equal(t, 1, peer.sessionCreates)
}

func TestCloseWhileDialIsOpeningHasNoLateTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted, release := make(chan struct{}), make(chan struct{})
	peer := newResumePeer(t, ctx, resumePeerConfig{openAccepted: accepted, openRelease: release})
	c, err := NewClient(peer.endpoint, RequestTimeout(2*time.Second))
	require.NoError(t, err)
	dialDone := make(chan error, 1)
	go func() { dialDone <- c.Dial(ctx) }()
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("peer did not accept the pending Dial")
	}
	require.Eventually(t, func() bool {
		c.connMu.Lock()
		defer c.connMu.Unlock()
		return c.conn != nil
	}, time.Second, time.Millisecond, "pending Open must be visible to Close")
	closeDone := make(chan struct{})
	go func() { _ = c.Close(ctx); close(closeDone) }()
	select {
	case <-closeDone:
	case <-ctx.Done():
		t.Fatal("Close blocked behind network I/O")
	}
	close(release)
	select {
	case err := <-dialDone:
		require.Error(t, err, "a Dial overlapping Close must not publish a new transport")
	case <-ctx.Done():
		t.Fatal("cancelled Dial did not stop")
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	require.Nil(t, c.conn)
	require.Nil(t, c.SecureChannel())
	require.Equal(t, Closed, c.State())
}

func resumeValue(t *testing.T, ctx context.Context, notifs <-chan *PublishNotificationData) int32 {
	t.Helper()
	select {
	case n := <-notifs:
		require.NoError(t, n.Error)
		data, ok := n.Value.(*ua.DataChangeNotification)
		require.True(t, ok)
		require.Len(t, data.MonitoredItems, 1)
		require.EqualValues(t, 42, data.MonitoredItems[0].ClientHandle)
		return data.MonitoredItems[0].Value.Value.Value().(int32)
	case <-ctx.Done():
		t.Fatal("timed out waiting for a real Publish/Republish notification")
		return 0
	}
}

type resumeFailure uint8

const (
	resumeNoFailure resumeFailure = iota
	resumeLoseActivateResponse
	resumeLoseTransferRequest
	resumeLoseTransferResponse
)

type resumePeerConfig struct {
	failure      resumeFailure
	openAccepted chan struct{}
	openRelease  chan struct{}
}

type resumePeer struct {
	endpoint string
	ctx      context.Context
	cfg      resumePeerConfig
	cut      chan struct{}
	mu       sync.Mutex
	wg       sync.WaitGroup
	conns    []*uacp.Conn

	channels            int
	firstPublish        int
	sessionCreates      int
	sessionActivations  int
	subscriptionCreates int
	subscriptionDeletes int
	subscriptionOwner   uint32
	transfers           int
	activationTokens    []uint32
	transferTokens      []uint32
	republished         []uint32
	replayedAck         bool
}

func newResumePeer(t *testing.T, parent context.Context, config ...resumePeerConfig) *resumePeer {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	ln, err := uacp.Listen(ctx, "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	p := &resumePeer{ctx: ctx, endpoint: "opc.tcp://" + ln.Addr().String(), cut: make(chan struct{}), subscriptionOwner: 200}
	if len(config) > 0 {
		p.cfg = config[0]
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.channels++
			channel := p.channels
			p.mu.Unlock()
			p.wg.Add(1)
			go p.serve(conn, channel)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		p.mu.Lock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		done := make(chan struct{})
		go func() { p.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("test peer did not stop")
		}
	})
	return p
}

func (p *resumePeer) serve(conn *uacp.Conn, channel int) {
	defer p.wg.Done()
	defer conn.Close()
	if channel == 1 && p.cfg.openAccepted != nil {
		close(p.cfg.openAccepted)
		select {
		case <-p.cfg.openRelease:
		case <-p.ctx.Done():
			return
		}
	}
	sc, err := uasc.NewServerSecureChannel(p.endpoint, conn, &uasc.Config{
		SecurityMode: ua.MessageSecurityModeNone, SecurityPolicyURI: ua.SecurityPolicyURINone,
		RequestTimeout: time.Second,
	}, make(chan error, 1), uint32(channel), 1, uint32(channel))
	if err != nil {
		return
	}
	for {
		msg := sc.Receive(p.ctx)
		if msg.Err != nil {
			return
		}
		req := msg.Request()
		if req == nil {
			continue
		} // OpenSecureChannel is answered by the transport.
		if _, ok := req.(*ua.CloseSecureChannelRequest); ok {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			response := p.response(req, conn, channel)
			if response != nil {
				_ = sc.SendResponseWithContext(p.ctx, msg.RequestID, response)
			}
		}()
	}
}

func (p *resumePeer) response(request ua.Request, conn *uacp.Conn, channel int) ua.Response {
	header := &ua.ResponseHeader{RequestHandle: request.Header().RequestHandle, Timestamp: time.Now(),
		ServiceDiagnostics: &ua.DiagnosticInfo{}, AdditionalHeader: ua.NewExtensionObject(nil)}
	switch req := request.(type) {
	case *ua.CreateSessionRequest:
		p.mu.Lock()
		p.sessionCreates++
		number := uint32(p.sessionCreates - 1)
		p.mu.Unlock()
		return &ua.CreateSessionResponse{ResponseHeader: header,
			SessionID: ua.NewNumericNodeID(1, 100+number), AuthenticationToken: ua.NewNumericNodeID(1, 200+number),
			RevisedSessionTimeout: 60000, ServerSignature: &ua.SignatureData{},
			ServerEndpoints: []*ua.EndpointDescription{{EndpointURL: p.endpoint,
				Server:            &ua.ApplicationDescription{ApplicationURI: "urn:resume-test", ApplicationName: ua.NewLocalizedText("resume-test"), ApplicationType: ua.ApplicationTypeServer},
				SecurityPolicyURI: ua.SecurityPolicyURINone,
				SecurityMode:      ua.MessageSecurityModeNone, UserIdentityTokens: []*ua.UserTokenPolicy{{
					PolicyID: "Anonymous", TokenType: ua.UserTokenTypeAnonymous,
				}}}},
		}
	case *ua.ActivateSessionRequest:
		token := req.RequestHeader.AuthenticationToken.IntID()
		p.mu.Lock()
		p.sessionActivations++
		activation := p.sessionActivations
		p.activationTokens = append(p.activationTokens, token)
		invalid := token < 200 || token >= 200+uint32(p.sessionCreates)
		if (p.cfg.failure == resumeLoseTransferRequest || p.cfg.failure == resumeLoseTransferResponse) && channel > 1 && token == 200 {
			invalid = true // only this explicit status permits a new Session
		}
		p.mu.Unlock()
		if invalid {
			header.ServiceResult = ua.StatusBadSessionIDInvalid
			return &ua.ServiceFault{ResponseHeader: header}
		}
		if p.cfg.failure == resumeLoseActivateResponse && activation == 2 {
			_ = conn.Close() // activation accepted, response lost
			return nil
		}
		return &ua.ActivateSessionResponse{ResponseHeader: header}
	case *ua.ReadRequest:
		v, _ := ua.NewVariant([]string{"http://opcfoundation.org/UA/", "urn:resume-test"})
		dv := &ua.DataValue{Value: v}
		dv.UpdateMask()
		return &ua.ReadResponse{ResponseHeader: header, Results: []*ua.DataValue{dv}}
	case *ua.CreateSubscriptionRequest:
		p.mu.Lock()
		p.subscriptionCreates++
		p.mu.Unlock()
		return &ua.CreateSubscriptionResponse{ResponseHeader: header, SubscriptionID: 300,
			RevisedPublishingInterval: 10, RevisedLifetimeCount: 100, RevisedMaxKeepAliveCount: 10}
	case *ua.CreateMonitoredItemsRequest:
		return &ua.CreateMonitoredItemsResponse{ResponseHeader: header,
			Results: []*ua.MonitoredItemCreateResult{{MonitoredItemID: 400, RevisedQueueSize: 10}}}
	case *ua.PublishRequest:
		p.mu.Lock()
		p.firstPublish++
		count := p.firstPublish
		for _, ack := range req.SubscriptionAcknowledgements {
			if channel > 1 && ack.SubscriptionID == 300 && ack.SequenceNumber == 2 {
				p.replayedAck = true
			}
		}
		p.mu.Unlock()
		if channel == 1 && count > 1 {
			select {
			case <-p.cut:
				_ = conn.Close()
			case <-p.ctx.Done():
			}
			return nil
		}
		sequence, value := uint32(1), int32(11)
		if channel > 1 {
			if count > 3 {
				<-p.ctx.Done()
				return nil
			}
			sequence, value = 3, 33
		}
		return &ua.PublishResponse{ResponseHeader: header, SubscriptionID: 300,
			AvailableSequenceNumbers: []uint32{sequence}, NotificationMessage: resumeNotification(sequence, value),
			Results: make([]ua.StatusCode, len(req.SubscriptionAcknowledgements))}
	case *ua.RepublishRequest:
		p.mu.Lock()
		p.republished = append(p.republished, req.RetransmitSequenceNumber)
		p.mu.Unlock()
		if req.SubscriptionID == 300 && req.RetransmitSequenceNumber == 2 {
			return &ua.RepublishResponse{ResponseHeader: header, NotificationMessage: resumeNotification(2, 22)}
		}
		header.ServiceResult = ua.StatusBadMessageNotAvailable
		return &ua.ServiceFault{ResponseHeader: header}
	case *ua.TransferSubscriptionsRequest:
		token := req.RequestHeader.AuthenticationToken.IntID()
		p.mu.Lock()
		p.transfers++
		attempt := p.transfers
		p.transferTokens = append(p.transferTokens, token)
		owner := p.subscriptionOwner
		if p.cfg.failure != resumeLoseTransferRequest || attempt > 1 {
			p.subscriptionOwner = token
		}
		p.mu.Unlock()
		if p.cfg.failure == resumeLoseTransferRequest || p.cfg.failure == resumeLoseTransferResponse {
			if attempt == 1 {
				_ = conn.Close()
				return nil
			}
			result := &ua.TransferResult{AvailableSequenceNumbers: []uint32{2}}
			if owner == token {
				result.StatusCode = ua.StatusBadNothingToDo
				result.AvailableSequenceNumbers = nil
			}
			return &ua.TransferSubscriptionsResponse{ResponseHeader: header, Results: []*ua.TransferResult{result}}
		}
		header.ServiceResult = ua.StatusBadServiceUnsupported
		return &ua.ServiceFault{ResponseHeader: header}
	case *ua.DeleteSubscriptionsRequest:
		p.mu.Lock()
		p.subscriptionDeletes += len(req.SubscriptionIDs)
		p.mu.Unlock()
		return &ua.DeleteSubscriptionsResponse{ResponseHeader: header, Results: make([]ua.StatusCode, len(req.SubscriptionIDs))}
	case *ua.CloseSessionRequest:
		return &ua.CloseSessionResponse{ResponseHeader: header}
	default:
		header.ServiceResult = ua.StatusBadServiceUnsupported
		return &ua.ServiceFault{ResponseHeader: header}
	}
}

func resumeNotification(sequence uint32, value int32) *ua.NotificationMessage {
	v, _ := ua.NewVariant(value)
	dv := &ua.DataValue{Value: v}
	dv.UpdateMask()
	return &ua.NotificationMessage{SequenceNumber: sequence, PublishTime: time.Now(),
		NotificationData: []*ua.ExtensionObject{ua.NewExtensionObject(&ua.DataChangeNotification{
			MonitoredItems: []*ua.MonitoredItemNotification{{ClientHandle: 42, Value: dv}},
		})}}
}

func TestSubscriptionPauseResumeIsCoalescedAndGenerationSafe(t *testing.T) {
	c, err := NewClient("opc.tcp://unused:0")
	require.NoError(t, err)
	ctx := context.Background()
	c.resumeSubscriptions(ctx)
	oldPublish := c.publishState.Load()
	for range 100 {
		c.pauseSubscriptions(ctx)
	}
	c.resumeSubscriptions(ctx)
	require.Zero(t, c.publishState.Load()&1)
	require.False(t, c.publishState.CompareAndSwap(oldPublish, oldPublish|1), "old Publish error must not pause recovery")
	for range 100 {
		c.resumeSubscriptions(ctx)
	}
	require.Len(t, c.resumech, 1, "wakeups must coalesce instead of blocking the caller")
	require.Zero(t, c.publishState.Load()&1)
}

func TestRepublishResumesSequenceAndACKWithoutReplacingSubscription(t *testing.T) {
	for _, initial := range []uint32{2, math.MaxUint32} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			c, err := NewClient("opc.tcp://unused:0")
			require.NoError(t, err)
			var sent []uint32
			stub := &stubClient{send: func(req ua.Request, h func(ua.Response) error) error {
				r, ok := req.(*ua.RepublishRequest)
				require.True(t, ok, "must not recreate a healthy subscription")
				sent = append(sent, r.RetransmitSequenceNumber)
				if len(sent) > 1 {
					return ua.StatusBadMessageNotAvailable
				}
				return h(&ua.RepublishResponse{ResponseHeader: &ua.ResponseHeader{},
					NotificationMessage: resumeNotification(initial, 42)})
			}}
			sub := newTestSubscription(t, c, stub, 300)
			sub.nextSeq = initial
			notifs := make(chan *PublishNotificationData, 2)
			sub.Notifs = notifs
			action, active := c.republishOrRecreateSubscriptions(context.Background(), c.SubscriptionIDs(), nil, nil)
			require.Equal(t, none, action)
			require.Equal(t, 1, active)
			require.Equal(t, []uint32{initial, nextNotificationSequence(initial)}, sent)
			require.Equal(t, []uint32{300}, c.SubscriptionIDs())
			require.Equal(t, initial, sub.lastSeq)
			require.Equal(t, nextNotificationSequence(initial), sub.nextSeq)
			require.Equal(t, []*ua.SubscriptionAcknowledgement{{SubscriptionID: 300, SequenceNumber: initial}}, c.pendingAcks)
			require.Len(t, notifs, 1)
		})
	}
}

func TestRepublishFailuresDoNotPretendRecoverySucceeded(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		action reconnectAction
	}{
		{"session gone", ua.StatusBadSessionIDInvalid, recreateSession},
		{"session closed", ua.StatusBadSessionClosed, recreateSession},
		{"session inactive", ua.StatusBadSessionNotActivated, createSecureChannel},
		{"transport closed", io.EOF, createSecureChannel},
		{"request timeout", ua.StatusBadTimeout, createSecureChannel},
		{"malformed response", ua.StatusBadDecodingError, createSecureChannel},
		{"unknown transport error", fmt.Errorf("broken transport"), createSecureChannel},
		{"secure channel gone", ua.StatusBadSecureChannelIDInvalid, createSecureChannel},
		{"request cancelled", context.Canceled, createSecureChannel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewClient("opc.tcp://unused:0")
			require.NoError(t, err)
			stub := &stubClient{send: func(req ua.Request, h func(ua.Response) error) error {
				_, ok := req.(*ua.RepublishRequest)
				require.True(t, ok, "transport or Session failure must not delete a valid subscription")
				return tt.err
			}}
			newTestSubscription(t, c, stub, 300)
			action, active := c.republishOrRecreateSubscriptions(context.Background(), []uint32{300}, nil, nil)
			require.Equal(t, tt.action, action)
			require.Zero(t, active)
			require.Equal(t, []uint32{300}, c.SubscriptionIDs())
		})
	}
}

func TestRecoveryFailureClassificationPreservesUncertainIdentity(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		action reconnectAction
	}{
		{"invalid session", ua.StatusBadSessionIDInvalid, recreateSession},
		{"closed session", ua.StatusBadSessionClosed, recreateSession},
		{"wrapped closed session", fmt.Errorf("activate: %w", ua.StatusBadSessionClosed), recreateSession},
		{"not activated", ua.StatusBadSessionNotActivated, createSecureChannel},
		{"activate response lost", io.EOF, createSecureChannel},
		{"activate timeout", ua.StatusBadTimeout, createSecureChannel},
		{"cancelled", context.Canceled, createSecureChannel},
		{"unknown failure", fmt.Errorf("uncertain"), createSecureChannel},
	} {
		t.Run(tt.name, func(t *testing.T) { require.Equal(t, tt.action, sessionRecoveryAction(tt.err)) })
	}
}

func TestTransferRecoveryNeverDeletesOnUncertainResponse(t *testing.T) {
	response := func(results ...*ua.TransferResult) *ua.TransferSubscriptionsResponse {
		return &ua.TransferSubscriptionsResponse{ResponseHeader: &ua.ResponseHeader{}, Results: results}
	}
	for _, tt := range []struct {
		name             string
		res              *ua.TransferSubscriptionsResponse
		err              error
		action           reconnectAction
		replay, recreate []uint32
	}{
		{"success", response(&ua.TransferResult{AvailableSequenceNumbers: []uint32{2}}), nil, restoreSubscriptions, []uint32{300}, nil},
		{"already transferred", response(&ua.TransferResult{StatusCode: ua.StatusBadNothingToDo}), nil, restoreSubscriptions, []uint32{300}, nil},
		{"missing subscription", response(&ua.TransferResult{StatusCode: ua.StatusBadSubscriptionIDInvalid}), nil, restoreSubscriptions, nil, []uint32{300}},
		{"unsupported", nil, ua.StatusBadServiceUnsupported, restoreSubscriptions, nil, []uint32{300}},
		{"not implemented", nil, ua.StatusBadNotImplemented, restoreSubscriptions, nil, []uint32{300}},
		{"response lost", nil, io.EOF, createSecureChannel, nil, nil},
		{"timeout", nil, ua.StatusBadTimeout, createSecureChannel, nil, nil},
		{"invalid session", nil, ua.StatusBadSessionIDInvalid, recreateSession, nil, nil},
		{"closed session", nil, ua.StatusBadSessionClosed, recreateSession, nil, nil},
		{"inactive session", nil, ua.StatusBadSessionNotActivated, createSecureChannel, nil, nil},
		{"nil response", nil, nil, createSecureChannel, nil, nil},
		{"nil header", &ua.TransferSubscriptionsResponse{}, nil, createSecureChannel, nil, nil},
		{"missing operation", response(), nil, createSecureChannel, nil, nil},
		{"extra operation", response(&ua.TransferResult{}, &ua.TransferResult{}), nil, createSecureChannel, nil, nil},
		{"nil operation", response(nil), nil, createSecureChannel, nil, nil},
		{"operation session invalid", response(&ua.TransferResult{StatusCode: ua.StatusBadSessionIDInvalid}), nil, recreateSession, nil, nil},
		{"operation access denied", response(&ua.TransferResult{StatusCode: ua.StatusBadUserAccessDenied}), nil, createSecureChannel, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			action, replay, recreate, _ := planSubscriptionTransfer([]uint32{300}, tt.res, tt.err)
			require.Equal(t, tt.action, action)
			require.Equal(t, tt.replay, replay)
			require.Equal(t, tt.recreate, recreate)
		})
	}
	action, replay, recreate, seqs := planSubscriptionTransfer(nil, nil, nil)
	require.Equal(t, restoreSubscriptions, action)
	require.Empty(t, replay)
	require.Empty(t, recreate)
	require.Empty(t, seqs)
}

func TestRepublishMissingSubscriptionIsRecreatedAndCountedOnce(t *testing.T) {
	c, err := NewClient("opc.tcp://unused:0")
	require.NoError(t, err)
	create := recreateResponder(301, ua.StatusOK)
	stub := &stubClient{send: func(req ua.Request, h func(ua.Response) error) error {
		if _, ok := req.(*ua.RepublishRequest); ok {
			return ua.StatusBadSubscriptionIDInvalid
		}
		return create(req, h)
	}}
	newTestSubscription(t, c, stub, 300)
	action, active := c.republishOrRecreateSubscriptions(context.Background(), []uint32{300}, nil, nil)
	require.Equal(t, none, action)
	require.Equal(t, 1, active, "a failed replay is not itself a restored subscription")
	require.Equal(t, []uint32{301}, c.SubscriptionIDs())
}

func TestRepublishMalformedResponseDoesNotAdvanceOrLoop(t *testing.T) {
	for _, message := range []*ua.NotificationMessage{nil, {SequenceNumber: 2}, resumeNotification(1, 42)} {
		c, err := NewClient("opc.tcp://unused:0")
		require.NoError(t, err)
		stub := &stubClient{send: func(req ua.Request, h func(ua.Response) error) error {
			return h(&ua.RepublishResponse{ResponseHeader: &ua.ResponseHeader{}, NotificationMessage: message})
		}}
		sub := newTestSubscription(t, c, stub, 300)
		sub.nextSeq = 2
		require.ErrorIs(t, c.sendRepublishRequests(context.Background(), sub, nil), ua.StatusBadDecodingError)
		require.EqualValues(t, 2, sub.nextSeq)
		require.Empty(t, c.pendingAcks)
		require.Len(t, stub.sentRequests(), 1)
	}
}
