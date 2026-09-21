package app

import (
	"context"
	"errors"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
	"sync"
	"testing"
	"time"
)

func redundancyRuntime(t *testing.T) *Runtime {
	t.Helper()
	r := seededRuntime(t)
	for _, id := range []string{"source-a", "source-b"} {
		r.OnLowerStatus(connectedStatus(id, 1))
		topology := &domain.SourceTopology{SchemaVersion: 1, Ports: []domain.TopologyPort{{PortID: "COM1", PortName: "COM1", SendNodeID: "COM1_Send", SendMethodID: "COM1_SendCommand", Devices: []domain.TopologyDevice{{DeviceID: "1", DeviceAddress: 1, NodeIDs: []string{"KKS001", "KKS002"}}}}}}
		r.OnCatalog(domain.SourceCatalog{SourceID: id, GroupID: "line", State: domain.CatalogReady, DiscoveredAt: time.Now(), Topology: topology, Nodes: []domain.CatalogNode{testNode("KKS001"), testNode("KKS002"), testStringNode("COM1_Send"), testMethodNode("COM1_SendCommand")}})
		r.mu.Lock()
		r.stationVersions[id] = domain.SourceStationVersions{ObservedAt: time.Now().UTC(), TopologyFingerprint: domain.SourceTopologyFingerprint(r.catalogs[id].Topology), Ports: []domain.StationVersionPort{{PortID: "COM1", Enabled: true, AppliedEnabled: true, Open: true, Devices: []domain.StationVersion{{DeviceID: "1", ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: true}}}}}
		r.stationVersionEpoch[id] = 1
		r.mu.Unlock()
		for _, node := range []string{"KKS001", "KKS002"} {
			r.OnSample(id, testKey(node), domain.Sample{Type: domain.DataTypeDouble, Value: 1.0, Epoch: 1, SourceTimestamp: time.Now().UTC()})
		}
	}
	return r
}
func saveRedundancyTest(t *testing.T, r *Runtime, defaults, compare bool) {
	t.Helper()
	p := domain.DefaultRedundancyPolicy("line")
	p.Enabled = true
	p.DefaultAutomatic = defaults
	p.Priority = []string{"source-a", "source-b"}
	p.FailureSeconds = 1
	p.HealthySeconds = 1
	p.CooldownSeconds = 2
	p.DifferenceSeconds = 1
	p.CompareEnabled = compare
	p.LeftSourceID = "source-a"
	p.RightSourceID = "source-b"
	if _, err := r.SaveRedundancy(context.Background(), "line", 1, p); err != nil {
		t.Fatal(err)
	}
}
func setStationGood(r *Runtime, id string, good bool) {
	r.mu.Lock()
	v := r.stationVersions[id].Clone()
	v.Ports[0].Devices[0].CommunicationOK = good
	v.ObservedAt = time.Now().UTC()
	r.stationVersions[id] = v
	r.mu.Unlock()
}

