package server

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awcullen/opcua/ua"
)

type lifecycleAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *lifecycleAddrConn) RemoteAddr() net.Addr { return c.remote }

func newLifecycleTestChannel(srv *Server, id uint32) (*serverSecureChannel, *publishTestConn) {
	ch, wire := newPublishTestChannel(srv)
	ch.channelID = id
	ch.conn = &lifecycleAddrConn{Conn: wire, remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50000}}
	return ch, wire
}

func waitLifecycleResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle operation did not finish")
		return nil
	}
}

func TestSessionLifecycleDelayedOldDetachCannotDescribeNewBinding(t *testing.T) {
	srv := newForkUnitServer()
	var events []SessionEvent
	srv.sessionEventHandler = func(event SessionEvent) { events = append(events, event) }
	session := newForkSession(srv, "old-detach")
	if err := srv.sessionManager.Add(session); err != nil {
		t.Fatal(err)
	}
	first, _ := newLifecycleTestChannel(srv, 7)
	if err := srv.activateSessionBinding(session, first, ua.AnonymousIdentity{}, "", nil); err != nil {
		t.Fatal(err)
	}
	// Model a candidate copied before SessionManager released its lock.
	candidate := session
	second, _ := newLifecycleTestChannel(srv, 9)
	if err := srv.activateSessionBinding(session, second, ua.AnonymousIdentity{}, "", nil); err != nil {
		t.Fatal(err)
	}
	candidate.detachChannel(srv, 7)
	srv.sessionManager.DetachChannel(7)
	if len(events) != 3 || events[2].Kind != SessionAttached || events[2].Session.SecureChannelID != 9 {
		t.Fatalf("late old-channel detach leaked into events: %#v", events)
	}
	srv.sessionManager.DetachChannel(9)
	srv.sessionManager.DetachChannel(9)
	if len(events) != 4 || events[3].Kind != SessionDetached || events[3].OldChannelID != 9 || events[3].Session.SecureChannelID != 9 {
		t.Fatalf("current detach was duplicated or mismatched: %#v", events)
	}
}

func TestSessionLifecycleSlowDetachOrdersAndBlocksActivationCompletion(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "slow-detach")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var mu sync.Mutex
	var events []SessionEvent
	var activeCallbacks, maxCallbacks atomic.Int32
	srv.sessionEventHandler = func(event SessionEvent) {
		active := activeCallbacks.Add(1)
		defer activeCallbacks.Add(-1)
		for previous := maxCallbacks.Load(); active > previous; previous = maxCallbacks.Load() {
			if maxCallbacks.CompareAndSwap(previous, active) {
				break
			}
		}
		_ = srv.Sessions()
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Kind == SessionDetached && event.OldChannelID == 7 {
			close(entered)
			<-release
		}
	}
	if err := srv.sessionManager.Add(session); err != nil {
		t.Fatal(err)
	}
	first, _ := newLifecycleTestChannel(srv, 7)
	if err := srv.activateSessionBinding(session, first, ua.AnonymousIdentity{}, "", nil); err != nil {
		t.Fatal(err)
	}
	detached := make(chan error, 1)
	go func() { srv.sessionManager.DetachChannel(7); detached <- nil }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Detach callback did not start")
	}
	second, _ := newLifecycleTestChannel(srv, 9)
	activated := make(chan error, 1)
	go func() { activated <- srv.activateSessionBinding(session, second, ua.AnonymousIdentity{}, "", nil) }()
	deadline := time.After(2 * time.Second)
	for session.SecureChannelId() != 9 {
		select {
		case <-deadline:
			t.Fatal("Session lock was held during callback")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-activated:
		t.Fatalf("activation completed before callback: %v", err)
	default:
	}
	// A delayed duplicate must not block or arrive after the new Attached event.
	srv.sessionManager.DetachChannel(7)
	releaseOnce.Do(func() { close(release) })
	if err := waitLifecycleResult(t, activated); err != nil {
		t.Fatal(err)
	}
	if err := waitLifecycleResult(t, detached); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 || events[2].Kind != SessionDetached || events[2].Session.SecureChannelID != 7 || events[3].Kind != SessionAttached || events[3].Session.SecureChannelID != 9 || maxCallbacks.Load() != 1 {
		t.Fatalf("callbacks overlapped/reordered: max=%d events=%#v", maxCallbacks.Load(), events)
	}
}

