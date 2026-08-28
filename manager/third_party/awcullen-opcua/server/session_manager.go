package server

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/awcullen/opcua/ua"
	"github.com/google/uuid"
)

// SessionManager manages the sessions for a server.
type SessionManager struct {
	sync.RWMutex
	server          *Server
	sessionsByToken map[ua.NodeID]*Session
	maxSessionCount int
}

// NewSessionManager instantiates a new SessionManager.
func NewSessionManager(server *Server) *SessionManager {
	m := &SessionManager{server: server, sessionsByToken: make(map[ua.NodeID]*Session), maxSessionCount: int(server.MaxSessionCount())}
	go func(m *SessionManager) {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.checkForExpiredSessions()
			case <-m.server.closing:
				return
			}
		}
	}(m)
	return m
}

// Get a session from the server by authenticationToken.
func (m *SessionManager) Get(authenticationToken ua.NodeID) (*Session, bool) {
	m.RLock()
	defer m.RUnlock()
	if s, ok := m.sessionsByToken[authenticationToken]; ok && !s.IsExpired() {
		s.SetLastAccess(time.Now())
		return s, ok
	}
	return nil, false
}

// Add a session to the server.
func (m *SessionManager) Add(s *Session) error {
	m.Lock()
	if m.maxSessionCount > 0 && len(m.sessionsByToken) >= m.maxSessionCount {
		m.Unlock()
		return ua.BadTooManySessions
	}
	m.sessionsByToken[s.authenticationToken] = s
	if m.server.serverDiagnostics {
		m.addDiagnosticsNode(s)
		m.server.Lock()
		atomic.AddUint32(&m.server.serverDiagnosticsSummary.CumulatedSessionCount, 1)
		atomic.StoreUint32(&m.server.serverDiagnosticsSummary.CurrentSessionCount, uint32(len(m.sessionsByToken)))
		m.server.Unlock()
	}
	m.Unlock()
	m.server.emitSessionEvent(SessionEvent{
		Kind:       SessionCreated,
		Session:    snapshotSession(s),
		OccurredAt: time.Now(),
	})
	return nil
}

// Delete the session from the server.
func (m *SessionManager) Delete(s *Session) {
	m.DeleteWithReason(s, SessionCloseReasonClient)
}

// DeleteWithReason removes a session and emits one terminal lifecycle event.
func (m *SessionManager) DeleteWithReason(s *Session, reason SessionCloseReason) {
	m.Lock()
	if _, exists := m.sessionsByToken[s.authenticationToken]; !exists {
		m.Unlock()
		return
	}
	snapshot := snapshotSession(s)
	delete(m.sessionsByToken, s.authenticationToken)
	if m.server.serverDiagnostics {
		m.removeDiagnosticsNode(s)
		m.server.Lock()
		atomic.StoreUint32(&m.server.serverDiagnosticsSummary.CurrentSessionCount, uint32(len(m.sessionsByToken)))
		m.server.Unlock()
	}
	s.delete()
	m.Unlock()
	m.server.emitSessionEvent(SessionEvent{
		Kind:       SessionClosed,
		Reason:     reason,
		Session:    snapshot,
		OccurredAt: time.Now(),
	})
}

// Len returns the number of sessions.
func (m *SessionManager) Len() int {
	m.RLock()
	defer m.RUnlock()
	return len(m.sessionsByToken)
}

func (m *SessionManager) checkForExpiredSessions() {
	m.Lock()
	closed := make([]SessionSnapshot, 0)
	for k, s := range m.sessionsByToken {
		if s.IsExpired() {
			closed = append(closed, snapshotSession(s))
			delete(m.sessionsByToken, k)
			if m.server.serverDiagnostics {
				m.removeDiagnosticsNode(s)
				m.server.Lock()
				atomic.AddUint32(&m.server.serverDiagnosticsSummary.SessionTimeoutCount, 1)
				atomic.StoreUint32(&m.server.serverDiagnosticsSummary.CurrentSessionCount, uint32(len(m.sessionsByToken)))
				m.server.Unlock()
			}
			s.delete()
		}
	}
	m.Unlock()
	for _, snapshot := range closed {
		m.server.emitSessionEvent(SessionEvent{
			Kind:       SessionClosed,
			Reason:     SessionCloseReasonTimeout,
			Session:    snapshot,
			OccurredAt: time.Now(),
		})
	}
}

