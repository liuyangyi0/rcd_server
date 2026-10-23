// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var applicationSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var applicationArgument = regexp.MustCompile(`^[A-Za-z0-9_./:=,@+% -]{1,256}$`)

// A definition never contains arbitrary paths, environment variables, scripts,
// root users or shell fragments. Runtime settings are immutable after creation.
type ApplicationRequest struct {
	Component                 string   `json:"component"`
	Name                      string   `json:"name"`
	Arguments                 []string `json:"arguments"`
	GracefulShutdownConfirmed bool     `json:"graceful_shutdown_confirmed"`
}

type Application struct {
	Component                 string   `json:"component"`
	Name                      string   `json:"name"`
	Revision                  int      `json:"revision"`
	Arguments                 []string `json:"arguments"`
	ServiceUser               string   `json:"service_user"`
	InstallPath               string   `json:"install_path"`
	WorkDir                   string   `json:"work_dir"`
	ServiceUnit               string   `json:"service_unit"`
	VersionMode               string   `json:"version_mode"`
	DefinitionSHA256          string   `json:"definition_sha256"`
	CreatedAt                 string   `json:"created_at"`
	CreatedBy                 string   `json:"created_by"`
	GracefulShutdownConfirmed bool     `json:"graceful_shutdown_confirmed"`
}

func IsBuiltinComponent(s string) bool {
	return s == "lower" || s == "manager" || s == "agent" || s == "communicate"
}

func IsApplicationComponent(s string) bool {
	if !applicationSlug.MatchString(s) || strings.HasSuffix(s, "-") || strings.Contains(s, "--") || IsBuiltinComponent(s) {
		return false
	}
	switch s {
	case "system", "systemd", "semaphore", "rcd", "root", "sudo", "ssh", "sshd", "init", "login", "kernel", "lower-stack":
		return false
	}
	return !strings.HasPrefix(s, "rcd-")
}

func (req ApplicationRequest) Validate() error {
	if !req.GracefulShutdownConfirmed || !IsApplicationComponent(req.Component) || !utf8.ValidString(req.Name) || strings.TrimSpace(req.Name) != req.Name || utf8.RuneCountInString(req.Name) < 1 || utf8.RuneCountInString(req.Name) > 80 || req.Arguments == nil || len(req.Arguments) > 16 {
		return fmt.Errorf("%w: 软件标识需为小写字母开头的 1–32 位字母数字短横线，名称 1–80 字，参数必须为数组且最多 16 项", ErrInvalid)
	}
	for _, ch := range req.Name {
		if unicode.IsControl(ch) {
			return fmt.Errorf("%w: 软件名称不能包含控制字符", ErrInvalid)
		}
	}
	total := 0
	for _, arg := range req.Arguments {
		total += len(arg)
		if !applicationArgument.MatchString(arg) || strings.TrimSpace(arg) != arg || strings.Contains(arg, "..") || strings.ContainsRune(arg, '%') {
			return fmt.Errorf("%w: 启动参数仅接受普通字母数字及 . / : = , @ + _ - 和空格，不支持 shell、变量展开或相对路径跳转", ErrInvalid)
		}
	}
	if total > 2048 {
		return fmt.Errorf("%w: 启动参数总长度超过 2048 字节", ErrInvalid)
	}
	return nil
}

func applicationDefinition(req ApplicationRequest) Application {
	a := Application{Component: req.Component, Name: req.Name, Revision: 1, Arguments: append([]string{}, req.Arguments...), ServiceUser: "device_owner", InstallPath: "/opt/rcd/apps/" + req.Component + "/bin/" + req.Component, WorkDir: "/var/lib/rcd/apps/" + req.Component, ServiceUnit: "rcd-app-" + req.Component + ".service", VersionMode: "manifest_sha256", GracefulShutdownConfirmed: true}
	// Hash only immutable runtime identity. An owner marker on the target also
	// pins project_id so a second project cannot overwrite the same host slug.
	raw, _ := json.Marshal(a)
	sum := sha256.Sum256(raw)
	a.DefinitionSHA256 = hex.EncodeToString(sum[:])
	return a
}

