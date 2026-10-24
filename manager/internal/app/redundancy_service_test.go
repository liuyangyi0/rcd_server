package app

import (
	"context"
	"rcd_manager/internal/domain"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRedundancyServiceOwnsSnapshotsAndPolicy(t *testing.T) {
	p := domain.DefaultRedundancyPolicy("g")
	p.Enabled, p.Priority = true, []string{"a", "b"}
	s := newRedundancyService([]domain.RedundancyPolicy{p})
	p.Priority[0] = "mutated"
	if s.policy("g").Priority[0] != "a" {
		t.Fatal("constructor retains policy slice")
	}
	state, version := s.begin("g")
	state.observe("a", "epoch1", true, "ok", time.Now())
	state.view.Differences = []domain.PointDifference{{NodeID: "n", Left: 1.0, Right: 2.0}}
	if !s.commit("g", version, state) {
		t.Fatal("commit rejected")
	}
	state.health["a"] = domain.SourceHealth{}
	state.view.Health[0].SourceID = "changed"
	state.view.Differences[0].NodeID = "changed"
	view := s.view("g")
	view.Health[0].SourceID = "reader"
	view.Differences[0].NodeID = "reader"
	copy, _ := s.begin("g")
	if !copy.health["a"].Healthy || copy.view.Health[0].SourceID != "a" || copy.view.Differences[0].NodeID != "n" {
		t.Fatal("mutable snapshot leaked")
	}
	policy, ok := s.configuredPolicy("g")
	if !ok {
		t.Fatal("policy missing")
	}
	policy.Priority[0] = "reader"
	groups := s.automaticGroups()
	groups["g"] = false
	if s.policy("g").Priority[0] != "a" || !s.automaticGroups()["g"] {
		t.Fatal("policy getter leaked")
	}
}

func TestRedundancyServiceRejectsStaleEvaluation(t *testing.T) {
	for _, change := range []string{"policy", "delete", "recreate", "alarm", "newer_evaluation"} {
		t.Run(change, func(t *testing.T) {
			p := domain.DefaultRedundancyPolicy("g")
			s := newRedundancyService([]domain.RedundancyPolicy{p})
			state, token := s.begin("g")
			state.view.LastError = "obsolete"
			switch change {
			case "policy":
				p.Revision++
				s.replacePolicy(p)
			case "delete":
				s.remove("g")
			case "recreate":
				s.remove("g")
				s.replacePolicy(p)
			case "alarm":
				s.restoreAlarm("g", "persisted warning")
			case "newer_evaluation":
				newer, v := s.begin("g")
				if !s.commit("g", v, newer) {
					t.Fatal("new evaluation failed")
				}
			}
			if s.commit("g", token, state) || s.view("g").LastError == "obsolete" {
				t.Fatal("stale result published")
			}
		})
	}
}

func TestRedundancyPolicyEditResetsQualificationButKeepsEvidence(t *testing.T) {
	p := domain.DefaultRedundancyPolicy("g")
	s := newRedundancyService(nil)
	state, token := s.begin("g")
	at := time.Now()
	state.observe("a", "epoch1", true, "ok", at)
	state.differences["n"], state.comparisonIdentity = at, "epoch1"
	state.view.LastSwitchAt, state.view.LastSwitchReason = at, "source-a -> source-b"
	state.alarmKnown, state.alarmActive, state.alarmMessage = true, true, "difference"
	state.view.DifferenceAlarmActive = true
	s.commit("g", token, state)
	s.replacePolicy(p)
	copy, _ := s.begin("g")
	if len(copy.health) != 0 || len(copy.identity) != 0 || len(copy.differences) != 0 || copy.comparisonIdentity != "" {
		t.Fatal("old timers retained")
	}
	if copy.view.LastSwitchAt != at || copy.view.LastSwitchReason == "" || !copy.alarmActive || !copy.view.DifferenceAlarmActive {
		t.Fatal("cooldown/alarm evidence lost")
	}
}

func TestRedundancyObservationResetsOnlyOnHealthOrIdentityChange(t *testing.T) {
	s := newRedundancyState()
	at := time.Now()
	s.observe("a", "epoch1", true, "ok", at)
	s.observe("a", "epoch1", true, "another reason", at.Add(time.Second))
	if s.health["a"].Since != at {
		t.Fatal("stable health timer reset")
	}
	s.observe("a", "epoch2", true, "ok", at.Add(2*time.Second))
	if s.health["a"].Since != at.Add(2*time.Second) {
		t.Fatal("epoch did not reset timer")
	}
	s.observe("a", "epoch2", false, "bad", at.Add(3*time.Second))
	if s.health["a"].Since != at.Add(3*time.Second) {
		t.Fatal("health change did not reset timer")
	}
	s.retainMembers(map[string]bool{"b": true})
	if len(s.health) != 0 || len(s.identity) != 0 {
		t.Fatal("removed member retained")
	}
}

func TestRedundancyComparisonDoesNotHoldServiceLock(t *testing.T) {
	p := domain.DefaultRedundancyPolicy("g")
	p.CompareEnabled, p.LeftSourceID, p.RightSourceID = true, "a", "b"
	s := newRedundancyService([]domain.RedundancyPolicy{p})
	state, token := s.begin("g")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		state.compare(p, true, "catalog1", time.Now(), func(func(string, domain.Sample, domain.Sample, bool)) { close(entered); <-release })
		s.commit("g", token, state)
	}()
	<-entered
	read := make(chan struct{})
	go func() { s.view("g"); s.policy("g"); s.replacePolicy(p); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("reader/policy blocked by scan")
	}
	close(release)
	<-done
	if s.view("g").ComparisonState == "Unknown" {
		t.Fatal("obsolete scan overwrote policy replacement")
	}
}

