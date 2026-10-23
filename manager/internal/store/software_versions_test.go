package store

import (
	"context"
	"errors"
	"path/filepath"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
	"testing"
	"time"
)

func TestSoftwareVersionCacheReopensAndTracksSourceIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	r, err := Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err = r.CreateGroup(ctx, domain.Group{ID: "g", Name: "g"}); err != nil {
		t.Fatal(err)
	}
	source, err := r.CreateSource(ctx, domain.Source{ID: "s", Name: "s", GroupID: "g", Endpoint: "opc.tcp://127.0.0.1:4841", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	info := buildinfo.ForComponent("lower")
	value := domain.SoftwareVersion{SourceID: source.ID, Component: "lower", Build: &info, ObservedAt: time.Now().UTC()}
	if err = r.SaveSoftwareVersion(ctx, source, domain.ProcessBinding{}, "physical-a", value); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	r, err = Open(ctx, path, PassthroughProtector{})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := r.LoadSoftwareVersion(ctx, "s", "lower", "physical-a")
	if err != nil || cached.Build == nil || !cached.Cached || !cached.ObservedAt.Equal(value.ObservedAt) {
		t.Fatalf("reopen: %+v %v", cached, err)
	}
	source.Name = "renamed"
	source, err = r.UpdateSource(ctx, source, nil, source.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.LoadSoftwareVersion(ctx, "s", "lower", "physical-a"); err != nil {
		t.Fatal("rename lost cache", err)
	}
	original := source
	source.Endpoint = "opc.tcp://127.0.0.2:4841"
	source, err = r.UpdateSource(ctx, source, nil, source.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.LoadSoftwareVersion(ctx, "s", "lower", "physical-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rebind retained old device: %v", err)
	}
	if err = r.SaveSoftwareVersion(ctx, original, domain.ProcessBinding{}, "physical-a", value); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale source reply accepted: %v", err)
	}
	if err = r.SaveSoftwareVersion(ctx, source, domain.ProcessBinding{}, "physical-b", value); err != nil {
		t.Fatal(err)
	}
	if err = r.DeleteSource(ctx, "s", source.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = r.LoadSoftwareVersion(ctx, "s", "lower", "physical-b"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted source retained: %v", err)
	}
}
