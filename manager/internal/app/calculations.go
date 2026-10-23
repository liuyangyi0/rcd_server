package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

const calculationDebounce = 10 * time.Millisecond

func (r *Runtime) CalculationMetadata() opc.CalculationMetadata {
	status, identifiers := r.calculations.metadata()
	value := opc.CalculationMetadata{
		RoutingMode: status.RoutingMode, ActiveRevision: status.ActiveRevision,
		ConfigHash: status.ConfigHash, RuleCount: uint32(status.RuleCount),
		GoodCount: uint32(status.GoodCount), BadCount: uint32(status.BadCount),
		DefaultRouteRevision: status.DefaultRouteRevision, LastEvaluationAt: status.LastEvaluationAt,
		LastEvaluationDurationMS: status.LastEvaluationDurationMS, PendingRestart: status.PendingRestart,
	}
	for _, id := range identifiers {
		value.CalculationNodeIDs = append(value.CalculationNodeIDs, "ns=2;s="+id)
	}
	return value
}

func (r *Runtime) GetCalculationStatus(_ context.Context) (api.CalculationStatus, error) {
	return r.calculations.status(), nil
}

func (r *Runtime) ListCalculationResults(_ context.Context, request api.PageRequest) (api.CalculationResultPage, error) {
	items := r.calculations.resultList()
	query := strings.ToLower(strings.TrimSpace(request.Query))
	filtered := items[:0]
	for _, result := range items {
		if query != "" && !strings.Contains(strings.ToLower(result.NodeID+" "+result.Expression+" "+result.Error), query) {
			continue
		}
		if request.Kind != "" && !strings.EqualFold(request.Kind, string(result.DataType)) {
			continue
		}
		if request.Status != "" && !matchesStatus(request.Status, result.StatusCode) {
			continue
		}
		filtered = append(filtered, result)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].NodeID < filtered[j].NodeID })
	limit, offset := calculationPageBounds(request, len(filtered))
	return api.CalculationResultPage{Items: append([]calculation.Result(nil), filtered[offset:min(offset+limit, len(filtered))]...), Total: len(filtered), Limit: limit, Offset: offset}, nil
}

func (r *Runtime) ListCalculationRules(_ context.Context, request api.PageRequest) (api.CalculationRulePage, error) {
	plan, _ := r.calculations.definition()
	if plan == nil {
		return api.CalculationRulePage{Items: []calculation.RuleView{}, Limit: request.Limit, Offset: request.Offset}, nil
	}
	items := plan.RuleViews()
	filtered := items[:0]
	for _, rule := range items {
		if calculation.MatchRule(rule, request.Query, request.Kind) {
			filtered = append(filtered, rule)
		}
	}
	limit, offset := calculationPageBounds(request, len(filtered))
	return api.CalculationRulePage{Items: append([]calculation.RuleView(nil), filtered[offset:min(offset+limit, len(filtered))]...), Total: len(filtered), Limit: limit, Offset: offset}, nil
}

func calculationPageBounds(request api.PageRequest, total int) (int, int) {
	limit := request.Limit
	if limit <= 0 || limit > calculation.MaxRules {
		limit = 100
	}
	offset := request.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	return limit, offset
}

func (r *Runtime) GetCalculationDraft(ctx context.Context) (calculation.Draft, error) {
	return r.repo.LoadCalculationDraft(ctx)
}

func (r *Runtime) SaveCalculationDraft(ctx context.Context, expected uint64, content string) (calculation.Draft, error) {
	validation, validationErr := r.validateCalculationContent(content)
	draft := calculation.Draft{Content: content, Valid: validation.Valid, Hash: validation.Hash, Problems: validation.Problems}
	if validationErr != nil && !errors.Is(validationErr, calculation.ErrConfigTooLarge) {
		// Syntax and semantic errors belong to the draft and are returned as a
		// report; only hard size ceilings reject the request itself.
		validationErr = nil
	}
	if validationErr != nil {
		return calculation.Draft{}, validationErr
	}
	saved, err := r.repo.SaveCalculationDraft(ctx, draft, expected)
	if err == nil {
		r.audit(ctx, "calculation.draft", "global", saved.Revision, map[string]any{"hash": saved.Hash, "valid": saved.Valid, "problem_count": len(saved.Problems)})
	}
	return saved, err
}

