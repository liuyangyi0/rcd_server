package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"rcd_manager/internal/domain"
)

type AlarmFilter struct {
	SourceID string
	State    string
	Query    string
	From     time.Time
	To       time.Time
	Limit    int
	Offset   int
}

type SourceEventFilter struct {
	SourceID string
	Kind     string
	Query    string
	From     time.Time
	To       time.Time
	Limit    int
	Offset   int
}

type AlarmCondition struct {
	SourceID string
	Kind     string
	Severity domain.AlarmSeverity
	Message  string
	Active   bool
}

func (r *Repository) AppendSourceEvent(ctx context.Context, event domain.SourceEvent) (domain.SourceEvent, error) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = r.clock().UTC()
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO source_events
		(source_id, kind, previous_value, current_value, message, occurred_at) VALUES (?, ?, ?, ?, ?, ?)`,
		event.SourceID, event.Kind, event.Previous, event.Current, event.Message, encodeTime(event.OccurredAt))
	if err != nil {
		return domain.SourceEvent{}, fmt.Errorf("append source event: %w", err)
	}
	event.ID, err = result.LastInsertId()
	return event, err
}

func (r *Repository) ListSourceEvents(ctx context.Context, filter SourceEventFilter) ([]domain.SourceEvent, int, error) {
	filter.Limit, filter.Offset = normalizedPage(filter.Limit, filter.Offset)
	where := ` WHERE (? = '' OR source_id = ?) AND (? = '' OR kind = ?)
		AND (? = '' OR source_id LIKE '%' || ? || '%' ESCAPE '\' OR kind LIKE '%' || ? || '%' ESCAPE '\' OR message LIKE '%' || ? || '%' ESCAPE '\')
		AND (? = '' OR occurred_at >= ?) AND (? = '' OR occurred_at <= ?)`
	from, to := encodedTimeBounds(filter.From, filter.To)
	query := likeLiteral(filter.Query)
	args := []any{filter.SourceID, filter.SourceID, filter.Kind, filter.Kind,
		query, query, query, query, from, from, to, to}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_events`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, source_id, kind, previous_value, current_value, message, occurred_at
		FROM source_events`+where+` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list source events: %w", err)
	}
	defer rows.Close()
	items := make([]domain.SourceEvent, 0, filter.Limit)
	for rows.Next() {
		var event domain.SourceEvent
		var occurred string
		if err := rows.Scan(&event.ID, &event.SourceID, &event.Kind, &event.Previous, &event.Current, &event.Message, &occurred); err != nil {
			return nil, 0, err
		}
		var parseErr error
		event.OccurredAt, parseErr = decodeTime(occurred)
		if parseErr != nil {
			return nil, 0, parseErr
		}
		items = append(items, event)
	}
	return items, total, rows.Err()
}

