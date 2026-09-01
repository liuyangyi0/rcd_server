package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LowerLogCursor is intentionally tiny. Real-time log contents stay out of
// SQLite; the cursor only makes HTTP retry delivery idempotent.
type LowerLogCursor struct {
	SourceID        string
	BootID          string
	AcceptedThrough uint64
	UpdatedAt       time.Time
}

func (r *Repository) GetLowerLogCursor(ctx context.Context, sourceID, bootID string) (LowerLogCursor, error) {
	var value LowerLogCursor
	var updated string
	err := r.db.QueryRowContext(ctx, `SELECT source_id, boot_id, accepted_through, updated_at
		FROM lower_log_ingest_cursor WHERE source_id = ? AND boot_id = ?`, sourceID, bootID).
		Scan(&value.SourceID, &value.BootID, &value.AcceptedThrough, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return LowerLogCursor{SourceID: sourceID, BootID: bootID}, nil
	}
	if err != nil {
		return LowerLogCursor{}, fmt.Errorf("read lower log cursor: %w", err)
	}
	value.UpdatedAt, err = decodeTime(updated)
	if err != nil {
		return LowerLogCursor{}, fmt.Errorf("decode lower log cursor time: %w", err)
	}
	return value, nil
}

func (r *Repository) SaveLowerLogCursor(ctx context.Context, sourceID, bootID string, acceptedThrough uint64, updatedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO lower_log_ingest_cursor
		(source_id, boot_id, accepted_through, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(source_id, boot_id) DO UPDATE SET
			accepted_through = CASE WHEN excluded.accepted_through > accepted_through THEN excluded.accepted_through ELSE accepted_through END,
			updated_at = CASE WHEN excluded.accepted_through >= accepted_through THEN excluded.updated_at ELSE updated_at END`,
		sourceID, bootID, acceptedThrough, encodeTime(updatedAt.UTC()))
	if err != nil {
		return fmt.Errorf("save lower log cursor: %w", err)
	}
	return nil
}

func (r *Repository) LatestLowerLogReceivedAt(ctx context.Context, sourceID string) (time.Time, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `SELECT updated_at FROM lower_log_ingest_cursor
		WHERE source_id = ? ORDER BY julianday(updated_at) DESC LIMIT 1`, sourceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read latest lower log receive time: %w", err)
	}
	value, err := decodeTime(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode latest lower log receive time: %w", err)
	}
	return value, nil
}
