package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"rcd_manager/internal/domain"
)

// The immutable binding and exact request are retained for reconciliation after
// either HTTP disconnects or a manager restart. No executable/path comes from a
// browser request, and this record contains no configuration or log body.
type ProcessControlRecord struct {
	Binding                 domain.ProcessBinding       `json:"binding"`
	Request                 domain.ProcessActionRequest `json:"request"`
	ExpectedConfigRevisions map[string]string           `json:"expected_config_revisions,omitempty"`
	Operation               domain.ProcessOperation     `json:"operation"`
	Dispatched              bool                        `json:"dispatched"`
	Actor                   string                      `json:"actor"`
	RemoteAddress           string                      `json:"remote_address,omitempty"`
	UserAgent               string                      `json:"user_agent,omitempty"`
}

func (r ProcessControlRecord) SameRequest(sourceRevision, bindingRevision uint64, request domain.ProcessActionRequest, configs map[string]string) bool {
	return r.Binding.SourceRevision == sourceRevision && r.Binding.Revision == bindingRevision &&
		r.Request == request && equalConfigRevisions(r.ExpectedConfigRevisions, configs)
}

func equalConfigRevisions(a, b map[string]string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func (r *Repository) GetProcessBinding(ctx context.Context, sourceID string) (domain.ProcessBinding, error) {
	source, err := r.GetSource(ctx, sourceID)
	if err != nil {
		return domain.ProcessBinding{}, err
	}
	binding := domain.ProcessBinding{SourceID: sourceID, SourceRevision: source.Revision}
	var updated string
	err = r.db.QueryRowContext(ctx, `SELECT agent_url, instance_id, revision, updated_at FROM process_bindings WHERE source_id=?`, sourceID).
		Scan(&binding.AgentURL, &binding.InstanceID, &binding.Revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return binding, nil
	}
	if err != nil {
		return domain.ProcessBinding{}, fmt.Errorf("read process binding: %w", err)
	}
	binding.UpdatedAt, err = decodeTime(updated)
	return binding, err
}

func (r *Repository) SaveProcessBinding(ctx context.Context, binding domain.ProcessBinding, expected uint64) (domain.ProcessBinding, error) {
	if err := binding.Validate(); err != nil {
		return domain.ProcessBinding{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ProcessBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var sourceRevision uint64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM sources WHERE id=?`, binding.SourceID).Scan(&sourceRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ProcessBinding{}, domain.ErrNotFound
		}
		return domain.ProcessBinding{}, err
	}
	if sourceRevision != binding.SourceRevision {
		return domain.ProcessBinding{}, &domain.RevisionConflictError{Expected: binding.SourceRevision, Actual: sourceRevision}
	}
	var current uint64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM process_bindings WHERE source_id=?`, binding.SourceID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.ProcessBinding{}, err
	}
	if current != expected {
		return domain.ProcessBinding{}, &domain.RevisionConflictError{Expected: expected, Actual: current}
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_control_operations WHERE source_id=? AND state NOT IN ('succeeded','failed')`, binding.SourceID).Scan(&pending); err != nil {
		return domain.ProcessBinding{}, err
	}
	if pending != 0 {
		return domain.ProcessBinding{}, processBusy()
	}
	binding.Revision = current + 1
	binding.UpdatedAt = r.clock().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO process_bindings(source_id,agent_url,instance_id,revision,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(source_id) DO UPDATE SET agent_url=excluded.agent_url,instance_id=excluded.instance_id,revision=excluded.revision,updated_at=excluded.updated_at`,
		binding.SourceID, binding.AgentURL, binding.InstanceID, binding.Revision, encodeTime(binding.UpdatedAt))
	if isConstraint(err) {
		return domain.ProcessBinding{}, &domain.OperationError{Code: "conflict", Message: "该进程实例标识已绑定其他来源，不能通过地址别名重复绑定"}
	}
	if err != nil {
		return domain.ProcessBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ProcessBinding{}, err
	}
	return binding, nil
}

func (r *Repository) CreateProcessOperation(ctx context.Context, record ProcessControlRecord) error {
	if err := validateProcessRecord(record); err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var sourceRevision, bindingRevision uint64
	var agentURL, instanceID string
	if err := tx.QueryRowContext(ctx, `SELECT s.revision,b.revision,b.agent_url,b.instance_id FROM sources s JOIN process_bindings b ON b.source_id=s.id WHERE s.id=?`, record.Binding.SourceID).
		Scan(&sourceRevision, &bindingRevision, &agentURL, &instanceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if sourceRevision != record.Binding.SourceRevision || bindingRevision != record.Binding.Revision ||
		agentURL != record.Binding.AgentURL || instanceID != record.Binding.InstanceID {
		return &domain.OperationError{Code: "conflict", Message: "来源或进程绑定已变化，请重新核对目标"}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO process_control_operations(source_id,request_id,state,dispatched,record,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		record.Binding.SourceID, record.Request.RequestID, record.Operation.State, boolInt(record.Dispatched), string(payload),
		encodeTime(record.Operation.CreatedAt), encodeTime(record.Operation.UpdatedAt))
	if isConstraint(err) {
		return processBusy()
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) GetProcessOperation(ctx context.Context, sourceID, requestID string) (ProcessControlRecord, error) {
	return scanProcessRecord(r.db.QueryRowContext(ctx, `SELECT record FROM process_control_operations WHERE source_id=? AND request_id=?`, sourceID, requestID))
}

// UpdateProcessOperation never regresses a durable terminal result. The returned
// record, rather than the caller's candidate, is the authoritative audit state.
func (r *Repository) UpdateProcessOperation(ctx context.Context, candidate ProcessControlRecord) (ProcessControlRecord, bool, error) {
	if err := validateProcessRecord(candidate); err != nil {
		return ProcessControlRecord{}, false, err
	}
	// A conditional single UPDATE avoids deferred-transaction read->write lock
	// upgrades when several independent agents complete concurrently. The exact
	// prior JSON is the CAS token, retaining terminal stickiness across writers.
	for attempt := 0; attempt < 8; attempt++ {
		var before string
		err := r.db.QueryRowContext(ctx, `SELECT record FROM process_control_operations WHERE source_id=? AND request_id=?`,
			candidate.Binding.SourceID, candidate.Request.RequestID).Scan(&before)
		if errors.Is(err, sql.ErrNoRows) {
			return ProcessControlRecord{}, false, domain.ErrNotFound
		}
		if err != nil {
			return ProcessControlRecord{}, false, err
		}
		current, err := decodeProcessRecord(before)
		if err != nil {
			return ProcessControlRecord{}, false, err
		}
		if current.Request != candidate.Request || !reflect.DeepEqual(current.Binding, candidate.Binding) ||
			!equalConfigRevisions(current.ExpectedConfigRevisions, candidate.ExpectedConfigRevisions) ||
			current.Actor != candidate.Actor || current.RemoteAddress != candidate.RemoteAddress || current.UserAgent != candidate.UserAgent {
			return ProcessControlRecord{}, false, &domain.OperationError{Code: "conflict", Message: "进程作业的固定目标、发起人或请求不允许改变"}
		}
		if current.Operation.Terminal() {
			return current, false, nil
		}
		// Dispatched is a write-ahead marker; uncertainty cannot clear it.
		candidate.Dispatched = candidate.Dispatched || current.Dispatched
		if reflect.DeepEqual(current, candidate) {
			return current, false, nil
		}
		payload, err := json.Marshal(candidate)
		if err != nil {
			return ProcessControlRecord{}, false, err
		}
		result, err := r.db.ExecContext(ctx, `UPDATE process_control_operations SET state=?,dispatched=?,record=?,updated_at=?
			WHERE source_id=? AND request_id=? AND record=? AND state NOT IN ('succeeded','failed')`,
			candidate.Operation.State, boolInt(candidate.Dispatched), string(payload), encodeTime(candidate.Operation.UpdatedAt),
			candidate.Binding.SourceID, candidate.Request.RequestID, before)
		if err != nil {
			return ProcessControlRecord{}, false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return ProcessControlRecord{}, false, err
		}
		if changed == 1 {
			return candidate, true, nil
		}
	}
	return ProcessControlRecord{}, false, &domain.OperationError{Code: "busy", Message: "进程作业同时更新，请按原请求核对结果"}
}

func (r *Repository) ListPendingProcessOperations(ctx context.Context) ([]ProcessControlRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT record FROM process_control_operations WHERE state NOT IN ('succeeded','failed') ORDER BY created_at,request_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ProcessControlRecord, 0)
	for rows.Next() {
		record, err := scanProcessRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *Repository) ListProcessOperations(ctx context.Context, sourceID string, limit, offset int) ([]ProcessControlRecord, int, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_control_operations WHERE source_id=?`, sourceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT record FROM process_control_operations WHERE source_id=? ORDER BY created_at DESC,request_id DESC LIMIT ? OFFSET ?`, sourceID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]ProcessControlRecord, 0)
	for rows.Next() {
		record, err := scanProcessRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, record)
	}
	return result, total, rows.Err()
}

func scanProcessRecord(row scanner) (ProcessControlRecord, error) {
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProcessControlRecord{}, domain.ErrNotFound
		}
		return ProcessControlRecord{}, err
	}
	return decodeProcessRecord(payload)
}

func decodeProcessRecord(payload string) (ProcessControlRecord, error) {
	var record ProcessControlRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return ProcessControlRecord{}, fmt.Errorf("decode persisted process operation: %w", err)
	}
	if err := validateProcessRecord(record); err != nil {
		return ProcessControlRecord{}, err
	}
	return record, nil
}

func validateProcessRecord(record ProcessControlRecord) error {
	if record.Binding.SourceID == "" || record.Operation.SourceID != record.Binding.SourceID ||
		record.Binding.InstanceID != record.Request.InstanceID || record.Operation.SourceRevision != record.Binding.SourceRevision ||
		record.Operation.BindingRevision != record.Binding.Revision || !record.Binding.Configured() ||
		record.Binding.SourceRevision == 0 || record.Binding.Revision == 0 {
		return errors.New("invalid persisted process operation identity")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if err := record.Binding.Validate(); err != nil {
		return err
	}
	return record.Operation.Validate(record.Request)
}

func processBusy() error {
	return &domain.OperationError{Code: "busy", Message: "该下级已有待确认的进程操作，请先核对原请求"}
}
