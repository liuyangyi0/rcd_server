package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"rcd_manager/internal/domain"
	"strings"
)

// Fetch only summary fields in one query; history stays in the detail endpoint.
func (r *Repository) AttachCommandSummaries(ctx context.Context, commands []domain.PendingCommand) error {
	if len(commands) == 0 {
		return nil
	}
	args := make([]any, len(commands))
	indices := make(map[string]int, len(commands))
	for i, c := range commands {
		args[i] = c.ID
		indices[c.ID] = i
	}
	rows, err := r.db.QueryContext(ctx, `SELECT command_id,protocol,history_gap,json_extract(snapshot,'$.phase'),json_extract(snapshot,'$.attempt_count'),json_extract(snapshot,'$.sent_count'),COALESCE(json_extract(snapshot,'$.audit_incomplete'),0) FROM command_trace WHERE command_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v domain.CommandTrackingSummary
		var phase sql.NullString
		var attempts, sent sql.NullInt64
		var gap, incomplete bool
		if err = rows.Scan(&id, &v.Protocol, &gap, &phase, &attempts, &sent, &incomplete); err != nil {
			return err
		}
		v.Phase = phase.String
		v.HistoryGap = gap
		v.AuditIncomplete = incomplete || !phase.Valid
		if attempts.Valid {
			x := uint32(attempts.Int64)
			v.AttemptCount = &x
		}
		if sent.Valid {
			x := uint32(sent.Int64)
			v.SentCount = &x
		}
		commands[indices[id]].Tracking = &v
	}
	return rows.Err()
}

func (r *Repository) ManagerInstanceID(ctx context.Context) (string, error) {
	if _, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO manager_identity VALUES(1,?)`, "manager-"+rand.Text()); err != nil {
		return "", err
	}
	var id string
	err := r.db.QueryRowContext(ctx, `SELECT instance_id FROM manager_identity WHERE id=1`).Scan(&id)
	return id, err
}
func (r *Repository) BeginCommandTrace(ctx context.Context, id string, trace domain.CommandTrace) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO command_trace(command_id,protocol,gateway,boot,port,request_hash,history_gap) VALUES(?,?,?,?,?,?,?) ON CONFLICT(command_id) DO UPDATE SET history_gap=MAX(history_gap,excluded.history_gap) WHERE protocol=excluded.protocol AND gateway=excluded.gateway AND boot=excluded.boot AND port=excluded.port AND request_hash=excluded.request_hash`, id, trace.Protocol, trace.GatewayInstanceID, trace.BootID, trace.PhysicalPort, trace.RequestHash, trace.HistoryGap)
	return err
}
func (r *Repository) CommandDetail(ctx context.Context, id string) (domain.CommandDetail, error) {
	var v domain.CommandDetail
	var started string
	var finished sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id,session_id,group_id,source_id,port,kind,node_id,request,response,state,status_code,error,started_at,finished_at FROM command_audit WHERE id=?`, id).
		Scan(&v.Command.ID, &v.Command.SessionID, &v.Command.GroupID, &v.Command.SourceID, &v.Command.Port, &v.Command.Kind, &v.Command.NodeID, &v.Command.Request, &v.Command.Response, &v.Command.State, &v.Command.StatusCode, &v.Command.Error, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return v, domain.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if v.Command.StartedAt, err = decodeTime(started); err != nil {
		return v, err
	}
	if finished.Valid {
		if v.Command.FinishedAt, err = decodeTime(finished.String); err != nil {
			return v, err
		}
	}
	var trace domain.CommandTrace
	var snapshot sql.NullString
	err = r.db.QueryRowContext(ctx, `SELECT protocol,gateway,boot,port,request_hash,snapshot,history_gap FROM command_trace WHERE command_id=?`, id).
		Scan(&trace.Protocol, &trace.GatewayInstanceID, &trace.BootID, &trace.PhysicalPort, &trace.RequestHash, &snapshot, &trace.HistoryGap)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if snapshot.Valid {
		if err = json.Unmarshal([]byte(snapshot.String), &trace.Snapshot); err != nil {
			return v, err
		}
	}
	v.Trace = &trace
	return v, nil
}
func (r *Repository) CommandTraceEvents(ctx context.Context, id string, limit, offset int) ([]domain.TrackedCommand, int, error) {
	if limit < 1 || limit > 1000 || offset < 0 {
		return nil, 0, domain.ErrInvalid
	}
	if _, err := r.CommandDetail(ctx, id); err != nil {
		return nil, 0, err
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM command_trace_events WHERE command_id=?`, id).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT snapshot FROM command_trace_events WHERE command_id=? ORDER BY revision LIMIT ? OFFSET ?`, id, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]domain.TrackedCommand, 0)
	for rows.Next() {
		var raw string
		var v domain.TrackedCommand
		if err = rows.Scan(&raw); err != nil {
			return nil, 0, err
		}
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, 0, err
		}
		items = append(items, v)
	}
	return items, total, rows.Err()
}
func (r *Repository) CommandTraceCursor(ctx context.Context, source, gateway string) (uint64, error) {
	var cursor uint64
	err := r.db.QueryRowContext(ctx, `SELECT cursor FROM command_trace_cursor WHERE source_id=? AND gateway=?`, source, gateway).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return cursor, err
}

