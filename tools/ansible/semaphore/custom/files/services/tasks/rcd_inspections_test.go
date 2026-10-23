// SPDX-License-Identifier: MIT
package tasks

import (
	"context"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
)

type rcdInspectionStore struct {
	db.Store
	repo     *rcd.Repository
	template db.Template
	pending  []db.TaskWithTpl
}

func (s *rcdInspectionStore) RCDRepository() *rcd.Repository { return s.repo }
func (s *rcdInspectionStore) GetTemplate(project, id int) (db.Template, error) {
	return s.template, nil
}
func (s *rcdInspectionStore) GetInventory(project, id int) (db.Inventory, error) {
	return db.Inventory{}, nil
}
func (s *rcdInspectionStore) GetTemplateTasks(project, id int, p db.RetrieveQueryParams) ([]db.TaskWithTpl, error) {
	return s.pending, nil
}
func TestRCDNativeInspectionGuard(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", t.TempDir())
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	inventory := 4
	s := &rcdInspectionStore{Store: store, repo: store.RCDRepository(), template: db.Template{ID: 3, App: db.AppAnsible, Playbook: "tools/ansible/inspect-platform.yml", InventoryID: &inventory, TaskParams: db.MapStringAnyField{"limit": []string{"test46"}}}}
	d, err := s.repo.AddDevice(context.Background(), 1, rcd.DeviceBinding{Name: "test", Address: "127.0.0.1", HostAlias: "test46", InventoryID: 4, TemplateID: 3}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	p := TaskPool{store: s}
	task := db.Task{TemplateID: 3, Environment: `{"injected":true}`, Playbook: "wrong.yml", Params: db.MapStringAnyField{"limit": []string{"all"}}}
	unlock, existing, err := p.prepareRCDInspection(&task, 1)
	if unlock != nil {
		unlock()
	}
	if err != nil || existing != nil || task.Environment != "{}" || task.Playbook != "" || len(task.Params) != 0 {
		t.Fatal(task, existing, err)
	}
	s.pending = []db.TaskWithTpl{{Task: db.Task{ID: 90, Status: task_logger.TaskRunningStatus}}}
	unlock, existing, err = p.prepareRCDInspection(&task, 1)
	if unlock != nil {
		unlock()
	}
	if err != nil || existing == nil || existing.ID != 90 {
		t.Fatal(existing, err)
	}
	task.ID = 91
	task.ProjectID = 1
	vars := map[string]any{"rcd_platform_inspection": "injected"}
	if err = rcdInspectionExtraVars(s, task, vars); err != nil {
		t.Fatal(err)
	}
	result := vars["rcd_platform_inspection"].(map[string]any)
	if result["device_id"] != d.ID || result["task_id"] != 91 {
		t.Fatal(vars)
	}
	s.template.Playbook = "arbitrary.yml"
	unlock, _, err = p.prepareRCDInspection(&task, 1)
	if unlock != nil {
		unlock()
	}
	if err == nil {
		t.Fatal("changed template bypassed guard")
	}
}

func TestRCDDisabledExtensionDoesNotChangeNativeVariables(t *testing.T) {
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", "")
	vars := map[string]any{"rcd_platform_inspection": "user value", "other": 123}
	if err := rcdInspectionExtraVars(nil, db.Task{}, vars); err != nil {
		t.Fatal(err)
	}
	if vars["rcd_platform_inspection"] != "user value" || vars["other"] != 123 {
		t.Fatal(vars)
	}
}

func TestRCDHostSnapshotRetainedWithoutInventingFreshness(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	repo := store.RCDRepository()
	d, err := repo.AddDevice(context.Background(), 1, rcd.DeviceBinding{Name: "test", Address: "127.0.0.1", HostAlias: "test46", InventoryID: 4, TemplateID: 3}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Now().UTC()
	components := []rcd.ComponentObservation{{Component: "manager", State: "stopped"}, {Component: "agent", State: "stopped"}, {Component: "lower", State: "unknown"}}
	host := &rcd.HostStatus{SchemaVersion: 1, ObservedAt: at.Format(time.RFC3339Nano), Available: true}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 10, at, components, "", host); err != nil {
		t.Fatal(err)
	}
	failed := &rcd.HostStatus{SchemaVersion: 1, ObservedAt: at.Add(time.Minute).Format(time.RFC3339Nano), Error: "metrics unavailable"}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 11, at.Add(time.Minute), components, "", failed); err != nil {
		t.Fatal(err)
	}
	d, err = repo.Device(ctx, 1, d.ID)
	if err != nil || d.HostStatus == nil || d.HostStatus.ObservedAt != host.ObservedAt || d.HostStatus.LastAttemptAt != failed.ObservedAt || d.HostStatus.Error == "" {
		t.Fatal(d, err)
	}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 12, at.Add(2*time.Minute), nil, "SSH failed"); err != nil {
		t.Fatal(err)
	}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 9, at, components, "", host); err != nil {
		t.Fatal(err)
	}
	d, err = repo.Device(ctx, 1, d.ID)
	if err != nil || d.LastTaskID != 12 || d.HostStatus.ObservedAt != host.ObservedAt || d.LastError != "SSH failed" {
		t.Fatal(d, err)
	}
}
