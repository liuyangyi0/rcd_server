package opc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/monitor"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

type observedNode struct {
	key      domain.NodeKey
	dataType domain.DataType
	command  bool
}

type observedVersion struct {
	epoch     uint64
	timestamp time.Time
}

type LowerSupervisor struct {
	cfg  LowerConfig
	sink LowerSink

	mu                   sync.RWMutex
	runContext           context.Context
	client               *gopcua.Client
	namespaces           map[string]uint16
	nodes                map[string]observedNode
	versions             map[string]observedVersion
	status               LowerStatus
	running              atomic.Bool
	epoch                atomic.Uint64
	commandMu            sync.Mutex
	stationRevisionEpoch uint64
	stationRevision      string
	operationCalls       atomic.Int32

	lastStatusEmit atomic.Int64
	rng            *rand.Rand
}

func NewLowerSupervisor(cfg LowerConfig, sink LowerSink) (*LowerSupervisor, error) {
	if strings.TrimSpace(cfg.Source.ID) == "" {
		return nil, errors.New("source id is required")
	}
	if strings.TrimSpace(cfg.Source.GroupID) == "" {
		return nil, errors.New("source group id is required")
	}
	if strings.TrimSpace(cfg.Source.Endpoint) == "" {
		return nil, errors.New("source endpoint is required")
	}
	if cfg.MonitorBatchSize == 0 {
		cfg.MonitorBatchSize = DefaultMonitorBatchSize
	}
	if cfg.MonitorBatchSize < 1 || cfg.MonitorBatchSize > DefaultMonitorBatchSize {
		return nil, fmt.Errorf("monitor batch size must be in [1,%d]", DefaultMonitorBatchSize)
	}
	if cfg.PublishingInterval <= 0 {
		cfg.PublishingInterval = 50 * time.Millisecond
	}
	if cfg.SamplingInterval <= 0 {
		cfg.SamplingInterval = 50 * time.Millisecond
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = 5 * time.Second
	}
	if cfg.SessionProbeInterval <= 0 {
		cfg.SessionProbeInterval = DefaultSessionProbeInterval
	}
	if cfg.SessionProbeInterval >= time.Minute {
		return nil, errors.New("session probe interval must be shorter than the Rust lower 60 second session timeout")
	}
	if cfg.MinReconnectBackoff <= 0 {
		cfg.MinReconnectBackoff = time.Second
	}
	if cfg.MaxReconnectBackoff <= 0 {
		cfg.MaxReconnectBackoff = 30 * time.Second
	}
	if cfg.MaxReconnectBackoff < cfg.MinReconnectBackoff {
		return nil, errors.New("maximum reconnect backoff is smaller than minimum")
	}
	if strings.TrimSpace(cfg.Source.SecurityMode) == "" {
		cfg.Source.SecurityMode = "None"
	}
	if strings.TrimSpace(cfg.Source.SecurityPolicy) == "" {
		cfg.Source.SecurityPolicy = "None"
	}
	mode := gu.MessageSecurityModeFromString(cfg.Source.SecurityMode)
	if mode == gu.MessageSecurityModeInvalid {
		return nil, fmt.Errorf("invalid security mode %q", cfg.Source.SecurityMode)
	}
	if mode != gu.MessageSecurityModeNone && (cfg.CertificateFile == "" || cfg.PrivateKeyFile == "") {
		return nil, errors.New("secure lower endpoint requires client certificate and private key")
	}
	if sink == nil {
		sink = NopLowerSink{}
	}
	now := time.Now().UTC()
	return &LowerSupervisor{
		cfg:        cfg,
		sink:       sink,
		namespaces: make(map[string]uint16),
		nodes:      make(map[string]observedNode),
		versions:   make(map[string]observedVersion),
		status: LowerStatus{
			SourceID: cfg.Source.ID, Connection: ConnectionDisconnected,
			Session: SessionInactive, Subscription: SubscriptionInactive,
			Catalog: domain.CatalogDiscovering, UpdatedAt: now,
		},
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

func (s *LowerSupervisor) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("lower supervisor is already running")
	}
	s.mu.Lock()
	s.runContext = ctx
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.runContext = nil
		s.mu.Unlock()
		s.running.Store(false)
	}()

	backoff := s.cfg.MinReconnectBackoff
	for {
		if err := ctx.Err(); err != nil {
			s.setDisconnected("")
			return nil
		}
		epoch := s.epoch.Add(1)
		s.updateStatus(func(st *LowerStatus) {
			st.Connection = ConnectionConnecting
			st.Session = SessionInactive
			st.Subscription = SubscriptionInactive
			st.Epoch = epoch
			st.LastError = ""
		})

		err := s.runConnection(ctx, epoch)
		if ctx.Err() != nil {
			s.setDisconnected("")
			return nil
		}
		wasActive := s.Status().Subscription == SubscriptionActive
		if wasActive {
			backoff = s.cfg.MinReconnectBackoff
		}
		s.mu.Lock()
		s.status.ReconnectCount++
		s.mu.Unlock()
		s.setDisconnected(errorString(err))

		wait := s.jitter(backoff)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
		if backoff < s.cfg.MaxReconnectBackoff {
			backoff *= 2
			if backoff > s.cfg.MaxReconnectBackoff {
				backoff = s.cfg.MaxReconnectBackoff
			}
		}
	}
}