// GetBySessionID returns a live session by its public SessionId.
func (m *SessionManager) GetBySessionID(sessionID ua.NodeID) (*Session, bool) {
	m.RLock()
	defer m.RUnlock()
	for _, s := range m.sessionsByToken {
		if s.SessionId() == sessionID {
			return s, true
		}
	}
	return nil, false
}

// Snapshots returns safe immutable session information.
func (m *SessionManager) Snapshots() []SessionSnapshot {
	m.RLock()
	defer m.RUnlock()
	result := make([]SessionSnapshot, 0, len(m.sessionsByToken))
	for _, s := range m.sessionsByToken {
		result = append(result, snapshotSession(s))
	}
	return result
}

// DetachChannel marks sessions on a terminated SecureChannel as detached but
// keeps them alive until CloseSession or SessionTimeout, allowing reactivation.
func (m *SessionManager) DetachChannel(channelID uint32) {
	if channelID == 0 {
		return
	}
	m.RLock()
	sessions := make([]*Session, 0)
	for _, s := range m.sessionsByToken {
		if s.SecureChannelId() == channelID {
			sessions = append(sessions, s)
		}
	}
	m.RUnlock()
	for _, s := range sessions {
		snapshot := snapshotSession(s)
		m.server.emitSessionEvent(SessionEvent{
			Kind:         SessionDetached,
			Session:      snapshot,
			OccurredAt:   time.Now(),
			OldChannelID: channelID,
		})
	}
}

// CloseAll removes every session using a common terminal reason.
func (m *SessionManager) CloseAll(reason SessionCloseReason) {
	m.RLock()
	sessions := make([]*Session, 0, len(m.sessionsByToken))
	for _, s := range m.sessionsByToken {
		sessions = append(sessions, s)
	}
	m.RUnlock()
	for _, s := range sessions {
		m.DeleteWithReason(s, reason)
	}
}

