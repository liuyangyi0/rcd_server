package app

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func stationSnapshotFixture(count int) domain.SourceStationVersions {
	communication, plc := uint16(0x0104), uint16(5)
	snapshot := domain.SourceStationVersions{SourceID: "a", TopologyFingerprint: "topology", StationRevision: "1",
		ObservedAt: time.Now().UTC(), Ports: []domain.StationVersionPort{{PortID: "COM1", Enabled: true, AppliedEnabled: true, Open: true}}}
	for i := 0; i < count; i++ {
		snapshot.Ports[0].Devices = append(snapshot.Ports[0].Devices, domain.StationVersion{
			DeviceID: fmt.Sprint(i), ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: true,
			CommunicationModuleVersionRaw: &communication, PLCVersionRaw: &plc})
	}
	return snapshot
}

func TestStationSnapshotsOwnAllNestedDataAndReturnedViews(t *testing.T) {
	var cache stationSnapshots
	input := stationSnapshotFixture(2)
	cache.replace("a", input, 9, true)
	expected := input.Clone()
	input.Ports[0].Devices[0].CommunicationOK = false
	*input.Ports[0].Devices[0].CommunicationModuleVersionRaw = 999
	*input.Ports[0].Devices[0].PLCVersionRaw = 999
	input.Ports[0].PortID = "changed"
	snapshot, epoch, live, exists := cache.read("a")
	if !exists || !live || epoch != 9 || !reflect.DeepEqual(snapshot, expected) {
		t.Fatal("input aliases cache")
	}
	snapshot.Ports[0].Devices[0].DeviceID = "changed"
	*snapshot.Ports[0].Devices[0].PLCVersionRaw = 800
	view := cache.view()
	all := view.cloneAll()
	all["a"].Ports[0].Devices[0].CommunicationOK = false
	*all["a"].Ports[0].Devices[0].CommunicationModuleVersionRaw = 700
	again, _ := cache.snapshot("a")
	if !reflect.DeepEqual(again, expected) || !reflect.DeepEqual(view.cloneAll()["a"], expected) {
		t.Fatal("returned snapshot aliases cache or pinned view")
	}
}

func TestStationSnapshotsPinnedViewSurvivesHeartbeatInvalidationAndRemoval(t *testing.T) {
	var cache stationSnapshots
	input := stationSnapshotFixture(1)
	cache.replace("a", input, 1, true)
	view := cache.view()
	newTime := input.ObservedAt.Add(time.Second)
	cache.touch("a", newTime, 2)
	current, epoch, live, _ := cache.read("a")
	if !live || epoch != 2 || !current.ObservedAt.Equal(newTime) {
		t.Fatal("heartbeat lost epoch/time")
	}
	cache.invalidateEpoch("a")
	if _, _, live, _ := cache.read("a"); live {
		t.Fatal("invalidation retained live receipt")
	}
	cache.removeOtherTopology("a", input.TopologyFingerprint)
	if _, exists := cache.snapshot("a"); !exists {
		t.Fatal("same topology removed snapshot")
	}
	cache.removeOtherTopology("a", "different")
	cache.touch("a", newTime, 3)
	if _, exists := cache.snapshot("a"); exists {
		t.Fatal("heartbeat resurrected removed snapshot")
	}
	cache.replace("a", stationSnapshotFixture(2), 4, true)
	cache.remove("a")
	if !reflect.DeepEqual(view.cloneAll()["a"], input) {
		t.Fatal("pinned view changed after replacement/removal")
	}
}

func TestStationSnapshotsCommunicationPreservesQualityAndExpiryRules(t *testing.T) {
	for _, scenario := range []struct {
		name                      string
		change                    func(*domain.SourceStationVersions)
		epoch                     uint64
		fingerprint               string
		online, unsupported, live bool
		want                      uint32
	}{
		{"good", nil, 1, "topology", true, false, true, 1},
		{"failed", func(s *domain.SourceStationVersions) { s.Ports[0].Devices[0].CommunicationOK = false }, 1, "topology", true, false, true, 2},
		{"port closed", func(s *domain.SourceStationVersions) { s.Ports[0].Open = false }, 1, "topology", true, false, true, 3},
		{"disabled", func(s *domain.SourceStationVersions) { s.Ports[0].Devices[0].ReadEnabled = false }, 1, "topology", true, false, true, 4},
		{"pending", func(s *domain.SourceStationVersions) { s.Ports[0].ControlPending = true }, 1, "topology", true, false, true, 0},
		{"cached", func(s *domain.SourceStationVersions) { s.Cached = true }, 1, "topology", true, false, true, 0},
		{"expired", func(s *domain.SourceStationVersions) { s.ObservedAt = s.ObservedAt.Add(-time.Minute) }, 1, "topology", true, false, true, 0},
		{"future", func(s *domain.SourceStationVersions) { s.ObservedAt = s.ObservedAt.Add(time.Minute) }, 1, "topology", true, false, true, 0},
		{"new epoch", nil, 2, "topology", true, false, true, 0},
		{"new topology", nil, 1, "other", true, false, true, 0},
		{"invalidated", nil, 1, "topology", true, false, false, 0},
		{"unsupported", nil, 1, "topology", true, true, true, 0},
		{"offline", nil, 1, "topology", false, false, true, 5},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var cache stationSnapshots
			input := stationSnapshotFixture(1)
			now := input.ObservedAt
			if scenario.change != nil {
				scenario.change(&input)
			}
			cache.replace("a", input, 1, scenario.live)
			got := cache.communication("a", "COM1", "0", scenario.epoch, scenario.fingerprint, scenario.online, scenario.unsupported, now)
			if got.State != scenario.want || !got.ObservedAt.Equal(input.ObservedAt) {
				t.Fatalf("got %+v want %d", got, scenario.want)
			}
		})
	}
}