func (s *LowerSupervisor) runConnection(parent context.Context, epoch uint64) error {
	connectCtx, cancelConnect := context.WithTimeout(parent, s.cfg.ConnectTimeout)
	defer cancelConnect()

	stateCh := make(chan gopcua.ConnState, 8)
	client, err := s.newClient(connectCtx, stateCh)
	if err != nil {
		return err
	}
	if err := client.Connect(connectCtx); err != nil {
		return fmt.Errorf("connect source %s: %w", s.cfg.Source.ID, err)
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), s.cfg.OperationTimeout)
		_ = client.Close(closeCtx)
		cancelClose()
		s.mu.Lock()
		if s.client == client {
			s.client = nil
		}
		s.mu.Unlock()
	}()

	namespaces := namespaceIndex(client.Namespaces())
	s.updateStatus(func(st *LowerStatus) {
		st.Connection = ConnectionConnected
		st.Session = SessionActive
		st.Subscription = SubscriptionCreating
	})

	discoveryCtx, cancelDiscovery := context.WithTimeout(parent, s.cfg.OperationTimeout*4)
	catalog, nodes, err := discoverCatalog(discoveryCtx, client, s.cfg.Source, namespaces)
	cancelDiscovery()
	if err != nil {
		if errors.Is(err, ErrInvalidTopology) {
			s.updateStatus(func(st *LowerStatus) {
				st.Catalog = domain.CatalogConflict
				st.LastError = err.Error()
			})
		}
		return fmt.Errorf("discover source %s: %w", s.cfg.Source.ID, err)
	}
	// Monitored-item callbacks can run between creation batches. Clear a
	// previous topology conflict before publishing the recovered catalog so
	// initial data cannot report that obsolete conflict back to the runtime.
	s.mu.Lock()
	s.status.Catalog = domain.CatalogReady
	s.status.LastError = ""
	s.mu.Unlock()
	s.sink.OnCatalog(catalog)
	stationRevisionNode := s.discoverStationVersionRevisionNode(parent, client, namespaces)

	runCtx, cancelRun := context.WithCancel(parent)
	defer cancelRun()
	monitorClient, err := monitor.NewNodeMonitor(client)
	if err != nil {
		return fmt.Errorf("create node monitor: %w", err)
	}
	monitorErr := make(chan error, 1)
	monitorClient.SetErrorHandler(func(_ *gopcua.Client, _ *monitor.Subscription, err error) {
		select {
		case monitorErr <- err:
		default:
		}
	})

	subs, err := s.subscribeBatches(runCtx, monitorClient, nodes, stationRevisionNode, epoch, monitorErr)
	if err != nil {
		return err
	}
	defer func() {
		unsubscribeCtx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeout)
		defer cancel()
		for _, sub := range subs {
			_ = sub.Unsubscribe(unsubscribeCtx)
		}
	}()
	s.mu.Lock()
	s.client = client
	s.namespaces = namespaces
	s.nodes = nodes
	s.status.PointCount = len(nodes)
	s.status.SubscriptionCount = len(subs)
	s.status.Catalog = domain.CatalogReady
	s.status.Subscription = SubscriptionActive
	s.status.LastProbeAt = time.Now().UTC()
	s.status.UpdatedAt = time.Now().UTC()
	status := s.status
	s.mu.Unlock()
	s.sink.OnLowerStatus(status)
	probeTicker := time.NewTicker(s.cfg.SessionProbeInterval)
	defer probeTicker.Stop()

	for {
		select {
		case <-parent.Done():
			return nil
		case <-probeTicker.C:
			if err := s.probeSession(parent, client); err != nil {
				return fmt.Errorf("probe source %s session: %w", s.cfg.Source.ID, err)
			}
		case err := <-monitorErr:
			if err == nil {
				err = errors.New("downstream subscription stopped")
			}
			return fmt.Errorf("monitor source %s: %w", s.cfg.Source.ID, err)
		case state := <-stateCh:
			switch state {
			case gopcua.Disconnected, gopcua.Closed:
				return fmt.Errorf("source %s connection state %s", s.cfg.Source.ID, state)
			}
		}
	}
}

