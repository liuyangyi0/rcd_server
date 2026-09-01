package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

func (r *Repository) LoadCalculationDraft(ctx context.Context) (calculation.Draft, error) {
	var draft calculation.Draft
	var valid int
	var problems, updated string
	err := r.db.QueryRowContext(ctx, `SELECT revision, content, content_hash, valid, problems, updated_at FROM calculation_draft WHERE id=1`).
		Scan(&draft.Revision, &draft.Content, &draft.Hash, &valid, &problems, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return calculation.Draft{}, domain.ErrNotFound
	}
	if err != nil {
		return calculation.Draft{}, fmt.Errorf("load calculation draft: %w", err)
	}
	draft.Valid = valid != 0
	if err := json.Unmarshal([]byte(problems), &draft.Problems); err != nil {
		return calculation.Draft{}, fmt.Errorf("decode calculation draft problems: %w", err)
	}
	draft.UpdatedAt, err = decodeTime(updated)
	return draft, err
}

func (r *Repository) SaveCalculationDraft(ctx context.Context, draft calculation.Draft, expected uint64) (calculation.Draft, error) {
	problems, err := json.Marshal(draft.Problems)
	if err != nil {
		return calculation.Draft{}, err
	}
	now := r.clock().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return calculation.Draft{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current uint64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM calculation_draft WHERE id=1`).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		if expected != 0 {
			return calculation.Draft{}, &domain.RevisionConflictError{Expected: expected, Actual: 0}
		}
		draft.Revision = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO calculation_draft(id,revision,content,content_hash,valid,problems,updated_at) VALUES(1,?,?,?,?,?,?)`, draft.Revision, draft.Content, draft.Hash, boolInt(draft.Valid), string(problems), encodeTime(now))
	} else if err != nil {
		return calculation.Draft{}, err
	} else {
		if current != expected {
			return calculation.Draft{}, &domain.RevisionConflictError{Expected: expected, Actual: current}
		}
		draft.Revision = current + 1
		_, err = tx.ExecContext(ctx, `UPDATE calculation_draft SET revision=?,content=?,content_hash=?,valid=?,problems=?,updated_at=? WHERE id=1`, draft.Revision, draft.Content, draft.Hash, boolInt(draft.Valid), string(problems), encodeTime(now))
	}
	if err != nil {
		return calculation.Draft{}, fmt.Errorf("save calculation draft: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return calculation.Draft{}, err
	}
	draft.UpdatedAt = now
	return draft, nil
}

func (r *Repository) LoadActiveCalculation(ctx context.Context) (calculation.Version, error) {
	return r.loadCalculationByState(ctx, "active")
}
func (r *Repository) LoadPendingCalculation(ctx context.Context) (calculation.Version, error) {
	return r.loadCalculationByState(ctx, "pending_restart")
}
func (r *Repository) loadCalculationByState(ctx context.Context, state string) (calculation.Version, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id,revision,content_hash,content,rule_count,added_count,removed_count,changed_count,state,actor,created_at,COALESCE(activated_at,'') FROM calculation_versions WHERE state=?`, state)
	return scanCalculationVersion(row)
}
func (r *Repository) LoadCalculationVersion(ctx context.Context, id int64) (calculation.Version, error) {
	return scanCalculationVersion(r.db.QueryRowContext(ctx, `SELECT id,revision,content_hash,content,rule_count,added_count,removed_count,changed_count,state,actor,created_at,COALESCE(activated_at,'') FROM calculation_versions WHERE id=?`, id))
}

func (r *Repository) ActivateCalculation(ctx context.Context, config calculation.Config, actor, state string, diff calculation.DiffSummary) (calculation.Version, error) {
	if state != "active" && state != "pending_restart" {
		return calculation.Version{}, domain.ErrInvalid
	}
	now := r.clock().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return calculation.Version{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if state == "active" {
		if _, err = tx.ExecContext(ctx, `UPDATE calculation_versions SET state='archived' WHERE state IN ('active','pending_restart')`); err != nil {
			return calculation.Version{}, err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE calculation_versions SET state='archived' WHERE state='pending_restart'`); err != nil {
			return calculation.Version{}, err
		}
	}
	var revision uint64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM calculation_versions`).Scan(&revision); err != nil {
		return calculation.Version{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO calculation_versions(revision,content,content_hash,rule_count,added_count,removed_count,changed_count,state,actor,created_at,activated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, revision, config.Content, config.Hash, len(config.Rules), diff.Added, diff.Removed, diff.Changed, state, actor, encodeTime(now), encodeTime(now))
	if err != nil {
		return calculation.Version{}, fmt.Errorf("activate calculation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return calculation.Version{}, err
	}
	if err = tx.Commit(); err != nil {
		return calculation.Version{}, err
	}
	return calculation.Version{ID: id, Revision: revision, Hash: config.Hash, Content: config.Content, RuleCount: len(config.Rules), AddedCount: diff.Added, RemovedCount: diff.Removed, ChangedCount: diff.Changed, State: state, Actor: actor, CreatedAt: now, ActivatedAt: now}, nil
}

func (r *Repository) DeactivateCalculation(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `UPDATE calculation_versions SET state='archived' WHERE state IN ('active','pending_restart')`)
	return err
}

func (r *Repository) PromotePendingCalculation(ctx context.Context, id int64) (calculation.Version, error) {
	now := r.clock().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return calculation.Version{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE calculation_versions SET state='archived' WHERE state='active'`); err != nil {
		return calculation.Version{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE calculation_versions SET state='active', activated_at=? WHERE id=? AND state='pending_restart'`, encodeTime(now), id)
	if err != nil {
		return calculation.Version{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return calculation.Version{}, err
	}
	if changed != 1 {
		return calculation.Version{}, domain.ErrNotFound
	}
	if err = tx.Commit(); err != nil {
		return calculation.Version{}, err
	}
	return r.LoadCalculationVersion(ctx, id)
}

func (r *Repository) ListCalculationVersions(ctx context.Context, limit, offset int) ([]calculation.Version, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM calculation_versions`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,revision,content_hash,'',rule_count,added_count,removed_count,changed_count,state,actor,created_at,COALESCE(activated_at,'') FROM calculation_versions ORDER BY revision DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]calculation.Version, 0, limit)
	for rows.Next() {
		version, scanErr := scanCalculationVersion(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		result = append(result, version)
	}
	return result, total, rows.Err()
}

func (r *Repository) PruneCalculationVersions(ctx context.Context, keep int) error {
	if keep < 1 {
		keep = 50
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM calculation_versions WHERE state='archived' AND id NOT IN (SELECT id FROM calculation_versions WHERE state='archived' ORDER BY revision DESC LIMIT ?)`, keep)
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanCalculationVersion(row rowScanner) (calculation.Version, error) {
	var value calculation.Version
	var created, activated string
	err := row.Scan(&value.ID, &value.Revision, &value.Hash, &value.Content, &value.RuleCount, &value.AddedCount, &value.RemovedCount, &value.ChangedCount, &value.State, &value.Actor, &created, &activated)
	if errors.Is(err, sql.ErrNoRows) {
		return calculation.Version{}, domain.ErrNotFound
	}
	if err != nil {
		return calculation.Version{}, err
	}
	value.CreatedAt, err = decodeTime(created)
	if err != nil {
		return calculation.Version{}, err
	}
	if activated != "" {
		value.ActivatedAt, err = decodeTime(activated)
	}
	return value, err
}
