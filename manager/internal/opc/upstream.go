package opc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
)

type UpstreamConfig struct {
	EndpointURL        string
	ListenAddress      string
	CertificateFile    string
	PrivateKeyFile     string
	Username           string
	Password           string
	CommandTimeout     time.Duration
	MaxSessions        uint32
	MaxSubscriptions   uint32
	MaxWorkerThreads   int
	InsecureSkipVerify bool
}

type businessEntry struct {
	spec    domain.CatalogNode
	node    awserver.Node
	retired atomic.Bool
}

// The discovery lists are deliberately served through stable holders. Every
// VariableNode for one list family loads the same immutable snapshot, so a hot
// catalog update cannot publish a new fingerprint while its count or array is
// still backed by the previous catalog.
type groupPointSnapshot struct {
	revision    uint64
	fingerprint string
	identifiers []string
}

type groupPointSnapshotStore struct {
	current atomic.Pointer[groupPointSnapshot]
}

type memberCoverageSnapshot struct {
	revision         uint64
	expectedPoints   uint32
	providedPoints   uint32
	missingPoints    uint32
	completeCoverage bool
	coverageKnown    bool
	fingerprint      string
	providedIDs      []string
	missingIDs       []string
}

type memberCoverageSnapshotStore struct {
	current atomic.Pointer[memberCoverageSnapshot]
}

type memberCoverageSnapshotKey struct {
	groupID  string
	sourceID string
}

const (
	forceSessionRefreshBatchSize = 500
	managementMaxArrayLength     = 131_072
)

type UpstreamServer struct {
	cfg     UpstreamConfig
	backend UpstreamBackend
	server  *awserver.Server

	mu                      sync.RWMutex
	business                map[string]*businessEntry
	management              map[string]awserver.Node
	managementGroups        map[string]awserver.Node
	managementSources       map[string]awserver.Node
	memberTreeHashes        map[string]string
	memberTreeNodeIDs       map[string]map[string]awu.NodeID
	memberTreeRouteIDs      map[string][]awu.NodeID
	groupCatalogRevisions   map[string]*atomic.Uint64
	groupPointSnapshots     map[string]*groupPointSnapshotStore
	memberCoverageSnapshots map[memberCoverageSnapshotKey]*memberCoverageSnapshotStore
	groupNodeIDs            map[string][]awu.NodeID
	businessNS              uint16
	managementNS            uint16
	devicesID               awu.NodeID
	methodsID               awu.NodeID
	managerRootID           awu.NodeID
	groupsRootID            awu.NodeID
	sourcesRootID           awu.NodeID
}

type upstreamCatalogPlan struct {
	desired      map[string]domain.CatalogNode
	groupForNode map[string]string
}

