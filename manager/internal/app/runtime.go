package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

type LowerFactory func(opc.LowerConfig, opc.LowerSink) (opc.LowerClient, error)
type CatalogPublisher func(domain.CatalogUnion, []domain.Group, []domain.Source) error

type RuntimeOptions struct {
	OPCEndpoint        string
	HTTPListen         string
	DataDir            string
	DatabasePath       string
	LogDir             string
	CertificateFile    string
	PrivateKeyFile     string
	Version            string
	GitSHA             string
	MaxSources         int
	MaxSessions        int
	CommandTimeout     time.Duration
	CommandPendingTTL  time.Duration
	AuditRetention     time.Duration
	MaxAuditRows       int
	PublishingInterval time.Duration
	SamplingInterval   time.Duration
	LowerFactory       LowerFactory
	ProcessAgentClient ProcessAgentClient
}

type lowerHandle struct {
	client     opc.LowerClient
	cancel     context.CancelFunc
	generation uint64
}

type pendingCommandMeta struct {
	generation       uint64
	epoch            uint64
	baselineSequence uint64
	armed            bool
	terminal         *pendingCommandTerminal
}

type pendingCommandTerminal struct {
	state   domain.CommandState
	status  domain.StatusCode
	message string
}

type sessionRefreshRequest struct {
	sessionID string
	groupID   string
}

// Runtime owns the mutable manager state. Configuration/catalog metadata is
// persisted, while samples remain exclusively in the sharded in-memory cache.
type Runtime struct {
	repo    *store.Repository
	routes  *domain.RouteTable
	broker  *api.Broker
	cache   *sampleCache
	options RuntimeOptions

	configMu                sync.RWMutex
	sourceOperations        map[string]bool   // guarded by configMu; no hot-path I/O
	sourceProcessOperations map[string]string // durable maintenance fences, guarded by configMu
	processExecuting        map[string]string // source -> executing request, guarded by configMu
	lifecycleMu             sync.Mutex
	commandMu               sync.Mutex
	statusPersistMu         sync.Mutex
	statusRetryMu           sync.Mutex
	stationCacheMu          sync.Mutex
	mu                      sync.RWMutex
	catalogs                map[string]domain.SourceCatalog
	index                   *catalogIndex
	statuses                map[string]opc.LowerStatus
	lowers                  map[string]lowerHandle
	generations             map[string]uint64
	dirtyCatalogs           map[string]domain.SourceCatalog
	dirtyCommands           map[string]domain.PendingCommand
	dirtyAudits             map[string]domain.AuditEntry
	dirtySourceEvents       []domain.SourceEvent
	dirtyAlarmStatuses      map[string]opc.LowerStatus
	stationVersions         map[string]domain.SourceStationVersions
	stationVersionEpoch     map[string]uint64
	stationRefreshPending   map[string]bool
	stationRefreshDirty     map[string]bool
	stationFeatureEpoch     map[string]uint64
	stationFeatureSupported map[string]bool
	quarantinedSources      map[string]string
	pendingCommandMeta      map[string]pendingCommandMeta
	ackSequences            map[string]uint64
	ackEpochs               map[string]uint64
	commandHistory          map[string]commandHistoryEntry
	catalogPersistError     string
	commandPersistError     string
	auditPersistError       string
	statusPersistError      string
	stationPersistError     string
	upstreamReconcileError  string
	upstreamReconcileDirty  bool
	conflictCount           int
	selectionCount          uint64
	lastSelectionAt         time.Time
	groupLastSelectedAt     map[string]time.Time
	lastLowerDataAt         time.Time
	lastSampleEventAt       time.Time
	sampleEventPending      bool
	startedAt               time.Time
	runContext              context.Context
	runCancel               context.CancelFunc
	shutdownDone            chan struct{}
	refreshSession          func(sessionID, groupID string)
	refreshQueue            chan sessionRefreshRequest
	statusPersistWake       chan struct{}
	stationRefreshQueue     chan string
	catalogValidator        CatalogPublisher
	catalogReconciler       CatalogPublisher
	sessionViews            func() map[string]api.SessionView
	subscriptionCount       func() int

	running atomic.Bool
	ready   atomic.Bool
	closing atomic.Bool
	started bool
	wg      sync.WaitGroup
}

