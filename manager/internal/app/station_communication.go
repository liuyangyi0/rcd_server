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
	known, supported := r.stationSync.support(sourceID, status.Epoch)
	online := status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive
	return r.stations.communication(sourceID, portID, deviceID, status.Epoch,
		domain.SourceTopologyFingerprint(r.catalogs[sourceID].Topology), online, known && !supported, time.Now().UTC())
}
