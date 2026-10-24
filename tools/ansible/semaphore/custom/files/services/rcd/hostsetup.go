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
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

var setupAccount = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,31}$`)
var setupRequestKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

func ValidateFirstInstallPackage(m Manifest) error {
	if m.Validate() != nil || !versionAtLeast("1.1.0", m.Compatibility.AdapterMinVersion) || len(m.Compatibility.ComponentMinVersions) > 0 {
		return fmt.Errorf("%w: 首装包的最低适配器版本或组件依赖尚未满足", ErrInvalid)
	}
	return nil
}

// HostSetupRequest references the project's native encrypted key store. It
// never contains passwords, private keys, caller-selected paths or commands.
type HostSetupRequest struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	SSHPort        int    `json:"ssh_port"`
	SSHUser        string `json:"ssh_user"`
	SSHKeyID       int    `json:"ssh_key_id"`
	BecomeKeyID    int    `json:"become_key_id"`
	ServiceAccount string `json:"service_account"`
}

func (r HostSetupRequest) Validate() error {
	if strings.TrimSpace(r.Name) != r.Name || len(r.Name) < 1 || len(r.Name) > 128 || net.ParseIP(r.Address) == nil || r.SSHPort < 1 || r.SSHPort > 65535 || !setupAccount.MatchString(r.SSHUser) || !setupAccount.MatchString(r.ServiceAccount) || r.ServiceAccount == "root" || r.SSHKeyID < 1 || r.BecomeKeyID < 0 {
		return ErrInvalid
	}
	for _, c := range r.Name {
		if unicode.IsControl(c) {
			return ErrInvalid
		}
	}
	ip := net.ParseIP(r.Address)
	if ip.IsUnspecified() || ip.IsMulticast() {
		return ErrInvalid
	}
	return nil
}
func (r HostSetupRequest) CanInstall() bool { return r.SSHUser == "root" || r.BecomeKeyID > 0 }

type HostKey struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"-"`
}
type HostSetupStep struct {
	Phase  string `json:"phase"`
	State  string `json:"state"`
	TaskID int    `json:"task_id"`
	Error  string `json:"error"`
}
type HostSetupInstall struct {
	PackageID       string       `json:"package_id"`
	PackageRevision int          `json:"package_revision"`
	RequestKey      string       `json:"-"`
	AdmissionID     string       `json:"-"`
	Component       string       `json:"component"`
	ArchiveSHA256   string       `json:"archive_sha256"`
	BinarySHA256    string       `json:"binary_sha256"`
	Application     *Application `json:"application,omitempty"`
}

type HostSetup struct {
	ID                    string            `json:"id"`
	Revision              int               `json:"revision"`
	State                 string            `json:"state"`
	Phase                 string            `json:"phase"`
	TaskID                int               `json:"task_id"`
	OriginalInstallTaskID int               `json:"original_install_task_id,omitempty"`
	ReconcileRevision     int               `json:"-"`
	QueryNotQueued        bool              `json:"-"`
	DeviceID              string            `json:"device_id"`
	ExistingDevice        bool              `json:"existing_device"`
	DeviceRevision        int               `json:"-"`
	Error                 string            `json:"error"`
	Warnings              []string          `json:"warnings"`
	Request               HostSetupRequest  `json:"request"`
	HostKeys              []HostKey         `json:"host_keys"`
	Steps                 []HostSetupStep   `json:"steps"`
	CreatedAt             string            `json:"created_at"`
	UpdatedAt             string            `json:"updated_at"`
	Actor                 string            `json:"actor"`
	UserID                int               `json:"-"`
	Nonce                 string            `json:"-"`
	InventoryID           int               `json:"-"`
	TemplateID            int               `json:"-"`
	InspectionTemplateID  int               `json:"-"`
	ReleaseTemplateID     int               `json:"-"`
	ControlTemplateID     int               `json:"-"`
	EnvironmentID         int               `json:"-"`
	RepositoryID          int               `json:"-"`
	RequestKey            string            `json:"-"`
	RequestHash           string            `json:"-"`
	HostKeysConfirmed     bool              `json:"host_keys_confirmed"`
	Install               *HostSetupInstall `json:"install,omitempty"`
	Platform              string            `json:"platform,omitempty"`
}

