//go:build linux

package processagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

// The only process a test starts/stops is its own copy of the Go test helper.
// These tests run on the isolated Linux host, never against an installed OPC.
func TestProcessAgentChild(t *testing.T) {
	args := os.Args
	index := -1
	for i, arg := range args {
		if arg == "--process-agent-test-child" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	mode := args[index+1]
	if mode == "exit" {
		fmt.Println("configured helper startup failure")
		os.Exit(7)
	}
	if mode == "agent" {
		exe, _ := os.Executable()
		root := args[index+2]
		cfg := Config{InstanceID: "owned-test", Executable: exe, WorkDir: root, DataDir: filepath.Join(root, "state"), HTTPListen: "127.0.0.1:0", AutoStart: true, RestartPolicy: "never", StopTimeout: time.Second, StartTimeout: time.Second, StartGrace: 50 * time.Millisecond, Arguments: helperArgs("graceful")}
		s, err := New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(8)
		}
		deadline := time.Now().Add(5 * time.Second)
		for s.Snapshot().State != "running" {
			if time.Now().After(deadline) {
				os.Exit(9)
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = os.WriteFile(filepath.Join(root, "child.pid"), []byte(strconv.Itoa(s.Snapshot().PID)), 0o600)
		for {
			time.Sleep(time.Second)
		}
	}
	if mode == "stubborn" {
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("stubborn helper ready")
		for {
			time.Sleep(time.Second)
		}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	cwd, _ := os.Getwd()
	fmt.Println("graceful helper ready", cwd)
	<-ch
	fmt.Println("graceful helper stopped")
	os.Exit(0)
}

func helperArgs(mode string) []string {
	return []string{"-test.run=^TestProcessAgentChild$", "--", "--process-agent-test-child", mode}
}

func supervisorConfig(t *testing.T, mode string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The race-instrumented helper performs Go's exit-time race drain before
	// exiting zero; allow that real graceful exit rather than forcing a 300ms
	// test-only timeout. Production's default grace remains 15 seconds.
	return Config{InstanceID: "owned-test", Executable: exe, WorkDir: dir, DataDir: filepath.Join(dir, "state"), HTTPListen: "127.0.0.1:0", RestartPolicy: "never", StopTimeout: 2 * time.Second, StartTimeout: time.Second, StartGrace: 150 * time.Millisecond, Arguments: helperArgs(mode)}
}

func newTestSupervisor(t *testing.T, cfg Config) *Supervisor {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func submitAction(t *testing.T, s *Supervisor, action string) domain.ProcessActionRequest {
	t.Helper()
	r := domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: action, ExpectedRevision: s.Snapshot().Revision}
	if _, err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func awaitOperation(t *testing.T, s *Supervisor, req domain.ProcessActionRequest) domain.ProcessOperation {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		value, ok := s.Operation(req.RequestID)
		if ok && value.Terminal() {
			if err := value.Validate(req); err != nil {
				t.Fatalf("invalid confirmed operation: %v, %+v", err, value)
			}
			return value
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation did not settle: %+v", s.Snapshot())
	return domain.ProcessOperation{}
}

func awaitState(t *testing.T, s *Supervisor, predicate func(domain.ProcessSnapshot) bool) domain.ProcessSnapshot {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		value := s.Snapshot()
		if predicate(value) {
			return value
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state not reached: %+v", s.Snapshot())
	return domain.ProcessSnapshot{}
}

func TestSupervisorStartRestartStopIdentityAndIdempotency(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t, "graceful"))
	if snapshot := s.Snapshot(); snapshot.State != "stopped" || snapshot.PID != 0 {
		t.Fatalf("unexpected initial state %+v", snapshot)
	}
	start := submitAction(t, s, "start")
	first := awaitOperation(t, s, start)
	if first.State != "succeeded" || first.Snapshot.PID <= 1 {
		t.Fatalf("start failed: %+v", first)
	}
	oldPID := first.Snapshot.PID
	if value, err := s.Submit(start); err != nil || value.Snapshot.PID != oldPID {
		t.Fatalf("idempotent start changed result: %+v %v", value, err)
	}
	different := start
	different.Action = "restart"
	if _, err := s.Submit(different); err == nil {
		t.Fatal("same request ID accepted with different payload")
	}
	stale := domain.ProcessActionRequest{RequestID: newID(), InstanceID: start.InstanceID, Action: "stop", ExpectedRevision: start.ExpectedRevision}
	if _, err := s.Submit(stale); err == nil {
		t.Fatal("stale revision accepted")
	}
	restart := submitAction(t, s, "restart")
	restarted := awaitOperation(t, s, restart)
	if restarted.State != "succeeded" || restarted.Snapshot.PID == oldPID || restarted.Forced {
		t.Fatalf("restart not graceful/unique: %+v", restarted)
	}
	if value, err := s.Submit(restart); err != nil || value.Snapshot.PID != restarted.Snapshot.PID || s.Snapshot().PID != restarted.Snapshot.PID {
		t.Fatalf("restart replayed: %+v %v", value, err)
	}
	stop := submitAction(t, s, "stop")
	stopped := awaitOperation(t, s, stop)
	if stopped.State != "succeeded" || stopped.Forced || stopped.Snapshot.PID != 0 || stopped.Snapshot.ExitCode == nil || *stopped.Snapshot.ExitCode != 0 {
		t.Fatalf("stop not confirmed: %+v", stopped)
	}
	log, err := s.Log(65536)
	if err != nil || !strings.Contains(log.Text, "graceful helper stopped") || !strings.Contains(log.Text, s.cfg.WorkDir) {
		t.Fatalf("fixed-cwd child output missing: %+v %v", log, err)
	}
}

func TestSupervisorPendingRejectsConflictingAction(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	cfg.StartGrace = 500 * time.Millisecond
	s := newTestSupervisor(t, cfg)
	start := submitAction(t, s, "start")
	other := domain.ProcessActionRequest{RequestID: newID(), InstanceID: cfg.InstanceID, Action: "restart", ExpectedRevision: s.Snapshot().Revision}
	_, err := s.Submit(other)
	var operationError *domain.OperationError
	if !errors.As(err, &operationError) || (operationError.Code != "busy" && operationError.Code != "conflict") {
		t.Fatalf("parallel action not rejected: %v", err)
	}
	if got := awaitOperation(t, s, start); got.State != "succeeded" {
		t.Fatal(got)
	}
}

func TestSupervisorForcedStopIsExplicit(t *testing.T) {
	cfg := supervisorConfig(t, "stubborn")
	cfg.StopTimeout = 300 * time.Millisecond
	s := newTestSupervisor(t, cfg)
	if got := awaitOperation(t, s, submitAction(t, s, "start")); got.State != "succeeded" {
		t.Fatal(got)
	}
	stop := awaitOperation(t, s, submitAction(t, s, "stop"))
	if stop.State != "succeeded" || !stop.Forced || !stop.Snapshot.LastStopForced || stop.Snapshot.PID != 0 {
		t.Fatalf("forced shutdown hidden: %+v", stop)
	}
}

func TestSupervisorFailedStartHasOutputAndNoLivePID(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t, "exit"))
	op := awaitOperation(t, s, submitAction(t, s, "start"))
	if op.State != "failed" || op.Snapshot.PID != 0 || op.Error == "" || op.Snapshot.ExitCode == nil || *op.Snapshot.ExitCode != 7 {
		t.Fatalf("startup failure not represented: %+v", op)
	}
	value, _ := s.Log(65536)
	if !strings.Contains(value.Text, "startup failure") {
		t.Fatal(value)
	}
}

func TestSupervisorManualStopPersistsAcrossAgentRestart(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	cfg.AutoStart = true
	s := newTestSupervisor(t, cfg)
	awaitState(t, s, func(v domain.ProcessSnapshot) bool { return v.State == "running" && v.PendingRequestID == "" })
	stop := submitAction(t, s, "stop")
	awaitOperation(t, s, stop)
	s.Close()
	reopened := newTestSupervisor(t, cfg)
	if value := reopened.Snapshot(); value.State != "stopped" || value.DesiredState != "stopped" || value.PID != 0 {
		t.Fatalf("manual stop was lost across agent restart: %+v", value)
	}
	if old, ok := reopened.Operation(stop.RequestID); !ok || old.State != "succeeded" {
		t.Fatalf("durable history lost: %+v", old)
	}
}

func TestSupervisorAgentRestartRestoresRunningDesiredState(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	s := newTestSupervisor(t, cfg)
	op := awaitOperation(t, s, submitAction(t, s, "start"))
	oldPID := op.Snapshot.PID
	s.Close()
	reopened := newTestSupervisor(t, cfg)
	value := awaitState(t, reopened, func(v domain.ProcessSnapshot) bool { return v.State == "running" && v.PendingRequestID == "" })
	if value.PID == oldPID || value.DesiredState != "running" {
		t.Fatalf("desired state not restored: %+v", value)
	}
}

func TestSupervisorCrashRestartPolicyNeverSwitchesSourceAndManualStopWins(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	cfg.RestartPolicy = "on-failure"
	s := newTestSupervisor(t, cfg)
	op := awaitOperation(t, s, submitAction(t, s, "start"))
	oldPID := op.Snapshot.PID
	s.mu.Lock()
	owned := s.child.process
	s.mu.Unlock()
	if err := owned.Kill(); err != nil {
		t.Fatal(err)
	}
	value := awaitState(t, s, func(v domain.ProcessSnapshot) bool {
		return v.State == "running" && v.PID != oldPID && v.PendingRequestID == ""
	})
	if value.InstanceID != cfg.InstanceID || value.RestartCount == 0 {
		t.Fatalf("restart changed instance identity or not counted: %+v", value)
	}
	awaitOperation(t, s, submitAction(t, s, "stop"))
	s.mu.Lock()
	retry := s.retryAt
	s.mu.Unlock()
	if !retry.IsZero() || s.Snapshot().DesiredState != "stopped" {
		t.Fatal("manual stop retained an auto-restart timer")
	}
}

func TestSupervisorResolveTombstonePreventsLateExecutionAndEvictedReplay(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t, "graceful"))
	req := domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: "restart", ExpectedRevision: s.Snapshot().Revision}
	resolved, err := s.Resolve(req)
	if err != nil || resolved.State != "failed" || resolved.ErrorCode != "unconfirmed_cancelled" {
		t.Fatalf("resolve did not seal missing request: %+v %v", resolved, err)
	}
	if got, err := s.Submit(req); err != nil || got.State != "failed" || s.Snapshot().PID != 0 {
		t.Fatalf("late POST executed after resolve: %+v %v", got, err)
	}
	for i := 0; i < retainedOperations; i++ {
		other := req
		other.RequestID = newID()
		if _, err := s.Resolve(other); err != nil {
			t.Fatal(err)
		}
	}
	if _, present := s.Operation(req.RequestID); present {
		t.Fatal("fixture did not evict the old tombstone")
	}
	if _, err := s.Submit(req); err == nil || s.Snapshot().PID != 0 {
		t.Fatal("an evicted tombstone's original revision became executable")
	}
}