func TestStationSnapshotsIndexMatchesPhysicalLookup(t *testing.T) {
	var cache stationSnapshots
	input := stationSnapshotFixture(128)
	other := input.Ports[0]
	other.PortID = "COM2"
	other.Devices = append([]domain.StationVersion(nil), other.Devices...)
	for i := range other.Devices {
		other.Devices[i].CommunicationOK = false
	}
	input.Ports = append(input.Ports, other)
	// Port/device pairs must not collide by delimiter concatenation.
	for _, pair := range []domain.TopologyOwner{{PortID: "x", DeviceID: "y\x00z"}, {PortID: "x\x00y", DeviceID: "z"}} {
		port := input.Ports[0]
		port.PortID = pair.PortID
		device := port.Devices[0]
		device.DeviceID, device.CommunicationOK = pair.DeviceID, pair.PortID == "x"
		port.Devices = []domain.StationVersion{device}
		input.Ports = append(input.Ports, port)
	}
	cache.replace("a", input, 1, true)
	for _, port := range input.Ports {
		for _, device := range port.Devices {
			p, d, exists := input.Device(port.PortID, device.DeviceID)
			want := domain.ResolveStationCommunication(true, exists, input.ObservedAt, input.ObservedAt, p, d)
			if got := cache.communication("a", port.PortID, device.DeviceID, 1, "topology", true, false, input.ObservedAt); got != want {
				t.Fatalf("%q/%q got %+v want %+v", port.PortID, device.DeviceID, got, want)
			}
		}
	}
	if got := cache.communication("a", "missing", "0", 1, "topology", true, false, input.ObservedAt); got.State != 0 {
		t.Fatal("missing station reported good")
	}
	if got := cache.communication("none", "COM1", "0", 1, "topology", false, false, input.ObservedAt); got.State != 5 {
		t.Fatal("known offline source without snapshot lost offline state")
	}
}

func TestStationSnapshotsConcurrentReadersWritersAndDeletion(t *testing.T) {
	var cache stationSnapshots
	input := stationSnapshotFixture(16)
	cache.replace("a", input, 1, true)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(writer bool) {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				if writer {
					cache.replace("a", input, 1, true)
					cache.touch("a", input.ObservedAt, 1)
					cache.invalidateEpoch("a")
					cache.remove("a")
				} else {
					cache.snapshot("a")
					cache.view().cloneAll()
					got := cache.communication("a", "COM1", "15", 1, "topology", true, false, input.ObservedAt)
					if got.State != 0 && got.State != 1 {
						t.Errorf("torn snapshot: %+v", got)
					}
				}
			}
		}(worker < 2)
	}
	workers.Wait()
}

func TestStationSnapshotsRestartCacheRequiresNewConnectionReceipt(t *testing.T) {
	r := redundancyRuntime(t)
	snapshot, _ := r.stations.snapshot("source-a")
	snapshot.Revision, snapshot.StationRevision = "1", "1"
	for p := range snapshot.Ports {
		for d := range snapshot.Ports[p].Devices {
			snapshot.Ports[p].Devices[d].ConfiguredModuleType = domain.ModuleTypeUnknown
		}
	}
	if err := r.repo.SaveSourceStationVersions(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewRuntime(context.Background(), r.repo, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, live, exists := restarted.stations.read("source-a")
	if !exists || live || !loaded.Cached {
		t.Fatal("startup cache acquired live receipt")
	}
	restarted.OnLowerStatus(connectedStatus("source-a", 2))
	if got := restarted.StationCommunication("source-a", "COM1", "1"); got.State != domain.StationCommunicationUnknown {
		t.Fatalf("cache reported current good: %+v", got)
	}
	if err := restarted.cacheStationVersionResult(domain.OperationRequest{Action: "status"}, stationStatusResult(t, 1, true),
		domain.Source{ID: "source-a"}, 2, snapshot.TopologyFingerprint); err != nil {
		t.Fatal(err)
	}
	if got := restarted.StationCommunication("source-a", "COM1", "1"); got.State != domain.StationCommunicationGood {
		t.Fatalf("fresh receipt not applied: %+v", got)
	}
}

func BenchmarkStationSnapshotsCommunication(b *testing.B) {
	for _, count := range []int{16, 1024, 5300} {
		input := stationSnapshotFixture(count)
		deviceID := fmt.Sprint(count - 1)
		var cache stationSnapshots
		cache.replace("a", input, 1, true)
		b.Run(fmt.Sprintf("indexed/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			var result domain.StationCommunication
			for i := 0; i < b.N; i++ {
				result = cache.communication("a", "COM1", deviceID, 1, "topology", true, false, input.ObservedAt)
			}
			if result.State != domain.StationCommunicationGood {
				b.Fatal(result)
			}
		})
		b.Run(fmt.Sprintf("scan/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			var result domain.StationCommunication
			for i := 0; i < b.N; i++ {
				port, device, exists := input.Device("COM1", deviceID)
				result = domain.ResolveStationCommunication(true, exists, input.ObservedAt, input.ObservedAt, port, device)
			}
			if result.State != domain.StationCommunicationGood {
				b.Fatal(result)
			}
		})
	}
}
