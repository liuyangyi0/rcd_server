// SPDX-License-Identifier: MIT
package sql

import (
	"context"
	"errors"
	"testing"

	"github.com/semaphoreui/semaphore/services/rcd"
)

func TestRCDMachineBindingPreservesIdentityAndCAS(t *testing.T) {
	s := InitConfigCreateTestStore()
	defer s.Close()
	if _, e := s.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'RCD')"); e != nil {
		t.Fatal(e)
	}
	r, ctx := s.RCDRepository(), context.Background()
	d, e := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "46", Address: "192.168.10.46", HostAlias: "rcd46", InventoryID: 5, TemplateID: 10}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	m := rcd.MachineBinding{Name: "RCD 46 下级", OPCEndpoint: "opc.tcp://192.168.10.46:4841", AgentURL: "http://192.168.10.46:8090", AgentInstanceID: "stable-agent46", GatewayInstanceID: "stable-gateway46", ManagerInstanceID: "stable-manager46"}
	updated, e := r.BindMachine(ctx, 1, d.ID, d.Revision, m)
	if e != nil || updated.Name != m.Name || updated.ID != d.ID || updated.InventoryID != 5 {
		t.Fatal(updated, e)
	}
	if _, e = r.BindMachine(ctx, 1, d.ID, d.Revision, m); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("stale revision", e)
	}
	bad := m
	bad.AgentInstanceID = "other"
	if _, e = r.BindMachine(ctx, 1, d.ID, updated.Revision, bad); !errors.Is(e, rcd.ErrInvalid) {
		t.Fatal("changed stable identity", e)
	}
	bad = m
	bad.AgentURL = "http://192.168.10.30:8090"
	if _, e = r.BindMachine(ctx, 1, d.ID, updated.Revision, bad); !errors.Is(e, rcd.ErrInvalid) {
		t.Fatal("different target address", e)
	}
	other, e := r.AddDevice(ctx, 1, rcd.DeviceBinding{Name: "30", Address: "192.168.10.30", HostAlias: "rcd30", InventoryID: 5, TemplateID: 11}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	bad = m
	bad.OPCEndpoint = "opc.tcp://192.168.10.30:4841"
	bad.AgentURL = "http://192.168.10.30:8090"
	if _, e = r.BindMachine(ctx, 1, other.ID, other.Revision, bad); !errors.Is(e, rcd.ErrConflict) {
		t.Fatal("duplicate physical identity", e)
	}
}