func TestRedundancyComparisonUnknownPreservesAlarmAndResetsTimers(t *testing.T) {
	p := domain.DefaultRedundancyPolicy("g")
	p.CompareEnabled, p.LeftSourceID, p.RightSourceID, p.DifferenceSeconds = true, "a", "b", 1
	s := newRedundancyState()
	at := time.Now()
	s.observe("a", "epoch1", true, "ok", at)
	s.observe("b", "epoch1", true, "ok", at)
	s.view.DifferenceAlarmActive, s.alarmActive = true, true
	a := domain.Sample{Type: domain.DataTypeDouble, Value: 1.0, SourceTimestamp: at}
	b := domain.Sample{Type: domain.DataTypeDouble, Value: 2.0, SourceTimestamp: at}
	scan := func(current bool) func(func(string, domain.Sample, domain.Sample, bool)) {
		return func(yield func(string, domain.Sample, domain.Sample, bool)) { yield("n", a, b, current) }
	}
	if update := s.compare(p, true, "epoch1", at, scan(true)); update != nil || s.view.ComparisonState != "Confirming" {
		t.Fatal("difference not qualified")
	}
	if update := s.compare(p, true, "epoch1", at.Add(time.Second), scan(true)); update == nil || !update.active {
		t.Fatal("confirmed difference missing")
	}
	if update := s.compare(p, true, "epoch1", at.Add(2*time.Second), scan(false)); update != nil || s.view.ComparisonState != "Unknown" || !s.view.DifferenceAlarmActive || len(s.differences) != 0 {
		t.Fatal("unavailable recovered alarm or retained timer")
	}
	if update := s.compare(p, true, "epoch1", at.Add(3*time.Second), scan(true)); update != nil || s.view.ComparisonState != "Confirming" {
		t.Fatal("interrupted timer accumulated")
	}
	if update := s.compare(p, true, "epoch2", at.Add(4*time.Second), scan(true)); update != nil {
		t.Fatal("epoch inherited timer")
	}
	b.Value = 1.0
	if update := s.compare(p, true, "epoch2", at.Add(5*time.Second), scan(true)); update == nil || update.active || s.view.ComparisonState != "Equal" {
		t.Fatal("equal did not recover")
	}
	p.CompareEnabled = false
	if update := s.compare(p, false, "", at, nil); update == nil || update.active || s.view.ComparisonState != "Disabled" {
		t.Fatal("disable did not resolve")
	}
}

