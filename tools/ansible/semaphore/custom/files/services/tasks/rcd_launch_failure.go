// SPDX-License-Identifier: MIT
package tasks

import (
	"context"
	"errors"
	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/semaphoreui/semaphore/services/rcd"
	"time"
)

// Only an in-process typed pre-Start error proves no target was contacted.
// Missing reports, generic errors and textual log matches remain unknown.
func recordRCDLaunchFailure(store db.Store, task db.Task, runErr error) {
	var launch *db_lib.AnsibleLaunchError
	if !errors.As(runErr, &launch) {
		return
	}
	repo := rcdRepository(store)
	if repo == nil {
		return
	}
	ctx := context.Background()
	o, err := repo.OperationForTask(ctx, task.ProjectID, task.ID)
	if err != nil || o.KnownTerminal() {
		return
	}
	if o.State != "running" && o.State != "queued" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	o.State = "failed_before_install"
	o.Error = "执行器未启动，未连接目标机、未替换程序；可修复后创建新操作"
	for i := range o.Targets {
		o.Targets[i].State = o.State
		o.Targets[i].Error = launch.Error()
		o.Targets[i].Stages = []rcd.ReleaseStage{{Stage: "executor_start", State: "failed", StartedAt: now, EndedAt: now, ErrorType: "process_not_started", Error: &rcd.ReleaseError{Code: "process_not_started", Message: launch.Error()}}}
	}
	_ = repo.UpdateOperation(ctx, task.ProjectID, o, o.Revision)
}