func (m *SessionManager) addDiagnosticsNode(s *Session) {
	srv := m.server
	nm := srv.NamespaceManager()
	nodes := []Node{}
	sessionDiagnosticsObject := NewObjectNode(
		srv,
		s.sessionId,
		ua.NewQualifiedName(1, s.sessionName),
		ua.NewLocalizedText(s.sessionName, ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.ObjectTypeIDSessionDiagnosticsObjectType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(ua.ObjectIDServerServerDiagnosticsSessionsDiagnosticsSummary)),
		},
		byte(0),
	)
	nodes = append(nodes, sessionDiagnosticsObject)
	sessionDiagnosticsVariable := NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SessionDiagnostics"),
		ua.NewLocalizedText("SessionDiagnostics", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDSessionDiagnosticsVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsObject.NodeID())),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(ua.VariableIDServerServerDiagnosticsSessionsDiagnosticsSummarySessionDiagnosticsArray)),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDSessionDiagnosticsDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	sessionDiagnosticsVariable.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		subs := srv.subscriptionManager.GetBySession(s)
		itemCount := 0
		for _, sub := range subs {
			itemCount += len(sub.Items())
		}
		diagnostics := s.diagnosticsSnapshot()
		diagnostics.CurrentSubscriptionsCount = uint32(len(subs))
		diagnostics.CurrentMonitoredItemsCount = uint32(itemCount)
		return ua.NewDataValue(diagnostics, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, sessionDiagnosticsVariable)
	n := NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SessionId"),
		ua.NewLocalizedText("SessionId", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDNodeID,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().SessionID, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SessionName"),
		ua.NewLocalizedText("SessionName", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().SessionName, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ClientDescription"),
		ua.NewLocalizedText("ClientDescription", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ClientDescription, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ServerUri"),
		ua.NewLocalizedText("ServerUri", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ServerURI, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "EndpointUrl"),
		ua.NewLocalizedText("EndpointUrl", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().EndpointURL, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "LocaleIds"),
		ua.NewLocalizedText("LocaleIds", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankOneDimension,
		[]uint32{0},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().LocaleIDs, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ActualSessionTimeout"),
		ua.NewLocalizedText("ActualSessionTimeout", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDDouble,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ActualSessionTimeout, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "MaxResponseMessageSize"),
		ua.NewLocalizedText("MaxResponseMessageSize", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDUInt32,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().MaxResponseMessageSize, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ClientConnectionTime"),
		ua.NewLocalizedText("ClientConnectionTime", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDDateTime,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ClientConnectionTime, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ClientLastContactTime"),
		ua.NewLocalizedText("ClientLastContactTime", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDDateTime,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ClientLastContactTime, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CurrentSubscriptionsCount"),
		ua.NewLocalizedText("CurrentSubscriptionsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDUInt32,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(uint32(len(srv.subscriptionManager.GetBySession(s))), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CurrentMonitoredItemsCount"),
		ua.NewLocalizedText("CurrentMonitoredItemsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDUInt32,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		itemCount := 0
		subs := srv.subscriptionManager.GetBySession(s)
		for _, sub := range subs {
			itemCount += len(sub.Items())
		}
		return ua.NewDataValue(uint32(itemCount), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CurrentPublishRequestsInQueue"),
		ua.NewLocalizedText("CurrentPublishRequestsInQueue", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDUInt32,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().CurrentPublishRequestsInQueue, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TotalRequestCount"),
		ua.NewLocalizedText("TotalRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().TotalRequestCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "UnauthorizedRequestCount"),
		ua.NewLocalizedText("UnauthorizedRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDUInt32,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().UnauthorizedRequestCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ReadCount"),
		ua.NewLocalizedText("ReadCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ReadCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "HistoryReadCount"),
		ua.NewLocalizedText("HistoryReadCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().HistoryReadCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "WriteCount"),
		ua.NewLocalizedText("WriteCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().WriteCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "HistoryUpdateCount"),
		ua.NewLocalizedText("HistoryUpdateCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().HistoryUpdateCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CallCount"),
		ua.NewLocalizedText("CallCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().CallCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CreateMonitoredItemsCount"),
		ua.NewLocalizedText("CreateMonitoredItemsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().CreateMonitoredItemsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ModifyMonitoredItemsCount"),
		ua.NewLocalizedText("ModifyMonitoredItemsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ModifyMonitoredItemsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SetMonitoringModeCount"),
		ua.NewLocalizedText("SetMonitoringModeCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().SetMonitoringModeCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SetTriggeringCount"),
		ua.NewLocalizedText("SetTriggeringCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().SetTriggeringCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DeleteMonitoredItemsCount"),
		ua.NewLocalizedText("DeleteMonitoredItemsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().DeleteMonitoredItemsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CreateSubscriptionCount"),
		ua.NewLocalizedText("CreateSubscriptionCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().CreateSubscriptionCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ModifySubscriptionCount"),
		ua.NewLocalizedText("ModifySubscriptionCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().ModifySubscriptionCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SetPublishingModeCount"),
		ua.NewLocalizedText("SetPublishingModeCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().SetPublishingModeCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "PublishCount"),
		ua.NewLocalizedText("PublishCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().PublishCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "RepublishCount"),
		ua.NewLocalizedText("RepublishCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().RepublishCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TransferSubscriptionsCount"),
		ua.NewLocalizedText("TransferSubscriptionsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().TransferSubscriptionsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DeleteSubscriptionsCount"),
		ua.NewLocalizedText("DeleteSubscriptionsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().DeleteSubscriptionsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "AddNodesCount"),
		ua.NewLocalizedText("AddNodesCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().AddNodesCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "AddReferencesCount"),
		ua.NewLocalizedText("AddReferencesCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().AddReferencesCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DeleteNodesCount"),
		ua.NewLocalizedText("DeleteNodesCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().DeleteNodesCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DeleteReferencesCount"),
		ua.NewLocalizedText("DeleteReferencesCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().DeleteReferencesCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "BrowseCount"),
		ua.NewLocalizedText("BrowseCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().BrowseCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "BrowseNextCount"),
		ua.NewLocalizedText("BrowseNextCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().BrowseNextCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TranslateBrowsePathsToNodeIdsCount"),
		ua.NewLocalizedText("TranslateBrowsePathsToNodeIdsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().TranslateBrowsePathsToNodeIDsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "QueryFirstCount"),
		ua.NewLocalizedText("QueryFirstCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().QueryFirstCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "QueryNextCount"),
		ua.NewLocalizedText("QueryNextCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().QueryNextCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "RegisterNodesCount"),
		ua.NewLocalizedText("RegisterNodesCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().RegisterNodesCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "UnregisterNodesCount"),
		ua.NewLocalizedText("UnregisterNodesCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBaseDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().UnregisterNodesCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)

	// SessionSecurityDiagnostics
	sessionSecurityDiagnosticsVariable := NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SessionSecurityDiagnostics"),
		ua.NewLocalizedText("SessionSecurityDiagnostics", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDSessionSecurityDiagnosticsType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsObject.NodeID())),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(ua.VariableIDServerServerDiagnosticsSessionsDiagnosticsSummarySessionSecurityDiagnosticsArray)),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDSessionSecurityDiagnosticsDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	sessionSecurityDiagnosticsVariable.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		security := s.securityDiagnosticsSnapshot()
		return ua.NewDataValue(ua.SessionSecurityDiagnosticsDataType{
			SessionID:               security.SessionID,
			ClientUserIDOfSession:   security.ClientUserIDOfSession,
			ClientUserIDHistory:     security.ClientUserIDHistory,
			AuthenticationMechanism: security.AuthenticationMechanism,
			Encoding:                "UA Binary",
			TransportProtocol:       ua.TransportProfileURIUaTcpTransport,
			SecurityMode:            session.SecurityMode(),
			SecurityPolicyURI:       session.SecurityPolicyURI(),
			ClientCertificate:       session.ClientCertificate(),
		}, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, sessionSecurityDiagnosticsVariable)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SessionId"),
		ua.NewLocalizedText("SessionId", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDNodeID,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(s.diagnosticsSnapshot().SessionID, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ClientUserIdOfSession"),
		ua.NewLocalizedText("ClientUserIdOfSession", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(s.securityDiagnosticsSnapshot().ClientUserIDOfSession, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ClientUserIdHistory"),
		ua.NewLocalizedText("ClientUserIdHistory", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankOneDimension,
		[]uint32{0},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(s.securityDiagnosticsSnapshot().ClientUserIDHistory, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "AuthenticationMechanism"),
		ua.NewLocalizedText("AuthenticationMechanism", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(s.securityDiagnosticsSnapshot().AuthenticationMechanism, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "Encoding"),
		ua.NewLocalizedText("Encoding", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue("UA Binary", 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TransportProtocol"),
		ua.NewLocalizedText("TransportProtocol", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(ua.TransportProfileURIUaTcpTransport, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SecurityMode"),
		ua.NewLocalizedText("SecurityMode", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDMessageSecurityMode,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(int32(session.SecurityMode()), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SecurityPolicyURI"),
		ua.NewLocalizedText("SecurityPolicyURI", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(session.SecurityPolicyURI(), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ClientCertificate"),
		ua.NewLocalizedText("ClientCertificate", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionSecurityDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDByteString,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		if session == nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		roles, err := n.server.GetRoles(session.UserIdentity(), "", "")
		if err != nil {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		ok := false
		for _, n := range roles {
			if n == ua.ObjectIDWellKnownRoleAuthenticatedUser {
				ok = true
			}
		}
		if !ok {
			return ua.NewDataValue(nil, ua.BadUserAccessDenied, time.Now(), 0, time.Now(), 0)
		}
		if session.SecurityMode() != ua.MessageSecurityModeSignAndEncrypt {
			return ua.NewDataValue(nil, ua.BadSecurityModeInsufficient, time.Now(), 0, time.Now(), 0)
		}
		return ua.NewDataValue(session.ClientCertificate(), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)

	// SubscriptionDiagnostics
	subscriptionDiagnosticsArrayVariable := NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SubscriptionDiagnosticsArray"),
		ua.NewLocalizedText("SubscriptionDiagnosticsArray", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDSubscriptionDiagnosticsArrayType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(sessionDiagnosticsObject.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDSubscriptionDiagnosticsDataType,
		ua.ValueRankOneDimension,
		[]uint32{0},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	subscriptionDiagnosticsArrayVariable.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue([]ua.ExtensionObject{}, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, subscriptionDiagnosticsArrayVariable)

	err := nm.AddNodes(nodes...)
	if err != nil {
		log.Printf("Error adding session diagnostics objects.\n")
	}

}

func (m *SessionManager) removeDiagnosticsNode(s *Session) {
	if n, ok := m.server.NamespaceManager().FindNode(s.SessionId()); ok {
		m.server.NamespaceManager().DeleteNode(n, true)
	}
}
