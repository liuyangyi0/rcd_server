package server

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/awcullen/opcua/ua"
)

type publishOp struct {
	ch        *serverSecureChannel
	requestId uint32
	req       *ua.PublishRequest
	results   []ua.StatusCode
}

type stateChangeOp struct {
	subscriptionId uint32
	message        ua.NotificationMessage
}

type browseCP struct {
	data []ua.ReferenceDescription
	max  int
}

type Session struct {
	sync.RWMutex
	sessionId                               ua.NodeID
	sessionName                             string
	authenticationToken                     ua.NodeID
	timeout                                 float64
	userIdentity                            any
	sessionNonce                            ua.ByteString
	lastAccess                              time.Time
	publishRequests                         chan *publishOp
	stateChanges                            chan *stateChangeOp
	channelId                               uint32
	channelDetached                         bool
	lifecycleEvents                         []*queuedSessionEvent
	drainingLifecycleEvents                 bool
	securityMode                            ua.MessageSecurityMode
	securityPolicyURI                       string
	clientCertificate                       ua.ByteString
	browseCPs                               map[uint32]browseCP
	lastBrowseCP                            uint32
	maxBrowseContinuationPoints             int
	historyCPs                              map[uint32]time.Time
	maxHistoryContinuationPoints            int
	clientDescription                       ua.ApplicationDescription
	serverUri                               string
	endpointURL                             string
	remoteAddress                           string
	maxResponseMessageSize                  uint32
	localeIds                               []string
	timeCreated                             time.Time
	requestCount                            uint32
	errorCount                              uint32
	unauthorizedRequestCount                uint32
	readCount                               uint32
	readErrorCount                          uint32
	historyReadCount                        uint32
	historyReadErrorCount                   uint32
	writeCount                              uint32
	writeErrorCount                         uint32
	historyUpdateCount                      uint32
	historyUpdateErrorCount                 uint32
	callCount                               uint32
	callErrorCount                          uint32
	createMonitoredItemsCount               uint32
	createMonitoredItemsErrorCount          uint32
	modifyMonitoredItemsCount               uint32
	modifyMonitoredItemsErrorCount          uint32
	setMonitoringModeCount                  uint32
	setMonitoringModeErrorCount             uint32
	setTriggeringCount                      uint32
	setTriggeringErrorCount                 uint32
	deleteMonitoredItemsCount               uint32
	deleteMonitoredItemsErrorCount          uint32
	createSubscriptionCount                 uint32
	createSubscriptionErrorCount            uint32
	modifySubscriptionCount                 uint32
	modifySubscriptionErrorCount            uint32
	setPublishingModeCount                  uint32
	setPublishingModeErrorCount             uint32
	publishCount                            uint32
	publishErrorCount                       uint32
	republishCount                          uint32
	republishErrorCount                     uint32
	transferSubscriptionsCount              uint32
	transferSubscriptionsErrorCount         uint32
	deleteSubscriptionsCount                uint32
	deleteSubscriptionsErrorCount           uint32
	addNodesCount                           uint32
	addNodesErrorCount                      uint32
	addReferencesCount                      uint32
	addReferencesErrorCount                 uint32
	deleteNodesCount                        uint32
	deleteNodesErrorCount                   uint32
	deleteReferencesCount                   uint32
	deleteReferencesErrorCount              uint32
	browseCount                             uint32
	browseErrorCount                        uint32
	browseNextCount                         uint32
	browseNextErrorCount                    uint32
	translateBrowsePathsToNodeIdsCount      uint32
	translateBrowsePathsToNodeIdsErrorCount uint32
	queryFirstCount                         uint32
	queryFirstErrorCount                    uint32
	queryNextCount                          uint32
	queryNextErrorCount                     uint32
	registerNodesCount                      uint32
	registerNodesErrorCount                 uint32
	unregisterNodesCount                    uint32
	unregisterNodesErrorCount               uint32
	clientUserIdOfSession                   string
	authenticationMechanism                 string
	clientUserIdHistory                     []string
	deleted                                 bool
}

