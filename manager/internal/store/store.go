package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"rcd_manager/internal/domain"
)

// likeLiteral makes user search text a literal LIKE operand: with ESCAPE '\' after the
// pattern, % and _ in the text (common in KKS identifiers) match only themselves.
func likeLiteral(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

type Repository struct {
	db        *sql.DB
	protector SecretProtector
	clock     func() time.Time
}

func Open(ctx context.Context, path string, protector SecretProtector) (*Repository, error) {
	if strings.TrimSpace(path) == "" {
		return nil, &domain.FieldError{Field: "database_path", Message: "must not be empty"}
	}
	if protector == nil {
		return nil, &domain.FieldError{Field: "secret_protector", Message: "must not be nil"}
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Every connection receives the DSN PRAGMAs; a small pool lets WAL readers
	// inspect catalog/audit data without waiting behind a configuration write.
	// A private :memory: database is connection-local and must stay on one handle.
	connections := 4
	if path == ":memory:" {
		connections = 1
	}
	db.SetMaxOpenConns(connections)
	db.SetMaxIdleConns(connections)
	for _, pragma := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Repository{db: db, protector: protector, clock: time.Now}, nil
}

func (r *Repository) Close() error { return r.db.Close() }

func (r *Repository) Ping(ctx context.Context) error { return r.db.PingContext(ctx) }

func (r *Repository) CreateGroup(ctx context.Context, group domain.Group) (domain.Group, error) {
	if err := validateGroup(group); err != nil {
		return domain.Group{}, err
	}
	now := r.clock().UTC()
	group.Revision = 1
	group.CreatedAt = now
	group.UpdatedAt = now
	_, err := r.db.ExecContext(ctx, `INSERT INTO groups
		(id, name, default_source_id, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		group.ID, group.Name, group.DefaultSourceID, group.Revision, encodeTime(now), encodeTime(now))
	if isConstraint(err) {
		return domain.Group{}, domain.ErrAlreadyExists
	}
	if err != nil {
		return domain.Group{}, fmt.Errorf("create group: %w", err)
	}
	return group, nil
}

func (r *Repository) GetGroup(ctx context.Context, id string) (domain.Group, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, name, default_source_id, revision, created_at, updated_at FROM groups WHERE id = ?`, id)
	return scanGroup(row)
}

func (r *Repository) ListGroups(ctx context.Context) ([]domain.Group, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, default_source_id, revision, created_at, updated_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	groups := make([]domain.Group, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (r *Repository) UpdateGroup(ctx context.Context, group domain.Group, expectedRevision uint64) (domain.Group, error) {
	if err := validateGroup(group); err != nil {
		return domain.Group{}, err
	}
	now := r.clock().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Group{}, fmt.Errorf("begin group update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE groups SET name = ?, default_source_id = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
		group.Name, group.DefaultSourceID, encodeTime(now), group.ID, expectedRevision)
	if err != nil {
		return domain.Group{}, fmt.Errorf("update group: %w", err)
	}
	if err := requireMutationTx(ctx, tx, result, "groups", group.ID, expectedRevision); err != nil {
		return domain.Group{}, err
	}
	persisted, err := scanGroup(tx.QueryRowContext(ctx, `SELECT id, name, default_source_id, revision, created_at, updated_at FROM groups WHERE id = ?`, group.ID))
	if err != nil {
		return domain.Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Group{}, fmt.Errorf("commit group update: %w", err)
	}
	return persisted, nil
}

func (r *Repository) DeleteGroup(ctx context.Context, id string, expectedRevision uint64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM groups WHERE id = ? AND revision = ?`, id, expectedRevision)
	if isConstraint(err) {
		return fmt.Errorf("group still has sources: %w", domain.ErrGroupInUse)
	}
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	return r.requireMutation(ctx, result, "groups", id, expectedRevision)
}

// DeleteGroupCascade atomically removes a group and all of its configured
// sources. Catalog rows are removed by their source foreign-key cascade; audit
// rows intentionally remain immutable.
func (r *Repository) DeleteGroupCascade(ctx context.Context, id string, expectedRevision uint64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin group cascade delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var actual uint64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM groups WHERE id = ?`, id).Scan(&actual); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("read group revision before delete: %w", err)
	}
	if actual != expectedRevision {
		return &domain.RevisionConflictError{Expected: expectedRevision, Actual: actual}
	}
	sourceRows, err := tx.QueryContext(ctx, sourceSelect)
	if err != nil {
		return err
	}
	for sourceRows.Next() {
		source, scanErr := scanSource(sourceRows)
		if scanErr != nil {
			sourceRows.Close()
			return scanErr
		}
		if len(source.GroupBindings) > 0 && source.HasGroup(id) {
			sourceRows.Close()
			return domain.ErrGroupInUse
		}
	}
	if err := sourceRows.Err(); err != nil {
		sourceRows.Close()
		return err
	}
	if err := sourceRows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE group_id = ?`, id); err != nil {
		return fmt.Errorf("delete group sources: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = ? AND revision = ?`, id, expectedRevision)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return &domain.RevisionConflictError{Expected: expectedRevision, Actual: actual}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit group cascade delete: %w", err)
	}
	return nil
}

