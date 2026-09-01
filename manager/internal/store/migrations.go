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
	`ALTER TABLE sources ADD COLUMN group_bindings TEXT NOT NULL DEFAULT '[]';
	ALTER TABLE catalog_nodes ADD COLUMN group_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE catalog_nodes ADD COLUMN port_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE catalog_nodes ADD COLUMN device_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE catalog_nodes ADD COLUMN kind TEXT NOT NULL DEFAULT '';
	CREATE TABLE source_topology (
		source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
		manifest TEXT NOT NULL,
		discovered_at TEXT NOT NULL
	);`,
	// Older Conflict rows do not distinguish a rejected discovery from a
	// recoverable peer conflict. Backfill only an explicit rejection in the
	// latest persisted status event; absent/stale evidence must stay unmarked.
	`ALTER TABLE source_topology ADD COLUMN quarantine_reason TEXT NOT NULL DEFAULT '';
	INSERT INTO source_topology (source_id, manifest, discovered_at, quarantine_reason)
	SELECT s.id, 'null', e.occurred_at, substr(e.message, length('catalog rejected: ') + 1)
	FROM sources s JOIN source_events e ON e.id = (
		SELECT latest.id FROM source_events latest WHERE latest.source_id = s.id
		ORDER BY julianday(latest.occurred_at) DESC, latest.id DESC LIMIT 1
	)
	WHERE s.catalog_state = 'CatalogConflict'
		AND substr(e.message, 1, length('catalog rejected: ')) = 'catalog rejected: '
		AND length(trim(substr(e.message, length('catalog rejected: ') + 1))) > 0
	ON CONFLICT(source_id) DO UPDATE SET quarantine_reason = excluded.quarantine_reason;`,
	`CREATE TABLE process_bindings (
		source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
		agent_url TEXT NOT NULL DEFAULT '',
		instance_id TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL CHECK(revision > 0),
		updated_at TEXT NOT NULL
	);
	CREATE UNIQUE INDEX process_bindings_instance_idx ON process_bindings(instance_id)
		WHERE instance_id <> '';
	CREATE TABLE process_control_operations (
		source_id TEXT NOT NULL,
		request_id TEXT NOT NULL,
		state TEXT NOT NULL,
		dispatched INTEGER NOT NULL DEFAULT 0 CHECK(dispatched IN (0,1)),
		record TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(source_id, request_id)
	);
	CREATE UNIQUE INDEX process_control_pending_idx ON process_control_operations(source_id)
		WHERE state NOT IN ('succeeded','failed');
	CREATE INDEX process_control_history_idx ON process_control_operations(source_id, created_at DESC);`,
	`CREATE TABLE source_station_version_state (
		source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
		topology_fingerprint TEXT NOT NULL,
		lower_revision TEXT NOT NULL,
		station_revision TEXT NOT NULL,
		software_version TEXT NOT NULL DEFAULT '',
		build_commit TEXT NOT NULL DEFAULT '',
		observed_at TEXT NOT NULL
	);
	CREATE TABLE station_version_cache (
		source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		topology_fingerprint TEXT NOT NULL,
		port_id TEXT NOT NULL,
		port_name TEXT NOT NULL DEFAULT '',
		port_enabled INTEGER NOT NULL CHECK(port_enabled IN (0,1)),
		port_applied_enabled INTEGER NOT NULL CHECK(port_applied_enabled IN (0,1)),
		port_open INTEGER NOT NULL CHECK(port_open IN (0,1)),
		control_pending INTEGER NOT NULL CHECK(control_pending IN (0,1)),
		pending_commands INTEGER NOT NULL CHECK(pending_commands >= 0),
		device_id TEXT NOT NULL,
		device_name TEXT NOT NULL DEFAULT '',
		read_enabled INTEGER NOT NULL CHECK(read_enabled IN (0,1)),
		applied_read_enabled INTEGER NOT NULL CHECK(applied_read_enabled IN (0,1)),
		communication_ok INTEGER NOT NULL CHECK(communication_ok IN (0,1)),
		configured_module_type TEXT NOT NULL,
		detected_module_type TEXT NOT NULL DEFAULT '',
		type_mismatch INTEGER NOT NULL CHECK(type_mismatch IN (0,1)),
		communication_module_version TEXT NOT NULL DEFAULT '',
		communication_module_version_raw INTEGER,
		plc_version TEXT NOT NULL DEFAULT '',
		plc_version_raw INTEGER,
		version_payload TEXT NOT NULL DEFAULT '',
		version_state TEXT NOT NULL,
		last_version_attempt_at TEXT,
		last_version_success_at TEXT,
		last_version_error TEXT NOT NULL DEFAULT '',
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		stale INTEGER NOT NULL CHECK(stale IN (0,1)),
		PRIMARY KEY(source_id, topology_fingerprint, port_id, device_id)
	);
	CREATE INDEX station_version_cache_source_idx ON station_version_cache(source_id, topology_fingerprint, port_id, device_id);`,
	`CREATE TABLE lower_log_ingest_cursor (
		source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		boot_id TEXT NOT NULL,
		accepted_through INTEGER NOT NULL DEFAULT 0 CHECK(accepted_through >= 0),
		updated_at TEXT NOT NULL,
		PRIMARY KEY(source_id, boot_id)
	);
	CREATE INDEX lower_log_ingest_updated_idx ON lower_log_ingest_cursor(source_id, updated_at DESC);`,
	`CREATE TABLE calculation_draft (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		revision INTEGER NOT NULL CHECK(revision > 0),
		content TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		valid INTEGER NOT NULL CHECK(valid IN (0,1)),
		problems TEXT NOT NULL DEFAULT '[]',
		updated_at TEXT NOT NULL
	);
	CREATE TABLE calculation_versions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		revision INTEGER NOT NULL UNIQUE CHECK(revision > 0),
		content TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		rule_count INTEGER NOT NULL CHECK(rule_count >= 0),
		state TEXT NOT NULL CHECK(state IN ('active','archived','pending_restart')),
		actor TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		activated_at TEXT
	);
	CREATE UNIQUE INDEX calculation_one_active_idx ON calculation_versions(state) WHERE state = 'active';
	CREATE UNIQUE INDEX calculation_one_pending_idx ON calculation_versions(state) WHERE state = 'pending_restart';
	CREATE INDEX calculation_versions_revision_idx ON calculation_versions(revision DESC);
	INSERT INTO calculation_draft(id, revision, content, content_hash, valid, problems, updated_at)
	VALUES(1, 1, '', '', 0, '[]', strftime('%Y-%m-%dT%H:%M:%fZ','now'));`,
	`ALTER TABLE calculation_versions ADD COLUMN added_count INTEGER NOT NULL DEFAULT 0 CHECK(added_count >= 0);
	ALTER TABLE calculation_versions ADD COLUMN removed_count INTEGER NOT NULL DEFAULT 0 CHECK(removed_count >= 0);
	ALTER TABLE calculation_versions ADD COLUMN changed_count INTEGER NOT NULL DEFAULT 0 CHECK(changed_count >= 0);`,
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
