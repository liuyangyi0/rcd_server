// SPDX-License-Identifier: MIT
package sql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/services/rcd"
)

func TestRCDRegisteredApplicationExplicitControlRequiresRegistrationAndImage(t *testing.T) {
	store := InitConfigCreateTestStore()
	defer store.Close()
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); err != nil {
		t.Fatal(err)
	}
	r := store.RCDRepository()
	ctx := context.Background()
	d, err := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "host", Address: "192.0.2.10", HostAlias: "host", InventoryID: 5, TemplateID: 10}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	d, err = r.BindControlTemplate(ctx, 1, d.ID, d.Revision, 21)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	req := rcd.OperationRequest{Kind: "service_control", Component: "my-app", Action: "start", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: d.Revision}}}
	if _, err = r.PlanOperation(ctx, 1, req); !errors.Is(err, rcd.ErrNotFound) {
		t.Fatal("unregistered service control", err)
	}
	a, _, err := r.RegisterApplication(ctx, 1, rcd.ApplicationRequest{Component: "my-app", Name: "App", Arguments: []string{}, GracefulShutdownConfirmed: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	components := []rcd.ComponentObservation{}
	for _, component := range []string{"lower", "manager", "agent", "my-app"} {
		components = append(components, rcd.ComponentObservation{Component: component, State: "stopped", DiskSHA256: hash})
	}
	if err = r.AcceptObservation(ctx, 1, d.ID, 1, time.Now(), components, ""); err != nil {
		t.Fatal(err)
	}
	o, err := r.PlanOperation(ctx, 1, req)
	if err != nil || o.Application == nil || o.Application.DefinitionSHA256 != a.DefinitionSHA256 || o.Targets[0].ExpectedBinarySHA256 != hash {
		t.Fatal(o, err)
	}
	for _, action := range []string{"stop", "restart", "kill", "recover"} {
		bad := req
		bad.Action = action
		if _, err = r.PlanOperation(ctx, 1, bad); err == nil {
			t.Fatalf("accepted action %s for stopped service", action)
		}
	}
	bad := req
	bad.Component = "communicate"
	if _, err = r.PlanOperation(ctx, 1, bad); !errors.Is(err, rcd.ErrInvalid) {
		t.Fatal("communicate bypassed drain boundary", err)
	}
}