func TestSupervisorBoundedHistoryNeverEvictsPending(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t, "graceful"))
	req := domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: "start", ExpectedRevision: s.Snapshot().Revision}
	s.mu.Lock()
	s.addLocked(req, "accepted")
	s.snapshot.PendingRequestID = req.RequestID
	s.mu.Unlock()
	for i := 0; i < retainedOperations+5; i++ {
		other := req
		other.RequestID = newID()
		if _, err := s.Resolve(other); err != nil {
			t.Fatal(err)
		}
	}
	if op, ok := s.Operation(req.RequestID); !ok || op.Terminal() {
		t.Fatalf("pending operation evicted: %+v %v", op, ok)
	}
	s.mu.Lock()
	count := len(s.state.Operations)
	s.mu.Unlock()
	if count != retainedOperations {
		t.Fatalf("unbounded journal: %d", count)
	}
}

func TestSupervisorLockAndCorruptStateNeverStartAnotherLower(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	s := newTestSupervisor(t, cfg)
	if other, err := New(cfg); err == nil {
		other.Close()
		t.Fatal("two agents acquired same data directory")
	}
	s.Close()
	statePath := filepath.Join(cfg.DataDir, "process-state.json")
	broken := []byte("{invalid durable state")
	if err := os.WriteFile(statePath, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if other, err := New(cfg); err == nil {
		other.Close()
		t.Fatal("corrupt state was silently replaced")
	}
	after, _ := os.ReadFile(statePath)
	if string(after) != string(broken) {
		t.Fatal("corrupt evidence was overwritten")
	}
}

func TestSupervisorNoSideEffectWhenJournalWriteFails(t *testing.T) {
	cfg := supervisorConfig(t, "graceful")
	s := newTestSupervisor(t, cfg)
	statePath := filepath.Join(s.cfg.DataDir, "process-state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	req := domain.ProcessActionRequest{RequestID: newID(), InstanceID: s.cfg.InstanceID, Action: "start", ExpectedRevision: s.Snapshot().Revision}
	if _, err := s.Submit(req); err == nil {
		t.Fatal("accepted operation whose journal could not be saved")
	}
	if value := s.Snapshot(); value.PID != 0 || value.LastError == "" {
		t.Fatalf("unjournaled child started: %+v", value)
	}
	if err := os.Remove(statePath); err != nil { // empty test-created obstacle only
		t.Fatal(err)
	}
	s.Close()
	reopened := newTestSupervisor(t, cfg)
	if value := reopened.Snapshot(); value.DesiredState != "stopped" || value.PID != 0 || value.State != "stopped" {
		t.Fatalf("a rejected start became desired state after storage recovery: %+v", value)
	}
}

func TestSupervisorParentDeathKillsOnlyOwnedChild(t *testing.T) {
	root := t.TempDir()
	exe, _ := os.Executable()
	args := append(helperArgs("agent"), root)
	cmd := exec.Command(exe, args...)
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	agentDone := false
	t.Cleanup(func() {
		if !agentDone {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	var childPID int
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(root, "child.pid")); err == nil {
			childPID, _ = strconv.Atoi(string(data))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 1 || childPID == cmd.Process.Pid || childPID == os.Getpid() {
		t.Fatalf("no safe owned-child identity: %d", childPID)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	agentDone = true
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", childPID))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err == nil {
			parts := strings.SplitN(string(data), ") ", 2)
			if len(parts) == 2 && strings.HasPrefix(parts[1], "Z ") {
				return // exited; awaiting init's reaping, cannot own serial/OPC
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := os.ReadFile(filepath.Join(root, "state", "process-state.json"))
	var journal map[string]any
	_ = json.Unmarshal(state, &journal)
	t.Fatalf("agent crash orphaned its child %d; journal=%v", childPID, journal)
}
