package app

import (
	"fmt"
	"maps"
	"rcd_manager/internal/domain"
	"sort"
	"strings"
	"sync"
	"time"
)

// redundancyService owns policies and qualification/alarm state. Evaluations
// are detached copies: cache scans, route transactions and persistence never
// run under mu. Runtime still coordinates their external effects with configMu.
type redundancyService struct {
	mu       sync.RWMutex
	sequence uint64
	policies map[string]domain.RedundancyPolicy
	states   map[string]*redundancyState
	versions map[string]uint64
}

// A working copy, not a pointer into the service's mutable state.
type redundancyState struct {
	view               domain.RedundancyView
	health             map[string]domain.SourceHealth
	identity           map[string]string
	differences        map[string]time.Time
	comparisonIdentity string
	alarmKnown         bool
	alarmActive        bool
	alarmMessage       string
}

func newRedundancyState() *redundancyState {
	return &redundancyState{
		view:   domain.RedundancyView{ComparisonState: "Disabled", Health: []domain.SourceHealth{}, Differences: []domain.PointDifference{}},
		health: map[string]domain.SourceHealth{}, identity: map[string]string{}, differences: map[string]time.Time{},
	}
}

func (s *redundancyState) clone() *redundancyState {
	if s == nil {
		return newRedundancyState()
	}
	c := *s
	c.health, c.identity, c.differences = maps.Clone(s.health), maps.Clone(s.identity), maps.Clone(s.differences)
	c.view = cloneRedundancyView(s.view)
	return &c
}

func cloneRedundancyView(v domain.RedundancyView) domain.RedundancyView {
	c := v
	c.Policy = v.Policy.Clone()
	c.Health = append([]domain.SourceHealth{}, v.Health...)
	// Comparison accepts only immutable scalars; arrays are never retained here.
	c.Differences = append([]domain.PointDifference{}, v.Differences...)
	c.Routes = maps.Clone(v.Routes)
	return c
}

func newRedundancyService(policies []domain.RedundancyPolicy) *redundancyService {
	s := &redundancyService{policies: map[string]domain.RedundancyPolicy{}, states: map[string]*redundancyState{}, versions: map[string]uint64{}}
	for _, p := range policies {
		s.policies[p.GroupID] = p.Clone()
	}
	return s
}

func (s *redundancyService) policy(id string) domain.RedundancyPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.policies[id]; ok {
		return p.Clone()
	}
	return domain.DefaultRedundancyPolicy(id)
}

func (s *redundancyService) configuredPolicy(id string) (domain.RedundancyPolicy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[id]
	return p.Clone(), ok
}

func (s *redundancyService) automaticGroups() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.policies))
	for id, p := range s.policies {
		out[id] = p.Enabled
	}
	return out
}

func (s *redundancyService) replacePolicy(p domain.RedundancyPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[p.GroupID] = p.Clone()
	state := s.states[p.GroupID].clone()
	// Policy edits reset qualification, not cooldown or persisted alarm evidence.
	state.health = map[string]domain.SourceHealth{}
	state.identity = map[string]string{}
	state.differences = map[string]time.Time{}
	state.comparisonIdentity = ""
	s.states[p.GroupID] = state
	s.sequence++
	s.versions[p.GroupID] = s.sequence
}

func (s *redundancyService) restoreAlarm(id, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[id].clone()
	state.alarmKnown, state.alarmActive, state.alarmMessage = true, true, message
	state.view.DifferenceAlarmActive, state.view.ComparisonState = true, "Unknown"
	s.states[id] = state
	s.sequence++
	s.versions[id] = s.sequence
}

func (s *redundancyService) view(id string) domain.RedundancyView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if state := s.states[id]; state != nil {
		// HTTP/OPC diagnostics copy only the bounded public view, never the
		// potentially point-count-sized difference qualification map.
		return cloneRedundancyView(state.view)
	}
	return domain.RedundancyView{ComparisonState: "Disabled", Health: []domain.SourceHealth{}, Differences: []domain.PointDifference{}}
}

func (s *redundancyService) hasState(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.states[id] != nil
}

func (s *redundancyService) stateIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.states))
	for id := range s.states {
		ids = append(ids, id)
	}
	return ids
}

func (s *redundancyService) begin(id string) (*redundancyState, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[id] == 0 {
		s.sequence++
		s.versions[id] = s.sequence
	}
	return s.states[id].clone(), s.versions[id]
}

// Commit cannot revive a deleted/recreated group or overwrite a policy change
// or a more recent evaluation. External effects require Runtime coordination;
// this generation check alone is not authorization to change routes.
func (s *redundancyService) commit(id string, version uint64, state *redundancyState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version == 0 || s.versions[id] != version {
		return false
	}
	s.states[id] = state.clone()
	s.sequence++
	s.versions[id] = s.sequence
	return true
}

func (s *redundancyService) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, id)
	delete(s.states, id)
	delete(s.versions, id)
}

