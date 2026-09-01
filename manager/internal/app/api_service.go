package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func (r *Runtime) Overview(ctx context.Context) (api.Overview, error) {
	groups := r.routes.Groups()
	sources := r.routes.Sources()
	sessions := r.routes.Sessions()
	r.mu.RLock()
	index := r.index
	statuses := cloneStatuses(r.statuses)
	lastData := r.lastLowerDataAt
	conflicts := r.conflictCount
	startedAt := r.startedAt
	selectionCount := r.selectionCount
	lastSelectionAt := r.lastSelectionAt
	subscriptionCount := r.subscriptionCount
	r.mu.RUnlock()
	result := api.Overview{
		ManagerState: "Running", OPCEndpoint: r.options.OPCEndpoint,
		GroupCount: len(groups), SourceCount: len(sources), SessionCount: len(sessions),
		Version: r.options.Version, GitSHA: r.options.GitSHA, SystemTime: time.Now().UTC(),
		StartedAt: startedAt, LastLowerDataAt: lastData, CatalogConflicts: conflicts,
		RecentSelectionAt: lastSelectionAt, RecentSelectionCount: selectionCount,
	}
	if subscriptionCount != nil {
		result.SubscriptionCount = subscriptionCount()
	}
	if !r.running.Load() {
		result.ManagerState = "Stopped"
	}
	for _, status := range statuses {
		if status.Connection == opc.ConnectionConnected {
			result.ConnectedSources++
		}
	}
	if index != nil {
		for _, point := range index.points {
			if point.Node.Class != domain.NodeClassVariable {
				continue
			}
			result.PointCount++
			group := findGroup(groups, point.GroupID)
			sample := r.sampleForSource(group.DefaultSourceID, point)
			if statusIsGood(sample.StatusCode) {
				result.GoodPoints++
			} else {
				result.BadPoints++
			}
		}
	}
	for _, group := range groups {
		result.PendingCommands += r.routes.PendingCount(group.ID)
	}
	openAlarms, unacknowledged, err := r.repo.AlarmCounts(ctx)
	if err != nil {
		return api.Overview{}, err
	}
	result.OpenAlarms, result.UnacknowledgedAlarms = openAlarms, unacknowledged
	return result, nil
}

func (r *Runtime) ListGroups(_ context.Context) ([]api.GroupView, error) {
	groups := r.routes.Groups()
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	views := make([]api.GroupView, 0, len(groups))
	for _, group := range groups {
		views = append(views, r.groupView(group))
	}
	return views, nil
}

func (r *Runtime) GetGroup(_ context.Context, id string) (api.GroupView, error) {
	for _, group := range r.routes.Groups() {
		if group.ID == id {
			return r.groupView(group), nil
		}
	}
	return api.GroupView{}, domain.ErrNotFound
}

func (r *Runtime) groupView(group domain.Group) api.GroupView {
	view := api.GroupView{Group: group, SelectedSourceID: group.DefaultSourceID, SelectionRevision: group.Revision,
		MemberSourceIDs: make([]string, 0), PendingCommands: r.routes.PendingCount(group.ID), Status: "Healthy"}
	for _, source := range r.routes.Sources() {
		if source.HasGroup(group.ID) {
			view.MemberSourceIDs = append(view.MemberSourceIDs, source.ID)
		}
	}
	sort.Strings(view.MemberSourceIDs)
	view.SourceCount = len(view.MemberSourceIDs)
	r.mu.RLock()
	index := r.index
	view.LastSelectedAt = r.groupLastSelectedAt[group.ID]
	r.mu.RUnlock()
	if index != nil {
		for _, point := range index.groupPoints[group.ID] {
			if point.Node.Class != domain.NodeClassVariable {
				continue
			}
			view.TotalPoints++
			if statusIsGood(r.sampleForSource(group.DefaultSourceID, point).StatusCode) {
				view.GoodPoints++
			} else {
				view.BadPoints++
			}
		}
	}
	if view.PendingCommands > 0 {
		view.Status = "CommandPending"
	} else if view.BadPoints > 0 {
		view.Status = "Degraded"
	}
	return view
}

