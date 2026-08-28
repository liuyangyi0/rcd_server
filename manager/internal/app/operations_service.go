package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"rcd_manager/internal/store"
)

func (r *Runtime) persistLowerStatusTransition(previous, current opc.LowerStatus, existed bool) {
	if !existed {
		previous = opc.LowerStatus{SourceID: current.SourceID}
	}
	type transition struct{ kind, before, after string }
	transitions := make([]transition, 0, 5)
	if previous.Connection != current.Connection {
		transitions = append(transitions, transition{"connection", string(previous.Connection), string(current.Connection)})
	}
	if previous.Session != current.Session {
		transitions = append(transitions, transition{"session", string(previous.Session), string(current.Session)})
	}
	if previous.Subscription != current.Subscription {
		transitions = append(transitions, transition{"subscription", string(previous.Subscription), string(current.Subscription)})
	}
	if previous.Catalog != current.Catalog {
		transitions = append(transitions, transition{"catalog", string(previous.Catalog), string(current.Catalog)})
	}
	if previous.LastError != current.LastError {
		transitions = append(transitions, transition{"error", previous.LastError, current.LastError})
	}

	occurredAt := time.Now().UTC()
	events := make([]domain.SourceEvent, 0, len(transitions))
	for _, change := range transitions {
		events = append(events, domain.SourceEvent{SourceID: current.SourceID, Kind: change.kind,
			Previous: change.before, Current: change.after, Message: current.LastError, OccurredAt: occurredAt})
	}
	r.statusPersistMu.Lock()
	r.dirtySourceEvents = append(r.dirtySourceEvents, events...)
	// Alarm rows represent current conditions, so a newer status supersedes an
	// older retry candidate for the same source. Event history remains ordered
	// and is never discarded on a transient SQLite failure.
	r.dirtyAlarmStatuses[current.SourceID] = current
	r.statusPersistMu.Unlock()
	// Lower status callbacks are part of the OPC connection supervision path.
	// Never make them wait for SQLite; a single worker drains the durable queue.
	select {
	case r.statusPersistWake <- struct{}{}:
	default:
	}
}

func (r *Runtime) retryDirtyStatuses(ctx context.Context) {
	r.statusRetryMu.Lock()
	defer r.statusRetryMu.Unlock()
	var persistErr error
	persistedEvents := false
	persistedAlarms := false
	for {
		r.statusPersistMu.Lock()
		if len(r.dirtySourceEvents) == 0 {
			r.dirtySourceEvents = nil
			r.statusPersistMu.Unlock()
			break
		}
		event := r.dirtySourceEvents[0]
		r.statusPersistMu.Unlock()
		if _, err := r.repo.AppendSourceEvent(ctx, event); err != nil {
			persistErr = err
			break
		}
		persistedEvents = true
		r.statusPersistMu.Lock()
		r.dirtySourceEvents[0] = domain.SourceEvent{}
		r.dirtySourceEvents = r.dirtySourceEvents[1:]
		r.statusPersistMu.Unlock()
	}
	if persistErr == nil {
		r.statusPersistMu.Lock()
		sourceIDs := make([]string, 0, len(r.dirtyAlarmStatuses))
		for sourceID := range r.dirtyAlarmStatuses {
			sourceIDs = append(sourceIDs, sourceID)
		}
		r.statusPersistMu.Unlock()
		sort.Strings(sourceIDs)
		for _, sourceID := range sourceIDs {
			r.statusPersistMu.Lock()
			status := r.dirtyAlarmStatuses[sourceID]
			r.statusPersistMu.Unlock()
			if err := r.reconcileStatusAlarms(ctx, status); err != nil {
				persistErr = err
				break
			}
			persistedAlarms = true
			r.statusPersistMu.Lock()
			// A newer callback may have replaced this source's desired alarm
			// state while SQLite was busy. Remove only the state just reconciled.
			if current, exists := r.dirtyAlarmStatuses[sourceID]; exists && lowerStatusEqualForAlarms(current, status) {
				delete(r.dirtyAlarmStatuses, sourceID)
			}
			r.statusPersistMu.Unlock()
		}
	}
	r.mu.Lock()
	if persistErr != nil {
		r.statusPersistError = persistErr.Error()
	} else {
		r.statusPersistError = ""
	}
	r.mu.Unlock()
	if persistedEvents {
		r.broker.Publish(api.Event{Type: "source.event"})
	}
	if persistedAlarms {
		r.broker.Publish(api.Event{Type: "alarm.changed"})
	}
}