func (s *LowerSupervisor) probeSession(parent context.Context, client *gopcua.Client) error {
	probeCtx, cancel := context.WithTimeout(parent, s.cfg.OperationTimeout)
	defer cancel()
	response, err := client.Read(probeCtx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead: []*gu.ReadValueID{{
			NodeID: gu.NewNumericNodeID(0, id.Server_NamespaceArray), AttributeID: gu.AttributeIDValue,
		}},
	})
	if err != nil {
		return err
	}
	if response == nil || len(response.Results) != 1 {
		return errors.New("invalid NamespaceArray probe response")
	}
	if response.Results[0].Status != gu.StatusOK {
		return response.Results[0].Status
	}
	s.updateStatus(func(st *LowerStatus) { st.LastProbeAt = time.Now().UTC() })
	return nil
}

func (s *LowerSupervisor) newClient(ctx context.Context, stateCh chan<- gopcua.ConnState) (*gopcua.Client, error) {
	endpoints, err := gopcua.GetEndpoints(ctx, s.cfg.Source.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("get endpoints: %w", err)
	}
	return s.newClientFromEndpoints(endpoints, stateCh)
}

func (s *LowerSupervisor) newClientFromEndpoints(endpoints []*gu.EndpointDescription, stateCh chan<- gopcua.ConnState) (*gopcua.Client, error) {
	mode := gu.MessageSecurityModeFromString(s.cfg.Source.SecurityMode)
	ep, err := gopcua.SelectEndpoint(endpoints, s.cfg.Source.SecurityPolicy, mode)
	if err != nil {
		return nil, err
	}
	authType := gu.UserTokenTypeAnonymous
	if s.cfg.Source.Username != "" {
		authType = gu.UserTokenTypeUserName
	}
	opts := []gopcua.Option{
		gopcua.SecurityFromEndpoint(ep, authType),
		gopcua.AutoReconnect(false),
		gopcua.StateChangedCh(stateCh),
		gopcua.ApplicationName("RCD Manager Lower Connector"),
		gopcua.ApplicationURI("urn:RcdManager:lower-client:" + s.cfg.Source.ID),
		gopcua.SessionName("rcd-manager-" + s.cfg.Source.ID),
		gopcua.RequestTimeout(s.cfg.OperationTimeout),
	}
	if authType == gu.UserTokenTypeUserName {
		opts = append(opts, gopcua.AuthUsername(s.cfg.Source.Username, s.cfg.Password))
	} else {
		opts = append(opts, gopcua.AuthAnonymous())
	}
	if mode != gu.MessageSecurityModeNone {
		opts = append(opts, gopcua.CertificateFile(s.cfg.CertificateFile), gopcua.PrivateKeyFile(s.cfg.PrivateKeyFile))
	}
	// EndpointDescription.EndpointURL is server-advertised metadata and is
	// frequently stale after an RCD host is moved or its address changes. The
	// configured source endpoint is the operator-approved dial target; keep the
	// selected description for policy/certificate options, but do not redirect
	// the TCP connection to an unrelated advertised host.
	return gopcua.NewClient(lowerDialEndpoint(s.cfg.Source.Endpoint, ep.EndpointURL), opts...)
}

