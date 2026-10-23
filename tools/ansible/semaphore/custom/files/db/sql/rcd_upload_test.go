// SPDX-License-Identifier: MIT
package sql

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func TestRCDUploadIdempotencyAndCrashRecovery(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'test')"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s, err := rcd.NewPackages(store.RCDRepository(), root)
	if err != nil {
		t.Fatal(err)
	}
	blob := testutil.Package(t, "lower", "r1", "inert")
	ctx := context.Background()
	var wait sync.WaitGroup
	results := make(chan bool, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, created, err := s.Upload(ctx, 1, "operator", bytes.NewReader(blob))
			results <- created
			failures <- err
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	for ok := range results {
		if ok {
			created++
		}
	}
	if created != 1 {
		t.Fatal("duplicate accepted", created)
	}
	// Simulate exactly the disk-published / SQL-not-committed crash boundary.
	v, err := rcd.ValidateArchive(ctx, bytes.NewReader(testutil.Package(t, "lower", "r2", "orphan")), filepath.Join(root, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(v.Directory, filepath.Join(root, strconv.Itoa(1), v.ID)); err != nil {
		t.Fatal(err)
	}
	p, newRecord, err := s.Upload(ctx, 1, "operator", bytes.NewReader(testutil.Package(t, "lower", "r2", "orphan")))
	if err != nil || !newRecord || p.ID != v.ID {
		t.Fatal("orphan recovery", p, newRecord, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "incoming"))
	if err != nil || len(entries) != 0 {
		t.Fatal("staging cleanup", err, entries)
	}
}