// Persistence deliberately includes internal admission identities but API JSON
// does not. Never serialize DBBody() directly into a HTTP response.
type persistedHostSetup struct {
	HostSetup
	UserID               int                `json:"user_id"`
	Nonce                string             `json:"nonce"`
	InventoryID          int                `json:"inventory_id"`
	TemplateID           int                `json:"template_id"`
	InspectionTemplateID int                `json:"inspection_template_id"`
	ReleaseTemplateID    int                `json:"release_template_id"`
	ControlTemplateID    int                `json:"control_template_id"`
	EnvironmentID        int                `json:"environment_id"`
	RepositoryID         int                `json:"repository_id"`
	RequestKey           string             `json:"request_key"`
	RequestHash          string             `json:"request_hash"`
	Keys                 []persistedHostKey `json:"keys"`
	InstallKey           string             `json:"install_key,omitempty"`
	InstallAdmissionID   string             `json:"install_admission_id,omitempty"`
	DeviceRevision       int                `json:"device_revision"`
	ReconcileRevision    int                `json:"reconcile_revision,omitempty"`
	QueryNotQueued       bool               `json:"query_not_queued,omitempty"`
}
type persistedHostKey struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

func (s HostSetup) dbBody() []byte {
	p := persistedHostSetup{HostSetup: s, UserID: s.UserID, Nonce: s.Nonce, InventoryID: s.InventoryID, TemplateID: s.TemplateID, InspectionTemplateID: s.InspectionTemplateID, ReleaseTemplateID: s.ReleaseTemplateID, ControlTemplateID: s.ControlTemplateID, EnvironmentID: s.EnvironmentID, RepositoryID: s.RepositoryID, RequestKey: s.RequestKey, RequestHash: s.RequestHash, DeviceRevision: s.DeviceRevision}
	p.ReconcileRevision = s.ReconcileRevision
	p.QueryNotQueued = s.QueryNotQueued
	for _, k := range s.HostKeys {
		p.Keys = append(p.Keys, persistedHostKey{k.Algorithm, k.Fingerprint, k.PublicKey})
	}
	if s.Install != nil {
		p.InstallKey = s.Install.RequestKey
		p.InstallAdmissionID = s.Install.AdmissionID
	}
	raw, _ := json.Marshal(p)
	return raw
}
func scanHostSetup(row scanner) (HostSetup, error) {
	var raw string
	if e := row.Scan(&raw); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return HostSetup{}, ErrNotFound
		}
		return HostSetup{}, e
	}
	var p persistedHostSetup
	if e := json.Unmarshal([]byte(raw), &p); e != nil {
		return HostSetup{}, e
	}
	s := p.HostSetup
	s.UserID = p.UserID
	s.Nonce = p.Nonce
	s.InventoryID = p.InventoryID
	s.TemplateID = p.TemplateID
	s.InspectionTemplateID = p.InspectionTemplateID
	s.ReleaseTemplateID = p.ReleaseTemplateID
	s.ControlTemplateID = p.ControlTemplateID
	s.EnvironmentID = p.EnvironmentID
	s.RepositoryID = p.RepositoryID
	s.RequestKey = p.RequestKey
	s.RequestHash = p.RequestHash
	s.HostKeys = []HostKey{}
	for _, k := range p.Keys {
		s.HostKeys = append(s.HostKeys, HostKey{k.Algorithm, k.Fingerprint, k.PublicKey})
	}
	if s.Install != nil {
		s.Install.RequestKey = p.InstallKey
		s.Install.AdmissionID = p.InstallAdmissionID
	}
	s.DeviceRevision = p.DeviceRevision
	s.ReconcileRevision = p.ReconcileRevision
	s.QueryNotQueued = p.QueryNotQueued
	return s, nil
}
func (s HostSetup) ETag() string      { return fmt.Sprintf(`"%d"`, s.Revision) }
func (s HostSetup) HostAlias() string { return "rcd" + s.ID[:12] }
func (s HostSetup) Authorize(nonce string, user int) bool {
	return nonce != "" && nonce == s.Nonce && user == s.UserID && s.TaskID == 0 && strings.HasSuffix(s.State, "_queued")
}
func (s HostSetup) CanSubmit() bool { return s.TaskID == 0 && strings.HasSuffix(s.State, "_queued") }
func (s *HostSetup) StartPhase(phase, state string) {
	s.Phase = phase
	s.State = state
	s.TaskID = 0
	s.QueryNotQueued = false
	s.Nonce = randomID()
	if phase == "install" && s.Install != nil {
		token := sha256.Sum256([]byte(s.Nonce))
		s.Install.AdmissionID = hex.EncodeToString(token[:])
	}
	s.Error = ""
	s.Steps = append(s.Steps, HostSetupStep{Phase: phase, State: state})
}
func (s *HostSetup) SetResult(state, message string) {
	s.State = state
	s.Error = message
	if n := len(s.Steps); n > 0 {
		s.Steps[n-1].State = state
		s.Steps[n-1].Error = message
	}
}