func (r *Runtime) CreateGroup(ctx context.Context, mutation api.GroupMutation) (api.GroupView, error) {
	if err := validateStableID("id", mutation.ID); err != nil {
		return api.GroupView{}, err
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return api.GroupView{}, domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	candidate := append(append([]domain.Group(nil), oldGroups...), domain.Group{ID: mutation.ID, Name: strings.TrimSpace(mutation.Name)})
	r.mu.RLock()
	currentIndex := r.index
	r.mu.RUnlock()
	if err := r.validateUpstream(currentIndex, candidate, oldSources); err != nil {
		return api.GroupView{}, err
	}
	var created domain.Group
	if err := r.routes.CommitConfiguration(candidate, oldSources, func() error {
		var persistErr error
		created, persistErr = r.repo.CreateGroup(ctx, domain.Group{ID: mutation.ID, Name: strings.TrimSpace(mutation.Name)})
		if persistErr == nil {
			replaceGroupInPlace(candidate, created)
		}
		return persistErr
	}); err != nil {
		return api.GroupView{}, err
	}
	r.reconcileAfterConfiguration()
	r.audit(ctx, "group.create", mutation.ID, created.Revision, nil)
	r.broker.Publish(api.Event{Type: "group.changed", Data: created})
	return r.groupView(created), nil
}

func (r *Runtime) UpdateGroup(ctx context.Context, id string, revision uint64, mutation api.GroupMutation) (api.GroupView, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return api.GroupView{}, domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	current, ok := groupByID(oldGroups, id)
	if !ok {
		return api.GroupView{}, domain.ErrNotFound
	}
	if current.Revision != revision {
		return api.GroupView{}, &domain.RevisionConflictError{Expected: revision, Actual: current.Revision}
	}
	current.Name = strings.TrimSpace(mutation.Name)
	candidate := replaceGroup(oldGroups, current)
	r.mu.RLock()
	currentIndex := r.index
	r.mu.RUnlock()
	if err := r.validateUpstream(currentIndex, candidate, oldSources); err != nil {
		return api.GroupView{}, err
	}
	var updated domain.Group
	if err := r.routes.CommitConfiguration(candidate, oldSources, func() error {
		var persistErr error
		updated, persistErr = r.repo.UpdateGroup(ctx, current, revision)
		if persistErr == nil {
			replaceGroupInPlace(candidate, updated)
		}
		return persistErr
	}); err != nil {
		return api.GroupView{}, err
	}
	r.reconcileAfterConfiguration()
	r.audit(ctx, "group.update", id, updated.Revision, nil)
	r.broker.Publish(api.Event{Type: "group.changed", Data: updated})
	return r.groupView(updated), nil
}

func (r *Runtime) DeleteGroup(ctx context.Context, id string, revision uint64) error {
	r.configMu.Lock()
	var refreshGroups []string
	defer func() {
		r.configMu.Unlock()
		r.queueGroupRefreshes(refreshGroups...)
	}()
	if r.closing.Load() {
		return domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	current, ok := groupByID(oldGroups, id)
	if !ok {
		return domain.ErrNotFound
	}
	if current.Revision != revision {
		return &domain.RevisionConflictError{Expected: revision, Actual: current.Revision}
	}
	candidate := removeGroup(oldGroups, id)
	removedSources := make([]domain.Source, 0)
	candidateSources := make([]domain.Source, 0, len(oldSources))
	for _, source := range oldSources {
		if source.HasGroup(id) && len(source.GroupBindings) > 0 {
			return fmt.Errorf("remove source %q device bindings before deleting this group: %w", source.ID, domain.ErrGroupInUse)
		}
		if source.GroupID == id {
			if err := r.checkSourceOperationLocked(source.ID); err != nil {
				return err
			}
			removedSources = append(removedSources, source)
			continue
		}
		candidateSources = append(candidateSources, source)
	}
	r.mu.RLock()
	candidateCatalogs := cloneCatalogMap(r.catalogs)
	r.mu.RUnlock()
	for _, source := range removedSources {
		delete(candidateCatalogs, source.ID)
	}
	candidateIndex, states, candidateConflictCount, err := buildCatalogIndexWithCalculations(candidateCatalogs, r.calculationPlan, r.calculationRetiredPoints, candidate)
	if err != nil {
		return err
	}
	candidateConflictCount += r.enforceCatalogQuarantine(candidateIndex, states, "")
	for sourceIndex := range candidateSources {
		if state, ok := states[candidateSources[sourceIndex].ID]; ok {
			candidateSources[sourceIndex].CatalogState = state
			catalog := candidateCatalogs[candidateSources[sourceIndex].ID]
			catalog.State = state
			candidateCatalogs[candidateSources[sourceIndex].ID] = catalog
		}
	}
	if err := r.validateUpstream(candidateIndex, candidate, candidateSources); err != nil {
		return err
	}
	if err := r.routes.CommitConfiguration(candidate, candidateSources, func() error {
		return r.repo.DeleteGroupCascade(ctx, id, revision)
	}); err != nil {
		return err
	}
	for _, source := range removedSources {
		r.stopLower(source.ID)
		r.cache.DeleteSource(source.ID)
	}
	r.mu.Lock()
	for sourceID, catalog := range candidateCatalogs {
		if r.catalogs[sourceID].State != catalog.State {
			r.dirtyCatalogs[sourceID] = catalog
		}
	}
	r.catalogs = candidateCatalogs
	r.index = candidateIndex
	r.conflictCount = candidateConflictCount
	for _, source := range removedSources {
		delete(r.statuses, source.ID)
		delete(r.dirtyCatalogs, source.ID)
		delete(r.quarantinedSources, source.ID)
		for key := range r.ackSequences {
			if strings.HasPrefix(key, source.ID+"\x00") {
				delete(r.ackSequences, key)
				delete(r.ackEpochs, key)
			}
		}
	}
	if len(r.dirtyCatalogs) == 0 {
		r.catalogPersistError = ""
	}
	r.mu.Unlock()
	for _, source := range removedSources {
		deleted := opc.LowerStatus{SourceID: source.ID}
		r.persistLowerStatusTransition(deleted, deleted, true)
	}
	refreshGroups = r.syncCatalogStatuses()
	r.reconcileAfterConfiguration()
	r.audit(ctx, "group.delete", id, revision, nil)
	r.broker.Publish(api.Event{Type: "group.changed", Data: map[string]string{"deleted_id": id}})
	return nil
}

func (r *Runtime) SetDefaultSource(ctx context.Context, groupID, sourceID string, revision uint64) (api.GroupView, error) {
	group, err := r.setDefault(ctx, groupID, sourceID, revision, "web")
	if err != nil {
		return api.GroupView{}, err
	}
	return r.groupView(group), nil
}

func (r *Runtime) setDefault(ctx context.Context, groupID, sourceID string, revision uint64, actor string) (domain.Group, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return domain.Group{}, domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	group, ok := groupByID(oldGroups, groupID)
	if !ok {
		return domain.Group{}, domain.ErrNotFound
	}
	if group.Revision != revision {
		return domain.Group{}, &domain.RevisionConflictError{Expected: revision, Actual: group.Revision}
	}
	// CommitConfiguration rejects changed routes while a command is pending,
	// but an identical default would otherwise still persist a new revision.
	// A default-source control write is locked for the entire command lifetime.
	if r.routes.PendingCount(groupID) > 0 {
		return domain.Group{}, domain.ErrGroupPending
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID != "" {
		source, exists := sourceByID(oldSources, sourceID)
		if !exists {
			return domain.Group{}, domain.ErrNotFound
		}
		if !source.HasGroup(groupID) {
			return domain.Group{}, domain.ErrSourceNotInGroup
		}
		if !source.Enabled || !source.CatalogState.Selectable() || !source.GroupAvailable(groupID) {
			return domain.Group{}, domain.ErrSourceNotSelectable
		}
	}
	group.DefaultSourceID = sourceID
	candidate := replaceGroup(oldGroups, group)
	var persisted domain.Group
	if err := r.routes.CommitConfiguration(candidate, oldSources, func() error {
		var persistErr error
		persisted, persistErr = r.repo.UpdateGroup(ctx, group, revision)
		if persistErr == nil {
			replaceGroupInPlace(candidate, persisted)
		}
		return persistErr
	}); err != nil {
		return domain.Group{}, err
	}
	r.recordSelection(groupID)
	r.audit(ctx, "group.default_source", groupID, persisted.Revision, map[string]any{"source_id": sourceID, "actor": actor})
	r.broker.Publish(api.Event{Type: "group.changed", Data: persisted})
	r.calculationRouteChanged()
	return persisted, nil
}

func (r *Runtime) ListSources(_ context.Context) ([]api.SourceView, error) {
	sources := r.routes.Sources()
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].GroupID == sources[j].GroupID {
			return sources[i].ID < sources[j].ID
		}
		return sources[i].GroupID < sources[j].GroupID
	})
	views := make([]api.SourceView, 0, len(sources))
	for _, source := range sources {
		views = append(views, r.sourceView(source))
	}
	return views, nil
}