func lowerDialEndpoint(configured, advertised string) string {
	if endpoint := strings.TrimSpace(configured); endpoint != "" {
		return endpoint
	}
	return strings.TrimSpace(advertised)
}

// ProbeLower performs endpoint discovery, authentication, namespace resolution
// and catalog browsing without changing the live supervisor or persisted
// catalog. Operational UIs can therefore validate edits before applying them.
func ProbeLower(ctx context.Context, cfg LowerConfig) (LowerProbeResult, error) {
	started := time.Now()
	result := LowerProbeResult{TestedAt: started.UTC()}
	supervisor, err := NewLowerSupervisor(cfg, NopLowerSink{})
	if err != nil {
		return result, err
	}
	stage := func(name string, began time.Time, stageErr error) {
		item := LowerProbeStage{Name: name, OK: stageErr == nil, DurationMS: time.Since(began).Milliseconds()}
		if stageErr != nil {
			item.Message = stageErr.Error()
			result.Error = stageErr.Error()
		}
		result.Stages = append(result.Stages, item)
	}

	discoverStarted := time.Now()
	discoverCtx, cancelDiscover := context.WithTimeout(ctx, supervisor.cfg.ConnectTimeout)
	endpoints, discoverErr := gopcua.GetEndpoints(discoverCtx, supervisor.cfg.Source.Endpoint)
	cancelDiscover()
	stage("DiscoverEndpoints", discoverStarted, discoverErr)
	if discoverErr != nil {
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}
	result.EndpointCount = len(endpoints)

	connectStarted := time.Now()
	stateCh := make(chan gopcua.ConnState, 8)
	client, connectErr := supervisor.newClientFromEndpoints(endpoints, stateCh)
	if connectErr == nil {
		connectCtx, cancelConnect := context.WithTimeout(ctx, supervisor.cfg.ConnectTimeout)
		connectErr = client.Connect(connectCtx)
		cancelConnect()
	}
	stage("ConnectAndActivateSession", connectStarted, connectErr)
	if connectErr != nil {
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), supervisor.cfg.OperationTimeout)
		_ = client.Close(closeCtx)
		cancel()
	}()

	namespaceStarted := time.Now()
	namespaces := namespaceIndex(client.Namespaces())
	var namespaceErr error
	if len(namespaces) == 0 {
		namespaceErr = errors.New("server returned an empty NamespaceArray")
	}
	stage("ResolveNamespaceArray", namespaceStarted, namespaceErr)
	if namespaceErr != nil {
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}
	result.NamespaceCount = len(namespaces)

	catalogStarted := time.Now()
	catalogCtx, cancelCatalog := context.WithTimeout(ctx, supervisor.cfg.OperationTimeout*4)
	catalog, _, catalogErr := discoverCatalog(catalogCtx, client, supervisor.cfg.Source, namespaces)
	cancelCatalog()
	stage("BrowseAndValidateCatalog", catalogStarted, catalogErr)
	if catalogErr == nil {
		for _, node := range catalog.Nodes {
			if node.Class == domain.NodeClassMethod {
				result.MethodCount++
			} else {
				result.VariableCount++
			}
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result, nil
}

func (s *LowerSupervisor) subscribeBatches(
	ctx context.Context,
	m *monitor.NodeMonitor,
	nodes map[string]observedNode,
	stationRevisionNode string,
	epoch uint64,
	errCh chan<- error,
) ([]*monitor.Subscription, error) {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	params := &gopcua.SubscriptionParameters{
		Interval:                   s.cfg.PublishingInterval,
		LifetimeCount:              10000,
		MaxKeepAliveCount:          3000,
		MaxNotificationsPerPublish: 10000,
	}
	if len(ids) == 0 && stationRevisionNode == "" {
		return nil, nil
	}
	// A source owns one Subscription. MonitoredItems are still added in bounded
	// batches so large catalogs do not exceed per-request limits. Creating one
	// Subscription per batch exhausts the Rust server's per-Session limit once
	// a source grows beyond roughly 5000 points.
	sub, err := m.Subscribe(ctx, params, func(_ *monitor.Subscription, msg *monitor.DataChangeMessage) {
		if msg == nil || msg.Error != nil || msg.NodeID == nil || msg.DataValue == nil {
			if msg != nil && msg.Error != nil {
				select {
				case errCh <- msg.Error:
				default:
				}
			}
			return
		}
		if stationRevisionNode != "" && msg.NodeID.String() == stationRevisionNode {
			s.emitStationVersionRevision(msg.DataValue, epoch)
			return
		}
		observed, ok := nodes[msg.NodeID.String()]
		if !ok || s.epoch.Load() != epoch {
			return
		}
		s.emitMonitoredSample(msg.NodeID.String(), observed, msg.DataValue, epoch)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	for start := 0; start < len(ids); start += s.cfg.MonitorBatchSize {
		end := start + s.cfg.MonitorBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		requests := make([]monitor.Request, 0, end-start)
		for _, textID := range ids[start:end] {
			nodeID, parseErr := gu.ParseNodeID(textID)
			if parseErr != nil {
				_ = sub.Unsubscribe(ctx)
				return nil, fmt.Errorf("parse monitored NodeId %q: %w", textID, parseErr)
			}
			requests = append(requests, monitor.Request{
				NodeID:         nodeID,
				MonitoringMode: gu.MonitoringModeReporting,
				MonitoringParameters: &gu.MonitoringParameters{
					SamplingInterval: float64(s.cfg.SamplingInterval) / float64(time.Millisecond),
					QueueSize:        1,
					DiscardOldest:    true,
				},
			})
		}
		if _, err := sub.AddMonitorItems(ctx, requests...); err != nil {
			_ = sub.Unsubscribe(ctx)
			return nil, fmt.Errorf("monitor batch %d-%d: %w", start, end, err)
		}
	}
	if stationRevisionNode != "" {
		nodeID, parseErr := gu.ParseNodeID(stationRevisionNode)
		if parseErr != nil {
			_ = sub.Unsubscribe(ctx)
			return nil, fmt.Errorf("parse station version revision NodeId %q: %w", stationRevisionNode, parseErr)
		}
		_, _ = sub.AddMonitorItems(ctx, monitor.Request{
			NodeID: nodeID, MonitoringMode: gu.MonitoringModeReporting,
			MonitoringParameters: &gu.MonitoringParameters{SamplingInterval: 1000, QueueSize: 1, DiscardOldest: true},
		})
		// This management invalidation is optional. Any monitored-item rejection
		// must leave the business subscription healthy; the 60 s status poll is
		// the bounded fallback for old or resource-constrained lower servers.
	}
	return []*monitor.Subscription{sub}, nil
}

func (s *LowerSupervisor) discoverStationVersionRevisionNode(parent context.Context, client opcValueReader, namespaces map[string]uint16) string {
	ns, ok := namespaces[domain.OperationsNamespaceURI]
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.OperationTimeout)
	defer cancel()
	capabilityID := gu.NewStringNodeID(ns, "Operations/Capabilities")
	response, err := client.Read(ctx, &gu.ReadRequest{TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead: []*gu.ReadValueID{{NodeID: capabilityID, AttributeID: gu.AttributeIDValue}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil ||
		response.Results[0].Status != gu.StatusOK || response.Results[0].Value == nil {
		return ""
	}
	content, ok := response.Results[0].Value.Value().(string)
	if !ok || len(content) > 16384 {
		return ""
	}
	var capabilities domain.OperationsCapabilities
	if json.Unmarshal([]byte(content), &capabilities) != nil || capabilities.SchemaVersion != 1 || !capabilities.HasFeature("station_versions") {
		return ""
	}
	revisionID := gu.NewStringNodeID(ns, "Operations/StationVersionRevision")
	response, err = client.Read(ctx, &gu.ReadRequest{TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead: []*gu.ReadValueID{{NodeID: revisionID, AttributeID: gu.AttributeIDValue}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil ||
		response.Results[0].Status != gu.StatusOK || response.Results[0].Value == nil {
		return ""
	}
	revision, ok := response.Results[0].Value.Value().(string)
	if !ok || !domain.ValidStationRevision(revision) {
		return ""
	}
	return revisionID.String()
}

func (s *LowerSupervisor) emitStationVersionRevision(value *gu.DataValue, epoch uint64) {
	if value == nil || value.Value == nil || s.epoch.Load() != epoch {
		return
	}
	revision, ok := value.Value.Value().(string)
	if !ok || !domain.ValidStationRevision(revision) {
		return
	}
	s.mu.Lock()
	if s.stationRevisionEpoch == epoch && s.stationRevision == revision {
		s.mu.Unlock()
		return
	}
	s.stationRevisionEpoch, s.stationRevision = epoch, revision
	s.mu.Unlock()
	if sink, ok := s.sink.(LowerStationVersionSink); ok {
		sink.OnStationVersionRevision(s.cfg.Source.ID, revision, epoch)
	}
}

func (s *LowerSupervisor) Read(ctx context.Context, key domain.NodeKey) (domain.Sample, error) {
	client, nodeID, dataType, epoch, err := s.operationTarget(key)
	if err != nil {
		return domain.Sample{}, err
	}
	res, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnBoth,
		NodesToRead:        []*gu.ReadValueID{{NodeID: nodeID, AttributeID: gu.AttributeIDValue}},
	})
	if err != nil {
		return domain.Sample{}, err
	}
	if len(res.Results) != 1 {
		return domain.Sample{}, errors.New("read returned no result")
	}
	return sampleFromGopcua(res.Results[0], dataType, epoch), nil
}

func (s *LowerSupervisor) Write(ctx context.Context, key domain.NodeKey, value any) (domain.StatusCode, error) {
	return s.WriteAtEpoch(ctx, key, value, 0)
}

func (s *LowerSupervisor) WriteAtEpoch(ctx context.Context, key domain.NodeKey, value any, expectedEpoch uint64) (domain.StatusCode, error) {
	client, nodeID, dataType, epoch, err := s.operationTargetAtEpoch(key, expectedEpoch)
	if err != nil {
		return domain.StatusBadNoCommunication, err
	}
	baselineTimestamp, hasBaseline := readServerTimestamp(ctx, client, nodeID)
	variant, err := gu.NewVariant(value)
	if err != nil {
		return domain.StatusBadConfigurationError, err
	}
	res, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
		NodeID: nodeID, AttributeID: gu.AttributeIDValue,
		Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: variant},
	}}})
	if err != nil {
		return domain.StatusBadNoCommunication, err
	}
	if len(res.Results) != 1 {
		return domain.StatusBadNoCommunication, errors.New("write returned no result")
	}
	status := domain.StatusCode(res.Results[0])
	if status.IsGood() && hasBaseline && strings.HasSuffix(key.Identifier, "_Send") {
		s.startCommandAckProbe(client, nodeID, key, dataType, epoch, baselineTimestamp)
	}
	return status, nil
}

type opcValueReader interface {
	Read(context.Context, *gu.ReadRequest) (*gu.ReadResponse, error)
}

func readServerTimestamp(ctx context.Context, client opcValueReader, nodeID *gu.NodeID) (time.Time, bool) {
	response, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnBoth,
		NodesToRead:        []*gu.ReadValueID{{NodeID: nodeID, AttributeID: gu.AttributeIDValue}},
	})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil || response.Results[0].ServerTimestamp.IsZero() {
		return time.Time{}, false
	}
	return response.Results[0].ServerTimestamp, true
}

