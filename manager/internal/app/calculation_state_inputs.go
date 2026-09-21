package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"rcd_manager/internal/api"
	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

func stateTargetExists(target calculation.StateTarget, catalogs map[string]domain.SourceCatalog) bool {
	catalog, ok := catalogs[target.SourceID]
	if !ok || catalog.Topology == nil {
		return false
	}
	for _, port := range catalog.Topology.Ports {
		if port.PortID == target.PortID {
			for _, device := range port.Devices {
				if device.DeviceID == target.DeviceID {
					return true
				}
			}
		}
	}
	return false
}

func validateStateTargets(inputs []calculation.StateInput, catalogs map[string]domain.SourceCatalog, groups []domain.Group, sources []domain.Source) error {
	var problems []calculation.Problem
	for _, input := range inputs {
		bad := func(message string) {
			problems = append(problems, calculation.Problem{Rule: input.Name, Field: "state_inputs", Message: input.Name + ": " + message})
		}
		members := map[string]domain.Source{}
		for _, source := range sources {
			members[source.ID] = source
		}
		if input.Mode == "group_default" {
			found := false
			for _, group := range groups {
				if group.ID == input.GroupID {
					found = true
				}
			}
			if !found {
				bad("逻辑组不存在")
			}
			for _, source := range sources {
				if source.HasGroup(input.GroupID) {
					mapped := false
					for _, target := range input.Targets {
						if target.SourceID == source.ID {
							mapped = true
						}
					}
					if !mapped {
						bad("缺少来源映射: " + source.ID)
					}
				}
			}
		}
		for _, target := range input.Targets {
			source, ok := members[target.SourceID]
			if !ok || !stateTargetExists(target, catalogs) {
				bad("来源或站点不存在，或尚无有效拓扑: " + target.SourceID + " " + target.PortID + "/" + target.DeviceID)
				continue
			}
			if input.Mode == "group_default" && source.GroupForDevice(target.PortID, target.DeviceID) != input.GroupID {
				bad("目标站点不属于指定组: " + target.SourceID)
			}
		}
	}
	if len(problems) > 0 {
		return &calculation.ValidationError{Problems: problems}
	}
	return nil
}

