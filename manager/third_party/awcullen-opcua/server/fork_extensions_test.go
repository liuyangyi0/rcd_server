package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awcullen/opcua/ua"
)

func TestDiagnosticsSnapshotsSupportConcurrentRequestUpdates(t *testing.T) {
	srv := newForkUnitServer()
	srv.serverDiagnosticsSummary = &ua.ServerDiagnosticsSummaryDataType{}
	session := newForkSession(srv, "diagnostics")
	session.SetLocaleIDs([]string{"en-US", "zh-CN"})
	srv.sessionManager.addDiagnosticsNode(session)
	diagnosticsObject, ok := srv.namespaceManager.FindNode(session.SessionId())
	if !ok {
		t.Fatal("session diagnostics object was not created")
	}
	diagnosticsNode, ok := srv.namespaceManager.FindComponent(diagnosticsObject, ua.NewQualifiedName(0, "SessionDiagnostics"))
	if !ok {
		t.Fatal("session diagnostics variable was not created")
	}
	diagnosticsRead := ua.ReadValueID{NodeID: diagnosticsNode.NodeID(), AttributeID: ua.AttributeIDValue}

	stopReaders := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					summary := srv.serverDiagnosticsSnapshot()
					_ = summary.RejectedRequestsCount
					value := srv.readValue(session, diagnosticsRead)
					diagnostics, ok := value.Value.(ua.SessionDiagnosticsDataType)
					if !ok || value.StatusCode.IsBad() {
						t.Errorf("session diagnostics read = %#v", value)
						return
					}
					if len(diagnostics.LocaleIDs) != 2 {
						t.Errorf("locale IDs length = %d, want 2", len(diagnostics.LocaleIDs))
						return
					}
					_ = diagnostics.ReadCount
					_ = diagnostics.WriteCount
					_ = diagnostics.CallCount
				}
			}
		}()
	}

	const writers = 16
	const increments = 2_000
	var updates sync.WaitGroup
	for i := 0; i < writers; i++ {
		updates.Add(1)
		go func() {
			defer updates.Done()
			for j := 0; j < increments; j++ {
				atomic.AddUint32(&srv.serverDiagnosticsSummary.RejectedRequestsCount, 1)
				atomic.AddUint32(&session.requestCount, 1)
				atomic.AddUint32(&session.readCount, 1)
				atomic.AddUint32(&session.writeCount, 1)
				atomic.AddUint32(&session.callCount, 1)
				if j%127 == 0 {
					session.SetLocaleIDs([]string{"en-US", "zh-CN"})
					session.SetLastAccess(time.Now())
				}
			}
		}()
	}
	updates.Wait()
	close(stopReaders)
	readers.Wait()

	want := uint32(writers * increments)
	if got := srv.serverDiagnosticsSnapshot().RejectedRequestsCount; got != want {
		t.Fatalf("rejected request count = %d, want %d", got, want)
	}
	diagnostics := session.diagnosticsSnapshot()
	if diagnostics.TotalRequestCount.TotalCount != want || diagnostics.ReadCount.TotalCount != want ||
		diagnostics.WriteCount.TotalCount != want || diagnostics.CallCount.TotalCount != want {
		t.Fatalf("session diagnostic counts = %#v, want %d each", diagnostics, want)
	}
	diagnostics.LocaleIDs[0] = "mutated"
	if got := session.diagnosticsSnapshot().LocaleIDs[0]; got != "en-US" {
		t.Fatalf("diagnostic snapshot leaked locale backing slice: %q", got)
	}
}

