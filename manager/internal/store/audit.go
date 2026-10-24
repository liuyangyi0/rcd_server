package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"rcd_manager/internal/domain"
)

func (r *Repository) SaveCommand(ctx context.Context, command domain.PendingCommand) error {
	var finished any
	if !command.FinishedAt.IsZero() {
		finished = encodeTime(command.FinishedAt)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO command_audit
		(id, session_id, group_id, source_id, port, kind, node_id, request, response, state, status_code, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state, status_code = excluded.status_code,
		error = excluded.error, response = excluded.response, finished_at = excluded.finished_at`,
		command.ID, command.SessionID, command.GroupID, command.SourceID, command.Port, command.Kind,
		command.NodeID, command.Request, command.Response, command.State, uint32(command.StatusCode), command.Error, encodeTime(command.StartedAt), finished)
	if err != nil {
		return fmt.Errorf("save command audit: %w", err)
	}
	return nil
}

func (r *Repository) ListCommands(ctx context.Context, limit, offset int) ([]domain.PendingCommand, int, error) {
	return r.ListCommandsFiltered(ctx, CommandFilter{Limit: limit, Offset: offset})
}

type CommandFilter struct {
	GroupID  string
	SourceID string
	Status   string
	Query    string
	From     time.Time
	To       time.Time
	Limit    int
	Offset   int
}

func (r *Repository) ListCommandsFiltered(ctx context.Context, filter CommandFilter) ([]domain.PendingCommand, int, error) {
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	where := ` WHERE (? = '' OR group_id = ?) AND (? = '' OR source_id = ?) AND (? = '' OR state = ?)
		AND (? = '' OR id LIKE '%' || ? || '%' OR session_id LIKE '%' || ? || '%' OR port LIKE '%' || ? || '%' OR kind LIKE '%' || ? || '%' OR node_id LIKE '%' || ? || '%' OR request LIKE '%' || ? || '%' OR response LIKE '%' || ? || '%' OR error LIKE '%' || ? || '%')
		AND (? = '' OR started_at >= ?) AND (? = '' OR started_at <= ?)`
	from, to := encodedTimeBounds(filter.From, filter.To)
	args := []any{filter.GroupID, filter.GroupID, filter.SourceID, filter.SourceID, filter.Status, filter.Status,
		filter.Query, filter.Query, filter.Query, filter.Query, filter.Query, filter.Query, filter.Query, filter.Query,
		filter.Query, from, from, to, to}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM command_audit`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, session_id, group_id, source_id, port, kind, node_id, request, response, state,
		status_code, error, started_at, finished_at FROM command_audit`+where+
		` ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list command audit: %w", err)
	}
	defer rows.Close()
	commands := make([]domain.PendingCommand, 0, filter.Limit)
	for rows.Next() {
		var command domain.PendingCommand
		var code uint32
		var started string
		var finished sql.NullString
		if err := rows.Scan(&command.ID, &command.SessionID, &command.GroupID, &command.SourceID, &command.Port,
			&command.Kind, &command.NodeID, &command.Request, &command.Response, &command.State, &code, &command.Error, &started, &finished); err != nil {
			return nil, 0, err
		}
		command.StatusCode = domain.StatusCode(code)
		command.StartedAt, err = decodeTime(started)
		if err != nil {
			return nil, 0, err
		}
		if finished.Valid {
			command.FinishedAt, err = decodeTime(finished.String)
			if err != nil {
				return nil, 0, err
			}
		}
		commands = append(commands, command)
	}
	return commands, total, rows.Err()
}

func (r *Repository) AppendAudit(ctx context.Context, entry domain.AuditEntry) (domain.AuditEntry, error) {
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = r.clock().UTC()
	}
	details, err := json.Marshal(entry.Details)
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("encode audit details: %w", err)
	}
	result, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO management_audit
		(kind, actor, target, revision, details, occurred_at) VALUES (?, ?, ?, ?, ?, ?)`,
		entry.Kind, entry.Actor, entry.Target, entry.Revision, string(details), encodeTime(entry.OccurredAt))
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("append management audit: %w", err)
	}
	entry.ID, err = result.LastInsertId()
	return entry, err
}

func (r *Repository) ListAudit(ctx context.Context, limit, offset int) ([]domain.AuditEntry, int, error) {
	return r.ListAuditFiltered(ctx, AuditFilter{Limit: limit, Offset: offset})
}

type AuditFilter struct {
	Kind   string
	Query  string
	From   time.Time
	To     time.Time
	Limit  int
	Offset int
}

func (r *Repository) ListAuditFiltered(ctx context.Context, filter AuditFilter) ([]domain.AuditEntry, int, error) {
	limit, offset := filter.Limit, filter.Offset
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where := ` WHERE (? = '' OR kind = ?) AND (? = '' OR kind LIKE '%' || ? || '%' OR actor LIKE '%' || ? || '%'
		OR target LIKE '%' || ? || '%' OR details LIKE '%' || ? || '%')
		AND (? = '' OR occurred_at >= ?) AND (? = '' OR occurred_at <= ?)`
	from, to := encodedTimeBounds(filter.From, filter.To)
	args := []any{filter.Kind, filter.Kind, filter.Query, filter.Query, filter.Query, filter.Query, filter.Query, from, from, to, to}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM management_audit`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, kind, actor, target, revision, details, occurred_at
		FROM management_audit`+where+` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries := make([]domain.AuditEntry, 0, limit)
	for rows.Next() {
		var entry domain.AuditEntry
		var details, occurred string
		if err := rows.Scan(&entry.ID, &entry.Kind, &entry.Actor, &entry.Target, &entry.Revision, &details, &occurred); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal([]byte(details), &entry.Details); err != nil {
			return nil, 0, err
		}
		entry.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, entry)
	}
	return entries, total, rows.Err()
}

func encodedTimeBounds(from, to time.Time) (string, string) {
	var fromText, toText string
	if !from.IsZero() {
		fromText = encodeTime(from.UTC())
	}
	if !to.IsZero() {
		toText = encodeTime(to.UTC())
	}
	return fromText, toText
}
