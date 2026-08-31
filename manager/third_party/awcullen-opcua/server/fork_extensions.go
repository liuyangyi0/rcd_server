package server

import (
	"fmt"
	"time"

	"github.com/awcullen/opcua/ua"
)

// EndpointProfile describes one advertised and accepted endpoint. Token flags
// allow callers to match an existing server's endpoint surface exactly.
type EndpointProfile struct {
	SecurityPolicyURI string
	SecurityMode      ua.MessageSecurityMode
	AllowAnonymous    bool
	AllowUserName     bool
	AllowX509         bool
}

func validateEndpointProfiles(profiles []EndpointProfile) error {
	if len(profiles) == 0 {
		return fmt.Errorf("at least one endpoint profile is required")
	}
	seen := make(map[string]struct{}, len(profiles))
	for _, p := range profiles {
		if p.SecurityPolicyURI == "" {
			return fmt.Errorf("endpoint security policy is required")
		}
		if p.SecurityPolicyURI == ua.SecurityPolicyURINone && p.SecurityMode != ua.MessageSecurityModeNone {
			return fmt.Errorf("SecurityPolicy None requires MessageSecurityMode None")
		}
		if p.SecurityPolicyURI != ua.SecurityPolicyURINone && p.SecurityMode == ua.MessageSecurityModeNone {
			return fmt.Errorf("secure policy %q cannot use MessageSecurityMode None", p.SecurityPolicyURI)
		}
		if p.SecurityMode != ua.MessageSecurityModeNone && p.SecurityMode != ua.MessageSecurityModeSign && p.SecurityMode != ua.MessageSecurityModeSignAndEncrypt {
			return fmt.Errorf("invalid MessageSecurityMode %d", p.SecurityMode)
		}
		key := fmt.Sprintf("%s/%d", p.SecurityPolicyURI, p.SecurityMode)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate endpoint profile %s", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (srv *Server) buildConfiguredEndpointDescriptions() []ua.EndpointDescription {
	profiles := append([]EndpointProfile(nil), srv.endpointProfiles...)
	eds := make([]ua.EndpointDescription, 0, len(profiles))
	for i, p := range profiles {
		tokens := make([]ua.UserTokenPolicy, 0, 3)
		if p.AllowAnonymous && srv.anonymousIdentityAuthenticator != nil {
			tokens = append(tokens, ua.UserTokenPolicy{
				PolicyID:          fmt.Sprintf("%s_%d", ua.UserTokenTypeAnonymous, i),
				TokenType:         ua.UserTokenTypeAnonymous,
				SecurityPolicyURI: ua.SecurityPolicyURINone,
			})
		}
		if p.AllowUserName && srv.userNameIdentityAuthenticator != nil {
			tokenPolicy := p.SecurityPolicyURI
			if tokenPolicy == ua.SecurityPolicyURINone {
				tokenPolicy = ua.SecurityPolicyURIBasic256Sha256
			}
			tokens = append(tokens, ua.UserTokenPolicy{
				PolicyID:          fmt.Sprintf("%s_%d", ua.UserTokenTypeUserName, i),
				TokenType:         ua.UserTokenTypeUserName,
				SecurityPolicyURI: tokenPolicy,
			})
		}
		if p.AllowX509 && srv.x509IdentityAuthenticator != nil {
			tokens = append(tokens, ua.UserTokenPolicy{
				PolicyID:          fmt.Sprintf("%s_%d", ua.UserTokenTypeCertificate, i),
				TokenType:         ua.UserTokenTypeCertificate,
				SecurityPolicyURI: p.SecurityPolicyURI,
			})
		}
		eds = append(eds, ua.EndpointDescription{
			EndpointURL:         srv.endpointURL,
			Server:              srv.localDescription,
			ServerCertificate:   ua.ByteString(srv.LocalCertificate()),
			SecurityMode:        p.SecurityMode,
			SecurityPolicyURI:   p.SecurityPolicyURI,
			TransportProfileURI: ua.TransportProfileURIUaTcpTransport,
			SecurityLevel:       byte(i),
			UserIdentityTokens:  tokens,
		})
	}
	return eds
}

// matchEndpoint returns only endpoints that the server advertises. Secure
// channels must not be opened with a policy/mode pair outside this list.
func (srv *Server) matchEndpoint(policyURI string, mode ua.MessageSecurityMode) (ua.EndpointDescription, ua.StatusCode) {
	policyKnown := false
	for _, endpoint := range srv.Endpoints() {
		if endpoint.TransportProfileURI != ua.TransportProfileURIUaTcpTransport || endpoint.SecurityPolicyURI != policyURI {
			continue
		}
		policyKnown = true
		if endpoint.SecurityMode == mode {
			return endpoint, ua.Good
		}
	}
	if policyKnown {
		return ua.EndpointDescription{}, ua.BadSecurityModeRejected
	}
	return ua.EndpointDescription{}, ua.BadSecurityPolicyRejected
}

type SessionEventKind uint8

const (
	SessionCreated SessionEventKind = iota + 1
	SessionActivated
	SessionAttached
	SessionDetached
	SessionClosed
)

type SessionCloseReason uint8

const (
	SessionCloseReasonNone SessionCloseReason = iota
	SessionCloseReasonClient
	SessionCloseReasonTimeout
	SessionCloseReasonServerShutdown
)

// SessionSnapshot is safe to retain after the callback returns.
type SessionSnapshot struct {
	SessionID            ua.NodeID
	SessionName          string
	ClientApplicationURI string
	EndpointURL          string
	RemoteAddress        string
	SecureChannelID      uint32
	SecurityMode         ua.MessageSecurityMode
	SecurityPolicyURI    string
	CreatedAt            time.Time
	LastAccess           time.Time
	Timeout              float64
}

type SessionEvent struct {
	Kind         SessionEventKind
	Reason       SessionCloseReason
	Session      SessionSnapshot
	OccurredAt   time.Time
	OldChannelID uint32
}

type SessionEventHandler func(SessionEvent)

func (srv *Server) emitSessionEvent(event SessionEvent) {
	if handler := srv.sessionEventHandler; handler != nil {
		handler(event)
	}
}

type queuedSessionEvent struct {
	event SessionEvent
	done  chan struct{}
}

// enqueueLifecycleEventLocked orders event snapshots with the state changes
// they describe. It requires the Session lock and never invokes user code.
func (s *Session) enqueueLifecycleEventLocked(event SessionEvent) <-chan struct{} {
	queued := &queuedSessionEvent{event: event, done: make(chan struct{})}
	s.lifecycleEvents = append(s.lifecycleEvents, queued)
	return queued.done
}

// drainLifecycleEvents serializes callbacks for one Session without holding
// any Session/Manager lock or creating a background goroutine. Reentrant
// Snapshot, Detach, and Delete calls are safe: new events are queued for the
// outer drainer instead of recursively invoking the callback.
func (s *Session) drainLifecycleEvents(srv *Server) {
	s.Lock()
	if s.drainingLifecycleEvents {
		s.Unlock()
		return
	}
	s.drainingLifecycleEvents = true
	for len(s.lifecycleEvents) > 0 {
		queued := s.lifecycleEvents[0]
		s.lifecycleEvents[0] = nil
		s.lifecycleEvents = s.lifecycleEvents[1:]
		s.Unlock()
		srv.emitSessionEvent(queued.event)
		close(queued.done)
		s.Lock()
	}
	s.lifecycleEvents = nil
	s.drainingLifecycleEvents = false
	s.Unlock()
}

// activateSessionBinding commits the authenticated binding and its event
// together. The service handler waits for this event BEFORE writing a success
// response; manager Session-local routes must exist before the client can Read.
// Only the internal activation path waits. Reentrant public lifecycle APIs do
// not wait on the callback which called them.
func (srv *Server) activateSessionBinding(s *Session, ch *serverSecureChannel, identity any, nonce ua.ByteString, locales []string) error {
	channelID, remoteAddress := ch.ChannelID(), ch.RemoteAddress()
	securityMode, policyURI := ch.SecurityMode(), ch.SecurityPolicyURI()
	s.Lock()
	if s.deleted {
		s.Unlock()
		return ua.BadSessionClosed
	}
	oldChannelID := s.channelId
	s.setUserIdentityLocked(identity)
	s.sessionNonce = nonce
	s.channelId = channelID
	s.channelDetached = false
	s.remoteAddress = remoteAddress
	s.securityMode = securityMode
	s.securityPolicyURI = policyURI
	s.localeIds = append([]string(nil), locales...)
	s.prunePublishRequestsLocked()
	kind := SessionActivated
	if oldChannelID != 0 && oldChannelID != channelID {
		kind = SessionAttached
	}
	done := s.enqueueLifecycleEventLocked(SessionEvent{
		Kind: kind, Session: snapshotSessionLocked(s),
		OccurredAt: time.Now(), OldChannelID: oldChannelID,
	})
	s.Unlock()
	s.drainLifecycleEvents(srv)
	select {
	case <-done:
		// A callback may explicitly close or detach this Session. Do not
		// report successful activation of a binding it has already removed.
		if status := s.publishChannelStatus(ch); status != ua.Good {
			return status
		}
		return nil
	case <-srv.closing:
		return ua.BadSessionClosed
	}
}

func snapshotSession(s *Session) SessionSnapshot {
	s.RLock()
	defer s.RUnlock()
	return snapshotSessionLocked(s)
}

func snapshotSessionLocked(s *Session) SessionSnapshot {
	return SessionSnapshot{
		SessionID:            s.sessionId,
		SessionName:          s.sessionName,
		ClientApplicationURI: s.clientDescription.ApplicationURI,
		EndpointURL:          s.endpointURL,
		RemoteAddress:        s.remoteAddress,
		SecureChannelID:      s.channelId,
		SecurityMode:         s.securityMode,
		SecurityPolicyURI:    s.securityPolicyURI,
		CreatedAt:            s.timeCreated,
		LastAccess:           s.lastAccess,
		Timeout:              s.timeout,
	}
}

// ForceResampleSessionNodes causes matching data-change monitored items to
// sample immediately. It is used after a session-local route change.
func (srv *Server) ForceResampleSessionNodes(sessionID ua.NodeID, nodeIDs []ua.NodeID) int {
	session, ok := srv.sessionManager.GetBySessionID(sessionID)
	if !ok {
		return 0
	}
	wanted := make(map[ua.NodeID]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		wanted[id] = struct{}{}
	}
	count := 0
	for _, sub := range srv.subscriptionManager.GetBySession(session) {
		for _, item := range sub.Items() {
			dc, ok := item.(*DataChangeMonitoredItem)
			if !ok {
				continue
			}
			if _, ok := wanted[dc.ItemToMonitor().NodeID]; ok {
				dc.ForceSample()
				count++
			}
		}
	}
	return count
}
