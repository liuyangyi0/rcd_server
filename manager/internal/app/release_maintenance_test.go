package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"rcd_manager/internal/domain"
)

func TestManagerReleaseFencePersistsAndDrainsWithoutChangingReads(t *testing.T) {
	r := seededRuntime(t)
	r.options.DataDir = t.TempDir()
	if err := r.loadReleaseState("manager-test"); err != nil {
		t.Fatal(err)
	}
	if err := r.openSession("client"); err != nil {
		t.Fatal(err)
	}
	before := r.ReadSample("client", testKey("KKS001"))
	pending, err := r.routes.BeginCommand("client", "line", "COM1", "Write")
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.changeRelease(managerReleaseRequest{"enter", "manager-test", "release-1", 1})
	if err != nil || !v.Active || v.Drained || v.PendingCommands != 1 {
		t.Fatalf("enter: %+v %v", v, err)
	}
	status, err := r.WriteCommand(context.Background(), "client", testKey("COM1_Send"), "44 08")
	if err == nil || status != domain.StatusBadWaitingForResponse {
		t.Fatalf("new command admitted: %#x %v", status, err)
	}
	if after := r.ReadSample("client", testKey("KKS001")); after.StatusCode != before.StatusCode {
		t.Fatal("maintenance changed reads")
	}
	if _, err = r.changeRelease(managerReleaseRequest{"complete", "manager-test", "release-1", v.Revision}); err == nil {
		t.Fatal("completed before drain")
	}
	if _, err = r.routes.FinishCommand(pending.ID, domain.CommandFailed, domain.StatusBadTimeout, "test"); err != nil {
		t.Fatal(err)
	}
	r.releaseDispatches.Add(1)
	if r.releaseSnapshot().Drained {
		t.Fatal("dispatch ignored")
	}
	r.releaseDispatches.Add(-1)
	if !r.releaseSnapshot().Drained {
		t.Fatal("not drained")
	}
	if err = r.loadReleaseState("manager-test"); err != nil || !r.release.Active {
		t.Fatalf("restart fence lost: %v", err)
	}
	if _, err = r.changeRelease(managerReleaseRequest{"enter", "manager-test", "another", r.release.Revision}); err == nil {
		t.Fatal("new id bypassed fence")
	}
	v, err = r.changeRelease(managerReleaseRequest{"complete", "manager-test", "release-1", r.release.Revision})
	if err != nil || v.Active {
		t.Fatalf("complete: %+v %v", v, err)
	}
	if _, err = r.changeRelease(managerReleaseRequest{"enter", "manager-test", "release-1", v.Revision}); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("replay: %v", err)
	}
}

func TestManagerReleaseFenceRefusesCorruptStateAndFailsClosed(t *testing.T) {
	r := seededRuntime(t)
	r.options.DataDir = t.TempDir()
	if err := r.loadReleaseState("m"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.options.DataDir, "release-maintenance.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.loadReleaseState("m"); err == nil {
		t.Fatal("ignored broken fence")
	}
	r.options.DataDir = filepath.Join(r.options.DataDir, "missing")
	if _, err := r.changeRelease(managerReleaseRequest{"enter", "m", "job", 1}); err == nil || !r.release.Active {
		t.Fatalf("not fail closed: %v", err)
	}
}
