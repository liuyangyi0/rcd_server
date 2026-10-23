// SPDX-License-Identifier: MIT
package tasks

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/util"
)

var rcdReleaseSubmission sync.Mutex

// Only the native pre-admission hook constructs this type: no task has been
// persisted, queued or dispatched. Errors after task creation stay ambiguous.
type RCDTaskAdmissionError struct{ Err error }

func (e *RCDTaskAdmissionError) Error() string { return e.Err.Error() }
func (e *RCDTaskAdmissionError) Unwrap() error { return e.Err }

func ValidateRCDReleaseTemplate(store db.Store, project, template, inventory int) error {
	return validateRCDTaskTemplate(store, project, template, inventory, "tools/ansible/release-platform.yml")
}

func ValidateRCDControlTemplate(store db.Store, project, template, inventory int) error {
	return validateRCDTaskTemplate(store, project, template, inventory, "tools/ansible/control-platform.yml")
}

func ValidateRCDOperationTemplate(store db.Store, project int, operation rcd.Operation) error {
	if operation.ServiceControl() {
		return ValidateRCDControlTemplate(store, project, operation.TemplateID, operation.InventoryID)
	}
	return ValidateRCDReleaseTemplate(store, project, operation.TemplateID, operation.InventoryID)
}

func validateRCDTaskTemplate(store db.Store, project, template, inventory int, playbook string) error {
	t, err := store.GetTemplate(project, template)
	if err != nil {
		return err
	}
	if t.App != db.AppAnsible || t.Type != db.TemplateTask || t.Playbook != playbook || t.InventoryID == nil || *t.InventoryID != inventory || t.RunnerTag != nil || util.Config.UseRemoteRunner {
		return fmt.Errorf("发布必须使用受控的本地 Ansible 发布模板和库存")
	}
	if t.Arguments != nil && *t.Arguments != "" && *t.Arguments != "[]" {
		return fmt.Errorf("发布模板不能添加执行参数")
	}
	var params db.AnsibleTemplateParams
	if err = t.FillParams(&params); err != nil {
		return err
	}
	if len(params.Tags) > 0 || len(params.SkipTags) > 0 || len(params.Limit) > 0 {
		return fmt.Errorf("发布模板不能预设目标或标签过滤")
	}
	inv, err := store.GetInventory(project, inventory)
	if err != nil {
		return err
	}
	if inv.RunnerTag != nil {
		return fmt.Errorf("发布不支持远程 Runner")
	}
	return nil
}

func (p *TaskPool) prepareRCDRelease(task *db.Task, project int, user *int) (func(), error) {
	repo := rcdRepository(p.store)
	if repo == nil {
		return nil, nil
	}
	tpl, err := p.store.GetTemplate(project, task.TemplateID)
	if err != nil {
		return nil, err
	}
	if tpl.Playbook == "tools/ansible/semaphore-release.yml" || task.Playbook == "tools/ansible/semaphore-release.yml" {
		return nil, fmt.Errorf("请从设备与版本页面创建升级或恢复，不再运行旧发布模板")
	}
	if tpl.Playbook != "tools/ansible/release-platform.yml" && tpl.Playbook != "tools/ansible/control-platform.yml" {
		delete(task.Params, "rcd_operation_id")
		if task.Playbook == "tools/ansible/release-platform.yml" || task.Playbook == "tools/ansible/control-platform.yml" {
			return nil, rcd.ErrInvalid
		}
		return nil, nil
	}
	rcdReleaseSubmission.Lock()
	var input struct {
		OperationID string `json:"rcd_operation_id"`
		Nonce       string `json:"rcd_nonce"`
	}
	if json.Unmarshal([]byte(task.Secret), &input) != nil {
		return rcdReleaseSubmission.Unlock, rcd.ErrInvalid
	}
	o, err := repo.AuthorizeSubmission(context.Background(), project, input.OperationID, input.Nonce)
	if err != nil {
		return rcdReleaseSubmission.Unlock, err
	}
	if user == nil || o.UserID != *user || task.TemplateID != o.TemplateID {
		return rcdReleaseSubmission.Unlock, rcd.ErrConflict
	}
	if err = ValidateRCDOperationTemplate(p.store, project, o); err != nil {
		return rcdReleaseSubmission.Unlock, err
	}
	task.Playbook = ""
	task.Environment = "{}"
	task.Arguments = nil
	task.GitBranch = nil
	task.InventoryID = nil
	task.Limit = ""
	task.Secret = "{}"
	task.Params = db.MapStringAnyField{"rcd_operation_id": o.ID, "skip_galaxy_install": true}
	task.Message = "RCD 软件" + o.Request.Action + " · " + o.Package.Component + " · " + o.Package.Version
	if o.ServiceControl() {
		task.Message = "RCD 服务" + o.Request.Action + " · " + o.Request.Component
	}
	return rcdReleaseSubmission.Unlock, nil
}