func (r *Repository) HostSetup(ctx context.Context, project int, id string) (HostSetup, error) {
	return scanHostSetup(r.DB.QueryRowContext(ctx, r.Bind("select body from rcd_host_setup where project_id=? and id=?"), project, id))
}
func (r *Repository) HostSetupByRequest(ctx context.Context, project int, key string) (HostSetup, error) {
	return scanHostSetup(r.DB.QueryRowContext(ctx, r.Bind("select body from rcd_host_setup where project_id=? and request_key=?"), project, key))
}
func (r *Repository) ActiveHostSetup(ctx context.Context, project int, address string) (HostSetup, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return HostSetup{}, ErrInvalid
	}
	return scanHostSetup(r.DB.QueryRowContext(ctx, r.Bind("select h.body from rcd_host_setup h join rcd_host_setup_active a on a.project_id=h.project_id and a.setup_id=h.id where a.project_id=? and a.address=?"), project, ip.String()))
}
func (r *Repository) ActiveHostSetupTx(ctx context.Context, tx *sql.Tx, project int, address string) (HostSetup, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return HostSetup{}, ErrInvalid
	}
	return scanHostSetup(tx.QueryRowContext(ctx, r.Bind("select h.body from rcd_host_setup h join rcd_host_setup_active a on a.project_id=h.project_id and a.setup_id=h.id where a.project_id=? and a.address=?"), project, ip.String()))
}
func (r *Repository) HostSetupForTask(ctx context.Context, project, task int) (HostSetup, error) {
	return scanHostSetup(r.DB.QueryRowContext(ctx, r.Bind("select body from rcd_host_setup where project_id=? and task_id=?"), project, task))
}
func (r *Repository) HostSetupForTemplate(ctx context.Context, project, template int) (HostSetup, error) {
	return scanHostSetup(r.DB.QueryRowContext(ctx, r.Bind("select body from rcd_host_setup where project_id=? and template_id=?"), project, template))
}
func (r *Repository) HostSetups(ctx context.Context, project int) ([]HostSetup, error) {
	rows, e := r.DB.QueryContext(ctx, r.Bind("select body from rcd_host_setup where project_id=? order by created_at desc,id desc"), project)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []HostSetup{}
	for rows.Next() {
		s, e := scanHostSetup(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, s)
	}
	return result, rows.Err()
}
func (r *Repository) CreateHostSetup(ctx context.Context, project int, req HostSetupRequest, key, actor string, user int) (HostSetup, bool, error) {
	if req.Validate() != nil || !setupRequestKey.MatchString(key) || project < 1 || actor == "" || user < 1 {
		return HostSetup{}, false, ErrInvalid
	}
	req.Address = net.ParseIP(req.Address).String()
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	existing, e := scanHostSetup(r.DB.QueryRowContext(ctx, r.Bind("select body from rcd_host_setup where project_id=? and request_key=?"), project, key))
	if e == nil {
		if existing.RequestHash != hash {
			return existing, false, ErrConflict
		}
		return existing, false, nil
	}
	if !errors.Is(e, ErrNotFound) {
		return HostSetup{}, false, e
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	s := HostSetup{ID: randomID(), Revision: 1, Request: req, RequestKey: key, RequestHash: hash, Actor: actor, UserID: user, CreatedAt: stamp, UpdatedAt: stamp, HostKeys: []HostKey{}, Steps: []HostSetupStep{}}
	s.StartPhase("keyscan", "keyscan_queued")
	e = r.insertHostSetup(ctx, project, s)
	if e != nil {
		other, readErr := r.HostSetupByRequest(ctx, project, key)
		if readErr == nil {
			if other.RequestHash == hash {
				return other, false, nil
			}
			return other, false, ErrConflict
		}
		if _, readErr = r.ActiveHostSetup(ctx, project, req.Address); readErr == nil {
			return s, false, ErrConflict
		}
	}
	return s, e == nil, e
}
func (r *Repository) insertHostSetup(ctx context.Context, project int, s HostSetup) error {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = r.hostSetupProjectAdmission(ctx, tx, project, s.Request.Address); e != nil {
		return e
	}
	if s.ExistingDevice {
		locked, e := tx.ExecContext(ctx, r.Bind("update rcd_device set revision=revision where project_id=? and id=? and revision=?"), project, s.DeviceID, s.DeviceRevision)
		if e != nil {
			return e
		}
		n, e := locked.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		var raw string
		if e = tx.QueryRowContext(ctx, r.Bind("select binding from rcd_device where project_id=? and id=?"), project, s.DeviceID).Scan(&raw); e != nil {
			return e
		}
		var binding DeviceBinding
		if json.Unmarshal([]byte(raw), &binding) != nil || !net.ParseIP(s.Request.Address).Equal(net.ParseIP(binding.Address)) {
			return ErrConflict
		}
	}
	_, e = tx.ExecContext(ctx, r.Bind("insert into rcd_host_setup_active(project_id,address,ssh_port,setup_id) values(?,?,?,?)"), project, s.Request.Address, s.Request.SSHPort, s.ID)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, r.Bind("insert into rcd_host_setup(project_id,id,request_key,request_hash,address,ssh_port,body,revision,task_id,template_id,state,created_at) values(?,?,?,?,?,?,?,1,0,0,?,?)"), project, s.ID, s.RequestKey, s.RequestHash, s.Request.Address, s.Request.SSHPort, string(s.dbBody()), s.State, s.CreatedAt)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (r *Repository) hostSetupProjectAdmission(ctx context.Context, tx *sql.Tx, project int, address string) error {
	if _, e := tx.ExecContext(ctx, r.Bind("update project set name=name where id=?"), project); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, r.Bind("select d.binding from rcd_operation_lock l join rcd_device d on d.project_id=l.project_id and d.id=l.device_id where l.project_id=?"), project)
	if e != nil {
		return e
	}
	defer rows.Close()
	target := net.ParseIP(address)
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return e
		}
		var b DeviceBinding
		if json.Unmarshal([]byte(raw), &b) != nil {
			return ErrInvalid
		}
		if ip := net.ParseIP(b.Address); ip != nil && target.Equal(ip) {
			return ErrConflict
		}
	}
	return rows.Err()
}
func (r *Repository) CreateExistingFirstInstall(ctx context.Context, project int, device Device, req HostSetupRequest, install HostSetupInstall, key, actor string, user int, platform string) (HostSetup, bool, error) {
	if req.Validate() != nil || !setupRequestKey.MatchString(key) || device.ID == "" || device.ActiveOperationID != "" || user < 1 || actor == "" {
		return HostSetup{}, false, ErrInvalid
	}
	req.Address = net.ParseIP(req.Address).String()
	raw, _ := json.Marshal(struct {
		Request  HostSetupRequest
		DeviceID string
		Install  HostSetupInstall
	}{req, device.ID, install})
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	old, e := r.HostSetupByRequest(ctx, project, key)
	if e == nil {
		if old.RequestHash != hash {
			return old, false, ErrConflict
		}
		return old, false, nil
	}
	if !errors.Is(e, ErrNotFound) {
		return old, false, e
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	s := HostSetup{ID: randomID(), Revision: 1, Request: req, RequestKey: key, RequestHash: hash, DeviceID: device.ID, DeviceRevision: device.Revision, ExistingDevice: true, HostKeysConfirmed: true, Install: &install, Actor: actor, UserID: user, CreatedAt: stamp, UpdatedAt: stamp, Platform: platform, HostKeys: []HostKey{}, Steps: []HostSetupStep{}}
	s.StartPhase("install", "installation_queued")
	e = r.insertHostSetup(ctx, project, s)
	if e != nil {
		other, check := r.HostSetupByRequest(ctx, project, key)
		if check == nil && other.RequestHash == hash {
			return other, false, nil
		}
		if _, check = r.ActiveHostSetup(ctx, project, req.Address); check == nil {
			return s, false, ErrConflict
		}
	}
	return s, e == nil, e
}
func (r *Repository) UpdateHostSetup(ctx context.Context, project int, s HostSetup, revision int) (HostSetup, error) {
	if s.Revision != revision || s.ID == "" {
		return s, ErrConflict
	}
	s.Revision++
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return s, e
	}
	defer tx.Rollback()
	if strings.HasSuffix(s.State, "_queued") || s.State == "running" {
		if e = r.hostSetupProjectAdmission(ctx, tx, project, s.Request.Address); e != nil {
			return s, e
		}
	}
	res, e := tx.ExecContext(ctx, r.Bind("update rcd_host_setup set body=?,revision=?,task_id=?,template_id=?,state=? where project_id=? and id=? and revision=?"), string(s.dbBody()), s.Revision, s.TaskID, s.TemplateID, s.State, project, s.ID, revision)
	if e != nil {
		return s, e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return s, e
	}
	if n != 1 {
		return s, ErrConflict
	}
	if s.State == "installed_stopped" || s.State == "failed" || s.State == "host_key_conflict" || s.State == "cancelled" {
		if _, e = tx.ExecContext(ctx, r.Bind("delete from rcd_host_setup_active where project_id=? and setup_id=?"), project, s.ID); e != nil {
			return s, e
		}
	}
	return s, tx.Commit()
}
func (r *Repository) AttachHostSetupTask(ctx context.Context, project int, id string, task int) error {
	s, e := r.HostSetup(ctx, project, id)
	if e != nil {
		return e
	}
	if s.TaskID == task {
		return nil
	}
	if !s.CanSubmit() || task < 1 {
		return ErrConflict
	}
	s.TaskID = task
	s.QueryNotQueued = false
	if s.Phase == "install" {
		s.OriginalInstallTaskID = task
	}
	if n := len(s.Steps); n > 0 {
		s.Steps[n-1].TaskID = task
	}
	_, e = r.UpdateHostSetup(ctx, project, s, s.Revision)
	return e
}

