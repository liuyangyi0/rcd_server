package app

import (
	"context"
	"fmt"
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"strings"
	"time"
)

func validateRedundancyImport(policies []domain.RedundancyPolicy, groups []domain.Group, sources []domain.Source) ([]domain.RedundancyPolicy, error) {
	out := make([]domain.RedundancyPolicy, 0, len(policies))
	seen := map[string]bool{}
	for _, p := range policies {
		if _, ok := groupByID(groups, p.GroupID); !ok || seen[p.GroupID] {
			return nil, &domain.FieldError{Field: "redundancy", Message: "unknown or duplicate group"}
		}
		seen[p.GroupID] = true
		if err := p.Validate(); err != nil {
			return nil, err
		}
		ids := append([]string{}, p.Priority...)
		if p.CompareEnabled {
			ids = append(ids, p.LeftSourceID, p.RightSourceID)
		}
		for _, id := range ids {
			source, ok := sourceByID(sources, id)
			if !ok || !source.HasGroup(p.GroupID) {
				return nil, domain.ErrSourceNotInGroup
			}
		}
		p = p.Clone()
		p.Enabled = false
		p.DefaultAutomatic = false
		p.Revision = 1
		out = append(out, p)
	}
	return out, nil
}

func (r *Runtime) redundancyPolicyLocked(id string) domain.RedundancyPolicy {
	return r.redundancy.policy(id)
}
func (r *Runtime) Redundancy(_ context.Context, id string) (domain.RedundancyView, error) {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	if _, ok := groupByID(r.routes.Groups(), id); !ok {
		return domain.RedundancyView{}, domain.ErrNotFound
	}
	v := r.redundancy.view(id)
	v.Policy = r.redundancyPolicyLocked(id)
	if v.Policy.CompareEnabled && v.EvaluatedAt.IsZero() {
		v.ComparisonState = "Unknown"
	}
	v.Routes = map[string]domain.GroupRoute{}
	for _, session := range r.routes.Sessions() {
		if route, ok := session.Routes[id]; ok {
			v.Routes[session.ID] = route
		}
	}
	return v, nil
}
func (r *Runtime) SaveRedundancy(ctx context.Context, id string, revision uint64, p domain.RedundancyPolicy) (domain.RedundancyPolicy, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return p, domain.ErrShuttingDown
	}
	if _, ok := groupByID(r.routes.Groups(), id); !ok {
		return p, domain.ErrNotFound
	}
	if p.Revision != revision || p.GroupID != id {
		return p, &domain.FieldError{Field: "policy", Message: "body group_id/revision must match URL and If-Match"}
	}
	current := r.redundancyPolicyLocked(id)
	if current.Revision != revision {
		return p, &domain.RevisionConflictError{Expected: revision, Actual: current.Revision}
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	if r.routes.PendingCount(id) > 0 {
		return p, domain.ErrGroupPending
	}
	ids := append([]string{}, p.Priority...)
	if p.CompareEnabled {
		ids = append(ids, p.LeftSourceID, p.RightSourceID)
	}
	for _, sourceID := range ids {
		source, err := r.source(sourceID)
		if err != nil {
			return p, err
		}
		if !source.HasGroup(id) {
			return p, domain.ErrSourceNotInGroup
		}
	}
	saved, err := r.repo.SaveRedundancy(ctx, p, revision)
	if err != nil {
		return p, err
	}
	r.redundancy.replacePolicy(saved)
	r.audit(ctx, "group.redundancy", id, saved.Revision, map[string]any{"policy": saved.Clone()})
	r.broker.Publish(api.Event{Type: "redundancy.changed", Data: map[string]string{"group_id": id}})
	return saved.Clone(), nil
}
func (r *Runtime) SetSessionAutomatic(ctx context.Context, session, group string, automatic bool, revision uint64) (domain.GroupRoute, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return domain.GroupRoute{}, domain.ErrShuttingDown
	}
	if automatic && !r.redundancyPolicyLocked(group).Enabled {
		return domain.GroupRoute{}, domain.ErrSourceNotSelectable
	}
	route, err := r.routes.SetAutomatic(session, group, automatic, revision)
	if err != nil {
		return route, err
	}
	r.queueSessionRefresh(session, group)
	r.audit(ctx, "session.automatic", session+"/"+group, route.Revision, map[string]any{"automatic": automatic})
	r.broker.Publish(api.Event{Type: "selection.changed", Data: map[string]any{"session_id": session, "route": route}})
	return route, nil
}