func (s *LowerSupervisor) startCommandAckProbe(client opcValueReader, nodeID *gu.NodeID, key domain.NodeKey, dataType domain.DataType, epoch uint64, baseline time.Time) {
	s.mu.RLock()
	parent := s.runContext
	s.mu.RUnlock()
	if parent == nil || parent.Err() != nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(parent, s.cfg.OperationTimeout)
	go func() {
		defer cancel()
		s.pollCommandAck(probeCtx, client, nodeID, key, dataType, epoch, baseline)
	}()
}

func (s *LowerSupervisor) pollCommandAck(ctx context.Context, client opcValueReader, nodeID *gu.NodeID, key domain.NodeKey, dataType domain.DataType, epoch uint64, baseline time.Time) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	lastTimestamp := baseline
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.epoch.Load() != epoch {
				return
			}
			response, err := client.Read(ctx, &gu.ReadRequest{
				TimestampsToReturn: gu.TimestampsToReturnBoth,
				NodesToRead:        []*gu.ReadValueID{{NodeID: nodeID, AttributeID: gu.AttributeIDValue}},
			})
			if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil {
				continue
			}
			value := response.Results[0]
			if value.ServerTimestamp.IsZero() || !value.ServerTimestamp.After(lastTimestamp) || value.Status != gu.StatusOK || value.Value == nil {
				continue
			}
			ack := strings.TrimSpace(fmt.Sprint(value.Value.Value()))
			if ack != "1" && ack != "2" && ack != "3" {
				continue
			}
			lastTimestamp = value.ServerTimestamp
			s.emitCommandSample(nodeID.String(), key, dataType, value, epoch)
			if ack == "1" || ack == "3" {
				return
			}
		}
	}
}