func TestPublishQueueDoesNotRaceOrBlockWhenSessionIsDeleted(t *testing.T) {
	newRequest := func(handle uint32) *ua.PublishRequest {
		return &ua.PublishRequest{RequestHeader: ua.RequestHeader{
			Timestamp:     time.Now(),
			RequestHandle: handle,
			TimeoutHint:   10_000,
		}}
	}

	// A full queue followed by delete used to replace publishRequests with nil;
	// the next add then selected default and blocked forever receiving from nil.
	srv := newForkUnitServer()
	session := newForkSession(srv, "full-publish-queue")
	ch, _ := newPublishTestChannel(srv)
	session.SetSecureChannelId(ch.ChannelID())
	for i := 0; i < cap(session.publishRequests); i++ {
		if err := session.addPublishRequest(ch, uint32(i), newRequest(uint32(i)), nil); err != nil {
			t.Fatalf("fill publish queue at %d: %v", i, err)
		}
	}
	session.delete()
	addResult := make(chan error, 1)
	go func() {
		addResult <- session.addPublishRequest(ch, 100, newRequest(100), nil)
	}()
	select {
	case err := <-addResult:
		if err != ua.BadSessionClosed {
			t.Fatalf("add after delete error = %v, want %s", err, ua.BadSessionClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("addPublishRequest blocked after session delete")
	}
	if _, _, _, _, ok, err := session.removePublishRequest(); err != nil || ok {
		t.Fatalf("closed Session retained an eligible Publish request: ok=%v err=%v", ok, err)
	}

	// Exercise the actual add/delete interleaving repeatedly for the race
	// detector. An add that wins may succeed; one that loses must report closed.
	for i := 0; i < 200; i++ {
		session := newForkSession(srv, "publish-race")
		session.SetSecureChannelId(ch.ChannelID())
		start := make(chan struct{})
		result := make(chan error, 1)
		deleted := make(chan struct{})
		go func() {
			<-start
			result <- session.addPublishRequest(ch, 1, newRequest(1), nil)
		}()
		go func() {
			<-start
			session.delete()
			close(deleted)
		}()
		close(start)
		select {
		case err := <-result:
			if err != nil && err != ua.BadSessionClosed {
				t.Fatalf("concurrent add error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent add/delete blocked")
		}
		<-deleted
	}
}

func TestSubscriptionDiagnosticsSnapshotConcurrentAddDelete(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "subscription-diagnostics")

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for _, diagnostic := range srv.subscriptionDiagnostics() {
						if _, ok := diagnostic.(ua.SubscriptionDiagnosticsDataType); !ok {
							t.Errorf("unexpected subscription diagnostic %T", diagnostic)
							return
						}
					}
				}
			}
		}()
	}

	const workers = 8
	const iterations = 300
	var writers sync.WaitGroup
	for i := 0; i < workers; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < iterations; j++ {
				subscription := NewSubscription(srv.subscriptionManager, session, 60_000, 20, 10, 0, true, 0)
				if err := srv.subscriptionManager.Add(subscription); err != nil {
					t.Errorf("add subscription: %v", err)
					return
				}
				subscription.SetPublishingMode(j%2 == 0)
				_ = subscription.diagnosticsSnapshot()
				srv.subscriptionManager.Delete(subscription)
				subscription.Delete()
			}
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	if got := srv.subscriptionManager.Len(); got != 0 {
		t.Fatalf("subscriptions left after add/delete test = %d", got)
	}
}

func TestSubscriptionStopModifyDeleteAreConcurrentAndIdempotent(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "subscription-stop")

	for i := 0; i < 300; i++ {
		subscription := NewSubscription(srv.subscriptionManager, session, 60_000, 20, 10, 0, true, 0)
		if err := srv.subscriptionManager.Add(subscription); err != nil {
			t.Fatal(err)
		}
		subscription.startPublishing()
		start := make(chan struct{})
		var operations sync.WaitGroup
		operations.Add(4)
		go func() {
			defer operations.Done()
			<-start
			subscription.Modify(60_000, 20, 10, 0, 0)
		}()
		go func() {
			defer operations.Done()
			<-start
			srv.subscriptionManager.stopPublishingAllSubscriptions()
		}()
		go func() {
			defer operations.Done()
			<-start
			subscription.Delete()
		}()
		go func() {
			defer operations.Done()
			<-start
			srv.subscriptionManager.Delete(subscription)
		}()
		close(start)
		operations.Wait()
		// Both operations are intentionally repeated to verify terminal
		// idempotence after every possible interleaving above.
		subscription.stopPublishing()
		subscription.stopPublishing()
		subscription.Delete()
		subscription.Delete()
		subscription.RLock()
		deleted := subscription.deleted
		cancel := subscription.cancelPublishing
		subscription.RUnlock()
		if !deleted || cancel != nil {
			t.Fatalf("terminal subscription state deleted=%v cancel=%v", deleted, cancel)
		}
	}
}

