package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/store"
)

var _ api.ProcessControlService = (*Runtime)(nil)

func (r *Runtime) checkProcessMaintenanceLocked(sourceID string) error {
	if r.release.Active {
		return &domain.OperationError{Code: "busy", Message: "管理端正在软件维护，停止接收新控制命令"}
	}
	return r.maintenance.check(sourceID, true)
}

func (r *Runtime) SourceProcessBinding(ctx context.Context, id string) (domain.ProcessBinding, error) {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	binding, err := r.repo.GetProcessBinding(ctx, id)
	if err == nil {
		if row, _, e := r.repo.FleetBinding(ctx, id); e == nil {
			binding.RegistryDeviceID = row.DeviceID
		}
	}
	return binding, err
}

func (r *Runtime) SaveSourceProcessBinding(ctx context.Context, binding domain.ProcessBinding, revision uint64) (domain.ProcessBinding, error) {
	if _, _, err := r.repo.FleetBinding(ctx, binding.SourceID); err == nil {
		return domain.ProcessBinding{}, &domain.OperationError{Code: "conflict", Message: "守护绑定由发布平台统一台账管理，请在平台修改"}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.ProcessBinding{}, err
	}
	binding.AgentURL = domain.NormalizeProcessAgentURL(binding.AgentURL)
	if err := binding.Validate(); err != nil {
		return domain.ProcessBinding{}, err
	}
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return domain.ProcessBinding{}, domain.ErrShuttingDown
	}
	err := r.checkSourceOperationLocked(binding.SourceID)
	if err == nil && r.routes.SourcePendingCount(binding.SourceID) != 0 {
		err = domain.ErrCommandPending
	}
	var result domain.ProcessBinding
	if err == nil {
		result, err = r.repo.SaveProcessBinding(ctx, binding, revision)
	}
	r.configMu.Unlock()
	if err == nil {
		r.audit(ctx, "source.process.binding", binding.SourceID, binding.SourceRevision, map[string]any{
			"binding_revision": result.Revision, "instance_id": result.InstanceID, "agent_url": result.AgentURL, "configured": result.Configured(),
		})
		r.broker.Publish(api.Event{Type: "source.process", SourceID: binding.SourceID, Data: map[string]any{"action": "binding"}})
	}
	return result, err
}

func (r *Runtime) processBindingSnapshot(ctx context.Context, sourceID string) (domain.ProcessBinding, *domain.ProcessOperation, error) {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	binding, err := r.repo.GetProcessBinding(ctx, sourceID)
	if err != nil {
		return binding, nil, err
	}
	if !binding.Configured() {
		return binding, nil, processUnsupported()
	}
	var pending *domain.ProcessOperation
	if requestID := r.maintenance.pending(sourceID); requestID != "" {
		record, err := r.repo.GetProcessOperation(ctx, sourceID, requestID)
		if err != nil {
			return binding, nil, err
		}
		if !record.Operation.Terminal() {
			pending = &record.Operation
		}
	}
	return binding, pending, nil
}