func TestRedundancySwitchDelayManualIsolationAndPending(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, false)
	ctx := context.Background()
	r.openSession("auto")
	r.openSession("manual")
	if _, err := r.SetSessionAutomatic(ctx, "manual", "line", false, 1); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	r.redundancyTick(ctx, at)
	setStationGood(r, "source-a", false)
	r.redundancyTick(ctx, at.Add(time.Second))
	if route, _ := r.routes.Route("auto", "line"); route.SourceID != "source-a" {
		t.Fatal("switched before failure confirmation")
	}
	pending, err := r.routes.BeginCommand("auto", "line", "COM1", "Write")
	if err != nil {
		t.Fatal(err)
	}
	r.redundancyTick(ctx, at.Add(3*time.Second))
	route, _ := r.routes.Route("auto", "line")
	if route.SourceID != "source-a" {
		t.Fatal("pending fence bypassed")
	}
	r.routes.FinishCommand(pending.ID, domain.CommandFailed, domain.StatusBadTimeout, "test")
	r.redundancyTick(ctx, at.Add(4*time.Second))
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" || !route.Automatic {
		v, _ := r.Redundancy(ctx, "line")
		t.Fatal(route, v)
	}
	manual, _ := r.routes.Route("manual", "line")
	if manual.SourceID != "source-a" {
		t.Fatal("manual session switched")
	}
	if r.routes.Groups()[0].DefaultSourceID != "source-a" {
		t.Fatal("default unexpectedly switched")
	}
	r.openSession("auto")
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal("reattach lost route")
	}
	setStationGood(r, "source-a", true)
	r.redundancyTick(ctx, at.Add(10*time.Second))
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal("automatic failback")
	}
	if _, err = r.selectSource("auto", "line", "source-a", route.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	route, _ = r.routes.Route("auto", "line")
	if route.Automatic {
		t.Fatal("manual write did not opt out")
	}
}
func TestRedundancyDefaultSwitchAndPersistence(t *testing.T) {
	r := redundancyRuntime(t)
	activateTestCalculation(t, r, "[[rules]]\nkks_calc = \"CENTRAL\"\ndata_type = \"double\"\nexpression = \"KKS001 * 2\"\n")
	before := r.calculationRouteRevision
	saveRedundancyTest(t, r, true, false)
	ctx := context.Background()
	at := time.Now()
	r.redundancyTick(ctx, at)
	setStationGood(r, "source-a", false)
	r.redundancyTick(ctx, at.Add(time.Second))
	r.redundancyTick(ctx, at.Add(3*time.Second))
	if r.routes.Groups()[0].DefaultSourceID != "source-b" {
		v, _ := r.Redundancy(ctx, "line")
		t.Fatal(v)
	}
	groups, err := r.repo.ListGroups(ctx)
	if err != nil || groups[0].DefaultSourceID != "source-b" {
		t.Fatal(groups, err)
	}
	if r.calculationRouteRevision <= before {
		t.Fatal("calculation route revision not advanced")
	}
	loaded, err := NewRuntime(ctx, r.repo, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := loaded.Redundancy(ctx, "line")
	if err != nil || !v.Policy.Enabled || !v.Policy.DefaultAutomatic {
		t.Fatal(v, err)
	}
	if _, err = r.SetSessionAutomatic(ctx, "closed", "line", true, 1); !errors.Is(err, domain.ErrSessionClosed) {
		t.Fatal(err)
	}
}
func TestRedundancyDifferencesPersistWithoutChurnAndRecover(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, true)
	ctx := context.Background()
	at := time.Now().UTC()
	r.OnSample("source-b", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 9.0, Epoch: 1, SourceTimestamp: at})
	r.redundancyTick(ctx, at)
	r.redundancyTick(ctx, at.Add(2*time.Second))
	v, _ := r.Redundancy(ctx, "line")
	if v.DifferenceCount != 1 || v.ComparisonState != "Different" {
		t.Fatal(v)
	}
	alarms, _, err := r.repo.ListAlarms(ctx, store.AlarmFilter{Query: "data_difference", State: "Open"})
	if err != nil || len(alarms) != 1 {
		t.Fatal(alarms, err)
	}
	revision := alarms[0].Revision
	r.redundancyTick(ctx, at.Add(3*time.Second))
	alarms, _, _ = r.repo.ListAlarms(ctx, store.AlarmFilter{Query: "data_difference", State: "Open"})
	if alarms[0].Revision != revision {
		t.Fatal("unchanged alarm rewritten")
	}
	setStationGood(r, "source-b", false)
	r.redundancyTick(ctx, at.Add(4*time.Second))
	alarms, _, _ = r.repo.ListAlarms(ctx, store.AlarmFilter{Query: "data_difference", State: "Open"})
	if len(alarms) != 1 {
		t.Fatal("unavailable falsely resolved alarm")
	}
	setStationGood(r, "source-b", true)
	r.OnSample("source-b", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: 1.0, Epoch: 1, SourceTimestamp: at})
	r.redundancyTick(ctx, at.Add(5*time.Second))
	alarms, _, _ = r.repo.ListAlarms(ctx, store.AlarmFilter{Query: "data_difference", State: "Open"})
	if len(alarms) != 0 {
		t.Fatal("equal samples did not resolve")
	}
}
func TestRedundancyRejectsStaleAndIncompleteCandidates(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, false)
	ctx := context.Background()
	at := time.Now()
	r.openSession("auto")
	setStationGood(r, "source-a", false)
	r.mu.Lock()
	v := r.stationVersions["source-b"].Clone()
	v.ObservedAt = at.Add(-time.Minute)
	r.stationVersions["source-b"] = v
	r.mu.Unlock()
	r.redundancyTick(ctx, at)
	r.redundancyTick(ctx, at.Add(3*time.Second))
	route, _ := r.routes.Route("auto", "line")
	if route.SourceID != "source-a" {
		t.Fatal("stale candidate selected")
	}
	p := r.redundancyPolicyLocked("line")
	if _, err := r.SaveRedundancy(ctx, "line", 1, p); err == nil {
		t.Fatal("stale policy accepted")
	}
}