func NewSession(server *Server, sessionId ua.NodeID, sessionName string, authenticationToken ua.NodeID, sessionNonce ua.ByteString, timeout float64, clientDescription ua.ApplicationDescription, serverUri string, endpointUrl string, clientCertificate ua.ByteString, maxResponseMessageSize uint32) *Session {
	return &Session{
		sessionId:                    sessionId,
		sessionName:                  sessionName,
		authenticationToken:          authenticationToken,
		timeout:                      timeout,
		sessionNonce:                 sessionNonce,
		securityMode:                 ua.MessageSecurityModeNone,
		securityPolicyURI:            ua.SecurityPolicyURINone,
		lastAccess:                   time.Now(),
		publishRequests:              make(chan *publishOp, 64),
		stateChanges:                 make(chan *stateChangeOp, 64),
		browseCPs:                    make(map[uint32]browseCP, 16),
		maxBrowseContinuationPoints:  int(server.ServerCapabilities().MaxBrowseContinuationPoints),
		historyCPs:                   make(map[uint32]time.Time, 16),
		maxHistoryContinuationPoints: int(server.ServerCapabilities().MaxHistoryContinuationPoints),
		clientDescription:            clientDescription,
		serverUri:                    serverUri,
		endpointURL:                  endpointUrl,
		localeIds:                    []string{"en-US"},
		maxResponseMessageSize:       maxResponseMessageSize,
		timeCreated:                  time.Now(),
		clientUserIdHistory:          []string{},
	}
}

func (s *Session) IsExpired() bool {
	s.RLock()
	defer s.RUnlock()
	return time.Since(s.lastAccess).Milliseconds() > int64(s.timeout)
}

func (s *Session) delete() {
	s.Lock()
	defer s.Unlock()
	s.deleteLocked()
}

func (s *Session) deleteLocked() {
	s.deleted = true
	//s.sessionId = nil  // need to keep to look up diagnostics node
	s.authenticationToken = nil
	s.userIdentity = nil
	s.channelId = 0
	s.securityMode = ua.MessageSecurityModeNone
	s.securityPolicyURI = ua.SecurityPolicyURINone
	s.sessionNonce = ua.ByteString("")
	s.clientCertificate = ua.ByteString("")
	// Keep the channel allocated: closing it would race senders and replacing
	// it with nil can block a queue consumer forever. Closed Sessions must not
	// let their queued requests consume any more monitored-item notifications.
	s.prunePublishRequestsLocked()
	for k := range s.browseCPs {
		delete(s.browseCPs, k)
	}
	s.browseCPs = nil
	for k := range s.historyCPs {
		delete(s.historyCPs, k)
	}
	s.historyCPs = nil
	s.clientUserIdHistory = nil
}

func (s *Session) SessionId() ua.NodeID {
	s.RLock()
	defer s.RUnlock()
	return s.sessionId
}

func (s *Session) SessionName() string {
	s.RLock()
	defer s.RUnlock()
	return s.sessionName
}

func (s *Session) AuthenticationToken() ua.NodeID {
	s.RLock()
	defer s.RUnlock()
	return s.authenticationToken
}

func (s *Session) Timeout() float64 {
	s.RLock()
	defer s.RUnlock()
	return s.timeout
}

func (s *Session) UserIdentity() any {
	s.RLock()
	defer s.RUnlock()
	return s.userIdentity
}

func (s *Session) SetUserIdentity(value any) {
	s.Lock()
	defer s.Unlock()
	s.setUserIdentityLocked(value)
}

func (s *Session) setUserIdentityLocked(value any) {
	s.userIdentity = value
	// update diagnostics
	switch ui := s.userIdentity.(type) {
	case ua.IssuedIdentity:
		s.clientUserIdOfSession = "<issued>"
		s.authenticationMechanism = "Issued"
	case ua.X509Identity:
		s.clientUserIdOfSession = "<certificate>"
		s.authenticationMechanism = "X509"
	case ua.UserNameIdentity:
		s.clientUserIdOfSession = ui.UserName
		s.authenticationMechanism = "UserName"
	default:
		s.clientUserIdOfSession = "<anonymous>"
		s.authenticationMechanism = "Anonymous"
	}
	s.clientUserIdHistory = append(s.clientUserIdHistory, s.clientUserIdOfSession)
}

func (s *Session) SessionNonce() ua.ByteString {
	s.RLock()
	defer s.RUnlock()
	return s.sessionNonce
}

func (s *Session) SetSessionNonce(value ua.ByteString) {
	s.Lock()
	defer s.Unlock()
	s.sessionNonce = value
}