func NewRuntime(ctx context.Context, repo *store.Repository, options RuntimeOptions) (*Runtime, error) {
	if repo == nil {
		return nil, errors.New("repository is required")
	}
	if options.MaxSources <= 0 {
		options.MaxSources = 20
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = 10
	}
	if options.CommandTimeout <= 0 {
		options.CommandTimeout = 5 * time.Second
	}
	if options.CommandPendingTTL <= 0 {
		options.CommandPendingTTL = 30 * time.Second
	}
	if options.CommandPendingTTL < options.CommandTimeout {
		return nil, errors.New("command pending timeout must be greater than or equal to command timeout")
	}
	if options.AuditRetention <= 0 {
		options.AuditRetention = 90 * 24 * time.Hour
	}
	if options.MaxAuditRows <= 0 {
		options.MaxAuditRows = 100000
	}
	if options.PublishingInterval <= 0 {
		options.PublishingInterval = 50 * time.Millisecond
	}
	if options.SamplingInterval <= 0 {
		options.SamplingInterval = 50 * time.Millisecond
	}
	if options.LowerFactory == nil {
		options.LowerFactory = func(cfg opc.LowerConfig, sink opc.LowerSink) (opc.LowerClient, error) {
			return opc.NewLowerSupervisor(cfg, sink)
		}
	}
	if options.ProcessAgentClient == nil {
		options.ProcessAgentClient = newProcessAgentHTTPClient()
	}

	groups, err := repo.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	sources, err := repo.ListSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("load sources: %w", err)
	}
	catalogs := make(map[string]domain.SourceCatalog, len(sources))
	quarantinedSources := make(map[string]string)
	dirtyCatalogs := make(map[string]domain.SourceCatalog)
	for index := range sources {
		catalog, loadErr := repo.LoadSourceCatalog(ctx, sources[index].ID)
		if loadErr != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("load catalog for %s: %w", sources[index].ID, loadErr)
			}
			catalog = domain.SourceCatalog{
				SourceID: sources[index].ID, GroupID: sources[index].GroupID,
				State: domain.CatalogConflict, QuarantineReason: "cached catalog unreadable: " + loadErr.Error(),
			}
			dirtyCatalogs[sources[index].ID] = catalog
		} else {
			active := catalog.Nodes[:0]
			for _, node := range catalog.Nodes {
				// Retired nodes only survive the process in which they disappeared.
				if !node.Retired {
					active = append(active, node)
				}
			}
			catalog.Nodes = active
			annotated, topologyErr := domain.AssignCatalogGroups(sources[index], catalog)
			if topologyErr != nil {
				catalog.QuarantineReason = topologyErr.Error()
				catalog.Nodes = nil
				// Never publish an unvalidated cached topology. A later successful
				// discovery replaces this quarantined snapshot atomically.
				catalog.Topology = nil
				catalog.State = domain.CatalogConflict
				dirtyCatalogs[sources[index].ID] = catalog
			} else {
				catalog = annotated
			}
		}
		if catalog.QuarantineReason != "" {
			catalog.State = domain.CatalogConflict
			quarantinedSources[sources[index].ID] = catalog.QuarantineReason
		} else if len(catalog.Nodes) == 0 {
			catalog.State = domain.CatalogDiscovering
		} else if catalog.State != domain.CatalogConflict {
			catalog.State = domain.CatalogCached
		}
		catalogs[catalog.SourceID] = catalog
		sources[index].CatalogState = catalog.State
		sources[index].AvailableGroupIDs = domain.AvailableGroups(sources[index], catalog)
	}
	index, states, conflicts, err := validateCatalogs(catalogs)
	if err != nil {
		return nil, err
	}
	for id := range quarantinedSources {
		states[id] = domain.CatalogConflict
		index.union.SourceStates[id] = domain.CatalogConflict
		conflicts++
	}
	for sourceIndex := range sources {
		if state, ok := states[sources[sourceIndex].ID]; ok {
			sources[sourceIndex].CatalogState = state
			catalog := catalogs[sources[sourceIndex].ID]
			catalog.State = state
			catalogs[sources[sourceIndex].ID] = catalog
		}
	}
	routes, err := domain.NewRouteTable(groups, sources)
	if err != nil {
		return nil, fmt.Errorf("build route table: %w", err)
	}
	selectionCount, lastSelectionAt, groupLastSelectedAt, err := loadSelectionMetrics(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("load selection metrics: %w", err)
	}
	now := time.Now().UTC()
	if err := repo.PruneOperationalHistory(ctx, now.Add(-options.AuditRetention), options.MaxAuditRows); err != nil {
		return nil, fmt.Errorf("prune operational history: %w", err)
	}
	stationVersions := make(map[string]domain.SourceStationVersions)
	for _, source := range sources {
		snapshot, loadErr := repo.LoadSourceStationVersions(ctx, source.ID)
		if errors.Is(loadErr, domain.ErrNotFound) {
			continue
		}
		if loadErr != nil {
			return nil, fmt.Errorf("load station versions for %s: %w", source.ID, loadErr)
		}
		fingerprint := domain.SourceTopologyFingerprint(catalogs[source.ID].Topology)
		if fingerprint == "" || snapshot.TopologyFingerprint != fingerprint {
			continue
		}
		snapshot.Cached = true
		stationVersions[source.ID] = snapshot
	}
	runtime := &Runtime{
		repo: repo, routes: routes, broker: api.NewBroker(), cache: newSampleCache(), options: options,
		catalogs: catalogs, index: index, statuses: make(map[string]opc.LowerStatus),
		lowers: make(map[string]lowerHandle), generations: make(map[string]uint64),
		dirtyCatalogs: dirtyCatalogs,
		dirtyCommands: make(map[string]domain.PendingCommand), dirtyAudits: make(map[string]domain.AuditEntry),
		dirtyAlarmStatuses: make(map[string]opc.LowerStatus),
		stationVersions:    stationVersions, stationVersionEpoch: make(map[string]uint64),
		stationRefreshPending: make(map[string]bool), stationRefreshDirty: make(map[string]bool),
		stationFeatureEpoch: make(map[string]uint64), stationFeatureSupported: make(map[string]bool),
		quarantinedSources:  quarantinedSources,
		pendingCommandMeta:  make(map[string]pendingCommandMeta),
		ackSequences:        make(map[string]uint64),
		ackEpochs:           make(map[string]uint64),
		refreshQueue:        make(chan sessionRefreshRequest, 256),
		statusPersistWake:   make(chan struct{}, 1),
		stationRefreshQueue: make(chan string, 64),
		conflictCount:       conflicts, selectionCount: selectionCount, lastSelectionAt: lastSelectionAt,
		groupLastSelectedAt: groupLastSelectedAt, startedAt: now,
		sourceProcessOperations: make(map[string]string), processExecuting: make(map[string]string),
	}
	if err := runtime.restoreProcessOperations(ctx); err != nil {
		return nil, fmt.Errorf("restore process maintenance fences: %w", err)
	}
	// Rehydrate in-flight command locks before any OPC Session can be accepted.
	for offset := 0; ; offset += 1000 {
		pending, total, loadErr := repo.ListCommandsFiltered(ctx, store.CommandFilter{Status: string(domain.CommandWaiting), Limit: 1000, Offset: offset})
		if loadErr != nil {
			return nil, fmt.Errorf("load pending commands: %w", loadErr)
		}
		for _, command := range pending {
			if restoreErr := routes.RestorePending(command); restoreErr != nil {
				slog.Warn("ignore invalid persisted command", "command_id", command.ID, "error", restoreErr)
			}
		}
		if offset+len(pending) >= total || len(pending) == 0 {
			break
		}
	}
	for _, command := range routes.SweepExpired(now, options.CommandPendingTTL) {
		if saveErr := repo.SaveCommand(ctx, command); saveErr != nil {
			return nil, fmt.Errorf("persist expired restored command %s: %w", command.ID, saveErr)
		}
	}
	for _, source := range sources {
		lastError := ""
		if source.CatalogState == domain.CatalogConflict {
			lastError = catalogConflictSummary(conflictsForSource(index.union.Conflicts, source.ID))
			if reason := quarantinedSources[source.ID]; reason != "" {
				lastError = "catalog rejected: " + reason
			}
		}
		runtime.statuses[source.ID] = opc.LowerStatus{
			SourceID: source.ID, Connection: opc.ConnectionDisconnected, Session: opc.SessionInactive,
			Subscription: opc.SubscriptionInactive, Catalog: source.CatalogState, LastError: lastError, UpdatedAt: now,
		}
	}
	return runtime, nil
}

