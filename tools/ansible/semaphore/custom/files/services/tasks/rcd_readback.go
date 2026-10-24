// SPDX-License-Identifier: MIT
package tasks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
)

// Only the fixed native inspection can supply remote evidence. This path never
// calls the release executor, and cannot choose another operation or machine.
func collectRCDReadback(store db.Store, task db.Task, device rcd.Device) {
	repo := rcdRepository(store)
	if repo == nil {
		return
	}
	root := os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH")
	report, err := rcd.ReadInspectionReport(root, task.ProjectID, device, task.ID)
	if err != nil || report.ReleaseInfo == nil || report.ReleaseInfo.SchemaVersion != 1 {
		return
	}
	var result releaseReport
	raw := report.ReleaseInfo.ReleaseReport
	if json.Unmarshal(raw, &result) != nil || result.SchemaVersion != 1 || result.DeviceID != device.ID || result.HostAlias != device.HostAlias {
		return
	}
	current, err := repo.Device(context.Background(), task.ProjectID, device.ID)
	if err != nil || current.ActiveOperationID != result.OperationID {
		return
	}
	o, err := repo.Operation(context.Background(), task.ProjectID, result.OperationID)
	if err != nil || o.KnownTerminal() || result.TaskID != o.TaskID || o.InventoryID != device.InventoryID {
		return
	}
	matched := false
	for _, target := range o.Targets {
		if target.DeviceID == device.ID && target.HostAlias == device.HostAlias && target.Address == device.Address {
			matched = true
		}
	}
	if !matched {
		return
	}
	dir := ReleaseReportDirectory(root, task.ProjectID, o.TaskID)
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".readback-*")
	if err != nil {
		return
	}
	name := file.Name()
	defer os.Remove(name)
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return
	}
	if os.Rename(name, filepath.Join(dir, device.ID+".json")) != nil {
		return
	}
	// Do not free reservations while the native executor could still proceed to
	// another machine. Its terminal hook/read-side collector will finish later.
	original, err := store.GetTask(task.ProjectID, o.TaskID)
	if err == nil {
		CollectRCDOperation(store, original)
	}
}