func (s *Session) LastAccess() time.Time {
	s.RLock()
	defer s.RUnlock()
	return s.lastAccess
}

func (s *Session) SetLastAccess(value time.Time) {
	s.Lock()
	defer s.Unlock()
	s.lastAccess = value
}

func (s *Session) SetLocaleIDs(value []string) {
	s.Lock()
	defer s.Unlock()
	s.localeIds = append([]string(nil), value...)
}

// diagnosticsSnapshot returns owned slices and atomically loaded counters so
// diagnostics nodes can be sampled while request workers update the session.
func (s *Session) diagnosticsSnapshot() ua.SessionDiagnosticsDataType {
	s.RLock()
	clientDescription := s.clientDescription
	clientDescription.DiscoveryURLs = append([]string(nil), s.clientDescription.DiscoveryURLs...)
	d := ua.SessionDiagnosticsDataType{
		SessionID:                     s.sessionId,
		SessionName:                   s.sessionName,
		ClientDescription:             clientDescription,
		ServerURI:                     s.serverUri,
		EndpointURL:                   s.endpointURL,
		LocaleIDs:                     append([]string(nil), s.localeIds...),
		ActualSessionTimeout:          s.timeout,
		MaxResponseMessageSize:        s.maxResponseMessageSize,
		ClientConnectionTime:          s.timeCreated,
		ClientLastContactTime:         s.lastAccess,
		CurrentPublishRequestsInQueue: uint32(len(s.publishRequests)),
	}
	s.RUnlock()

	d.TotalRequestCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.requestCount), ErrorCount: atomic.LoadUint32(&s.errorCount)}
	d.UnauthorizedRequestCount = atomic.LoadUint32(&s.unauthorizedRequestCount)
	d.ReadCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.readCount), ErrorCount: atomic.LoadUint32(&s.readErrorCount)}
	d.HistoryReadCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.historyReadCount), ErrorCount: atomic.LoadUint32(&s.historyReadErrorCount)}
	d.WriteCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.writeCount), ErrorCount: atomic.LoadUint32(&s.writeErrorCount)}
	d.HistoryUpdateCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.historyUpdateCount), ErrorCount: atomic.LoadUint32(&s.historyUpdateErrorCount)}
	d.CallCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.callCount), ErrorCount: atomic.LoadUint32(&s.callErrorCount)}
	d.CreateMonitoredItemsCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.createMonitoredItemsCount), ErrorCount: atomic.LoadUint32(&s.createMonitoredItemsErrorCount)}
	d.ModifyMonitoredItemsCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.modifyMonitoredItemsCount), ErrorCount: atomic.LoadUint32(&s.modifyMonitoredItemsErrorCount)}
	d.SetMonitoringModeCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.setMonitoringModeCount), ErrorCount: atomic.LoadUint32(&s.setMonitoringModeErrorCount)}
	d.SetTriggeringCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.setTriggeringCount), ErrorCount: atomic.LoadUint32(&s.setTriggeringErrorCount)}
	d.DeleteMonitoredItemsCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.deleteMonitoredItemsCount), ErrorCount: atomic.LoadUint32(&s.deleteMonitoredItemsErrorCount)}
	d.CreateSubscriptionCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.createSubscriptionCount), ErrorCount: atomic.LoadUint32(&s.createSubscriptionErrorCount)}
	d.ModifySubscriptionCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.modifySubscriptionCount), ErrorCount: atomic.LoadUint32(&s.modifySubscriptionErrorCount)}
	d.SetPublishingModeCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.setPublishingModeCount), ErrorCount: atomic.LoadUint32(&s.setPublishingModeErrorCount)}
	d.PublishCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.publishCount), ErrorCount: atomic.LoadUint32(&s.publishErrorCount)}
	d.RepublishCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.republishCount), ErrorCount: atomic.LoadUint32(&s.republishErrorCount)}
	d.TransferSubscriptionsCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.transferSubscriptionsCount), ErrorCount: atomic.LoadUint32(&s.transferSubscriptionsErrorCount)}
	d.DeleteSubscriptionsCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.deleteSubscriptionsCount), ErrorCount: atomic.LoadUint32(&s.deleteSubscriptionsErrorCount)}
	d.AddNodesCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.addNodesCount), ErrorCount: atomic.LoadUint32(&s.addNodesErrorCount)}
	d.AddReferencesCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.addReferencesCount), ErrorCount: atomic.LoadUint32(&s.addReferencesErrorCount)}
	d.DeleteNodesCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.deleteNodesCount), ErrorCount: atomic.LoadUint32(&s.deleteNodesErrorCount)}
	d.DeleteReferencesCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.deleteReferencesCount), ErrorCount: atomic.LoadUint32(&s.deleteReferencesErrorCount)}
	d.BrowseCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.browseCount), ErrorCount: atomic.LoadUint32(&s.browseErrorCount)}
	d.BrowseNextCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.browseNextCount), ErrorCount: atomic.LoadUint32(&s.browseNextErrorCount)}
	d.TranslateBrowsePathsToNodeIDsCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.translateBrowsePathsToNodeIdsCount), ErrorCount: atomic.LoadUint32(&s.translateBrowsePathsToNodeIdsErrorCount)}
	d.QueryFirstCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.queryFirstCount), ErrorCount: atomic.LoadUint32(&s.queryFirstErrorCount)}
	d.QueryNextCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.queryNextCount), ErrorCount: atomic.LoadUint32(&s.queryNextErrorCount)}
	d.RegisterNodesCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.registerNodesCount), ErrorCount: atomic.LoadUint32(&s.registerNodesErrorCount)}
	d.UnregisterNodesCount = ua.ServiceCounterDataType{TotalCount: atomic.LoadUint32(&s.unregisterNodesCount), ErrorCount: atomic.LoadUint32(&s.unregisterNodesErrorCount)}
	return d
}

