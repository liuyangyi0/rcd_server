// SPDX-License-Identifier: MIT
package sql

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDFirstInstallExcludesUpgradeAndServiceControlAdmission(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	r := store.RCDRepository()
	ctx := context.Background()
	d, err := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "host", Address: "192.0.2.10", HostAlias: "host", InventoryID: 5, TemplateID: 10, ReleaseTemplateID: 20}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	d, err = r.BindControlTemplate(ctx, 1, d.ID, d.Revision, 21)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	components := []rcd.ComponentObservation{{Component: "manager", State: "running", DiskSHA256: hash, RunningSHA256: hash, RunningBuild: &rcd.BuildObservation{Component: "manager", Version: "1.0.0", Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 1}}}}
	for _, component := range []string{"lower", "agent"} {
		components = append(components, rcd.ComponentObservation{Component: component, State: "stopped", DiskSHA256: hash})
	}
	if err = r.AcceptObservation(ctx, 1, d.ID, 1, time.Now(), components, ""); err != nil {
		t.Fatal(err)
	}
	packages, err := rcd.NewPackages(r, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := packages.Upload(ctx, 1, "admin", bytes.NewReader(testutil.Package(t, "manager", "release-1", "inert")))
	if err != nil {
		t.Fatal(err)
	}
	request := rcd.OperationRequest{PackageID: p.ID, PackageRevision: 1, Action: "upgrade", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: d.Revision}}}
	setupRequest := rcd.HostSetupRequest{Name: "host", Address: d.Address, SSHPort: 22, SSHUser: "operator", ServiceAccount: "operator", SSHKeyID: 1, BecomeKeyID: 2}
	setup, _, err := r.CreateHostSetup(ctx, 1, setupRequest, "first-setup", "admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.PlanOperation(ctx, 1, request); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("upgrade ignored active setup", err)
	}
	control := rcd.OperationRequest{Kind: "service_control", Component: "manager", Action: "restart", CommandsPaused: true, Targets: request.Targets}
	if _, err = r.PlanOperation(ctx, 1, control); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("service control ignored active setup", err)
	}
	setup.SetResult("failed", "preflight failed before mutation")
	if _, err = r.UpdateHostSetup(ctx, 1, setup, setup.Revision); err != nil {
		t.Fatal(err)
	}
	// Both callers can pass their read-only precheck. The shared project-row
	// transaction lock still lets only one of the two incompatible reservations win.
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, _, e := r.CreateOperation(ctx, 1, "upgrade-race", request, "admin", 1)
		results <- e
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _, e := r.CreateHostSetup(ctx, 1, setupRequest, "setup-race", "admin", 1)
		results <- e
	}()
	close(start)
	wg.Wait()
	close(results)
	succeeded, conflicts := 0, 0
	for e := range results {
		if e == nil {
			succeeded++
		} else if errors.Is(e, rcd.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Fatalf("expected one reservation, got success=%d conflict=%d", succeeded, conflicts)
	}
}