func (r *Runtime) Start(ctx context.Context) error {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	r.lifecycleMu.Lock()
	if r.started {
		r.lifecycleMu.Unlock()
		return errors.New("runtime cannot be restarted")
	}
	r.started = true
	r.closing.Store(false)
	r.running.Store(true)
	r.mu.Lock()
	r.runContext, r.runCancel = context.WithCancel(ctx)
	r.shutdownDone = nil
	r.mu.Unlock()
	r.lifecycleMu.Unlock()
	for _, source := range r.routes.Sources() {
		if source.Enabled {
			if err := r.startLower(source); err != nil {
				slog.Error("start lower OPC supervisor", "source_id", source.ID, "error", err)
				r.recordLowerStartFailure(source, err)
			}
		}
	}
	if !r.running.Load() || r.closing.Load() {
		return domain.ErrShuttingDown
	}
	r.ready.Store(true)
	r.wg.Add(1)
	go r.commandSweeper()
	r.wg.Add(1)
	go r.sessionRefreshWorker()
	r.wg.Add(1)
	go r.retentionSweeper()
	r.wg.Add(1)
	go r.processControlWorker()
	r.wg.Add(1)
	go r.stationVersionRefreshWorker()
	return nil
}

func (r *Runtime) Close() {
	_ = r.CloseContext(context.Background())
}

func (r *Runtime) CloseContext(ctx context.Context) error {
	r.lifecycleMu.Lock()
	r.mu.Lock()
	done := r.shutdownDone
	startWaiter := false
	if r.running.Load() {
		r.running.Store(false)
		r.closing.Store(true)
		r.ready.Store(false)
		if r.runCancel != nil {
			r.runCancel()
		}
		for sourceID, handle := range r.lowers {
			r.generations[sourceID]++
			handle.cancel()
			delete(r.lowers, sourceID)
		}
		done = make(chan struct{})
		r.shutdownDone = done
		startWaiter = true
	}
	r.mu.Unlock()
	r.lifecycleMu.Unlock()
	if startWaiter {
		go func() {
			// Every path that can add a worker is serialized by configMu and
			// rechecks running before WaitGroup.Add. Waiting for this short gate
			// asynchronously makes CloseContext honor its deadline even if a
			// configuration/DPAPI/factory operation is currently blocked.
			r.configMu.Lock()
			r.configMu.Unlock()
			r.wg.Wait()
			flushContext, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
			r.retryDirtyCommands()
			r.persistDirtyCatalogs(flushContext)
			r.retryDirtyAudits(flushContext)
			r.retryDirtyStatuses(flushContext)
			flushCancel()
			r.broker.Close()
			close(done)
		}()
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) SetRefreshHandler(handler func(sessionID, groupID string)) {
	r.mu.Lock()
	r.refreshSession = handler
	r.mu.Unlock()
}

func (r *Runtime) queueSessionRefresh(sessionID, groupID string) {
	r.mu.RLock()
	handler := r.refreshSession
	ctx := r.runContext
	r.mu.RUnlock()
	if handler == nil || ctx == nil || !r.running.Load() {
		return
	}
	select {
	case r.refreshQueue <- sessionRefreshRequest{sessionID: sessionID, groupID: groupID}:
	case <-ctx.Done():
	}
}

func (r *Runtime) sessionRefreshWorker() {
	defer r.wg.Done()
	r.mu.RLock()
	ctx := r.runContext
	r.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-r.refreshQueue:
			r.mu.RLock()
			handler := r.refreshSession
			r.mu.RUnlock()
			if handler != nil {
				handler(request.sessionID, request.groupID)
			}
		}
	}
}

func (r *Runtime) retentionSweeper() {
	defer r.wg.Done()
	r.mu.RLock()
	ctx := r.runContext
	r.mu.RUnlock()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := r.repo.PruneOperationalHistory(pruneCtx, time.Now().UTC().Add(-r.options.AuditRetention), r.options.MaxAuditRows)
			cancel()
			if err != nil {
				slog.Error("prune operational history", "error", err)
			}
		}
	}
}

func (r *Runtime) SetCatalogHandlers(validator, reconciler CatalogPublisher) error {
	r.mu.Lock()
	r.catalogValidator = validator
	r.catalogReconciler = reconciler
	union := cloneCatalogUnion(r.index.union)
	r.mu.Unlock()
	groups, sources := r.routes.Groups(), r.routes.Sources()
	if validator != nil {
		if err := validator(union, groups, sources); err != nil {
			r.recordReconcileError(err)
			return err
		}
	}
	if reconciler != nil {
		if err := reconciler(union, groups, sources); err != nil {
			r.recordUpstreamReconcileResult(err)
			return err
		}
	}
	r.recordUpstreamReconcileResult(nil)
	return nil
}

func (r *Runtime) reconcileUpstream() error {
	r.mu.RLock()
	handler := r.catalogReconciler
	union := cloneCatalogUnion(r.index.union)
	r.mu.RUnlock()
	if handler == nil {
		return nil
	}
	err := handler(union, r.routes.Groups(), r.routes.Sources())
	r.recordUpstreamReconcileResult(err)
	return err
}

func (r *Runtime) validateUpstream(index *catalogIndex, groups []domain.Group, sources []domain.Source) error {
	r.mu.RLock()
	validator := r.catalogValidator
	r.mu.RUnlock()
	if validator == nil {
		return nil
	}
	union := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{}, SourceStates: map[string]domain.CatalogState{}}
	if index != nil {
		union = cloneCatalogUnion(index.union)
	}
	return validator(union, groups, sources)
}

func (r *Runtime) recordReconcileError(err error) {
	r.mu.Lock()
	if err == nil {
		if len(r.quarantinedSources) == 0 {
			r.upstreamReconcileError = ""
		}
	} else {
		r.upstreamReconcileError = err.Error()
	}
	r.mu.Unlock()
}

func (r *Runtime) recordUpstreamReconcileResult(err error) {
	r.mu.Lock()
	r.upstreamReconcileDirty = err != nil
	if err == nil {
		if len(r.quarantinedSources) == 0 {
			r.upstreamReconcileError = ""
		}
	} else {
		r.upstreamReconcileError = err.Error()
	}
	r.mu.Unlock()
}

func (r *Runtime) SetSessionViewHandler(handler func() map[string]api.SessionView) {
	r.mu.Lock()
	r.sessionViews = handler
	r.mu.Unlock()
}

