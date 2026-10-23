package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"rcd_manager/internal/domain"
)

func (r *Repository) SaveSoftwareVersion(ctx context.Context, source domain.Source, binding domain.ProcessBinding, key string, value domain.SoftwareVersion) error {
	if value.Build == nil || domain.ValidateSoftwareBuild(*value.Build, value.Component) != nil || value.SourceID != source.ID || value.ObservedAt.IsZero() {
		return &domain.FieldError{Field: "software_version", Message: "valid observed software identity required"}
	}
	// Bind insertion to the current source and agent revision atomically. Late
	// responses cannot recreate deleted sources or overwrite a changed binding.
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO software_version_cache(source_id,component,binding,snapshot)
 SELECT id,?,?,? FROM sources WHERE id=? AND revision=? AND created_at=?
 AND (?='lower' OR EXISTS(SELECT 1 FROM process_bindings b WHERE b.source_id=sources.id AND b.revision=? AND b.agent_url=? AND b.instance_id=?))
 ON CONFLICT(source_id,component) DO UPDATE SET binding=excluded.binding,snapshot=excluded.snapshot`,
		value.Component, key, string(raw), source.ID, source.Revision, encodeTime(source.CreatedAt), value.Component, binding.Revision, binding.AgentURL, binding.InstanceID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return domain.ErrNotFound
	}
	return err
}

func (r *Repository) LoadSoftwareVersion(ctx context.Context, sourceID, component, key string) (domain.SoftwareVersion, error) {
	var raw string
	var value domain.SoftwareVersion
	err := r.db.QueryRowContext(ctx, `SELECT snapshot FROM software_version_cache WHERE source_id=? AND component=? AND binding=?`, sourceID, component, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return value, domain.ErrNotFound
	}
	if err != nil {
		return value, err
	}
	err = json.Unmarshal([]byte(raw), &value)
	if err == nil && (value.Build == nil || domain.ValidateSoftwareBuild(*value.Build, component) != nil) {
		err = errors.New("invalid cached software identity")
	}
	value.Cached = true
	return value, err
}