func lowerStatusEqualForAlarms(left, right opc.LowerStatus) bool {
	return left.SourceID == right.SourceID && left.Connection == right.Connection && left.Session == right.Session &&
		left.Subscription == right.Subscription && left.Catalog == right.Catalog && left.LastError == right.LastError
}

func (r *Runtime) reconcileStatusAlarms(ctx context.Context, status opc.LowerStatus) error {
	operational := false
	if source, err := r.source(status.SourceID); err == nil {
		operational = source.Enabled
	}
	connectionActive := operational && status.Connection == opc.ConnectionDisconnected && strings.TrimSpace(status.LastError) != ""
	// Connecting is an intermediate retry state. Keep an existing connection
	// alarm open until a real Connected status arrives, rather than resolving
	// and reopening it on every backoff cycle.
	reconcileConnection := !operational || status.Connection != opc.ConnectionConnecting
	connectionMessage := status.LastError
	if connectionMessage == "" {
		connectionMessage = "下级 OPC 连接已断开"
	}
	catalogActive := operational && status.Catalog == domain.CatalogConflict
	catalogMessage := "下级目录与分组点表存在冲突"
	if catalogActive {
		r.mu.RLock()
		if reason := r.quarantinedSources[status.SourceID]; reason != "" {
			catalogMessage = "目录被隔离：" + reason
		} else if r.index != nil {
			if conflicts := conflictsForSource(r.index.union.Conflicts, status.SourceID); len(conflicts) > 0 {
				catalogMessage = catalogConflictSummary(conflicts)
			}
		}
		r.mu.RUnlock()
	}
	subscriptionActive := operational && status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive && status.Subscription == opc.SubscriptionInactive

	conditions := []struct {
		kind      string
		severity  domain.AlarmSeverity
		message   string
		active    bool
		reconcile bool
	}{
		{"source.connection", domain.AlarmSeverityCritical, connectionMessage, connectionActive, reconcileConnection},
		{"source.catalog", domain.AlarmSeverityCritical, catalogMessage, catalogActive, true},
		{"source.subscription", domain.AlarmSeverityWarning, "下级 OPC 已连接但订阅未激活", subscriptionActive, true},
	}
	updates := make([]store.AlarmCondition, 0, len(conditions))
	for _, condition := range conditions {
		if !condition.reconcile {
			continue
		}
		updates = append(updates, store.AlarmCondition{SourceID: status.SourceID, Kind: condition.kind,
			Severity: condition.severity, Message: condition.message, Active: condition.active})
	}
	return r.repo.ReconcileAlarms(ctx, updates)
}

func (r *Runtime) ListAlarms(ctx context.Context, request api.PageRequest) (api.AlarmPage, error) {
	limit, offset := normalizePage(request.Limit, request.Offset)
	items, total, err := r.repo.ListAlarms(ctx, store.AlarmFilter{SourceID: request.SourceID, State: request.Status,
		Query: request.Query, Limit: limit, Offset: offset})
	return api.AlarmPage{Items: items, Total: total, Limit: limit, Offset: offset}, err
}

func (r *Runtime) AcknowledgeAlarm(ctx context.Context, id int64, revision uint64) (domain.Alarm, error) {
	actor := requestActor(ctx)
	alarm, err := r.repo.AcknowledgeAlarm(ctx, id, revision, actor)
	if err != nil {
		return domain.Alarm{}, err
	}
	r.audit(ctx, "alarm.acknowledge", fmt.Sprint(id), alarm.Revision, map[string]any{"source_id": alarm.SourceID})
	r.broker.Publish(api.Event{Type: "alarm.changed", SourceID: alarm.SourceID, Revision: alarm.Revision, Data: alarm})
	return alarm, nil
}

func (r *Runtime) SilenceAlarm(ctx context.Context, id int64, revision uint64, until time.Time) (domain.Alarm, error) {
	if !until.IsZero() && until.Before(time.Now().UTC()) {
		return domain.Alarm{}, &domain.FieldError{Field: "silenced_until", Message: "must be in the future or empty"}
	}
	alarm, err := r.repo.SilenceAlarm(ctx, id, revision, until.UTC())
	if err != nil {
		return domain.Alarm{}, err
	}
	r.audit(ctx, "alarm.silence", fmt.Sprint(id), alarm.Revision, map[string]any{"source_id": alarm.SourceID, "until": until})
	r.broker.Publish(api.Event{Type: "alarm.changed", SourceID: alarm.SourceID, Revision: alarm.Revision, Data: alarm})
	return alarm, nil
}

