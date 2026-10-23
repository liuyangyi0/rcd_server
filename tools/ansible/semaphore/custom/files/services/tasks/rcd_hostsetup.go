// SPDX-License-Identifier: MIT
package tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/util"
)

const hostSetupPlaybook = "tools/ansible/host-setup-platform.yml"

var hostSetupSubmission sync.Mutex

func HostSetupSource(store db.Store, project int) (db.Template, error) {
	templates, e := store.GetTemplates(project, db.TemplateFilter{}, db.RetrieveQueryParams{})
	if e != nil {
		return db.Template{}, e
	}
	for _, t := range templates {
		if t.Playbook != "tools/ansible/release-platform.yml" || t.InventoryID == nil {
			continue
		}
		if ValidateRCDReleaseTemplate(store, project, t.ID, *t.InventoryID) != nil {
			continue
		}
		repository, err := store.GetRepository(project, t.RepositoryID)
		if err == nil && filepath.IsAbs(repository.GitURL) && filepath.Clean(repository.GitURL) == repository.GitURL {
			return t, nil
		}
	}
	return db.Template{}, fmt.Errorf("项目未配置受控的本地 RCD 发布代码仓库；请先由管理员完成高级管理绑定")
}

func SetupInventory(s rcd.HostSetup) string {
	host := map[string]any{"ansible_host": s.Request.Address, "ansible_port": s.Request.SSHPort, "ansible_user": s.Request.SSHUser,
		"ansible_connection": "paramiko", "ansible_python_interpreter": "/usr/bin/python3", "ansible_host_key_checking": true,
		"ansible_paramiko_host_key_checking": true, "ansible_paramiko_host_key_auto_add": false, "ansible_paramiko_look_for_keys": false,
		"rcd_inspection_become": s.Request.BecomeKeyID > 0, "rcd_device_owner": s.Request.ServiceAccount,
		"rcd_release_profiles": map[string]any{}, "rcd_release_roots": []string{"/var/lib/rcd-release-platform/artifacts/lower", "/var/lib/rcd-release-platform/artifacts/manager", "/var/lib/rcd-release-platform/artifacts/agent", "/var/lib/rcd-release-platform/artifacts/communicate"},
		"rcd_services": []map[string]any{
			{"component": "manager", "instance": "main", "scope": "user", "unit": "rcd-manager.service", "run_as": s.Request.ServiceAccount},
			{"component": "lower-stack", "instance": "main", "scope": "user", "unit": "rcd-process-agent@main.service", "run_as": s.Request.ServiceAccount}}}
	raw, _ := json.Marshal(map[string]any{"all": map[string]any{"children": map[string]any{"rcd_nodes": map[string]any{"hosts": map[string]any{s.HostAlias(): host}}}}})
	return string(raw)
}