func (r *Runtime) ImportCalculationFromLower(ctx context.Context, sourceID string, sourceRevision uint64) (calculation.Draft, error) {
	if sourceRevision == 0 {
		return calculation.Draft{}, &domain.FieldError{Field: "source_revision", Message: "is required"}
	}
	// Check the requested revision in the same short transaction that captures
	// the physical client, not in a separate source read before that capture.
	result, err := r.ExecuteSourceOperation(ctx, sourceID, sourceRevision, domain.OperationRequest{Action: "read_config", Name: "calc.toml"})
	if err != nil {
		return calculation.Draft{}, err
	}
	// A read does not hold a mutation lease. If the source was edited while
	// the OPC call was in flight, do not let that old response replace a draft
	// associated with the now-current source definition.
	source, err := r.source(sourceID)
	if err != nil {
		return calculation.Draft{}, err
	}
	if source.Revision != sourceRevision {
		return calculation.Draft{}, &domain.RevisionConflictError{Expected: sourceRevision, Actual: source.Revision}
	}
	var response struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(result, &response) != nil || response.Content == "" {
		return calculation.Draft{}, &domain.OperationError{Code: "invalid_response", Message: "下级 calc.toml 内容无效"}
	}
	validation, validationErr := r.validateCalculationContent(response.Content)
	if validationErr != nil && errors.Is(validationErr, calculation.ErrConfigTooLarge) {
		return calculation.Draft{}, validationErr
	}
	return calculation.Draft{Content: response.Content, Hash: validation.Hash, Valid: validation.Valid, Problems: validation.Problems}, nil
}

func (r *Runtime) ValidateCalculation(ctx context.Context, content *string) (api.CalculationValidation, error) {
	if content == nil {
		draft, err := r.repo.LoadCalculationDraft(ctx)
		if err != nil {
			return api.CalculationValidation{}, err
		}
		content = &draft.Content
	}
	return r.validateCalculationContent(*content)
}

func (r *Runtime) validateCalculationContent(content string) (api.CalculationValidation, error) {
	config, err := calculation.ParseConfig(content)
	if err != nil {
		if errors.Is(err, calculation.ErrConfigTooLarge) {
			return api.CalculationValidation{}, err
		}
		return api.CalculationValidation{Valid: false, Problems: []calculation.Problem{{Field: "config", Message: err.Error()}}}, nil
	}
	r.mu.RLock()
	catalogs := cloneCatalogMap(r.catalogs)
	r.mu.RUnlock()
	rawIndex, _, _, err := validateCatalogs(filterLowerCalculations(catalogs))
	if err != nil {
		return api.CalculationValidation{}, err
	}
	err = validateStateTargets(config.StateInputs, catalogs, r.routes.Groups(), r.routes.Sources())
	if err == nil {
		_, err = calculation.Compile(config, calculationInputs(rawIndex))
	}
	if err != nil {
		var invalid *calculation.ValidationError
		if errors.As(err, &invalid) {
			return api.CalculationValidation{Valid: false, Hash: config.Hash, RuleCount: len(config.Rules), Problems: invalid.Problems}, nil
		}
		return api.CalculationValidation{}, err
	}
	return api.CalculationValidation{Valid: true, Hash: config.Hash, RuleCount: len(config.Rules), Problems: []calculation.Problem{}}, nil
}

