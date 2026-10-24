// SPDX-License-Identifier: MIT
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type existingSetupStore struct {
	db.Store
	inventory db.Inventory
}

type hostSetupRecoveryStore struct {
	*hostSetupResultStore
	tasks []db.TaskWithTpl
}

func (s *hostSetupRecoveryStore) GetTemplateTasks(project, template int, params db.RetrieveQueryParams) ([]db.TaskWithTpl, error) {
	return s.tasks, nil
}

func TestRCDHostSetupRecoversOnlyUniqueOriginalNativeTaskWithoutQueueing(t *testing.T) {
	for _, scenario := range []string{"unique", "ambiguous", "wrong_user", "wrong_phase", "wrong_nonce", "old_task"} {
		t.Run(scenario, func(t *testing.T) {
			store := dbsql.InitConfigCreateTestStore()
			defer store.Close()
			if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
				t.Fatal(err)
			}
			repo, ctx := store.RCDRepository(), context.Background()
			s, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "root", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-task-recovery", "operator", 1)
			if err != nil {
				t.Fatal(err)
			}
			s.TemplateID = 7
			s.Steps[0].TaskID = 50
			s.StartPhase("install", "installation_queued")
			s.SetResult("unknown", "native insert acknowledgement lost")
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			user := 1
			task := db.Task{ID: 77, ProjectID: 1, TemplateID: 7, UserID: &user, Created: time.Now().Add(time.Second), Params: db.MapStringAnyField{"rcd_host_setup_id": s.ID}}
			switch scenario {
			case "wrong_user":
				other := 2
				task.UserID = &other
			case "wrong_phase":
				task.Params["rcd_host_setup_phase"] = "probe"
			case "wrong_nonce":
				task.Params["rcd_host_setup_phase"] = "install"
				task.Params["rcd_host_setup_admission"] = "not-the-current-admission"
			case "old_task":
				task.ID = 50
			}
			fake := &hostSetupRecoveryStore{hostSetupResultStore: &hostSetupResultStore{Store: store, repo: repo}, tasks: []db.TaskWithTpl{{Task: task}}}
			if scenario == "ambiguous" {
				second := task
				second.ID = 78
				fake.tasks = append(fake.tasks, db.TaskWithTpl{Task: second})
			}
			got, err := RecoverRCDHostSetupTask(fake, repo, 1, s)
			if scenario == "ambiguous" {
				if !errors.Is(err, rcd.ErrConflict) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unique" {
				if got.TaskID != 77 || got.InstallationTaskID() != 77 || got.State != "unknown" || got.Steps[0].TaskID != 50 {
					t.Fatal(got)
				}
			} else if got.TaskID != 0 {
				t.Fatal("attached an unrelated native task", got)
			}
			if _, err = repo.ActiveHostSetup(ctx, 1, s.Request.Address); err != nil {
				t.Fatal("recovery must retain reservation", err)
			}
		})
	}
}

