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

func TestRCDOperationIdempotencyCASOrderingAndHostExclusion(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD'),(2,CURRENT_TIMESTAMP,'other')"); err != nil {
		t.Fatal(err)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	packages, err := rcd.NewPackages(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := packages.Upload(ctx, 1, "operator", bytes.NewReader(testutil.Package(t, "manager", "r1", "test")))
	if err != nil {
		t.Fatal(err)
	}
	devices := []rcd.Device{}
	for i, ip := range []string{"192.168.10.46", "192.168.10.30", "192.168.10.58"} {
		d, e := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: ip, Address: ip, HostAlias: []string{"rcd46", "rcd30", "rcd58"}[i], InventoryID: 5, TemplateID: 10 + i, ReleaseTemplateID: 20}, "operator")
		if e != nil {
			t.Fatal(e)
		}
		components := []rcd.ComponentObservation{}
		for _, name := range []string{"lower", "manager", "agent"} {
			components = append(components, rcd.ComponentObservation{Component: name, State: "running", RunningBuild: &rcd.BuildObservation{Component: name, Version: "1.0.0", Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 1}}})
		}
		if e = repo.AcceptObservation(ctx, 1, d.ID, 1, time.Now().UTC(), components, ""); e != nil {
			t.Fatal(e)
		}
		devices = append(devices, d)
	}
	req := rcd.OperationRequest{PackageID: p.ID, PackageRevision: 1, Action: "upgrade", CommandsPaused: true}
	for _, d := range devices {
		req.Targets = append(req.Targets, rcd.OperationTarget{DeviceID: d.ID, Revision: 1})
	}
	o, nonce, created, err := repo.CreateOperation(ctx, 1, "once", req, "operator", 1)
	if err != nil || !created || nonce == "" || o.Targets[0].Address != "192.168.10.30" || o.Targets[1].Address != "192.168.10.58" {
		t.Fatal(o, created, err)
	}
	again, nonce2, created, err := repo.CreateOperation(ctx, 1, "once", req, "operator", 1)
	if err != nil || created || nonce2 != "" || again.ID != o.ID {
		t.Fatal(again, created, err)
	}
	if _, _, _, err = repo.CreateOperation(ctx, 1, "another", req, "operator", 1); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("host lock", err)
	}
	if _, err = repo.AuthorizeSubmission(ctx, 1, o.ID, "wrong"); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("nonce", err)
	}
	if _, err = repo.AuthorizeSubmission(ctx, 1, o.ID, nonce); err != nil {
		t.Fatal(err)
	}
	if err = repo.AttachTask(ctx, 1, o.ID, 30); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.AuthorizeSubmission(ctx, 1, o.ID, nonce); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("second dispatch", err)
	}
	o, err = repo.OperationForTask(ctx, 1, 30)
	if err != nil || o.State != "queued" {
		t.Fatal(o, err)
	}
	old := o.Revision
	o.State = "running"
	if err = repo.UpdateOperation(ctx, 1, o, old); err != nil {
		t.Fatal(err)
	}
	o.State = "cancelled"
	if err = repo.UpdateOperation(ctx, 1, o, old); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("cancel raced execution", err)
	}
	o, _ = repo.Operation(ctx, 1, o.ID)
	o.State = "unknown"
	if err = repo.UpdateOperation(ctx, 1, o, o.Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = repo.CreateOperation(ctx, 1, "third", req, "operator", 1); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("unknown released host", err)
	}
	if _, err = repo.Operation(ctx, 2, o.ID); !errors.Is(err, rcd.ErrNotFound) {
		t.Fatal("project isolation", err)
	}
	o, _ = repo.Operation(ctx, 1, o.ID)
	o.State = "succeeded"
	for i := range o.Targets {
		o.Targets[i].State = "succeeded"
	}
	if err = repo.UpdateOperation(ctx, 1, o, o.Revision); err != nil {
		t.Fatal(err)
	}
	req.Action = "restore"
	if _, _, _, err = repo.CreateOperation(ctx, 1, "restore", req, "operator", 1); err != nil {
		t.Fatal("history restore", err)
	}
}

func TestRCDOperationRequiresPauseFreshBindingAndPackageRevision(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	packages, _ := rcd.NewPackages(repo, t.TempDir())
	p, _, err := packages.Upload(ctx, 1, "operator", bytes.NewReader(testutil.Package(t, "manager", "r1", "test")))
	if err != nil {
		t.Fatal(err)
	}
	d, err := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 5, TemplateID: 10, ReleaseTemplateID: 20}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	req := rcd.OperationRequest{PackageID: p.ID, PackageRevision: 1, Action: "upgrade", Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: 1}}}
	if _, err = repo.PlanOperation(ctx, 1, req); !errors.Is(err, rcd.ErrInvalid) {
		t.Fatal("pause", err)
	}
	req.CommandsPaused = true
	if _, err = repo.PlanOperation(ctx, 1, req); !errors.Is(err, rcd.ErrInvalid) {
		t.Fatal("never checked", err)
	}
	req.PackageRevision = 2
	if _, err = repo.PlanOperation(ctx, 1, req); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("package revision", err)
	}
}

func TestRCDSubmissionFailureOnlyReleasesProvenPreAdmissionRejection(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "pre_admission", false: "ambiguous"}[before], func(t *testing.T) {
			s := InitConfigCreateTestStore()
			defer s.Close()
			_, err := s.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')")
			if err != nil {
				t.Fatal(err)
			}
			r := s.RCDRepository()
			ctx := context.Background()
			p, _ := rcd.NewPackages(r, t.TempDir())
			pkg, _, err := p.Upload(ctx, 1, "operator", bytes.NewReader(testutil.Package(t, "manager", "r1", "test")))
			if err != nil {
				t.Fatal(err)
			}
			d, err := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 5, TemplateID: 10, ReleaseTemplateID: 20}, "operator")
			if err != nil {
				t.Fatal(err)
			}
			components := []rcd.ComponentObservation{}
			for _, name := range []string{"lower", "manager", "agent"} {
				components = append(components, rcd.ComponentObservation{Component: name, State: "running", RunningBuild: &rcd.BuildObservation{Component: name, Version: "1.0.0", Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 1}}})
			}
			if err := r.AcceptObservation(ctx, 1, d.ID, 1, time.Now().UTC(), components, ""); err != nil {
				t.Fatal(err)
			}
			req := rcd.OperationRequest{PackageID: pkg.ID, PackageRevision: 1, Action: "upgrade", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: 1}}}
			op, _, _, err := r.CreateOperation(ctx, 1, "original", req, "operator", 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.RecordSubmissionFailure(ctx, 1, op.ID, "invalid template", before); err != nil {
				t.Fatal(err)
			}
			op, _ = r.Operation(ctx, 1, op.ID)
			d, _ = r.Device(ctx, 1, d.ID)
			if before {
				if op.State != "failed_before_execution" || d.ActiveOperationID != "" {
					t.Fatal(op, d)
				}
				if _, _, _, err := r.CreateOperation(ctx, 1, "new-after-fix", req, "operator", 1); err != nil {
					t.Fatal(err)
				}
			} else if op.State != "unknown" || d.ActiveOperationID != op.ID {
				t.Fatal("ambiguous submission freed reservation", op, d)
			}
		})
	}
}