func (s *LowerSupervisor) emitObservedSample(_ string, key domain.NodeKey, dataType domain.DataType, value *gu.DataValue, epoch uint64) bool {
	if value == nil || s.epoch.Load() != epoch {
		return false
	}
	// A downstream wall clock is not a sequence number. Ordinary values and
	// quality transitions remain valid at equal or decreasing timestamps.
	s.sink.OnSample(s.cfg.Source.ID, key, sampleFromGopcua(value, dataType, epoch))
	s.touchLastData(epoch)
	return true
}

func (s *LowerSupervisor) emitMonitoredSample(nodeID string, node observedNode, value *gu.DataValue, epoch uint64) bool {
	if node.command {
		return s.emitCommandSample(nodeID, node.key, node.dataType, value, epoch)
	}
	return s.emitObservedSample(nodeID, node.key, node.dataType, value, epoch)
}

func (s *LowerSupervisor) emitCommandSample(nodeID string, key domain.NodeKey, dataType domain.DataType, value *gu.DataValue, epoch uint64) bool {
	// Only actual writable command variables merge subscription notifications
	// with ACK probe reads. Retain their timestamp guard so an old terminal ACK
	// cannot be replayed after a newer Waiting/terminal notification. Serialize
	// delivery as well as the version check to prevent the two paths overtaking
	// each other after accepting their versions.
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if value == nil || s.epoch.Load() != epoch {
		return false
	}
	timestamp := value.ServerTimestamp
	s.mu.Lock()
	if s.versions == nil {
		s.versions = make(map[string]observedVersion)
	}
	version := s.versions[nodeID]
	if version.epoch == epoch && !timestamp.IsZero() && !version.timestamp.IsZero() && !timestamp.After(version.timestamp) {
		s.mu.Unlock()
		return false
	}
	s.versions[nodeID] = observedVersion{epoch: epoch, timestamp: timestamp}
	s.mu.Unlock()
	return s.emitObservedSample(nodeID, key, dataType, value, epoch)
}