// Existing machine onboarding reuses only fixed host properties and native key
// references. No caller can turn a missing application into an arbitrary SSH
// execution target. Current platform inventories are canonical JSON, which is
// valid static YAML; unsupported handwritten layouts fail closed.
func RCDExistingHostSetupRequest(store db.Store, project int, d rcd.Device) (rcd.HostSetupRequest, error) {
	inv, e := store.GetInventory(project, d.InventoryID)
	if e != nil {
		return rcd.HostSetupRequest{}, e
	}
	if inv.Type != db.InventoryStaticYaml || inv.RunnerTag != nil || inv.SSHKeyID == nil {
		return rcd.HostSetupRequest{}, fmt.Errorf("首装要求受控本地 static-yaml 库存及已有加密 SSH 凭据")
	}
	var data struct {
		All struct {
			Children struct {
				Nodes struct {
					Hosts map[string]json.RawMessage `json:"hosts"`
				} `json:"rcd_nodes"`
			} `json:"children"`
		} `json:"all"`
	}
	if json.Unmarshal([]byte(inv.Inventory), &data) != nil {
		return rcd.HostSetupRequest{}, fmt.Errorf("现有库存格式不受控；请管理员先转换为固定 JSON static-yaml 主机绑定，不会尝试猜测目标")
	}
	var host struct {
		Address  string `json:"ansible_host"`
		User     string `json:"ansible_user"`
		Port     int    `json:"ansible_port"`
		Owner    string `json:"rcd_device_owner"`
		Services []struct {
			Component string `json:"component"`
			RunAs     string `json:"run_as"`
		} `json:"rcd_services"`
	}
	raw, ok := data.All.Children.Nodes.Hosts[d.HostAlias]
	if !ok || json.Unmarshal(raw, &host) != nil || net.ParseIP(host.Address) == nil || !net.ParseIP(host.Address).Equal(net.ParseIP(d.Address)) {
		return rcd.HostSetupRequest{}, rcd.ErrConflict
	}
	if host.Port == 0 {
		host.Port = 22
	}
	owner := host.Owner
	for _, service := range host.Services {
		if service.Component == "manager" && service.RunAs != "" {
			if owner != "" && owner != service.RunAs {
				return rcd.HostSetupRequest{}, rcd.ErrConflict
			}
			owner = service.RunAs
		}
	}
	become := 0
	if inv.BecomeKeyID != nil {
		become = *inv.BecomeKeyID
	}
	req := rcd.HostSetupRequest{Name: d.Name, Address: d.Address, SSHPort: host.Port, SSHUser: host.User, SSHKeyID: *inv.SSHKeyID, BecomeKeyID: become, ServiceAccount: owner}
	if req.Validate() != nil {
		return req, fmt.Errorf("库存缺少唯一的非 root 服务账户、明确 SSH 用户或地址；请先核实设备绑定")
	}
	return req, nil
}