func (r *Runtime) ActivateCalculation(ctx context.Context, draftRevision uint64) (calculation.Version, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	draft, err := r.repo.LoadCalculationDraft(ctx)
	if err != nil {
		return calculation.Version{}, err
	}
	if draft.Revision != draftRevision {
		return calculation.Version{}, &domain.RevisionConflictError{Expected: draftRevision, Actual: draft.Revision}
	}
	validation, err := r.validateCalculationContent(draft.Content)
	if err != nil {
		return calculation.Version{}, err
	}
	if !validation.Valid {
		return calculation.Version{}, &calculation.ValidationError{Problems: validation.Problems}
	}
	config, err := calculation.ParseConfig(draft.Content)
	if err != nil {
		return calculation.Version{}, err
	}
	// Consume this exact draft revision so two activation requests cannot both
	// create an immutable version from the same optimistic snapshot.
	if _, err = r.repo.SaveCalculationDraft(ctx, draft, draftRevision); err != nil {
		return calculation.Version{}, err
	}
	return r.activateCalculationConfigLocked(ctx, config, "draft")
}

func (r *Runtime) activateCalculationConfigLocked(ctx context.Context, config calculation.Config, reason string) (calculation.Version, error) {
	r.mu.RLock()
	catalogs := cloneCatalogMap(r.catalogs)
	old := r.calculations.snapshot()
	oldPlan, oldResults, oldRetiredResults, oldRetiredPoints := old.plan, old.results, old.retired, old.retiredPoints
	oldIndex := r.index
	r.mu.RUnlock()
	rawIndex, _, _, err := validateCatalogs(filterLowerCalculations(catalogs))
	if err != nil {
		return calculation.Version{}, err
	}
	if err = validateStateTargets(config.StateInputs, catalogs, r.routes.Groups(), r.routes.Sources()); err != nil {
		return calculation.Version{}, err
	}
	plan, err := calculation.Compile(config, calculationInputs(rawIndex))
	if err != nil {
		return calculation.Version{}, err
	}
	if calculationShapeChanged(oldPlan, oldRetiredPoints, plan) {
		diff := calculation.ComparePlans(oldPlan, plan)
		version, persistErr := r.repo.ActivateCalculation(ctx, config, requestActor(ctx), "pending_restart", diff)
		if persistErr != nil {
			return calculation.Version{}, persistErr
		}
		r.calculations.setPending(version)
		_ = r.repo.PruneCalculationVersions(ctx, 50)
		r.audit(ctx, "calculation.pending_restart", "global", version.Revision, map[string]any{"hash": version.Hash, "rules": version.RuleCount, "reason": reason})
		r.broker.Publish(api.Event{Type: "calculation_config_changed", Revision: version.Revision})
		return version, nil
	}
	newOutputs := make(map[string]struct{}, plan.RuleCount())
	for _, node := range plan.CatalogNodes() {
		newOutputs[node.Key.Identifier] = struct{}{}
	}
	retiredPoints := cloneIndexedPoints(oldRetiredPoints)
	for identifier := range newOutputs {
		delete(retiredPoints, identifier)
	}
	if oldPlan != nil {
		for _, node := range oldPlan.CatalogNodes() {
			if _, exists := newOutputs[node.Key.Identifier]; exists {
				continue
			}
			point, exists := oldIndex.pointForNode(node.Key.Identifier)
			if !exists {
				point = indexedPoint{Node: node}
			}
			point.Node = node
			point.Node.Retired = true
			retiredPoints[node.Key.Identifier] = point
		}
	}
	groups, sources := r.routes.Groups(), r.routes.Sources()
	index, states, conflicts, err := buildCatalogIndexWithCalculations(catalogs, plan, retiredPoints, groups)
	if err != nil {
		return calculation.Version{}, err
	}
	conflicts += r.enforceCatalogQuarantine(index, states, "")
	for i := range sources {
		if state, ok := states[sources[i].ID]; ok {
			sources[i].CatalogState = state
		}
	}
	if err = r.validateUpstream(index, groups, sources); err != nil {
		return calculation.Version{}, err
	}
	diff := calculation.ComparePlans(oldPlan, plan)
	version, err := r.repo.ActivateCalculation(ctx, config, requestActor(ctx), "active", diff)
	if err != nil {
		return calculation.Version{}, err
	}
	for sourceID, state := range states {
		catalog := catalogs[sourceID]
		catalog.State = state
		catalogs[sourceID] = catalog
	}
	if err = r.routes.ApplyConfiguration(groups, sources); err != nil {
		return calculation.Version{}, err
	}
	now := time.Now().UTC()
	retired := make(map[string]calculation.Result)
	if oldPlan != nil {
		for _, node := range oldPlan.CatalogNodes() {
			if _, exists := newOutputs[node.Key.Identifier]; exists {
				continue
			}
			result := oldResults[node.Key.Identifier]
			result.NodeID, result.DataType = node.Key.Identifier, node.DataType
			result.Value = zeroValue(node.DataType, -1)
			result.StatusCode = domain.StatusBadOutOfService
			result.EvaluatedAt = now
			result.Error = "calculation output removed by active configuration"
			retired[node.Key.Identifier] = result
		}
	}
	r.mu.Lock()
	r.catalogs, r.index, r.conflictCount = catalogs, index, conflicts
	for id, value := range oldRetiredResults {
		if _, active := newOutputs[id]; !active {
			value.Value = zeroValue(value.DataType, -1)
			retired[id] = value
		}
	}
	r.calculations.install(plan, version, retired, retiredPoints)
	r.mu.Unlock()
	if err = r.publishCatalog(index, groups, sources); err != nil {
		return calculation.Version{}, err
	}
	_ = r.repo.PruneCalculationVersions(ctx, 50)
	r.audit(ctx, "calculation.activated", "global", version.Revision, map[string]any{
		"hash": version.Hash, "rules": version.RuleCount, "added": diff.Added, "removed": diff.Removed, "changed": diff.Changed, "reason": reason,
	})
	r.broker.Publish(api.Event{Type: "calculation_config_changed", Revision: version.Revision})
	r.requestCalculationFull()
	return version, nil
}