func (r *Runtime) GetSource(_ context.Context, id string) (api.SourceView, error) {
	source, err := r.source(id)
	if err != nil {
		return api.SourceView{}, err
	}
	return r.sourceView(source), nil
}

func (r *Runtime) sourceView(source domain.Source) api.SourceView {
	view := api.SourceView{Source: source, ConnectionState: string(opc.ConnectionDisconnected),
		SessionState: string(opc.SessionInactive), SubscriptionState: string(opc.SubscriptionInactive)}
	r.mu.RLock()
	status, ok := r.statuses[source.ID]
	var conflicts []domain.CatalogConflictDetail
	if r.index != nil {
		conflicts = conflictsForSource(r.index.union.Conflicts, source.ID)
	}
	r.mu.RUnlock()
	view.CatalogConflicts = conflicts
	if ok {
		view.ConnectionState = string(status.Connection)
		view.SessionState = string(status.Session)
		view.SubscriptionState = string(status.Subscription)
		view.LastDataAt = status.LastDataAt
		view.LastProbeAt = status.LastProbeAt
		view.LastError = status.LastError
		view.ReconnectCount = status.ReconnectCount
		view.MonitoredCount = status.PointCount
	}
	if len(conflicts) > 0 && view.LastError == "" {
		view.LastError = catalogConflictSummary(conflicts)
	}
	stats := r.SourcePointStatistics(source.ID)
	view.GoodPoints, view.BadPoints, view.TotalPoints = stats.Good, stats.Bad, stats.Total
	return view
}

func conflictsForSource(conflicts []domain.CatalogConflictDetail, sourceID string) []domain.CatalogConflictDetail {
	result := make([]domain.CatalogConflictDetail, 0)
	for _, conflict := range conflicts {
		if containsString(conflict.SourceIDs, sourceID) {
			clone := conflict
			clone.SourceIDs = append([]string(nil), conflict.SourceIDs...)
			result = append(result, clone)
		}
	}
	return result
}

func catalogConflictSummary(conflicts []domain.CatalogConflictDetail) string {
	if len(conflicts) == 0 {
		return "catalog conflict"
	}
	const visible = 3
	limit := min(len(conflicts), visible)
	parts := make([]string, 0, limit+1)
	for _, conflict := range conflicts[:limit] {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", conflict.Kind, conflict.Identifier, strings.Join(conflict.SourceIDs, ",")))
	}
	if len(conflicts) > visible {
		parts = append(parts, fmt.Sprintf("and %d more", len(conflicts)-visible))
	}
	return "catalog conflict: " + strings.Join(parts, "; ")
}