func TestSessionLifecycleActivationInitializesReadBeforeSuccessResponse(t *testing.T) {
	srv := newForkUnitServer()
	srv.serverDiagnosticsSummary = &ua.ServerDiagnosticsSummaryDataType{}
	srv.anonymousIdentityAuthenticator = AuthenticateAnonymousIdentityFunc(func(ua.AnonymousIdentity, string, string) error { return nil })
	session := newForkSession(srv, "read-before-response")
	if err := srv.sessionManager.Add(session); err != nil {
		t.Fatal(err)
	}
	ch, wire := newLifecycleTestChannel(srv, 7)
	ch.localEndpoint = ua.EndpointDescription{UserIdentityTokens: []ua.UserTokenPolicy{{PolicyID: "anonymous", TokenType: ua.UserTokenTypeAnonymous}}}
	var initialized, readReadyAtWrite atomic.Bool
	nodeID := ua.NewNodeIDString(2, "session-route")
	node := NewVariableNode(srv, nodeID, ua.NewQualifiedName(2, "session-route"), ua.LocalizedText{}, ua.LocalizedText{}, nil, nil,
		ua.NewDataValue("", ua.Good, time.Time{}, 0, time.Time{}, 0), ua.DataTypeIDString, ua.ValueRankScalar, nil, ua.AccessLevelsCurrentRead, 0, false, nil)
	node.SetReadValueHandler(func(s *Session, _ ua.ReadValueID) ua.DataValue {
		if s == session && initialized.Load() {
			return ua.NewDataValue("lower-b", ua.Good, time.Time{}, 0, time.Time{}, 0)
		}
		return ua.NewDataValue("", ua.BadSessionNotActivated, time.Time{}, 0, time.Time{}, 0)
	})
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}
	read := ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue}
	wire.beforeWrite = func() {
		value := srv.readValue(session, read)
		readReadyAtWrite.Store(value.StatusCode == ua.Good && value.Value == "lower-b")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	srv.sessionEventHandler = func(event SessionEvent) {
		if event.Kind == SessionActivated {
			close(entered)
			<-release
			initialized.Store(true)
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- srv.handleActivateSession(ch, 1, &ua.ActivateSessionRequest{
			RequestHeader:     ua.RequestHeader{AuthenticationToken: session.AuthenticationToken(), RequestHandle: 1},
			UserIdentityToken: ua.AnonymousIdentityToken{PolicyID: "anonymous"},
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Activated callback did not start")
	}
	wire.Lock()
	writes := len(wire.writes)
	wire.Unlock()
	if writes != 0 {
		t.Fatal("ActivateSession succeeded on wire before routes initialized")
	}
	releaseOnce.Do(func() { close(release) })
	if err := waitLifecycleResult(t, done); err != nil {
		t.Fatal(err)
	}
	if !readReadyAtWrite.Load() {
		t.Fatal("immediate Read was not ready when activation success was written")
	}
}

func TestSessionLifecycleCallbacksCanSnapshotDetachAndDeleteReentrantly(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "reentrant-lifecycle")
	var events []SessionEventKind
	srv.sessionEventHandler = func(event SessionEvent) {
		events = append(events, event.Kind)
		_ = srv.Sessions()
		switch event.Kind {
		case SessionActivated:
			srv.sessionManager.DetachChannel(event.Session.SecureChannelID)
		case SessionDetached:
			srv.sessionManager.DeleteWithReason(session, SessionCloseReasonClient)
		}
	}
	if err := srv.sessionManager.Add(session); err != nil {
		t.Fatal(err)
	}
	ch, _ := newLifecycleTestChannel(srv, 7)
	done := make(chan error, 1)
	go func() { done <- srv.activateSessionBinding(session, ch, ua.AnonymousIdentity{}, "", nil) }()
	if err := waitLifecycleResult(t, done); err != ua.BadSessionClosed {
		t.Fatalf("callback closed Session but activation returned %v", err)
	}
	if len(events) != 4 || events[0] != SessionCreated || events[1] != SessionActivated || events[2] != SessionDetached || events[3] != SessionClosed || srv.sessionManager.Len() != 0 {
		t.Fatalf("reentrant callback order = %#v", events)
	}
}

func TestSessionLifecycleConcurrentAttachDetachKeepsPerBindingOrder(t *testing.T) {
	srv := newForkUnitServer()
	var mu sync.Mutex
	var events []SessionEvent
	srv.sessionEventHandler = func(event SessionEvent) { mu.Lock(); events = append(events, event); mu.Unlock() }
	for iteration := 0; iteration < 100; iteration++ {
		session := newForkSession(srv, "concurrent-lifecycle")
		if err := srv.sessionManager.Add(session); err != nil {
			t.Fatal(err)
		}
		first, _ := newLifecycleTestChannel(srv, 7)
		second, _ := newLifecycleTestChannel(srv, 9)
		if err := srv.activateSessionBinding(session, first, ua.AnonymousIdentity{}, "", nil); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		done := make(chan error, 1)
		go func() { defer workers.Done(); <-start; srv.sessionManager.DetachChannel(7) }()
		go func() {
			defer workers.Done()
			<-start
			done <- srv.activateSessionBinding(session, second, ua.AnonymousIdentity{}, "", nil)
		}()
		close(start)
		if err := waitLifecycleResult(t, done); err != nil {
			t.Fatal(err)
		}
		workers.Wait()
		session.detachChannel(srv, 7)
		srv.sessionManager.Delete(session)
		mu.Lock()
		attached, closed := false, false
		for _, event := range events {
			if closed {
				t.Fatal("lifecycle event arrived after Closed")
			}
			switch event.Kind {
			case SessionAttached:
				attached = true
			case SessionDetached:
				if attached || event.OldChannelID != event.Session.SecureChannelID {
					t.Fatalf("stale/mismatched detach: %#v", events)
				}
			case SessionClosed:
				closed = true
			}
		}
		if !attached || !closed {
			t.Fatalf("missing terminal or attach event: %#v", events)
		}
		events = nil
		mu.Unlock()
	}
}
