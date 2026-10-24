// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Package struct {
	ID            string   `json:"id"`
	Manifest      Manifest `json:"manifest"`
	ArchiveSHA256 string   `json:"archive_sha256"`
	ArchiveSize   int64    `json:"archive_size"`
	Enabled       bool     `json:"enabled"`
	Revision      int      `json:"revision"`
	UploadedAt    string   `json:"uploaded_at"`
	UploadedBy    string   `json:"uploaded_by"`
}

type Page struct {
	Items      []Package `json:"items"`
	Total      int       `json:"total"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	TotalPages int       `json:"total_pages"`
}

type Filter struct {
	Component, Search string
	Enabled           *bool
	Page, PageSize    int
}

func (f Filter) Validate() error {
	if (f.Component != "" && !IsComponent(f.Component)) || len(f.Search) > 128 || f.Page < 1 || f.Page > 1000000 || (f.PageSize != 20 && f.PageSize != 50 && f.PageSize != 100) {
		return ErrInvalid
	}
	return nil
}

// Repository uses Semaphore's existing connection and migrations, not a second
// database. Bind is the native dialect's placeholder converter.
type Repository struct {
	DB   *sql.DB
	Bind func(string) string
}

const packageColumns = "id,manifest,archive_sha256,archive_size,enabled,revision,uploaded_at,uploaded_by"

type scanner interface{ Scan(...any) error }

// Third-party programs have no embedded RCD version report. Contradictory
// public identities for the same bytes would make runtime inventory ambiguous.
var ErrBinaryIdentityConflict = fmt.Errorf("%w: one application binary SHA-256 cannot name different build identities", ErrConflict)

type packageQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func binaryBuildIdentity(m Manifest) string {
	m.Compatibility = Compatibility{}
	m.ReleaseNotes = nil
	raw, _ := json.Marshal(m)
	return string(raw)
}

func (r *Repository) checkBinaryIdentity(ctx context.Context, query packageQuerier, project int, m Manifest) error {
	if !IsApplicationComponent(m.Component) {
		return nil
	}
	rows, err := query.QueryContext(ctx, r.Bind("select manifest from rcd_package where project_id=? and component=?"), project, m.Component)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var prior Manifest
		if err = json.Unmarshal([]byte(raw), &prior); err != nil {
			return err
		}
		if prior.BinarySHA256 == m.BinarySHA256 && binaryBuildIdentity(prior) != binaryBuildIdentity(m) {
			return ErrBinaryIdentityConflict
		}
	}
	return rows.Err()
}

func (r *Repository) CheckBinaryIdentity(ctx context.Context, project int, m Manifest) error {
	return r.checkBinaryIdentity(ctx, r.DB, project, m)
}

func scanPackage(row scanner) (p Package, err error) {
	var raw string
	var enabled int
	err = row.Scan(&p.ID, &raw, &p.ArchiveSHA256, &p.ArchiveSize, &enabled, &p.Revision, &p.UploadedAt, &p.UploadedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == 1
	err = json.Unmarshal([]byte(raw), &p.Manifest)
	return
}

func (r *Repository) Get(ctx context.Context, project int, id string) (Package, error) {
	return scanPackage(r.DB.QueryRowContext(ctx, r.Bind("select "+packageColumns+" from rcd_package where project_id=? and id=?"), project, id))
}

func (r *Repository) FindRelease(ctx context.Context, project int, m Manifest) (Package, error) {
	return scanPackage(r.DB.QueryRowContext(ctx, r.Bind("select "+packageColumns+" from rcd_package where project_id=? and component=? and release_id=?"), project, m.Component, m.ReleaseID))
}

func randomID() string {
	var id [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func (r *Repository) event(ctx context.Context, tx *sql.Tx, project int, id, action, actor string) error {
	_, err := tx.ExecContext(ctx, r.Bind("insert into rcd_package_event(project_id,id,package_id,action,actor,created_at) values(?,?,?,?,?,?)"), project, randomID(), id, action, actor, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (r *Repository) Insert(ctx context.Context, project int, validated ValidatedArchive, actor string) (Package, error) {
	raw, id := validated.Manifest.Canonical()
	p := Package{ID: id, Manifest: validated.Manifest, ArchiveSHA256: validated.ArchiveSHA256, ArchiveSize: validated.ArchiveSize, Enabled: true, Revision: 1, UploadedBy: actor, UploadedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if IsApplicationComponent(p.Manifest.Component) {
		// Recheck under the same project lock used by registration/admission so
		// separate uploader objects cannot commit incompatible identities.
		if _, err = tx.ExecContext(ctx, r.Bind("update project set name=name where id=?"), project); err != nil {
			return p, err
		}
		if err = r.checkBinaryIdentity(ctx, tx, project, p.Manifest); err != nil {
			return p, err
		}
	}
	_, err = tx.ExecContext(ctx, r.Bind("insert into rcd_package(project_id,id,component,release_id,version,target,manifest,archive_sha256,archive_size,enabled,revision,uploaded_at,uploaded_by) values(?,?,?,?,?,?,?,?,?,?,?,?,?)"), project, id, p.Manifest.Component, p.Manifest.ReleaseID, p.Manifest.Version, p.Manifest.Target, string(raw), p.ArchiveSHA256, p.ArchiveSize, 1, 1, p.UploadedAt, actor)
	if err != nil {
		return p, err
	}
	if err = r.event(ctx, tx, project, id, "uploaded", actor); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (r *Repository) SetEnabled(ctx context.Context, project int, id string, revision int, enabled bool, actor string) (Package, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return Package{}, err
	}
	defer tx.Rollback()
	value := 0
	if enabled {
		value = 1
	}
	result, err := tx.ExecContext(ctx, r.Bind("update rcd_package set enabled=?,revision=revision+1 where project_id=? and id=? and revision=?"), value, project, id, revision)
	if err != nil {
		return Package{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Package{}, err
	}
	if count != 1 {
		return Package{}, ErrConflict
	}
	action := "disabled"
	if enabled {
		action = "enabled"
	}
	if err = r.event(ctx, tx, project, id, action, actor); err != nil {
		return Package{}, err
	}
	p, err := scanPackage(tx.QueryRowContext(ctx, r.Bind("select "+packageColumns+" from rcd_package where project_id=? and id=?"), project, id))
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (r *Repository) List(ctx context.Context, project int, filter Filter) (Page, error) {
	page := Page{Items: []Package{}, Page: filter.Page, PageSize: filter.PageSize}
	if err := filter.Validate(); err != nil {
		return page, err
	}
	where := " where project_id=?"
	args := []any{project}
	if filter.Component != "" {
		where += " and component=?"
		args = append(args, filter.Component)
	}
	if filter.Enabled != nil {
		v := 0
		if *filter.Enabled {
			v = 1
		}
		where += " and enabled=?"
		args = append(args, v)
	}
	if filter.Search != "" {
		search := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(filter.Search) + "%"
		where += " and (version like ? escape '!' or release_id like ? escape '!')"
		args = append(args, search, search)
	}
	// One read transaction keeps the exact count and page on the same snapshot.
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, r.Bind("select count(*) from rcd_package"+where), args...).Scan(&page.Total); err != nil {
		return page, err
	}
	page.TotalPages = (page.Total + filter.PageSize - 1) / filter.PageSize
	query := "select " + packageColumns + " from rcd_package" + where + " order by uploaded_at desc,id limit ? offset ?"
	args = append(args, filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := tx.QueryContext(ctx, r.Bind(query), args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			rows.Close()
			return page, err
		}
		page.Items = append(page.Items, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	return page, tx.Commit()
}

func (p Package) ETag() string { return fmt.Sprintf(`"%d"`, p.Revision) }