// RecoverHostSetupTask only attaches a proven existing native task. It never
// creates/requeues a task or turns an unknown execution into a new attempt.
func (r *Repository) RecoverHostSetupTask(ctx context.Context, project int, s HostSetup, task int) (HostSetup, error) {
	if task < 1 || s.TaskID != 0 || (s.State != "unknown" && !strings.HasSuffix(s.State, "_queued")) || len(s.Steps) == 0 || s.Steps[len(s.Steps)-1].Phase != s.Phase {
		return s, ErrConflict
	}
	s.TaskID = task
	s.QueryNotQueued = false
	s.Steps[len(s.Steps)-1].TaskID = task
	if s.Phase == "install" {
		s.OriginalInstallTaskID = task
	}
	return r.UpdateHostSetup(ctx, project, s, s.Revision)
}

func (s HostSetup) InstallationTaskID() int {
	if s.OriginalInstallTaskID > 0 {
		return s.OriginalInstallTaskID
	}
	for i := len(s.Steps) - 1; i >= 0; i-- {
		if s.Steps[i].Phase == "install" && s.Steps[i].TaskID > 0 {
			return s.Steps[i].TaskID
		}
	}
	if s.Phase == "install" {
		return s.TaskID
	}
	return 0
}
func (r *Repository) ConfirmHostSetupKeys(ctx context.Context, project int, s HostSetup, fingerprints []string) (HostSetup, error) {
	if s.State != "awaiting_host_key" || len(s.HostKeys) == 0 || len(fingerprints) != len(s.HostKeys) {
		return s, ErrConflict
	}
	expected := []string{}
	for _, k := range s.HostKeys {
		expected = append(expected, k.Fingerprint)
	}
	actual := append([]string{}, fingerprints...)
	sort.Strings(expected)
	sort.Strings(actual)
	for i := range expected {
		if expected[i] != actual[i] {
			return s, ErrConflict
		}
	}
	s.HostKeysConfirmed = true
	s.StartPhase("probe", "ssh_probe_queued")
	return r.UpdateHostSetup(ctx, project, s, s.Revision)
}