func TestRCDHostSetupReadOnlyReconciliationRequiresOriginalReceiptProof(t *testing.T) {
	for _, scenario := range []string{"confirmed", "wrong_original_task", "wrong_hash", "wrong_operation", "wrong_admission", "wrong_package", "wrong_archive", "wrong_component", "query_failed", "unknown_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			store := dbsql.InitConfigCreateTestStore()
			defer store.Close()
			if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
				t.Fatal(err)
			}
			repo, ctx := store.RCDRepository(), context.Background()
			root := t.TempDir()
			t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", root)
			s, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "root", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-reconcile", "operator", 1)
			if err != nil {
				t.Fatal(err)
			}
			s.Install = &rcd.HostSetupInstall{PackageID: "package", Component: "demo", BinarySHA256: "original-binary"}
			s.StartPhase("install", "installation_queued")
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 77); err != nil {
				t.Fatal(err)
			}
			s, _ = repo.HostSetup(ctx, 1, s.ID)
			s.SetResult("unknown", "original task reply lost")
			s.StartPhase("reconcile", "reconcile_queued")
			s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 78); err != nil {
				t.Fatal(err)
			}
			report := setupReport{SchemaVersion: 1, ProjectID: 1, OperationID: s.ID, TaskID: 78, Phase: "reconcile", State: "installed_stopped", BinarySHA256: "original-binary", OriginalTaskID: 77,
				OriginalReport: &setupReport{SchemaVersion: 1, ProjectID: 1, OperationID: s.ID, TaskID: 77, Phase: "install", State: "installed_stopped", BinarySHA256: "original-binary"}}
			report.AdmissionID = s.Install.AdmissionID
			report.OriginalReport.AdmissionID = s.Install.AdmissionID
			report.PackageID = s.Install.PackageID
			report.OriginalReport.PackageID = s.Install.PackageID
			report.Component = s.Install.Component
			report.OriginalReport.Component = s.Install.Component
			report.ArchiveSHA256 = s.Install.ArchiveSHA256
			report.OriginalReport.ArchiveSHA256 = s.Install.ArchiveSHA256
			status := task_logger.TaskSuccessStatus
			switch scenario {
			case "wrong_original_task":
				report.OriginalReport.TaskID = 76
			case "wrong_hash":
				report.OriginalReport.BinarySHA256 = "different"
			case "wrong_operation":
				report.OriginalReport.OperationID = "different"
			case "wrong_admission":
				report.OriginalReport.AdmissionID = "other-install-admission"
			case "wrong_package":
				report.OriginalReport.PackageID = "other-package"
			case "wrong_archive":
				report.OriginalReport.ArchiveSHA256 = "other-archive"
			case "wrong_component":
				report.OriginalReport.Component = "manager"
			case "query_failed":
				status = task_logger.TaskFailStatus
			case "unknown_receipt":
				report.OriginalReport.State = "unknown"
			}
			path := HostSetupReportPath(root, 1, 78)
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(report)
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			CollectRCDHostSetup(&hostSetupResultStore{Store: store, repo: repo}, db.Task{ID: 78, ProjectID: 1, Status: status})
			got, err := repo.HostSetup(ctx, 1, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, reserved := repo.ActiveHostSetup(ctx, 1, s.Request.Address)
			if got.InstallationTaskID() != 77 || got.Steps[len(got.Steps)-2].TaskID != 77 || got.Steps[len(got.Steps)-2].State != "unknown" {
				t.Fatal("rewrote original installation", got)
			}
			if scenario == "confirmed" {
				if got.State != "installed_stopped" || got.Error != "" || len(got.Warnings) == 0 || !errors.Is(reserved, rcd.ErrNotFound) {
					t.Fatal(got, reserved)
				}
			} else if got.State != "unknown" || reserved != nil {
				t.Fatal("untrusted evidence released reservation", got, reserved)
			}
		})
	}
}

func (s *existingSetupStore) GetInventory(project, id int) (db.Inventory, error) {
	return s.inventory, nil
}
func TestRCDHostSetupExistingInventoryBindingDoesNotAcceptCallerPaths(t *testing.T) {
	key, become := 3, 4
	host := map[string]any{"ansible_host": "192.0.2.10", "ansible_user": "operator", "ansible_port": 2222, "rcd_services": []map[string]any{{"component": "manager", "run_as": "operator"}}}
	inventory := func() string {
		raw, _ := json.Marshal(map[string]any{"all": map[string]any{"children": map[string]any{"rcd_nodes": map[string]any{"hosts": map[string]any{"fixture": host}}}}})
		return string(raw)
	}
	store := &existingSetupStore{inventory: db.Inventory{ID: 7, Type: db.InventoryStaticYaml, SSHKeyID: &key, BecomeKeyID: &become, Inventory: inventory()}}
	d := rcd.Device{ID: "existing", DeviceBinding: rcd.DeviceBinding{Name: "fixture", Address: "192.0.2.10", HostAlias: "fixture", InventoryID: 7}}
	req, e := RCDExistingHostSetupRequest(store, 1, d)
	if e != nil || req.ServiceAccount != "operator" || req.SSHPort != 2222 || req.SSHKeyID != key || req.BecomeKeyID != become {
		t.Fatal(req, e)
	}
	host["ansible_host"] = "192.0.2.11"
	store.inventory.Inventory = inventory()
	if _, e = RCDExistingHostSetupRequest(store, 1, d); e == nil {
		t.Fatal("different physical address accepted")
	}
	host["ansible_host"] = "192.0.2.10"
	host["rcd_device_owner"] = "root"
	store.inventory.Inventory = inventory()
	if _, e = RCDExistingHostSetupRequest(store, 1, d); e == nil {
		t.Fatal("root/conflicting owner accepted")
	}
	host["rcd_device_owner"] = "another"
	store.inventory.Inventory = inventory()
	if _, e = RCDExistingHostSetupRequest(store, 1, d); e == nil {
		t.Fatal("ambiguous owner accepted")
	}
}