// Resource IDs are persisted after each native CRUD. A partial provisioning
// failure is visible on the same setup; it is never an excuse to duplicate SSH
// operations or claim that a host already has software installed.
func EnsureRCDHostSetupResources(store db.Store, repo *rcd.Repository, project int, s rcd.HostSetup) (rcd.HostSetup, error) {
	source, e := HostSetupSource(store, project)
	if e != nil {
		return s, e
	}
	save := func() error {
		var err error
		s, err = repo.UpdateHostSetup(context.Background(), project, s, s.Revision)
		return err
	}
	s.RepositoryID = source.RepositoryID
	if s.EnvironmentID == 0 {
		name := "RCD 接入环境 · " + s.ID
		envs, err := store.GetEnvironments(project, db.RetrieveQueryParams{})
		if err != nil {
			return s, err
		}
		envvars := `{"ANSIBLE_HOST_KEY_CHECKING":"True","ANSIBLE_PARAMIKO_HOST_KEY_CHECKING":"True","ANSIBLE_PARAMIKO_LOOK_FOR_KEYS":"False","ANSIBLE_PARAMIKO_HOST_KEY_AUTO_ADD":"False"}`
		var found *db.Environment
		for i := range envs {
			if envs[i].Name == name {
				if found != nil {
					return s, rcd.ErrConflict
				}
				copy := envs[i]
				found = &copy
			}
		}
		if found != nil {
			if found.JSON != "{}" || found.ENV == nil || *found.ENV != envvars {
				return s, rcd.ErrConflict
			}
			s.EnvironmentID = found.ID
		} else {
			env, err := store.CreateEnvironment(db.Environment{ProjectID: project, Name: name, JSON: "{}", ENV: &envvars})
			if err != nil {
				return s, err
			}
			s.EnvironmentID = env.ID
		}
		if e = save(); e != nil {
			return s, e
		}
	}
	if s.InventoryID == 0 {
		key := s.Request.SSHKeyID
		var become *int
		if s.Request.BecomeKeyID > 0 {
			value := s.Request.BecomeKeyID
			become = &value
		}
		name := "RCD 接入主机 · " + s.ID
		list, err := store.GetInventories(project, db.RetrieveQueryParams{}, []db.InventoryType{db.InventoryStaticYaml})
		if err != nil {
			return s, err
		}
		var found *db.Inventory
		for i := range list {
			if list[i].Name == name {
				if found != nil {
					return s, rcd.ErrConflict
				}
				copy := list[i]
				found = &copy
			}
		}
		wanted := db.Inventory{ProjectID: project, Name: name, Inventory: SetupInventory(s), Type: db.InventoryStaticYaml, SSHKeyID: &key, BecomeKeyID: become}
		if found != nil {
			if found.Inventory != wanted.Inventory || found.SSHKeyID == nil || *found.SSHKeyID != key || ((found.BecomeKeyID == nil) != (become == nil)) || (become != nil && *found.BecomeKeyID != *become) {
				return s, rcd.ErrConflict
			}
			s.InventoryID = found.ID
		} else {
			inv, err := store.CreateInventory(wanted)
			if err != nil {
				return s, err
			}
			s.InventoryID = inv.ID
		}
		if e = save(); e != nil {
			return s, e
		}
	}
	createTemplate := func(name, playbook string, limit bool) (int, error) {
		list, err := store.GetTemplates(project, db.TemplateFilter{}, db.RetrieveQueryParams{})
		if err != nil {
			return 0, err
		}
		var found *db.Template
		for i := range list {
			if list[i].Name == name {
				if found != nil {
					return 0, rcd.ErrConflict
				}
				copy := list[i]
				found = &copy
			}
		}
		params := db.MapStringAnyField{"skip_galaxy_install": true}
		if limit {
			params["limit"] = []string{s.HostAlias()}
		}
		if found != nil {
			if found.Playbook != playbook || found.InventoryID == nil || *found.InventoryID != s.InventoryID || found.RepositoryID != s.RepositoryID {
				return 0, rcd.ErrConflict
			}
			return found.ID, nil
		}
		inv := s.InventoryID
		t, err := store.CreateTemplate(db.Template{ProjectID: project, Name: name, App: db.AppAnsible, Type: db.TemplateTask, InventoryID: &inv, RepositoryID: s.RepositoryID, EnvironmentID: s.EnvironmentID, EnvironmentIDs: []int{s.EnvironmentID}, Playbook: playbook, TaskParams: params})
		return t.ID, err
	}
	if s.TemplateID == 0 {
		s.TemplateID, e = createTemplate("RCD 接入任务 · "+s.ID, hostSetupPlaybook, false)
		if e != nil {
			return s, e
		}
		if e = save(); e != nil {
			return s, e
		}
	}
	if !s.ExistingDevice && s.InspectionTemplateID == 0 {
		s.InspectionTemplateID, e = createTemplate("RCD 状态检查 · "+s.ID, "tools/ansible/inspect-platform.yml", true)
		if e != nil {
			return s, e
		}
		if e = save(); e != nil {
			return s, e
		}
	}
	if !s.ExistingDevice && s.ReleaseTemplateID == 0 {
		s.ReleaseTemplateID, e = createTemplate("RCD 软件发布 · "+s.ID, "tools/ansible/release-platform.yml", false)
		if e != nil {
			return s, e
		}
		if e = save(); e != nil {
			return s, e
		}
	}
	if !s.ExistingDevice && s.ControlTemplateID == 0 {
		s.ControlTemplateID, e = createTemplate("RCD 受控服务启停 · "+s.ID, "tools/ansible/control-platform.yml", false)
		if e != nil {
			return s, e
		}
		if e = save(); e != nil {
			return s, e
		}
	}
	return s, ValidateRCDHostSetupTemplate(store, project, s)
}