// Lower evidence and cursor commit together. Unrelated/legacy commands cannot
// attach by bytes or timestamps, and stale snapshots cannot replace new ones.
func (r *Repository) SaveCommandEvidence(ctx context.Context, source, gateway, manager string, values []domain.TrackedCommand, cursor *uint64, gap bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range values {
		if err = v.Validate(); err != nil {
			return err
		}
		if v.ManagerInstanceID != manager || v.GatewayInstanceID != gateway {
			return fmt.Errorf("command evidence identity mismatch")
		}
		var expectedSource, boot, port, hash string
		err = tx.QueryRowContext(ctx, `SELECT c.source_id,t.boot,t.port,t.request_hash FROM command_trace t JOIN command_audit c ON c.id=t.command_id WHERE t.command_id=? AND t.gateway=?`, v.CommandID, gateway).Scan(&expectedSource, &boot, &port, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		// A gateway can be configured under more than one logical source. Its
		// manager-scoped event stream includes both; do not poison this source's
		// cursor with an event owned by another source or attach it to this one.
		if expectedSource != source {
			continue
		}
		if boot != v.BootID || port != v.PortID || hash != v.RequestHash {
			return fmt.Errorf("command evidence pinned target mismatch")
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE command_trace SET snapshot=?,revision=?,history_gap=MAX(history_gap,?) WHERE command_id=? AND revision<?`, string(raw), v.Revision, gap, v.CommandID, v.Revision); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO command_trace_events VALUES(?,?,?)`, v.CommandID, v.Revision, string(raw)); err != nil {
			return err
		}
	}
	// Source-wide gap marking belongs to a page commit, never to each 100 ms
	// individual snapshot save. Rewriting the entire history per record can
	// permanently starve persistence and leave the cursor stuck behind retention.
	if gap && cursor != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE command_trace SET history_gap=1 WHERE history_gap=0 AND gateway=? AND command_id IN(SELECT id FROM command_audit WHERE source_id=?)`, gateway, source); err != nil {
			return err
		}
	}
	if cursor != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO command_trace_cursor VALUES(?,?,?) ON CONFLICT(source_id,gateway) DO UPDATE SET cursor=MAX(cursor,excluded.cursor)`, source, gateway, *cursor); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Keyset traversal prevents missing/pruned records from blocking newer evidence.
// Only read-side recovery: callers must never re-submit these command IDs.
func (r *Repository) IncompleteCommandIDs(ctx context.Context, gateway, before string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, domain.ErrInvalid
	}
	rows, err := r.db.QueryContext(ctx, `SELECT t.command_id FROM command_trace t JOIN command_audit c ON c.id=t.command_id
	 WHERE t.gateway=? AND (?='' OR (c.started_at,c.id)<(SELECT started_at,id FROM command_audit WHERE id=?))
	 AND (t.snapshot IS NULL OR json_extract(t.snapshot,'$.phase') IN ('Accepted','Executing'))
	 ORDER BY c.started_at DESC,c.id DESC LIMIT ?`, gateway, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