func (r *Repository) CreateSource(ctx context.Context, source domain.Source, password string) (domain.Source, error) {
	if err := validateSource(source); err != nil {
		return domain.Source{}, err
	}
	for _, binding := range source.GroupBindings {
		if _, err := r.GetGroup(ctx, binding.GroupID); err != nil {
			return domain.Source{}, fmt.Errorf("binding group %q does not exist: %w", binding.GroupID, domain.ErrInvalid)
		}
	}
	cipher, err := r.protectPassword(ctx, password)
	if err != nil {
		return domain.Source{}, err
	}
	now := r.clock().UTC()
	source.Revision = 1
	source.CreatedAt = now
	source.UpdatedAt = now
	source.HasPassword = len(cipher) > 0
	if source.CatalogState == "" {
		source.CatalogState = domain.CatalogDiscovering
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO sources
		(id, group_id, name, endpoint, security_policy, security_mode, username, password_cipher, enabled, catalog_state, revision, created_at, updated_at, group_bindings)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		source.ID, source.GroupID, source.Name, source.Endpoint, source.SecurityPolicy, source.SecurityMode,
		source.Username, nullableBytes(cipher), boolInt(source.Enabled), source.CatalogState, source.Revision, encodeTime(now), encodeTime(now), encodeBindings(source.GroupBindings))
	if isConstraint(err) {
		if _, lookupErr := r.GetSource(ctx, source.ID); lookupErr == nil {
			return domain.Source{}, domain.ErrAlreadyExists
		}
		if _, lookupErr := r.GetGroup(ctx, source.GroupID); errors.Is(lookupErr, domain.ErrNotFound) {
			return domain.Source{}, fmt.Errorf("source group does not exist: %w", domain.ErrInvalid)
		}
		return domain.Source{}, fmt.Errorf("create source constraint: %w", err)
	}
	if err != nil {
		return domain.Source{}, fmt.Errorf("create source: %w", err)
	}
	return source, nil
}

// UpdateSource preserves the existing password when password is nil. A pointer
// to an empty string explicitly clears it.
func (r *Repository) UpdateSource(ctx context.Context, source domain.Source, password *string, expectedRevision uint64) (domain.Source, error) {
	return r.updateSource(ctx, source, password, expectedRevision, false)
}

// UpdateSourceAndResetCatalog atomically changes connection/configuration data,
// clears the persisted point catalog, and marks discovery pending unless an
// explicit rejection is still quarantined. It prevents a failed catalog reset
// from leaving a newly revised source paired with stale schema rows.
func (r *Repository) UpdateSourceAndResetCatalog(ctx context.Context, source domain.Source, password *string, expectedRevision uint64) (domain.Source, error) {
	return r.updateSource(ctx, source, password, expectedRevision, true)
}