func (r *Runtime) SetSubscriptionCountHandler(handler func() int) {
	r.mu.Lock()
	r.subscriptionCount = handler
	r.mu.Unlock()
}

func loadSelectionMetrics(ctx context.Context, repo *store.Repository) (uint64, time.Time, map[string]time.Time, error) {
	groupTimes := make(map[string]time.Time)
	var count uint64
	var latest time.Time
	for offset := 0; ; offset += 1000 {
		entries, total, err := repo.ListAudit(ctx, 1000, offset)
		if err != nil {
			return 0, time.Time{}, nil, err
		}
		for _, entry := range entries {
			groupID := ""
			switch entry.Kind {
			case "group.default_source":
				groupID = entry.Target
			case "session.selection":
				if separator := strings.LastIndex(entry.Target, "/"); separator >= 0 {
					groupID = entry.Target[separator+1:]
				}
			default:
				continue
			}
			count++
			if entry.OccurredAt.After(latest) {
				latest = entry.OccurredAt
			}
			if groupID != "" && entry.OccurredAt.After(groupTimes[groupID]) {
				groupTimes[groupID] = entry.OccurredAt
			}
		}
		if offset+len(entries) >= total || len(entries) == 0 {
			break
		}
	}
	return count, latest, groupTimes, nil
}

func (r *Runtime) CatalogUnion() domain.CatalogUnion {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneCatalogUnion(r.index.union)
}

func (r *Runtime) startLower(source domain.Source) error {
	r.mu.Lock()
	ctx := r.runContext
	if ctx == nil || !r.running.Load() || ctx.Err() != nil {
		r.mu.Unlock()
		return nil
	}
	if old, exists := r.lowers[source.ID]; exists {
		old.cancel()
		delete(r.lowers, source.ID)
	}
	r.generations[source.ID]++
	generation := r.generations[source.ID]
	previousStatus := r.statuses[source.ID]
	r.mu.Unlock()

	_, password, err := r.repo.SourceCredential(ctx, source.ID)
	if err != nil {
		return err
	}
	proxy := lowerSinkProxy{runtime: r, sourceID: source.ID, generation: generation,
		reconnectBase: previousStatus.ReconnectCount, lastDataAt: previousStatus.LastDataAt, lastProbeAt: previousStatus.LastProbeAt}
	client, err := r.options.LowerFactory(opc.LowerConfig{
		Source: source, Password: password, CertificateFile: r.options.CertificateFile,
		PrivateKeyFile: r.options.PrivateKeyFile, PublishingInterval: r.options.PublishingInterval,
		SamplingInterval: r.options.SamplingInterval, MonitorBatchSize: opc.DefaultMonitorBatchSize,
		OperationTimeout: r.options.CommandTimeout,
	}, proxy)
	if err != nil {
		return err
	}
	lowerCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if !r.running.Load() || ctx.Err() != nil || r.generations[source.ID] != generation {
		r.mu.Unlock()
		cancel()
		return nil
	}
	r.wg.Add(1)
	r.lowers[source.ID] = lowerHandle{client: client, cancel: cancel, generation: generation}
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		runErr := client.Run(lowerCtx)
		if lowerCtx.Err() == nil {
			if runErr == nil {
				runErr = errors.New("lower OPC supervisor stopped unexpectedly")
			}
			slog.Error("lower OPC supervisor stopped", "source_id", source.ID, "error", runErr)
			proxy.OnLowerStatus(opc.LowerStatus{SourceID: source.ID, Connection: opc.ConnectionDisconnected,
				Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive,
				LastError: runErr.Error(), UpdatedAt: time.Now().UTC()})
		}
		r.mu.Lock()
		if current, exists := r.lowers[source.ID]; exists && current.generation == generation {
			delete(r.lowers, source.ID)
		}
		r.mu.Unlock()
	}()
	return nil
}

func (r *Runtime) recordLowerStartFailure(source domain.Source, err error) {
	r.mu.RLock()
	current := r.statuses[source.ID]
	r.mu.RUnlock()
	r.OnLowerStatus(opc.LowerStatus{SourceID: source.ID, Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive,
		Epoch: current.Epoch, ReconnectCount: current.ReconnectCount,
		LastDataAt: current.LastDataAt, LastProbeAt: current.LastProbeAt,
		LastError: err.Error(), UpdatedAt: time.Now().UTC()})
}

func (r *Runtime) stopLower(sourceID string) {
	r.mu.Lock()
	r.generations[sourceID]++
	if handle, ok := r.lowers[sourceID]; ok {
		handle.cancel()
		delete(r.lowers, sourceID)
	}
	previous, existed := r.statuses[sourceID]
	status := previous
	status.SourceID = sourceID
	status.Connection = opc.ConnectionDisconnected
	status.Session = opc.SessionInactive
	status.Subscription = opc.SubscriptionInactive
	status.UpdatedAt = time.Now().UTC()
	r.statuses[sourceID] = status
	r.mu.Unlock()
	if existed && lowerStatusControlChanged(previous, status) {
		r.persistLowerStatusTransition(previous, status, true)
	}
}

type lowerSinkProxy struct {
	runtime       *Runtime
	sourceID      string
	generation    uint64
	reconnectBase uint64
	lastDataAt    time.Time
	lastProbeAt   time.Time
}

func (p lowerSinkProxy) OnLowerStatus(status opc.LowerStatus) {
	status.SourceID = p.sourceID
	status.Epoch = p.generation<<32 | status.Epoch&0xffffffff
	status.ReconnectCount += p.reconnectBase
	if status.LastDataAt.IsZero() {
		status.LastDataAt = p.lastDataAt
	}
	if status.LastProbeAt.IsZero() {
		status.LastProbeAt = p.lastProbeAt
	}
	p.runtime.onLowerStatus(status, p.generation)
}

func (p lowerSinkProxy) OnCatalog(catalog domain.SourceCatalog) {
	p.runtime.onCatalog(p.sourceID, p.generation, catalog)
}

func (p lowerSinkProxy) OnSample(_ string, key domain.NodeKey, sample domain.Sample) {
	sample.Epoch = p.generation<<32 | sample.Epoch&0xffffffff
	p.runtime.onSample(p.sourceID, key, sample, p.generation)
}

func (p lowerSinkProxy) OnStationVersionRevision(_ string, _ string, _ uint64) {
	p.runtime.mu.RLock()
	current := p.runtime.generations[p.sourceID]
	p.runtime.mu.RUnlock()
	if current == p.generation {
		p.runtime.queueStationVersionRefresh(p.sourceID)
	}
}