func NewUpstreamServer(cfg UpstreamConfig, backend UpstreamBackend) (*UpstreamServer, error) {
	if backend == nil {
		return nil, errors.New("upstream backend is required")
	}
	if strings.TrimSpace(cfg.EndpointURL) == "" {
		return nil, errors.New("upstream endpoint URL is required")
	}
	if cfg.CertificateFile == "" || cfg.PrivateKeyFile == "" {
		return nil, errors.New("upstream certificate and private key are required")
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = 5 * time.Second
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 10
	}
	if cfg.MaxWorkerThreads == 0 {
		cfg.MaxWorkerThreads = 16
	}
	if cfg.Username == "" {
		cfg.Username = "user1"
	}
	if cfg.Password == "" {
		cfg.Password = "pwd"
	}

	application := awu.ApplicationDescription{
		ApplicationURI:  ApplicationNamespaceURI,
		ProductURI:      "urn:RcdManager",
		ApplicationName: awu.NewLocalizedText("RCD OPC Manager", "zh-CN"),
		ApplicationType: awu.ApplicationTypeServer,
		DiscoveryURLs:   []string{cfg.EndpointURL},
	}
	serverCapabilities := awu.NewServerCapabilities()
	serverCapabilities.MaxArrayLength = managementMaxArrayLength
	adapter := &UpstreamServer{
		cfg: cfg, backend: backend,
		business:                make(map[string]*businessEntry),
		management:              make(map[string]awserver.Node),
		managementGroups:        make(map[string]awserver.Node),
		managementSources:       make(map[string]awserver.Node),
		memberTreeHashes:        make(map[string]string),
		memberTreeNodeIDs:       make(map[string]map[string]awu.NodeID),
		memberTreeRouteIDs:      make(map[string][]awu.NodeID),
		groupCatalogRevisions:   make(map[string]*atomic.Uint64),
		groupPointSnapshots:     make(map[string]*groupPointSnapshotStore),
		memberCoverageSnapshots: make(map[memberCoverageSnapshotKey]*memberCoverageSnapshotStore),
		groupNodeIDs:            make(map[string][]awu.NodeID),
	}
	opts := []awserver.Option{
		awserver.WithServerCapabilities(serverCapabilities),
		awserver.WithMaxSessionCount(cfg.MaxSessions),
		awserver.WithMaxSubscriptionCount(cfg.MaxSubscriptions),
		awserver.WithMaxWorkerThreads(cfg.MaxWorkerThreads),
		awserver.WithAnonymousIdentity(true),
		awserver.WithSecurityPolicyNone(true),
		awserver.WithRolePermissions(controlRolePermissions()),
		awserver.WithAuthenticateUserNameIdentityFunc(func(identity awu.UserNameIdentity, _, _ string) error {
			if identity.UserName != cfg.Username || identity.Password != cfg.Password {
				return awu.BadUserAccessDenied
			}
			return nil
		}),
		awserver.WithEndpointProfiles(
			awserver.EndpointProfile{
				SecurityPolicyURI: awu.SecurityPolicyURINone,
				SecurityMode:      awu.MessageSecurityModeNone,
				AllowAnonymous:    true,
			},
			awserver.EndpointProfile{
				SecurityPolicyURI: awu.SecurityPolicyURIBasic256Sha256,
				SecurityMode:      awu.MessageSecurityModeSignAndEncrypt,
				AllowAnonymous:    true,
				AllowUserName:     true,
			},
		),
		awserver.WithSessionScopedWriteHandlers(true),
		awserver.WithSessionEventHandler(adapter.handleSessionEvent),
	}
	if strings.TrimSpace(cfg.ListenAddress) != "" {
		opts = append(opts, awserver.WithListenAddress(cfg.ListenAddress))
	}
	if cfg.InsecureSkipVerify {
		opts = append(opts, awserver.WithInsecureSkipVerify())
	}
	srv, err := awserver.New(application, cfg.CertificateFile, cfg.PrivateKeyFile, cfg.EndpointURL, opts...)
	if err != nil {
		return nil, err
	}
	adapter.server = srv
	adapter.businessNS = srv.NamespaceManager().Add(BusinessNamespaceURI)
	adapter.managementNS = srv.NamespaceManager().Add(ManagementNamespaceURI)
	if adapter.businessNS != 2 || adapter.managementNS != 3 {
		return nil, fmt.Errorf("unexpected namespace indexes: business=%d management=%d", adapter.businessNS, adapter.managementNS)
	}
	if err := adapter.addRoots(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func controlRolePermissions() []awu.RolePermissionType {
	permissions := append([]awu.RolePermissionType(nil), awserver.DefaultRolePermissions...)
	control := awu.PermissionTypeBrowse | awu.PermissionTypeRead | awu.PermissionTypeWrite |
		awu.PermissionTypeReadHistory | awu.PermissionTypeReceiveEvents | awu.PermissionTypeCall
	for i := range permissions {
		if permissions[i].RoleID == awu.ObjectIDWellKnownRoleAnonymous || permissions[i].RoleID == awu.ObjectIDWellKnownRoleAuthenticatedUser {
			permissions[i].Permissions = control
		}
	}
	return permissions
}

func (s *UpstreamServer) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *UpstreamServer) Close() error {
	return s.server.Close()
}

func (s *UpstreamServer) NamespaceURIs() []string {
	return append([]string(nil), s.server.NamespaceUris()...)
}

// ReconcileCatalog adds live nodes and marks removed nodes BadOutOfService.
// Marked nodes are physically removed only when the process restarts.
func (s *UpstreamServer) ReconcileCatalog(catalog domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
	plan, err := s.validateCatalog(catalog, groups, sources)
	if err != nil {
		return err
	}
	desired, groupForNode := plan.desired, plan.groupForNode

	s.mu.Lock()
	defer s.mu.Unlock()
	// Recheck hot shapes under the mutation lock in case another reconciliation
	// completed after the dry-run.
	for identifier, spec := range desired {
		if existing := s.business[identifier]; existing != nil &&
			(existing.spec.Class != spec.Class || existing.spec.DataType != spec.DataType || existing.spec.ValueRank != spec.ValueRank) {
			return fmt.Errorf("cannot hot-change shape of node %q", identifier)
		}
	}
	for identifier, entry := range s.business {
		_, exists := desired[identifier]
		entry.retired.Store(!exists)
	}
	for identifier, spec := range desired {
		if existing := s.business[identifier]; existing != nil {
			existing.retired.Store(spec.Retired)
			continue
		}
		entry, err := s.addBusinessNodeLocked(spec)
		if err != nil {
			return err
		}
		entry.retired.Store(spec.Retired)
		s.business[identifier] = entry
	}

	if err := s.reconcileManagementLocked(catalog, groups, sources); err != nil {
		return err
	}
	s.groupNodeIDs = make(map[string][]awu.NodeID, len(catalog.Groups))
	for identifier, groupID := range groupForNode {
		if entry := s.business[identifier]; entry != nil && entry.spec.Class == domain.NodeClassVariable {
			s.groupNodeIDs[groupID] = append(s.groupNodeIDs[groupID], entry.node.NodeID())
		}
	}
	for _, group := range groups {
		for _, name := range []string{"SelectedSourceId", "SelectionRevision", "Send"} {
			s.groupNodeIDs[group.ID] = append(s.groupNodeIDs[group.ID], awu.NewNodeIDString(s.managementNS, "Groups/"+group.ID+"/"+name))
		}
	}
	for _, group := range groups {
		s.groupNodeIDs[group.ID] = append(s.groupNodeIDs[group.ID], s.memberTreeRouteIDs[group.ID]...)
	}
	// A legacy command variable is shared by the devices on its physical port.
	// Its ACK view can become ambiguous when any of those device routes changes.
	commandsByPort := make(map[string][]awu.NodeID)
	for identifier, spec := range desired {
		if spec.Kind == "command" && spec.Class == domain.NodeClassVariable {
			commandsByPort[spec.PortID] = append(commandsByPort[spec.PortID], s.business[identifier].node.NodeID())
		}
	}
	for _, source := range sources {
		for _, binding := range source.GroupBindings {
			s.groupNodeIDs[binding.GroupID] = append(s.groupNodeIDs[binding.GroupID], commandsByPort[binding.PortID]...)
		}
	}
	for groupID, nodes := range s.groupNodeIDs {
		unique := make(map[awu.NodeID]struct{}, len(nodes))
		filtered := nodes[:0]
		for _, node := range nodes {
			if _, exists := unique[node]; !exists {
				unique[node] = struct{}{}
				filtered = append(filtered, node)
			}
		}
		sort.Slice(filtered, func(i, j int) bool { return fmt.Sprint(filtered[i]) < fmt.Sprint(filtered[j]) })
		s.groupNodeIDs[groupID] = filtered
	}
	return nil
}

// ValidateCatalog performs every deterministic address-space check without
// changing nodes. Runtime calls this before committing a hot catalog/config
// version to SQLite or its in-memory route table.
func (s *UpstreamServer) ValidateCatalog(catalog domain.CatalogUnion, groups []domain.Group, sources []domain.Source) error {
	_, err := s.validateCatalog(catalog, groups, sources)
	return err
}

func (s *UpstreamServer) validateCatalog(catalog domain.CatalogUnion, groups []domain.Group, sources []domain.Source) (upstreamCatalogPlan, error) {
	desired := make(map[string]domain.CatalogNode)
	groupForNode := make(map[string]string)
	reserved := map[string]struct{}{"Devices": {}, "Methods": {}}
	for groupID, nodes := range catalog.Groups {
		pointCount := 0
		for identifier, node := range nodes {
			if identifier == "" || node.Key.Identifier != identifier {
				return upstreamCatalogPlan{}, fmt.Errorf("catalog node identifier mismatch for %q", identifier)
			}
			if node.Key.NamespaceURI != BusinessNamespaceURI {
				return upstreamCatalogPlan{}, fmt.Errorf("node %q uses unsupported namespace %q", identifier, node.Key.NamespaceURI)
			}
			if _, conflict := reserved[identifier]; conflict {
				return upstreamCatalogPlan{}, fmt.Errorf("node %q conflicts with a reserved address-space node", identifier)
			}
			if existingGroup, exists := groupForNode[identifier]; exists && existingGroup != groupID {
				return upstreamCatalogPlan{}, fmt.Errorf("node %q appears in groups %q and %q", identifier, existingGroup, groupID)
			}
			desired[identifier] = node
			groupForNode[identifier] = groupID
			switch node.Class {
			case domain.NodeClassVariable:
				if _, err := awcullenDataType(node.DataType); err != nil {
					return upstreamCatalogPlan{}, fmt.Errorf("node %q: %w", identifier, err)
				}
			case domain.NodeClassMethod:
				if !validProxyMethod(node) {
					return upstreamCatalogPlan{}, fmt.Errorf("method %q does not match HexData String -> Result String", identifier)
				}
				reserved[identifier+"_Input"] = struct{}{}
				reserved[identifier+"_Output"] = struct{}{}
			default:
				return upstreamCatalogPlan{}, fmt.Errorf("unsupported node class %q", node.Class)
			}
			if memberCoveragePoint(node) {
				pointCount++
			}
		}
		if pointCount > managementMaxArrayLength {
			return upstreamCatalogPlan{}, fmt.Errorf("group %q has %d data points; maximum is %d", groupID, pointCount, managementMaxArrayLength)
		}
	}
	for identifier := range desired {
		if _, conflict := reserved[identifier]; conflict {
			return upstreamCatalogPlan{}, fmt.Errorf("node %q conflicts with a method argument node", identifier)
		}
	}
	groupIDs := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if group.ID == "" {
			return upstreamCatalogPlan{}, errors.New("management group id is required")
		}
		if _, exists := groupIDs[group.ID]; exists {
			return upstreamCatalogPlan{}, fmt.Errorf("duplicate management group %q", group.ID)
		}
		groupIDs[group.ID] = struct{}{}
	}
	sourceIDs := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if _, exists := groupIDs[source.GroupID]; !exists {
			return upstreamCatalogPlan{}, fmt.Errorf("source %q references missing group %q", source.ID, source.GroupID)
		}
		for _, binding := range source.GroupBindings {
			if _, exists := groupIDs[binding.GroupID]; !exists {
				return upstreamCatalogPlan{}, fmt.Errorf("source %q device binding references missing group %q", source.ID, binding.GroupID)
			}
		}
		if _, exists := sourceIDs[source.ID]; exists {
			return upstreamCatalogPlan{}, fmt.Errorf("duplicate management source %q", source.ID)
		}
		sourceIDs[source.ID] = struct{}{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for identifier, spec := range desired {
		if existing := s.business[identifier]; existing != nil &&
			(existing.spec.Class != spec.Class || existing.spec.DataType != spec.DataType || existing.spec.ValueRank != spec.ValueRank) {
			return upstreamCatalogPlan{}, fmt.Errorf("cannot hot-change shape of node %q", identifier)
		}
	}
	return upstreamCatalogPlan{desired: desired, groupForNode: groupForNode}, nil
}

func (s *UpstreamServer) ForceSessionRefresh(sessionID, groupID string) int {
	s.mu.RLock()
	nodes := append([]awu.NodeID(nil), s.groupNodeIDs[groupID]...)
	s.mu.RUnlock()
	id := awu.ParseNodeID(sessionID)
	if id == nil {
		return 0
	}
	return forceSessionRefreshBatches(nodes, func(batch []awu.NodeID) int {
		return s.server.ForceResampleSessionNodes(id, batch)
	})
}

func forceSessionRefreshBatches(nodes []awu.NodeID, refresh func([]awu.NodeID) int) int {
	total := 0
	for start := 0; start < len(nodes); start += forceSessionRefreshBatchSize {
		end := min(start+forceSessionRefreshBatchSize, len(nodes))
		total += refresh(nodes[start:end])
	}
	return total
}

func (s *UpstreamServer) Sessions() []awserver.SessionSnapshot {
	return s.server.Sessions()
}

func (s *UpstreamServer) SubscriptionCount() int {
	return s.server.SubscriptionManager().Len()
}

func (s *UpstreamServer) handleSessionEvent(event awserver.SessionEvent) {
	sessionID := fmt.Sprint(event.Session.SessionID)
	switch event.Kind {
	case awserver.SessionActivated, awserver.SessionAttached:
		_ = s.backend.OpenSession(sessionID)
	case awserver.SessionDetached:
		_ = s.backend.TouchSession(sessionID)
	case awserver.SessionClosed:
		s.backend.CloseSession(sessionID)
	}
}

func (s *UpstreamServer) commandContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cfg.CommandTimeout)
}