func (r *Repository) updateSource(ctx context.Context, source domain.Source, password *string, expectedRevision uint64, resetCatalog bool) (domain.Source, error) {
	if err := validateSource(source); err != nil {
		return domain.Source{}, err
	}
	now := r.clock().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Source{}, fmt.Errorf("begin source update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, binding := range source.GroupBindings {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=?`, binding.GroupID).Scan(&id); err != nil {
			return domain.Source{}, fmt.Errorf("binding group %q does not exist: %w", binding.GroupID, domain.ErrInvalid)
		}
	}
	var result sql.Result
	if password == nil {
		result, err = tx.ExecContext(ctx, `UPDATE sources SET group_id = ?, name = ?, endpoint = ?, security_policy = ?, security_mode = ?, username = ?, enabled = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
			source.GroupID, source.Name, source.Endpoint, source.SecurityPolicy, source.SecurityMode, source.Username,
			boolInt(source.Enabled), encodeTime(now), source.ID, expectedRevision)
	} else {
		cipher, protectErr := r.protectPassword(ctx, *password)
		if protectErr != nil {
			return domain.Source{}, protectErr
		}
		result, err = tx.ExecContext(ctx, `UPDATE sources SET group_id = ?, name = ?, endpoint = ?, security_policy = ?, security_mode = ?, username = ?, password_cipher = ?, enabled = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
			source.GroupID, source.Name, source.Endpoint, source.SecurityPolicy, source.SecurityMode, source.Username,
			nullableBytes(cipher), boolInt(source.Enabled), encodeTime(now), source.ID, expectedRevision)
	}
	if isConstraint(err) {
		return domain.Source{}, fmt.Errorf("source group does not exist: %w", domain.ErrInvalid)
	}
	if err != nil {
		return domain.Source{}, fmt.Errorf("update source: %w", err)
	}
	if err := requireMutationTx(ctx, tx, result, "sources", source.ID, expectedRevision); err != nil {
		return domain.Source{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sources SET group_bindings = ? WHERE id = ?`, encodeBindings(source.GroupBindings), source.ID); err != nil {
		return domain.Source{}, err
	}
	if resetCatalog {
		// A connection/mapping edit is not proof of a valid replacement
		// discovery. Discard stale topology but keep an explicit quarantine.
		if _, err := tx.ExecContext(ctx, `UPDATE source_topology SET manifest = 'null', discovered_at = ? WHERE source_id = ?`, encodeTime(now), source.ID); err != nil {
			return domain.Source{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_nodes WHERE source_id = ?`, source.ID); err != nil {
			return domain.Source{}, fmt.Errorf("clear source catalog: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sources SET catalog_state = CASE
			WHEN EXISTS (SELECT 1 FROM source_topology WHERE source_id = ? AND quarantine_reason <> '') THEN ? ELSE ? END
			WHERE id = ?`, source.ID, domain.CatalogConflict, domain.CatalogDiscovering, source.ID); err != nil {
			return domain.Source{}, fmt.Errorf("reset source catalog state: %w", err)
		}
	}
	persisted, err := scanSource(tx.QueryRowContext(ctx, sourceSelect+` WHERE id = ?`, source.ID))
	if err != nil {
		return domain.Source{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Source{}, fmt.Errorf("commit source update: %w", err)
	}
	return persisted, nil
}

func (r *Repository) GetSource(ctx context.Context, id string) (domain.Source, error) {
	row := r.db.QueryRowContext(ctx, sourceSelect+` WHERE id = ?`, id)
	return scanSource(row)
}

func (r *Repository) ListSources(ctx context.Context) ([]domain.Source, error) {
	rows, err := r.db.QueryContext(ctx, sourceSelect+` ORDER BY group_id, id`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	sources := make([]domain.Source, 0)
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (r *Repository) DeleteSource(ctx context.Context, id string, expectedRevision uint64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM sources WHERE id = ? AND revision = ?`, id, expectedRevision)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	return r.requireMutation(ctx, result, "sources", id, expectedRevision)
}

func (r *Repository) SourceCredential(ctx context.Context, id string) (username, password string, err error) {
	var cipher []byte
	err = r.db.QueryRowContext(ctx, `SELECT username, password_cipher FROM sources WHERE id = ?`, id).Scan(&username, &cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", domain.ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read source credential: %w", err)
	}
	if len(cipher) == 0 {
		return username, "", nil
	}
	plain, err := r.protector.Unprotect(ctx, cipher)
	if err != nil {
		return "", "", fmt.Errorf("unprotect source credential: %w", err)
	}
	return username, string(plain), nil
}