func (r *Runtime) OnLowerStatus(status opc.LowerStatus) {
	r.onLowerStatus(status, 0)
}

func (r *Runtime) onLowerStatus(status opc.LowerStatus, generation uint64) {
	var refreshGroups []string
	refreshStationVersions := false
	if status.Catalog == domain.CatalogConflict {
		r.configMu.Lock()
		defer func() {
			r.configMu.Unlock()
			r.queueGroupRefreshes(refreshGroups...)
		}()
	}
	now := time.Now().UTC()
	r.mu.Lock()
	if !r.lowerCallbackCurrentLocked(status.SourceID, generation) {
		r.mu.Unlock()
		return
	}
	previous, existed := r.statuses[status.SourceID]
	if existed && previous.Epoch > status.Epoch && status.Epoch != 0 {
		r.mu.Unlock()
		return
	}
	quarantined := false
	if status.Catalog == domain.CatalogConflict {
		// Do not unlock between accepting this connection and quarantining its
		// catalog. Otherwise a delayed old epoch can clear newer Session routes.
		quarantined = r.markCatalogQuarantineLocked(status.SourceID, status.LastError)
		status.LastError = "catalog rejected: " + r.quarantinedSources[status.SourceID]
	}
	if catalog, ok := r.catalogs[status.SourceID]; ok {
		status.Catalog = catalog.State
		if catalog.State == domain.CatalogConflict && strings.TrimSpace(status.LastError) == "" {
			if reason := r.quarantinedSources[status.SourceID]; reason != "" {
				status.LastError = "catalog rejected: " + reason
			} else if r.index != nil {
				status.LastError = catalogConflictSummary(conflictsForSource(r.index.union.Conflicts, status.SourceID))
			}
		}
	}
	r.statuses[status.SourceID] = status
	refreshStationVersions = status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive &&
		status.Subscription == opc.SubscriptionActive && (!existed || previous.Subscription != opc.SubscriptionActive || previous.Epoch != status.Epoch)
	meaningfulChange := !existed || lowerStatusControlChanged(previous, status)
	publish := meaningfulChange || now.Sub(r.lastSampleEventAt) >= time.Second
	if publish {
		// LastDataAt-only status ticks and data-change invalidations share one
		// global budget. One query refresh observes every source's latest state.
		r.lastSampleEventAt = now
		r.sampleEventPending = false
	} else {
		r.sampleEventPending = true
	}
	r.mu.Unlock()
	if quarantined {
		r.applyQuarantinedSourceLocked(status.SourceID)
		refreshGroups = r.groupsForSources(status.SourceID)
		r.broker.Publish(api.Event{Type: "catalog.changed", SourceID: status.SourceID})
	}
	if meaningfulChange {
		r.persistLowerStatusTransition(previous, status, existed)
	}
	if publish {
		r.broker.Publish(api.Event{Type: "source.status", Data: status})
	}
	if refreshStationVersions {
		r.queueStationVersionRefresh(status.SourceID)
	}
}

func lowerStatusControlChanged(before, after opc.LowerStatus) bool {
	return before.SourceID != after.SourceID || before.Connection != after.Connection ||
		before.Session != after.Session || before.Subscription != after.Subscription ||
		before.Catalog != after.Catalog || before.Epoch != after.Epoch ||
		before.ReconnectCount != after.ReconnectCount || before.PointCount != after.PointCount ||
		before.SubscriptionCount != after.SubscriptionCount || before.LastError != after.LastError
}

func (r *Runtime) OnCatalog(catalog domain.SourceCatalog) {
	r.onCatalog(catalog.SourceID, 0, catalog)
}

