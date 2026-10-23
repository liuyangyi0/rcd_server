// SPDX-License-Identifier: MIT
package sql

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/services/rcd"
)

func TestRCDNativeMigrationAndRepository(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if err := db.Migrate(store, nil); err != nil {
		t.Fatal("migration must be idempotent", err)
	}
	for _, id := range []int{1, 2} {
		if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(?,CURRENT_TIMESTAMP,?)", id, "RCD"); err != nil {
			t.Fatal(err)
		}
	}
	r := store.RCDRepository()
	ctx := context.Background()
	dirty := true
	notes := "test"
	zero := 0
	one := 1
	m := rcd.Manifest{SchemaVersion: 1, Component: "manager", Version: "1.0.0", ReleaseID: "r-1", Commit: strings.Repeat("a", 40), SourceSHA256: strings.Repeat("b", 64), Dirty: &dirty, BuiltAt: "2026-10-08T00:00:00Z", Target: "linux/amd64", BinarySHA256: strings.Repeat("c", 64), BinarySize: 64, ReleaseNotes: &notes, Compatibility: rcd.Compatibility{AdapterMinVersion: "1.0.0", StateFormats: map[string]rcd.SchemaRange{"manager_db": {Min: &zero, Max: &one}}, ComponentMinVersions: map[string]string{}}}
	v := rcd.ValidatedArchive{Manifest: m, ArchiveSHA256: strings.Repeat("d", 64), ArchiveSize: 123}
	p, err := r.Insert(ctx, 1, v, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if p.UploadedBy != "operator" || p.Revision != 1 {
		t.Fatal(p)
	}
	if _, err = r.Get(ctx, 2, p.ID); !errors.Is(err, rcd.ErrNotFound) {
		t.Fatal("project isolation", err)
	}
	if _, err = r.Insert(ctx, 1, v, "operator"); err == nil {
		t.Fatal("duplicate release accepted")
	}
	if _, err = r.Insert(ctx, 2, v, "other"); err != nil {
		t.Fatal("cross-project independent package", err)
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); _, err := r.SetEnabled(ctx, 1, p.ID, 1, false, "operator"); results <- err }()
	}
	wait.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, rcd.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("CAS failed", successes, conflicts)
	}
	page, err := r.List(ctx, 1, rcd.Filter{Page: 1, PageSize: 20, Component: "manager"})
	if err != nil || page.Total != 1 || page.TotalPages != 1 || page.Items[0].Enabled {
		t.Fatal(page, err)
	}
	page, err = r.List(ctx, 1, rcd.Filter{Page: 1, PageSize: 20, Search: "%"})
	if err != nil || page.Total != 0 {
		t.Fatal("literal search", page, err)
	}
	var events int
	if err = store.Sql().Db.QueryRow("select count(*) from rcd_package_event where project_id=1").Scan(&events); err != nil || events != 2 {
		t.Fatal("audit transaction", events, err)
	}
}

func TestRCDMigrationPreservesExistingProject(t *testing.T) {
	version := "2.19.12"
	store := InitConfigCreateTestStoreAt(&version)
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(44,CURRENT_TIMESTAMP,'keep-existing-project')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(store, nil); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := store.Sql().Db.QueryRow("select name from project where id=44").Scan(&name); err != nil || name != "keep-existing-project" {
		t.Fatal("existing project lost", name, err)
	}
	page, err := store.RCDRepository().List(context.Background(), 44, rcd.Filter{Page: 1, PageSize: 20})
	if err != nil || page.Total != 0 {
		t.Fatal(page, err)
	}
}