func (r *Runtime) CreateSource(ctx context.Context, mutation api.SourceMutation) (api.SourceView, error) {
	if err := validateSourceMutation(mutation); err != nil {
		return api.SourceView{}, err
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return api.SourceView{}, domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	if len(oldSources) >= r.options.MaxSources {
		return api.SourceView{}, &domain.FieldError{Field: "sources", Message: fmt.Sprintf("maximum %d sources", r.options.MaxSources)}
	}
	source := sourceFromMutation(mutation)
	source.CatalogState = domain.CatalogDiscovering
	candidate := append(append([]domain.Source(nil), oldSources...), source)
	r.mu.RLock()
	candidateCatalogs := cloneCatalogMap(r.catalogs)
	r.mu.RUnlock()
	candidateCatalogs[source.ID] = domain.SourceCatalog{SourceID: source.ID, GroupID: source.GroupID, State: domain.CatalogDiscovering}
	candidateIndex, states, candidateConflictCount, err := buildCatalogIndexWithCalculations(candidateCatalogs, r.calculationPlan, r.calculationRetiredPoints, oldGroups)
	if err != nil {
		return api.SourceView{}, err
	}
	candidateConflictCount += r.enforceCatalogQuarantine(candidateIndex, states, "")
	for i := range candidate {
		if state, ok := states[candidate[i].ID]; ok {
			candidate[i].CatalogState = state
			catalog := candidateCatalogs[candidate[i].ID]
			catalog.State = state
			candidateCatalogs[candidate[i].ID] = catalog
		}
	}
	if err := r.validateUpstream(candidateIndex, oldGroups, candidate); err != nil {
		return api.SourceView{}, err
	}
	password := ""
	if mutation.Password != nil {
		password = *mutation.Password
	}
	var created domain.Source
	if err := r.routes.CommitConfiguration(oldGroups, candidate, func() error {
		var persistErr error
		created, persistErr = r.repo.CreateSource(ctx, source, password)
		if persistErr == nil {
			replaceSourceInPlace(candidate, created)
		}
		return persistErr
	}); err != nil {
		return api.SourceView{}, err
	}
	r.mu.Lock()
	r.catalogs = candidateCatalogs
	r.index = candidateIndex
	r.conflictCount = candidateConflictCount
	r.statuses[created.ID] = opc.LowerStatus{SourceID: created.ID, Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive, Catalog: domain.CatalogDiscovering, UpdatedAt: time.Now().UTC()}
	r.mu.Unlock()
	if created.Enabled && r.running.Load() {
		if err := r.startLower(created); err != nil {
			slogSourceError("start new lower source", created.ID, err)
			r.recordLowerStartFailure(created, err)
		}
	}
	r.reconcileAfterConfiguration()
	r.audit(ctx, "source.create", created.ID, created.Revision, map[string]any{"group_id": created.GroupID, "group_bindings": append([]domain.GroupBinding(nil), created.GroupBindings...)})
	r.broker.Publish(api.Event{Type: "source.status", Data: r.sourceView(created)})
	return r.sourceView(created), nil
}

func (r *Runtime) UpdateSource(ctx context.Context, id string, revision uint64, mutation api.SourceMutation) (api.SourceView, error) {
	if mutation.ID == "" {
		mutation.ID = id
	}
	if mutation.ID != id {
		return api.SourceView{}, &domain.FieldError{Field: "id", Message: "cannot be changed"}
	}
	if err := validateSourceMutation(mutation); err != nil {
		return api.SourceView{}, err
	}
	r.configMu.Lock()
	var refreshGroups []string
	defer func() {
		r.configMu.Unlock()
		r.queueGroupRefreshes(refreshGroups...)
	}()
	if r.closing.Load() {
		return api.SourceView{}, domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	current, ok := sourceByID(oldSources, id)
	if !ok {
		return api.SourceView{}, domain.ErrNotFound
	}
	if current.Revision != revision {
		return api.SourceView{}, &domain.RevisionConflictError{Expected: revision, Actual: current.Revision}
	}
	if mutation.GroupBindings == nil {
		mutation.GroupBindings = append([]domain.GroupBinding(nil), current.GroupBindings...)
	}
	if err := r.checkSourceOperationLocked(id); err != nil {
		return api.SourceView{}, err
	}
	updatedInput := sourceFromMutation(mutation)
	updatedInput.Revision = current.Revision
	updatedInput.CreatedAt = current.CreatedAt
	updatedInput.UpdatedAt = current.UpdatedAt
	updatedInput.HasPassword = current.HasPassword
	updatedInput.CatalogState = current.CatalogState
	updatedInput.AvailableGroupIDs = append([]string(nil), current.AvailableGroupIDs...)
	bindingsChanged := !domain.SameGroupBindings(current.GroupBindings, updatedInput.GroupBindings)
	r.mu.RLock()
	hasTopology := r.catalogs[id].Topology != nil
	r.mu.RUnlock()
	ownershipChanged := bindingsChanged || (current.GroupID != updatedInput.GroupID && (hasTopology || len(current.GroupBindings) > 0 || len(updatedInput.GroupBindings) > 0))
	if ownershipChanged && len(r.routes.Sessions()) > 0 {
		return api.SourceView{}, domain.ErrGroupInUse
	}
	if r.sourceGroupsPending(current, updatedInput) {
		return api.SourceView{}, domain.ErrGroupPending
	}
	resetCatalog := current.Endpoint != updatedInput.Endpoint || current.GroupID != updatedInput.GroupID ||
		current.SecurityMode != updatedInput.SecurityMode || current.SecurityPolicy != updatedInput.SecurityPolicy
	restartRequired := resetCatalog || current.Username != updatedInput.Username || current.Enabled != updatedInput.Enabled || mutation.Password != nil
	for _, group := range oldGroups {
		if group.DefaultSourceID == current.ID && (!updatedInput.Enabled || resetCatalog || !updatedInput.HasGroup(group.ID)) {
			return api.SourceView{}, fmt.Errorf("default source %q: %w", current.ID, domain.ErrSourceInUse)
		}
	}
	if resetCatalog {
		updatedInput.CatalogState = domain.CatalogDiscovering
	}
	candidate := replaceSource(oldSources, updatedInput)
	r.mu.RLock()
	candidateCatalogs := cloneCatalogMap(r.catalogs)
	currentIndex := r.index
	candidateConflictCount := r.conflictCount
	r.mu.RUnlock()
	candidateIndex := currentIndex
	if resetCatalog || bindingsChanged {
		previous := candidateCatalogs[id]
		if current.GroupID != updatedInput.GroupID {
			// Old-group nodes cannot be re-parented as tombstones in the new
			// group: doing so creates artificial cross-group duplicates. The
			// upstream reconciler already retires business nodes absent from the
			// candidate union, so a group move starts with an empty catalog.
			previous.Nodes = nil
		} else if resetCatalog {
			for nodeIndex := range previous.Nodes {
				previous.Nodes[nodeIndex].Retired = true
			}
		}
		previous.GroupID = updatedInput.GroupID
		if resetCatalog {
			previous.State = domain.CatalogDiscovering
			previous.Topology = nil
		}
		var topologyErr error
		previous, topologyErr = domain.AssignCatalogGroups(updatedInput, previous)
		if topologyErr != nil {
			return api.SourceView{}, topologyErr
		}
		updatedInput.AvailableGroupIDs = domain.AvailableGroups(updatedInput, previous)
		candidate = replaceSource(candidate, updatedInput)
		candidateCatalogs[id] = previous
		var states map[string]domain.CatalogState
		var validateErr error
		candidateIndex, states, candidateConflictCount, validateErr = buildCatalogIndexWithCalculations(candidateCatalogs, r.calculationPlan, r.calculationRetiredPoints, oldGroups)
		if validateErr != nil {
			return api.SourceView{}, validateErr
		}
		// A configuration edit is not successful rediscovery. Keep explicit
		// rejection markers even when the old connection/catalog is reset.
		candidateConflictCount += r.enforceCatalogQuarantine(candidateIndex, states, "")
		for sourceIndex := range candidate {
			if state, ok := states[candidate[sourceIndex].ID]; ok {
				candidate[sourceIndex].CatalogState = state
				catalog := candidateCatalogs[candidate[sourceIndex].ID]
				catalog.State = state
				candidateCatalogs[candidate[sourceIndex].ID] = catalog
			}
		}
	}
	if err := r.validateUpstream(candidateIndex, oldGroups, candidate); err != nil {
		return api.SourceView{}, err
	}
	var updated domain.Source
	if err := r.routes.CommitConfiguration(oldGroups, candidate, func() error {
		var persistErr error
		if resetCatalog {
			updated, persistErr = r.repo.UpdateSourceAndResetCatalog(ctx, updatedInput, mutation.Password, revision)
		} else {
			updated, persistErr = r.repo.UpdateSource(ctx, updatedInput, mutation.Password, revision)
		}
		if persistErr == nil {
			replaceSourceInPlace(candidate, updated)
		}
		return persistErr
	}); err != nil {
		return api.SourceView{}, err
	}
	// RouteTable preserves the validated in-memory CatalogState when SQLite is
	// temporarily behind a dirty catalog retry; use that authoritative copy for
	// restart, response, and SSE rather than the metadata row readback.
	updated, _ = sourceByID(r.routes.Sources(), id)
	if restartRequired {
		r.stopLower(id)
		if !updated.Enabled {
			// Alarm reconciliation uses the same durable retry queue as live
			// status transitions, so a temporary SQLite failure cannot leave a
			// disabled source alarm open forever.
			disabled := opc.LowerStatus{SourceID: id}
			r.persistLowerStatusTransition(disabled, disabled, true)
		}
	}
	if resetCatalog || bindingsChanged {
		r.mu.Lock()
		r.catalogs = candidateCatalogs
		r.index = candidateIndex
		r.conflictCount = candidateConflictCount
		delete(r.dirtyCatalogs, id)
		if (bindingsChanged && !resetCatalog) || candidateCatalogs[id].QuarantineReason != "" {
			r.dirtyCatalogs[id] = candidateCatalogs[id]
		}
		if len(r.dirtyCatalogs) == 0 {
			r.catalogPersistError = ""
		}
		r.mu.Unlock()
		if resetCatalog {
			if clearErr := r.repo.ClearSourceStationVersions(ctx, id); clearErr != nil {
				r.mu.Lock()
				r.stationPersistError = clearErr.Error()
				r.mu.Unlock()
			}
			r.mu.Lock()
			delete(r.stationVersions, id)
			delete(r.stationVersionEpoch, id)
			r.mu.Unlock()
		}
	}
	if restartRequired && updated.Enabled && r.running.Load() {
		if err := r.startLower(updated); err != nil {
			slogSourceError("restart lower source", updated.ID, err)
			r.recordLowerStartFailure(updated, err)
		}
	}
	if resetCatalog || bindingsChanged {
		refreshGroups = append(updated.GroupIDs(), r.syncCatalogStatuses()...)
	}
	r.reconcileAfterConfiguration()
	r.audit(ctx, "source.update", id, updated.Revision, map[string]any{"group_id": updated.GroupID, "group_bindings": append([]domain.GroupBinding(nil), updated.GroupBindings...)})
	r.broker.Publish(api.Event{Type: "source.status", Data: r.sourceView(updated)})
	return r.sourceView(updated), nil
}

func (r *Runtime) DeleteSource(ctx context.Context, id string, revision uint64) error {
	r.configMu.Lock()
	var refreshGroups []string
	defer func() {
		r.configMu.Unlock()
		r.queueGroupRefreshes(refreshGroups...)
	}()
	if r.closing.Load() {
		return domain.ErrShuttingDown
	}
	oldGroups, oldSources := r.routes.Groups(), r.routes.Sources()
	current, ok := sourceByID(oldSources, id)
	if !ok {
		return domain.ErrNotFound
	}
	if current.Revision != revision {
		return &domain.RevisionConflictError{Expected: revision, Actual: current.Revision}
	}
	candidate := removeSource(oldSources, id)
	if err := r.checkSourceOperationLocked(id); err != nil {
		return err
	}
	r.mu.RLock()
	candidateCatalogs := cloneCatalogMap(r.catalogs)
	r.mu.RUnlock()
	delete(candidateCatalogs, id)
	candidateIndex, states, candidateConflictCount, err := buildCatalogIndexWithCalculations(candidateCatalogs, r.calculationPlan, r.calculationRetiredPoints, oldGroups)
	if err != nil {
		return err
	}
	candidateConflictCount += r.enforceCatalogQuarantine(candidateIndex, states, "")
	for sourceIndex := range candidate {
		if state, ok := states[candidate[sourceIndex].ID]; ok {
			candidate[sourceIndex].CatalogState = state
			catalog := candidateCatalogs[candidate[sourceIndex].ID]
			catalog.State = state
			candidateCatalogs[candidate[sourceIndex].ID] = catalog
		}
	}
	if err := r.validateUpstream(candidateIndex, oldGroups, candidate); err != nil {
		return err
	}
	if err := r.routes.CommitConfiguration(oldGroups, candidate, func() error {
		return r.repo.DeleteSource(ctx, id, revision)
	}); err != nil {
		return err
	}
	r.stopLower(id)
	r.cache.DeleteSource(id)
	r.mu.Lock()
	for sourceID, catalog := range candidateCatalogs {
		if r.catalogs[sourceID].State != catalog.State {
			r.dirtyCatalogs[sourceID] = catalog
		}
	}
	r.catalogs = candidateCatalogs
	r.index = candidateIndex
	r.conflictCount = candidateConflictCount
	delete(r.statuses, id)
	delete(r.stationVersions, id)
	delete(r.stationVersionEpoch, id)
	delete(r.stationRefreshPending, id)
	delete(r.stationRefreshDirty, id)
	delete(r.stationFeatureEpoch, id)
	delete(r.stationFeatureSupported, id)
	delete(r.dirtyCatalogs, id)
	delete(r.quarantinedSources, id)
	for key := range r.ackSequences {
		if strings.HasPrefix(key, id+"\x00") {
			delete(r.ackSequences, key)
			delete(r.ackEpochs, key)
		}
	}
	if len(r.dirtyCatalogs) == 0 {
		r.catalogPersistError = ""
	}
	r.mu.Unlock()
	deleted := opc.LowerStatus{SourceID: id}
	r.persistLowerStatusTransition(deleted, deleted, true)
	refreshGroups = append(current.GroupIDs(), r.syncCatalogStatuses()...)
	r.reconcileAfterConfiguration()
	r.audit(ctx, "source.delete", id, revision, nil)
	r.broker.Publish(api.Event{Type: "source.status", Data: map[string]string{"deleted_id": id}})
	return nil
}

func (r *Runtime) ListSessions(_ context.Context) ([]api.SessionView, error) {
	sessions := r.routes.Sessions()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ActivatedAt.Before(sessions[j].ActivatedAt) })
	r.mu.RLock()
	metadataProvider := r.sessionViews
	r.mu.RUnlock()
	metadata := map[string]api.SessionView{}
	if metadataProvider != nil {
		metadata = metadataProvider()
	}
	views := make([]api.SessionView, len(sessions))
	for index := range sessions {
		views[index] = metadata[sessions[index].ID]
		serverLastSeen := views[index].Session.LastSeenAt
		views[index].Session = sessions[index]
		if !serverLastSeen.IsZero() {
			views[index].Session.LastSeenAt = serverLastSeen
		}
	}
	return views, nil
}