func (r *Runtime) ListSourceEvents(ctx context.Context, request api.PageRequest) (api.SourceEventPage, error) {
	limit, offset := normalizePage(request.Limit, request.Offset)
	items, total, err := r.repo.ListSourceEvents(ctx, store.SourceEventFilter{SourceID: request.SourceID, Kind: request.Kind,
		Query: request.Query, Limit: limit, Offset: offset})
	return api.SourceEventPage{Items: items, Total: total, Limit: limit, Offset: offset}, err
}

func (r *Runtime) ListManagementAudit(ctx context.Context, request api.PageRequest) (api.AuditPage, error) {
	limit, offset := normalizePage(request.Limit, request.Offset)
	items, total, err := r.repo.ListAuditFiltered(ctx, store.AuditFilter{Kind: request.Kind, Query: request.Query, Limit: limit, Offset: offset})
	return api.AuditPage{Items: items, Total: total, Limit: limit, Offset: offset}, err
}

func (r *Runtime) ProbeSource(ctx context.Context, id string) (api.SourceProbeResult, error) {
	source, err := r.source(id)
	if err != nil {
		return api.SourceProbeResult{}, err
	}
	_, password, err := r.repo.SourceCredential(ctx, id)
	if err != nil {
		return api.SourceProbeResult{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result, err := opc.ProbeLower(probeCtx, opc.LowerConfig{Source: source, Password: password,
		CertificateFile: r.options.CertificateFile, PrivateKeyFile: r.options.PrivateKeyFile,
		OperationTimeout: r.options.CommandTimeout, ConnectTimeout: 10 * time.Second})
	if err != nil {
		return api.SourceProbeResult{}, err
	}
	view := api.SourceProbeResult{SourceID: source.ID, Endpoint: source.Endpoint, OK: result.Error == "", Error: result.Error,
		EndpointCount: result.EndpointCount, NamespaceCount: result.NamespaceCount, VariableCount: result.VariableCount,
		MethodCount: result.MethodCount, DurationMS: result.DurationMS, TestedAt: result.TestedAt,
		Stages: make([]api.SourceProbeStage, len(result.Stages))}
	for index, stage := range result.Stages {
		view.Stages[index] = api.SourceProbeStage{Name: stage.Name, OK: stage.OK, Message: stage.Message, DurationMS: stage.DurationMS}
	}
	r.audit(ctx, "source.probe", id, source.Revision, map[string]any{"ok": view.OK, "error": view.Error})
	return view, nil
}

func (r *Runtime) RestartSource(ctx context.Context, id string, revision uint64, rediscover bool) (api.SourceView, error) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return api.SourceView{}, domain.ErrShuttingDown
	}
	source, err := r.source(id)
	if err != nil {
		return api.SourceView{}, err
	}
	if source.Revision != revision {
		return api.SourceView{}, &domain.RevisionConflictError{Expected: revision, Actual: source.Revision}
	}
	if !source.Enabled {
		return api.SourceView{}, &domain.FieldError{Field: "enabled", Message: "disabled source cannot be restarted"}
	}
	if r.sourceGroupsPending(source) {
		return api.SourceView{}, domain.ErrGroupPending
	}
	r.mu.Lock()
	status := r.statuses[id]
	status.SourceID = id
	status.ReconnectCount++
	status.UpdatedAt = time.Now().UTC()
	r.statuses[id] = status
	r.mu.Unlock()
	r.stopLower(id)
	if err := r.startLower(source); err != nil {
		r.recordLowerStartFailure(source, err)
		return api.SourceView{}, err
	}
	kind := "source.reconnect"
	if rediscover {
		kind = "source.rediscover"
	}
	r.audit(ctx, kind, id, source.Revision, nil)
	r.broker.Publish(api.Event{Type: "source.status", SourceID: id, Data: r.sourceView(source)})
	return r.sourceView(source), nil
}

func requestActor(ctx context.Context) string {
	info := api.RequestInfoFromContext(ctx)
	if strings.TrimSpace(info.Actor) != "" {
		return info.Actor
	}
	return "manager"
}
