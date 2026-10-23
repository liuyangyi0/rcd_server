package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/db_lib"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

type readbackStore struct {
	db.Store
	repo *rcd.Repository
	task db.Task
}

func (s *readbackStore) RCDRepository() *rcd.Repository           { return s.repo }
func (s *readbackStore) GetTask(project, id int) (db.Task, error) { return s.task, nil }

func TestRCDReadbackRecoversLostReportWithoutReexecuting(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	root := t.TempDir()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", root)
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'readback')"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo := store.RCDRepository()
	packages, _ := rcd.NewPackages(repo, root)
	p, _, err := packages.Upload(ctx, 1, "test", bytes.NewReader(testutil.Package(t, "manager", "r1", "fixture")))
	if err != nil {
		t.Fatal(err)
	}
	d, err := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "isolated", Address: "127.0.0.1", HostAlias: "test", InventoryID: 2, TemplateID: 3, ReleaseTemplateID: 4}, "test")
	if err != nil {
		t.Fatal(err)
	}
	observations := []rcd.ComponentObservation{{Component: "manager", State: "running", RunningBuild: &rcd.BuildObservation{Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 1}}}, {Component: "lower", State: "stopped"}, {Component: "agent", State: "stopped"}}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 1, time.Now().UTC(), observations, ""); err != nil {
		t.Fatal(err)
	}
	req := rcd.OperationRequest{PackageID: p.ID, PackageRevision: 1, Action: "upgrade", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: 1}}}
	op, _, _, err := repo.CreateOperation(ctx, 1, "once", req, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.AttachTask(ctx, 1, op.ID, 90); err != nil {
		t.Fatal(err)
	}
	op, _ = repo.Operation(ctx, 1, op.ID)
	op.State = "unknown"
	if err = repo.UpdateOperation(ctx, 1, op, op.Revision); err != nil {
		t.Fatal(err)
	}
	original := db.Task{ID: 90, ProjectID: 1, Status: task_logger.TaskFailStatus}
	s := &readbackStore{Store: store, repo: repo, task: original}
	task := db.Task{ID: 91, ProjectID: 1, TemplateID: 3, Status: task_logger.TaskSuccessStatus}
	result := releaseReport{SchemaVersion: 1, OperationID: op.ID, TaskID: 90, DeviceID: d.ID, HostAlias: d.HostAlias, State: "succeeded", BinarySHA256: p.Manifest.BinarySHA256, Acceptance: map[string]any{"binary_sha256": p.Manifest.BinarySHA256}, Stages: []rcd.ReleaseStage{}}
	path := rcd.InspectionPath(root, 1, 91)
	os.MkdirAll(filepath.Dir(path), 0700)
	save := func() {
		raw, _ := json.Marshal(result)
		report := rcd.InspectionReport{SchemaVersion: 1, DeviceID: d.ID, TaskID: 91, HostAlias: d.HostAlias, Probes: []rcd.RuntimeProbe{{SchemaVersion: 1, Component: "manager", Processes: []rcd.RuntimeProcess{{Component: "manager"}}}, {SchemaVersion: 1, Component: "lower-stack", Processes: []rcd.RuntimeProcess{{Component: "lower"}, {Component: "agent"}}}}, ReleaseInfo: &rcd.ReleaseInspection{SchemaVersion: 1, ReleaseReport: raw}}
		blob, _ := json.Marshal(report)
		if e := os.WriteFile(path, blob, 0600); e != nil {
			t.Fatal(e)
		}
	}
	result.TaskID = 89
	save()
	collectRCDReadback(s, task, d)
	if x, _ := repo.Device(ctx, 1, d.ID); x.ActiveOperationID == "" {
		t.Fatal("mismatched original task released lock")
	}
	result.TaskID = 90
	save()
	s.task.Status = task_logger.TaskRunningStatus
	collectRCDReadback(s, task, d)
	if x, _ := repo.Device(ctx, 1, d.ID); x.ActiveOperationID == "" {
		t.Fatal("running native task released lock")
	}
	s.task.Status = task_logger.TaskFailStatus
	collectRCDReadback(s, task, d)
	got, _ := repo.Operation(ctx, 1, op.ID)
	if got.State != "succeeded" {
		t.Fatal(got)
	}
	if x, _ := repo.Device(ctx, 1, d.ID); x.ActiveOperationID != "" {
		t.Fatal("confirmed completed release stayed locked")
	}
	revision := got.Revision
	collectRCDReadback(s, task, d)
	got, _ = repo.Operation(ctx, 1, op.ID)
	if got.Revision != revision {
		t.Fatal("duplicate readback changed terminal result")
	}
	second, _, _, err := repo.CreateOperation(ctx, 1, "second", req, "test", 1)
	if err != nil {
		t.Fatal("next release remains blocked", err)
	}
	if err = repo.AttachTask(ctx, 1, second.ID, 92); err != nil {
		t.Fatal(err)
	}
	failedTask := db.Task{ID: 92, ProjectID: 1, Status: task_logger.TaskFailStatus}
	recordRCDLaunchFailure(s, failedTask, errors.New("open /dev/pts/0: permission denied"))
	if x, _ := repo.Device(ctx, 1, d.ID); x.ActiveOperationID != second.ID {
		t.Fatal("text error released reservation")
	}
	recordRCDLaunchFailure(s, failedTask, &db_lib.AnsibleLaunchError{Err: errors.New("open /dev/pts/0: permission denied")})
	confirmed, _ := repo.Operation(ctx, 1, second.ID)
	if confirmed.State != "failed_before_install" || len(confirmed.Targets[0].Stages) != 1 {
		t.Fatal(confirmed)
	}
	if x, _ := repo.Device(ctx, 1, d.ID); x.ActiveOperationID != "" {
		t.Fatal("known prelaunch failure stayed locked")
	}
	CollectRCDOperation(s, failedTask)
	after, _ := repo.Operation(ctx, 1, second.ID)
	if after.Revision != confirmed.Revision {
		t.Fatal("collector overwrote known launch failure")
	}
}
