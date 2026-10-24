//go:build linux

package processagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Opt-in actual Rust/Go/OPC handshake. Every file/process belongs to TempDir,
// there are no CSV ports and no serial device is opened or command transmitted.
func TestMaintenanceRealRustInterop(t *testing.T) {
	binary := os.Getenv("RCD_MAINTENANCE_INTEROP_BINARY")
	if binary == "" {
		t.Skip("requires explicit isolated Rust binary")
	}
	root := t.TempDir()
	exe := filepath.Join(root, "rcd_server")
	input, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(exe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal(copyErr, closeErr)
	}
	arguments := []string{}
	for _, name := range []string{"config", "state", "pki", "log"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		arguments = append(arguments, "--"+name+"-dir", path)
	}
	config := `[server]
primary_ip = "127.0.0.1:11001"
secondary_ip = "127.0.0.1:11002"
run_on = "primary"
current_run = "primary"
[serial]
baud_rate = 28800
data_bits = "Eight"
parity = "None"
stop_bits = "One"
flow_control = "none"
[settings]
timeout = 50
poll_interval_ms = 5
command_retry_count = 3
parity_switch_delay_ms = 5
transmit_settle_guard_ms = 5
[calculation]
mode = "manager"
`
	if err := os.WriteFile(filepath.Join(root, "config", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// A fixture identity, not a discovery/write to any installed gateway database.
	db, err := sql.Open("sqlite", filepath.Join(root, "state", "command-journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE identity(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL); INSERT INTO identity VALUES(1,'maintenance-interop-gateway')")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	t.Setenv("RCD_OPCUA_PORT", strconv.Itoa(port))
	cfg := Config{InstanceID: "interop-agent", Executable: exe, WorkDir: root, DataDir: filepath.Join(root, "agent-state"), LogDir: filepath.Join(root, "agent-logs"),
		HTTPListen: "127.0.0.1:0", ReadyAddress: fmt.Sprintf("127.0.0.1:%d", port), AutoStart: true, RestartPolicy: "never", StartGrace: 100 * time.Millisecond,
		StartTimeout: 45 * time.Second, StopTimeout: 5 * time.Second, Arguments: arguments, MaintenanceEndpoint: fmt.Sprintf("opc.tcp://127.0.0.1:%d", port), GatewayInstanceID: "maintenance-interop-gateway"}
	cfg.HostReleaseDir = root
	if err := os.Mkdir(filepath.Join(root, ".rcd-host-control"), 0700); err != nil {
		t.Fatal(err)
	}
	s := newTestSupervisor(t, cfg)
	t.Cleanup(func() {
		if t.Failed() {
			text, _ := os.ReadFile(filepath.Join(cfg.LogDir, "process.log"))
			if len(text) > 12000 {
				text = text[len(text)-12000:]
			}
			t.Log(string(text))
		}
	})
	// A fresh test PKI can take longer to generate than the small helper's 8s
	// deadline. This does not alter installed service/device timeouts.
	awaitRust := func(agent *Supervisor, requestID string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if requestID != "" {
				if done, ok := agent.Operation(requestID); ok && done.Terminal() {
					if done.State != "succeeded" {
						t.Fatalf("%+v", done)
					}
					return
				}
			} else if snap := agent.Snapshot(); snap.State == "running" {
				return
			} else if snap.State == "failed" {
				t.Fatalf("%+v", snap)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("isolated Rust did not become ready: %+v", agent.Snapshot())
	}
	awaitRust(s, "")
	entered := enterMaintenance(t, s)
	if !entered.Job.Lower.Drained || entered.Job.FencingToken != "1" {
		t.Fatal(entered)
	}
	oldBoot := entered.Job.Lower.BootID
	restart := ownedRequest(s, "restart")
	if _, err := s.Submit(restart); err != nil {
		t.Fatal(err)
	}
	awaitRust(s, restart.RequestID)
	reconciled, err := s.MaintenanceAction(context.Background(), maintenanceReq(s, "reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled.Job.Lower.Active || reconciled.Job.Lower.BootID == oldBoot {
		t.Fatal("Rust restart lost its fence or boot identity")
	}
	_, err = s.maintenancePeer.Change(context.Background(), "leave_maintenance", "job-a", "1", oldBoot)
	if err == nil {
		t.Fatal("stale boot released maintenance")
	}
	// Agent restart must preserve the job and NOT automatically launch its child.
	s.Close()
	restored := newTestSupervisor(t, cfg)
	if restored.Snapshot().PID != 0 || restored.Maintenance().Job.Phase != "unknown" {
		t.Fatal("agent replayed an interrupted maintenance job")
	}
	start := ownedRequest(restored, "start")
	if _, err = restored.Submit(start); err != nil {
		t.Fatal(err)
	}
	awaitRust(restored, start.RequestID)
	_, err = restored.MaintenanceAction(context.Background(), maintenanceReq(restored, "reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := restored.MaintenanceAction(context.Background(), maintenanceReq(restored, "complete"))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Job.Phase != "completed" || completed.Job.Lower.Active {
		t.Fatal(completed)
	}
	t.Log("real Rust + agent: durable gate, owned restart, stale boot rejection, agent restart without replay, explicit restart and release passed; no serial devices")
	t.Run("ConfigurationGuardRealRust", func(t *testing.T) {
		path := filepath.Join(root, "config", "config.toml")
		previous, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(path, previous, 0600); err != nil {
				t.Error("restore isolated configuration fixture", err)
			}
		}()
		hash := func(raw []byte) string { value := sha256.Sum256(raw); return hex.EncodeToString(value[:]) }
		req := safeRequest(restored, "restart", "isolated-manager-46")
		guard, _ := json.Marshal(map[string]string{"config.toml": hash(previous)})
		req.ConfigurationGuard = string(guard)
		// Edit after the manager's initial version snapshot, before Agent closes
		// the gateway gate. The real OPC second check must reject the restart.
		next := append(append([]byte{}, previous...), []byte("\n# another manager saved this config\n")...)
		if err := os.WriteFile(path, next, 0600); err != nil {
			t.Fatal(err)
		}
		pid := restored.Snapshot().PID
		if _, err := restored.SubmitLifecycle(req); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(45 * time.Second)
		for {
			value, _ := restored.Operation(req.RequestID)
			if value.Terminal() {
				if value.State != "failed" || value.ErrorCode != "conflict" || restored.Snapshot().PID != pid {
					t.Fatalf("changed configuration restarted lower: %+v", value)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("configuration rejection did not finish")
			}
			time.Sleep(50 * time.Millisecond)
		}
		fresh := safeRequest(restored, "restart", "isolated-manager-58")
		guard, _ = json.Marshal(map[string]string{"config.toml": hash(next)})
		fresh.ConfigurationGuard = string(guard)
		if _, err := restored.SubmitLifecycle(fresh); err != nil {
			t.Fatal(err)
		}
		awaitRust(restored, fresh.RequestID)
		if restored.Snapshot().PID == pid {
			t.Fatal("matching configuration did not restart")
		}
	})
	t.Run("SafeLifecycleThreeManagerIdentities", func(t *testing.T) {
		for _, manager := range []string{"isolated-manager-30", "isolated-manager-58", "isolated-manager-46"} {
			for _, action := range []string{"stop", "start", "restart"} {
				req := safeRequest(restored, action, manager)
				if _, err := restored.SubmitLifecycle(req); err != nil {
					t.Fatal(manager, action, err)
				}
				awaitRust(restored, req.RequestID)
				if action == "stop" && !restored.Snapshot().ManualStopped {
					t.Fatal("manual stop not retained")
				}
				if action != "stop" {
					f, e := restored.maintenancePeer.Read(context.Background())
					if e != nil || f.Active || f.AdmissionBlocked {
						t.Fatal("readiness/admission not restored", f, e)
					}
				}
			}
		}
	})
	t.Run("DurableReleaseRunner", func(t *testing.T) {
		previousFence, err := strconv.ParseUint(restored.Maintenance().Job.FencingToken, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		// Exercise the real Python HTTP publisher against this same isolated
		// supervisor/Rust pair, never installed systemd units or hardware ports.
		var lostStart, lostStop, lostComplete atomic.Bool
		var starts, stops, completes atomic.Int32
		handler := restored.Handler()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost {
				handler.ServeHTTP(w, request)
				return
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			var action struct {
				Action string `json:"action"`
			}
			if err := json.Unmarshal(body, &action); err != nil {
				t.Error(err)
				return
			}
			switch action.Action {
			case "start":
				starts.Add(1)
			case "stop":
				stops.Add(1)
			case "complete":
				completes.Add(1)
			}
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, request)
			if (action.Action == "start" && lostStart.Swap(false)) || (action.Action == "stop" && lostStop.Swap(false)) || (action.Action == "complete" && lostComplete.Swap(false)) {
				// The real agent already accepted/executed the action. Lose only
				// the publisher-facing reply; never inject a serial command.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
		}))
		defer server.Close()
		source, err := filepath.Abs("../../..")
		if err != nil {
			t.Fatal(err)
		}
		addresses, err := exec.Command("hostname", "-I").Output()
		if err != nil || len(strings.Fields(string(addresses))) == 0 {
			t.Fatal("host identity unavailable", err)
		}
		host := strings.Fields(string(addresses))[0]
		releases := t.TempDir()
		artifact := filepath.Join(releases, "build-interop")
		if err := os.Mkdir(artifact, 0700); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifact, "binary"), contents, 0700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		versionJSON, err := exec.Command(exe, "version", "--json").Output()
		if err != nil {
			t.Fatal(err)
		}
		var version map[string]any
		if err := json.Unmarshal(versionJSON, &version); err != nil {
			t.Fatal(err)
		}
		writeJSONFile := func(path string, value any) {
			t.Helper()
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		writeJSONFile(filepath.Join(artifact, "manifest.json"), map[string]any{
			"host": host, "component": "lower", "binary_sha256": hex.EncodeToString(digest[:]), "build_info": version})
		profile := filepath.Join(releases, "profile.json")
		writeJSONFile(profile, map[string]any{
			"host": host, "component": "lower", "source_dir": source, "release_dir": releases,
			"runtime_dir": root, "executable": exe, "gateway_instance_id": cfg.GatewayInstanceID,
			"maintenance_endpoint": cfg.MaintenanceEndpoint, "opc_port": port,
			"databases":    []string{filepath.Join(root, "state", "command-journal.sqlite"), filepath.Join(root, "state", "maintenance.sqlite")},
			"config_files": []string{filepath.Join(root, "config", "config.toml")},
			"control":      map[string]string{"kind": "process-agent", "url": server.URL, "instance_id": cfg.InstanceID}})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "python3", filepath.Join(source, "tools", "release", "release.py"), "deploy",
			"--profile", profile, "--host", host, "--component", "lower", "--artifact", artifact,
			"--maintenance", "--deployment-id", "isolated-release-interop", "--initiator", "isolated-test",
			"--confirm-live-target", host+"/lower", "--confirm-no-active-commands")
		text, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("release runner: %v\n%s", err, text)
		}
		var report map[string]any
		if err := json.Unmarshal(text, &report); err != nil {
			t.Fatalf("%v: %s", err, text)
		}
		recordBytes, err := os.ReadFile(filepath.Join(releases, "deployments", "isolated-release-interop", "deployment.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(recordBytes, &record); err != nil {
			t.Fatal(err)
		}
		if record["state"] != "succeeded" || restored.Maintenance().Job.Phase != "completed" ||
			restored.Maintenance().Job.FencingToken != strconv.FormatUint(previousFence+1, 10) || restored.Snapshot().State != "running" {
			t.Fatalf("unexpected release state: %s; %+v", recordBytes, restored.Maintenance())
		}
		after, err := os.ReadFile(filepath.Join(root, "config", "config.toml"))
		if err != nil || string(after) != config {
			t.Fatal("release changed user configuration", err)
		}
		revision := restored.Maintenance().Revision
		inspection := exec.CommandContext(ctx, "python3", filepath.Join(source, "tools", "release", "release.py"), "inspect-deployment",
			"--profile", profile, "--host", host, "--component", "lower", "--deployment-id", "isolated-release-interop")
		text, err = inspection.CombinedOutput()
		if err != nil {
			t.Fatalf("read-only inspection: %v\n%s", err, text)
		}
		var observed map[string]any
		if err := json.Unmarshal(text, &observed); err != nil {
			t.Fatal(err)
		}
		if observed["artifact_unchanged"] != true || observed["running_binary_sha256"] != hex.EncodeToString(digest[:]) ||
			observed["automatic_resume"] != false || restored.Maintenance().Revision != revision {
			t.Fatalf("inspection mutated state or failed to verify running image: %s", text)
		}
		for _, failure := range []string{"stop", "start", "complete"} {
			t.Run("RecoverLostReply_"+failure, func(t *testing.T) {
				deploymentID := "isolated-recovery-" + failure
				beforeStarts, beforeStops, beforeCompletes := starts.Load(), stops.Load(), completes.Load()
				if failure == "start" {
					lostStart.Store(true)
				} else if failure == "stop" {
					lostStop.Store(true)
				} else {
					lostComplete.Store(true)
				}
				args := []string{filepath.Join(source, "tools", "release", "release.py"), "deploy", "--profile", profile,
					"--host", host, "--component", "lower", "--artifact", artifact, "--maintenance",
					"--deployment-id", deploymentID, "--initiator", "isolated-test", "--confirm-live-target", host + "/lower", "--confirm-no-active-commands"}
				text, err := exec.CommandContext(ctx, "python3", args...).CombinedOutput()
				if err == nil {
					t.Fatalf("lost reply unexpectedly succeeded: %s", text)
				}
				if failure == "stop" {
					awaitRust(restored, deploymentID+".stop")
					resumeArgs := []string{filepath.Join(source, "tools", "release", "release.py"), "resume-deployment", "--profile", profile,
						"--host", host, "--component", "lower", "--deployment-id", deploymentID, "--initiator", "isolated-resume",
						"--confirm-live-target", host + "/lower", "--confirm-no-active-commands"}
					text, err = exec.CommandContext(ctx, "python3", resumeArgs...).CombinedOutput()
					if err != nil {
						t.Fatalf("resume-deployment failed: %v\n%s", err, text)
					}
					if restored.Maintenance().Job.Phase == "completed" {
						t.Fatal("resume prematurely released maintenance")
					}
				}
				awaitRust(restored, deploymentID+".start")
				args = []string{filepath.Join(source, "tools", "release", "release.py"), "finish-deployment", "--profile", profile,
					"--host", host, "--component", "lower", "--deployment-id", deploymentID, "--initiator", "isolated-recovery",
					"--confirm-live-target", host + "/lower", "--confirm-no-active-commands"}
				text, err = exec.CommandContext(ctx, "python3", args...).CombinedOutput()
				if err != nil {
					t.Fatalf("finish-deployment failed: %v\n%s", err, text)
				}
				if starts.Load()-beforeStarts != 1 || stops.Load()-beforeStops != 1 || completes.Load()-beforeCompletes != 1 ||
					restored.Maintenance().Job.Phase != "completed" {
					t.Fatalf("replayed mutation or incomplete recovery: start=%d stop=%d complete=%d", starts.Load()-beforeStarts, stops.Load()-beforeStops, completes.Load()-beforeCompletes)
				}
				t.Log("lost " + failure + " reply recovered using original job, no duplicate start/stop/complete")
			})
		}
		for _, resumeRollback := range []bool{false, true} {
			name := "RollbackBeforeNewStart"
			if resumeRollback {
				name = "ResumeInterruptedRollback"
			}
			t.Run(name, func(t *testing.T) {
				deploymentID := "isolated-" + name
				beforeStarts, beforeStops, beforeCompletes := starts.Load(), stops.Load(), completes.Load()
				// Fault belongs to this publisher test process only. No production
				// environment switch exists that can interrupt installed releases.
				faultScript := `import sys
sys.path.insert(0, sys.argv.pop(1))
import release
original = release.Deployment.step
def step(self, name, function, intent=None):
    if name == 'started':
        raise RuntimeError('isolated publisher interruption before start intent')
    return original(self, name, function, intent)
release.Deployment.step = step
release.main()
`
				args := []string{"-c", faultScript, filepath.Join(source, "tools", "release"), "deploy", "--profile", profile,
					"--host", host, "--component", "lower", "--artifact", artifact, "--maintenance", "--deployment-id", deploymentID,
					"--initiator", "isolated-test", "--confirm-live-target", host + "/lower", "--confirm-no-active-commands"}
				text, err := exec.CommandContext(ctx, "python3", args...).CombinedOutput()
				if err == nil || starts.Load() != beforeStarts {
					t.Fatalf("upgrade unexpectedly started: %v %s", err, text)
				}
				awaitRust(restored, deploymentID+".stop")
				currentConfig := config + "\n# user configuration retained during rollback\n"
				if err := os.WriteFile(filepath.Join(root, "config", "config.toml"), []byte(currentConfig), 0600); err != nil {
					t.Fatal(err)
				}
				lostStart.Store(true)
				args = []string{filepath.Join(source, "tools", "release", "release.py"), "rollback-deployment", "--profile", profile,
					"--host", host, "--component", "lower", "--deployment-id", deploymentID, "--initiator", "isolated-rollback",
					"--confirm-live-target", host + "/lower", "--confirm-no-active-commands"}
				if resumeRollback {
					rollbackFault := `import sys
sys.path.insert(0, sys.argv.pop(1))
import release
from recovery import FinishDeployment
original = FinishDeployment.step
def step(self, name, function):
    if name == 'rollback_files_verified':
        raise RuntimeError('isolated interruption after old binary restore, before start intent')
    return original(self, name, function)
FinishDeployment.step = step
release.main()
`
					interruptedArgs := append([]string{"-c", rollbackFault, filepath.Join(source, "tools", "release")}, args[1:]...)
					text, err = exec.CommandContext(ctx, "python3", interruptedArgs...).CombinedOutput()
					if err == nil || starts.Load() != beforeStarts || restored.Snapshot().State != "stopped" {
						t.Fatalf("rollback interruption unexpectedly started: %v %s", err, text)
					}
					currentConfig += "# additional user edit after interrupted rollback\n"
					if err := os.WriteFile(filepath.Join(root, "config", "config.toml"), []byte(currentConfig), 0600); err != nil {
						t.Fatal(err)
					}
					args[1] = "resume-rollback"
				}
				text, err = exec.CommandContext(ctx, "python3", args...).CombinedOutput()
				if err == nil {
					t.Fatalf("lost rollback start reply unexpectedly succeeded: %s", text)
				}
				awaitRust(restored, deploymentID+".rollback-start")
				if resumeRollback {
					if replay, err := exec.CommandContext(ctx, "python3", args...).CombinedOutput(); err == nil || starts.Load()-beforeStarts != 1 {
						t.Fatalf("resume replay not refused: %v %s", err, replay)
					}
				}
				args[1] = "finish-rollback"
				text, err = exec.CommandContext(ctx, "python3", args...).CombinedOutput()
				if err != nil {
					t.Fatalf("finish rollback failed: %v\n%s", err, text)
				}
				var outcome map[string]any
				if err := json.Unmarshal(text, &outcome); err != nil {
					t.Fatal(err)
				}
				if outcome["outcome"] != "rolled_back" || outcome["data_restored"] != false ||
					starts.Load()-beforeStarts != 1 || stops.Load()-beforeStops != 1 || completes.Load()-beforeCompletes != 1 {
					t.Fatalf("rollback outcome/replay mismatch: %s", text)
				}
				retained, err := os.ReadFile(filepath.Join(root, "config", "config.toml"))
				if err != nil || string(retained) != currentConfig {
					t.Fatal("rollback replaced user's current config", err)
				}
				t.Log("pre-start binary rollback + lost rollback-start reply + finish passed; current config retained, no duplicate process actions")
			})
		}
		t.Run("AnsibleRelease", func(t *testing.T) {
			ansible := os.Getenv("RCD_ANSIBLE_PLAYBOOK")
			if ansible == "" {
				t.Skip("set RCD_ANSIBLE_PLAYBOOK to the isolated controller executable")
			}
			ansibleCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			inventory := filepath.Join(releases, "inventory.json")
			variables := filepath.Join(releases, "ansible-vars.json")
			writeJSONFile(inventory, map[string]any{"all": map[string]any{"children": map[string]any{
				"rcd_nodes": map[string]any{"hosts": map[string]any{"isolated": map[string]any{
					"ansible_connection": "local", "ansible_host": host, "ansible_python_interpreter": "/usr/bin/python3"}}}}}})
			releaseVars := map[string]any{"host": host, "component": "lower", "profile": profile,
				"tool_dir": filepath.Join(releases, "ansible-tools"), "artifact_dir": artifact,
				"mode": "maintenance", "operation_id": "ansible-deploy", "deployment_id": "ansible-deploy"}
			vars := map[string]any{"rcd_release": releaseVars, "rcd_confirm_targets": []string{host + "/lower"},
				"rcd_no_active_commands": true, "rcd_initiator": "isolated-ansible-test"}
			invoke := func(playbook string, wantSuccess bool) {
				t.Helper()
				writeJSONFile(variables, vars)
				out, err := exec.CommandContext(ansibleCtx, ansible, "-i", inventory,
					filepath.Join(source, "tools", "ansible", playbook+".yml"), "--limit", "isolated", "-e", "@"+variables).CombinedOutput()
				if (err == nil) != wantSuccess {
					t.Fatalf("Ansible %s unexpected result: %v\n%s", playbook, err, out)
				}
				t.Logf("Ansible %s expected_success=%t\n%s", playbook, wantSuccess, out)
			}
			beforeStarts, beforeStops, beforeCompletes := starts.Load(), stops.Load(), completes.Load()
			invoke("deploy", true)
			invoke("deploy", true) // Same operation returns saved result, not another restart.
			invoke("verify", true)
			if starts.Load()-beforeStarts != 1 || stops.Load()-beforeStops != 1 || completes.Load()-beforeCompletes != 1 {
				t.Fatal("Ansible replayed a successful process action")
			}
			releaseVars["operation_id"], releaseVars["deployment_id"] = "ansible-lost-complete", "ansible-lost-complete"
			lostComplete.Store(true)
			invoke("deploy", false)
			invoke("deploy", false) // Uncertain result must be inspected/recovered, never replayed.
			vars["rcd_recovery_action"] = "inspect-deployment"
			invoke("recover", true)
			releaseVars["operation_id"] = "ansible-finish-lost-complete"
			vars["rcd_recovery_action"] = "finish-deployment"
			invoke("recover", true)
			if starts.Load()-beforeStarts != 2 || stops.Load()-beforeStops != 2 || completes.Load()-beforeCompletes != 2 ||
				restored.Maintenance().Job.Phase != "completed" {
				t.Fatal("Ansible recovery replayed an action or did not complete")
			}
			t.Log("Real ansible-playbook -> Python release -> Go agent -> Rust OPC passed; no serial devices")
		})
		t.Log("Python publisher -> real HTTP agent -> real Rust OPC: release and recovery paths passed; no serial devices")
	})
}