func TestRedundancyCooldownEpochAndNoHealthyCandidate(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, false)
	ctx := context.Background()
	r.openSession("auto")
	at := time.Now()
	r.redundancyTick(ctx, at)
	setStationGood(r, "source-a", false)
	r.redundancyTick(ctx, at.Add(time.Second))
	r.redundancyTick(ctx, at.Add(2*time.Second))
	route, _ := r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal(route)
	}
	setStationGood(r, "source-b", false)
	r.redundancyTick(ctx, at.Add(3*time.Second))
	r.redundancyTick(ctx, at.Add(4*time.Second))
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal("switched with no healthy backup")
	}
	setStationGood(r, "source-a", true)
	r.redundancyTick(ctx, at.Add(5*time.Second))
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal("unqualified recovery selected")
	}
	r.mu.Lock()
	status := r.statuses["source-a"]
	status.Epoch = 2
	r.statuses["source-a"] = status
	r.mu.Unlock()
	r.redundancyTick(ctx, at.Add(6*time.Second))
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal("old epoch samples selected")
	}
}
func TestRedundancyBackupRestoresParametersButNotAutomaticAuthority(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, true, true)
	doc, err := r.ExportConfiguration(context.Background())
	if err != nil || len(doc.Redundancy) != 1 {
		t.Fatal(doc, err)
	}
	policies, err := validateRedundancyImport(doc.Redundancy, r.routes.Groups(), r.routes.Sources())
	if err != nil || len(policies) != 1 || policies[0].Enabled || policies[0].DefaultAutomatic || !policies[0].CompareEnabled {
		t.Fatal(policies, err)
	}
	if !doc.Redundancy[0].Enabled {
		t.Fatal("export mutated")
	}
	doc.Redundancy[0].Priority[0] = "missing"
	if _, err = validateRedundancyImport(doc.Redundancy, r.routes.Groups(), r.routes.Sources()); err == nil {
		t.Fatal("invalid source accepted")
	}
}
func TestRedundancyAlarmSurvivesRestartAndGroupDeletion(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, true)
	ctx := context.Background()
	p := r.redundancyPolicyLocked("line")
	if _, _, err := r.repo.ReconcileAlarm(ctx, "", p.DifferenceKind(), domain.AlarmSeverityWarning, "existing difference", true); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRuntime(ctx, r.repo, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	restored.redundancyTick(ctx, time.Now())
	v, _ := restored.Redundancy(ctx, "line")
	if !v.DifferenceAlarmActive || v.ComparisonState != "Unknown" {
		t.Fatal(v)
	}
	group := restored.routes.Groups()[0]
	if err = restored.DeleteGroup(ctx, group.ID, group.Revision); err != nil {
		t.Fatal(err)
	}
	alarms, _, err := r.repo.ListAlarms(ctx, store.AlarmFilter{Query: "data_difference", State: "Open"})
	if err != nil || len(alarms) != 0 {
		t.Fatal(alarms, err)
	}
	if _, exists := restored.redundancyPolicies[group.ID]; exists {
		t.Fatal("deleted policy retained")
	}
}
func TestRedundancyConcurrentReadsModesAndTicks(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, true)
	ctx := context.Background()
	r.openSession("s")
	var wg sync.WaitGroup
	for task := 0; task < 4; task++ {
		wg.Add(1)
		go func(task int) {
			defer wg.Done()
			for n := 0; n < 25; n++ {
				switch task {
				case 0:
					r.redundancyTick(ctx, time.Now())
				case 1:
					v, err := r.Redundancy(ctx, "line")
					if err != nil {
						t.Error(err)
					}
					if len(v.Policy.Priority) > 0 {
						v.Policy.Priority[0] = "not-shared"
					}
				case 2:
					route, _ := r.routes.Route("s", "line")
					r.SetSessionAutomatic(ctx, "s", "line", n%2 == 0, route.Revision)
				case 3:
					r.ReadSample("s", testKey("KKS001"))
					r.OnSample("source-a", testKey("KKS001"), domain.Sample{Type: domain.DataTypeDouble, Value: float64(n), Epoch: 1, SourceTimestamp: time.Now()})
				}
			}
		}(task)
	}
	wg.Wait()
	v, _ := r.Redundancy(ctx, "line")
	if v.Policy.Priority[0] != "source-a" {
		t.Fatal("snapshot leaked mutable policy")
	}
}

func TestRedundancyCooldownAndCandidateChangedBeforeCommit(t *testing.T) {
	r := redundancyRuntime(t)
	saveRedundancyTest(t, r, false, false)
	ctx := context.Background()
	r.openSession("auto")
	at := time.Now()
	r.redundancyTick(ctx, at)
	setStationGood(r, "source-a", false)
	r.redundancyTick(ctx, at.Add(time.Second))
	r.configMu.Lock()
	state := r.redundancyStates["line"]
	state.view.LastSwitchAt = at.Add(time.Second)
	r.configMu.Unlock()
	r.redundancyTick(ctx, at.Add(2*time.Second))
	route, _ := r.routes.Route("auto", "line")
	if route.SourceID != "source-a" {
		t.Fatal("cooldown bypassed")
	}
	// A connection changes between qualification and decision. Old epoch health
	// is still in the decision snapshot, but must not authorize a route change.
	r.mu.Lock()
	status := r.statuses["source-b"]
	status.Epoch = 2
	r.statuses["source-b"] = status
	r.mu.Unlock()
	r.configMu.Lock()
	r.automaticRoutesLocked(ctx, r.routes.Groups()[0], r.redundancyPolicyLocked("line"), state, at.Add(4*time.Second))
	r.configMu.Unlock()
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-a" {
		t.Fatal("changed candidate selected")
	}
	r.mu.Lock()
	status.Epoch = 1
	r.statuses["source-b"] = status
	r.mu.Unlock()
	r.redundancyTick(ctx, at.Add(5*time.Second))
	route, _ = r.routes.Route("auto", "line")
	if route.SourceID != "source-b" {
		t.Fatal("did not switch after cooldown", route)
	}
}