func (s *redundancyState) observe(id, identity string, healthy bool, reason string, now time.Time) {
	old, exists := s.health[id]
	since := old.Since
	if !exists || old.Healthy != healthy || s.identity[id] != identity {
		since = now
	}
	h := domain.SourceHealth{SourceID: id, Healthy: healthy, Reason: reason, Since: since}
	s.health[id], s.identity[id] = h, identity
	s.view.Health = append(s.view.Health, h)
}

func (s *redundancyState) retainMembers(members map[string]bool) {
	for id := range s.health {
		if !members[id] {
			delete(s.health, id)
			delete(s.identity, id)
		}
	}
}

type differenceAlarmUpdate struct {
	active  bool
	message string
}

type automaticSelectionPlan struct {
	target string
	failed map[string]bool
}

func (p automaticSelectionPlan) shouldSwitch(current string) bool {
	return current != "" && p.target != "" && current != p.target && p.failed[current]
}

func (s *redundancyState) automaticSelection(p domain.RedundancyPolicy, pending bool, now time.Time) automaticSelectionPlan {
	plan := automaticSelectionPlan{failed: map[string]bool{}}
	if pending {
		s.view.LastError = "命令等待 ACK，自动切源已锁定"
		return plan
	}
	if !s.view.LastSwitchAt.IsZero() && now.Sub(s.view.LastSwitchAt) < time.Duration(p.CooldownSeconds)*time.Second {
		return plan
	}
	for _, id := range p.Priority {
		h, ok := s.health[id]
		if ok && h.Healthy && now.Sub(h.Since) >= time.Duration(p.HealthySeconds)*time.Second {
			plan.target = id
			break
		}
	}
	if plan.target == "" {
		s.view.LastError = "没有经过稳定确认的健康备用来源"
		return plan
	}
	for id, h := range s.health {
		if !h.Healthy && now.Sub(h.Since) >= time.Duration(p.FailureSeconds)*time.Second {
			plan.failed[id] = true
		}
	}
	return plan
}

// compare updates only this detached evaluation. The scanner supplies scalar
// samples from the captured catalog; it performs no writes or route selection.
func (s *redundancyState) compare(p domain.RedundancyPolicy, available bool, identity string, now time.Time,
	scan func(func(string, domain.Sample, domain.Sample, bool))) *differenceAlarmUpdate {
	v := &s.view
	v.ComparedCount, v.SkippedCount, v.DifferenceCount = 0, 0, 0
	v.Differences = []domain.PointDifference{}
	if !p.CompareEnabled {
		v.ComparisonState = "Disabled"
		s.differences = map[string]time.Time{}
		return &differenceAlarmUpdate{message: "差异检测已关闭"}
	}
	if !available {
		v.ComparisonState = "Unknown"
		return nil
	}
	if identity != s.comparisonIdentity {
		s.differences = map[string]time.Time{}
		s.comparisonIdentity = identity
	}
	leftOK, rightOK := s.health[p.LeftSourceID].Healthy, s.health[p.RightSourceID].Healthy
	seen := map[string]bool{}
	scan(func(id string, a, b domain.Sample, current bool) {
		seen[id] = true
		different, comparable := domain.CompareSamples(a, b, p)
		if !leftOK || !rightOK || !current || !comparable {
			v.SkippedCount++
			delete(s.differences, id)
			return
		}
		v.ComparedCount++
		if !different {
			delete(s.differences, id)
			return
		}
		since, ok := s.differences[id]
		if !ok {
			since = now
			s.differences[id] = since
		}
		if now.Sub(since) < time.Duration(p.DifferenceSeconds)*time.Second {
			return
		}
		v.DifferenceCount++
		if len(v.Differences) < 20 {
			v.Differences = append(v.Differences, domain.PointDifference{NodeID: id, Left: a.Value, Right: b.Value, Since: since})
		}
	})
	for id := range s.differences {
		if !seen[id] {
			delete(s.differences, id)
		}
	}
	sort.Slice(v.Differences, func(i, j int) bool { return v.Differences[i].NodeID < v.Differences[j].NodeID })
	switch {
	case v.DifferenceCount > 0:
		v.ComparisonState = "Different"
		names := make([]string, 0, len(v.Differences))
		for _, d := range v.Differences {
			names = append(names, d.NodeID)
		}
		return &differenceAlarmUpdate{true, fmt.Sprintf("组 %s：%s 与 %s 有 %d 个持续差异点；%s", p.GroupID, p.LeftSourceID, p.RightSourceID, v.DifferenceCount, strings.Join(names, ", "))}
	case v.SkippedCount > 0 || v.ComparedCount == 0:
		v.ComparisonState = "Unknown" // unavailable must not resolve an existing alarm
	case len(s.differences) > 0:
		v.ComparisonState = "Confirming"
	default:
		v.ComparisonState = "Equal"
		return &differenceAlarmUpdate{message: "数据差异已恢复"}
	}
	return nil
}