func (s *Session) securityDiagnosticsSnapshot() ua.SessionSecurityDiagnosticsDataType {
	s.RLock()
	defer s.RUnlock()
	return ua.SessionSecurityDiagnosticsDataType{
		SessionID:               s.sessionId,
		ClientUserIDOfSession:   s.clientUserIdOfSession,
		ClientUserIDHistory:     append([]string(nil), s.clientUserIdHistory...),
		AuthenticationMechanism: s.authenticationMechanism,
		Encoding:                "UA Binary",
		TransportProtocol:       ua.TransportProfileURIUaTcpTransport,
		SecurityMode:            s.securityMode,
		SecurityPolicyURI:       s.securityPolicyURI,
		ClientCertificate:       s.clientCertificate,
	}
}

func (s *Session) SecureChannelId() uint32 {
	s.RLock()
	defer s.RUnlock()
	return s.channelId
}

func (s *Session) SetSecureChannelId(value uint32) {
	s.Lock()
	defer s.Unlock()
	s.channelId = value
	s.channelDetached = false
	// Rebinding and queue cleanup share the same lock as admission/removal.
	// Preserve requests for this channel, including on same-channel activation;
	// never let a delayed cleanup discard requests admitted on the new channel.
	s.prunePublishRequestsLocked()
}

func (s *Session) SetRemoteAddress(value string) {
	s.Lock()
	defer s.Unlock()
	s.remoteAddress = value
}

func (s *Session) SecurityMode() ua.MessageSecurityMode {
	s.RLock()
	defer s.RUnlock()
	return s.securityMode
}

func (s *Session) SetSecurityMode(value ua.MessageSecurityMode) {
	s.Lock()
	defer s.Unlock()
	s.securityMode = value
}

func (s *Session) SecurityPolicyURI() string {
	s.RLock()
	defer s.RUnlock()
	return s.securityPolicyURI
}

func (s *Session) SetSecurityPolicyURI(value string) {
	s.Lock()
	defer s.Unlock()
	s.securityPolicyURI = value
}

func (s *Session) ClientCertificate() ua.ByteString {
	s.RLock()
	defer s.RUnlock()
	return s.clientCertificate
}

// publishChannelStatusLocked requires the Session lock. No Subscription lock
// or network write is acquired here; publishing uses Subscription -> Session.
func (s *Session) publishChannelStatusLocked(ch *serverSecureChannel) ua.StatusCode {
	if s.deleted {
		return ua.BadSessionClosed
	}
	if ch.IsClosed() {
		return ua.BadSecureChannelClosed
	}
	if s.channelDetached {
		return ua.BadSecureChannelClosed
	}
	if s.channelId == 0 || s.channelId != ch.ChannelID() {
		return ua.BadSecureChannelIDInvalid
	}
	if ch.IsExpired() {
		return ua.BadSecureChannelClosed
	}
	return ua.Good
}