func (r *Runtime) recordSelection(groupID string) {
	now := time.Now().UTC()
	r.mu.Lock()
	r.selectionCount++
	r.lastSelectionAt = now
	r.groupLastSelectedAt[groupID] = now
	r.mu.Unlock()
}

func (r *Runtime) SetSessionSelection(ctx context.Context, sessionID, groupID, sourceID string, revision uint64) (domain.GroupRoute, error) {
	return r.selectSourceWithContext(ctx, sessionID, groupID, sourceID, revision, "web")
}

func (r *Runtime) ListPoints(_ context.Context, request api.PageRequest) (api.PointPage, error) {
	limit, offset := normalizePage(request.Limit, request.Offset)
	r.configMu.RLock()
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	groups := r.routes.Groups()
	var session *domain.Session
	if request.SessionID != "" {
		value, err := r.routes.Session(request.SessionID)
		if err != nil {
			r.configMu.RUnlock()
			return api.PointPage{}, err
		}
		session = &value
	}
	r.configMu.RUnlock()
	var points []indexedPoint
	if index != nil {
		// catalogIndex instances are immutable after publication, so filtering a
		// large directory does not hold the hot sample/status mutex.
		points = append(points, index.filteredPoints(request.GroupID, request.SourceID, request.Status, request.Query)...)
	}
	if request.PortID != "" || request.DeviceID != "" {
		filtered := make([]indexedPoint, 0, len(points))
		for _, point := range points {
			node := point.Node
			if request.SourceID != "" {
				if sourceNode, ok := index.sourceNode(request.SourceID, node.Key.Identifier); ok {
					node = sourceNode
				}
			}
			if request.PortID != "" && node.PortID != request.PortID {
				continue
			}
			if request.DeviceID != "" && node.DeviceID != request.DeviceID {
				continue
			}
			filtered = append(filtered, point)
		}
		points = filtered
	}
	makeView := func(point indexedPoint) api.PointView {
		if point.Node.Kind == "manager_calculation" {
			sample := r.sampleForSourceAtIndex("", point, index)
			present := true
			return api.PointView{GroupID: point.GroupID, SourceIDs: []string{}, Node: point.Node,
				SelectedSourceID: "default-routes", SourcePresent: &present, Sample: &sample}
		}
		group := findGroup(groups, point.GroupID)
		selected := group.DefaultSourceID
		if session != nil {
			selected = session.Routes[point.GroupID].SourceID
		} else if request.RouteSourceID != "" {
			selected = request.RouteSourceID
		}
		sample := r.sampleForSourceAtIndex(selected, point, index)
		copySample := sample
		present := index.sourceHasNode(selected, point.Node.Key.Identifier)
		viewNode := point.Node
		if sourceNode, ok := index.sourceNode(selected, point.Node.Key.Identifier); ok {
			viewNode = sourceNode
		} else if selected != "" {
			viewNode.PortID = ""
			viewNode.DeviceID = ""
		}
		return api.PointView{GroupID: point.GroupID, SourceIDs: append([]string(nil), point.SourceIDs...),
			Node: viewNode, SelectedSourceID: selected, SourcePresent: &present, Sample: &copySample}
	}
	if request.Status == "" {
		total := len(points)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		views := make([]api.PointView, 0, end-offset)
		for _, point := range points[offset:end] {
			views = append(views, makeView(point))
		}
		return api.PointPage{Items: views, Total: total, Limit: limit, Offset: offset}, nil
	}
	views := make([]api.PointView, 0, min(len(points), limit))
	total := 0
	for _, point := range points {
		view := makeView(point)
		if !matchesStatus(request.Status, view.Sample.StatusCode) {
			continue
		}
		if total >= offset && len(views) < limit {
			views = append(views, view)
		}
		total++
	}
	return api.PointPage{Items: views, Total: total, Limit: limit, Offset: offset}, nil
}

