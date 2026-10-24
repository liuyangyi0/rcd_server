package store

import (
	"context"
	"database/sql"
	"errors"
)

type WebAdmin struct {
	Username     string
	PasswordHash string
	MustChange   bool
}

func (r *Repository) WebAdmin(ctx context.Context) (WebAdmin, bool, error) {
	var user WebAdmin
	err := r.db.QueryRowContext(ctx, `SELECT username,password_hash,must_change FROM web_admin WHERE id=1`).Scan(&user.Username, &user.PasswordHash, &user.MustChange)
	if errors.Is(err, sql.ErrNoRows) {
		return user, false, nil
	}
	return user, err == nil, err
}
func (r *Repository) CreateWebAdmin(ctx context.Context, user WebAdmin) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO web_admin(id,username,password_hash,must_change) VALUES(1,?,?,?)`, user.Username, user.PasswordHash, user.MustChange)
	return err
}
func (r *Repository) UpdateWebPassword(ctx context.Context, oldHash, newHash string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE web_admin SET password_hash=?,must_change=0 WHERE id=1 AND password_hash=?`, newHash, oldHash)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
