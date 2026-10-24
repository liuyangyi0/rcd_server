package app

import (
	"context"
	"rcd_manager/internal/api"
	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"strings"
	"testing"
	"time"
)

const stateConfig = `[[state_inputs]]
name="FIXED"
mode="fixed"
[[state_inputs.targets]]
source_id="source-a"
port_id="COM1"
device_id="11"
[[state_inputs]]
name="DEFAULT"
mode="group_default"
group_id="line"
[[state_inputs.targets]]
source_id="source-a"
port_id="COM1"
device_id="11"
[[state_inputs.targets]]
source_id="source-b"
port_id="COM2"
device_id="12"
[[rules]]
kks_calc="FIXED_VALUE"
data_type="UInt32"
expression="FIXED"
[[rules]]
kks_calc="DEFAULT_VALUE"
data_type="UInt32"
expression="DEFAULT"
[[rules]]
kks_calc="ALARM"
data_type="UInt32"
expression="IF(DEFAULT == 1, 0, 1)"
`

func TestStateTargetDiagnosticsDistinguishIdentityAndTopology(t *testing.T) {
	r := stateRuntime(t)
	for _, test := range []struct{ source, port, device, want string }{
		{"missing", "COM1", "11", "来源ID不存在: missing"},
		{"source-a", "missing", "11", "串口不存在"},
		{"source-a", "COM1", "99", "站点不存在"},
	} {
		input := calculation.StateInput{Name: "CHECK", Mode: "fixed", Targets: []calculation.StateTarget{{SourceID: test.source, PortID: test.port, DeviceID: test.device}}}
		err := validateStateTargets([]calculation.StateInput{input}, r.catalogs, r.routes.Groups(), r.routes.Sources())
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("want %s got %v", test.want, err)
		}
		view := r.stateInputValue(input, r.routes.Groups(), r.routes.Sources())
		if view.StatusCode != domain.StatusBadConfigurationError || !strings.Contains(view.Error, test.want) {
			t.Fatal(view)
		}
	}
	if message := stateTargetProblem(calculation.StateTarget{SourceID: "source-a", PortID: "COM1", DeviceID: "11"}, nil); !strings.Contains(message, "尚无有效拓扑") {
		t.Fatal(message)
	}
}

func stateRuntime(t *testing.T) *Runtime {
	r := seededRuntime(t)
	for i, id := range []string{"source-a", "source-b"} {
		port, device := "COM1", "11"
		if i == 1 {
			port, device = "COM2", "12"
		}
		r.OnLowerStatus(connectedStatus(id, 1))
		r.mu.Lock()
		c := r.catalogs[id]
		c.Topology = &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: port, Devices: []domain.TopologyDevice{{DeviceID: device}}}}}
		r.catalogs[id] = c
		r.stations.replace(id, domain.SourceStationVersions{TopologyFingerprint: domain.SourceTopologyFingerprint(c.Topology), ObservedAt: time.Now().UTC(), Ports: []domain.StationVersionPort{{PortID: port, Enabled: true, AppliedEnabled: true, Open: true, Devices: []domain.StationVersion{{DeviceID: device, ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: i == 0}}}}}, 1, true)
		r.mu.Unlock()
	}
	return r
}

func TestCalculationStateRoutingAndOffline(t *testing.T) {
	r := stateRuntime(t)
	activateTestCalculation(t, r, stateConfig)
	r.runCalculationBatch()
	check := func(name string, want uint32) {
		t.Helper()
		r.mu.RLock()
		got, _ := r.calculations.result(name)
		r.mu.RUnlock()
		if got.Value != want || got.StatusCode != domain.StatusGood {
			t.Fatalf("%s: %#v want %d", name, got, want)
		}
	}
	check("DEFAULT_VALUE", 1)
	check("FIXED_VALUE", 1)
	check("ALARM", 0)
	group := r.routes.Groups()[0]
	if _, err := r.SetDefaultSource(context.Background(), "line", "source-b", group.Revision); err != nil {
		t.Fatal(err)
	}
	r.runCalculationBatch()
	check("DEFAULT_VALUE", 2)
	check("FIXED_VALUE", 1)
	check("ALARM", 1)
	if err := r.openSession("state-test"); err != nil {
		t.Fatal(err)
	}
	session, _ := r.routes.Session("state-test")
	if _, err := r.selectSource("state-test", "line", "source-a", session.Routes["line"].Revision, "test"); err != nil {
		t.Fatal(err)
	}
	r.runCalculationBatch()
	check("DEFAULT_VALUE", 2)
	r.OnLowerStatus(opc.LowerStatus{SourceID: "source-b", Connection: opc.ConnectionDisconnected, Session: opc.SessionInactive, Epoch: 1, Catalog: domain.CatalogReady})
	r.runCalculationBatch()
	check("DEFAULT_VALUE", 5)
	check("ALARM", 1)
	r.OnLowerStatus(connectedStatus("source-b", 2))
	r.runCalculationBatch()
	check("DEFAULT_VALUE", 0)
}