func TestBrowseContinuationPointRejectsDeletedSession(t *testing.T) {
	srv := newForkUnitServer()
	for i := 0; i < 300; i++ {
		session := newForkSession(srv, "browse-race")
		start := make(chan struct{})
		result := make(chan error, 1)
		deleted := make(chan struct{})
		go func() {
			<-start
			_, err := session.addBrowseContinuationPoint([]ua.ReferenceDescription{{BrowseName: ua.NewQualifiedName(2, "KKS")}}, 1)
			result <- err
		}()
		go func() {
			<-start
			session.delete()
			close(deleted)
		}()
		close(start)
		select {
		case err := <-result:
			if err != nil && err != ua.BadSessionClosed {
				t.Fatalf("concurrent continuation-point add error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("continuation-point add/delete blocked")
		}
		<-deleted
		if _, err := session.addBrowseContinuationPoint(nil, 1); err != ua.BadSessionClosed {
			t.Fatalf("add after delete error = %v, want %s", err, ua.BadSessionClosed)
		}
	}
}

func TestDeletedSubscriptionRejectsStaleMonitoredItemConstructionAndAppend(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "stale-monitored-item")
	parameters := ua.MonitoringParameters{
		ClientHandle: 1, SamplingInterval: -1, QueueSize: 1, DiscardOldest: true,
	}
	itemToMonitor := ua.ReadValueID{NodeID: ua.NewNodeIDString(2, "KKS"), AttributeID: ua.AttributeIDValue}

	for i := 0; i < 300; i++ {
		subscription := NewSubscription(srv.subscriptionManager, session, 250, 20, 10, 0, true, 0)
		start := make(chan struct{})
		constructed := make(chan *DataChangeMonitoredItem, 1)
		deleted := make(chan struct{})
		go func() {
			<-start
			constructed <- NewDataChangeMonitoredItem(
				subscription, nil, itemToMonitor, ua.MonitoringModeDisabled,
				parameters, ua.TimestampsToReturnBoth, 0,
			)
		}()
		go func() {
			<-start
			subscription.Delete()
			close(deleted)
		}()
		close(start)
		item := <-constructed
		<-deleted
		if subscription.AppendItem(item) {
			t.Fatal("deleted subscription accepted a stale monitored item")
		}
		if item != nil {
			item.Delete()
		}
		if subscription.DeleteItem(1) {
			t.Fatal("deleted subscription reported a monitored item deletion")
		}
		subscription.SetPublishingMode(false)
		if got := NewDataChangeMonitoredItem(
			subscription, nil, itemToMonitor, ua.MonitoringModeDisabled,
			parameters, ua.TimestampsToReturnBoth, 0,
		); got != nil {
			t.Fatal("constructor accepted a deleted subscription")
		}
	}
}

func TestMonitoringStartAndDeleteCannotLeaveADeletedPollSubscription(t *testing.T) {
	srv := newForkUnitServer()
	srv.closing = make(chan struct{})
	srv.scheduler = NewScheduler(srv)
	defer close(srv.closing)
	session := newForkSession(srv, "monitoring-delete")
	nodeID := ua.NewNodeIDString(2, "monitoring-delete-KKS")
	node := NewVariableNode(
		srv, nodeID, ua.NewQualifiedName(2, "monitoring-delete-KKS"), ua.NewLocalizedText("monitoring-delete-KKS", ""), ua.LocalizedText{}, nil, nil,
		ua.NewDataValue(uint32(1), ua.Good, time.Time{}, 0, time.Now(), 0), ua.DataTypeIDUInt32, ua.ValueRankScalar, nil,
		ua.AccessLevelsCurrentRead, 0, false, nil,
	)
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 300; i++ {
		subscription := NewSubscription(srv.subscriptionManager, session, 250, 20, 10, 0, true, 0)
		item := NewDataChangeMonitoredItem(
			subscription, node, ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue}, ua.MonitoringModeDisabled,
			ua.MonitoringParameters{ClientHandle: 1, SamplingInterval: 250, QueueSize: 1, DiscardOldest: true},
			ua.TimestampsToReturnBoth, 0,
		)
		if item == nil || !subscription.AppendItem(item) {
			t.Fatal("failed to append monitored item")
		}

		start := make(chan struct{})
		var operations sync.WaitGroup
		operations.Add(2)
		go func() {
			defer operations.Done()
			<-start
			item.SetMonitoringMode(ua.MonitoringModeReporting)
		}()
		go func() {
			defer operations.Done()
			<-start
			subscription.Delete()
		}()
		close(start)
		operations.Wait()

		group := srv.scheduler.GetPollGroup(250 * time.Millisecond)
		group.Lock()
		_, subscribed := group.subs[item]
		group.Unlock()
		if subscribed {
			t.Fatal("deleted monitored item remained subscribed to poll group")
		}
	}
}

func TestRetransmissionQueueIsSerializedWithDeleteAndAcknowledge(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "republish-race")
	for i := 0; i < 300; i++ {
		subscription := NewSubscription(srv.subscriptionManager, session, 250, 20, 10, 0, true, 0)
		subscription.Lock()
		subscription.retransmissionQueue.PushBack(ua.NotificationMessage{SequenceNumber: 1, PublishTime: time.Now()})
		subscription.Unlock()

		start := make(chan struct{})
		var operations sync.WaitGroup
		operations.Add(3)
		go func() {
			defer operations.Done()
			<-start
			_, _ = subscription.republish(1, func(ua.NotificationMessage) error { return nil })
		}()
		go func() {
			defer operations.Done()
			<-start
			_ = subscription.acknowledge(1)
		}()
		go func() {
			defer operations.Done()
			<-start
			subscription.Delete()
		}()
		close(start)
		operations.Wait()
		if found, err := subscription.republish(1, func(ua.NotificationMessage) error { return nil }); found || err != nil {
			t.Fatalf("republish after delete: found=%v err=%v", found, err)
		}
	}
}

