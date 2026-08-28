package server

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/awcullen/opcua/ua"
	"github.com/google/uuid"
)

// SubscriptionManager manages the subscriptions for a server.
type SubscriptionManager struct {
	sync.RWMutex
	server               *Server
	subscriptionsByID    map[uint32]*Subscription
	maxSubscriptionCount int
}

// NewSubscriptionManager instantiates a new SubscriptionManager.
func NewSubscriptionManager(server *Server) *SubscriptionManager {
	m := &SubscriptionManager{server: server, subscriptionsByID: make(map[uint32]*Subscription), maxSubscriptionCount: int(server.MaxSubscriptionCount())}
	go func(m *SubscriptionManager) {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.checkForExpiredSubscriptions()
			case <-m.server.closing:
				m.stopPublishingAllSubscriptions()
				return
			}
		}
	}(m)
	return m
}

// Get a subscription from the server.
func (m *SubscriptionManager) Get(id uint32) (*Subscription, bool) {
	m.RLock()
	defer m.RUnlock()
	if s, ok := m.subscriptionsByID[id]; ok {
		return s, ok
	}
	return nil, false
}

// Add a subscription to the server.
func (m *SubscriptionManager) Add(s *Subscription) error {
	m.Lock()
	defer m.Unlock()
	if m.maxSubscriptionCount > 0 && len(m.subscriptionsByID) >= m.maxSubscriptionCount {
		return ua.BadTooManySubscriptions
	}
	m.subscriptionsByID[s.id] = s
	if m.server.serverDiagnostics {
		m.addDiagnosticsNode(s)
		atomic.AddUint32(&m.server.serverDiagnosticsSummary.CumulatedSubscriptionCount, 1)
		atomic.StoreUint32(&m.server.serverDiagnosticsSummary.CurrentSubscriptionCount, uint32(len(m.subscriptionsByID)))
	}
	return nil
}

// Delete the subscription from the server.
func (m *SubscriptionManager) Delete(s *Subscription) {
	m.Lock()
	if current, ok := m.subscriptionsByID[s.id]; !ok || current != s {
		m.Unlock()
		return
	}
	delete(m.subscriptionsByID, s.id)
	count := uint32(len(m.subscriptionsByID))
	m.Unlock()
	if m.server.serverDiagnostics {
		m.removeDiagnosticsNode(s)
		atomic.StoreUint32(&m.server.serverDiagnosticsSummary.CurrentSubscriptionCount, count)
	}
}

// Len returns the number of subscriptions.
func (m *SubscriptionManager) Len() int {
	m.RLock()
	defer m.RUnlock()
	return len(m.subscriptionsByID)
}

// GetBySession returns subscriptions for the session.
func (m *SubscriptionManager) GetBySession(session *Session) []*Subscription {
	subs := make([]*Subscription, 0, 4)
	for _, sub := range m.Subscriptions() {
		if sub.belongsToSession(session) {
			subs = append(subs, sub)
		}
	}
	return subs
}

// Subscriptions returns a stable pointer snapshot. Callers must release the
// manager lock before taking an individual Subscription lock to preserve the
// Subscription -> SubscriptionManager ordering used by the publishing path.
func (m *SubscriptionManager) Subscriptions() []*Subscription {
	m.RLock()
	defer m.RUnlock()
	subs := make([]*Subscription, 0, len(m.subscriptionsByID))
	for _, sub := range m.subscriptionsByID {
		subs = append(subs, sub)
	}
	return subs
}

func (m *SubscriptionManager) checkForExpiredSubscriptions() {
	for _, s := range m.Subscriptions() {
		if s.IsExpired() {
			m.Delete(s)
			s.Delete()
		}
	}
}

func (m *SubscriptionManager) stopPublishingAllSubscriptions() {
	for _, v := range m.Subscriptions() {
		v.stopPublishing()
	}
}