func (s *LowerSupervisor) Call(ctx context.Context, key domain.NodeKey, hexData string) (string, domain.StatusCode, error) {
	return s.CallAtEpoch(ctx, key, hexData, 0)
}

func (s *LowerSupervisor) CallAtEpoch(ctx context.Context, key domain.NodeKey, hexData string, expectedEpoch uint64) (string, domain.StatusCode, error) {
	client, methodID, _, _, err := s.operationTargetAtEpoch(key, expectedEpoch)
	if err != nil {
		return "", domain.StatusBadNoCommunication, err
	}
	ns := methodID.Namespace()
	arg, err := gu.NewVariant(hexData)
	if err != nil {
		return "", domain.StatusBadConfigurationError, err
	}
	result, err := client.Call(ctx, &gu.CallMethodRequest{
		ObjectID: gu.NewStringNodeID(ns, "Methods"), MethodID: methodID,
		InputArguments: []*gu.Variant{arg},
	})
	if err != nil {
		return "", domain.StatusBadNoCommunication, err
	}
	status := domain.StatusCode(result.StatusCode)
	if result.StatusCode != gu.StatusOK {
		return "", status, nil
	}
	if len(result.OutputArguments) != 1 {
		return "", domain.StatusBadConfigurationError, errors.New("method returned an invalid output signature")
	}
	output, ok := result.OutputArguments[0].Value().(string)
	if !ok {
		return "", domain.StatusBadConfigurationError, errors.New("method result is not a string")
	}
	return output, status, nil
}