func (r *Runtime) ListCommands(ctx context.Context, request api.PageRequest) (api.CommandPage, error) {
	limit, offset := normalizePage(request.Limit, request.Offset)
	items, total, err := r.repo.ListCommandsFiltered(ctx, store.CommandFilter{
		GroupID: request.GroupID, SourceID: request.SourceID, Status: request.Status,
		Query: request.Query, Limit: limit, Offset: offset,
	})
	return api.CommandPage{Items: items, Total: total, Limit: limit, Offset: offset}, err
}

func (r *Runtime) Subscribe(ctx context.Context) (api.Subscription, error) {
	return r.broker.Subscribe(ctx)
}

func (r *Runtime) Health(ctx context.Context) api.HealthStatus {
	err := r.repo.Ping(ctx)
	checks := map[string]string{"database": "ok"}
	if err != nil {
		checks["database"] = err.Error()
	}
	r.mu.RLock()
	catalogError := r.catalogPersistError
	commandError := r.commandPersistError
	auditError := r.auditPersistError
	statusError := r.statusPersistError
	stationError := r.stationPersistError
	lowerLogError := r.lowerLogPersistError
	upstreamError := r.upstreamReconcileError
	r.mu.RUnlock()
	if catalogError == "" {
		checks["catalog_persistence"] = "ok"
	} else {
		checks["catalog_persistence"] = catalogError
	}
	if upstreamError == "" {
		checks["upstream_catalog"] = "ok"
	} else {
		checks["upstream_catalog"] = upstreamError
	}
	if commandError == "" {
		checks["command_persistence"] = "ok"
	} else {
		checks["command_persistence"] = commandError
	}
	if auditError == "" {
		checks["management_audit_persistence"] = "ok"
	} else {
		checks["management_audit_persistence"] = auditError
	}
	if statusError == "" {
		checks["status_history_persistence"] = "ok"
	} else {
		checks["status_history_persistence"] = statusError
	}
	if stationError == "" {
		checks["station_version_persistence"] = "ok"
	} else {
		checks["station_version_persistence"] = stationError
	}
	if lowerLogError == "" {
		checks["central_lower_log_persistence"] = "ok"
	} else {
		checks["central_lower_log_persistence"] = lowerLogError
	}
	ok := err == nil && catalogError == "" && commandError == "" && auditError == "" && statusError == "" && stationError == "" && lowerLogError == "" && upstreamError == ""
	return api.HealthStatus{OK: ok, Status: ternary(ok, "ok", "unhealthy"), Checks: checks, Time: time.Now().UTC()}
}

