package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	dbsql "github.com/semaphoreui/semaphore/db/sql"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/rcd"
	"github.com/semaphoreui/semaphore/services/rcd/testutil"
)

func resultProofFixture(t *testing.T, count int) (*readbackStore, rcd.Operation, db.Task, string) {
	t.Helper()
	store := dbsql.InitConfigCreateTestStore()
	t.Cleanup(store.Close)
	root := t.TempDir()
	t.Setenv("SEMAPHORE_RCD_PACKAGES_PATH", root)
	if _, err := store.Sql().Db.Exec("insert into project(id,created,name) values(1,CURRENT_TIMESTAMP,'proof')"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo := store.RCDRepository()
	packages, err := rcd.NewPackages(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := packages.Upload(ctx, 1, "proof", bytes.NewReader(testutil.Package(t, "manager", "proof-r1", "fixture")))
	if err != nil {
		t.Fatal(err)
	}
	req := rcd.OperationRequest{PackageID: p.ID, PackageRevision: 1, Action: "upgrade", CommandsPaused: true}
	for i := 0; i < count; i++ {
		addresses := []string{"192.168.10.30", "192.168.10.58", "192.168.10.46"}
		aliases := []string{"proof30", "proof58", "proof46"}
		d, err := repo.AddDevice(ctx, 1, rcd.DeviceBinding{Name: aliases[i], Address: addresses[i], HostAlias: aliases[i], InventoryID: 2, TemplateID: 3 + i, ReleaseTemplateID: 7}, "proof")
		if err != nil {
			t.Fatal(err)
		}
		observation := []rcd.ComponentObservation{
			{Component: "manager", State: "running", DiskSHA256: strings.Repeat("c", 64), RunningBuild: &rcd.BuildObservation{Target: "linux/amd64"}, Compatibility: &rcd.CompatibilityObservation{AdapterVersion: "1.0.0", StateFormats: map[string]int{"manager_db": 1}}},
			{Component: "lower", State: "stopped"}, {Component: "agent", State: "stopped"},
		}
		if err := repo.AcceptObservation(ctx, 1, d.ID, 1, time.Now().UTC(), observation, ""); err != nil {
			t.Fatal(err)
		}
		req.Targets = append(req.Targets, rcd.OperationTarget{DeviceID: d.ID, Revision: 1})
	}
	op, _, _, err := repo.CreateOperation(ctx, 1, "proof-once", req, "proof", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachTask(ctx, 1, op.ID, 90); err != nil {
		t.Fatal(err)
	}
	op, err = repo.Operation(ctx, 1, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range op.Targets {
		if target.PreviousBinarySHA256 != strings.Repeat("c", 64) || target.PreviousState != "running" {
			t.Fatal("prior image not pinned", target)
		}
	}
	task := db.Task{ID: 90, ProjectID: 1, Status: task_logger.TaskFailStatus}
	return &readbackStore{Store: store, repo: repo, task: task}, op, task, root
}

func resultReport(op rcd.Operation, task db.Task, index int, state string) releaseReport {
	target := op.Targets[index]
	r := releaseReport{SchemaVersion: 1, OperationID: op.ID, TaskID: task.ID, DeviceID: target.DeviceID, HostAlias: target.HostAlias, State: state}
	if state == "succeeded" {
		r.BinarySHA256 = op.Package.BinarySHA256
		r.Acceptance = map[string]any{"binary_sha256": r.BinarySHA256, "state": "running", "pid": float64(123), "opc_tcp_reachable": true}
	}
	if state == "recovered" {
		r.BinarySHA256 = target.PreviousBinarySHA256
		r.Acceptance = map[string]any{"binary_sha256": r.BinarySHA256, "pid": float64(123), "opc_tcp_reachable": true}
		r.Stages = []rcd.ReleaseStage{{Stage: "automatic_binary_recovery", State: "confirmed", StartedAt: "2026-10-13T01:00:00Z", EndedAt: "2026-10-13T01:00:01Z"}}
	}
	return r
}
func saveResultReport(t *testing.T, root string, task db.Task, report releaseReport) {
	t.Helper()
	dir := ReleaseReportDirectory(root, task.ProjectID, task.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, report.DeviceID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRCDRecoveryProofRetainsReservationUntilOriginalImageConfirmed(t *testing.T) {
	for _, scenario := range []string{"missing-all", "wrong-image", "missing-acceptance", "no-ready", "zero-pid", "fractional-pid", "unconfirmed-stage", "unfinished-time", "missing-prior"} {
		t.Run(scenario, func(t *testing.T) {
			s, op, task, root := resultProofFixture(t, 1)
			r := resultReport(op, task, 0, "recovered")
			switch scenario {
			case "missing-all":
				r.BinarySHA256 = ""
				r.Acceptance = nil
				r.Stages = nil
			case "wrong-image":
				r.BinarySHA256 = strings.Repeat("d", 64)
				r.Acceptance["binary_sha256"] = r.BinarySHA256
			case "missing-acceptance":
				r.Acceptance = nil
			case "no-ready":
				delete(r.Acceptance, "opc_tcp_reachable")
			case "zero-pid":
				r.Acceptance["pid"] = float64(0)
			case "fractional-pid":
				r.Acceptance["pid"] = float64(1.5)
			case "unconfirmed-stage":
				r.Stages[0].State = "intent"
			case "unfinished-time":
				r.Stages[0].EndedAt = ""
			case "missing-prior":
				op.Targets[0].PreviousBinarySHA256 = ""
				if err := s.repo.UpdateOperation(context.Background(), 1, op, op.Revision); err != nil {
					t.Fatal(err)
				}
			}
			saveResultReport(t, root, task, r)
			CollectRCDOperation(s, task)
			got, err := s.repo.Operation(context.Background(), 1, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.repo.Device(context.Background(), 1, r.DeviceID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "unknown" || d.ActiveOperationID != op.ID {
				t.Fatal("unverified recovery released reservation", got.State, d.ActiveOperationID)
			}
			// An original report with complete pinned evidence may be reconciled;
			// no new native task is submitted and no release is re-executed.
			if scenario == "missing-prior" {
				return
			}
			saveResultReport(t, root, task, resultReport(op, task, 0, "recovered"))
			CollectRCDOperation(s, task)
			got, _ = s.repo.Operation(context.Background(), 1, op.ID)
			d, _ = s.repo.Device(context.Background(), 1, r.DeviceID)
			if got.State != "recovered" || d.ActiveOperationID != "" {
				t.Fatal("valid original recovery not reconciled", got.State, d.ActiveOperationID)
			}
			revision := got.Revision
			CollectRCDOperation(s, task)
			got, _ = s.repo.Operation(context.Background(), 1, op.ID)
			if got.Revision != revision {
				t.Fatal("terminal result rewritten")
			}
		})
	}
}

func TestRCDRecoveryProofPreservesStoppedStateAndUsesComponentReadiness(t *testing.T) {
	s, op, task, _ := resultProofFixture(t, 1)
	r := resultReport(op, task, 0, "recovered")
	if !confirmedRCDRecovery(op, op.Targets[0], r) {
		t.Fatal("valid running proof refused")
	}
	op.Targets[0].PreviousState = "stopped"
	if confirmedRCDRecovery(op, op.Targets[0], r) {
		t.Fatal("stopped program reported as restored running")
	}
	r.Acceptance["pid"] = float64(0)
	r.Acceptance["state"] = "installed_stopped"
	if !confirmedRCDRecovery(op, op.Targets[0], r) {
		t.Fatal("confirmed stopped recovery refused")
	}
	op.Package.Component = "agent"
	op.Targets[0].PreviousState = "running"
	r.Acceptance["pid"] = float64(123)
	delete(r.Acceptance, "state")
	if confirmedRCDRecovery(op, op.Targets[0], r) {
		t.Fatal("Agent recovered with only OPC readiness")
	}
	r.Acceptance["agent_http_ready"] = true
	if !confirmedRCDRecovery(op, op.Targets[0], r) {
		t.Fatal("Agent HTTP proof refused")
	}
	_ = s
}

func TestRCDPartialResultKeepsPerDeviceTruthAndOnlyFreesConfirmedOperations(t *testing.T) {
	for _, scenario := range []struct {
		states []string
		want   string
		free   bool
	}{
		{[]string{"succeeded", "failed_before_install", "not_started"}, "partial_failed", true},
		{[]string{"succeeded", "recovered", "not_started"}, "partial_failed", true},
		{[]string{"succeeded", "unknown", "not_started"}, "unknown", false},
		{[]string{"succeeded", "needs_maintenance", "not_started"}, "unknown", false},
		{[]string{"failed_before_install", "not_started", "not_started"}, "failed_before_install", true},
		{[]string{"recovered", "not_started", "not_started"}, "recovered", true},
		{[]string{"succeeded", "succeeded", "succeeded"}, "succeeded", true},
	} {
		t.Run(strings.Join(scenario.states, "-"), func(t *testing.T) {
			s, op, task, root := resultProofFixture(t, 3)
			for i, state := range scenario.states {
				saveResultReport(t, root, task, resultReport(op, task, i, state))
			}
			CollectRCDOperation(s, task)
			got, err := s.repo.Operation(context.Background(), 1, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != scenario.want {
				t.Fatal("incorrect aggregate result", got.State, scenario.want)
			}
			for i, target := range got.Targets {
				if target.State != scenario.states[i] {
					t.Fatal("per-device result overwritten", target.State, scenario.states[i])
				}
				d, err := s.repo.Device(context.Background(), 1, target.DeviceID)
				if err != nil {
					t.Fatal(err)
				}
				if (d.ActiveOperationID == "") != scenario.free {
					t.Fatal("incorrect reservation", d.ActiveOperationID, scenario.free)
				}
			}
		})
	}
}