func (s *LowerSupervisor) operationTarget(key domain.NodeKey) (*gopcua.Client, *gu.NodeID, domain.DataType, uint64, error) {
	return s.operationTargetAtEpoch(key, 0)
}

func (s *LowerSupervisor) operationTargetAtEpoch(key domain.NodeKey, expectedEpoch uint64) (*gopcua.Client, *gu.NodeID, domain.DataType, uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.status.Connection != ConnectionConnected {
		return nil, nil, "", 0, ErrLowerUnavailable
	}
	if expectedEpoch != 0 && s.status.Epoch != expectedEpoch {
		return nil, nil, "", s.status.Epoch, fmt.Errorf("lower connection epoch changed from %d to %d: %w", expectedEpoch, s.status.Epoch, ErrLowerUnavailable)
	}
	ns, ok := s.namespaces[key.NamespaceURI]
	if !ok {
		return nil, nil, "", 0, fmt.Errorf("namespace %q: %w", key.NamespaceURI, ErrLowerUnavailable)
	}
	nodeID := gu.NewStringNodeID(ns, key.Identifier)
	dataType := domain.DataType("")
	if node, ok := s.nodes[nodeID.String()]; ok {
		dataType = node.dataType
	}
	return s.client, nodeID, dataType, s.status.Epoch, nil
}

func (s *LowerSupervisor) Status() LowerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *LowerSupervisor) updateStatus(update func(*LowerStatus)) {
	s.mu.Lock()
	update(&s.status)
	s.status.UpdatedAt = time.Now().UTC()
	status := s.status
	s.mu.Unlock()
	s.sink.OnLowerStatus(status)
}

func (s *LowerSupervisor) setDisconnected(message string) {
	s.updateStatus(func(st *LowerStatus) {
		st.Connection = ConnectionDisconnected
		st.Session = SessionInactive
		st.Subscription = SubscriptionInactive
		st.LastError = message
	})
}

func (s *LowerSupervisor) touchLastData(epoch uint64) {
	now := time.Now().UTC()
	s.mu.Lock()
	// OnSample can finish after its connection was replaced (for example
	// while a command audit was being persisted). Do not relabel that old
	// callback as current-epoch data or emit the new connection's status.
	if s.epoch.Load() != epoch || s.status.Epoch != epoch {
		s.mu.Unlock()
		return
	}
	s.status.LastDataAt = now
	s.status.UpdatedAt = now
	status := s.status
	s.mu.Unlock()
	last := s.lastStatusEmit.Load()
	if now.UnixNano()-last >= int64(time.Second) && s.lastStatusEmit.CompareAndSwap(last, now.UnixNano()) {
		s.sink.OnLowerStatus(status)
	}
}

func (s *LowerSupervisor) jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	// 0.8x..1.2x avoids synchronized reconnect storms across 20 sources.
	factor := 0.8 + s.rng.Float64()*0.4
	return time.Duration(float64(base) * factor)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
