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
	"sort"
	"strconv"
	"time"
)

type OperationTarget struct {
	DeviceID string `json:"device_id"`
	Revision int    `json:"revision"`
}
type OperationRequest struct {
	Kind            string            `json:"kind,omitempty"`
	Component       string            `json:"component,omitempty"`
	PackageID       string            `json:"package_id"`
	PackageRevision int               `json:"package_revision"`
	Action          string            `json:"action"`
	Targets         []OperationTarget `json:"targets"`
	CommandsPaused  bool              `json:"commands_paused"`
}
type ReleaseTarget struct {
	ExpectedBinarySHA256 string         `json:"expected_binary_sha256,omitempty"`
	PreviousBinarySHA256 string         `json:"previous_binary_sha256,omitempty"`
	PreviousState        string         `json:"previous_state,omitempty"`
	DeviceID             string         `json:"device_id"`
	Name                 string         `json:"name"`
	Address              string         `json:"address"`
	HostAlias            string         `json:"host_alias"`
	State                string         `json:"state"`
	Error                string         `json:"error"`
	Stages               []ReleaseStage `json:"stages"`
	Acceptance           map[string]any `json:"acceptance,omitempty"`
}
type ReleaseStage struct {
	Error     *ReleaseError `json:"error,omitempty"`
	Stage     string        `json:"stage"`
	State     string        `json:"state"`
	StartedAt string        `json:"started_at"`
	EndedAt   string        `json:"ended_at,omitempty"`
	ErrorType string        `json:"error_type,omitempty"`
}
type ReleaseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Operation struct {
	Application             *Application     `json:"application,omitempty"`
	PostInspectionRequested bool             `json:"post_inspection_requested"`
	ID                      string           `json:"id"`
	Request                 OperationRequest `json:"request"`
	Package                 Manifest         `json:"package"`
	Targets                 []ReleaseTarget  `json:"targets"`
	Actor                   string           `json:"actor"`
	UserID                  int              `json:"user_id"`
	TemplateID              int              `json:"template_id"`
	InventoryID             int              `json:"inventory_id"`
	TaskID                  int              `json:"task_id"`
	State                   string           `json:"state"`
	Revision                int              `json:"revision"`
	CreatedAt               string           `json:"created_at"`
	Error                   string           `json:"error"`
}

func (o Operation) ETag() string { return strconv.Quote(strconv.Itoa(o.Revision)) }
func (o Operation) KnownTerminal() bool {
	return o.State == "succeeded" || o.State == "recovered" || o.State == "partial_failed" || o.State == "failed_before_install" || o.State == "failed_before_execution" || o.State == "cancelled"
}

func (o Operation) ServiceControl() bool { return o.Request.Kind == "service_control" }
func (o Operation) Component() string {
	if o.ServiceControl() {
		return o.Request.Component
	}
	return o.Package.Component
}