type hostSetupResultStore struct {
	db.Store
	repo *rcd.Repository
}

func (s *hostSetupResultStore) RCDRepository() *rcd.Repository { return s.repo }
func TestRCDHostSetupOnlyTrustedBeforeInstallFailureReleasesOccupancy(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{true: "trusted", false: "mismatched"}[valid], func(t *testing.T) {
			store := dbsql.InitConfigCreateTestStore()
			defer store.Close()
			if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
				t.Fatal(e)
			}
			root := t.TempDir()
			t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", root)
			repo := store.RCDRepository()
			ctx := context.Background()
			s, _, e := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "operator", SSHKeyID: 1, BecomeKeyID: 2, ServiceAccount: "operator"}, "fixture-before-install", "operator", 1)
			if e != nil {
				t.Fatal(e)
			}
			s.StartPhase("install", "installation_queued")
			s, e = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
			if e != nil {
				t.Fatal(e)
			}
			if e = repo.AttachHostSetupTask(ctx, 1, s.ID, 77); e != nil {
				t.Fatal(e)
			}
			path := HostSetupReportPath(root, 1, 77)
			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			id := s.ID
			if !valid {
				id = "not-original"
			}
			report := map[string]any{"schema_version": 1, "operation_id": id, "project_id": 1, "task_id": 77, "phase": "install", "state": "failed_before_install", "error": "existing entry: no files replaced"}
			raw, _ := json.Marshal(report)
			if e = os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			CollectRCDHostSetup(&hostSetupResultStore{Store: store, repo: repo}, db.Task{ID: 77, ProjectID: 1, Status: task_logger.TaskFailStatus})
			got, e := repo.HostSetup(ctx, 1, s.ID)
			if e != nil {
				t.Fatal(e)
			}
			_, lockErr := repo.ActiveHostSetup(ctx, 1, s.Request.Address)
			if valid {
				if got.State != "failed" || !errors.Is(lockErr, rcd.ErrNotFound) {
					t.Fatal(got.State, lockErr)
				}
			} else if got.State != "unknown" || lockErr != nil {
				t.Fatal("untrusted failure released reservation", got.State, lockErr)
			}
		})
	}
}
func TestRCDHostSetupInventoryStrictNativeCredentialsOnly(t *testing.T) {
	s := rcd.HostSetup{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Request: rcd.HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "operator", SSHKeyID: 3, ServiceAccount: "operator"}}
	var value map[string]any
	if json.Unmarshal([]byte(SetupInventory(s)), &value) != nil {
		t.Fatal("not valid static YAML JSON")
	}
	raw := SetupInventory(s)
	for _, word := range []string{"password", "private_key", "shell", "reboot"} {
		for i := 0; i+len(word) <= len(raw); i++ {
			if raw[i:i+len(word)] == word {
				t.Fatal("inventory contains unsafe material", word)
			}
		}
	}
}