// ReconcileAlarm opens/updates an alarm when active is true and resolves the
// current open occurrence when false. Each re-open creates a new history row.
func (r *Repository) ReconcileAlarm(ctx context.Context, sourceID, kind string, severity domain.AlarmSeverity, message string, active bool) (domain.Alarm, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Alarm{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	current, changed, err := r.reconcileAlarmTx(ctx, tx, AlarmCondition{SourceID: sourceID, Kind: kind,
		Severity: severity, Message: message, Active: active}, r.clock().UTC())
	if err != nil {
		return domain.Alarm{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Alarm{}, false, err
	}
	return current, changed, nil
}

// ReconcileAlarms applies one status snapshot atomically. A failed condition
// cannot leave earlier conditions committed and then double-counted on retry.
func (r *Repository) ReconcileAlarms(ctx context.Context, conditions []AlarmCondition) error {
	if len(conditions) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := r.clock().UTC()
	for _, condition := range conditions {
		if _, _, err := r.reconcileAlarmTx(ctx, tx, condition, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Repository) reconcileAlarmTx(ctx context.Context, tx *sql.Tx, condition AlarmCondition, now time.Time) (domain.Alarm, bool, error) {
	sourceID, kind, severity, message, active := condition.SourceID, condition.Kind, condition.Severity, condition.Message, condition.Active
	current, findErr := scanAlarm(tx.QueryRowContext(ctx, alarmSelect+` WHERE source_id = ? AND kind = ? AND state = 'Open'`, sourceID, kind))
	if errors.Is(findErr, domain.ErrNotFound) {
		if !active {
			return domain.Alarm{}, false, nil
		}
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO alarms
			(source_id, kind, severity, state, message, occurrences, opened_at, updated_at, revision)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?, 1)`, sourceID, kind, severity, domain.AlarmOpen, message, encodeTime(now), encodeTime(now))
		if insertErr != nil {
			return domain.Alarm{}, false, fmt.Errorf("open alarm: %w", insertErr)
		}
		id, _ := result.LastInsertId()
		current, findErr = scanAlarm(tx.QueryRowContext(ctx, alarmSelect+` WHERE id = ?`, id))
		if findErr != nil {
			return domain.Alarm{}, false, findErr
		}
	} else if findErr != nil {
		return domain.Alarm{}, false, findErr
	} else if active {
		if _, err := tx.ExecContext(ctx, `UPDATE alarms SET severity = ?, message = ?, occurrences = occurrences + 1,
			updated_at = ?, revision = revision + 1 WHERE id = ?`, severity, message, encodeTime(now), current.ID); err != nil {
			return domain.Alarm{}, false, fmt.Errorf("update alarm: %w", err)
		}
		current, findErr = scanAlarm(tx.QueryRowContext(ctx, alarmSelect+` WHERE id = ?`, current.ID))
		if findErr != nil {
			return domain.Alarm{}, false, findErr
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE alarms SET state = ?, resolved_at = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`,
			domain.AlarmResolved, encodeTime(now), encodeTime(now), current.ID); err != nil {
			return domain.Alarm{}, false, fmt.Errorf("resolve alarm: %w", err)
		}
		current, findErr = scanAlarm(tx.QueryRowContext(ctx, alarmSelect+` WHERE id = ?`, current.ID))
		if findErr != nil {
			return domain.Alarm{}, false, findErr
		}
	}
	return current, true, nil
}

func (r *Repository) ListAlarms(ctx context.Context, filter AlarmFilter) ([]domain.Alarm, int, error) {
	filter.Limit, filter.Offset = normalizedPage(filter.Limit, filter.Offset)
	where := ` WHERE (? = '' OR source_id = ?) AND (? = '' OR state = ?)
		AND (? = '' OR source_id LIKE '%' || ? || '%' ESCAPE '\' OR kind LIKE '%' || ? || '%' ESCAPE '\' OR message LIKE '%' || ? || '%' ESCAPE '\')
		AND (? = '' OR updated_at >= ?) AND (? = '' OR updated_at <= ?)`
	from, to := encodedTimeBounds(filter.From, filter.To)
	query := likeLiteral(filter.Query)
	args := []any{filter.SourceID, filter.SourceID, filter.State, filter.State,
		query, query, query, query, from, from, to, to}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alarms`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, alarmSelect+where+` ORDER BY CASE state WHEN 'Open' THEN 0 ELSE 1 END,
		updated_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list alarms: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Alarm, 0, filter.Limit)
	for rows.Next() {
		alarm, scanErr := scanAlarm(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		items = append(items, alarm)
	}
	return items, total, rows.Err()
}

func (r *Repository) AlarmCounts(ctx context.Context) (open, unacknowledged int, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN acknowledged_at IS NULL THEN 1 ELSE 0 END), 0)
		FROM alarms WHERE state = 'Open' AND (silenced_until IS NULL OR silenced_until <= ?)`, encodeTime(r.clock().UTC())).Scan(&open, &unacknowledged)
	return
}

func (r *Repository) AcknowledgeAlarm(ctx context.Context, id int64, expectedRevision uint64, actor string) (domain.Alarm, error) {
	now := r.clock().UTC()
	result, err := r.db.ExecContext(ctx, `UPDATE alarms SET acknowledged_at = ?, acknowledged_by = ?, updated_at = ?,
		revision = revision + 1 WHERE id = ? AND revision = ?`, encodeTime(now), actor, encodeTime(now), id, expectedRevision)
	if err != nil {
		return domain.Alarm{}, err
	}
	if err := requireAlarmMutation(ctx, r.db, result, id, expectedRevision); err != nil {
		return domain.Alarm{}, err
	}
	return scanAlarm(r.db.QueryRowContext(ctx, alarmSelect+` WHERE id = ?`, id))
}

func (r *Repository) SilenceAlarm(ctx context.Context, id int64, expectedRevision uint64, until time.Time) (domain.Alarm, error) {
	var stored any
	if !until.IsZero() {
		stored = encodeTime(until)
	}
	now := r.clock().UTC()
	result, err := r.db.ExecContext(ctx, `UPDATE alarms SET silenced_until = ?, updated_at = ?, revision = revision + 1
		WHERE id = ? AND revision = ?`, stored, encodeTime(now), id, expectedRevision)
	if err != nil {
		return domain.Alarm{}, err
	}
	if err := requireAlarmMutation(ctx, r.db, result, id, expectedRevision); err != nil {
		return domain.Alarm{}, err
	}
	return scanAlarm(r.db.QueryRowContext(ctx, alarmSelect+` WHERE id = ?`, id))
}

func (r *Repository) PruneOperationalHistory(ctx context.Context, cutoff time.Time, maxRows int) error {
	if maxRows <= 0 {
		maxRows = 100000
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cutoffText := encodeTime(cutoff)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM source_events WHERE occurred_at < ? OR id NOT IN (SELECT id FROM source_events ORDER BY occurred_at DESC, id DESC LIMIT ?)`, []any{cutoffText, maxRows}},
		{`DELETE FROM management_audit WHERE occurred_at < ? OR id NOT IN (SELECT id FROM management_audit ORDER BY occurred_at DESC, id DESC LIMIT ?)`, []any{cutoffText, maxRows}},
		{`DELETE FROM command_audit WHERE state NOT IN ('Waiting', 'Queued') AND (COALESCE(finished_at, started_at) < ? OR id NOT IN
			(SELECT id FROM command_audit ORDER BY started_at DESC, id DESC LIMIT ?))`, []any{cutoffText, maxRows}},
		{`DELETE FROM alarms WHERE state = 'Resolved' AND resolved_at < ?`, []any{cutoffText}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("prune operational history: %w", err)
		}
	}
	return tx.Commit()
}