func (r *Runtime) redundancyWorker() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.runContext.Done():
			return
		case now := <-ticker.C:
			r.redundancyTick(r.runContext, now.UTC())
		}
	}
}
func rawRedundancyPoint(p indexedPoint) bool {
	name := p.Node.Key.Identifier
	if strings.HasPrefix(name, "RcdSync.") || name == "TransmitStatus" || name == "InputRefreshIntervalMs" || name == "InputTimeoutMs" {
		return false
	}
	return !p.Node.Retired && !p.Node.Writable && p.Node.Class == domain.NodeClassVariable && p.Node.ValueRank == -1 && p.Node.Kind != "command" && p.Node.Kind != "calculation" && p.Node.Kind != "manager_calculation"
}

// configMu serializes policy changes, manual selection, command dispatch and
// defaults with the decisions below. Cache/status readers retain their own locks.
func (r *Runtime) redundancyTick(ctx context.Context, now time.Time) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return
	}
	groups := r.routes.Groups()
	sources := r.routes.Sources()
	r.mu.RLock()
	index := r.index
	statuses := cloneStatuses(r.statuses)
	r.mu.RUnlock()
	active := map[string]bool{}
	for _, group := range groups {
		p := r.redundancyPolicyLocked(group.ID)
		if !p.Enabled && !p.CompareEnabled && !r.redundancy.hasState(group.ID) && p.Revision == 1 {
			continue
		}
		active[group.ID] = true
		s, version := r.redundancy.begin(group.ID)
		v := &s.view
		v.EvaluatedAt = now
		v.LastError = ""
		v.Health = []domain.SourceHealth{}
		if !p.Enabled && !p.CompareEnabled {
			s.health = map[string]domain.SourceHealth{}
			s.identity = map[string]string{}
			r.compareGroupLocked(ctx, group.ID, p, s, index, statuses, now)
			r.redundancy.commit(group.ID, version, s)
			continue
		}
		currentMembers := map[string]bool{}
		for _, source := range sources {
			if !source.HasGroup(group.ID) {
				continue
			}
			currentMembers[source.ID] = true
			healthy, reason := r.redundancyHealth(source, group.ID, index, statuses[source.ID])
			identity := fmt.Sprintf("%d/%d/%p", source.Revision, statuses[source.ID].Epoch, index)
			s.observe(source.ID, identity, healthy, reason, now)
		}
		s.retainMembers(currentMembers)
		if p.Enabled {
			r.automaticRoutesLocked(ctx, group, p, s, now)
		}
		r.compareGroupLocked(ctx, group.ID, p, s, index, statuses, now)
		r.redundancy.commit(group.ID, version, s)
	}
	for _, id := range r.redundancy.stateIDs() {
		if !active[id] {
			s, version := r.redundancy.begin(id)
			s.view.LastError = ""
			r.persistDifferenceAlarm(ctx, domain.DefaultRedundancyPolicy(id), s, false, "分组已删除")
			if s.view.LastError == "" {
				r.redundancy.remove(id)
			} else {
				r.redundancy.commit(id, version, s)
			}
		}
	}
}

func (r *Runtime) redundancyHealth(source domain.Source, group string, index *catalogIndex, status opc.LowerStatus) (bool, string) {
	if !source.Enabled {
		return false, "来源停用"
	}
	if !source.GroupAvailable(group) || !source.CatalogState.Selectable() || index == nil || !index.union.SourceStates[source.ID].Selectable() {
		return false, "目录冲突或未就绪"
	}
	if status.Connection != opc.ConnectionConnected || status.Session != opc.SessionActive || status.Subscription != opc.SubscriptionActive {
		return false, "OPC 连接或订阅不可用"
	}
	if r.maintenance.maintained(source.ID) {
		return false, "来源维护中"
	}
	count := 0
	devices := map[string]bool{}
	for _, point := range index.groupPoints[group] {
		if !rawRedundancyPoint(point) {
			continue
		}
		count++
		node, exists := index.sourceNode(source.ID, point.Node.Key.Identifier)
		if !exists || node.Retired {
			return false, "组内点表不完整"
		}
		if node.PortID == "" || node.DeviceID == "" {
			return false, "缺少物理站点状态，不能确认采集正常"
		}
		key := node.PortID + "\x00" + node.DeviceID
		if !devices[key] {
			devices[key] = true
			state := r.StationCommunication(source.ID, node.PortID, node.DeviceID)
			if state.State != domain.StationCommunicationGood {
				return false, node.PortID + "/" + node.DeviceID + ": " + state.Text()
			}
		}
		sample, ok := r.cache.Get(source.ID, node.Key)
		if !ok || sample.Epoch != status.Epoch || !sample.StatusCode.IsGood() {
			return false, "组内存在坏质量或尚无当前连接首值"
		}
	}
	if count == 0 {
		return false, "组内没有可验证的原始采集点"
	}
	return true, "组内采集正常且点表完整"
}