func (s *Session) publishChannelStatus(ch *serverSecureChannel) ua.StatusCode {
	s.RLock()
	defer s.RUnlock()
	return s.publishChannelStatusLocked(ch)
}

func (s *Session) prunePublishRequestsLocked() {
	// Every producer/consumer holds this lock, so rotating exactly the original
	// queue length preserves FIFO order without blocking or dropping new work.
	for n := len(s.publishRequests); n > 0; n-- {
		op := <-s.publishRequests
		if op != nil && s.publishChannelStatusLocked(op.ch) == ua.Good {
			s.publishRequests <- op
		}
	}
}

func (s *Session) addPublishRequest(ch *serverSecureChannel, requestid uint32, req *ua.PublishRequest, results []ua.StatusCode) error {
	for {
		var rejected *publishOp
		s.Lock()
		if status := s.publishChannelStatusLocked(ch); status != ua.Good {
			s.Unlock()
			return status
		}
		select {
		case s.publishRequests <- &publishOp{ch, requestid, req, results}:
			s.Unlock()
			return nil
		default:
			// The queue may have been drained between the failed send and this
			// receive only if another code path bypasses these helpers. Keep the
			// receive non-blocking so this function can never stall while holding
			// the Session lock.
			select {
			case rejected = <-s.publishRequests:
			default:
			}
			s.Unlock()
			if rejected == nil || s.publishChannelStatus(rejected.ch) != ua.Good {
				continue
			}
			// A response on a superseded/broken channel must not reject the
			// incoming request on a healthy replacement channel. Transport write
			// failures invalidate only the rejected request's channel.
			_ = rejected.ch.Write(
				&ua.ServiceFault{
					ResponseHeader: ua.ResponseHeader{
						Timestamp:     time.Now(),
						RequestHandle: rejected.req.RequestHandle,
						ServiceResult: ua.BadTooManyPublishRequests,
					},
				},
				rejected.requestId,
			)
		}
	}
}

func (s *Session) removePublishRequest() (*serverSecureChannel, uint32, *ua.PublishRequest, []ua.StatusCode, bool, error) {
	for {
		s.Lock()
		var op *publishOp
		select {
		case op = <-s.publishRequests:
			if op == nil || s.publishChannelStatusLocked(op.ch) != ua.Good {
				s.Unlock()
				continue
			}
			s.Unlock()
			ch := op.ch
			req := op.req
			rid := op.requestId
			results := op.results
			// check if expired
			if req.TimeoutHint > 0 && time.Now().After(req.Timestamp.Add(time.Duration(req.TimeoutHint)*time.Millisecond)) {
				// Best effort only: a timeout response may race reconnection. Its
				// failure must not prevent this call from finding the next valid
				// Publish request (which may already be on the replacement channel).
				_ = ch.Write(
					&ua.ServiceFault{
						ResponseHeader: ua.ResponseHeader{
							Timestamp:     time.Now(),
							RequestHandle: req.RequestHandle,
							ServiceResult: ua.BadTimeout,
						},
					},
					rid,
				)
				continue
			}
			return ch, rid, req, results, true, nil
		default:
			s.Unlock()
			return nil, 0, nil, nil, false, nil
		}
	}
}

func (s *Session) addBrowseContinuationPoint(data []ua.ReferenceDescription, max int) ([]byte, error) {
	s.Lock()
	defer s.Unlock()
	if s.deleted {
		return nil, ua.BadSessionClosed
	}
	if s.maxBrowseContinuationPoints > 0 && len(s.browseCPs) >= s.maxBrowseContinuationPoints {
		return nil, ua.BadNoContinuationPoints
	}
	id := atomic.AddUint32(&s.lastBrowseCP, 1)
	s.browseCPs[id] = browseCP{data, max}
	cp := make([]byte, 4)
	binary.LittleEndian.PutUint32(cp, id)
	return cp, nil
}

func (s *Session) removeBrowseContinuationPoint(cp []byte) ([]ua.ReferenceDescription, int, bool) {
	if cp == nil {
		return nil, 0, false
	}
	s.Lock()
	defer s.Unlock()
	id := binary.LittleEndian.Uint32(cp)
	if x, ok := s.browseCPs[id]; ok {
		delete(s.browseCPs, id)
		return x.data, x.max, ok
	}
	return nil, 0, false
}
