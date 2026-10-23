// SPDX-License-Identifier: MIT
package sql

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDGenericFirstInstallEntersHistoryOnlyAfterVerifiedNativeStart(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	r := store.RCDRepository()
	ctx := context.Background()
	a, _, e := r.RegisterApplication(ctx, 1, rcd.ApplicationRequest{Component: "my-app", Name: "App", Arguments: []string{}, GracefulShutdownConfirmed: true}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	packages, e := rcd.NewPackages(r, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	p, _, e := packages.Upload(ctx, 1, "admin", bytes.NewReader(testutil.Package(t, "my-app", "initial-v1", "inert")))
	if e != nil {
		t.Fatal(e)
	}
	d, e := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "host", Address: "192.0.2.10", HostAlias: "host", InventoryID: 5, TemplateID: 10, ReleaseTemplateID: 20}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	d, e = r.BindControlTemplate(ctx, 1, d.ID, d.Revision, 21)
	if e != nil {
		t.Fatal(e)
	}
	s, _, e := r.CreateExistingFirstInstall(ctx, 1, d, rcd.HostSetupRequest{Name: "host", Address: d.Address, SSHPort: 22, SSHUser: "operator", ServiceAccount: "operator", SSHKeyID: 1, BecomeKeyID: 2}, rcd.HostSetupInstall{PackageID: p.ID, PackageRevision: p.Revision, Component: "my-app", ArchiveSHA256: p.ArchiveSHA256, BinarySHA256: p.Manifest.BinarySHA256, Application: &a}, "first-install-v1", "admin", 1, "linux/amd64")
	if e != nil {
		t.Fatal(e)
	}
	s.SetResult("installed_stopped", "")
	if _, e = r.UpdateHostSetup(ctx, 1, s, s.Revision); e != nil {
		t.Fatal(e)
	}
	observed := []rcd.ComponentObservation{{Component: "lower", State: "stopped"}, {Component: "manager", State: "stopped"}, {Component: "agent", State: "stopped"}, {Component: "my-app", State: "stopped", DiskSHA256: p.Manifest.BinarySHA256, DiskBuild: &rcd.BuildObservation{Component: "my-app", Version: "1.0.0", Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.1.0", StateFormats: map[string]int{}}}}
	if e = r.AcceptObservation(ctx, 1, d.ID, 1, time.Now(), observed, ""); e != nil {
		t.Fatal(e)
	}
	restore := rcd.OperationRequest{PackageID: p.ID, PackageRevision: p.Revision, Action: "restore", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: d.Revision}}}
	if _, e = r.PlanOperation(ctx, 1, restore); !errors.Is(e, rcd.ErrInvalid) {
		t.Fatal("stopped first install was claimed as verified", e)
	}
	start := rcd.OperationRequest{Kind: "service_control", Component: "my-app", Action: "start", CommandsPaused: true, Targets: restore.Targets}
	o, _, _, e := r.CreateOperation(ctx, 1, "explicit-first-start", start, "admin", 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.AttachTask(ctx, 1, o.ID, 101); e != nil {
		t.Fatal(e)
	}
	o, e = r.Operation(ctx, 1, o.ID)
	if e != nil {
		t.Fatal(e)
	}
	o.State = "succeeded"
	o.Targets[0].State = "succeeded"
	o.Targets[0].Acceptance = map[string]any{"state": "running", "pid": float64(123), "binary_sha256": p.Manifest.BinarySHA256}
	if e = r.UpdateOperation(ctx, 1, o, o.Revision); e != nil {
		t.Fatal(e)
	}
	history, e := r.Installations(ctx, 1, d.ID, "my-app")
	if e != nil || len(history) != 1 || history[0].PackageID != p.ID || history[0].OperationID != o.ID || history[0].Kind != "first_install_verified_start" {
		t.Fatal(history, e)
	}
	if _, e = r.PlanOperation(ctx, 1, restore); e != nil {
		t.Fatal("verified initial version cannot be restored", e)
	}
	// A later confirmed restart must not move or duplicate the first verified
	// installation. Real publish records, including repeat installs of the same
	// package, are installation history and must never be deduplicated by package.
	complete := func(key string, request rcd.OperationRequest, task int) rcd.Operation {
		t.Helper()
		next, _, _, err := r.CreateOperation(ctx, 1, key, request, "admin", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.AttachTask(ctx, 1, next.ID, task); err != nil {
			t.Fatal(err)
		}
		next, err = r.Operation(ctx, 1, next.ID)
		if err != nil {
			t.Fatal(err)
		}
		next.State = "succeeded"
		next.Targets[0].State = "succeeded"
		next.Targets[0].Acceptance = map[string]any{"state": "running", "pid": float64(124), "binary_sha256": p.Manifest.BinarySHA256}
		if err = r.UpdateOperation(ctx, 1, next, next.Revision); err != nil {
			t.Fatal(err)
		}
		return next
	}
	complete("explicit-later-start", start, 102)
	upgrade := restore
	upgrade.Action = "upgrade"
	firstRelease := complete("same-package-install-one", upgrade, 103)
	secondRelease := complete("same-package-install-two", upgrade, 104)
	history, e = r.Installations(ctx, 1, d.ID, "my-app")
	if e != nil || len(history) != 3 {
		t.Fatal("installation records lost or duplicated", history, e)
	}
	releases := map[string]bool{}
	for _, row := range history {
		if row.PackageID != p.ID {
			t.Fatal(row)
		}
		if row.Kind == "release" {
			releases[row.OperationID] = true
			continue
		}
		if row.Kind != "first_install_verified_start" || row.OperationID != o.ID || row.InstalledAt != o.CreatedAt {
			t.Fatal("first confirmation changed", row)
		}
	}
	if len(releases) != 2 || !releases[firstRelease.ID] || !releases[secondRelease.ID] {
		t.Fatal("repeat package installs were collapsed", history)
	}
}