func (r *Runtime) Ready(ctx context.Context) api.HealthStatus {
	health := r.Health(ctx)
	health.OK = health.OK && r.ready.Load()
	if health.OK {
		health.Status = "ready"
	} else {
		health.Status = "not_ready"
	}
	return health
}

func (r *Runtime) audit(ctx context.Context, kind, target string, revision uint64, details map[string]any) {
	if details == nil {
		details = make(map[string]any)
	}
	info := api.RequestInfoFromContext(ctx)
	if info.RemoteAddress != "" {
		details["remote_address"] = info.RemoteAddress
	}
	if info.UserAgent != "" {
		details["user_agent"] = info.UserAgent
	}
	actor := requestActor(ctx)
	if explicit, ok := details["actor"].(string); ok && explicit != "" && actor == "manager" {
		actor = explicit
	}
	entry := domain.AuditEntry{Kind: kind, Actor: actor, Target: target, Revision: revision,
		Details: details, OccurredAt: time.Now().UTC()}
	persistContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err := r.repo.AppendAudit(persistContext, entry)
	cancel()
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", kind, target, revision, entry.OccurredAt.UnixNano())
	r.mu.Lock()
	if err != nil {
		r.dirtyAudits[key] = entry
		r.auditPersistError = err.Error()
	} else if len(r.dirtyAudits) == 0 {
		r.auditPersistError = ""
	}
	r.mu.Unlock()
	if err != nil {
		slog.Error("persist management audit", "kind", kind, "target", target, "error", err)
	} else {
		r.broker.Publish(api.Event{Type: "management.audit", Data: entry})
	}
}

func (r *Runtime) reconcileAfterConfiguration() {
	if err := r.reconcileUpstream(); err != nil {
		slog.Error("reconcile upstream after configuration change", "error", err)
	}
	r.requestCalculationFull()
}

func (r *Runtime) source(id string) (domain.Source, error) {
	if source, ok := sourceByID(r.routes.Sources(), id); ok {
		return source, nil
	}
	return domain.Source{}, domain.ErrNotFound
}