func TestRepublishRetainsMessageUntilPublishAcknowledgement(t *testing.T) {
	srv := newForkUnitServer()
	subscription := NewSubscription(srv.subscriptionManager, newForkSession(srv, "republish-ack"), 250, 20, 10, 0, true, 0)
	subscription.Lock()
	subscription.retransmissionQueue.PushBack(ua.NotificationMessage{SequenceNumber: 7, PublishTime: time.Now()})
	subscription.Unlock()

	writes := 0
	write := func(message ua.NotificationMessage) error {
		if message.SequenceNumber != 7 {
			t.Fatalf("republished sequence = %d, want 7", message.SequenceNumber)
		}
		writes++
		return nil
	}
	for i := 0; i < 2; i++ {
		found, err := subscription.republish(7, write)
		if err != nil || !found {
			t.Fatalf("republish %d: found=%v err=%v", i, found, err)
		}
	}
	if writes != 2 {
		t.Fatalf("republish writes = %d, want 2", writes)
	}
	if !subscription.acknowledge(7) {
		t.Fatal("PublishRequest acknowledgement did not remove message")
	}
	if found, err := subscription.republish(7, write); err != nil || found {
		t.Fatalf("republish after acknowledgement: found=%v err=%v", found, err)
	}
}

func TestEventDispatchDoesNotDeadlockConcurrentMonitoredItemDelete(t *testing.T) {
	srv := newForkUnitServer()
	session := newForkSession(srv, "event-delete")
	for i := 0; i < 300; i++ {
		node := NewObjectNode(
			srv, ua.NewNodeIDNumeric(2, uint32(i+1)), ua.NewQualifiedName(2, "events"),
			ua.NewLocalizedText("events", ""), ua.LocalizedText{}, nil, nil, ua.EventNotifierSubscribeToEvents,
		)
		subscription := NewSubscription(srv.subscriptionManager, session, 250, 20, 10, 0, true, 0)
		item := NewEventMonitoredItem(
			subscription, node, ua.ReadValueID{NodeID: node.NodeID(), AttributeID: ua.AttributeIDEventNotifier},
			ua.MonitoringModeReporting,
			ua.MonitoringParameters{ClientHandle: 1, QueueSize: 4, DiscardOldest: true, Filter: ua.EventFilter{}},
		)
		if item == nil || !subscription.AppendItem(item) {
			t.Fatal("failed to append event monitored item")
		}

		start := make(chan struct{})
		var operations sync.WaitGroup
		operations.Add(2)
		go func() {
			defer operations.Done()
			<-start
			node.OnEvent(&ua.BaseEvent{EventType: ua.ObjectTypeIDBaseEventType, Time: time.Now()})
		}()
		go func() {
			defer operations.Done()
			<-start
			subscription.Delete()
		}()
		close(start)
		done := make(chan struct{})
		go func() {
			operations.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("event dispatch and monitored-item deletion deadlocked")
		}
	}
}

func TestConcurrentCloseIsIdempotentAndServerStatusIsRaceFree(t *testing.T) {
	srv := &Server{
		closing:            make(chan struct{}),
		state:              ua.ServerStateUnknown,
		startTime:          time.Now(),
		serverCapabilities: ua.NewServerCapabilities(),
	}
	srv.sessionManager = NewSessionManager(srv)

	stopReaders := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					status := srv.serverStatus()
					if status.StartTime.IsZero() {
						t.Errorf("server status returned zero start time")
						return
					}
				}
			}
		}()
	}

	var closers sync.WaitGroup
	for i := 0; i < 32; i++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			if err := srv.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}()
	}
	closers.Wait()
	close(stopReaders)
	readers.Wait()

	select {
	case <-srv.Closing():
	default:
		t.Fatal("closing channel was not closed")
	}
	status := srv.serverStatus()
	if status.State != ua.ServerStateShutdown || status.SecondsTillShutdown != 0 {
		t.Fatalf("final server status = %#v", status)
	}
	// A later Close remains a no-op and must not panic on the already closed
	// broadcast channel.
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestConfiguredEndpointDescriptionsAreExact(t *testing.T) {
	srv := &Server{
		endpointURL: "opc.tcp://localhost:4840",
		localDescription: ua.ApplicationDescription{
			ApplicationURI: "urn:test", ApplicationName: ua.NewLocalizedText("test", ""),
		},
		localCertificate:               []byte{1, 2, 3},
		anonymousIdentityAuthenticator: AuthenticateAnonymousIdentityFunc(func(ua.AnonymousIdentity, string, string) error { return nil }),
		userNameIdentityAuthenticator:  AuthenticateUserNameIdentityFunc(func(ua.UserNameIdentity, string, string) error { return nil }),
		endpointProfiles: []EndpointProfile{
			{SecurityPolicyURI: ua.SecurityPolicyURINone, SecurityMode: ua.MessageSecurityModeNone, AllowAnonymous: true},
			{SecurityPolicyURI: ua.SecurityPolicyURIBasic256Sha256, SecurityMode: ua.MessageSecurityModeSignAndEncrypt, AllowAnonymous: true, AllowUserName: true},
		},
	}
	endpoints := srv.buildConfiguredEndpointDescriptions()
	if len(endpoints) != 2 {
		t.Fatalf("endpoint count = %d, want 2", len(endpoints))
	}
	if endpoints[0].SecurityPolicyURI != ua.SecurityPolicyURINone || endpoints[0].SecurityMode != ua.MessageSecurityModeNone {
		t.Fatalf("none endpoint = %#v", endpoints[0])
	}
	if len(endpoints[0].UserIdentityTokens) != 1 || endpoints[0].UserIdentityTokens[0].TokenType != ua.UserTokenTypeAnonymous {
		t.Fatalf("none tokens = %#v", endpoints[0].UserIdentityTokens)
	}
	if endpoints[1].SecurityPolicyURI != ua.SecurityPolicyURIBasic256Sha256 || endpoints[1].SecurityMode != ua.MessageSecurityModeSignAndEncrypt {
		t.Fatalf("secure endpoint = %#v", endpoints[1])
	}
	if len(endpoints[1].UserIdentityTokens) != 2 {
		t.Fatalf("secure tokens = %#v", endpoints[1].UserIdentityTokens)
	}
}

