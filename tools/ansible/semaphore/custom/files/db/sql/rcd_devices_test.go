// SPDX-License-Identifier: MIT
package sql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/services/rcd"
)

func TestRCDDeviceSnapshotsPersistAndDoNotRegress(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'one'),(2,CURRENT_TIMESTAMP,'two')"); err != nil {
		t.Fatal(err)
	}
	repo := store.RCDRepository()
	ctx := context.Background()
	binding := rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 5, TemplateID: 10, WebURL: "http://192.168.10.46:8087"}
	d, err := repo.AddDevice(ctx, 1, binding, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "never_checked" || len(d.Components) != 0 {
		t.Fatal(d)
	}
	if _, err = repo.Device(ctx, 2, d.ID); !errors.Is(err, rcd.ErrNotFound) {
		t.Fatal("project isolation", err)
	}
	if _, err = repo.AddDevice(ctx, 1, binding, "operator"); err == nil {
		t.Fatal("duplicate binding")
	}
	sample := []rcd.ComponentObservation{{Component: "manager", State: "stopped"}, {Component: "lower", State: "unknown"}, {Component: "agent", State: "stopped"}}
	now := time.Now().UTC()
	if err = repo.AcceptObservation(ctx, 1, d.ID, 10, now, sample, ""); err != nil {
		t.Fatal(err)
	}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 11, now, nil, "SSH failed"); err != nil {
		t.Fatal(err)
	}
	// Delayed success must not clear a newer error or replace its last-good sample.
	if err = repo.AcceptObservation(ctx, 1, d.ID, 9, now.Add(-time.Hour), sample, ""); err != nil {
		t.Fatal(err)
	}
	d, err = store.RCDRepository().Device(ctx, 1, d.ID)
	if err != nil || d.LastTaskID != 11 || d.State != "check_failed" || len(d.Components) != 3 || d.LastSuccessAt != now.Format(time.RFC3339Nano) {
		t.Fatal(d, err)
	}
	if err = repo.AcceptObservation(ctx, 1, d.ID, 12, now.Add(-time.Hour), sample, ""); err != nil {
		t.Fatal(err)
	}
	d, err = repo.Device(ctx, 1, d.ID)
	if err != nil || d.State != "stale" {
		t.Fatal(d, err)
	}
	binding.HostAlias = "all:*"
	if binding.Validate() == nil {
		t.Fatal("host expression")
	}
}