func scanOperation(row scanner) (o Operation, err error) {
	var raw string
	var revision, task int
	var state string
	err = row.Scan(&raw, &state, &revision, &task)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(raw), &o); err != nil {
		return
	}
	o.State, o.Revision, o.TaskID = state, revision, task
	return
}
func (r *Repository) Operation(ctx context.Context, project int, id string) (Operation, error) {
	return scanOperation(r.DB.QueryRowContext(ctx, r.Bind("select body,state,revision,task_id from rcd_operation where project_id=? and id=?"), project, id))
}
func (r *Repository) OperationForTask(ctx context.Context, project, task int) (Operation, error) {
	return scanOperation(r.DB.QueryRowContext(ctx, r.Bind("select body,state,revision,task_id from rcd_operation where project_id=? and task_id=?"), project, task))
}
func (r *Repository) Operations(ctx context.Context, project int) ([]Operation, error) {
	rows, err := r.DB.QueryContext(ctx, r.Bind("select body,state,revision,task_id from rcd_operation where project_id=? order by created_at desc,id desc"), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Operation{}
	for rows.Next() {
		o, e := scanOperation(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

func (r *Repository) PlanOperation(ctx context.Context, project int, req OperationRequest) (Operation, error) {
	if req.Kind == "service_control" {
		return r.planServiceControl(ctx, project, req)
	}
	if req.Kind != "" || req.Component != "" {
		return Operation{}, ErrInvalid
	}
	o := Operation{Request: req}
	if !req.CommandsPaused || (req.Action != "upgrade" && req.Action != "restore") || len(req.Targets) < 1 || len(req.Targets) > 3 || !digestPattern.MatchString(req.PackageID) {
		return o, ErrInvalid
	}
	p, err := r.Get(ctx, project, req.PackageID)
	if err != nil {
		return o, err
	}
	if !p.Enabled || req.PackageRevision != p.Revision {
		return o, ErrConflict
	}
	o.Package = p.Manifest
	if err = r.RequireRegisteredComponent(ctx, project, p.Manifest.Component); err != nil {
		return o, err
	}
	if !IsBuiltinComponent(p.Manifest.Component) {
		a, e := r.GetApplication(ctx, project, p.Manifest.Component)
		if e != nil {
			return o, e
		}
		o.Application = &a
	}
	seen := map[string]bool{}
	for _, target := range req.Targets {
		if seen[target.DeviceID] {
			return o, ErrInvalid
		}
		seen[target.DeviceID] = true
		d, e := r.Device(ctx, project, target.DeviceID)
		if e != nil {
			return o, e
		}
		if _, e = r.ActiveHostSetup(ctx, project, d.Address); e == nil {
			return o, ErrConflict
		} else if !errors.Is(e, ErrNotFound) {
			return o, e
		}
		if d.Revision != target.Revision {
			return o, ErrConflict
		}
		if d.ReleaseTemplateID < 1 || d.State != "checked" {
			return o, fmt.Errorf("%w: %s 需要成功的近期检查和发布模板绑定", ErrInvalid, d.Name)
		}
		if o.TemplateID != 0 && (o.TemplateID != d.ReleaseTemplateID || o.InventoryID != d.InventoryID) {
			return o, fmt.Errorf("%w: 多机发布必须使用同一个受控库存和发布模板", ErrInvalid)
		}
		o.TemplateID, o.InventoryID = d.ReleaseTemplateID, d.InventoryID
		var component *ComponentObservation
		for i := range d.Components {
			if d.Components[i].Component == p.Manifest.Component {
				component = &d.Components[i]
			}
		}
		if component == nil || (component.State != "running" && component.State != "stopped") {
			return o, fmt.Errorf("%w: %s 程序状态无法确认", ErrInvalid, d.Name)
		}
		build := component.RunningBuild
		if component.State == "stopped" {
			build = component.DiskBuild
		}
		if build == nil || Architecture(build.Target) != Architecture(p.Manifest.Target) {
			return o, fmt.Errorf("%w: %s 缺少可信平台信息或架构不兼容", ErrInvalid, d.Name)
		}
		if reason := CompatibilityReason(d, p.Manifest); reason != "" {
			return o, fmt.Errorf("%w: %s %s", ErrInvalid, d.Name, reason)
		}
		if req.Action == "restore" {
			history, e := r.Installations(ctx, project, d.ID, p.Manifest.Component)
			if e != nil {
				return o, e
			}
			found := false
			for _, old := range history {
				if old.PackageID == req.PackageID {
					found = true
				}
			}
			if !found {
				return o, fmt.Errorf("%w: 此设备没有该软件包的成功安装记录", ErrInvalid)
			}
		}
		// Pin the installed image, not a later mutable inspection or a claimed
		// hash in a recovery report. The publisher backs up this on-disk binary.
		prior := component.DiskSHA256
		if !digestPattern.MatchString(prior) {
			prior = "" // Old evidence may upgrade, but cannot prove recovery.
		}
		o.Targets = append(o.Targets, ReleaseTarget{DeviceID: d.ID, Name: d.Name, Address: d.Address, HostAlias: d.HostAlias, State: "waiting", Stages: []ReleaseStage{}, PreviousBinarySHA256: prior, PreviousState: component.State})
	}
	// Explicit operational order, not lexicographic inventory/IP order.
	rank := func(address string) int {
		switch address {
		case "192.168.10.30":
			return 0
		case "192.168.10.58":
			return 1
		case "192.168.10.46":
			return 2
		}
		return 3
	}
	sort.Slice(o.Targets, func(i, j int) bool {
		a, b := rank(o.Targets[i].Address), rank(o.Targets[j].Address)
		if a == b {
			return o.Targets[i].DeviceID < o.Targets[j].DeviceID
		}
		return a < b
	})
	return o, nil
}

func (r *Repository) CreateOperation(ctx context.Context, project int, key string, req OperationRequest, actor string, user int) (Operation, string, bool, error) {
	if !identifier.MatchString(key) || user < 1 || actor == "" {
		return Operation{}, "", false, ErrInvalid
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var id, existingHash, nonce string
	err := r.DB.QueryRowContext(ctx, r.Bind("select id,request_hash,nonce from rcd_operation where project_id=? and request_key=?"), project, key).Scan(&id, &existingHash, &nonce)
	if err == nil {
		if hash != existingHash {
			return Operation{}, "", false, ErrConflict
		}
		o, e := r.Operation(ctx, project, id)
		return o, "", false, e
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, "", false, err
	}
	o, err := r.PlanOperation(ctx, project, req)
	if err != nil {
		return o, "", false, err
	}
	o.ID, o.State, o.Revision, o.CreatedAt, o.Actor, o.UserID = randomID(), "created", 1, time.Now().UTC().Format(time.RFC3339Nano), actor, user
	raw, _ = json.Marshal(o)
	nonce = randomID()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return o, "", false, err
	}
	defer tx.Rollback()
	// First-install admission takes this same project row lock before reserving
	// its host. Recheck inside this transaction, not only during UI planning.
	if _, err = tx.ExecContext(ctx, r.Bind("update project set name=name where id=?"), project); err != nil {
		return o, "", false, err
	}
	// CAS against binding and package changes between plan validation and commit.
	var revision, enabled int
	if !o.ServiceControl() {
		if err = tx.QueryRowContext(ctx, r.Bind("select revision,enabled from rcd_package where project_id=? and id=?"), project, req.PackageID).Scan(&revision, &enabled); err != nil {
			return o, "", false, err
		}
		if revision != req.PackageRevision || enabled != 1 {
			return o, "", false, ErrConflict
		}
	}
	for _, t := range req.Targets {
		var address string
		for _, fixed := range o.Targets {
			if fixed.DeviceID == t.DeviceID {
				address = fixed.Address
				break
			}
		}
		if address == "" {
			return o, "", false, ErrInvalid
		}
		if _, err = r.ActiveHostSetupTx(ctx, tx, project, address); err == nil {
			return o, "", false, ErrConflict
		} else if !errors.Is(err, ErrNotFound) {
			return o, "", false, err
		}
		if err = tx.QueryRowContext(ctx, r.Bind("select revision from rcd_device where project_id=? and id=?"), project, t.DeviceID).Scan(&revision); err != nil {
			return o, "", false, err
		}
		if revision != t.Revision {
			return o, "", false, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, r.Bind("insert into rcd_operation_lock(project_id,device_id,operation_id) values(?,?,?)"), project, t.DeviceID, o.ID); err != nil {
			return o, "", false, ErrConflict
		}
	}
	_, err = tx.ExecContext(ctx, r.Bind("insert into rcd_operation(project_id,id,request_key,request_hash,nonce,body,state,revision,task_id,created_at) values(?,?,?,?,?,?,?,1,0,?)"), project, o.ID, key, hash, nonce, string(raw), o.State, o.CreatedAt)
	if err != nil {
		return o, "", false, err
	}
	err = tx.Commit()
	return o, nonce, err == nil, err
}

func (r *Repository) AuthorizeSubmission(ctx context.Context, project int, id, nonce string) (Operation, error) {
	o, err := r.Operation(ctx, project, id)
	if err != nil {
		return o, err
	}
	var stored string
	err = r.DB.QueryRowContext(ctx, r.Bind("select nonce from rcd_operation where project_id=? and id=?"), project, id).Scan(&stored)
	if err != nil {
		return o, err
	}
	if nonce == "" || nonce != stored || o.State != "created" || o.TaskID != 0 {
		return o, ErrConflict
	}
	return o, nil
}
func (r *Repository) AttachTask(ctx context.Context, project int, id string, task int) error {
	res, err := r.DB.ExecContext(ctx, r.Bind("update rcd_operation set task_id=?,state='queued',revision=revision+1,nonce='' where project_id=? and id=? and task_id=0 and state='created'"), task, project, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// A typed native pre-admission rejection proves that execution never started.
// A missing task ID alone is not that proof (e.g. lost AttachTask commit).
func (r *Repository) RecordSubmissionFailure(ctx context.Context, project int, id string, message string, rejectedBeforeAdmission bool) error {
	o, err := r.Operation(ctx, project, id)
	if err != nil || o.KnownTerminal() {
		return err
	}
	if rejectedBeforeAdmission && o.TaskID == 0 && o.State == "created" {
		o.State = "failed_before_execution"
		o.Error = "任务准入校验失败，未创建执行任务、未连接目标机；修正后可创建新操作"
		if len(message) > 4096 {
			message = message[:4096]
		}
		for i := range o.Targets {
			o.Targets[i].State, o.Targets[i].Error = "failed_before_execution", message
		}
	} else {
		o.State = "unknown"
		o.Error = "任务派发结果待确认；请检查原任务，不会自动重新执行"
	}
	return r.UpdateOperation(ctx, project, o, o.Revision)
}
func (r *Repository) UpdateOperation(ctx context.Context, project int, o Operation, expected int) error {
	raw, _ := json.Marshal(o)
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, r.Bind("update rcd_operation set body=?,state=?,revision=revision+1 where project_id=? and id=? and revision=?"), string(raw), o.State, project, o.ID, expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if o.KnownTerminal() {
		if _, err = tx.ExecContext(ctx, r.Bind("delete from rcd_operation_lock where project_id=? and operation_id=?"), project, o.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
