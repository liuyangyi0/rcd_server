package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"rcd_manager/internal/domain"
)

func (r *Repository) LoadRedundancy(ctx context.Context) ([]domain.RedundancyPolicy, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT group_id, revision, policy_json FROM group_redundancy")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RedundancyPolicy{}
	for rows.Next() {
		var id, raw string
		var rev uint64
		if err = rows.Scan(&id, &rev, &raw); err != nil {
			return nil, err
		}
		var p domain.RedundancyPolicy
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		p.GroupID = id
		p.Revision = rev
		if err = p.Validate(); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (r *Repository) SaveRedundancy(ctx context.Context, p domain.RedundancyPolicy, revision uint64) (domain.RedundancyPolicy, error) {
	if err := p.Validate(); err != nil {
		return p, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var exists string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM groups WHERE id=?", p.GroupID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return p, domain.ErrNotFound
	} else if err != nil {
		return p, err
	}
	var current uint64
	err = tx.QueryRowContext(ctx, "SELECT revision FROM group_redundancy WHERE group_id=?", p.GroupID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		current = 1
	} else if err != nil {
		return p, err
	}
	if revision != current {
		return p, &domain.RevisionConflictError{Expected: revision, Actual: current}
	}
	p.Revision = current + 1
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO group_redundancy(group_id,revision,policy_json) VALUES(?,?,?) ON CONFLICT(group_id) DO UPDATE SET revision=excluded.revision,policy_json=excluded.policy_json`, p.GroupID, p.Revision, string(raw))
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}
