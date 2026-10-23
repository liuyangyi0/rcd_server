// SPDX-License-Identifier: MIT
package sql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
	"testing"
)

func TestRCDHostSetupIdempotencyAndAddressLock(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	req := rcd.HostSetupRequest{Name: "fixture", Address: "2001:db8:0:0::10", SSHPort: 22, SSHUser: "operator", SSHKeyID: 4, ServiceAccount: "operator"}
	s, created, e := repo.CreateHostSetup(ctx, 1, req, "fixture-once", "operator", 1)
	if e != nil || !created || s.Request.Address != "2001:db8::10" {
		t.Fatal(s, e)
	}
	again, created, e := repo.CreateHostSetup(ctx, 1, req, "fixture-once", "operator", 1)
	if e != nil || created || again.ID != s.ID {
		t.Fatal("idempotency", again, e)
	}
	changed := req
	changed.Name = "different"
	if _, _, e = repo.CreateHostSetup(ctx, 1, changed, "fixture-once", "operator", 1); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("same key different content", e)
	}
	port := req
	port.SSHPort = 2222
	if _, _, e = repo.CreateHostSetup(ctx, 1, port, "fixture-other", "operator", 1); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("same physical host different SSH port", e)
	}
	s.SetResult("unknown", "missing response")
	s, e = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = repo.ActiveHostSetup(ctx, 1, req.Address); e != nil {
		t.Fatal("unknown released lock", e)
	}
	revision := s.Revision
	s.SetResult("failed", "failed before any install")
	s, e = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = repo.UpdateHostSetup(ctx, 1, s, revision); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("stale CAS", e)
	}
	if _, e = repo.ActiveHostSetup(ctx, 1, req.Address); !errors.Is(e, rcd.ErrNotFound) {
		t.Fatal("known preinstall failure retained lock", e)
	}
	if _, created, e = repo.CreateHostSetup(ctx, 1, port, "fixture-new", "operator", 1); e != nil || !created {
		t.Fatal("new explicit request after known terminal", e)
	}
}

func TestRCDHostSetupOriginalAdmissionAndTaskSurviveReadOnlyQuery(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	repo, ctx := store.RCDRepository(), context.Background()
	s, _, err := repo.CreateHostSetup(ctx, 1, rcd.HostSetupRequest{Name: "host", Address: "192.0.2.10", SSHPort: 22, SSHUser: "root", SSHKeyID: 1, ServiceAccount: "operator"}, "fixture-admission-recovery", "operator", 1)
	if err != nil {
		t.Fatal(err)
	}
	s.Install = &rcd.HostSetupInstall{PackageID: "original", BinarySHA256: "original-hash"}
	s.StartPhase("install", "installation_queued")
	admission := s.Install.AdmissionID
	if len(admission) != 64 {
		t.Fatal("missing immutable admission", s)
	}
	s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	s.SetResult("unknown", "task association lost")
	s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	stale := s
	s, err = repo.RecoverHostSetupTask(ctx, 1, s, 77)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.RecoverHostSetupTask(ctx, 1, stale, 78); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("recovery bypassed CAS", err)
	}
	s.ReconcileRevision = s.Revision
	s.StartPhase("reconcile", "reconcile_queued")
	s, err = repo.UpdateHostSetup(ctx, 1, s, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.AttachHostSetupTask(ctx, 1, s.ID, 79); err != nil {
		t.Fatal(err)
	}
	got, err := repo.HostSetup(ctx, 1, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InstallationTaskID() != 77 || got.TaskID != 79 || got.Install.AdmissionID != admission || got.ReconcileRevision != s.ReconcileRevision {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(got)
	if bytes.Contains(raw, []byte(admission)) || bytes.Contains(raw, []byte(got.Nonce)) || bytes.Contains(raw, []byte("reconcile_revision")) {
		t.Fatal("private admission leaked", string(raw))
	}
	if _, err = repo.ActiveHostSetup(ctx, 1, s.Request.Address); err != nil {
		t.Fatal("read-only query lost host reservation", err)
	}
}

func TestRCDHostSetupAdmissionAgainstExistingRelease(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	d, e := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "host", Address: "192.0.2.10", HostAlias: "host", InventoryID: 1, TemplateID: 2}, "operator")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = store.Sql().Db.Exec("insert into rcd_operation_lock(project_id,device_id,operation_id) values(1,?,'existing-release')", d.ID); e != nil {
		t.Fatal(e)
	}
	req := rcd.HostSetupRequest{Name: "host", Address: d.Address, SSHPort: 22, SSHUser: "operator", SSHKeyID: 1, ServiceAccount: "operator"}
	if _, _, e = repo.CreateHostSetup(ctx, 1, req, "fixture-release", "operator", 1); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("release occupancy bypass", e)
	}
}

func TestRCDHostSetupFirstInstallChecksAdapterVersion(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	repo := store.RCDRepository()
	packages, e := rcd.NewPackages(repo, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	pkg, _, e := packages.Upload(context.Background(), 1, "operator", bytes.NewReader(testutil.Package(t, "manager", "first", "test")))
	if e != nil {
		t.Fatal(e)
	}
	if e = rcd.ValidateFirstInstallPackage(pkg.Manifest); e != nil {
		t.Fatal("supported package rejected", e)
	}
	newer := pkg.Manifest
	newer.Compatibility.AdapterMinVersion = "9.0.0"
	if !errors.Is(rcd.ValidateFirstInstallPackage(newer), rcd.ErrInvalid) {
		t.Fatal("newer adapter requirement bypassed")
	}
}
