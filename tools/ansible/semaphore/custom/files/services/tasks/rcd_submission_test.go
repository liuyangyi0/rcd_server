// SPDX-License-Identifier: MIT
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDInvalidNativeTemplateIsTypedBeforeTaskCreation(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", t.TempDir())
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	packages, _ := rcd.NewPackages(repo, t.TempDir())
	pkg, _, err := packages.Upload(ctx, 1, "operator", bytes.NewReader(testutil.Package(t, "manager", "r1", "test")))
	if err != nil {
		t.Fatal(err)
	}
	d, err := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 4, TemplateID: 3, ReleaseTemplateID: 20}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	components := []rcd.ComponentObservation{}
	for _, name := range []string{"lower", "manager", "agent"} {
		components = append(components, rcd.ComponentObservation{Component: name, State: "running", RunningBuild: &rcd.BuildObservation{Component: name, Version: "1.0.0", Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 1}}})
	}
	if err := repo.AcceptObservation(ctx, 1, d.ID, 1, time.Now().UTC(), components, ""); err != nil {
		t.Fatal(err)
	}
	req := rcd.OperationRequest{PackageID: pkg.ID, PackageRevision: 1, Action: "upgrade", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: 1}}}
	op, nonce, _, err := repo.CreateOperation(ctx, 1, "invalid-template", req, "operator", 1)
	if err != nil {
		t.Fatal(err)
	}
	inv := 4
	args := `["--unsafe-override"]`
	fake := &rcdInspectionStore{Store: store, repo: repo, template: db.Template{ID: 20, App: db.AppAnsible, Playbook: "tools/ansible/release-platform.yml", InventoryID: &inv, Arguments: &args}}
	pool := TaskPool{store: fake}
	uid := 1
	secret, _ := json.Marshal(map[string]string{"rcd_operation_id": op.ID, "rcd_nonce": nonce})
	_, err = pool.AddTask(db.Task{TemplateID: 20, Secret: string(secret)}, &uid, "operator", 1, false)
	var rejected *RCDTaskAdmissionError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected typed pre-admission error, got %v", err)
	}
	var tasks int
	if err := store.Sql().Db.QueryRow("select count(*) from task").Scan(&tasks); err != nil || tasks != 0 {
		t.Fatal("a rejected task was persisted", tasks, err)
	}
	if err := repo.RecordSubmissionFailure(ctx, 1, op.ID, err.Error(), true); err != nil {
		t.Fatal(err)
	}
	d, _ = repo.Device(ctx, 1, d.ID)
	if d.ActiveOperationID != "" {
		t.Fatal("pre-admission failure retained host lock", d)
	}
}
