package store

import (
	"context"
	"fmt"

	"rcd_manager/internal/domain"
)

// ImportEmptyConfiguration restores a validated, password-free configuration
// into an empty database in one transaction. Existing installations must use
// normal revisioned CRUD so an import cannot silently replace live routes.
func (r *Repository) ImportEmptyConfiguration(ctx context.Context, groups []domain.Group, sources []domain.Source) ([]domain.Group, []domain.Source, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var groupCount, sourceCount int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM groups), (SELECT COUNT(*) FROM sources)`).Scan(&groupCount, &sourceCount); err != nil {
		return nil, nil, err
	}
	if groupCount != 0 || sourceCount != 0 {
		return nil, nil, fmt.Errorf("configuration import requires an empty manager: %w", domain.ErrSourceInUse)
	}
	now := r.clock().UTC()
	for index := range groups {
		if err := validateGroup(groups[index]); err != nil {
			return nil, nil, err
		}
		groups[index].Revision = 1
		groups[index].CreatedAt, groups[index].UpdatedAt = now, now
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups
			(id, name, default_source_id, revision, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
			groups[index].ID, groups[index].Name, groups[index].DefaultSourceID, encodeTime(now), encodeTime(now)); err != nil {
			return nil, nil, fmt.Errorf("import group %s: %w", groups[index].ID, err)
		}
	}
	for index := range sources {
		if err := validateSource(sources[index]); err != nil {
			return nil, nil, err
		}
		sources[index].Revision = 1
		sources[index].CreatedAt, sources[index].UpdatedAt = now, now
		sources[index].CatalogState = domain.CatalogDiscovering
		sources[index].HasPassword = false
		if _, err := tx.ExecContext(ctx, `INSERT INTO sources
			(id, group_id, name, endpoint, security_policy, security_mode, username, password_cipher, enabled,
			catalog_state, revision, created_at, updated_at, group_bindings) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, 1, ?, ?, ?)`,
			sources[index].ID, sources[index].GroupID, sources[index].Name, sources[index].Endpoint,
			sources[index].SecurityPolicy, sources[index].SecurityMode, sources[index].Username, boolInt(sources[index].Enabled),
			domain.CatalogDiscovering, encodeTime(now), encodeTime(now), encodeBindings(sources[index].GroupBindings)); err != nil {
			return nil, nil, fmt.Errorf("import source %s: %w", sources[index].ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return groups, sources, nil
}