func TestEndpointProfilesRejectDuplicatesAndInvalidNoneMode(t *testing.T) {
	if err := validateEndpointProfiles(nil); err == nil {
		t.Fatal("empty endpoint profile list was accepted")
	}
	duplicate := []EndpointProfile{
		{SecurityPolicyURI: ua.SecurityPolicyURINone, SecurityMode: ua.MessageSecurityModeNone},
		{SecurityPolicyURI: ua.SecurityPolicyURINone, SecurityMode: ua.MessageSecurityModeNone},
	}
	if err := validateEndpointProfiles(duplicate); err == nil {
		t.Fatal("duplicate endpoint profiles were accepted")
	}
	if err := validateEndpointProfiles([]EndpointProfile{{
		SecurityPolicyURI: ua.SecurityPolicyURINone, SecurityMode: ua.MessageSecurityModeSign,
	}}); err == nil {
		t.Fatal("invalid None/Sign endpoint was accepted")
	}
}

func TestEndpointMatchRejectsUnlistedPolicyAndMode(t *testing.T) {
	srv := &Server{
		endpointURL: "opc.tcp://localhost:4840",
		localDescription: ua.ApplicationDescription{
			ApplicationURI: "urn:test", ApplicationName: ua.NewLocalizedText("test", ""),
		},
		endpointProfiles: []EndpointProfile{
			{SecurityPolicyURI: ua.SecurityPolicyURINone, SecurityMode: ua.MessageSecurityModeNone},
			{SecurityPolicyURI: ua.SecurityPolicyURIBasic256Sha256, SecurityMode: ua.MessageSecurityModeSignAndEncrypt},
		},
	}
	if _, status := srv.matchEndpoint(ua.SecurityPolicyURIBasic256Sha256, ua.MessageSecurityModeSignAndEncrypt); status != ua.Good {
		t.Fatalf("listed endpoint status = %s", status)
	}
	if _, status := srv.matchEndpoint(ua.SecurityPolicyURIBasic256Sha256, ua.MessageSecurityModeSign); status != ua.BadSecurityModeRejected {
		t.Fatalf("unlisted mode status = %s", status)
	}
	if _, status := srv.matchEndpoint(ua.SecurityPolicyURIBasic256, ua.MessageSecurityModeSignAndEncrypt); status != ua.BadSecurityPolicyRejected {
		t.Fatalf("unlisted policy status = %s", status)
	}
}

func TestListenAddressOptionDoesNotChangeAdvertisedEndpoint(t *testing.T) {
	srv := &Server{endpointURL: "opc.tcp://manager.example:4840"}
	if err := WithListenAddress("127.0.0.1:5840")(srv); err != nil {
		t.Fatal(err)
	}
	if srv.ListenAddress() != "127.0.0.1:5840" {
		t.Fatalf("listen address = %q", srv.ListenAddress())
	}
	if srv.EndpointURL() != "opc.tcp://manager.example:4840" {
		t.Fatalf("advertised endpoint changed to %q", srv.EndpointURL())
	}
	if err := WithListenAddress("not-an-address")(srv); err == nil {
		t.Fatal("invalid listen address was accepted")
	}
}

