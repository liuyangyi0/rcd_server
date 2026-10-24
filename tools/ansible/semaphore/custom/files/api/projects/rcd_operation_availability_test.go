// SPDX-License-Identifier: MIT
package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/semaphoreui/semaphore/api/helpers"
	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDPackageCompatibilityAndHistoryShowActiveFirstInstall(t *testing.T) {
	store := dbsql.InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	repo, ctx := store.RCDRepository(), context.Background()
	root := t.TempDir()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", root)
	packages, err := rcd.NewPackages(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := packages.Upload(ctx, 1, "admin", bytes.NewReader(testutil.Package(t, "manager", "initial", "inert")))
	if err != nil {
		t.Fatal(err)
	}
	d, err := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "host", Address: "192.0.2.10", HostAlias: "host", InventoryID: 5, TemplateID: 10, ReleaseTemplateID: 20}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	observed := []rcd.ComponentObservation{
		{Component: "manager", State: "running", DiskSHA256: p.Manifest.BinarySHA256, RunningSHA256: p.Manifest.BinarySHA256, RunningBuild: &rcd.BuildObservation{Component: "manager", Version: "1.0.0", Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.1.0", StateFormats: map[string]int{"manager_db": 1}}},
		{Component: "lower", State: "stopped"}, {Component: "agent", State: "stopped"},
	}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 1, time.Now(), observed, ""); err != nil {
		t.Fatal(err)
	}
	req := rcd.OperationRequest{PackageID: p.ID, PackageRevision: p.Revision, Action: "upgrade", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: d.Revision}}}
	o, _, _, err := repo.CreateOperation(ctx, 1, "completed-release", req, "admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.AttachTask(ctx, 1, o.ID, 100); err != nil {
		t.Fatal(err)
	}
	o, err = repo.Operation(ctx, 1, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	o.State = "succeeded"
	o.Targets[0].State = "succeeded"
	if err = repo.UpdateOperation(ctx, 1, o, o.Revision); err != nil {
		t.Fatal(err)
	}
	setup, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "host", Address: d.Address, SSHPort: 22, SSHUser: "operator", ServiceAccount: "operator", SSHKeyID: 1, BecomeKeyID: 2}, "active-first-install", "admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	c := rcdOperationsController{store: store, repo: repo}
	check := func(blocked bool) {
		t.Helper()
		request := httptest.NewRequest("GET", "/", nil)
		request = helpers.SetContextValue(request, "project", db.Project{ID: 1})
		request = mux.SetURLVars(request, map[string]string{"package_id": p.ID})
		response := httptest.NewRecorder()
		c.compatibility(response, request)
		var availability struct {
			Reasons map[string]string `json:"reasons"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &availability) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		reason := availability.Reasons[d.ID]
		if (reason != "") != blocked || (blocked && !strings.Contains(reason, "首次安装")) {
			t.Fatal("compatibility hides setup reservation", reason)
		}
		request = mux.SetURLVars(request, map[string]string{"device_id": d.ID, "component": "manager"})
		response = httptest.NewRecorder()
		c.history(response, request)
		var history struct {
			Total int `json:"total"`
			Items []struct {
				Reason string `json:"restore_disabled_reason"`
			} `json:"items"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &history) != nil || history.Total != 1 || len(history.Items) != 1 {
			t.Fatal(response.Code, response.Body.String())
		}
		if history.Items[0].Reason != reason {
			t.Fatal("history and upgrade availability disagree", history.Items[0].Reason, reason)
		}
	}
	check(true)
	setup.SetResult("failed", "stopped before mutation")
	if _, err = repo.UpdateHostSetup(ctx, 1, setup, setup.Revision); err != nil {
		t.Fatal(err)
	}
	check(false)
}
