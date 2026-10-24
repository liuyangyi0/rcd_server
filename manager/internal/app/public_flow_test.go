package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

func TestPublicFlowAcceptedSamplesAndSessionIsolation(t *testing.T) {
	r := seededRuntime(t)
	ctx := context.Background()
	r.OnLowerStatus(connectedStatus("source-a", 2))
	r.OnLowerStatus(connectedStatus("source-b", 2))
	if err := r.openSession("secret-session-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.openSession("secret-session-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.selectSource("secret-session-b", "line", "source-b", 1, "test"); err != nil {
		t.Fatal(err)
	}
	r.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 12.5, StatusCode: domain.StatusGood, Epoch: 2})
	r.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 99.0, StatusCode: domain.StatusGood, Epoch: 1})
	r.SetSessionViewHandler(func() map[string]api.SessionView {
		return map[string]api.SessionView{"secret-session-a": {Attached: true, Subscriptions: 1, Notifications: map[string]uint32{"10": 3}}}
	})
	view, err := r.PublicFlow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Sources[0].Samples != "1" {
		t.Fatalf("late epoch counted: %+v", view.Sources)
	}
	if len(view.Sessions) != 2 || view.Sessions[0].Routes["line"] == view.Sessions[1].Routes["line"] {
		t.Fatalf("Session routes collapsed: %+v", view.Sessions)
	}
	payload, _ := json.Marshal(view)
	if strings.Contains(string(payload), "secret-session") || strings.Contains(string(payload), "password") {
		t.Fatal("private identity exposed")
	}
	points, err := r.PublicFlowPoints(ctx, api.PageRequest{SourceID: "source-a", Limit: 1000})
	if err != nil || len(points.Items) != 1 || points.Items[0].ID != "KKS001" || string(points.Items[0].Value) != "12.5" {
		t.Fatalf("public point values: %+v %v", points, err)
	}
	r.OnLowerStatus(opc.LowerStatus{SourceID: "source-a", Epoch: 2, Connection: opc.ConnectionDisconnected})
	points, err = r.PublicFlowPoints(ctx, api.PageRequest{SourceID: "source-a"})
	if err != nil || points.Items[0].Status != uint32(domain.StatusBadNoCommunication) || string(points.Items[0].Value) != "0" {
		t.Fatalf("offline value: %+v %v", points, err)
	}
}

func TestPublicFlowStationFreshnessAndAddressSanitizing(t *testing.T) {
	r := seededRuntime(t)
	topology := &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "COM1", Devices: []domain.TopologyDevice{{DeviceID: "11", Name: "Station"}}}}}
	r.catalogs["source-a"] = domain.SourceCatalog{SourceID: "source-a", Topology: topology}
	r.OnLowerStatus(connectedStatus("source-a", 2))
	snapshot := domain.SourceStationVersions{TopologyFingerprint: domain.SourceTopologyFingerprint(topology), ObservedAt: time.Now().UTC(), Ports: []domain.StationVersionPort{{PortID: "COM1", Enabled: true, AppliedEnabled: true, Open: true, Devices: []domain.StationVersion{{DeviceID: "11", ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: true}}}}}
	check := func(want uint32) {
		t.Helper()
		v, err := r.PublicFlow(context.Background())
		if err != nil || v.Sources[0].Ports[0].Devices[0].State != want {
			t.Fatalf("want %d: %+v %v", want, v, err)
		}
	}
	r.stations.replace("source-a", snapshot, 2, true)
	check(1)
	r.stations.replace("source-a", snapshot, 1, true)
	check(0)
	snapshot.ObservedAt = time.Now().Add(-time.Minute)
	r.stations.replace("source-a", snapshot, 2, true)
	check(0)
	snapshot.ObservedAt = time.Now()
	snapshot.Ports[0].Enabled = false
	r.stations.replace("source-a", snapshot, 2, true)
	check(4)
	r.OnLowerStatus(opc.LowerStatus{SourceID: "source-a", Epoch: 2, Connection: opc.ConnectionDisconnected})
	check(5)
	if got := publicFlowAddress("opc.tcp://private:secret@192.168.10.46:4841/path?token=secret"); got != "opc.tcp://192.168.10.46:4841" {
		t.Fatal(got)
	}
}

func TestPublicFlowConcurrentSampleReads(t *testing.T) {
	r := seededRuntime(t)
	r.OnLowerStatus(connectedStatus("source-a", 1))
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				r.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: float64(n), Epoch: 1})
				if _, err := r.PublicFlow(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
