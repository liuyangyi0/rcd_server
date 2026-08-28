package store

import (
	"context"
	"database/sql"
	"fmt"
)

var migrations = []string{
	`CREATE TABLE groups (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		default_source_id TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL CHECK (revision > 0),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE sources (
		id TEXT PRIMARY KEY,
		group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT,
		name TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		security_policy TEXT NOT NULL,
		security_mode TEXT NOT NULL,
		username TEXT NOT NULL DEFAULT '',
		password_cipher BLOB,
		enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
		catalog_state TEXT NOT NULL,
		revision INTEGER NOT NULL CHECK (revision > 0),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX sources_group_id_idx ON sources(group_id);
	CREATE TABLE catalog_nodes (
		source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		namespace_uri TEXT NOT NULL,
		identifier TEXT NOT NULL,
		browse_name TEXT NOT NULL,
		parent_path TEXT NOT NULL,
		node_class TEXT NOT NULL,
		data_type TEXT NOT NULL,
		value_rank INTEGER NOT NULL,
		writable INTEGER NOT NULL CHECK (writable IN (0, 1)),
		input_arguments TEXT NOT NULL DEFAULT '[]',
		output_arguments TEXT NOT NULL DEFAULT '[]',
		retired INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0, 1)),
		discovered_at TEXT NOT NULL,
		PRIMARY KEY (source_id, namespace_uri, identifier)
	);
	CREATE INDEX catalog_nodes_identifier_idx ON catalog_nodes(identifier);
	CREATE TABLE command_audit (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		group_id TEXT NOT NULL,
		source_id TEXT NOT NULL,
		port TEXT NOT NULL,
		kind TEXT NOT NULL,
		state TEXT NOT NULL,
		status_code INTEGER NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		finished_at TEXT
	);
	CREATE INDEX command_audit_started_idx ON command_audit(started_at DESC);
	CREATE TABLE management_audit (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		actor TEXT NOT NULL,
		target TEXT NOT NULL,
		revision INTEGER NOT NULL,
		details TEXT NOT NULL DEFAULT '{}',
		occurred_at TEXT NOT NULL
	);
	CREATE INDEX management_audit_occurred_idx ON management_audit(occurred_at DESC);`,
	`CREATE UNIQUE INDEX management_audit_event_idx
		ON management_audit(kind, target, revision, occurred_at);`,
	`CREATE TABLE source_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		previous_value TEXT NOT NULL DEFAULT '',
		current_value TEXT NOT NULL,
		message TEXT NOT NULL DEFAULT '',
		occurred_at TEXT NOT NULL
	);
	CREATE INDEX source_events_occurred_idx ON source_events(occurred_at DESC);
	CREATE INDEX source_events_source_idx ON source_events(source_id, occurred_at DESC);
	CREATE TABLE alarms (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		severity TEXT NOT NULL,
		state TEXT NOT NULL,
		message TEXT NOT NULL,
		occurrences INTEGER NOT NULL DEFAULT 1,
		acknowledged_at TEXT,
		acknowledged_by TEXT NOT NULL DEFAULT '',
		silenced_until TEXT,
		opened_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		resolved_at TEXT,
		revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0)
	);
	CREATE UNIQUE INDEX alarms_open_source_kind_idx ON alarms(source_id, kind) WHERE state = 'Open';
	CREATE INDEX alarms_state_updated_idx ON alarms(state, updated_at DESC);`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, len(migrations))
	}
	for index := current; index < len(migrations); index++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", index+1, err)
		}
		if _, err = tx.ExecContext(ctx, migrations[index]); err == nil {
			_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", index+1))
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return fmt.Errorf("apply migration %d: %w", index+1, err)
		}
	}
	return nil
}