func TestSessionManagerEmitsCompleteLifecycleSnapshots(t *testing.T) {
	events := make([]SessionEvent, 0, 5)
	srv := &Server{
		closing: make(chan struct{}), serverCapabilities: ua.NewServerCapabilities(),
		sessionEventHandler: func(event SessionEvent) { events = append(events, event) },
	}
	srv.sessionManager = NewSessionManager(srv)
	defer close(srv.closing)
	sessionID := ua.NewNodeIDString(1, "session")
	session := NewSession(
		srv, sessionID, "test", ua.NewNodeIDString(0, "token"), ua.ByteString(""), 60_000,
		ua.ApplicationDescription{ApplicationURI: "urn:client"}, "", "opc.tcp://localhost:4840", ua.ByteString(""), 0,
	)
	if err := srv.sessionManager.Add(session); err != nil {
		t.Fatal(err)
	}
	firstChannel, _ := newLifecycleTestChannel(srv, 7)
	if err := srv.activateSessionBinding(session, firstChannel, ua.AnonymousIdentity{}, "", nil); err != nil {
		t.Fatal(err)
	}
	srv.sessionManager.DetachChannel(7)
	secondChannel, _ := newLifecycleTestChannel(srv, 9)
	if err := srv.activateSessionBinding(session, secondChannel, ua.AnonymousIdentity{}, "", nil); err != nil {
		t.Fatal(err)
	}
	srv.sessionManager.DeleteWithReason(session, SessionCloseReasonClient)
	if len(events) != 5 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Kind != SessionCreated || events[1].Kind != SessionActivated || events[2].Kind != SessionDetached || events[3].Kind != SessionAttached || events[4].Kind != SessionClosed {
		t.Fatalf("event order = %#v", events)
	}
	if events[2].OldChannelID != 7 || events[2].Session.ClientApplicationURI != "urn:client" {
		t.Fatalf("detached snapshot = %#v", events[2])
	}
	if events[3].OldChannelID != 7 || events[3].Session.SecureChannelID != 9 {
		t.Fatalf("attached snapshot = %#v", events[3])
	}
	if events[4].Reason != SessionCloseReasonClient || events[4].Session.SessionName != "test" || events[4].Session.RemoteAddress != "127.0.0.1:50000" {
		t.Fatalf("closed snapshot = %#v", events[4])
	}
	if time.Since(events[4].OccurredAt) > time.Second {
		t.Fatalf("stale event time: %s", events[4].OccurredAt)
	}
}

func TestSessionScopedWriteHandlerDoesNotMutateBackingValue(t *testing.T) {
	srv := newForkUnitServer()
	srv.sessionScopedWriteHandlers = true
	nodeID := ua.NewNodeIDString(2, "selection")
	node := NewVariableNode(
		srv, nodeID, ua.NewQualifiedName(2, "selection"), ua.NewLocalizedText("selection", ""), ua.LocalizedText{}, nil, nil,
		ua.NewDataValue("global", ua.Good, time.Time{}, 0, time.Now(), 0), ua.DataTypeIDString, ua.ValueRankScalar, nil,
		ua.AccessLevelsCurrentRead|ua.AccessLevelsCurrentWrite, 0, false, nil,
	)
	node.SetWriteValueHandler(func(_ *Session, req ua.WriteValue) (ua.DataValue, ua.StatusCode) {
		return req.Value, ua.Good
	})
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}
	session := newForkSession(srv, "one")
	status := srv.writeValue(session, ua.WriteValue{
		NodeID: nodeID, AttributeID: ua.AttributeIDValue,
		Value: ua.NewDataValue("session-one", ua.Good, time.Time{}, 0, time.Now(), 0),
	})
	if status != ua.Good {
		t.Fatalf("write status = %s", status)
	}
	if got := node.Value().Value; got != "global" {
		t.Fatalf("session-scoped write leaked to backing value: %#v", got)
	}

	srv.sessionScopedWriteHandlers = false
	status = srv.writeValue(session, ua.WriteValue{
		NodeID: nodeID, AttributeID: ua.AttributeIDValue,
		Value: ua.NewDataValue("legacy-global", ua.Good, time.Time{}, 0, time.Now(), 0),
	})
	if status != ua.Good || node.Value().Value != "legacy-global" {
		t.Fatalf("legacy write status=%s backing=%#v", status, node.Value().Value)
	}
}

func TestVariableNodeHandlerSnapshotsAreSafeDuringReplacement(t *testing.T) {
	srv := newForkUnitServer()
	node := NewVariableNode(
		srv, ua.NewNodeIDString(2, "hot-handler"), ua.NewQualifiedName(2, "hot-handler"),
		ua.NewLocalizedText("hot-handler", ""), ua.LocalizedText{}, nil, nil,
		ua.NewDataValue(uint32(0), ua.Good, time.Time{}, 0, time.Now(), 0),
		ua.DataTypeIDUInt32, ua.ValueRankScalar, nil,
		ua.AccessLevelsCurrentRead|ua.AccessLevelsCurrentWrite, 0, false, nil,
	)
	read := func(*Session, ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(uint32(1), ua.Good, time.Time{}, 0, time.Now(), 0)
	}
	write := func(_ *Session, value ua.WriteValue) (ua.DataValue, ua.StatusCode) {
		return value.Value, ua.Good
	}
	node.SetReadValueHandler(read)
	node.SetWriteValueHandler(write)

	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		for i := 0; i < 10_000; i++ {
			node.SetReadValueHandler(read)
			node.SetWriteValueHandler(write)
		}
	}()
	for range 2 {
		go func() {
			defer workers.Done()
			for i := 0; i < 10_000; i++ {
				if handler := node.ReadValueHandler(); handler != nil {
					_ = handler(nil, ua.ReadValueID{})
				}
				if handler := node.WriteValueHandler(); handler != nil {
					_, _ = handler(nil, ua.WriteValue{})
				}
			}
		}()
	}
	workers.Wait()
}

