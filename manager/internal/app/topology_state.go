package app

import (
	"log/slog"
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"strings"
	"time"
)

// lowerCallbackCurrentLocked and the corresponding state mutation must share
// r.mu. A check before acquiring the mutation lock lets a late callback poison
// a newer connection, or a source removed/replaced while it was waiting.
func (r *Runtime) lowerCallbackCurrentLocked(sourceID string, generation uint64) bool {
	if r.closing.Load() || (generation != 0 && (!r.running.Load() || r.generations[sourceID] != generation)) {
		return false
	}
	_, exists := r.catalogs[sourceID]
	return exists
}

// markCatalogQuarantineLocked requires configMu and mu. Keeping the reason in
// the immutable catalog snapshot makes it survive restart and dirty retries.
func (r *Runtime) markCatalogQuarantineLocked(sourceID, reason string) bool {
	reason = strings.TrimSpace(strings.TrimPrefix(reason, "catalog rejected: "))
	if reason == "" {
		reason = r.quarantinedSources[sourceID]
		if reason == "" {
			reason = "lower source rejected its advertised topology"
		}
	}
	catalog := r.catalogs[sourceID]
	if catalog.QuarantineReason == reason && catalog.State == domain.CatalogConflict {
		return false
	}
	catalog.State = domain.CatalogConflict
	catalog.QuarantineReason = reason
	r.catalogs[sourceID] = catalog
	r.dirtyCatalogs[sourceID] = catalog
	if _, exists := r.quarantinedSources[sourceID]; !exists {
		r.conflictCount++
	}
	r.quarantinedSources[sourceID] = reason
	if r.index != nil {
		next := *r.index
		next.union.SourceStates = make(map[string]domain.CatalogState, len(r.index.union.SourceStates))
		for id, state := range r.index.union.SourceStates {
			next.union.SourceStates[id] = state
		}
		next.union.SourceStates[sourceID] = domain.CatalogConflict
		r.index = &next
	}
	return true
}

// quarantineLowerTopologyLocked is called by catalog discovery with configMu
// held. Status callbacks use markCatalogQuarantineLocked in the same critical
// section as their epoch check, before publishing any side effects.
func (r *Runtime) quarantineLowerTopologyLocked(sourceID, reason string, generation uint64) bool {
	r.mu.Lock()
	if !r.lowerCallbackCurrentLocked(sourceID, generation) {
		r.mu.Unlock()
		return false
	}
	changed := r.markCatalogQuarantineLocked(sourceID, reason)
	previous, existed := r.statuses[sourceID]
	status := previous
	status.SourceID = sourceID
	status.Catalog = domain.CatalogConflict
	status.LastError = "catalog rejected: " + r.quarantinedSources[sourceID]
	status.UpdatedAt = time.Now().UTC()
	r.statuses[sourceID] = status
	r.mu.Unlock()
	if changed {
		r.applyQuarantinedSourceLocked(sourceID)
	}
	if !existed || lowerStatusControlChanged(previous, status) {
		r.persistLowerStatusTransition(previous, status, existed)
	}
	r.broker.Publish(api.Event{Type: "catalog.changed", SourceID: sourceID})
	return changed
}

// configMu is held, so neither source membership nor a newer discovery can
// change between the catalog mutation and route invalidation.
func (r *Runtime) applyQuarantinedSourceLocked(sourceID string) {
	groups, sources := r.routes.Groups(), r.routes.Sources()
	for i := range sources {
		if sources[i].ID == sourceID {
			sources[i].CatalogState = domain.CatalogConflict
		}
	}
	if err := r.routes.ApplyConfiguration(groups, sources); err != nil {
		slog.Error("apply source quarantine to routes", "source_id", sourceID, "error", err)
	}
}

func (r *Runtime) groupsForSources(sourceIDs ...string) []string {
	selected := make(map[string]bool, len(sourceIDs))
	for _, id := range sourceIDs {
		selected[id] = true
	}
	groups := make([]string, 0)
	for _, source := range r.routes.Sources() {
		if selected[source.ID] {
			groups = append(groups, source.GroupIDs()...)
		}
	}
	return groups
}

// Call after releasing configMu: a full refresh queue must not keep a
// configuration lock held while the upstream samples the refreshed group.
func (r *Runtime) queueGroupRefreshes(groupIDs ...string) {
	if len(groupIDs) == 0 {
		return
	}
	groups := make(map[string]bool, len(groupIDs))
	for _, groupID := range groupIDs {
		groups[groupID] = true
	}
	for _, session := range r.routes.Sessions() {
		for groupID := range groups {
			if _, exists := session.Routes[groupID]; exists {
				r.queueSessionRefresh(session.ID, groupID)
			}
		}
	}
}

func (r *Runtime) queueSourceRefreshes(sourceID string) {
	if sourceID == "" {
		return
	}
	for _, session := range r.routes.Sessions() {
		for groupID, route := range session.Routes {
			if route.SourceID == sourceID {
				r.queueSessionRefresh(session.ID, groupID)
			}
		}
	}
}

func (r *Runtime) sourceGroupsPending(sources ...domain.Source) bool {
	for _, source := range sources {
		for _, group := range source.GroupIDs() {
			if r.routes.PendingCount(group) > 0 {
				return true
			}
		}
	}
	return false
}

// Mapping edits and source/group deletion can quarantine or recover peers
// without reconnecting them. Publish derived states before the next heartbeat.
func (r *Runtime) syncCatalogStatuses() []string {
	type change struct {
		old, current opc.LowerStatus
		existed      bool
	}
	changes := make([]change, 0)
	r.mu.Lock()
	for id, catalog := range r.catalogs {
		old, existed := r.statuses[id]
		status := old
		status.SourceID = id
		status.Catalog = catalog.State
		if catalog.State == domain.CatalogConflict {
			if reason := r.quarantinedSources[id]; reason != "" {
				status.LastError = "catalog rejected: " + reason
			} else {
				status.LastError = catalogConflictSummary(conflictsForSource(r.index.union.Conflicts, id))
			}
		} else if strings.HasPrefix(status.LastError, "catalog conflict") || strings.HasPrefix(status.LastError, "catalog rejected:") {
			status.LastError = ""
		}
		if !existed || lowerStatusControlChanged(old, status) {
			status.UpdatedAt = time.Now().UTC()
			changes = append(changes, change{old: old, current: status, existed: existed})
		}
		r.statuses[id] = status
	}
	r.mu.Unlock()
	sourceIDs := make([]string, 0, len(changes))
	for _, change := range changes {
		r.persistLowerStatusTransition(change.old, change.current, change.existed)
		r.broker.Publish(api.Event{Type: "source.status", SourceID: change.current.SourceID, Data: change.current})
		sourceIDs = append(sourceIDs, change.current.SourceID)
	}
	return r.groupsForSources(sourceIDs...)
}