func (r *Runtime) automaticRoutesLocked(ctx context.Context, group domain.Group, p domain.RedundancyPolicy, s *redundancyState, now time.Time) {
	selection := s.automaticSelection(p, r.routes.PendingCount(group.ID) > 0, now)
	target := selection.target
	if target == "" {
		return
	}
	// No automatic failback or filling an intentionally empty selection.
	// Status callbacks do not all take configMu. Recheck the candidate against
	// the latest connection before applying a decision made from the tick snapshot.
	source, err := r.source(target)
	if err != nil {
		s.view.LastError = err.Error()
		return
	}
	r.mu.RLock()
	latestIndex := r.index
	latestStatus := r.statuses[target]
	r.mu.RUnlock()
	identity := fmt.Sprintf("%d/%d/%p", source.Revision, latestStatus.Epoch, latestIndex)
	if healthy, _ := r.redundancyHealth(source, group.ID, latestIndex, latestStatus); !healthy || identity != s.identity[target] {
		s.view.LastError = "备用来源状态已变化，等待重新确认"
		return
	}
	mark := func(from, to, targetRoute string) {
		s.view.LastSwitchAt = now
		s.view.LastSwitchReason = fmt.Sprintf("%s：%s → %s；%s", targetRoute, from, to, s.health[from].Reason)
	}
	if p.DefaultAutomatic && selection.shouldSwitch(group.DefaultSourceID) {
		before := group.DefaultSourceID
		updated, err := r.setDefaultLocked(ctx, group.ID, target, group.Revision, "automatic")
		if err != nil {
			s.view.LastError = err.Error()
		} else {
			mark(before, updated.DefaultSourceID, "组默认来源（含中央计算）")
			r.audit(ctx, "group.automatic_switch", group.ID, updated.Revision, map[string]any{"actor": "automatic", "from": before, "to": target, "reason": s.health[before].Reason})
		}
	}
	for _, session := range r.routes.Sessions() {
		route, ok := session.Routes[group.ID]
		if !ok || !route.Automatic || !selection.shouldSwitch(route.SourceID) {
			continue
		}
		updated, err := r.routes.SelectAutomatic(session.ID, group.ID, target, route.Revision)
		if err != nil {
			s.view.LastError = err.Error()
			continue
		}
		mark(route.SourceID, target, "Session "+session.ID)
		r.queueSessionRefresh(session.ID, group.ID)
		r.recordSelection(group.ID)
		r.audit(ctx, "session.automatic_switch", session.ID+"/"+group.ID, updated.Revision, map[string]any{"actor": "automatic", "from": route.SourceID, "to": target, "reason": s.health[route.SourceID].Reason})
		r.broker.Publish(api.Event{Type: "selection.changed", Data: map[string]any{"session_id": session.ID, "route": updated}})
	}
}

func (r *Runtime) compareGroupLocked(ctx context.Context, group string, p domain.RedundancyPolicy, s *redundancyState, index *catalogIndex, statuses map[string]opc.LowerStatus, now time.Time) {
	identity := fmt.Sprintf("%d/%p/%d/%d", p.Revision, index, statuses[p.LeftSourceID].Epoch, statuses[p.RightSourceID].Epoch)
	update := s.compare(p, index != nil, identity, now, func(yield func(string, domain.Sample, domain.Sample, bool)) {
		for _, point := range index.groupPoints[group] {
			if !rawRedundancyPoint(point) {
				continue
			}
			a, aOK := r.cache.Get(p.LeftSourceID, point.Node.Key)
			b, bOK := r.cache.Get(p.RightSourceID, point.Node.Key)
			current := aOK && bOK && a.Epoch == statuses[p.LeftSourceID].Epoch && b.Epoch == statuses[p.RightSourceID].Epoch
			yield(point.Node.Key.Identifier, a, b, current)
		}
	})
	if update != nil {
		r.persistDifferenceAlarm(ctx, p, s, update.active, update.message)
	}
}
func (r *Runtime) persistDifferenceAlarm(ctx context.Context, p domain.RedundancyPolicy, s *redundancyState, active bool, message string) {
	if s.alarmKnown && s.alarmActive == active && (!active || s.alarmMessage == message) {
		return
	}
	_, changed, err := r.repo.ReconcileAlarm(ctx, "", p.DifferenceKind(), domain.AlarmSeverityWarning, message, active)
	if err != nil {
		s.view.LastError = "告警保存失败，将重试：" + err.Error()
		return
	}
	s.alarmKnown = true
	s.alarmActive = active
	s.alarmMessage = message
	s.view.DifferenceAlarmActive = active
	if changed {
		r.broker.Publish(api.Event{Type: "alarm.changed", Data: map[string]string{"group_id": p.GroupID}})
	}
}
