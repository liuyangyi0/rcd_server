// SPDX-License-Identifier: MIT
package sql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDApplicationsRegistrationIdempotencyIsolationAndPackageAdmission(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two')"); err != nil {
		t.Fatal(err)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	packages, err := rcd.NewPackages(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob := testutil.Package(t, "my-app", "release-1", "inert ELF; do not execute")
	if _, _, err = packages.Upload(ctx, 1, "operator", bytes.NewReader(blob)); !errors.Is(err, rcd.ErrInvalid) {
		t.Fatal("unregistered component admitted", err)
	}
	req := rcd.ApplicationRequest{Component: "my-app", Name: "测试软件", Arguments: []string{}, GracefulShutdownConfirmed: true}
	a, created, err := repo.RegisterApplication(ctx, 1, req, "operator")
	if err != nil || !created || a.CreatedBy != "operator" {
		t.Fatal(a, created, err)
	}
	again, created, err := repo.RegisterApplication(ctx, 1, req, "second")
	if err != nil || created || again.DefinitionSHA256 != a.DefinitionSHA256 || again.CreatedBy != "operator" {
		t.Fatal(again, created, err)
	}
	changed := req
	changed.Arguments = []string{"--changed"}
	if _, _, err = repo.RegisterApplication(ctx, 1, changed, "operator"); !errors.Is(err, rcd.ErrConflict) {
		t.Fatal("registration overwrite", err)
	}
	if _, err = repo.GetApplication(ctx, 2, req.Component); !errors.Is(err, rcd.ErrNotFound) {
		t.Fatal("project data leaked", err)
	}
	list, err := repo.ListApplications(ctx, 1)
	if err != nil || len(list) != 1 || list[0].Component != "my-app" {
		t.Fatal(list, err)
	}
	if _, created, err = packages.Upload(ctx, 1, "operator", bytes.NewReader(blob)); err != nil || !created {
		t.Fatal("registered package rejected", err)
	}
	if _, _, err = packages.Upload(ctx, 1, "operator", bytes.NewReader(testutil.Package(t, "my-app", "same-binary-new-identity", "inert"))); !errors.Is(err, rcd.ErrBinaryIdentityConflict) {
		t.Fatal("ambiguous runtime identity admitted", err)
	}
	if _, created, err = packages.Upload(ctx, 1, "operator", bytes.NewReader(blob)); err != nil || created {
		t.Fatal("original package lost idempotency", err)
	}
	if _, _, err = packages.Upload(ctx, 2, "operator", bytes.NewReader(blob)); !errors.Is(err, rcd.ErrInvalid) {
		t.Fatal("registration not project scoped", err)
	}
	for i := 1; i < 32; i++ {
		next := req
		next.Component = fmt.Sprintf("app-%d", i)
		if _, _, err = repo.RegisterApplication(ctx, 1, next, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	overflow := req
	overflow.Component = "overflow"
	if _, _, err = repo.RegisterApplication(ctx, 1, overflow, "operator"); !errors.Is(err, rcd.ErrInvalid) {
		t.Fatal("33rd software accepted", err)
	}
	if _, created, err = repo.RegisterApplication(ctx, 1, req, "operator"); err != nil || created {
		t.Fatal("idempotency blocked at limit", err)
	}
}

func TestRCDGenericBinaryIdentityIsAtomicAcrossUploaders(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, e := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	r := store.RCDRepository()
	ctx := context.Background()
	if _, _, e := r.RegisterApplication(ctx, 1, rcd.ApplicationRequest{Component: "my-app", Name: "App", Arguments: []string{}, GracefulShutdownConfirmed: true}, "admin"); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	uploaders := []*rcd.Packages{}
	for i := 0; i < 2; i++ {
		u, e := rcd.NewPackages(r, root)
		if e != nil {
			t.Fatal(e)
		}
		uploaders = append(uploaders, u)
	}
	// Inert ELF fixtures have identical bytes but distinct claimed build IDs.
	blobs := [][]byte{testutil.Package(t, "my-app", "build-a", "inert"), testutil.Package(t, "my-app", "build-b", "inert")}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range uploaders {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, _, e := uploaders[index].Upload(ctx, 1, "admin", bytes.NewReader(blobs[index]))
			results <- e
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, rejected := 0, 0
	for e := range results {
		if e == nil {
			accepted++
		} else if errors.Is(e, rcd.ErrBinaryIdentityConflict) {
			rejected++
		} else {
			t.Fatal(e)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	page, e := r.List(ctx, 1, rcd.Filter{Component: "my-app", Page: 1, PageSize: 20})
	if e != nil || page.Total != 1 {
		t.Fatal(page, e)
	}
}