func ValidateRCDHostSetupTemplate(store db.Store, project int, s rcd.HostSetup) error {
	t, e := store.GetTemplate(project, s.TemplateID)
	if e != nil {
		return e
	}
	if util.Config.UseRemoteRunner || t.RunnerTag != nil || t.Playbook != hostSetupPlaybook || t.App != db.AppAnsible || t.Type != db.TemplateTask || t.InventoryID == nil || *t.InventoryID != s.InventoryID || t.RepositoryID != s.RepositoryID || t.AllowOverrideArgsInTask || t.AllowOverrideBranchInTask {
		return rcd.ErrInvalid
	}
	if t.Arguments != nil && *t.Arguments != "" && *t.Arguments != "[]" {
		return rcd.ErrInvalid
	}
	var params db.AnsibleTemplateParams
	if t.FillParams(&params) != nil || len(params.Limit) > 0 || len(params.Tags) > 0 || len(params.SkipTags) > 0 || params.AllowOverrideInventory || params.AllowOverrideLimit || params.AllowOverrideTags || params.AllowOverrideSkipTags {
		return rcd.ErrInvalid
	}
	inv, e := store.GetInventory(project, s.InventoryID)
	if e != nil {
		return e
	}
	if inv.RunnerTag != nil || inv.Inventory != SetupInventory(s) || inv.Type != db.InventoryStaticYaml || inv.SSHKeyID == nil || *inv.SSHKeyID != s.Request.SSHKeyID {
		return rcd.ErrConflict
	}
	if s.Request.BecomeKeyID == 0 {
		if inv.BecomeKeyID != nil {
			return rcd.ErrConflict
		}
	} else if inv.BecomeKeyID == nil || *inv.BecomeKeyID != s.Request.BecomeKeyID {
		return rcd.ErrConflict
	}
	if len(t.EnvironmentIDs) != 1 || t.EnvironmentIDs[0] != s.EnvironmentID {
		return rcd.ErrConflict
	}
	env, e := store.GetEnvironment(project, s.EnvironmentID)
	if e != nil {
		return e
	}
	expectedEnvironment := `{"ANSIBLE_HOST_KEY_CHECKING":"True","ANSIBLE_PARAMIKO_HOST_KEY_CHECKING":"True","ANSIBLE_PARAMIKO_LOOK_FOR_KEYS":"False","ANSIBLE_PARAMIKO_HOST_KEY_AUTO_ADD":"False"}`
	if env.JSON != "{}" || env.ENV == nil || *env.ENV != expectedEnvironment {
		return rcd.ErrConflict
	}
	return nil
}

func (p *TaskPool) prepareRCDHostSetup(task *db.Task, project int, user *int) (func(), error) {
	repo := rcdRepository(p.store)
	if repo == nil {
		return nil, nil
	}
	template, e := p.store.GetTemplate(project, task.TemplateID)
	if e != nil {
		return nil, e
	}
	if template.Playbook != hostSetupPlaybook {
		if task.Playbook == hostSetupPlaybook {
			return nil, rcd.ErrInvalid
		}
		return nil, nil
	}
	hostSetupSubmission.Lock()
	var input struct {
		ID    string `json:"rcd_host_setup_id"`
		Nonce string `json:"rcd_nonce"`
	}
	if json.Unmarshal([]byte(task.Secret), &input) != nil {
		return hostSetupSubmission.Unlock, rcd.ErrInvalid
	}
	s, e := repo.HostSetup(context.Background(), project, input.ID)
	if e != nil {
		return hostSetupSubmission.Unlock, e
	}
	if user == nil || !s.Authorize(input.Nonce, *user) || task.TemplateID != s.TemplateID {
		return hostSetupSubmission.Unlock, rcd.ErrConflict
	}
	if e = ValidateRCDHostSetupTemplate(p.store, project, s); e != nil {
		return hostSetupSubmission.Unlock, e
	}
	task.Playbook = ""
	task.Environment = "{}"
	task.Secret = "{}"
	task.Arguments = nil
	task.GitBranch = nil
	task.InventoryID = nil
	task.Limit = ""
	token := sha256.Sum256([]byte(s.Nonce))
	task.Params = db.MapStringAnyField{"rcd_host_setup_id": s.ID, "rcd_host_setup_phase": s.Phase, "rcd_host_setup_admission": hex.EncodeToString(token[:]), "skip_galaxy_install": true}
	task.Message = "RCD 主机接入 · " + s.Request.Name + " · " + s.Phase
	return hostSetupSubmission.Unlock, nil
}
func (p *TaskPool) attachRCDHostSetup(task db.Task) error {
	repo := rcdRepository(p.store)
	if repo == nil {
		return nil
	}
	id, _ := task.Params["rcd_host_setup_id"].(string)
	if id == "" {
		return nil
	}
	return repo.AttachHostSetupTask(context.Background(), task.ProjectID, id, task.ID)
}
func HostSetupReportPath(root string, project, task int) string {
	return filepath.Join(root, "host-setups", strconv.Itoa(project), strconv.Itoa(task), "report.json")
}

