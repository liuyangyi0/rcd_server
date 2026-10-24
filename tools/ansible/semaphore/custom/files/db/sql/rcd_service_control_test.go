// SPDX-License-Identifier: MIT
package sql

import (
	"context"
	"errors"
	"github.com/semaphoreui/semaphore/services/rcd"
	"strings"
	"testing"
	"time"
)

func TestRCDServiceControlWithoutPackageReservesHostAndPinsImage(t *testing.T) {
	s := InitConfigCreateTestStore()
	defer s.Close()
	if _, e := s.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	r := s.RCDRepository()
	ctx := context.Background()
	d, e := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 5, TemplateID: 10}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	d, e = r.BindControlTemplate(ctx, 1, d.ID, d.Revision, 21)
	if e != nil {
		t.Fatal(e)
	}
	digest := strings.Repeat("a", 64)
	obs := []rcd.ComponentObservation{}
	for _, c := range []string{"lower", "manager", "agent"} {
		obs = append(obs, rcd.ComponentObservation{Component: c, State: "running", DiskSHA256: digest, RunningSHA256: digest})
	}
	if e = r.AcceptObservation(ctx, 1, d.ID, 1, time.Now(), obs, ""); e != nil {
		t.Fatal(e)
	}
	req := rcd.OperationRequest{Kind: "service_control", Component: "manager", Action: "restart", CommandsPaused: true, Targets: []rcd.OperationTarget{{DeviceID: d.ID, Revision: d.Revision}}}
	for _, component := range []string{"lower", "agent"} {
		bad := req
		bad.Component = component
		if _, e = r.PlanOperation(ctx, 1, bad); !errors.Is(e, rcd.ErrInvalid) {
			t.Fatal(component, e)
		}
	}
	bad := req
	bad.PackageID = "invented"
	if _, e = r.PlanOperation(ctx, 1, bad); !errors.Is(e, rcd.ErrInvalid) {
		t.Fatal(e)
	}
	bad = req
	bad.CommandsPaused = false
	if _, e = r.PlanOperation(ctx, 1, bad); !errors.Is(e, rcd.ErrInvalid) {
		t.Fatal(e)
	}
	o, nonce, created, e := r.CreateOperation(ctx, 1, "safe-restart", req, "admin", 1)
	if e != nil || !created || nonce == "" || o.TemplateID != 21 || o.Targets[0].ExpectedBinarySHA256 != digest || o.Request.PackageID != "" {
		t.Fatal(o, e)
	}
	again, _, created, e := r.CreateOperation(ctx, 1, "safe-restart", req, "admin", 1)
	if e != nil || created || again.ID != o.ID {
		t.Fatal(again, e)
	}
	if _, _, _, e = r.CreateOperation(ctx, 1, "other", req, "admin", 1); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("duplicate execution", e)
	}
	o.State = "unknown"
	if e = r.UpdateOperation(ctx, 1, o, o.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e = r.BindControlTemplate(ctx, 1, d.ID, 1, 22); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("binding while unknown", e)
	}
}