func (r *Runtime) DeactivateCalculation(ctx context.Context, activeRevision uint64) error {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	r.mu.RLock()
	plan, version := r.calculations.definition()
	catalogs := cloneCatalogMap(r.catalogs)
	r.mu.RUnlock()
	if plan == nil || version.Revision == 0 {
		return domain.ErrNotFound
	}
	if version.Revision != activeRevision {
		return &domain.RevisionConflictError{Expected: activeRevision, Actual: version.Revision}
	}
	index, states, conflicts, err := buildCatalogIndexWithCalculations(catalogs, nil, nil, r.routes.Groups())
	if err != nil {
		return err
	}
	for _, node := range plan.CatalogNodes() {
		if _, state := plan.StateInput(node.Key.Identifier); state {
			// State outputs belong to the manager, not to a lower local engine.
			continue
		}
		legacy, exists := index.pointForNode(node.Key.Identifier)
		if !exists || legacy.Node.Kind != "calculation" || legacy.Node.DataType != node.DataType {
			return &domain.OperationError{Code: "conflict", Message: "下级尚未恢复 local 计算模式，不能停用中央计算"}
		}
	}
	groups, sources := r.routes.Groups(), r.routes.Sources()
	for i := range sources {
		if state, ok := states[sources[i].ID]; ok {
			sources[i].CatalogState = state
		}
	}
	if err = r.validateUpstream(index, groups, sources); err != nil {
		return err
	}
	if err = r.repo.DeactivateCalculation(ctx); err != nil {
		return err
	}
	if err = r.routes.ApplyConfiguration(groups, sources); err != nil {
		return err
	}
	r.mu.Lock()
	r.index, r.conflictCount = index, conflicts
	r.calculations.install(nil, calculation.Version{}, nil, nil)
	r.mu.Unlock()
	if err = r.publishCatalog(index, groups, sources); err != nil {
		return err
	}
	r.audit(ctx, "calculation.deactivated", "global", activeRevision, map[string]any{"previous_hash": version.Hash})
	r.broker.Publish(api.Event{Type: "calculation_config_changed", Revision: activeRevision})
	ids := make([]string, 0, plan.RuleCount())
	for _, node := range plan.CatalogNodes() {
		ids = append(ids, node.Key.Identifier)
	}
	r.refreshCalculationNodes(ids)
	return nil
}