// RecoverRCDHostSetupTask repairs the crash window between native task insert
// and association. Only a unique existing task is attached; no queue write.
func RecoverRCDHostSetupTask(store db.Store, repo *rcd.Repository, project int, s rcd.HostSetup) (rcd.HostSetup, error) {
	if s.TaskID != 0 || s.TemplateID < 1 || (s.State != "unknown" && !strings.HasSuffix(s.State, "_queued")) {
		return s, nil
	}
	items, err := store.GetTemplateTasks(project, s.TemplateID, db.RetrieveQueryParams{Count: 100})
	if err != nil {
		return s, err
	}
	token := sha256.Sum256([]byte(s.Nonce))
	expected := hex.EncodeToString(token[:])
	prior := 0
	for _, step := range s.Steps {
		if step.TaskID > prior {
			prior = step.TaskID
		}
	}
	created, err := time.Parse(time.RFC3339Nano, s.CreatedAt)
	if err != nil {
		return s, rcd.ErrInvalid
	}
	matched := 0
	for _, item := range items {
		task := item.Task
		id, _ := task.Params["rcd_host_setup_id"].(string)
		phase, _ := task.Params["rcd_host_setup_phase"].(string)
		admission, _ := task.Params["rcd_host_setup_admission"].(string)
		if task.ProjectID != project || task.TemplateID != s.TemplateID || task.ID <= prior || id != s.ID || task.UserID == nil || *task.UserID != s.UserID || task.Created.Before(created) {
			continue
		}
		// Legacy tasks were still admitted through the setup-exclusive template;
		// never use a task from another known phase. New tasks require both tags.
		if (phase != "" || admission != "") && (phase != s.Phase || admission != expected) {
			continue
		}
		if matched != 0 {
			return s, rcd.ErrConflict
		}
		matched = task.ID
	}
	if matched == 0 {
		return s, nil
	}
	return repo.RecoverHostSetupTask(context.Background(), project, s, matched)
}
func rcdHostSetupExtraVars(store db.Store, task db.Task, vars map[string]any) error {
	delete(vars, "rcd_host_setup")
	repo := rcdRepository(store)
	if repo == nil {
		return nil
	}
	s, e := repo.HostSetupForTask(context.Background(), task.ProjectID, task.ID)
	if errors.Is(e, rcd.ErrNotFound) {
		return nil
	}
	if e != nil {
		return e
	}
	if !strings.HasSuffix(s.State, "_queued") {
		return fmt.Errorf("原主机接入步骤已执行或待确认，不自动重投")
	}
	if e = ValidateRCDHostSetupTemplate(store, task.ProjectID, s); e != nil {
		return e
	}
	if s.Phase != "keyscan" && !s.HostKeysConfirmed {
		return rcd.ErrConflict
	}
	if s.ExistingDevice {
		d, e := repo.Device(context.Background(), task.ProjectID, s.DeviceID)
		if e != nil {
			return e
		}
		if d.Revision != s.DeviceRevision || !net.ParseIP(s.Request.Address).Equal(net.ParseIP(d.Address)) || d.ActiveOperationID != "" {
			return rcd.ErrConflict
		}
	}
	root := os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH")
	report := HostSetupReportPath(root, task.ProjectID, task.ID)
	if e = os.MkdirAll(filepath.Dir(report), 0700); e != nil {
		return e
	}
	request := map[string]any{"schema_version": 1, "operation_id": s.ID, "task_id": task.ID, "project_id": task.ProjectID, "address": s.Request.Address, "ssh_port": s.Request.SSHPort, "service_account": s.Request.ServiceAccount}
	bind := map[string]any{"phase": s.Phase, "host_alias": s.HostAlias(), "report_path": report, "request": request, "become": s.Request.BecomeKeyID > 0}
	if s.Phase == "install" || s.Phase == "reconcile" {
		if !s.Request.CanInstall() {
			return rcd.ErrInvalid
		}
		if s.Install == nil {
			return rcd.ErrInvalid
		}
		request["component"] = s.Install.Component
		request["package_id"] = s.Install.PackageID
		request["archive_sha256"] = s.Install.ArchiveSHA256
		request["binary_sha256"] = s.Install.BinarySHA256
		if s.Install.AdmissionID != "" {
			request["admission_id"] = s.Install.AdmissionID
		}
		if s.Install.Application != nil {
			request["application"] = s.Install.Application
		}
		if s.Phase == "reconcile" {
			if s.InstallationTaskID() < 1 {
				return rcd.ErrConflict
			}
			request["original_task_id"] = s.InstallationTaskID()
		} else {
			pkg, e := repo.Get(context.Background(), task.ProjectID, s.Install.PackageID)
			if e != nil {
				return e
			}
			if !pkg.Enabled || pkg.Revision != s.Install.PackageRevision || pkg.ArchiveSHA256 != s.Install.ArchiveSHA256 || rcd.ValidateFirstInstallPackage(pkg.Manifest) != nil {
				return rcd.ErrConflict
			}
			packages, e := rcd.NewPackages(repo, root)
			if e != nil {
				return e
			}
			f, e := packages.OpenArchive(task.ProjectID, pkg)
			if e != nil {
				return e
			}
			f.Close()
			bind["package_path"] = filepath.Join(root, strconv.Itoa(task.ProjectID), s.Install.PackageID, "package.rcd.tar.gz")
		}
	}
	s.State = "running"
	if _, e = repo.UpdateHostSetup(context.Background(), task.ProjectID, s, s.Revision); e != nil {
		return e
	}
	vars["rcd_host_setup"] = bind
	return nil
}

