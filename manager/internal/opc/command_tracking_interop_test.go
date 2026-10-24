package opc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

type trackingInteropSink struct {
	NopLowerSink
	revisions atomic.Uint64
}

func (s *trackingInteropSink) OnCommandRevision(string, uint64) { s.revisions.Add(1) }

// Opt-in real Rust binary / Go OPC adapter integration. It creates all runtime
// files itself, uses a disabled NONEXISTENT port inside t.TempDir and a loopback
// ephemeral OPC listener. It cannot point a command at a field serial device.
// No simulator, external endpoint or production runtime configuration is used.
// This verifies protocol rejection/recovery, not successful hardware control.
func TestCommandTrackingRealRustInterop(t *testing.T) {
	binary := os.Getenv("RCD_TRACKING_INTEROP_BINARY")
	if binary == "" {
		t.Skip("requires explicitly built isolated Rust binary")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolated Linux acceptance only")
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "rcd_server")
	input, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(executable, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal(copyErr, closeErr)
	}
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", `[server]
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
`)
	portID := filepath.Join(dir, "NONEXISTENT-SERIAL-PORT")
	// Keep the deployed legacy labels and FALSE spelling: an unrelated typed
	// timing edit must not fail merely because the unchanged CSV is old-format.
	write("rcd_interop.csv", fmt.Sprintf("COM,%s,,,,,,,\nBaudRate,28800,,,,,,,\nIsSpecial,FALSE,,,,,,,\nComIndex,1,,,,,,,\nDeviceID,1,,,,,,,\nDataLen,1,,,,,,,\nPre,INTEROP_,,,,,,,\nKKS,Type,F_Type,byte_index,bit_index,Def,Max,Min,LH\nStatus,uint,status,1,0.7,0,255,0,0\n", portID))
	state, _ := json.Marshal(map[string]any{"schema_version": 1, "revision": "1", "ports": map[string]any{portID: map[string]any{"enabled": false, "device_read_enabled": map[string]bool{"1": false}}}})
	write("operations.json", string(state))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	endpoint := fmt.Sprintf("opc.tcp://127.0.0.1:%d/", port)
	start := func() func() {
		t.Helper()
		logFile, err := os.OpenFile(filepath.Join(dir, "lower.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable)
		cmd.Dir, cmd.Stdout, cmd.Stderr = dir, logFile, logFile
		cmd.Env = append(os.Environ(), fmt.Sprintf("RCD_OPCUA_PORT=%d", port))
		if err := cmd.Start(); err != nil {
			logFile.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			logFile.Close()
			if t.Failed() {
				content, _ := os.ReadFile(filepath.Join(dir, "lower.log"))
				if len(content) > 16000 {
					content = content[len(content)-16000:]
				}
				t.Log(string(content))
			}
		}
		t.Cleanup(stop)
		return stop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pair, err := pki.Ensure(filepath.Join(dir, "client-pki"), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	connect := func(secure bool, username string) (*LowerSupervisor, *trackingInteropSink, uint64, domain.CommandTrackingHealth, func()) {
		t.Helper()
		sink := &trackingInteropSink{}
		cfg := LowerConfig{Source: domain.Source{ID: "interop", GroupID: "g", Endpoint: endpoint, Enabled: true}, ConnectTimeout: 2 * time.Second, OperationTimeout: 2 * time.Second}
		if secure {
			cfg.Source.SecurityPolicy, cfg.Source.SecurityMode = "Basic256Sha256", "SignAndEncrypt"
			cfg.CertificateFile, cfg.PrivateKeyFile = pair.CertificatePath, pair.PrivateKeyPath
			cfg.Source.Username, cfg.Password = username, "pwd" // existing lower test identity, not field credentials
		}
		lower, err := NewLowerSupervisor(cfg, sink)
		if err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = lower.Run(runCtx) }()
		cleanup := func() { stop(); <-done }
		t.Cleanup(cleanup)
		for ctx.Err() == nil {
			status := lower.Status()
			if status.Subscription == SubscriptionActive {
				info, supported, err := lower.CommandTrackingInfo(ctx, status.Epoch)
				if err == nil && supported {
					return lower, sink, status.Epoch, info, cleanup
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("Rust/Go tracking connection timed out", lower.Status())
		return nil, nil, 0, domain.CommandTrackingHealth{}, nil
	}
	stop := start()
	lower, sink, epoch, identity, disconnect := connect(false, "")
	// Verify the exact parameter descriptor consumed by the manager's generic
	// configuration form through the real Rust OPC operations adapter. Prepare
	// is read-only: neither generating a draft nor rejecting it may alter disk.
	beforeConfig, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(beforeConfig)
	var preparedContent string
	for _, interval := range []int{0, 500} {
		edits := json.RawMessage(`[]`)
		if interval != 0 {
			edits = json.RawMessage(fmt.Sprintf(`[{"path":["settings","command_interval_ms"],"value":%d},{"path":["settings","transmit_settle_guard_ms"],"value":10}]`, interval))
		}
		raw, prepareErr := lower.ExecuteOperation(ctx, domain.OperationRequest{Action: "prepare_config", Name: "config.toml", Content: &content, Edits: edits}, epoch)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		var form struct {
			Content string `json:"content"`
			Fields  []struct {
				Key       string `json:"key"`
				Value     any    `json:"value"`
				Inherited bool   `json:"inherited"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(raw, &form); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, field := range form.Fields {
			if field.Key == "settings.command_interval_ms" {
				found = true
				if field.Value != float64(interval) || field.Inherited != (interval == 0) {
					t.Fatalf("wrong command interval descriptor: %+v", field)
				}
			}
		}
		if !found || (interval == 500 && !strings.Contains(form.Content, "command_interval_ms = 500")) {
			t.Fatalf("missing command interval parameter or draft: %s", raw)
		}
		if interval == 500 {
			if !strings.Contains(form.Content, "transmit_settle_guard_ms = 10") {
				t.Fatalf("transmit guard draft missing with legacy CSV: %s", raw)
			}
			preparedContent = form.Content
		}
	}
	if _, err := lower.ExecuteOperation(ctx, domain.OperationRequest{Action: "prepare_config", Name: "config.toml", Content: &content,
		Edits: json.RawMessage(`[{"path":["settings","command_interval_ms"],"value":60001}]`)}, epoch); err == nil {
		t.Fatal("out-of-range command interval accepted")
	}
	afterConfig, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil || string(afterConfig) != content {
		t.Fatalf("read-only interval preparation changed configuration: %v", err)
	}
	t.Log("real Rust/Go OPC: command interval default, 500ms draft, bounds rejection and unchanged disk passed")
	// Persist only this disposable lower's config, never a field endpoint. Verify
	// the same CAS + pending-restart semantics used by the web save path.
	pointTable, err := os.ReadFile(filepath.Join(configDir, "rcd_interop.csv"))
	if err != nil {
		t.Fatal(err)
	}
	pointTable = append(pointTable, []byte("InvalidAddress,uint,status,20,0.15,0,255,0,0\n")...)
	write("rcd_interop.csv", string(pointTable))
	revision := fmt.Sprintf("%x", sha256.Sum256(beforeConfig))
	validation, err := lower.ExecuteOperation(ctx, domain.OperationRequest{Action: "validate_config", Name: "config.toml", Content: &preparedContent}, epoch)
	if err != nil {
		t.Fatal(err)
	}
	var validated struct {
		Valid    bool     `json:"valid"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(validation, &validated); err != nil || !validated.Valid {
		t.Fatalf("interval draft failed real lower validation: %s %v", validation, err)
	}
	if len(validated.Warnings) != 1 || !strings.Contains(validated.Warnings[0], "rcd_interop.csv: 第 10 行 INTEROP_InvalidAddress: byte_index=20") {
		t.Fatalf("point warning missing or downgraded to blocking: %s", validation)
	}
	saved, err := lower.ExecuteOperation(ctx, domain.OperationRequest{Action: "write_config", Name: "config.toml", Content: &preparedContent, ExpectedRevision: revision}, epoch)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Content         string `json:"content"`
		ActiveRevision  string `json:"active_revision"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.Unmarshal(saved, &document); err != nil || document.Content != preparedContent || document.ActiveRevision != revision || !document.RestartRequired {
		t.Fatalf("wrong real lower saved/pending-restart state: %s %v", saved, err)
	}
	if _, err := lower.ExecuteOperation(ctx, domain.OperationRequest{Action: "write_config", Name: "config.toml", Content: &content, ExpectedRevision: revision}, epoch); err == nil {
		t.Fatal("stale config revision overwrote the saved interval")
	}
	if disk, err := os.ReadFile(filepath.Join(configDir, "config.toml")); err != nil || string(disk) != preparedContent {
		t.Fatalf("interval CAS save did not preserve the actual file: %v", err)
	}
	invalidConfig := strings.Replace(preparedContent, "transmit_settle_guard_ms = 10", "transmit_settle_guard_ms = 101", 1)
	currentRevision := fmt.Sprintf("%x", sha256.Sum256([]byte(preparedContent)))
	if _, err := lower.ExecuteOperation(ctx, domain.OperationRequest{Action: "write_config", Name: "config.toml", Content: &invalidConfig, ExpectedRevision: currentRevision}, epoch); err == nil {
		t.Fatal("application configuration error accepted with point warnings")
	}
	if disk, err := os.ReadFile(filepath.Join(configDir, "rcd_interop.csv")); err != nil || string(disk) != string(pointTable) {
		t.Fatalf("point warning caused point table rewrite: %v", err)
	}
	t.Log("real Rust/Go OPC: interval validate, CAS save, pending restart and stale revision rejection passed in disposable runtime")
	t.Log("real Rust/Go OPC: point bounds warning permits save without changing CSV; invalid application guard still rejected")
	request := map[string]any{"action": "submit_command", "schema_version": 1, "manager_instance_id": "interop-manager", "command_id": "one", "port_id": portID, "hex_data": "01 08", "expected_gateway_instance_id": identity.GatewayInstanceID, "expected_boot_id": identity.BootID, "timeout_ms": 1000}
	before := sink.revisions.Load()
	rejected, err := lower.SubmitTrackedCommand(ctx, epoch, request)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Accepted || rejected.Phase != "Failed" || rejected.AttemptCount != 0 || rejected.SentCount != 0 || rejected.StatusCode == domain.StatusGood {
		t.Fatalf("disabled physical port must reject before sending: %+v", rejected)
	}
	duplicate, err := lower.SubmitTrackedCommand(ctx, epoch, request)
	if err != nil || duplicate.AckID != rejected.AckID {
		t.Fatal("duplicate changed identity", duplicate, err)
	}
	request["hex_data"] = "01 09"
	if _, err := lower.SubmitTrackedCommand(ctx, epoch, request); err == nil {
		t.Fatal("content conflict accepted")
	}
	request["hex_data"] = "01 08"
	request["expected_boot_id"] = "incorrect-boot"
	if _, err := lower.SubmitTrackedCommand(ctx, epoch, request); err == nil {
		t.Fatal("stale boot accepted")
	}
	request["expected_boot_id"] = identity.BootID
	var found bool
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
		events, err := lower.TrackedCommandEvents(ctx, epoch, "interop-manager", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, command := range events.Items {
			if command.CommandID == "one" && command.Phase == "Failed" {
				found = true
			}
		}
		if found && sink.revisions.Load() > before {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !found || sink.revisions.Load() <= before {
		t.Fatal("durable event or revision subscription missing", found, sink.revisions.Load(), before)
	}
	for _, username := range []string{"", "user1"} {
		secureLower, _, secureEpoch, secureIdentity, closeSecure := connect(true, username)
		if secureIdentity.GatewayInstanceID != identity.GatewayInstanceID {
			t.Fatal("secure endpoint changed lower identity")
		}
		request["command_id"] = "secure-" + username
		secured, err := secureLower.SubmitTrackedCommand(ctx, secureEpoch, request)
		if err != nil || secured.Accepted || secured.Phase != "Failed" || secured.AttemptCount != 0 {
			t.Fatal("secure tracking rejection", secured, err)
		}
		closeSecure()
		t.Logf("Basic256Sha256/SignAndEncrypt tracking passed (username=%q)", username)
	}
	disconnect()
	stop()
	stop = start()
	defer stop()
	lower, _, epoch, restarted, _ := connect(false, "")
	if restarted.GatewayInstanceID != identity.GatewayInstanceID || restarted.BootID == identity.BootID {
		t.Fatal("invalid restart identities")
	}
	history, err := lower.GetTrackedCommand(ctx, epoch, "interop-manager", "one")
	if err != nil || history == nil || history.BootID != identity.BootID || history.Phase != "Failed" {
		t.Fatal("history lost at restart", history, err)
	}
	if _, err := os.Stat(portID); !os.IsNotExist(err) {
		t.Fatal("isolated port path unexpectedly exists")
	}
	t.Log("real Rust/Go loopback OPC: capability, disabled-port rejection, duplicate/content conflict, stale boot, revision subscription, event pull and restart history passed; no serial device exists")
}