func (r *Runtime) onCatalog(sourceID string, generation uint64, discovered domain.SourceCatalog) {
	r.configMu.Lock()
	var refreshGroups []string
	defer func() {
		r.configMu.Unlock()
		r.queueGroupRefreshes(refreshGroups...)
	}()
	if r.closing.Load() {
		return
	}
	if generation != 0 {
		r.mu.RLock()
		current := r.generations[sourceID]
		r.mu.RUnlock()
		if !r.running.Load() || current != generation {
			return
		}
	}
	source, err := r.source(sourceID)
	if err != nil {
		return
	}
	discovered.SourceID = sourceID
	discovered.GroupID = source.GroupID
	discovered.State = domain.CatalogReady
	// Incoming discoveries must pass all topology and upstream validation;
	// only their successful publication clears a saved explicit rejection.
	discovered.QuarantineReason = ""
	annotated, topologyErr := domain.AssignCatalogGroups(source, discovered)
	if topologyErr != nil {
		if r.quarantineLowerTopologyLocked(sourceID, topologyErr.Error(), generation) {
			refreshGroups = source.GroupIDs()
		}
		return
	}
	discovered = annotated
	if discovered.DiscoveredAt.IsZero() {
		discovered.DiscoveredAt = time.Now().UTC()
	}

	r.mu.RLock()
	previous := r.catalogs[sourceID]
	before := cloneCatalogMap(r.catalogs)
	handler := r.catalogReconciler
	r.mu.RUnlock()
	merged := retainRemovedNodes(previous, discovered)
	candidate := cloneCatalogMap(before)
	candidate[sourceID] = merged
	index, states, conflicts, validateErr := validateCatalogs(candidate)
	if validateErr != nil {
		slog.Error("reject discovered catalog", "source_id", sourceID, "error", validateErr)
		return
	}
	conflicts += r.enforceCatalogQuarantine(index, states, sourceID)
	for id, state := range states {
		catalog := candidate[id]
		catalog.State = state
		candidate[id] = catalog
	}
	groups := r.routes.Groups()
	sources := r.routes.Sources()
	for i := range sources {
		sources[i].AvailableGroupIDs = domain.AvailableGroups(sources[i], candidate[sources[i].ID])
		if state, ok := states[sources[i].ID]; ok {
			sources[i].CatalogState = state
		}
	}
	if err := r.validateUpstream(index, groups, sources); err != nil {
		slog.Error("reject catalog before upstream publish", "source_id", sourceID, "error", err)
		if r.isolateRejectedCatalog(sourceID, generation, discovered.DiscoveredAt, before, groups, sources, handler, err) {
			refreshGroups = source.GroupIDs()
		}
		return
	}
	type statusChange struct {
		previous opc.LowerStatus
		current  opc.LowerStatus
		existed  bool
	}
	statusChanges := make([]statusChange, 0, len(states))
	r.mu.Lock()
	if !r.lowerCallbackCurrentLocked(sourceID, generation) {
		r.mu.Unlock()
		return
	}
	r.catalogs = candidate
	r.index = index
	r.conflictCount = conflicts
	delete(r.quarantinedSources, sourceID)
	now := time.Now().UTC()
	for id, state := range states {
		previousStatus, existed := r.statuses[id]
		status := previousStatus
		status.SourceID = id
		status.Catalog = state
		if state == domain.CatalogConflict {
			if reason := candidate[id].QuarantineReason; reason != "" {
				status.LastError = "catalog rejected: " + reason
			} else if status.LastError == "" || strings.HasPrefix(status.LastError, "catalog conflict:") || strings.HasPrefix(status.LastError, "catalog rejected:") {
				status.LastError = catalogConflictSummary(conflictsForSource(index.union.Conflicts, id))
			}
		} else if strings.HasPrefix(status.LastError, "catalog conflict:") || strings.HasPrefix(status.LastError, "catalog rejected:") || (id == sourceID && previousStatus.Catalog == domain.CatalogConflict) {
			status.LastError = ""
		}
		status.UpdatedAt = now
		r.statuses[id] = status
		if !existed || lowerStatusControlChanged(previousStatus, status) {
			statusChanges = append(statusChanges, statusChange{previous: previousStatus, current: status, existed: existed})
		}
	}
	r.mu.Unlock()
	for _, change := range statusChanges {
		r.persistLowerStatusTransition(change.previous, change.current, change.existed)
	}

	// Persist the reconciled state. Retired rows are harmless on disk and are
	// deliberately discarded by NewRuntime after the next process restart.
	changed := make([]domain.SourceCatalog, 0, len(candidate))
	for id, catalog := range candidate {
		old := before[id]
		if id == sourceID || old.State != catalog.State {
			changed = append(changed, catalog)
		}
	}
	for _, catalog := range changed {
		if err := r.repo.ReplaceSourceCatalog(context.Background(), catalog); err != nil {
			slog.Error("persist catalog", "source_id", catalog.SourceID, "error", err)
			r.mu.Lock()
			r.dirtyCatalogs[catalog.SourceID] = catalog
			r.catalogPersistError = err.Error()
			r.mu.Unlock()
		} else {
			r.mu.Lock()
			delete(r.dirtyCatalogs, catalog.SourceID)
			if len(r.dirtyCatalogs) == 0 {
				r.catalogPersistError = ""
			}
			r.mu.Unlock()
		}
	}
	if err := r.routes.ApplyConfiguration(groups, sources); err != nil {
		slog.Error("apply catalog selection state", "error", err)
	}
	changedSources := []string{sourceID}
	for _, change := range statusChanges {
		changedSources = append(changedSources, change.current.SourceID)
	}
	refreshGroups = r.groupsForSources(changedSources...)
	if handler != nil {
		if err := handler(cloneCatalogUnion(index.union), groups, sources); err != nil {
			slog.Error("reconcile upstream catalog", "error", err)
			r.recordUpstreamReconcileResult(err)
		} else {
			r.recordUpstreamReconcileResult(nil)
		}
	}
	r.broker.Publish(api.Event{Type: "catalog.changed", Data: map[string]any{"source_id": sourceID}})
	r.invalidateStationVersionsForTopology(sourceID, discovered.Topology)
	r.queueStationVersionRefresh(sourceID)
}

func (r *Runtime) OnSample(sourceID string, key domain.NodeKey, sample domain.Sample) {
	r.onSample(sourceID, key, sample, 0)
}

func (r *Runtime) onSample(sourceID string, key domain.NodeKey, sample domain.Sample, generation uint64) {
	now := time.Now().UTC()
	ackPort := ""
	ackSequence := uint64(0)
	r.mu.Lock()
	if generation != 0 && (!r.running.Load() || r.generations[sourceID] != generation) {
		r.mu.Unlock()
		return
	}
	currentStatus := r.statuses[sourceID]
	if currentStatus.Epoch != 0 && sample.Epoch != currentStatus.Epoch {
		r.mu.Unlock()
		return
	}
	if strings.HasSuffix(key.Identifier, "_Send") && (sample.Type == "" || sample.Type == domain.DataTypeString) {
		// ACK handling is pinned by command/source/epoch and must survive a
		// concurrent catalog quarantine. Cache publication below still enforces
		// the current catalog shape, but a terminal ACK may complete the already
		// started command until its timeout.
		ackPort = strings.TrimSuffix(key.Identifier, "_Send")
		ackKey := sourceID + "\x00" + ackPort
		r.ackSequences[ackKey]++
		r.ackEpochs[ackKey] = sample.Epoch
		ackSequence = r.ackSequences[ackKey]
	}
	catalog := r.catalogs[sourceID]
	sourceNode, sourceNodeExists := r.index.sourceNode(sourceID, key.Identifier)
	if catalog.State == domain.CatalogConflict || !sourceNodeExists || sourceNode.Retired ||
		sourceNode.Class != domain.NodeClassVariable || !sampleMatchesCatalogNode(sample, sourceNode) {
		r.mu.Unlock()
		if ackPort != "" {
			r.completeWriteFromAck(sourceID, ackPort, sample, ackSequence)
		}
		return
	}
	if sample.Type == "" {
		sample.Type = sourceNode.DataType
	}
	if !r.cache.Put(sourceID, key, sample) {
		r.mu.Unlock()
		return
	}
	r.lastLowerDataAt = now
	status := currentStatus
	status.LastDataAt = now
	r.statuses[sourceID] = status
	publishSampleEvent := now.Sub(r.lastSampleEventAt) >= time.Second
	if publishSampleEvent {
		r.lastSampleEventAt = now
		r.sampleEventPending = false
	} else {
		r.sampleEventPending = true
	}
	r.mu.Unlock()
	if ackPort != "" {
		r.completeWriteFromAck(sourceID, ackPort, sample, ackSequence)
	}
	if publishSampleEvent {
		r.broker.Publish(api.Event{Type: "source.sample", Data: map[string]any{"source_id": sourceID}})
	}
}

