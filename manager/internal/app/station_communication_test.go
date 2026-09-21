package app

import (
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

func TestStationCommunicationRejectsStaleEpochAndCachedHealth(t *testing.T) {
	r := &Runtime{
		statuses:            map[string]opc.LowerStatus{"a": {Connection: opc.ConnectionConnected, Session: opc.SessionActive, Epoch: 2}},
		stationVersionEpoch: map[string]uint64{"a": 2},
		stationVersions: map[string]domain.SourceStationVersions{"a": {
			ObservedAt: time.Now().UTC(), Ports: []domain.StationVersionPort{{PortID: "COM1", Enabled: true, AppliedEnabled: true, Open: true,
				Devices: []domain.StationVersion{{DeviceID: "1", ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: true}}}},
		}},
	}
	assert := func(want uint32) {
		t.Helper()
		if got := r.StationCommunication("a", "COM1", "1").State; got != want {
			t.Fatalf("state=%d want %d", got, want)
		}
	}
	assert(1)
	r.stationVersionEpoch["a"] = 1
	assert(0)
	delete(r.stationVersionEpoch, "a") // topology replacement invalidates epoch
	assert(0)
	r.stationVersionEpoch["a"] = 2
	s := r.stationVersions["a"]
	s.Cached = true
	r.stationVersions["a"] = s
	assert(0)
	s.Cached, s.ObservedAt = false, time.Now().Add(-time.Minute)
	r.stationVersions["a"] = s
	assert(0)
	r.statuses["a"] = opc.LowerStatus{Connection: opc.ConnectionDisconnected, Epoch: 2}
	assert(5)
	if r.StationCommunication("missing", "COM1", "1").State != 0 {
		t.Fatal("missing source must be unknown")
	}
}

func TestStationHealthChangesDoNotCauseVersionPersistence(t *testing.T) {
	a := domain.SourceStationVersions{Revision: "1", StationRevision: "2", Ports: []domain.StationVersionPort{{
		PortID: "COM1", Enabled: true, AppliedEnabled: true, Open: true,
		Devices: []domain.StationVersion{{DeviceID: "1", ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: true}},
	}}}
	b := a.Clone()
	b.Ports[0].Open = false
	b.Ports[0].Devices[0].CommunicationOK = false
	b.Ports[0].Devices[0].Stale = true
	if sameStationVersionSnapshot(a, b) || !samePersistedStationVersionSnapshot(a, b) {
		t.Fatal("health must update memory but not SQLite")
	}
	if !a.Ports[0].Open || !a.Ports[0].Devices[0].CommunicationOK {
		t.Fatal("comparison mutated shared snapshot")
	}
	b.StationRevision = "3"
	if samePersistedStationVersionSnapshot(a, b) {
		t.Fatal("new station version must persist")
	}
	b = a.Clone()
	b.Ports[0].Devices[0].ReadEnabled = false
	if samePersistedStationVersionSnapshot(a, b) {
		t.Fatal("configuration changes must persist")
	}
}