func (r *Runtime) rebuildIndexLocked() {
	groups := r.routes.Groups()
	index, states, conflicts, err := buildCatalogIndexWithCalculations(r.catalogs, r.calculationPlan, r.calculationRetiredPoints, groups)
	if err != nil {
		return
	}
	r.index, r.conflictCount = index, conflicts
	sources := r.routes.Sources()
	for i := range sources {
		if state, ok := states[sources[i].ID]; ok {
			sources[i].CatalogState = state
		}
	}
	_ = r.routes.ApplyConfiguration(groups, sources)
}

func validateSourceMutation(mutation api.SourceMutation) error {
	if err := domain.ValidateGroupBindings(mutation.GroupBindings); err != nil {
		return err
	}
	for _, binding := range mutation.GroupBindings {
		if err := validateStableID("group_bindings.group_id", binding.GroupID); err != nil {
			return err
		}
	}
	if err := validateStableID("id", mutation.ID); err != nil {
		return err
	}
	if err := validateStableID("group_id", mutation.GroupID); err != nil {
		return err
	}
	if strings.TrimSpace(mutation.Name) == "" {
		return &domain.FieldError{Field: "name", Message: "must not be empty"}
	}
	endpoint, err := url.Parse(mutation.Endpoint)
	if err != nil || endpoint.Scheme != "opc.tcp" || endpoint.Host == "" {
		return &domain.FieldError{Field: "endpoint", Message: "must be an opc.tcp URL"}
	}
	policy, mode := normalizedSecurity(mutation.SecurityPolicy, mutation.SecurityMode)
	if !((policy == "None" && mode == "None") || (policy == "Basic256Sha256" && mode == "SignAndEncrypt")) {
		return &domain.FieldError{Field: "security", Message: "supported combinations are None/None and Basic256Sha256/SignAndEncrypt"}
	}
	return nil
}

func sourceFromMutation(mutation api.SourceMutation) domain.Source {
	policy, mode := normalizedSecurity(mutation.SecurityPolicy, mutation.SecurityMode)
	return domain.Source{ID: strings.TrimSpace(mutation.ID), GroupID: strings.TrimSpace(mutation.GroupID),
		GroupBindings: append([]domain.GroupBinding(nil), mutation.GroupBindings...),
		Name:          strings.TrimSpace(mutation.Name), Endpoint: strings.TrimSpace(mutation.Endpoint), SecurityPolicy: policy,
		SecurityMode: mode, Username: strings.TrimSpace(mutation.Username), Enabled: mutation.Enabled}
}

func normalizedSecurity(policy, mode string) (string, string) {
	policy = strings.TrimSpace(strings.TrimPrefix(policy, "http://opcfoundation.org/UA/SecurityPolicy#"))
	mode = strings.TrimSpace(mode)
	if policy == "" {
		policy = "None"
	}
	if mode == "" {
		mode = "None"
	}
	return policy, mode
}

func validateStableID(field, id string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 64 {
		return &domain.FieldError{Field: field, Message: "must contain 1-64 characters"}
	}
	for _, character := range id {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return &domain.FieldError{Field: field, Message: "only letters, digits, dot, underscore and hyphen are allowed"}
	}
	return nil
}

func normalizePage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func matchesStatus(filter string, status domain.StatusCode) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "good" {
		return statusIsGood(status)
	}
	if filter == "bad" {
		return !statusIsGood(status)
	}
	return strings.EqualFold(filter, fmt.Sprintf("0x%08X", uint32(status)))
}

func statusIsGood(status domain.StatusCode) bool { return uint32(status)&0xC0000000 == 0 }

func cloneStatuses(input map[string]opc.LowerStatus) map[string]opc.LowerStatus {
	output := make(map[string]opc.LowerStatus, len(input))
	for id, status := range input {
		output[id] = status
	}
	return output
}

func findGroup(groups []domain.Group, id string) domain.Group {
	group, _ := groupByID(groups, id)
	return group
}

func groupByID(groups []domain.Group, id string) (domain.Group, bool) {
	for _, group := range groups {
		if group.ID == id {
			return group, true
		}
	}
	return domain.Group{}, false
}

func sourceByID(sources []domain.Source, id string) (domain.Source, bool) {
	for _, source := range sources {
		if source.ID == id {
			return source, true
		}
	}
	return domain.Source{}, false
}

func replaceGroup(groups []domain.Group, replacement domain.Group) []domain.Group {
	result := append([]domain.Group(nil), groups...)
	for index := range result {
		if result[index].ID == replacement.ID {
			result[index] = replacement
			return result
		}
	}
	return append(result, replacement)
}

func replaceGroupInPlace(groups []domain.Group, replacement domain.Group) {
	for index := range groups {
		if groups[index].ID == replacement.ID {
			groups[index] = replacement
			return
		}
	}
}

func removeGroup(groups []domain.Group, id string) []domain.Group {
	result := make([]domain.Group, 0, len(groups))
	for _, group := range groups {
		if group.ID != id {
			result = append(result, group)
		}
	}
	return result
}

func replaceSource(sources []domain.Source, replacement domain.Source) []domain.Source {
	result := append([]domain.Source(nil), sources...)
	for index := range result {
		if result[index].ID == replacement.ID {
			result[index] = replacement
			return result
		}
	}
	return append(result, replacement)
}

func replaceSourceInPlace(sources []domain.Source, replacement domain.Source) {
	for index := range sources {
		if sources[index].ID == replacement.ID {
			sources[index] = replacement
			return
		}
	}
}

func removeSource(sources []domain.Source, id string) []domain.Source {
	result := make([]domain.Source, 0, len(sources))
	for _, source := range sources {
		if source.ID != id {
			result = append(result, source)
		}
	}
	return result
}

func ternary[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}

func slogSourceError(message, sourceID string, err error) {
	if err != nil {
		// Kept in a helper so CRUD paths stay readable and credentials are never logged.
		slog.Error(message, "source_id", sourceID, "error", err)
	}
}