func TestCalculationStateExpiryValidationAndPaging(t *testing.T) {
	r := stateRuntime(t)
	activateTestCalculation(t, r, stateConfig)
	r.runCalculationBatch()
	r.refreshCalculationStateInputs("")
	r.runCalculationBatch()
	r.mu.Lock()
	s, _ := r.stations.snapshot("source-a")
	s.ObservedAt = time.Now().Add(-time.Minute)
	r.stations.replace("source-a", s, 1, true)
	r.mu.Unlock()
	r.refreshCalculationStateInputs("source-a")
	r.runCalculationBatch()
	r.mu.RLock()
	got, _ := r.calculations.result("FIXED_VALUE")
	r.mu.RUnlock()
	if got.Value != uint32(0) || got.StatusCode != domain.StatusGood {
		t.Fatal(got)
	}
	page, err := r.ListCalculationStateInputs(context.Background(), api.PageRequest{Limit: 1}, "active", "", "")
	if err != nil || page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	candidates, err := r.ListCalculationStateCandidates(context.Background(), api.PageRequest{Limit: 10}, "line", "")
	if err != nil || candidates.Total != 2 {
		t.Fatal(candidates, err)
	}
	bad := strings.Replace(stateConfig, `source_id="source-b"`, `source_id="missing"`, 1)
	result, err := r.ValidateCalculation(context.Background(), &bad)
	if err != nil || result.Valid {
		t.Fatal(result, err)
	}
	r.mu.Lock()
	c := r.catalogs["source-a"]
	c.Topology.Ports = nil
	r.catalogs["source-a"] = c
	r.mu.Unlock()
	r.requestCalculationFull()
	r.runCalculationBatch()
	r.mu.RLock()
	got, _ = r.calculations.result("FIXED_VALUE")
	r.mu.RUnlock()
	if got.StatusCode != domain.StatusBadConfigurationError {
		t.Fatal(got)
	}
}

func TestStateInputRuntimeConfigZeroOnMissingMapping(t *testing.T) {
	r := stateRuntime(t)
	config, _ := calculation.ParseConfig(stateConfig)
	input := config.StateInputs[1]
	input.Targets = input.Targets[1:]
	got := r.stateInputValue(input, r.routes.Groups(), r.routes.Sources())
	if got.StatusCode != domain.StatusBadConfigurationError || got.Error == "" {
		t.Fatal(got)
	}
}

func TestStateInputHeartbeatDoesNotDirtyCalculations(t *testing.T) {
	r := stateRuntime(t)
	activateTestCalculation(t, r, stateConfig)
	r.refreshCalculationStateInputs("")
	r.runCalculationBatch()
	r.flushCalculationStateEvent()
	r.mu.Lock()
	snapshot, _ := r.stations.snapshot("source-a")
	snapshot.ObservedAt = time.Now().UTC()
	r.stations.replace("source-a", snapshot, 1, true)
	r.mu.Unlock()
	r.refreshCalculationStateInputs("source-a")
	r.mu.RLock()
	dirty, pending := r.calculations.pendingWork()
	r.mu.RUnlock()
	if dirty != 0 || pending {
		t.Fatalf("heartbeat caused recompute/event: dirty=%d pending=%v", dirty, pending)
	}
	r.mu.Lock()
	snapshot.Ports[0].Devices[0].CommunicationOK = false
	r.stations.replace("source-a", snapshot, 1, true)
	r.mu.Unlock()
	r.refreshCalculationStateInputs("source-a")
	r.mu.RLock()
	dirty, pending = r.calculations.pendingWork()
	r.mu.RUnlock()
	if dirty != 2 || !pending {
		t.Fatalf("state transition missing: dirty=%d pending=%v", dirty, pending)
	}
	r.runCalculationBatch()
	r.mu.RLock()
	result, _ := r.calculations.result("ALARM")
	r.mu.RUnlock()
	if result.Value != uint32(1) || result.StatusCode != domain.StatusGood {
		t.Fatal(result)
	}
}

func TestCalculationDigitLeadingFixedStateInput(t *testing.T) {
	const name = "9CYE91GH201_COM1"
	r := stateRuntime(t)
	content := strings.Replace(stateConfig, `name="FIXED"`, `name="`+name+`"`, 1)
	content = strings.Replace(content, `expression="FIXED"`, `expression="`+name+`"`, 1)
	activateTestCalculation(t, r, content)
	r.runCalculationBatch()
	check := func(want uint32) {
		t.Helper()
		got := r.ReadSample("digit-leading-test", testKey("FIXED_VALUE"))
		if got.Value != want || got.StatusCode != domain.StatusGood {
			t.Fatalf("fixed state input result=%#v want=%d/Good", got, want)
		}
	}
	check(1)
	page, err := r.ListCalculationStateInputs(context.Background(), api.PageRequest{Limit: 10, Query: name}, "active", "fixed", "source-a")
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Name != name {
		t.Fatalf("active binding missing: %#v err=%v", page, err)
	}
	r.OnLowerStatus(opc.LowerStatus{SourceID: "source-a", Connection: opc.ConnectionDisconnected, Session: opc.SessionInactive, Epoch: 1, Catalog: domain.CatalogReady})
	r.runCalculationBatch()
	check(5)
}
