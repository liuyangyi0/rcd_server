package app

import (
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

// Read-only, memory-only, and deliberately independent of Session routing.
func (r *Runtime) StationCommunication(sourceID, portID, deviceID string) domain.StationCommunication {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stationCommunicationLocked(sourceID, portID, deviceID)
}

func (r *Runtime) stationCommunicationLocked(sourceID, portID, deviceID string) domain.StationCommunication {
	status, exists := r.statuses[sourceID]
	if !exists {
		return domain.StationCommunication{}
	}
	snapshot, hasSnapshot := r.stationVersions[sourceID]
	epoch, hasEpoch := r.stationVersionEpoch[sourceID]
	port, device, hasDevice := snapshot.Device(portID, deviceID)
	fresh := hasSnapshot && hasDevice && hasEpoch && epoch == status.Epoch && !snapshot.Cached && snapshot.TopologyFingerprint == domain.SourceTopologyFingerprint(r.catalogs[sourceID].Topology)
	if featureEpoch, known := r.stationFeatureEpoch[sourceID]; known && featureEpoch == status.Epoch && !r.stationFeatureSupported[sourceID] {
		fresh = false
	}
	online := status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive
	return domain.ResolveStationCommunication(online, fresh, snapshot.ObservedAt, time.Now().UTC(), port, device)
}