func (m *SubscriptionManager) addDiagnosticsNode(s *Subscription) {
	srv := m.server
	nm := srv.NamespaceManager()
	nodes := []Node{}

	refs := []ua.Reference{
		ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDSubscriptionDiagnosticsType)),
		ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(ua.VariableIDServerServerDiagnosticsSubscriptionDiagnosticsArray)),
	}
	if n1, ok := nm.FindNode(s.sessionId); ok {
		if n2, ok := nm.FindComponent(n1, ua.NewQualifiedName(0, "SubscriptionDiagnosticsArray")); ok {
			refs = append(refs, ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(n2.NodeID())))
		}
	}
	subscriptionDiagnosticsVariable := NewVariableNode(
		srv,
		s.diagnosticsNodeId,
		ua.NewQualifiedName(uint16(1), fmt.Sprint(s.id)),
		ua.NewLocalizedText(fmt.Sprint(s.id), ""),
		ua.NewLocalizedText("", ""),
		nil,
		refs,
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDSubscriptionDiagnosticsDataType,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	subscriptionDiagnosticsVariable.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot(), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, subscriptionDiagnosticsVariable)
	n := NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "SessionId"),
		ua.NewLocalizedText("SessionId", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		ua.NewQualifiedName(0, "SubscriptionId"),
		ua.NewLocalizedText("SubscriptionId", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().SubscriptionID, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "Priority"),
		ua.NewLocalizedText("Priority", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDByte,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().Priority, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "PublishingInterval"),
		ua.NewLocalizedText("PublishingInterval", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().PublishingInterval, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "MaxKeepAliveCount"),
		ua.NewLocalizedText("MaxKeepAliveCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().MaxKeepAliveCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "MaxLifetimeCount"),
		ua.NewLocalizedText("MaxLifetimeCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().MaxLifetimeCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "MaxNotificationsPerPublish"),
		ua.NewLocalizedText("MaxNotificationsPerPublish", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().MaxNotificationsPerPublish, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "PublishingEnabled"),
		ua.NewLocalizedText("PublishingEnabled", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
		},
		ua.NewDataValue(nil, ua.BadWaitingForInitialData, time.Now(), 0, time.Now(), 0),
		ua.DataTypeIDBoolean,
		ua.ValueRankScalar,
		[]uint32{},
		ua.AccessLevelsCurrentRead,
		125,
		false,
		srv.historian,
	)
	n.SetReadValueHandler(func(session *Session, req ua.ReadValueID) ua.DataValue {
		return ua.NewDataValue(s.diagnosticsSnapshot().PublishingEnabled, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "ModifyCount"),
		ua.NewLocalizedText("ModifyCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().ModifyCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "EnableCount"),
		ua.NewLocalizedText("EnableCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DisableCount"),
		ua.NewLocalizedText("DisableCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "RepublishRequestCount"),
		ua.NewLocalizedText("RepublishRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().RepublishRequestCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "RepublishMessageRequestCount"),
		ua.NewLocalizedText("RepublishMessageRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().RepublishMessageRequestCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "RepublishMessageCount"),
		ua.NewLocalizedText("RepublishMessageCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().RepublishMessageCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TransferRequestCount"),
		ua.NewLocalizedText("TransferRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TransferredToAltClientCount"),
		ua.NewLocalizedText("TransferredToAltClientCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "TransferredToSameClientCount"),
		ua.NewLocalizedText("TransferredToSameClientCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "PublishRequestCount"),
		ua.NewLocalizedText("PublishRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().PublishRequestCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DataChangeNotificationsCount"),
		ua.NewLocalizedText("DataChangeNotificationsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().DataChangeNotificationsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "EventNotificationsCount"),
		ua.NewLocalizedText("EventNotificationsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().EventNotificationsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "NotificationsCount"),
		ua.NewLocalizedText("NotificationsCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().NotificationsCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "LatePublishRequestCount"),
		ua.NewLocalizedText("LatePublishRequestCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().LatePublishRequestCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CurrentKeepAliveCount"),
		ua.NewLocalizedText("CurrentKeepAliveCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().CurrentKeepAliveCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "CurrentLifetimeCount"),
		ua.NewLocalizedText("CurrentLifetimeCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().CurrentLifetimeCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "UnacknowledgedMessageCount"),
		ua.NewLocalizedText("UnacknowledgedMessageCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().UnacknowledgedMessageCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DiscardedMessageCount"),
		ua.NewLocalizedText("DiscardedMessageCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "MonitoredItemCount"),
		ua.NewLocalizedText("MonitoredItemCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().MonitoredItemCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "DisabledMonitoredItemCount"),
		ua.NewLocalizedText("DisabledMonitoredItemCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().DisabledMonitoredItemCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "MonitoringQueueOverflowCount"),
		ua.NewLocalizedText("MonitoringQueueOverflowCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().MonitoringQueueOverflowCount, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "NextSequenceNumber"),
		ua.NewLocalizedText("NextSequenceNumber", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(s.diagnosticsSnapshot().NextSequenceNumber, 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)
	n = NewVariableNode(
		srv,
		ua.NewNodeIDGUID(1, uuid.New()),
		ua.NewQualifiedName(0, "EventQueueOverFlowCount"),
		ua.NewLocalizedText("EventQueueOverFlowCount", ""),
		ua.NewLocalizedText("", ""),
		nil,
		[]ua.Reference{
			ua.NewReference(ua.ReferenceTypeIDHasTypeDefinition, false, ua.NewExpandedNodeID(ua.VariableTypeIDBaseDataVariableType)),
			ua.NewReference(ua.ReferenceTypeIDHasComponent, true, ua.NewExpandedNodeID(subscriptionDiagnosticsVariable.NodeID())),
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
		return ua.NewDataValue(uint32(0), 0, time.Now(), 0, time.Now(), 0)
	})
	nodes = append(nodes, n)

	err := nm.AddNodes(nodes...)
	if err != nil {
		log.Printf("Error adding session diagnostics objects.\n")
	}
}

func (m *SubscriptionManager) removeDiagnosticsNode(s *Subscription) {
	nm := m.server.NamespaceManager()
	if n, ok := nm.FindNode(s.diagnosticsNodeId); ok {
		nm.DeleteNode(n, true)
	}
}
