package opc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

// The catalog integration tests use an ephemeral loopback OPC fixture and do
// not require a running Rust gateway.
func newLowerCatalogFixture(t *testing.T) (*UpstreamServer, string, domain.SourceTopology) {
	t.Helper()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "opc.tcp://" + reservation.Addr().String()
	_ = reservation.Close()
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: endpoint, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
	}, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	topology, nodes := topologyFixture()
	group := domain.Group{ID: "fallback"}
	source := domain.Source{ID: "lower", GroupID: group.ID}
	union := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{group.ID: {}}}
	for _, node := range nodes {
		union.Groups[group.ID][node.Key.Identifier] = node
	}
	if err := server.ReconcileCatalog(union, []domain.Group{group}, []domain.Source{source}); err != nil {
		t.Fatal(err)
	}
	return server, endpoint, topology
}

func serveLowerCatalogFixture(t *testing.T, server *UpstreamServer, endpoint string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("lower catalog OPC fixture did not stop")
		}
	})
	client := connectAnonymousClient(t, endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLowerLegacyBrowseFailureIsNotAnEmptyReadyCatalog(t *testing.T) {
	server, endpoint, _ := newLowerCatalogFixture(t)
	for _, name := range []string{"Devices", "Methods"} {
		node, ok := server.server.NamespaceManager().FindNode(awu.NewNodeIDString(2, name))
		if !ok {
			t.Fatalf("fixture node %s is missing", name)
		}
		if err := server.server.NamespaceManager().DeleteNode(node, true); err != nil {
			t.Fatal(err)
		}
	}
	serveLowerCatalogFixture(t, server, endpoint)
	client := connectAnonymousClient(t, endpoint)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	namespaces := namespaceIndex(client.Namespaces())
	if _, ok := namespaces[TopologyNamespaceURI]; ok {
		t.Fatal("fixture unexpectedly advertises topology; this test covers the legacy path")
	}
	response, err := client.Browse(ctx, &gu.BrowseRequest{NodesToBrowse: []*gu.BrowseDescription{{
		NodeID:          gu.NewStringNodeID(namespaces[BusinessNamespaceURI], "Devices"),
		BrowseDirection: gu.BrowseDirectionForward, ReferenceTypeID: gu.NewNumericNodeID(0, id.HasComponent),
		IncludeSubtypes: true, NodeClassMask: uint32(gu.NodeClassVariable), ResultMask: uint32(gu.BrowseResultMaskAll),
	}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0].StatusCode != gu.StatusBadNodeIDUnknown {
		t.Fatalf("fixture must return an operation-level BadNodeIDUnknown: response=%#v err=%v", response, err)
	}
	catalog, observed, err := discoverCatalog(ctx, client, domain.Source{ID: "lower", GroupID: "fallback"}, namespaces)
	if err == nil {
		t.Fatalf("a failed Browse was accepted as a valid catalog: state=%s nodes=%d monitored=%d; do not retire cached points on a Browse error", catalog.State, len(catalog.Nodes), len(observed))
	}
}

func TestLowerLegacyEmptyFoldersAreAValidCatalog(t *testing.T) {
	server, endpoint, _ := newLowerCatalogFixture(t)
	_, nodes := topologyFixture()
	for _, catalogNode := range nodes {
		node, ok := server.server.NamespaceManager().FindNode(awu.NewNodeIDString(2, catalogNode.Key.Identifier))
		if !ok {
			t.Fatalf("fixture node %s is missing", catalogNode.Key.Identifier)
		}
		if err := server.server.NamespaceManager().DeleteNode(node, true); err != nil {
			t.Fatal(err)
		}
	}
	serveLowerCatalogFixture(t, server, endpoint)
	client := connectAnonymousClient(t, endpoint)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	catalog, observed, err := discoverCatalog(ctx, client, domain.Source{ID: "lower", GroupID: "fallback"}, namespaceIndex(client.Namespaces()))
	if err != nil || catalog.State != domain.CatalogReady || len(catalog.Nodes) != 0 || len(observed) != 0 || catalog.Topology != nil {
		t.Fatalf("legitimate empty legacy catalog rejected: state=%s nodes=%d monitored=%d err=%v", catalog.State, len(catalog.Nodes), len(observed), err)
	}
}

type catalogRecoverySink struct {
	mu             sync.Mutex
	catalogSeen    bool
	postCatalog    []LowerStatus
	firstConflict  chan struct{}
	firstDataState chan struct{}
	active         chan struct{}
	conflictOnce   sync.Once
	dataOnce       sync.Once
	activeOnce     sync.Once
}

func (s *catalogRecoverySink) OnCatalog(domain.SourceCatalog) {
	s.mu.Lock()
	s.catalogSeen = true
	s.mu.Unlock()
}

func (s *catalogRecoverySink) OnSample(string, domain.NodeKey, domain.Sample) {}

func (s *catalogRecoverySink) OnLowerStatus(status LowerStatus) {
	s.mu.Lock()
	seen := s.catalogSeen
	if seen {
		s.postCatalog = append(s.postCatalog, status)
	}
	s.mu.Unlock()
	if !seen && status.Catalog == domain.CatalogConflict {
		s.conflictOnce.Do(func() { close(s.firstConflict) })
	}
	if seen && !status.LastDataAt.IsZero() {
		s.dataOnce.Do(func() { close(s.firstDataState) })
	}
	if seen && status.Subscription == SubscriptionActive {
		s.activeOnce.Do(func() { close(s.active) })
	}
}

func TestLowerCatalogRecoveryDoesNotReemitConflictFromInitialSamples(t *testing.T) {
	server, endpoint, topology := newLowerCatalogFixture(t)
	sink := &catalogRecoverySink{
		firstConflict: make(chan struct{}), firstDataState: make(chan struct{}), active: make(chan struct{}),
	}
	stopFixture := make(chan struct{})
	t.Cleanup(func() { close(stopFixture) })
	var delayOnce, waitOnce sync.Once
	for _, name := range []string{"A", "B", "CALC_A"} {
		node, ok := server.server.NamespaceManager().FindNode(awu.NewNodeIDString(2, name))
		if !ok {
			t.Fatalf("fixture variable %s is missing", name)
		}
		variable, ok := node.(*awserver.VariableNode)
		if !ok {
			t.Fatalf("fixture node %s is not a variable", name)
		}
		name := name
		variable.SetReadValueHandler(func(_ *awserver.Session, _ awu.ReadValueID) awu.DataValue {
			if name == "B" {
				// Accumulate an A notification while AddMonitorItems holds the
				// monitor's write lock. At the next batch boundary the waiting
				// notification reader is released before the next batch writer.
				delayOnce.Do(func() {
					select {
					case <-time.After(700 * time.Millisecond):
					case <-stopFixture:
					}
				})
			}
			if name == "CALC_A" {
				waitOnce.Do(func() {
					select {
					case <-sink.firstDataState:
					case <-time.After(3 * time.Second):
					case <-stopFixture:
					}
				})
			}
			var value any = uint32(11)
			if name == "CALC_A" {
				value = float64(11)
			}
			now := time.Now().UTC()
			return awu.NewDataValue(value, awu.Good, now, 0, now, 0)
		})
	}
	topologyNS := server.server.NamespaceManager().Add(TopologyNamespaceURI)
	now := time.Now().UTC()
	manifest := awserver.NewVariableNode(server.server, awu.NewNodeIDString(topologyNS, "Topology/Manifest"),
		awu.NewQualifiedName(topologyNS, "Manifest"), awu.NewLocalizedText("Manifest", ""), awu.NewLocalizedText("", ""), nil, nil,
		awu.NewDataValue(`{"schema_version":99}`, awu.Good, now, 0, now, 0),
		awu.DataTypeIDString, awu.ValueRankScalar, nil, awu.AccessLevelsCurrentRead, 0, false, nil)
	if err := server.server.NamespaceManager().AddNode(manifest); err != nil {
		t.Fatal(err)
	}
	serveLowerCatalogFixture(t, server, endpoint)
	supervisor, err := NewLowerSupervisor(LowerConfig{
		Source:           domain.Source{ID: "lower", GroupID: "fallback", Endpoint: endpoint, Enabled: true},
		MonitorBatchSize: 1, PublishingInterval: 100 * time.Millisecond, SamplingInterval: 100 * time.Millisecond,
		OperationTimeout: 5 * time.Second, MinReconnectBackoff: 100 * time.Millisecond, MaxReconnectBackoff: 100 * time.Millisecond,
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("lower supervisor did not stop")
		}
	})
	select {
	case <-sink.firstConflict:
	case <-ctx.Done():
		t.Fatal("invalid manifest did not enter CatalogConflict")
	}
	now = time.Now().UTC()
	manifest.SetValue(awu.NewDataValue(topologyJSON(t, topology), awu.Good, now, 0, now, 0))
	select {
	case <-sink.active:
	case <-ctx.Done():
		t.Fatalf("valid manifest did not re-subscribe: %#v", supervisor.Status())
	}
	sink.mu.Lock()
	statuses := append([]LowerStatus(nil), sink.postCatalog...)
	sink.mu.Unlock()
	var sawInitialData bool
	for _, status := range statuses {
		if !status.LastDataAt.IsZero() && status.Subscription == SubscriptionCreating {
			sawInitialData = true
			if status.Catalog == domain.CatalogConflict {
				t.Errorf("successful OnCatalog was followed by stale CatalogConflict from initial data: epoch=%d subscription=%s last_data=%s", status.Epoch, status.Subscription, status.LastDataAt.Format(time.RFC3339Nano))
			}
		}
	}
	if !sawInitialData {
		t.Fatalf("fixture did not expose the subscription-creation window; statuses=%#v", statuses)
	}
}
