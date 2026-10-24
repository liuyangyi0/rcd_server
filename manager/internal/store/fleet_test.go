package store

import (
	"context"
	"errors"
	"path/filepath"
	"rcd_manager/internal/domain"
	"testing"
)

func TestFleetBindingPreservesBusinessConfigurationAndRejectsIdentityDrift(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	r, e := Open(ctx, path, PassthroughProtector{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { r.Close() }()
	if _, e = r.CreateGroup(ctx, domain.Group{ID: "g", Name: "user-group"}); e != nil {
		t.Fatal(e)
	}
	src, e := r.CreateSource(ctx, domain.Source{ID: "stable-source", Name: "user-name", GroupID: "g", Endpoint: "opc.tcp://127.0.0.1:4841", Enabled: false, SecurityPolicy: "None", SecurityMode: "None", Username: "operator"}, "private")
	if e != nil {
		t.Fatal(e)
	}
	b, e := r.GetProcessBinding(ctx, src.ID)
	if e != nil {
		t.Fatal(e)
	}
	m := domain.FleetMachine{ID: "device-46", Revision: 1, Name: "RCD 46", Address: "192.168.10.46", OPCEndpoint: "opc.tcp://192.168.10.46:4841", AgentURL: "http://192.168.10.46:8090", WebURL: "http://192.168.10.46:8087", AgentInstanceID: "real-lower", GatewayInstanceID: "physical46", ManagerInstanceID: "manager46"}
	updated, e := r.ApplyFleetMachine(ctx, "platform", src, b, m)
	if e != nil {
		t.Fatal(e)
	}
	if updated.ID != src.ID || updated.GroupID != src.GroupID || updated.Enabled || updated.Username != src.Username || updated.SecurityPolicy != src.SecurityPolicy || updated.Name != m.Name {
		t.Fatal("business configuration modified", updated)
	}
	_, secret, e := r.SourceCredential(ctx, src.ID)
	if e != nil || secret != "private" {
		t.Fatal("credentials changed", e)
	}
	b, e = r.GetProcessBinding(ctx, src.ID)
	if e != nil || b.Revision != 1 || b.SourceRevision != updated.Revision || b.AgentURL != m.AgentURL {
		t.Fatal(b, e)
	}
	bad := m
	bad.AgentInstanceID = "different"
	bad.Revision++
	if _, e = r.ApplyFleetMachine(ctx, "platform", updated, b, bad); e == nil {
		t.Fatal("identity drift accepted")
	}
	bad = m
	bad.Name = "same-revision-different-content"
	if _, e = r.ApplyFleetMachine(ctx, "platform", updated, b, bad); e == nil {
		t.Fatal("mutable same revision accepted")
	}
	if _, e = r.ApplyFleetMachine(ctx, "platform", src, b, m); !errors.Is(e, domain.ErrRevisionConflict) {
		t.Fatal("source CAS lost", e)
	}
	snap := domain.FleetSnapshot{SchemaVersion: 1, Machines: []domain.FleetMachine{m}}
	at, e := r.SaveFleetSnapshot(ctx, "platform", snap)
	if e != nil {
		t.Fatal(e)
	}
	r.Close()
	r, e = Open(ctx, path, PassthroughProtector{})
	if e != nil {
		t.Fatal(e)
	}
	cached, stamp, e := r.LoadFleetSnapshot(ctx, "platform")
	if e != nil || len(cached.Machines) != 1 || !stamp.Equal(at) {
		t.Fatal("offline cache lost", e)
	}
	binding, owner, e := r.FleetBinding(ctx, src.ID)
	if e != nil || owner != "platform" || binding.Machine.ID != m.ID {
		t.Fatal(binding, e)
	}
}