func (r *Repository) DatabaseStats(ctx context.Context) (pageCount, pageSize int64, err error) {
	if err = r.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return
	}
	err = r.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
	return
}

const alarmSelect = `SELECT id, source_id, kind, severity, state, message, occurrences,
	acknowledged_at, acknowledged_by, silenced_until, opened_at, updated_at, resolved_at, revision FROM alarms`

func scanAlarm(row scanner) (domain.Alarm, error) {
	var alarm domain.Alarm
	var acknowledged, silenced, resolved sql.NullString
	var opened, updated string
	if err := row.Scan(&alarm.ID, &alarm.SourceID, &alarm.Kind, &alarm.Severity, &alarm.State, &alarm.Message,
		&alarm.Occurrences, &acknowledged, &alarm.AcknowledgedBy, &silenced, &opened, &updated, &resolved, &alarm.Revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Alarm{}, domain.ErrNotFound
		}
		return domain.Alarm{}, err
	}
	var err error
	if alarm.OpenedAt, err = decodeTime(opened); err != nil {
		return domain.Alarm{}, err
	}
	if alarm.UpdatedAt, err = decodeTime(updated); err != nil {
		return domain.Alarm{}, err
	}
	if acknowledged.Valid {
		if alarm.AcknowledgedAt, err = decodeTime(acknowledged.String); err != nil {
			return domain.Alarm{}, err
		}
	}
	if silenced.Valid {
		if alarm.SilencedUntil, err = decodeTime(silenced.String); err != nil {
			return domain.Alarm{}, err
		}
	}
	if resolved.Valid {
		if alarm.ResolvedAt, err = decodeTime(resolved.String); err != nil {
			return domain.Alarm{}, err
		}
	}
	return alarm, nil
}

func requireAlarmMutation(ctx context.Context, db *sql.DB, result sql.Result, id int64, expected uint64) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	var actual uint64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM alarms WHERE id = ?`, id).Scan(&actual); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	return &domain.RevisionConflictError{Expected: expected, Actual: actual}
}

func normalizedPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
