package app

import (
	"sync"
	"time"

	"rcd_manager/internal/domain"
)

type stationLocation struct{ port, device int }

// Entries are immutable after publication, including all nested slices and
// optional version pointers. Heartbeats/invalidation replace the header only.
type stationSnapshotEntry struct {
	snapshot domain.SourceStationVersions
	epoch    uint64
	live     bool
	devices  map[domain.TopologyOwner]stationLocation
}

// stationSnapshots owns the physical-source snapshots and their receipt epochs.
// Runtime coordinates commits with connection/topology state under r.mu, then
// enters this component. This component never calls Runtime or performs I/O.
// The zero value is usable.
type stationSnapshots struct {
	mu      sync.RWMutex
	entries map[string]*stationSnapshotEntry
}

func (s *stationSnapshots) replace(sourceID string, snapshot domain.SourceStationVersions, epoch uint64, live bool) {
	entry := &stationSnapshotEntry{snapshot: snapshot.Clone(), epoch: epoch, live: live,
		devices: make(map[domain.TopologyOwner]stationLocation)}
	entry.snapshot.SourceID = sourceID
	for p, port := range entry.snapshot.Ports {
		for d, device := range port.Devices {
			key := domain.TopologyOwner{PortID: port.PortID, DeviceID: device.DeviceID}
			// Match SourceStationVersions.Device's first-match behavior even for
			// malformed duplicates; normal decoded snapshots reject duplicates.
			if _, exists := entry.devices[key]; !exists {
				entry.devices[key] = stationLocation{p, d}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*stationSnapshotEntry)
	}
	s.entries[sourceID] = entry
}

func (s *stationSnapshots) entry(sourceID string) *stationSnapshotEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[sourceID]
}

func (s *stationSnapshots) read(sourceID string) (domain.SourceStationVersions, uint64, bool, bool) {
	entry := s.entry(sourceID)
	if entry == nil {
		return domain.SourceStationVersions{}, 0, false, false
	}
	// Cloning an immutable entry does not hold the component state lock.
	return entry.snapshot.Clone(), entry.epoch, entry.live, true
}

func (s *stationSnapshots) snapshot(sourceID string) (domain.SourceStationVersions, bool) {
	snapshot, _, _, exists := s.read(sourceID)
	return snapshot, exists
}

func (s *stationSnapshots) touch(sourceID string, observedAt time.Time, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[sourceID]; entry != nil {
		next := *entry
		next.snapshot.ObservedAt, next.snapshot.Cached = observedAt, false
		next.epoch, next.live = epoch, true
		s.entries[sourceID] = &next
	}
}

func (s *stationSnapshots) invalidateEpoch(sourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[sourceID]; entry != nil {
		next := *entry
		next.live = false
		s.entries[sourceID] = &next
	}
}

func (s *stationSnapshots) remove(sourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, sourceID)
}

func (s *stationSnapshots) removeOtherTopology(sourceID, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[sourceID]; entry != nil && entry.snapshot.TopologyFingerprint != fingerprint {
		delete(s.entries, sourceID)
	}
}

// A pinned view permits Runtime to capture stations alongside statuses under
// its short consistency lock, then clone the large payloads after releasing it.
type stationSnapshotView struct {
	entries map[string]*stationSnapshotEntry
}

func (s *stationSnapshots) view() stationSnapshotView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	view := stationSnapshotView{entries: make(map[string]*stationSnapshotEntry, len(s.entries))}
	for id, entry := range s.entries {
		view.entries[id] = entry
	}
	return view
}

func (v stationSnapshotView) cloneAll() map[string]domain.SourceStationVersions {
	snapshots := make(map[string]domain.SourceStationVersions, len(v.entries))
	for id, entry := range v.entries {
		snapshots[id] = entry.snapshot.Clone()
	}
	return snapshots
}

func (s *stationSnapshots) communication(sourceID, portID, deviceID string, epoch uint64, fingerprint string, online, unsupported bool, now time.Time) domain.StationCommunication {
	entry := s.entry(sourceID)
	if entry == nil {
		return domain.ResolveStationCommunication(online, false, time.Time{}, now, domain.StationVersionPort{}, domain.StationVersion{})
	}
	snapshot := entry.snapshot
	location, exists := entry.devices[domain.TopologyOwner{PortID: portID, DeviceID: deviceID}]
	fresh := exists && entry.live && entry.epoch == epoch && !snapshot.Cached && snapshot.TopologyFingerprint == fingerprint && !unsupported
	var port domain.StationVersionPort
	var device domain.StationVersion
	if exists {
		port = snapshot.Ports[location.port]
		device = port.Devices[location.device]
	}
	return domain.ResolveStationCommunication(online, fresh, snapshot.ObservedAt, now, port, device)
}