func TestReadValueHandlerAppliesIndexRange(t *testing.T) {
	srv := newForkUnitServer()
	nodeID := ua.NewNodeIDString(2, "handled-array")
	node := NewVariableNode(
		srv, nodeID, ua.NewQualifiedName(2, "handled-array"), ua.NewLocalizedText("handled-array", ""),
		ua.LocalizedText{}, nil, nil, ua.NewDataValue([]string{}, ua.Good, time.Time{}, 0, time.Now(), 0),
		ua.DataTypeIDString, ua.ValueRankOneDimension, nil, ua.AccessLevelsCurrentRead, 0, false, nil,
	)
	node.SetReadValueHandler(func(*Session, ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue([]string{"zero", "one", "two", "three"}, ua.Good, time.Time{}, 0, time.Now(), 0)
	})
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}
	value := srv.readValue(newForkSession(srv, "range"), ua.ReadValueID{
		NodeID: nodeID, AttributeID: ua.AttributeIDValue, IndexRange: "1:2",
	})
	items, ok := value.Value.([]string)
	if value.StatusCode != ua.Good || !ok || len(items) != 2 || items[0] != "one" || items[1] != "two" {
		t.Fatalf("ranged handler value=%#v", value)
	}
	value = srv.readValue(newForkSession(srv, "single-range"), ua.ReadValueID{
		NodeID: nodeID, AttributeID: ua.AttributeIDValue, IndexRange: "0:0",
	})
	items, ok = value.Value.([]string)
	if value.StatusCode != ua.Good || !ok || len(items) != 1 || items[0] != "zero" {
		t.Fatalf("single-element ranged handler value=%#v", value)
	}
}

func TestForceResampleTargetsOnlyRequestedSessionAndNode(t *testing.T) {
	srv := newForkUnitServer()
	nodeID := ua.NewNodeIDString(2, "KKS")
	node := NewVariableNode(
		srv, nodeID, ua.NewQualifiedName(2, "KKS"), ua.NewLocalizedText("KKS", ""), ua.LocalizedText{}, nil, nil,
		ua.NewDataValue(uint32(0), ua.Good, time.Time{}, 0, time.Now(), 0), ua.DataTypeIDUInt32, ua.ValueRankScalar, nil,
		ua.AccessLevelsCurrentRead, 0, false, nil,
	)
	node.SetReadValueHandler(func(session *Session, _ ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(session.SessionName(), ua.Good, time.Time{}, 0, time.Now(), 0)
	})
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}

	firstSession := newForkSession(srv, "first")
	secondSession := newForkSession(srv, "second")
	if err := srv.sessionManager.Add(firstSession); err != nil {
		t.Fatal(err)
	}
	if err := srv.sessionManager.Add(secondSession); err != nil {
		t.Fatal(err)
	}
	firstItem := addDisabledMonitoredItem(t, srv, firstSession, node)
	secondItem := addDisabledMonitoredItem(t, srv, secondSession, node)

	if count := srv.ForceResampleSessionNodes(firstSession.SessionId(), []ua.NodeID{nodeID}); count != 1 {
		t.Fatalf("resampled item count = %d", count)
	}
	firstItem.RLock()
	firstLen := firstItem.prequeue.Len()
	firstValue := firstItem.prequeue.Front().Value
	firstItem.RUnlock()
	secondItem.RLock()
	secondLen := secondItem.prequeue.Len()
	secondItem.RUnlock()
	if firstLen != 1 || firstValue != "first" {
		t.Fatalf("first prequeue len=%d value=%#v", firstLen, firstValue)
	}
	if secondLen != 0 {
		t.Fatalf("second session was resampled: len=%d", secondLen)
	}
	if count := srv.ForceResampleSessionNodes(firstSession.SessionId(), []ua.NodeID{ua.NewNodeIDString(2, "other")}); count != 0 {
		t.Fatalf("non-matching node resample count = %d", count)
	}
}