func (r *Runtime) ListCalculationVersions(ctx context.Context, request api.PageRequest) (api.CalculationVersionPage, error) {
	limit, offset := calculationPageBounds(request, int(^uint(0)>>1))
	items, total, err := r.repo.ListCalculationVersions(ctx, limit, offset)
	return api.CalculationVersionPage{Items: items, Total: total, Limit: limit, Offset: offset}, err
}

func (r *Runtime) RestoreCalculationVersion(ctx context.Context, id int64, activeRevision uint64) (calculation.Version, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	_, active := r.calculations.definition()
	current := active.Revision
	if current != activeRevision {
		return calculation.Version{}, &domain.RevisionConflictError{Expected: activeRevision, Actual: current}
	}
	archived, err := r.repo.LoadCalculationVersion(ctx, id)
	if err != nil {
		return calculation.Version{}, err
	}
	config, err := calculation.ParseConfig(archived.Content)
	if err != nil {
		return calculation.Version{}, err
	}
	return r.activateCalculationConfigLocked(ctx, config, fmt.Sprintf("restore:%d", id))
}

func calculationShapeChanged(before *calculation.Plan, retired map[string]indexedPoint, after *calculation.Plan) bool {
	if after == nil {
		return false
	}
	types := make(map[string]domain.DataType, len(retired))
	if before != nil {
		for _, node := range before.CatalogNodes() {
			types[node.Key.Identifier] = node.DataType
		}
	}
	for identifier, point := range retired {
		types[identifier] = point.Node.DataType
	}
	for _, node := range after.CatalogNodes() {
		if old, exists := types[node.Key.Identifier]; exists && old != node.DataType {
			return true
		}
	}
	return false
}

func cloneIndexedPoints(input map[string]indexedPoint) map[string]indexedPoint {
	result := make(map[string]indexedPoint, len(input))
	for key, value := range input {
		value.SourceIDs = append([]string(nil), value.SourceIDs...)
		result[key] = value
	}
	return result
}

func countAddedOutputs(before, after *calculation.Plan) int {
	known := make(map[string]struct{})
	if before != nil {
		for _, node := range before.CatalogNodes() {
			known[node.Key.Identifier] = struct{}{}
		}
	}
	count := 0
	if after != nil {
		for _, node := range after.CatalogNodes() {
			if _, exists := known[node.Key.Identifier]; !exists {
				count++
			}
		}
	}
	return count
}

func cloneCalculationResults(input map[string]calculation.Result) map[string]calculation.Result {
	result := make(map[string]calculation.Result, len(input))
	for key, value := range input {
		result[key] = cloneCalculationResult(value)
	}
	return result
}

func cloneCalculationResult(value calculation.Result) calculation.Result {
	value.CommunicationInputs = append([]string(nil), value.CommunicationInputs...)
	return value
}

func (r *Runtime) requestCalculationFull() { r.calculations.invalidate(false) }

func (r *Runtime) calculationRouteChanged() { r.calculations.invalidate(true) }

func (r *Runtime) calculationInputChanged(sourceID, identifier string) {
	groups := r.routes.Groups()
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	if index == nil {
		return
	}
	groupID, exists := index.groupForNode(identifier)
	if !exists {
		return
	}
	for _, group := range groups {
		if group.ID == groupID && group.DefaultSourceID == sourceID {
			r.calculations.inputChanged(identifier)
			return
		}
	}
}

func (r *Runtime) wakeCalculationWorker() { r.calculations.signal() }