func (p *TaskPool) attachRCDRelease(task db.Task) error {
	repo := rcdRepository(p.store)
	if repo == nil {
		return nil
	}
	id, _ := task.Params["rcd_operation_id"].(string)
	if id == "" {
		return nil
	}
	return repo.AttachTask(context.Background(), task.ProjectID, id, task.ID)
}

func ReleaseReportDirectory(root string, project, task int) string {
	return filepath.Join(root, "operations", strconv.Itoa(project), strconv.Itoa(task))
}

func rcdReleaseExtraVars(store db.Store, task db.Task, vars map[string]any) error {
	repo := rcdRepository(store)
	if repo == nil {
		return nil
	}
	delete(vars, "rcd_platform_release")
	delete(vars, "rcd_platform_control")
	o, err := repo.OperationForTask(context.Background(), task.ProjectID, task.ID)
	if errors.Is(err, rcd.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = ValidateRCDOperationTemplate(store, task.ProjectID, o); err != nil {
		return err
	}
	if o.State != "queued" {
		return fmt.Errorf("发布已取消、已执行或待确认，禁止重新派发")
	}
	// Native executor preparation is the one-way execution admission. Cancellation
	// CAS may win first; a restarted task never re-enters an already running op.
	o.State = "running"
	if err = repo.UpdateOperation(context.Background(), task.ProjectID, o, o.Revision); err != nil {
		return err
	}
	root := os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH")
	reports := ReleaseReportDirectory(root, task.ProjectID, task.ID)
	if err = os.MkdirAll(reports, 0700); err != nil {
		return err
	}
	if o.ServiceControl() {
		hosts := []string{}
		for _, t := range o.Targets {
			hosts = append(hosts, t.HostAlias)
		}
		vars["rcd_platform_control"] = map[string]any{"operation_id": o.ID, "task_id": task.ID, "component": o.Request.Component, "action": o.Request.Action, "actor": o.Actor, "targets": o.Targets, "hosts": hosts, "report_directory": reports, "commands_paused": o.Request.CommandsPaused, "application": o.Application, "project_id": task.ProjectID}
		return nil
	}
	packages, err := rcd.NewPackages(repo, root)
	if err != nil {
		return err
	}
	p, err := repo.Get(context.Background(), task.ProjectID, o.Request.PackageID)
	if err != nil {
		return err
	}
	f, err := packages.OpenArchive(task.ProjectID, p)
	if err != nil {
		return err
	}
	f.Close()
	hosts := []string{}
	for _, t := range o.Targets {
		hosts = append(hosts, t.HostAlias)
	}
	vars["rcd_platform_release"] = map[string]any{"operation_id": o.ID, "task_id": task.ID, "component": o.Package.Component,
		"actor": o.Actor, "action": o.Request.Action, "targets": o.Targets, "hosts": hosts, "report_directory": reports,
		"package_directory": filepath.Join(root, strconv.Itoa(task.ProjectID), o.Request.PackageID), "package_id": o.Request.PackageID,
		"manifest": o.Package, "commands_paused": o.Request.CommandsPaused, "project_id": task.ProjectID, "application": o.Application}
	return nil
}

type releaseReport struct {
	SchemaVersion int                `json:"schema_version"`
	OperationID   string             `json:"operation_id"`
	TaskID        int                `json:"task_id"`
	DeviceID      string             `json:"device_id"`
	HostAlias     string             `json:"host_alias"`
	State         string             `json:"state"`
	Error         string             `json:"error"`
	BinarySHA256  string             `json:"binary_sha256"`
	Stages        []rcd.ReleaseStage `json:"stages"`
	Acceptance    map[string]any     `json:"acceptance"`
}

// Recovery is a result with proof, not a trusted state label. Missing immutable
// pre-install evidence (including older operations) stays unknown and reserved.
func confirmedRCDRecovery(o rcd.Operation, target rcd.ReleaseTarget, report releaseReport) bool {
	if o.ServiceControl() || len(target.PreviousBinarySHA256) != 64 ||
		report.BinarySHA256 != target.PreviousBinarySHA256 || report.Acceptance["binary_sha256"] != target.PreviousBinarySHA256 || len(report.Stages) == 0 {
		return false
	}
	if _, err := hex.DecodeString(target.PreviousBinarySHA256); err != nil {
		return false
	}
	last := report.Stages[len(report.Stages)-1]
	if last.Stage != "automatic_binary_recovery" || last.State != "confirmed" || last.Error != nil {
		return false
	}
	start, a := time.Parse(time.RFC3339Nano, last.StartedAt)
	end, b := time.Parse(time.RFC3339Nano, last.EndedAt)
	if a != nil || b != nil || end.Before(start) {
		return false
	}
	// JSON reports decode numbers as float64. Require an actual integral PID,
	// not a truthy string or an omitted zero, for both running and stopped proof.
	pid, ok := report.Acceptance["pid"].(float64)
	if !ok || math.IsNaN(pid) || math.IsInf(pid, 0) || pid < 0 || pid > math.MaxInt32 || pid != math.Trunc(pid) {
		return false
	}
	if target.PreviousState == "stopped" {
		return pid == 0 && report.Acceptance["state"] == "installed_stopped"
	}
	if target.PreviousState != "running" || pid == 0 {
		return false
	}
	if state, supplied := report.Acceptance["state"]; supplied && state != "running" {
		return false
	}
	if o.Package.Component == "agent" {
		return report.Acceptance["agent_http_ready"] == true
	}
	return report.Acceptance["opc_tcp_reachable"] == true
}

func CollectRCDOperation(store db.Store, task db.Task) {
	repo := rcdRepository(store)
	if repo == nil {
		return
	}
	o, err := repo.OperationForTask(context.Background(), task.ProjectID, task.ID)
	if err != nil || o.KnownTerminal() {
		return
	}
	if !task.Status.IsFinished() {
		return
	}
	allSuccess := true
	allKnown := true
	succeeded, recovered := 0, 0
	for i := range o.Targets {
		target := &o.Targets[i]
		path := filepath.Join(ReleaseReportDirectory(os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH"), task.ProjectID, task.ID), target.DeviceID+".json")
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
			target.State = "unknown"
			target.Error = "没有可靠的目标机结果，请检查原任务；不会自动重投"
			allSuccess = false
			allKnown = false
			continue
		}
		f, e := os.Open(path)
		if e != nil {
			allSuccess = false
			allKnown = false
			target.State = "unknown"
			continue
		}
		raw, e := io.ReadAll(io.LimitReader(f, 256<<10+1))
		f.Close()
		var report releaseReport
		if e != nil || json.Unmarshal(raw, &report) != nil || report.SchemaVersion != 1 || report.OperationID != o.ID || report.TaskID != task.ID || report.DeviceID != target.DeviceID || report.HostAlias != target.HostAlias || len(report.Stages) > 100 || len(report.Error) > 4096 {
			target.State = "unknown"
			target.Error = "发布报告不完整或身份不匹配"
			allSuccess = false
			allKnown = false
			continue
		}
		switch report.State {
		case "succeeded":
			expected := o.Package.BinarySHA256
			if o.ServiceControl() {
				expected = target.ExpectedBinarySHA256
			}
			if expected == "" || report.BinarySHA256 != expected || report.Acceptance["binary_sha256"] != expected {
				report.State = "unknown"
				report.Error = "软件验收缺少目标映像证据"
			}
			if o.ServiceControl() {
				want := "running"
				if o.Request.Action == "stop" {
					want = "stopped"
				}
				if report.Acceptance["state"] != want {
					report.State = "unknown"
					report.Error = "服务控制缺少目标状态证据"
				}
			}
		case "recovered":
			if !confirmedRCDRecovery(o, *target, report) {
				report.State = "unknown"
				report.Error = "恢复报告缺少原程序摘要、进程状态或验收证据；保留占用，请补查原事务"
			}
		case "failed_before_install", "failed_before_execution", "not_started", "recovery_failed", "needs_maintenance", "unknown":
		default:
			report.State = "unknown"
		}
		target.State, target.Error, target.Stages, target.Acceptance = report.State, report.Error, report.Stages, report.Acceptance
		if target.State != "succeeded" {
			allSuccess = false
		} else {
			succeeded++
		}
		if target.State == "recovered" {
			recovered++
		}
		if target.State == "unknown" || target.State == "needs_maintenance" || target.State == "recovery_failed" {
			allKnown = false
		}
	}
	o.State = "unknown"
	o.Error = "部分目标结果待确认；保留发布占用，不自动重新执行"
	if allKnown {
		o.State = "failed_before_install"
		if o.ServiceControl() {
			o.State = "failed_before_execution"
		}
		o.Error = "发布未全部完成，后续目标已停止"
		if recovered > 0 {
			o.State = "recovered"
			o.Error = fmt.Sprintf("安装失败，%d 台已确认恢复原程序；其余目标未完成升级，请查看逐机结果", recovered)
		}
		if succeeded > 0 {
			o.State = "partial_failed"
			o.Error = fmt.Sprintf("部分完成：%d/%d 台成功，%d 台恢复原程序；其余目标失败或未开始，后续目标不再执行", succeeded, len(o.Targets), recovered)
		}
	}
	if allSuccess {
		o.State = "succeeded"
		o.Error = ""
	}
	_ = repo.UpdateOperation(context.Background(), task.ProjectID, o, o.Revision)
}

func rcdRejectNativeStop(store db.Store, task db.Task) error {
	repo := rcdRepository(store)
	if repo == nil {
		return nil
	}
	_, err := repo.OperationForTask(context.Background(), task.ProjectID, task.ID)
	if errors.Is(err, rcd.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("RCD 发布不能强制中断；仅能在升级任务页取消尚未开始的操作")
}

func (p *TaskPool) scheduleRCDPostRelease(task db.Task) {
	repo := rcdRepository(p.store)
	if repo == nil {
		return
	}
	o, err := repo.OperationForTask(context.Background(), task.ProjectID, task.ID)
	if err != nil || !o.KnownTerminal() || o.PostInspectionRequested || o.State == "cancelled" {
		return
	}
	o.PostInspectionRequested = true
	if repo.UpdateOperation(context.Background(), task.ProjectID, o, o.Revision) != nil {
		return
	}
	// Same native queue, same per-device coalescing guard. No hardware write.
	for _, target := range o.Targets {
		if target.State != "succeeded" && target.State != "recovered" {
			continue
		}
		d, e := repo.Device(context.Background(), task.ProjectID, target.DeviceID)
		if e != nil {
			continue
		}
		_, _ = p.AddTask(db.Task{TemplateID: d.TemplateID, Message: "RCD 发布后只读版本确认"}, &o.UserID, o.Actor, task.ProjectID, false)
	}
}

func IsRCDReleaseTemplate(playbook string) bool {
	return strings.HasSuffix(playbook, "/release-platform.yml") || strings.HasSuffix(playbook, "/semaphore-release.yml") || strings.HasSuffix(playbook, "/control-platform.yml")
}