func (r *Runtime) stateInputValue(input calculation.StateInput, groups []domain.Group, sources []domain.Source) api.StateInputView {
	view := api.StateInputView{StateInput: input, DataType: "UInt32", StatusCode: domain.StatusGood}
	fail := func(message string) api.StateInputView {
		view.StatusCode = domain.StatusBadConfigurationError
		view.Error = message
		view.StateText = "配置异常"
		return view
	}
	selected := ""
	if input.Mode == "fixed" && len(input.Targets) == 1 {
		selected = input.Targets[0].SourceID
	} else {
		for _, group := range groups {
			if group.ID == input.GroupID {
				selected = group.DefaultSourceID
			}
		}
		if selected == "" {
			return fail("逻辑组不存在或未配置默认来源")
		}
	}
	var target calculation.StateTarget
	for _, candidate := range input.Targets {
		if candidate.SourceID == selected {
			target = candidate
			break
		}
	}
	if target.SourceID == "" {
		return fail("当前默认来源缺少站点映射: " + selected)
	}
	view.ResolvedTarget = &target
	var source domain.Source
	found := false
	for _, candidate := range sources {
		if candidate.ID == selected {
			source = candidate
			found = true
			break
		}
	}
	if !found {
		return fail("来源已删除: " + selected)
	}
	if input.Mode == "group_default" && source.GroupForDevice(target.PortID, target.DeviceID) != input.GroupID {
		return fail("目标站点已不属于指定组")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !stateTargetExists(target, r.catalogs) {
		return fail("目标站点已删除或拓扑不可用")
	}
	if source.CatalogState == domain.CatalogConflict {
		return fail("来源目录冲突")
	}
	state := r.stationCommunicationLocked(target.SourceID, target.PortID, target.DeviceID)
	view.Value = state.State
	view.StateText = state.Text()
	view.ObservedAt = state.ObservedAt
	return view
}

func stateViewInput(view api.StateInputView) calculation.Input {
	return calculation.Input{Type: domain.DataTypeUInt32, Value: view.Value, StatusCode: view.StatusCode, SourceTimestamp: view.ObservedAt}
}

// Check state transitions only, not observation heartbeats. Called after a new
// snapshot and once per second so silence still expires to state 0.
func (r *Runtime) refreshCalculationStateInputs(sourceID string) {
	r.mu.RLock()
	plan := r.calculationPlan
	r.mu.RUnlock()
	if plan == nil {
		return
	}
	groups, sources := r.routes.Groups(), r.routes.Sources()
	changed := false
	for _, input := range plan.StateInputsForSource(sourceID) {
		view := r.stateInputValue(input, groups, sources)
		signature := fmt.Sprintf("%d/%d/%v/%s", view.Value, view.StatusCode, view.ResolvedTarget, view.Error)
		r.mu.Lock()
		if r.calculationPlan != plan {
			r.mu.Unlock()
			return
		}
		if r.calculationStateSeen == nil {
			r.calculationStateSeen = map[string]string{}
		}
		if r.calculationStateSeen[input.Name] != signature {
			r.calculationStateSeen[input.Name] = signature
			if plan.HasBaseVariable(input.Name) {
				r.calculationDirty[input.Name] = struct{}{}
				changed = true
			}
			r.calculationStateEventPending = true
		}
		r.mu.Unlock()
	}
	if changed {
		r.wakeCalculationWorker()
	}
}

func (r *Runtime) flushCalculationStateEvent() {
	r.mu.Lock()
	pending := r.calculationStateEventPending
	r.calculationStateEventPending = false
	r.mu.Unlock()
	if pending {
		r.broker.Publish(api.Event{Type: "calculation_state_inputs_changed"})
	}
}

func (r *Runtime) ListCalculationStateInputs(ctx context.Context, request api.PageRequest, viewMode, mode, sourceID string) (api.StateInputPage, error) {
	var inputs []calculation.StateInput
	var revision uint64
	var dependencies map[string]bool
	if viewMode == "draft" {
		draft, err := r.repo.LoadCalculationDraft(ctx)
		if err != nil {
			return api.StateInputPage{}, err
		}
		revision = draft.Revision
		config, err := calculation.ParseConfig(draft.Content)
		if err != nil {
			return api.StateInputPage{}, &calculation.ValidationError{Problems: []calculation.Problem{{Field: "config", Message: err.Error()}}}
		}
		inputs = config.StateInputs
	} else {
		r.mu.RLock()
		plan := r.calculationPlan
		revision = r.calculationVersion.Revision
		r.mu.RUnlock()
		if plan != nil {
			inputs = plan.StateInputs()
			if request.Kind != "" {
				dependencies = plan.StateDependencies(request.Kind)
			}
		}
	}
	items := []api.StateInputView{}
	groups, sources := r.routes.Groups(), r.routes.Sources()
	for _, input := range inputs {
		if request.Kind != "" && !dependencies[input.Name] {
			continue
		}
		if mode != "" && input.Mode != mode || request.Query != "" && !strings.Contains(strings.ToLower(input.Name), strings.ToLower(request.Query)) {
			continue
		}
		value := r.stateInputValue(input, groups, sources)
		if sourceID != "" && (value.ResolvedTarget == nil || value.ResolvedTarget.SourceID != sourceID) {
			continue
		}
		if request.Status != "" && request.Status != fmt.Sprint(value.Value) {
			continue
		}
		items = append(items, value)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	limit, offset := calculationPageBounds(request, len(items))
	return api.StateInputPage{Items: items[offset:min(offset+limit, len(items))], Total: len(items), Limit: limit, Offset: offset, Revision: revision}, nil
}

func (r *Runtime) ListCalculationStateCandidates(_ context.Context, request api.PageRequest, groupID, sourceID string) (api.StateCandidatePage, error) {
	groups, sources := r.routes.Groups(), r.routes.Sources()
	names := map[string]string{}
	for _, group := range groups {
		names[group.ID] = group.Name
	}
	r.mu.RLock()
	catalogs := cloneCatalogMap(r.catalogs)
	r.mu.RUnlock()
	items := []api.StateCandidate{}
	for _, source := range sources {
		if sourceID != "" && sourceID != source.ID {
			continue
		}
		topology := catalogs[source.ID].Topology
		if topology == nil {
			continue
		}
		for _, port := range topology.Ports {
			for _, device := range port.Devices {
				if request.PortID != "" && request.PortID != port.PortID || request.DeviceID != "" && request.DeviceID != device.DeviceID {
					continue
				}
				group := source.GroupForDevice(port.PortID, device.DeviceID)
				if groupID != "" && groupID != group {
					continue
				}
				if request.Query != "" && !strings.Contains(strings.ToLower(source.ID+" "+source.Name+" "+port.PortID+" "+device.DeviceID+" "+device.Name), strings.ToLower(request.Query)) {
					continue
				}
				items = append(items, api.StateCandidate{StateTarget: calculation.StateTarget{SourceID: source.ID, PortID: port.PortID, DeviceID: device.DeviceID}, SourceName: source.Name, GroupID: group, GroupName: names[group], DeviceName: device.Name})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		return a.SourceID+"\x00"+a.PortID+"\x00"+a.DeviceID < b.SourceID+"\x00"+b.PortID+"\x00"+b.DeviceID
	})
	limit, offset := calculationPageBounds(request, len(items))
	return api.StateCandidatePage{Items: items[offset:min(offset+limit, len(items))], Total: len(items), Limit: limit, Offset: offset}, nil
}
