package app

import (
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"strings"
)

func (r *Runtime) quarantineLowerTopology(status opc.LowerStatus, generation uint64) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	r.mu.RLock()
	current := r.generations[status.SourceID]
	r.mu.RUnlock()
	if r.closing.Load() || (generation != 0 && current != generation) {
		return
	}
	r.quarantineLowerTopologyLocked(status.SourceID, status.LastError)
}

func (r *Runtime) quarantineLowerTopologyLocked(sourceID, reason string) {
	groups, sources := r.routes.Groups(), r.routes.Sources()
	found := false
	for i := range sources {
		if sources[i].ID == sourceID {
			sources[i].CatalogState = domain.CatalogConflict
			found = true
		}
	}
	if !found {
		return
	}
	r.mu.Lock()
	catalog := r.catalogs[sourceID]
	catalog.State = domain.CatalogConflict
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
	previous, existed := r.statuses[sourceID]
	status := previous
	status.Catalog = domain.CatalogConflict
	status.LastError = "catalog rejected: " + reason
	r.statuses[sourceID] = status
	r.mu.Unlock()
	_ = r.routes.ApplyConfiguration(groups, sources)
	if !existed || lowerStatusControlChanged(previous, status) {
		r.persistLowerStatusTransition(previous, status, existed)
	}
	r.broker.Publish(api.Event{Type: "catalog.changed", SourceID: sourceID})
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

// Mapping edits can quarantine or recover peer sources without reconnecting
// them. Publish the new derived states immediately, not at the next heartbeat.
func (r *Runtime) syncCatalogStatuses() {
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
		} else if strings.HasPrefix(status.LastError, "catalog conflict") {
			status.LastError = ""
		}
		r.statuses[id] = status
		if !existed || lowerStatusControlChanged(old, status) {
			changes = append(changes, change{old: old, current: status, existed: existed})
		}
	}
	r.mu.Unlock()
	for _, change := range changes {
		r.persistLowerStatusTransition(change.old, change.current, change.existed)
	}
}
