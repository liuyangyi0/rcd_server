// SPDX-License-Identifier: MIT
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/util"
)

// This is a submission guard on the native queue, not another execution queue.
// Scheduled, manual and advanced-template submissions all pass through it.
var rcdInspectionSubmission sync.Mutex

func rcdRepository(store db.Store) *rcd.Repository {
	if os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH") == "" {
		return nil
	}
	if p, ok := store.(interface{ RCDRepository() *rcd.Repository }); ok {
		return p.RCDRepository()
	}
	return nil
}

func ValidateRCDInspectionTemplate(store db.Store, project int, b rcd.DeviceBinding) error {
	t, err := store.GetTemplate(project, b.TemplateID)
	if err != nil {
		return err
	}
	if t.App != db.AppAnsible || t.Type != db.TemplateTask || t.Playbook != "tools/ansible/inspect-platform.yml" || t.InventoryID == nil || *t.InventoryID != b.InventoryID || t.RunnerTag != nil || util.Config.UseRemoteRunner {
		return fmt.Errorf("RCD inspection requires its fixed local Ansible template and inventory")
	}
	if t.Arguments != nil && *t.Arguments != "" && *t.Arguments != "[]" {
		return fmt.Errorf("RCD inspection template cannot override arguments")
	}
	var params db.AnsibleTemplateParams
	if err = t.FillParams(&params); err != nil {
		return err
	}
	if len(params.Limit) != 1 || params.Limit[0] != b.HostAlias || len(params.Tags) > 0 || len(params.SkipTags) > 0 {
		return fmt.Errorf("RCD inspection requires one exact host and no tag filters")
	}
	inv, err := store.GetInventory(project, b.InventoryID)
	if err != nil {
		return err
	}
	if inv.RunnerTag != nil {
		return fmt.Errorf("RCD inspection does not support remote runners")
	}
	return nil
}

func (p *TaskPool) prepareRCDInspection(task *db.Task, project int) (unlock func(), existing *db.Task, err error) {
	repo := rcdRepository(p.store)
	if repo == nil {
		return
	}
	device, e := repo.DeviceForTemplate(context.Background(), project, task.TemplateID)
	if errors.Is(e, rcd.ErrNotFound) {
		return
	}
	if e != nil {
		err = e
		return
	}
	device, e = repo.Device(context.Background(), project, device.ID)
	if e != nil {
		err = e
		return
	}
	rcdInspectionSubmission.Lock()
	unlock = rcdInspectionSubmission.Unlock
	if err = ValidateRCDInspectionTemplate(p.store, project, device.DeviceBinding); err != nil {
		return
	}
	latest, e := p.store.GetTemplateTasks(project, task.TemplateID, db.RetrieveQueryParams{Count: 1})
	if e != nil {
		err = e
		return
	}
	if len(latest) > 0 && !latest[0].Status.IsFinished() {
		existing = &latest[0].Task
		return
	}
	if len(latest) > 0 {
		CollectRCDInspection(p.store, latest[0].Task)
	}
	// Ignore browser-supplied execution overrides for a bound read-only template.
	// Variables come only from its administrator-owned native variable groups.
	task.Playbook = ""
	task.Environment = "{}"
	task.Secret = "{}"
	task.Arguments = nil
	task.GitBranch = nil
	task.InventoryID = nil
	task.Limit = ""
	task.Params = db.MapStringAnyField{}
	return
}

func rcdInspectionExtraVars(store db.Store, task db.Task, vars map[string]any) error {
	repo := rcdRepository(store)
	if repo == nil {
		return nil
	}
	// Reserved names must not be smuggled via variable groups on unbound tasks.
	delete(vars, "rcd_platform_inspection")
	device, err := repo.DeviceForTemplate(context.Background(), task.ProjectID, task.TemplateID)
	if errors.Is(err, rcd.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	path := rcd.InspectionPath(os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH"), task.ProjectID, task.ID)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err = ValidateRCDInspectionTemplate(store, task.ProjectID, device.DeviceBinding); err != nil {
		return err
	}
	binding := map[string]any{"device_id": device.ID, "task_id": task.ID, "host_alias": device.HostAlias, "report_path": path, "project_id": task.ProjectID}
	current, err := repo.Device(context.Background(), task.ProjectID, device.ID)
	if err != nil {
		return err
	}
	applications, err := repo.ListApplications(context.Background(), task.ProjectID)
	if err != nil {
		return err
	}
	binding["applications"] = applications
	if current.ActiveOperationID != "" {
		o, e := repo.Operation(context.Background(), task.ProjectID, current.ActiveOperationID)
		if e != nil {
			return e
		}
		if o.TaskID > 0 {
			binding["release_operation"] = map[string]any{"operation_id": o.ID, "task_id": o.TaskID, "device_id": device.ID, "host_alias": device.HostAlias, "component": o.Component()}
		}
	}
	vars["rcd_platform_inspection"] = binding
	return nil
}

// Reconciliation is safe to repeat after a platform restart. It only reads the
// original task's immutable result artifact; it never queues work or runs SSH.
func CollectRCDInspection(store db.Store, task db.Task) {
	if !task.Status.IsFinished() {
		return
	}
	repo := rcdRepository(store)
	if repo == nil {
		return
	}
	device, err := repo.DeviceForTemplate(context.Background(), task.ProjectID, task.TemplateID)
	if errors.Is(err, rcd.ErrNotFound) {
		return
	}
	if err != nil {
		log.Printf("RCD inspection lookup: %v", err)
		return
	}
	if device.LastTaskID >= task.ID {
		return
	}
	message := ""
	var observation []rcd.ComponentObservation
	var host *rcd.HostStatus
	if task.Status != task_logger.TaskSuccessStatus {
		message = "检查任务未成功；保留上次版本快照，请查看该任务日志"
	} else {
		var report *rcd.InspectionReport
		report, err = rcd.ReadInspectionReport(os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH"), task.ProjectID, device, task.ID)
		if err == nil {
			var applications []rcd.Application
			applications, err = repo.ListApplications(context.Background(), task.ProjectID)
			if err == nil {
				err = report.ValidateApplications(applications)
			}
			if err == nil {
				observation, err = report.Normalize(device, task.ID)
				host = report.HostObservation()
			}
		}
		if err != nil {
			message = "缺少或无法验证检查报告；未将任务成功当成程序正常"
			log.Printf("RCD inspection result %d: %v", task.ID, err)
		}
		if err == nil {
			collectRCDReadback(store, task, device)
		}
	}
	at := task.Created
	if task.Start != nil {
		at = *task.Start
	}
	if err = repo.AcceptObservation(context.Background(), task.ProjectID, device.ID, task.ID, at, observation, message, host); err != nil {
		log.Printf("RCD inspection persistence: %v", err)
	}
}