func (r *Runtime) flushPendingSampleEvent(now time.Time) {
	r.mu.Lock()
	if !r.sampleEventPending || now.Sub(r.lastSampleEventAt) < time.Second {
		r.mu.Unlock()
		return
	}
	r.sampleEventPending = false
	r.lastSampleEventAt = now
	r.mu.Unlock()
	// A generic sample event is sufficient: Web clients invalidate the active
	// overview/source/point queries and fetch the latest coalesced state.
	r.broker.Publish(api.Event{Type: "source.sample"})
}

func (r *Runtime) isolateRejectedCatalog(sourceID string, generation uint64, discoveredAt time.Time, before map[string]domain.SourceCatalog,
	groups []domain.Group, sources []domain.Source, handler CatalogPublisher, validationErr error,
) bool {
	rejectedCatalogs := cloneCatalogMap(before)
	rejected := rejectedCatalogs[sourceID]
	rejected.SourceID = sourceID
	if source, ok := sourceByID(sources, sourceID); ok {
		rejected.GroupID = source.GroupID
	}
	rejected.State = domain.CatalogConflict
	rejected.QuarantineReason = validationErr.Error()
	rejected.DiscoveredAt = discoveredAt
	for index := range rejected.Nodes {
		rejected.Nodes[index].Retired = true
	}
	rejectedCatalogs[sourceID] = rejected
	visible := make([]domain.SourceCatalog, 0, len(rejectedCatalogs))
	for id, catalog := range rejectedCatalogs {
		if id == sourceID || catalog.State != domain.CatalogConflict {
			visible = append(visible, catalog)
		}
	}
	index, buildErr := buildCatalogIndex(visible)
	if buildErr != nil {
		slog.Error("build isolated catalog", "source_id", sourceID, "error", buildErr)
		return false
	}
	for sourceIndex := range sources {
		if sources[sourceIndex].ID == sourceID {
			sources[sourceIndex].CatalogState = domain.CatalogConflict
		}
	}
	r.mu.Lock()
	if !r.lowerCallbackCurrentLocked(sourceID, generation) {
		r.mu.Unlock()
		return false
	}
	_, wasQuarantined := r.quarantinedSources[sourceID]
	r.catalogs = rejectedCatalogs
	r.index = index
	r.quarantinedSources[sourceID] = validationErr.Error()
	r.upstreamReconcileError = validationErr.Error()
	if !wasQuarantined {
		r.conflictCount++
	}
	previousStatus, statusExisted := r.statuses[sourceID]
	status := previousStatus
	status.SourceID = sourceID
	status.Catalog = domain.CatalogConflict
	status.LastError = "catalog rejected: " + validationErr.Error()
	status.UpdatedAt = time.Now().UTC()
	r.statuses[sourceID] = status
	r.mu.Unlock()
	if err := r.routes.ApplyConfiguration(groups, sources); err != nil {
		slog.Error("isolate rejected catalog route", "source_id", sourceID, "error", err)
	}
	if !statusExisted || lowerStatusControlChanged(previousStatus, status) {
		r.persistLowerStatusTransition(previousStatus, status, statusExisted)
	}
	if err := r.repo.ReplaceSourceCatalog(context.Background(), rejected); err != nil {
		r.mu.Lock()
		r.dirtyCatalogs[sourceID] = rejected
		r.catalogPersistError = err.Error()
		r.mu.Unlock()
	} else {
		r.mu.Lock()
		delete(r.dirtyCatalogs, sourceID)
		if len(r.dirtyCatalogs) == 0 {
			r.catalogPersistError = ""
		}
		r.mu.Unlock()
	}
	if handler != nil {
		if err := handler(cloneCatalogUnion(index.union), groups, sources); err != nil {
			slog.Error("reconcile isolated catalog", "source_id", sourceID, "error", err)
			r.recordUpstreamReconcileResult(err)
		} else {
			r.recordUpstreamReconcileResult(nil)
		}
	}
	r.broker.Publish(api.Event{Type: "catalog.changed", Data: map[string]any{"source_id": sourceID, "rejected": true}})
	return true
}

func (r *Runtime) enforceCatalogQuarantine(index *catalogIndex, states map[string]domain.CatalogState, recoveringSourceID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for sourceID := range r.quarantinedSources {
		if sourceID != recoveringSourceID {
			if _, exists := states[sourceID]; exists {
				states[sourceID] = domain.CatalogConflict
				if index != nil {
					index.union.SourceStates[sourceID] = domain.CatalogConflict
				}
				count++
			}
		}
	}
	return count
}

func sampleMatchesCatalogNode(sample domain.Sample, node domain.CatalogNode) bool {
	if sample.Type != "" && sample.Type != node.DataType {
		return false
	}
	if sample.Value == nil {
		return true
	}
	valueType := reflect.TypeOf(sample.Value)
	isCollection := valueType.Kind() == reflect.Array || valueType.Kind() == reflect.Slice
	switch node.ValueRank {
	case -1: // scalar
		if isCollection {
			return false
		}
	case -2: // any rank
	case -3: // scalar or one dimension; nested collections are rejected below
	default: // one or more dimensions
		if !isCollection {
			return false
		}
	}
	for valueType.Kind() == reflect.Array || valueType.Kind() == reflect.Slice {
		valueType = valueType.Elem()
		if node.ValueRank == -3 && (valueType.Kind() == reflect.Array || valueType.Kind() == reflect.Slice) {
			return false
		}
	}
	switch node.DataType {
	case domain.DataTypeBoolean:
		return valueType.Kind() == reflect.Bool
	case domain.DataTypeUInt32:
		return valueType.Kind() == reflect.Uint32
	case domain.DataTypeFloat:
		return valueType.Kind() == reflect.Float32
	case domain.DataTypeDouble:
		return valueType.Kind() == reflect.Float64
	case domain.DataTypeString:
		return valueType.Kind() == reflect.String
	default:
		return false
	}
}

func retainRemovedNodes(previous, discovered domain.SourceCatalog) domain.SourceCatalog {
	current := make(map[string]struct{}, len(discovered.Nodes))
	for index := range discovered.Nodes {
		discovered.Nodes[index].Retired = false
		current[discovered.Nodes[index].Key.Identifier] = struct{}{}
	}
	for _, old := range previous.Nodes {
		if _, exists := current[old.Key.Identifier]; exists {
			continue
		}
		old.Retired = true
		discovered.Nodes = append(discovered.Nodes, old)
	}
	return discovered
}