func TestRedundancySelectionQualificationAndNoFailback(t *testing.T) {
	p := domain.DefaultRedundancyPolicy("g")
	p.Priority = []string{"a", "b", "c"}
	s := newRedundancyState()
	at := time.Now()
	s.observe("a", "epoch1", false, "down", at)
	s.observe("b", "epoch1", true, "ok", at)
	s.observe("c", "epoch1", true, "ok", at)
	if plan := s.automaticSelection(p, false, at.Add(4*time.Second)); plan.target != "" {
		t.Fatal("selected before healthy confirmation")
	}
	plan := s.automaticSelection(p, false, at.Add(5*time.Second))
	if plan.target != "b" || !plan.shouldSwitch("a") || plan.shouldSwitch("b") || plan.shouldSwitch("c") || plan.shouldSwitch("") || plan.shouldSwitch("missing") {
		t.Fatal("invalid failure-only selection", plan)
	}
	if plan := s.automaticSelection(p, true, at.Add(5*time.Second)); plan.target != "" {
		t.Fatal("pending command bypassed")
	}
	s.view.LastSwitchAt = at.Add(5 * time.Second)
	if plan := s.automaticSelection(p, false, at.Add(34*time.Second)); plan.target != "" {
		t.Fatal("cooldown bypassed")
	}
	if plan := s.automaticSelection(p, false, at.Add(35*time.Second)); plan.target != "b" {
		t.Fatal("cooldown did not expire")
	}
	s.observe("a", "epoch1", true, "recovered", at.Add(35*time.Second))
	plan = s.automaticSelection(p, false, at.Add(40*time.Second))
	if plan.target != "a" || plan.shouldSwitch("b") {
		t.Fatal("higher priority recovery caused failback")
	}
}

func TestRedundancyRemovedGroupCleanupRetriesPastError(t *testing.T) {
	r := redundancyRuntime(t)
	r.redundancy.replacePolicy(domain.DefaultRedundancyPolicy("removed"))
	state, token := r.redundancy.begin("removed")
	state.view.LastError = "previous alarm persistence failure"
	if !r.redundancy.commit("removed", token, state) {
		t.Fatal("test setup commit failed")
	}
	r.redundancyTick(context.Background(), time.Now())
	if _, exists := r.redundancy.configuredPolicy("removed"); exists || r.redundancy.hasState("removed") {
		t.Fatal("old error prevented successful cleanup retry")
	}
}

func TestRedundancyServiceConcurrentAccess(t *testing.T) {
	s := newRedundancyService(nil)
	var wg sync.WaitGroup
	for task := 0; task < 4; task++ {
		wg.Add(1)
		go func(task int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				switch task {
				case 0:
					state, token := s.begin("g")
					state.observe("a", "epoch1", true, "ok", time.Now())
					s.commit("g", token, state)
				case 1:
					s.replacePolicy(domain.DefaultRedundancyPolicy("g"))
				case 2:
					s.view("g")
					s.policy("g")
					s.automaticGroups()
					s.stateIDs()
				case 3:
					s.remove("g")
					s.restoreAlarm("g", "warning")
				}
			}
		}(task)
	}
	wg.Wait()
}

func BenchmarkRedundancyDiagnosticsView(b *testing.B) {
	for _, count := range []int{0, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			s := newRedundancyService(nil)
			state, token := s.begin("g")
			at := time.Now()
			state.observe("a", "epoch1", true, "ok", at)
			for n := 0; n < count; n++ {
				state.differences[strconv.Itoa(n)] = at
			}
			s.commit("g", token, state)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				s.view("g")
			}
		})
	}
}