func scanApplication(row scanner) (Application, error) {
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Application{}, ErrNotFound
		}
		return Application{}, err
	}
	var a Application
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return a, err
	}
	req := ApplicationRequest{Component: a.Component, Name: a.Name, Arguments: a.Arguments, GracefulShutdownConfirmed: a.GracefulShutdownConfirmed}
	if err := req.Validate(); err != nil {
		return a, err
	}
	expected := applicationDefinition(req)
	if a.Revision != 1 || a.DefinitionSHA256 != expected.DefinitionSHA256 || a.InstallPath != expected.InstallPath || a.ServiceUnit != expected.ServiceUnit || a.WorkDir != expected.WorkDir || a.ServiceUser != expected.ServiceUser || a.VersionMode != expected.VersionMode {
		return a, ErrInvalid
	}
	return a, nil
}

func (r *Repository) GetApplication(ctx context.Context, project int, component string) (Application, error) {
	if project < 1 || !IsApplicationComponent(component) {
		return Application{}, ErrInvalid
	}
	return scanApplication(r.DB.QueryRowContext(ctx, r.Bind("select body from rcd_application where project_id=? and component=?"), project, component))
}

func (r *Repository) ListApplications(ctx context.Context, project int) ([]Application, error) {
	if project < 1 {
		return nil, ErrInvalid
	}
	rows, err := r.DB.QueryContext(ctx, r.Bind("select body from rcd_application where project_id=? order by component"), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Application{}
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (r *Repository) RegisterApplication(ctx context.Context, project int, req ApplicationRequest, actor string) (Application, bool, error) {
	if project < 1 || actor == "" {
		return Application{}, false, ErrInvalid
	}
	if err := req.Validate(); err != nil {
		return Application{}, false, err
	}
	wanted := applicationDefinition(req)
	existing, err := r.GetApplication(ctx, project, req.Component)
	if err == nil {
		if existing.DefinitionSHA256 != wanted.DefinitionSHA256 {
			return Application{}, false, ErrConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Application{}, false, err
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Application{}, false, err
	}
	defer tx.Rollback()
	// Serialize definitions on the existing project row. A count followed by an
	// insert without this write lock could admit the 33rd concurrent definition.
	_, err = tx.ExecContext(ctx, r.Bind("update project set name=name where id=?"), project)
	if err != nil {
		return Application{}, false, err
	}
	var projectID int
	if err = tx.QueryRowContext(ctx, r.Bind("select id from project where id=?"), project).Scan(&projectID); err != nil {
		return Application{}, false, ErrNotFound
	}
	existing, err = scanApplication(tx.QueryRowContext(ctx, r.Bind("select body from rcd_application where project_id=? and component=?"), project, req.Component))
	if err == nil {
		if existing.DefinitionSHA256 != wanted.DefinitionSHA256 {
			return Application{}, false, ErrConflict
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return Application{}, false, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, r.Bind("select count(*) from rcd_application where project_id=?"), project).Scan(&count); err != nil {
		return Application{}, false, err
	}
	if count >= 32 {
		return Application{}, false, fmt.Errorf("%w: 每个项目最多注册 32 种额外软件", ErrInvalid)
	}
	wanted.CreatedAt, wanted.CreatedBy = time.Now().UTC().Format(time.RFC3339Nano), actor
	raw, _ := json.Marshal(wanted)
	_, err = tx.ExecContext(ctx, r.Bind("insert into rcd_application(project_id,component,body,created_at,created_by) values(?,?,?,?,?)"), project, req.Component, string(raw), wanted.CreatedAt, actor)
	if err == nil {
		return wanted, true, tx.Commit()
	}
	tx.Rollback()
	// A concurrent identical registration is idempotent, never an overwrite.
	existing, checkErr := r.GetApplication(ctx, project, req.Component)
	if checkErr == nil {
		if existing.DefinitionSHA256 != wanted.DefinitionSHA256 {
			return Application{}, false, ErrConflict
		}
		return existing, false, nil
	}
	return Application{}, false, err
}

// Manifest validation checks syntax only. Registration is project-scoped and
// must be checked at every mutation boundary, not trusted from browser fields.
func (r *Repository) RequireRegisteredComponent(ctx context.Context, project int, component string) error {
	if IsBuiltinComponent(component) {
		return nil
	}
	_, err := r.GetApplication(ctx, project, component)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: 软件种类 %s 尚未注册，请先添加软件种类", ErrInvalid, component)
	}
	return err
}