func (r *Runtime) calculationWorker() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		r.mu.RLock()
		ctx := r.runContext
		r.mu.RUnlock()
		if ctx == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshCalculationStateInputs("")
			r.flushCalculationStateEvent()
			r.flushCalculationEvent()
		case <-r.calculations.wake:
			timer := time.NewTimer(calculationDebounce)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
			r.runCalculationBatch()
		}
	}
}

func (r *Runtime) runCalculationBatch() {
	// Route/source/catalog invalidation also refreshes directly published
	// states, including configurations with no formulas at all.
	r.refreshCalculationStateInputs("")
	changed, publish, revision := r.calculations.evaluate(func(plan *calculation.Plan, dirty []string) (map[string]calculation.Input, bool) {
		r.configMu.RLock()
		defer r.configMu.RUnlock()
		r.mu.RLock()
		index := r.index
		r.mu.RUnlock()
		if index == nil {
			return nil, false
		}
		return r.calculationInputsAtIndex(plan, index, dirty), true
	})
	if len(changed) == 0 {
		return
	}
	identifiers := make([]string, 0, len(changed))
	for _, result := range changed {
		identifiers = append(identifiers, result.NodeID)
	}
	r.refreshCalculationNodes(identifiers)
	if publish {
		r.broker.Publish(api.Event{Type: "calculation_results_changed", Revision: revision})
	}
}

func (r *Runtime) calculationInputsAtIndex(plan *calculation.Plan, index *catalogIndex, names []string) map[string]calculation.Input {
	groups := r.routes.Groups()
	defaults := make(map[string]string, len(groups))
	for _, group := range groups {
		defaults[group.ID] = group.DefaultSourceID
	}
	inputs := make(map[string]calculation.Input, len(names))
	sources := r.routes.Sources()
	for _, name := range names {
		if state, ok := plan.StateInput(name); ok {
			inputs[name] = stateViewInput(r.stateInputValue(state, groups, sources))
			continue
		}
		point, exists := index.pointForNode(name)
		definition, hasDefinition := plan.InputDefinition(name)
		dataType := definition.DataType
		if !hasDefinition || dataType == "" {
			dataType = domain.DataTypeDouble
		}
		if !exists || point.Node.Kind == "manager_calculation" {
			inputs[name] = calculation.Input{Type: dataType, Value: zeroValue(dataType, -1), StatusCode: domain.StatusBadConfigurationError}
			continue
		}
		dataType = point.Node.DataType
		sourceID := defaults[point.GroupID]
		if sourceID == "" {
			inputs[name] = calculation.Input{Type: dataType, Value: zeroValue(dataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
			continue
		}
		sample := r.sampleForSourceAtIndex(sourceID, point, index)
		inputs[name] = calculation.InputFromSample(sample, dataType)
	}
	return inputs
}

func (r *Runtime) refreshCalculationNodes(identifiers []string) {
	if len(identifiers) == 0 {
		return
	}
	r.mu.RLock()
	refresh := r.refreshCalculations
	r.mu.RUnlock()
	if refresh != nil {
		refresh(identifiers)
	}
}

func (r *Runtime) flushCalculationEvent() {
	if revision, pending := r.calculations.takeResultEvent(); pending {
		r.broker.Publish(api.Event{Type: "calculation_results_changed", Revision: revision})
	}
}

func (r *Runtime) calculationSample(identifier string) (domain.Sample, bool) {
	plan, _ := r.calculations.definition()
	if plan != nil {
		if input, exists := plan.StateInput(identifier); exists {
			view := r.stateInputValue(input, r.routes.Groups(), r.routes.Sources())
			return domain.Sample{Type: domain.DataTypeUInt32, Value: view.Value, StatusCode: view.StatusCode, SourceTimestamp: view.ObservedAt}, true
		}
	}
	return r.calculations.sample(identifier)
}

var _ api.CalculationService = (*Runtime)(nil)