func validateCatalogs(catalogs map[string]domain.SourceCatalog) (*catalogIndex, map[string]domain.CatalogState, int, error) {
	working := cloneCatalogMap(catalogs)
	for id, catalog := range working {
		// Conflict is a derived state. Re-evaluate it on every discovery so a
		// corrected point table can recover without deleting the source.
		if catalog.State == domain.CatalogConflict {
			if len(catalog.Nodes) == 0 {
				catalog.State = domain.CatalogDiscovering
			} else {
				catalog.State = domain.CatalogCached
			}
			working[id] = catalog
		}
	}
	// BuildCatalogUnion deliberately returns on the first cross-group duplicate.
	// Scan all identifiers first so every implicated source is isolated in one
	// pass instead of leaving later conflicts hidden behind the first error.
	type groupSources map[string]map[string]struct{}
	owners := make(map[string]groupSources)
	for sourceID, catalog := range working {
		for _, node := range catalog.Nodes {
			identifier := strings.TrimSpace(node.Key.Identifier)
			if identifier == "" {
				continue
			}
			if owners[identifier] == nil {
				owners[identifier] = make(groupSources)
			}
			groupID := catalog.NodeGroup(node)
			if owners[identifier][groupID] == nil {
				owners[identifier][groupID] = make(map[string]struct{})
			}
			owners[identifier][groupID][sourceID] = struct{}{}
		}
	}
	crossConflictSources := make(map[string]struct{})
	crossDetails := make([]domain.CatalogConflictDetail, 0)
	for identifier, groups := range owners {
		if len(groups) < 2 {
			continue
		}
		groupIDs := make([]string, 0, len(groups))
		sourceIDs := make([]string, 0)
		for groupID, groupSources := range groups {
			groupIDs = append(groupIDs, groupID)
			for sourceID := range groupSources {
				crossConflictSources[sourceID] = struct{}{}
				sourceIDs = append(sourceIDs, sourceID)
			}
		}
		sort.Strings(groupIDs)
		sort.Strings(sourceIDs)
		crossDetails = append(crossDetails, domain.CatalogConflictDetail{
			Kind: domain.ConflictCrossGroup, GroupID: strings.Join(groupIDs, ","), Identifier: identifier,
			SourceIDs: sourceIDs, Message: "identifier appears in more than one group",
		})
	}
	withoutCrossConflicts := make([]domain.SourceCatalog, 0, len(working))
	for sourceID, catalog := range working {
		if _, conflict := crossConflictSources[sourceID]; conflict {
			catalog.State = domain.CatalogConflict
			working[sourceID] = catalog
			continue
		}
		withoutCrossConflicts = append(withoutCrossConflicts, catalog)
	}
	union, err := domain.BuildCatalogUnion(withoutCrossConflicts)
	if err != nil {
		return nil, nil, 0, err
	}
	details := append(crossDetails, union.Conflicts...)
	sort.SliceStable(details, func(i, j int) bool {
		if details[i].Identifier != details[j].Identifier {
			return details[i].Identifier < details[j].Identifier
		}
		return details[i].Kind < details[j].Kind
	})
	for id, state := range union.SourceStates {
		catalog := working[id]
		catalog.State = state
		working[id] = catalog
	}
	selectable := make([]domain.SourceCatalog, 0, len(working))
	states := make(map[string]domain.CatalogState, len(working))
	for id, catalog := range working {
		states[id] = catalog.State
		if catalog.State != domain.CatalogConflict {
			selectable = append(selectable, catalog)
		}
	}
	index, err := buildCatalogIndex(selectable)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build catalog index: %w", err)
	}
	// Physical topology and per-source coverage are diagnostic data, not public
	// union ownership. Keep validated snapshots even when a point conflict makes
	// that source unselectable, so operators can still inspect the real binding.
	for sourceID, catalog := range working {
		if catalog.Topology != nil {
			index.union.SourceTopologies[sourceID] = catalog.Topology.Clone()
		}
		nodes := make(map[string]domain.CatalogNode, len(catalog.Nodes))
		for _, node := range catalog.Nodes {
			nodes[node.Key.Identifier] = node.Clone()
		}
		index.union.SourceNodes[sourceID] = nodes
	}
	index.union.Conflicts = make([]domain.CatalogConflictDetail, len(details))
	for i := range details {
		index.union.Conflicts[i] = details[i].Clone()
	}
	for sourceID, state := range states {
		index.union.SourceStates[sourceID] = state
	}
	return index, states, len(details), nil
}

func cloneCatalogMap(input map[string]domain.SourceCatalog) map[string]domain.SourceCatalog {
	output := make(map[string]domain.SourceCatalog, len(input))
	for id, catalog := range input {
		output[id] = catalog.Clone()
	}
	return output
}

func cloneCatalogUnion(input domain.CatalogUnion) domain.CatalogUnion {
	output := domain.CatalogUnion{
		Groups:           make(map[string]map[string]domain.CatalogNode, len(input.Groups)),
		SourceStates:     make(map[string]domain.CatalogState, len(input.SourceStates)),
		SourceTopologies: make(map[string]*domain.SourceTopology, len(input.SourceTopologies)),
		SourceNodes:      make(map[string]map[string]domain.CatalogNode, len(input.SourceNodes)),
		Conflicts:        make([]domain.CatalogConflictDetail, len(input.Conflicts)),
	}
	for groupID, nodes := range input.Groups {
		copyNodes := make(map[string]domain.CatalogNode, len(nodes))
		for identifier, node := range nodes {
			copyNodes[identifier] = node.Clone()
		}
		output.Groups[groupID] = copyNodes
	}
	for sourceID, state := range input.SourceStates {
		output.SourceStates[sourceID] = state
	}
	for sourceID, topology := range input.SourceTopologies {
		output.SourceTopologies[sourceID] = topology.Clone()
	}
	for sourceID, nodes := range input.SourceNodes {
		copyNodes := make(map[string]domain.CatalogNode, len(nodes))
		for identifier, node := range nodes {
			copyNodes[identifier] = node.Clone()
		}
		output.SourceNodes[sourceID] = copyNodes
	}
	for i := range input.Conflicts {
		output.Conflicts[i] = input.Conflicts[i].Clone()
	}
	return output
}

func catalogSlice(catalogs map[string]domain.SourceCatalog) []domain.SourceCatalog {
	result := make([]domain.SourceCatalog, 0, len(catalogs))
	for _, catalog := range catalogs {
		result = append(result, catalog)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SourceID < result[j].SourceID })
	return result
}