const sourceSelect = `SELECT id, group_id, name, endpoint, security_policy, security_mode, username,
	CASE WHEN password_cipher IS NULL OR length(password_cipher) = 0 THEN 0 ELSE 1 END,
	enabled, catalog_state, revision, created_at, updated_at, group_bindings FROM sources`

type scanner interface{ Scan(...any) error }

func scanGroup(row scanner) (domain.Group, error) {
	var group domain.Group
	var created, updated string
	if err := row.Scan(&group.ID, &group.Name, &group.DefaultSourceID, &group.Revision, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Group{}, domain.ErrNotFound
		}
		return domain.Group{}, fmt.Errorf("scan group: %w", err)
	}
	var err error
	if group.CreatedAt, err = decodeTime(created); err != nil {
		return domain.Group{}, err
	}
	if group.UpdatedAt, err = decodeTime(updated); err != nil {
		return domain.Group{}, err
	}
	return group, nil
}

func scanSource(row scanner) (domain.Source, error) {
	var source domain.Source
	var hasPassword, enabled int
	var created, updated string
	var bindings string
	if err := row.Scan(&source.ID, &source.GroupID, &source.Name, &source.Endpoint, &source.SecurityPolicy,
		&source.SecurityMode, &source.Username, &hasPassword, &enabled, &source.CatalogState,
		&source.Revision, &created, &updated, &bindings); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Source{}, domain.ErrNotFound
		}
		return domain.Source{}, fmt.Errorf("scan source: %w", err)
	}
	source.HasPassword = hasPassword != 0
	if err := json.Unmarshal([]byte(bindings), &source.GroupBindings); err != nil {
		return domain.Source{}, fmt.Errorf("decode source bindings: %w", err)
	}
	source.Enabled = enabled != 0
	var err error
	if source.CreatedAt, err = decodeTime(created); err != nil {
		return domain.Source{}, err
	}
	if source.UpdatedAt, err = decodeTime(updated); err != nil {
		return domain.Source{}, err
	}
	return source, nil
}

func (r *Repository) requireMutation(ctx context.Context, result sql.Result, table, id string, expected uint64) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	var actual uint64
	err = r.db.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE id = ?`, id).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	return &domain.RevisionConflictError{Expected: expected, Actual: actual}
}

func requireMutationTx(ctx context.Context, tx *sql.Tx, result sql.Result, table, id string, expected uint64) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	var actual uint64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE id = ?`, id).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	return &domain.RevisionConflictError{Expected: expected, Actual: actual}
}

func (r *Repository) protectPassword(ctx context.Context, password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	cipher, err := r.protector.Protect(ctx, []byte(password))
	if err != nil {
		return nil, fmt.Errorf("protect source credential: %w", err)
	}
	return cipher, nil
}

func validateGroup(group domain.Group) error {
	if strings.TrimSpace(group.ID) == "" {
		return &domain.FieldError{Field: "id", Message: "must not be empty"}
	}
	if strings.TrimSpace(group.Name) == "" {
		return &domain.FieldError{Field: "name", Message: "must not be empty"}
	}
	return nil
}

func validateSource(source domain.Source) error {
	for field, value := range map[string]string{"id": source.ID, "group_id": source.GroupID, "name": source.Name, "endpoint": source.Endpoint} {
		if strings.TrimSpace(value) == "" {
			return &domain.FieldError{Field: field, Message: "must not be empty"}
		}
	}
	return domain.ValidateGroupBindings(source.GroupBindings)
}

func encodeBindings(bindings []domain.GroupBinding) string {
	if len(bindings) == 0 {
		return "[]"
	}
	encoded, _ := json.Marshal(bindings)
	return string(encoded)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sqliteDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_foreign_keys=on&_busy_timeout=5000&_journal_mode=wal&_synchronous=normal"
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func isConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}

func encodeTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func decodeTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode stored timestamp: %w", err)
	}
	return parsed, nil
}