func TestForceResamplePublishesSameValueOnlyToTargetSession(t *testing.T) {
	srv := newForkUnitServer()
	nodeID := ua.NewNodeIDString(2, "same-value-KKS")
	node := NewVariableNode(
		srv, nodeID, ua.NewQualifiedName(2, "same-value-KKS"), ua.NewLocalizedText("same-value-KKS", ""), ua.LocalizedText{}, nil, nil,
		ua.NewDataValue(uint32(42), ua.Good, time.Time{}, 0, time.Now(), 0), ua.DataTypeIDUInt32, ua.ValueRankScalar, nil,
		ua.AccessLevelsCurrentRead, 0, false, nil,
	)
	node.SetReadValueHandler(func(*Session, ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(uint32(42), ua.Good, time.Time{}, 0, time.Now(), 0)
	})
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}

	newReportingItem := func(session *Session) *DataChangeMonitoredItem {
		if err := srv.sessionManager.Add(session); err != nil {
			t.Fatal(err)
		}
		subscription := NewSubscription(srv.subscriptionManager, session, 250, 20, 10, 0, true, 0)
		if err := srv.subscriptionManager.Add(subscription); err != nil {
			t.Fatal(err)
		}
		// Construct disabled to avoid a scheduler goroutine in this focused unit
		// test, then enable reporting under the item lock.
		item := NewDataChangeMonitoredItem(
			subscription, node, ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue}, ua.MonitoringModeDisabled,
			ua.MonitoringParameters{ClientHandle: 1, SamplingInterval: 0, QueueSize: 4, DiscardOldest: true},
			ua.TimestampsToReturnBoth, 0,
		)
		if item == nil || !subscription.AppendItem(item) {
			t.Fatal("failed to append monitored item")
		}
		item.Lock()
		item.monitoringMode = ua.MonitoringModeReporting
		item.previousQueuedValue = ua.NewDataValue(uint32(42), ua.Good, time.Time{}, 0, time.Time{}, 0)
		item.Unlock()
		return item
	}

	firstSession := newForkSession(srv, "same-value-first")
	secondSession := newForkSession(srv, "same-value-second")
	firstItem := newReportingItem(firstSession)
	secondItem := newReportingItem(secondSession)

	// Normal sampling correctly suppresses an unchanged value for both items.
	firstItem.Poll()
	secondItem.Poll()
	if firstItem.notificationsAvailable(time.Now(), false, false) || secondItem.notificationsAvailable(time.Now(), false, false) {
		t.Fatal("normal sampling unexpectedly reported an unchanged value")
	}
	// Simulate both sampled and already-queued values from the old route. A
	// forced route refresh must not emit either after the new value is sampled.
	stale := ua.NewDataValue(uint32(17), ua.Good, time.Time{}, 0, time.Now(), 0)
	firstItem.Lock()
	firstItem.prequeue.PushBack(stale)
	firstItem.enqueue(stale)
	firstItem.Unlock()

	if count := srv.ForceResampleSessionNodes(firstSession.SessionId(), []ua.NodeID{nodeID}); count != 1 {
		t.Fatalf("resampled item count = %d, want 1", count)
	}
	if !firstItem.notificationsAvailable(time.Now(), false, false) {
		t.Fatal("target item did not expose a forced same-value notification")
	}
	notifications, more := firstItem.notifications(10)
	if more || len(notifications) != 1 {
		t.Fatalf("target notifications len=%d more=%v", len(notifications), more)
	}
	value, ok := notifications[0].(ua.DataValue)
	if !ok || value.Value != uint32(42) || value.StatusCode != ua.Good {
		t.Fatalf("forced notification = %#v", notifications[0])
	}
	if secondItem.notificationsAvailable(time.Now(), false, false) {
		t.Fatal("non-target session received a forced notification")
	}
}

func newForkUnitServer() *Server {
	srv := &Server{
		localDescription:   ua.ApplicationDescription{ApplicationURI: "urn:test"},
		serverCapabilities: ua.NewServerCapabilities(),
		rolesProvider: GetRolesFunc(func(any, string, string) ([]ua.NodeID, error) {
			return []ua.NodeID{ua.ObjectIDWellKnownRoleAnonymous}, nil
		}),
		rolePermissions: []ua.RolePermissionType{{
			RoleID:      ua.ObjectIDWellKnownRoleAnonymous,
			Permissions: ua.PermissionTypeBrowse | ua.PermissionTypeRead | ua.PermissionTypeWrite,
		}},
	}
	srv.namespaceManager = NewNamespaceManager(srv)
	srv.sessionManager = &SessionManager{server: srv, sessionsByToken: make(map[ua.NodeID]*Session)}
	srv.subscriptionManager = &SubscriptionManager{server: srv, subscriptionsByID: make(map[uint32]*Subscription)}
	return srv
}

func newForkSession(srv *Server, name string) *Session {
	return NewSession(
		srv, ua.NewNodeIDString(1, name), name, ua.NewNodeIDString(1, name+"-token"), ua.ByteString(""), 60_000,
		ua.ApplicationDescription{ApplicationURI: "urn:client:" + name}, "", "opc.tcp://localhost:4840", ua.ByteString(""), 0,
	)
}

func addDisabledMonitoredItem(t *testing.T, srv *Server, session *Session, node Node) *DataChangeMonitoredItem {
	t.Helper()
	subscription := NewSubscription(srv.subscriptionManager, session, 250, 20, 10, 0, true, 0)
	if err := srv.subscriptionManager.Add(subscription); err != nil {
		t.Fatal(err)
	}
	item := NewDataChangeMonitoredItem(
		subscription, node, ua.ReadValueID{NodeID: node.NodeID(), AttributeID: ua.AttributeIDValue}, ua.MonitoringModeDisabled,
		ua.MonitoringParameters{ClientHandle: 1, SamplingInterval: 250, QueueSize: 1, DiscardOldest: true},
		ua.TimestampsToReturnBoth, 0,
	)
	if !subscription.AppendItem(item) {
		t.Fatal("failed to append monitored item")
	}
	return item
}