func (r *Runtime) SourceProcessStatus(ctx context.Context, id string) (domain.ProcessStatusView, error) {
	binding, pending, err := r.processBindingSnapshot(ctx, id)
	if err != nil {
		return domain.ProcessStatusView{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, processRequestTimeout)
	defer cancel()
	snapshot, err := r.options.ProcessAgentClient.Status(requestCtx, binding)
	if err == nil {
		err = snapshot.Validate(binding.InstanceID)
		if _, _, e := r.repo.FleetBinding(ctx, id); err == nil && e != nil && !errors.Is(e, domain.ErrNotFound) {
			err = e
		}
		if row, _, e := r.repo.FleetBinding(ctx, id); err == nil && e == nil && row.Machine != nil && snapshot.GatewayInstanceID != row.Machine.GatewayInstanceID {
			err = processInvalidResponse("Agent 下级身份与统一台账不符")
		}
	}
	return domain.ProcessStatusView{ProcessSnapshot: snapshot, SourceID: id, SourceRevision: binding.SourceRevision,
		BindingRevision: binding.Revision, PendingOperation: pending}, err
}

func (r *Runtime) SourceProcessLog(ctx context.Context, id string, limit int) (domain.ProcessLogView, error) {
	if limit < 4 || limit > domain.MaxProcessLogBytes {
		return domain.ProcessLogView{}, &domain.FieldError{Field: "limit", Message: "must be between 4 and 65536"}
	}
	binding, _, err := r.processBindingSnapshot(ctx, id)
	if err != nil {
		return domain.ProcessLogView{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, processRequestTimeout)
	defer cancel()
	result, err := r.options.ProcessAgentClient.Log(requestCtx, binding, limit)
	if err == nil && (result.InstanceID != binding.InstanceID || result.SizeBytes < 0 || len(result.Text) > limit) {
		err = processInvalidResponse("进程日志身份或长度无效")
	}
	return domain.ProcessLogView{ProcessLog: result, SourceID: id, SourceRevision: binding.SourceRevision, BindingRevision: binding.Revision}, err
}

func (r *Runtime) ListSourceProcessOperations(ctx context.Context, id string, limit, offset int) (domain.ProcessOperationPage, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	source, err := r.source(id)
	if err != nil {
		return domain.ProcessOperationPage{}, err
	}
	records, total, err := r.repo.ListProcessOperations(ctx, id, limit, offset)
	page := domain.ProcessOperationPage{SourceID: id, SourceRevision: source.Revision, Items: make([]domain.ProcessOperation, 0, len(records)), Total: total, Limit: limit, Offset: offset}
	for _, record := range records {
		page.Items = append(page.Items, record.Operation)
	}
	return page, err
}

func validateProcessInput(input domain.ProcessActionInput, revision string) error {
	if input.SourceRevision == 0 || input.BindingRevision == 0 {
		return &domain.FieldError{Field: "revision", Message: "source_revision and binding_revision are required"}
	}
	if err := (domain.ProcessActionRequest{RequestID: input.RequestID, InstanceID: "validation", Action: input.Action, ExpectedRevision: revision}).Validate(); err != nil {
		return err
	}
	if len(input.ExpectedConfigRevisions) != 0 && input.Action != "restart" {
		return &domain.FieldError{Field: "expected_config_revisions", Message: "configuration preconditions are only valid for restart"}
	}
	if len(input.ExpectedConfigRevisions) > 256 {
		return &domain.FieldError{Field: "expected_config_revisions", Message: "too many configuration files"}
	}
	for name, revision := range input.ExpectedConfigRevisions {
		if !domain.ValidOperationResourceName(name) || (name != "config.toml" && name != "calc.toml" && !(strings.HasPrefix(name, "rcd") && strings.HasSuffix(name, ".csv"))) {
			return &domain.FieldError{Field: "expected_config_revisions", Message: "select a listed configuration filename"}
		}
		if len(revision) != 64 || strings.ToLower(revision) != revision {
			return &domain.FieldError{Field: "expected_config_revisions", Message: "configuration revision must be a lowercase SHA256"}
		}
		if _, err := hex.DecodeString(revision); err != nil {
			return &domain.FieldError{Field: "expected_config_revisions", Message: "configuration revision must be a SHA256"}
		}
	}
	return nil
}

func (r *Runtime) ExecuteSourceProcess(ctx context.Context, id, revision string, input domain.ProcessActionInput) (domain.ProcessOperation, error) {
	return r.admitSourceProcess(ctx, id, revision, input, false)
}

// Resolve can establish a manager-side tombstone before a delayed original
// HTTP Action arrives. It shares admission and the exact durable fingerprint;
// its agent call never executes a process action.
func (r *Runtime) ResolveSourceProcess(ctx context.Context, id, revision string, input domain.ProcessActionInput) (domain.ProcessOperation, error) {
	return r.admitSourceProcess(ctx, id, revision, input, true)
}

func (r *Runtime) admitSourceProcess(ctx context.Context, id, revision string, input domain.ProcessActionInput, resolveOnly bool) (domain.ProcessOperation, error) {
	if err := validateProcessInput(input, revision); err != nil {
		return domain.ProcessOperation{}, err
	}
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, domain.ErrShuttingDown
	}
	if _, err := r.source(id); err != nil {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	existing, err := r.repo.GetProcessOperation(ctx, id, input.RequestID)
	if err == nil {
		request := existing.Request
		request.RequestID, request.InstanceID, request.Action, request.ExpectedRevision = input.RequestID, existing.Binding.InstanceID, input.Action, revision
		if !existing.SameRequest(input.SourceRevision, input.BindingRevision, request, input.ExpectedConfigRevisions) {
			r.configMu.Unlock()
			return domain.ProcessOperation{}, &domain.OperationError{Code: "conflict", Message: "request_id 已用于不同的目标版本或操作，不得改写原请求"}
		}
		r.configMu.Unlock()
		// Repeated Action is a lookup, never a redispatch. Resolve may reconcile
		// the already-recorded request but cannot cancel an accepted agent job.
		if resolveOnly && !existing.Operation.Terminal() {
			return r.SourceProcessOperation(ctx, id, input.RequestID)
		}
		return existing.Operation, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	binding, err := r.repo.GetProcessBinding(ctx, id)
	if err == nil && (binding.SourceRevision != input.SourceRevision || binding.Revision != input.BindingRevision) {
		err = &domain.OperationError{Code: "conflict", Message: "来源或进程绑定版本已变化，请重新核对目标"}
	}
	if err == nil && !binding.Configured() {
		err = processUnsupported()
	}
	if err == nil {
		err = r.checkSourceOperationLocked(id)
	}
	if err == nil && r.routes.SourcePendingCount(id) != 0 {
		err = domain.ErrCommandPending
	}
	if err != nil {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	request := domain.ProcessActionRequest{RequestID: input.RequestID, InstanceID: binding.InstanceID, Action: input.Action, ExpectedRevision: revision}
	if prepare, ok := r.options.ProcessAgentClient.(interface {
		Prepare(context.Context, domain.ProcessBinding, domain.ProcessActionRequest) (domain.ProcessActionRequest, error)
	}); ok && !resolveOnly {
		if len(input.ExpectedConfigRevisions) > 0 {
			guard, e := json.Marshal(input.ExpectedConfigRevisions)
			if e != nil {
				r.configMu.Unlock()
				return domain.ProcessOperation{}, e
			}
			request.ConfigurationGuard = string(guard)
		}
		request.ManagerInstanceID, request.Actor = r.release.InstanceID, requestActor(ctx)
		r.configMu.Unlock()
		probeCtx, probeCancel := context.WithTimeout(ctx, processRequestTimeout)
		request, err = prepare.Prepare(probeCtx, binding, request)
		probeCancel()
		r.configMu.Lock()
		if r.closing.Load() {
			err = domain.ErrShuttingDown
		}
		if err == nil {
			row, _, e := r.repo.FleetBinding(ctx, id)
			if e == nil && row.Machine != nil && request.GatewayInstanceID != row.Machine.GatewayInstanceID {
				err = processInvalidResponse("下级身份与固定台账不符，未派发")
			} else if e != nil && !errors.Is(e, domain.ErrNotFound) {
				err = e
			}
		}
		if err == nil {
			latest, e := r.repo.GetProcessBinding(ctx, id)
			if e != nil {
				err = e
			} else if latest.SourceRevision != binding.SourceRevision || latest.Revision != binding.Revision {
				err = domain.ErrRevisionConflict
			}
		}
		if err == nil {
			err = r.checkSourceOperationLocked(id)
		}
		if err == nil && r.routes.SourcePendingCount(id) != 0 {
			err = domain.ErrCommandPending
		}
		if err != nil {
			r.configMu.Unlock()
			return domain.ProcessOperation{}, err
		}
		if existing, e := r.repo.GetProcessOperation(ctx, id, input.RequestID); e == nil {
			r.configMu.Unlock()
			// A resolve-first tombstone can win while Prepare is probing the
			// Agent. Compare the original caller input, not freshly discovered
			// lifecycle identity, and never dispatch over that durable result.
			original := existing.Request
			original.RequestID, original.InstanceID, original.Action, original.ExpectedRevision = request.RequestID, request.InstanceID, request.Action, request.ExpectedRevision
			if !existing.SameRequest(input.SourceRevision, input.BindingRevision, original, input.ExpectedConfigRevisions) {
				return domain.ProcessOperation{}, domain.ErrRevisionConflict
			}
			return existing.Operation, nil
		} else if !errors.Is(e, domain.ErrNotFound) {
			r.configMu.Unlock()
			return domain.ProcessOperation{}, e
		}
	}
	info, now := api.RequestInfoFromContext(ctx), time.Now().UTC()
	configs := make(map[string]string, len(input.ExpectedConfigRevisions))
	for name, hash := range input.ExpectedConfigRevisions {
		configs[name] = hash
	}
	record := store.ProcessControlRecord{Binding: binding, Request: request, ExpectedConfigRevisions: configs,
		Actor: requestActor(ctx), RemoteAddress: info.RemoteAddress, UserAgent: info.UserAgent,
		Operation: domain.ProcessOperation{SourceID: id, SourceRevision: binding.SourceRevision, BindingRevision: binding.Revision,
			RequestID: request.RequestID, InstanceID: request.InstanceID, Action: request.Action, State: "accepted", CreatedAt: now, UpdatedAt: now}}
	if resolveOnly {
		// No manager record means this manager never dispatched the request:
		// dispatch is always preceded by a durable intent. Seal the original
		// input locally while holding the admission lock. Do not guess a new
		// Agent identity or call the legacy remote resolve endpoint. A late
		// Action (including one currently in Prepare) reads this tombstone.
		record.Operation.State, record.Operation.ErrorCode = "failed", "unconfirmed_cancelled"
		record.Operation.Error = "未找到管理端作业登记，已封存该编号；未向下级派发进程操作"
		err = r.repo.CreateProcessOperation(ctx, record)
		r.configMu.Unlock()
		if err != nil {
			return domain.ProcessOperation{}, err
		}
		r.auditProcess(record, "source.process.terminal")
		r.broker.Publish(api.Event{Type: "source.process", SourceID: id, Data: record.Operation})
		return record.Operation, nil
	}
	if err = r.maintenance.beginProcess(id, request.RequestID); err != nil {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	// The durable intent is written before any request can leave this manager.
	// Network I/O begins only after configMu is released.
	err = r.repo.CreateProcessOperation(ctx, record)
	if err != nil {
		r.maintenance.releaseFence(id, request.RequestID)
		r.maintenance.endExecution(id, request.RequestID)
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	var lowerTarget lowerOperationsTarget
	var preflightErr error
	if len(configs) > 0 {
		lowerTarget, preflightErr = r.lowerOperationsTargetLocked(id)
	}
	if r.running.Load() {
		// Close waits for configMu before WaitGroup.Wait, so no admission can
		// race a late Add against shutdown's waiter.
		r.wg.Add(1)
		defer r.wg.Done()
	}
	r.configMu.Unlock()
	defer r.endProcessExecution(id, request.RequestID)
	r.auditProcess(record, "source.process.accepted")
	// A disconnected browser must not abandon a durably accepted intent between
	// validation and dispatch. This bounded call completes its journal update;
	// subsequent work uses GET/Resolve, never a new Action.
	operationCtx, cancel := r.processOperationContext(ctx)
	defer cancel()
	if preflightErr == nil && len(configs) > 0 {
		preflightErr = r.checkProcessConfiguration(operationCtx, lowerTarget, configs)
	}
	if preflightErr != nil {
		record = failProcessRecord(record, preflightErr, "precondition_failed")
		saved, saveErr := r.persistProcessRecord(record)
		if saveErr != nil {
			return saved.Operation, saveErr
		}
		return saved.Operation, preflightErr
	}
	record.Dispatched = true
	record, err = r.persistProcessRecord(record)
	if err != nil {
		return record.Operation, err
	}
	if record.Operation.Terminal() {
		return record.Operation, nil
	}
	var operation domain.ProcessOperation
	dispatchCtx, dispatchCancel := context.WithTimeout(operationCtx, processRequestTimeout)
	defer dispatchCancel()
	operation, err = r.options.ProcessAgentClient.Action(dispatchCtx, binding, request)
	if err == nil {
		err = operation.Validate(request)
	}
	if err != nil {
		// Only explicit business rejection of an Action is proof no job began.
		if processDefinitiveRejection(err) {
			record = failProcessRecord(record, err, "rejected")
		} else {
			record = unknownProcessRecord(record, err)
		}
	} else {
		record.Operation = bindProcessOperation(operation, record)
	}
	saved, saveErr := r.persistProcessRecord(record)
	if saveErr != nil {
		return saved.Operation, saveErr
	}
	return saved.Operation, err
}

func (r *Runtime) checkProcessConfiguration(ctx context.Context, target lowerOperationsTarget, expected map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, lowerOperationsTimeout)
	defer cancel()
	capabilities, err := target.client.OperationsCapabilities(ctx, target.epoch)
	if err != nil {
		return normalizeLowerOperationError(ctx, err, false)
	}
	if !capabilities.Supported || !capabilities.HasFeature("configuration") {
		return &domain.OperationError{Code: "unsupported", Message: "下级未提供配置版本检查能力，不能应用已保存配置"}
	}
	content, err := target.client.ExecuteOperation(ctx, domain.OperationRequest{Action: "list_configs"}, target.epoch)
	if err != nil {
		return normalizeLowerOperationError(ctx, err, false)
	}
	var result struct {
		Items []struct {
			Name     string `json:"name"`
			Revision string `json:"revision"`
		} `json:"items"`
	}
	if len(content) > domain.MaxOperationJSONBytes || json.Unmarshal(content, &result) != nil || result.Items == nil {
		return processInvalidResponse("下级配置目录响应无效")
	}
	actual := make(map[string]string, len(result.Items))
	for _, item := range result.Items {
		if _, duplicate := actual[item.Name]; duplicate {
			return processInvalidResponse("下级配置目录出现重复文件")
		}
		actual[item.Name] = item.Revision
	}
	for name, revision := range expected {
		if actual[name] != revision {
			return &domain.OperationError{Code: "conflict", Message: "配置 " + name + " 已变化或不存在；请重新读取后再应用", CurrentRevision: actual[name]}
		}
	}
	return nil
}

func (r *Runtime) processOperationContext(requestCtx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), lowerOperationsTimeout+2*processRequestTimeout)
	r.mu.RLock()
	runCtx := r.runContext
	r.mu.RUnlock()
	if runCtx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(runCtx, cancel)
	return ctx, func() { stop(); cancel() }
}

func bindProcessOperation(operation domain.ProcessOperation, record store.ProcessControlRecord) domain.ProcessOperation {
	operation.SourceID, operation.SourceRevision, operation.BindingRevision = record.Binding.SourceID, record.Binding.SourceRevision, record.Binding.Revision
	return operation
}

func processDefinitiveRejection(err error) bool {
	var operationError *domain.OperationError
	if errors.As(err, &operationError) {
		switch operationError.Code {
		case "conflict", "busy", "invalid_request", "unsupported", "too_large":
			return true
		}
	}
	return false
}

func failProcessRecord(record store.ProcessControlRecord, err error, fallback string) store.ProcessControlRecord {
	record.Operation.State, record.Operation.ErrorCode, record.Operation.Error = "failed", fallback, "进程操作未派发"
	var operationError *domain.OperationError
	if errors.As(err, &operationError) {
		record.Operation.ErrorCode, record.Operation.Error = operationError.Code, operationError.Message
	}
	record.Operation.UpdatedAt = processUpdateTime(record.Operation.CreatedAt)
	return record
}

func unknownProcessRecord(record store.ProcessControlRecord, err error) store.ProcessControlRecord {
	code, message := "unknown", "进程操作结果尚未确认，请使用原 request_id 对账"
	var operationError *domain.OperationError
	if errors.As(err, &operationError) {
		code, message = operationError.Code, operationError.Message
	}
	if len(message) > 16384 {
		message = "进程操作结果尚未确认：代理错误信息超限"
	}
	if len(code) > 128 {
		code = "unknown"
	}
	if record.Operation.State != "unknown" || record.Operation.ErrorCode != code || record.Operation.Error != message {
		record.Operation.State, record.Operation.ErrorCode, record.Operation.Error = "unknown", code, message
		record.Operation.UpdatedAt = processUpdateTime(record.Operation.CreatedAt)
	}
	return record
}

func processUpdateTime(created time.Time) time.Time {
	now := time.Now().UTC()
	if now.Before(created) {
		return created
	}
	return now
}

func (r *Runtime) persistProcessRecord(record store.ProcessControlRecord) (store.ProcessControlRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	saved, changed, err := r.repo.UpdateProcessOperation(ctx, record)
	if err != nil {
		// The in-memory fence stays in place even when terminal persistence
		// fails. The durable prior intent allows the worker/restart to reconcile.
		slog.Error("persist process operation", "source_id", record.Binding.SourceID, "request_id", record.Request.RequestID, "error", err)
		return record, fmt.Errorf("persist process operation: %w", err)
	}
	if saved.Operation.Terminal() {
		r.maintenance.releaseFence(saved.Binding.SourceID, saved.Request.RequestID)
	}
	if changed {
		if saved.Operation.Terminal() {
			r.auditProcess(saved, "source.process.terminal")
		} else if saved.Operation.State == "unknown" {
			r.auditProcess(saved, "source.process.unknown")
		}
		r.broker.Publish(api.Event{Type: "source.process", SourceID: saved.Binding.SourceID, Data: saved.Operation})
	}
	return saved, nil
}

func (r *Runtime) auditProcess(record store.ProcessControlRecord, kind string) {
	details := map[string]any{"request_id": record.Request.RequestID, "action": record.Request.Action, "instance_id": record.Binding.InstanceID,
		"binding_revision": record.Binding.Revision, "expected_agent_revision": record.Request.ExpectedRevision, "state": record.Operation.State,
		"forced": record.Operation.Forced, "actor": record.Actor, "remote_address": record.RemoteAddress, "user_agent": record.UserAgent}
	if record.Operation.ErrorCode != "" {
		details["error_code"] = record.Operation.ErrorCode
	}
	if len(record.ExpectedConfigRevisions) > 0 {
		details["expected_config_revisions"] = record.ExpectedConfigRevisions
	}
	r.audit(context.Background(), kind, record.Binding.SourceID, record.Binding.SourceRevision, details)
}

func (r *Runtime) endProcessExecution(id, requestID string) {
	r.maintenance.endExecution(id, requestID)
}

func (r *Runtime) SourceProcessOperation(ctx context.Context, id, requestID string) (domain.ProcessOperation, error) {
	if !domain.ValidOperationRevision(requestID) || requestID == "." || requestID == ".." {
		return domain.ProcessOperation{}, domain.ErrInvalid
	}
	r.configMu.Lock()
	if _, err := r.source(id); err != nil {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	record, err := r.repo.GetProcessOperation(ctx, id, requestID)
	if err != nil {
		r.configMu.Unlock()
		return domain.ProcessOperation{}, err
	}
	if record.Operation.Terminal() || r.closing.Load() || !r.maintenance.tryReconcile(id, requestID) {
		r.configMu.Unlock()
		return record.Operation, nil
	}
	if r.running.Load() {
		r.wg.Add(1)
		defer r.wg.Done()
	}
	r.configMu.Unlock()
	defer r.endProcessExecution(id, requestID)
	return r.reconcileProcess(ctx, record)
}

func (r *Runtime) reconcileProcess(ctx context.Context, record store.ProcessControlRecord) (domain.ProcessOperation, error) {
	if !record.Dispatched {
		record = failProcessRecord(record, nil, "interrupted_before_dispatch")
		saved, err := r.persistProcessRecord(record)
		return saved.Operation, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, 2*processRequestTimeout)
	defer cancel()
	operation, err := r.options.ProcessAgentClient.Operation(queryCtx, record.Binding, record.Request)
	var operationErr *domain.OperationError
	if (errors.As(err, &operationErr) && operationErr.Code == "not_found") || (err == nil && record.Request.Lifecycle && operation.State == "unknown") {
		// A plain 404 is not proof a delayed original Action cannot still
		// arrive. Resolve atomically tombstones it or returns the accepted job.
		// An existing unknown lifecycle job also needs Resolve: GET only reads
		// its journal, while Resolve can confirm a late exit/release without
		// repeating any enter, signal, start, or leave operation.
		operation, err = r.options.ProcessAgentClient.Resolve(queryCtx, record.Binding, record.Request)
	}
	if err == nil {
		err = operation.Validate(record.Request)
	}
	if err != nil {
		record = unknownProcessRecord(record, err)
	} else {
		record.Operation = bindProcessOperation(operation, record)
	}
	saved, saveErr := r.persistProcessRecord(record)
	// Known requests remain visible with state=unknown even when the agent is
	// unreachable. This is distinct from an unknown manager request (404).
	return saved.Operation, saveErr
}

func (r *Runtime) restoreProcessOperations(ctx context.Context) error {
	records, err := r.repo.ListPendingProcessOperations(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if _, err := r.source(record.Binding.SourceID); err != nil {
			return fmt.Errorf("pending process source %s is missing: %w", record.Binding.SourceID, err)
		}
		if err := r.maintenance.restoreFence(record.Binding.SourceID, record.Request.RequestID); err != nil {
			return fmt.Errorf("restore process fence for %s: %w", record.Binding.SourceID, err)
		}
		if !record.Dispatched {
			record = failProcessRecord(record, nil, "interrupted_before_dispatch")
			if _, err := r.persistProcessRecord(record); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runtime) processControlWorker() {
	defer r.wg.Done()
	r.mu.RLock()
	ctx := r.runContext
	r.mu.RUnlock()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pending := r.maintenance.pendingSnapshot()
		// A down agent must not serialize confirmation of every other source.
		// The parent worker is joined at shutdown; each bounded batch joins all
		// its workers, and per-source gates prevent overlapping API polls.
		type job struct{ sourceID, requestID string }
		jobs := make(chan job, len(pending))
		for id, requestID := range pending {
			jobs <- job{id, requestID}
		}
		close(jobs)
		var workers sync.WaitGroup
		for i := 0; i < min(4, len(pending)); i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for item := range jobs {
					if ctx.Err() != nil {
						return
					}
					if _, err := r.SourceProcessOperation(ctx, item.sourceID, item.requestID); err != nil && ctx.Err() == nil {
						slog.Warn("reconcile process operation", "source_id", item.sourceID, "request_id", item.requestID, "error", err)
					}
				}
			}()
		}
		workers.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