type setupReport struct {
	SchemaVersion  int          `json:"schema_version"`
	OperationID    string       `json:"operation_id"`
	ProjectID      int          `json:"project_id"`
	TaskID         int          `json:"task_id"`
	Phase          string       `json:"phase"`
	State          string       `json:"state"`
	Error          string       `json:"error"`
	Warning        string       `json:"warning"`
	Platform       string       `json:"platform"`
	BinarySHA256   string       `json:"binary_sha256"`
	ArchiveSHA256  string       `json:"archive_sha256"`
	PackageID      string       `json:"package_id"`
	Component      string       `json:"component"`
	AdmissionID    string       `json:"admission_id"`
	OriginalTaskID int          `json:"original_task_id"`
	OriginalReport *setupReport `json:"original_report"`
	HostKeys       []struct {
		Algorithm   string `json:"algorithm"`
		Fingerprint string `json:"fingerprint"`
		PublicKey   string `json:"public_key"`
	} `json:"host_keys"`
}

func CollectRCDHostSetup(store db.Store, task db.Task) {
	if !task.Status.IsFinished() {
		return
	}
	repo := rcdRepository(store)
	if repo == nil {
		return
	}
	s, e := repo.HostSetupForTask(context.Background(), task.ProjectID, task.ID)
	if e != nil || (s.State != "running" && s.State != "unknown" && !strings.HasSuffix(s.State, "_queued")) {
		return
	}
	before, _ := json.Marshal(s)
	path := HostSetupReportPath(os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH"), task.ProjectID, task.ID)
	info, e := os.Lstat(path)
	var report setupReport
	valid := false
	if e == nil && info.Mode().IsRegular() && info.Size() <= 65536 {
		f, e := os.Open(path)
		if e == nil {
			raw, err := io.ReadAll(io.LimitReader(f, 65537))
			f.Close()
			if err == nil && len(raw) <= 65536 && json.Unmarshal(raw, &report) == nil && report.SchemaVersion == 1 && report.ProjectID == task.ProjectID && report.OperationID == s.ID && report.TaskID == task.ID && report.Phase == s.Phase && len(report.Error) <= 4096 {
				valid = true
			}
		}
	}
	if !valid {
		s.SetResult("unknown", "没有可信原任务结果；保留该接入操作，不自动重新执行")
	} else {
		switch {
		case s.Phase == "keyscan" && report.State == "awaiting_host_key" && task.Status == task_logger.TaskSuccessStatus:
			keys := []rcd.HostKey{}
			for _, k := range report.HostKeys {
				keys = append(keys, rcd.HostKey{Algorithm: k.Algorithm, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey})
			}
			if rcd.ValidateHostKeys(keys) != nil {
				s.SetResult("unknown", "SSH 指纹报告无效")
			} else {
				s.HostKeys = keys
				s.SetResult("awaiting_host_key", "")
			}
		case s.Phase == "probe" && report.State == "ready_for_install" && task.Status == task_logger.TaskSuccessStatus:
			if report.Platform != "linux/amd64" && report.Platform != "linux/arm64" {
				s.SetResult("failed", "主机架构不支持")
				break
			}
			s.Platform = report.Platform
			s.SetResult("ready_for_install", "")
		case s.Phase == "install" && report.State == "installed_stopped" && task.Status == task_logger.TaskSuccessStatus && matchesInstallReport(s, report):
			s.SetResult("installed_stopped", "")
			s.Warnings = []string{"已安装并保持停止；需完成业务配置和绑定，不代表已运行", "首装包及 SHA 已记录在本接入作业；不将它冒充已验收的历史恢复点"}
			if report.Warning != "" && len(report.Warning) <= 4096 {
				s.Warnings = append(s.Warnings, report.Warning)
			}
		case s.Phase == "install" && report.State == "failed_before_install" && (s.Install == nil || matchesInstallReport(s, report)):
			s.SetResult("failed", report.Error)
		case s.Phase == "reconcile" && report.State == "installed_stopped" && task.Status == task_logger.TaskSuccessStatus && validOriginalInstallation(s, task.ProjectID, report):
			s.SetResult("installed_stopped", "")
			s.Warnings = []string{"已只读确认原首装完成并保持停止；本次没有重新安装、启停或修改原任务结论", "首装包及 SHA 保留在原接入作业，尚未成为已验收的历史恢复点"}
			if report.Warning != "" && len(report.Warning) <= 4096 {
				s.Warnings = append(s.Warnings, report.Warning)
			}
		default:
			state := "failed"
			if s.Phase == "install" || s.Phase == "reconcile" || report.State == "unknown" {
				state = "unknown"
			}
			message := report.Error
			if message == "" {
				message = "步骤未成功，请查看原生任务日志"
			}
			s.SetResult(state, message)
		}
	}
	after, _ := json.Marshal(s)
	if bytes.Equal(before, after) {
		return
	}
	if _, e = repo.UpdateHostSetup(context.Background(), task.ProjectID, s, s.Revision); e != nil {
		log.Printf("RCD host setup reconcile: %v", e)
	}
}

func validOriginalInstallation(s rcd.HostSetup, project int, report setupReport) bool {
	original := report.OriginalReport
	return s.InstallationTaskID() > 0 && report.OriginalTaskID == s.InstallationTaskID() && matchesInstallReport(s, report) && original != nil && original.SchemaVersion == 1 && original.ProjectID == project && original.OperationID == s.ID && original.TaskID == s.InstallationTaskID() && original.Phase == "install" && original.State == "installed_stopped" && matchesInstallReport(s, *original)
}

func matchesInstallReport(s rcd.HostSetup, report setupReport) bool {
	if s.Install == nil || report.BinarySHA256 != s.Install.BinarySHA256 || report.AdmissionID != s.Install.AdmissionID {
		return false
	}
	// Empty admission denotes a legacy receipt written before this extension.
	// Newly admitted installs must prove the entire immutable package binding.
	return s.Install.AdmissionID == "" || (report.PackageID == s.Install.PackageID && report.Component == s.Install.Component && report.ArchiveSHA256 == s.Install.ArchiveSHA256)
}
func rcdRejectHostSetupStop(store db.Store, task db.Task) error {
	repo := rcdRepository(store)
	if repo == nil {
		return nil
	}
	_, e := repo.HostSetupForTask(context.Background(), task.ProjectID, task.ID)
	if errors.Is(e, rcd.ErrNotFound) {
		return nil
	}
	if e != nil {
		return e
	}
	return fmt.Errorf("主机接入操作不能强制中断，请补查原任务结果；不会自动重投")
}
