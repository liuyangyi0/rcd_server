package opc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

const processLiveRoot = "/home/lenovo/code/rcd_device_groups_20260828/runs/lifecycle-20260828-v1/fixture"

type processLiveFixture struct {
	Root            string `json:"root"`
	ControlFile     string `json:"control_file"`
	State           string `json:"state"`
	SerialTimeoutMS int    `json:"serial_timeout_ms"`
	Sources         map[string]struct {
		PID                 int                `json:"pid"`
		AgentPID            int                `json:"agent_pid"`
		Endpoint            string             `json:"endpoint"`
		AgentURL            string             `json:"agent_url"`
		InstanceID          string             `json:"instance_id"`
		RunDir              string             `json:"run_dir"`
		PortID              string             `json:"port_id"`
		SendNodeID          string             `json:"send_node_id"`
		PTYSlave            string             `json:"pty_slave"`
		RequestedServerMode string             `json:"requested_server_mode"`
		CurrentValues       map[string]float64 `json:"current_values"`
	} `json:"sources"`
}

type processLiveIdentity struct {
	PID        int
	Executable string
	WorkDir    string
	StartTick  string
	ParentPID  int
}

type processLiveRequest struct {
	Input         api.ProcessActionInput
	AgentRevision string
	StartedAt     time.Time
}

type processLiveController struct {
	Base     string
	Binding  domain.ProcessBinding
	Requests []*processLiveRequest
}

// Deliberately serial and opt-in. Only lifecycle fixture B's owned child may
// stop/restart; the original Rust process, previous deployment, fixture A and
// both Go agents are read-only anchors. Never point this test at production.
func TestLiveProcessControlPreservesOPCSessionsAndSubscriptions(t *testing.T) {
	if os.Getenv("RCD_PROCESS_CONTROL_LIVE") != "1" {
		t.Skip("set RCD_PROCESS_CONTROL_LIVE=1 only for the isolated remote lifecycle fixture")
	}
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	apiURL := strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	control := os.Getenv("RCD_DEVICE_GROUPS_CONTROL")
	if runtime.GOOS != "linux" || !processLiveURL(endpoint, "opc.tcp", "4872") || !processLiveURL(apiURL, "http", "8086") || control != processLiveRoot+"/control.json" {
		t.Fatal("requires remote Linux lifecycle-v1 fixture, manager OPC4872/API8086 and its exact control.json; old deployments are forbidden")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	fixture, bindings, anchors := processLiveGuard(t, ctx, apiURL, control)
	var groups struct {
		Items []api.GroupView `json:"items"`
	}
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/groups", 0, nil, &groups, http.StatusOK)
	groupBaseline := processLiveGroups(groups.Items)
	if len(groupBaseline) != 3 {
		t.Fatalf("expected only three disposable groups, got %v", groupBaseline)
	}
	for _, id := range []string{"device-a", "device-b", "shared"} {
		if groupBaseline[id].DefaultSourceID != "lower-a" {
			t.Fatalf("fixture group %s must initially default to lower-a", id)
		}
	}
	t.Cleanup(func() {
		for _, before := range anchors {
			after, err := processLiveReadIdentity(before.PID)
			if err != nil || after != before {
				t.Errorf("protected process changed: before=%+v after=%+v err=%v", before, after, err)
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		var current struct {
			Items []api.GroupView `json:"items"`
		}
		status, err := liveOperationsHTTP(cleanupCtx, apiURL, http.MethodGet, "/api/v1/groups", "", nil, &current)
		if err != nil || status != http.StatusOK || !reflect.DeepEqual(processLiveGroups(current.Items), groupBaseline) {
			t.Errorf("group defaults/revisions changed: HTTP=%d err=%v groups=%v", status, err, processLiveGroups(current.Items))
		}
	})
	setACK := liveDeviceFaultControl(t, ctx, control)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	firstName, secondName := "process-live-b-"+nonce, "process-live-a-"+nonce
	first, _ := connectLiveManagerAnonymous(t, endpoint, firstName)
	second, _ := connectLiveManagerAnonymous(t, endpoint, secondName)
	liveDeviceFaultWrite(t, ctx, first, 3, "Groups/device-a/SelectedSourceId", "lower-b", gu.StatusOK)
	liveDeviceFaultWrite(t, ctx, second, 3, "Groups/device-a/SelectedSourceId", "lower-a", gu.StatusOK)
	firstSession := liveDeviceFaultSession(t, ctx, apiURL, firstName)
	secondSession := liveDeviceFaultSession(t, ctx, apiURL, secondName)
	processLiveAwaitPoint(t, ctx, first, 101, true)
	processLiveAwaitPoint(t, ctx, second, 11, true)
	firstNotifications := liveGroupsSubscribe(t, ctx, first, "A_VALUE")
	secondNotifications := liveGroupsSubscribe(t, ctx, second, "A_VALUE")
	liveGroupsAwaitValue(t, firstNotifications, 1, 101)
	liveGroupsAwaitValue(t, secondNotifications, 1, 11)
	controller := &processLiveController{Base: apiURL, Binding: bindings["b"]}
	// Every uncertain dispatch is reconciled with its original ID. A separate
	// recovery start is permitted only after every prior request is terminal
	// and the fixed B child is confirmed stopped/failed with no pending job.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		if err := controller.restoreRunning(cleanupCtx); err != nil {
			t.Errorf("restore fixed lifecycle B (no blind retries): %v", err)
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		for _, client := range []*gopcua.Client{first, second} {
			if err := client.Close(closeCtx); err != nil {
				t.Errorf("close lifecycle test Session: %v", err)
			}
		}
		closeCancel()
		sessionsCtx, sessionsCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer sessionsCancel()
		if err := processLiveWaitSessionsGone(sessionsCtx, apiURL, firstSession.ID, secondSession.ID); err != nil {
			t.Error(err)
		}
	})

	// Use the actual existing command protocol and wait for its real delayed
	// ACK. The maintenance admission must reject before sending an agent action.
	before, err := controller.status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setACK("success", 1000)
	liveDeviceFaultWrite(t, ctx, first, 3, "Groups/device-a/Send", "0109023010014D", gu.StatusOK)
	blocked := api.ProcessActionInput{Action: "stop", RequestID: "process-live-blocked-" + nonce, SourceRevision: controller.Binding.SourceRevision, BindingRevision: controller.Binding.Revision}
	controller.Requests = append(controller.Requests, &processLiveRequest{Input: blocked, AgentRevision: before.Revision, StartedAt: time.Now().UTC()})
	var rejection struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	status, err := liveOperationsHTTP(ctx, apiURL, http.MethodPost, "/api/v1/sources/lower-b/process/actions", before.Revision, blocked, &rejection)
	if err != nil || status != http.StatusConflict || rejection.Error.Code != "pending_command" {
		t.Fatalf("real pending Send must reject process action: HTTP=%d code=%q err=%v", status, rejection.Error.Code, err)
	}
	// The complete, expected 409 proves this request was rejected. Unexpected
	// responses leave the original fingerprint in cleanup's reconciliation log.
	controller.Requests = controller.Requests[:len(controller.Requests)-1]
	liveDeviceFaultAwaitSend(t, ctx, first, 3, "Groups/device-a/Send", "1", true)
	liveDeviceFaultNoPending(t, ctx, apiURL)
	unchanged, err := controller.status(ctx)
	if err != nil || unchanged.PID != before.PID || unchanged.Revision != before.Revision {
		t.Fatalf("rejected stop changed agent process: before=%+v after=%+v err=%v", before, unchanged, err)
	}
	setACK("success", 100)
	t.Logf("real _Send ACK interlock: process stop rejected HTTP409; B PID %d unchanged", before.PID)

	stopped, _, err := controller.action(ctx, "stop")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Snapshot == nil || stopped.Snapshot.State != "stopped" || stopped.Snapshot.PID != 0 {
		t.Fatalf("stop did not confirm exit: %+v", stopped)
	}
	liveDeviceFaultEventually(t, ctx, 5*time.Second, "owned B PID exits", func() bool {
		_, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", before.PID))
		return errors.Is(err, os.ErrNotExist)
	})
	processLiveAwaitPoint(t, ctx, first, 0, false)
	processLiveAwaitNotification(t, ctx, firstNotifications, 0, false, time.Time{})
	processLiveAssertOtherSession(t, ctx, second, secondNotifications)
	processLiveAssertRoutes(t, ctx, apiURL, first, second, firstSession, secondSession)
	t.Logf("stop B PID=%d: first Session Read/Subscribe publish zero with BadNoCommunication; second stays Good11 and selections are unchanged", before.PID)

	started, startRequest, err := controller.action(ctx, "start")
	if err != nil {
		t.Fatal(err)
	}
	processLiveAssertOwnedPID(t, started.Snapshot, fixture.Sources["b"].AgentPID)
	if started.Snapshot.PID == before.PID {
		t.Fatal("start reused the stopped B PID")
	}
	processLiveAwaitSource(t, ctx, apiURL, "lower-b", startRequest.StartedAt)
	processLiveAwaitPoint(t, ctx, first, 101, true)
	processLiveAwaitNotification(t, ctx, firstNotifications, 101, true, startRequest.StartedAt)
	processLiveAssertOtherSession(t, ctx, second, secondNotifications)
	processLiveAssertRoutes(t, ctx, apiURL, first, second, firstSession, secondSession)
	t.Logf("start B PID %d -> %d: original monitored item recovered Good101 without recreating either Session", before.PID, started.Snapshot.PID)

	restarted, restartRequest, err := controller.action(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	processLiveAssertOwnedPID(t, restarted.Snapshot, fixture.Sources["b"].AgentPID)
	if restarted.Snapshot.PID == started.Snapshot.PID {
		t.Fatal("restart did not replace the owned B PID")
	}
	processLiveAwaitSource(t, ctx, apiURL, "lower-b", restartRequest.StartedAt)
	processLiveAwaitPoint(t, ctx, first, 101, true)
	processLiveAwaitNotification(t, ctx, firstNotifications, 101, true, restartRequest.StartedAt)
	processLiveAssertOtherSession(t, ctx, second, secondNotifications)
	processLiveAssertRoutes(t, ctx, apiURL, first, second, firstSession, secondSession)
	// This replay is deliberate only AFTER a confirmed success, to verify the
	// public idempotency contract. Unknown actions are never replayed by helpers.
	var replay domain.ProcessOperation
	status, err = liveOperationsHTTP(ctx, apiURL, http.MethodPost, "/api/v1/sources/lower-b/process/actions", restartRequest.AgentRevision, restartRequest.Input, &replay)
	if err != nil || status != http.StatusOK || !reflect.DeepEqual(replay, restarted) {
		t.Fatalf("same-ID replay changed completed operation: HTTP=%d replay=%+v err=%v", status, replay, err)
	}
	timer := time.NewTimer(1500 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	after, err := controller.status(ctx)
	if err != nil || after.PID != restarted.Snapshot.PID || after.PendingRequestID != "" || after.PendingOperation != nil {
		t.Fatalf("same-ID replay restarted again or left a job: %+v err=%v", after, err)
	}
	processLiveAssertOtherSession(t, ctx, second, secondNotifications)
	processLiveAssertRoutes(t, ctx, apiURL, first, second, firstSession, secondSession)
	t.Logf("restart B PID %d -> %d and explicit same-ID replay: exactly one PID transition; both original Sessions/routes remain intact", started.Snapshot.PID, restarted.Snapshot.PID)
}

func processLiveURL(raw, scheme, port string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == scheme && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "192.168.10.46") &&
		u.Port() == port && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func processLiveReadIdentity(pid int) (processLiveIdentity, error) {
	if pid <= 1 {
		return processLiveIdentity{}, errors.New("invalid PID")
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return processLiveIdentity{}, err
	}
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return processLiveIdentity{}, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processLiveIdentity{}, err
	}
	separator := strings.LastIndex(string(stat), ") ")
	if separator < 0 {
		return processLiveIdentity{}, errors.New("invalid /proc stat")
	}
	fields := strings.Fields(string(stat)[separator+2:])
	if len(fields) < 20 {
		return processLiveIdentity{}, errors.New("short /proc stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return processLiveIdentity{}, err
	}
	return processLiveIdentity{PID: pid, Executable: exe, WorkDir: cwd, StartTick: fields[19], ParentPID: parent}, nil
}

func processLiveGuard(t *testing.T, ctx context.Context, base, control string) (processLiveFixture, map[string]domain.ProcessBinding, []processLiveIdentity) {
	t.Helper()
	root, err := filepath.EvalSymlinks(processLiveRoot)
	if err != nil || root != processLiveRoot {
		t.Fatalf("fixture root is not the exact isolated directory: %s %v", root, err)
	}
	manifestPath := filepath.Join(root, "Fixture.json")
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
		t.Fatalf("invalid fixture manifest %v", err)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture processLiveFixture
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.Root != root || fixture.ControlFile != control || fixture.State != "ready" || fixture.SerialTimeoutMS != 1500 || len(fixture.Sources) != 2 {
		t.Fatalf("fixture is not ready/disposable: %+v %v", fixture, err)
	}
	var overview api.Overview
	liveGroupsJSON(t, base, http.MethodGet, "/api/v1/overview", 0, nil, &overview, http.StatusOK)
	if overview.SourceCount != 2 || overview.ConnectedSources != 2 || overview.PendingCommands != 0 || overview.SessionCount != 0 || overview.CatalogConflicts != 0 {
		t.Fatalf("requires idle isolated two-source manager: %+v", overview)
	}
	bindings := make(map[string]domain.ProcessBinding)
	anchors := make([]processLiveIdentity, 0, 7)
	for _, pid := range []int{398713, 612417, 618222, 614880} {
		identity, err := processLiveReadIdentity(pid)
		if err != nil {
			t.Fatalf("protected earlier deployment PID %d: %v", pid, err)
		}
		if pid == 398713 && identity.Executable != "/home/lenovo/code/rcd/rcd_server" {
			t.Fatalf("original PID identity mismatch %+v", identity)
		}
		anchors = append(anchors, identity)
	}
	for index, key := range []string{"a", "b"} {
		owned := fixture.Sources[key]
		id := "lower-" + key
		dir := filepath.Join(root, key)
		if owned.InstanceID != "lifecycle-"+key || owned.AgentURL != "http://127.0.0.1:"+strconv.Itoa(8091+index) ||
			!processLiveURL(owned.Endpoint, "opc.tcp", strconv.Itoa(4870+index)) || owned.RunDir != dir || owned.PortID != filepath.Join(dir, "serial_"+key) ||
			owned.SendNodeID != "serial_"+key+"_Send" || owned.RequestedServerMode != "running" || !strings.HasPrefix(owned.PTYSlave, "/dev/pts/") ||
			owned.CurrentValues["A_VALUE"] != []float64{11, 101}[index] || owned.CurrentValues["B_VALUE"] != []float64{22, 202}[index] {
			t.Fatalf("unexpected physical source %s: %+v", key, owned)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(dir, "rcd_server"))
		if err != nil || resolved != filepath.Join(dir, "rcd_server") {
			t.Fatalf("lower executable must be a physical fixture copy: %s %v", resolved, err)
		}
		var source api.SourceView
		var topology api.SourceTopologyView
		var binding domain.ProcessBinding
		var state api.ProcessStatusView
		liveGroupsJSON(t, base, http.MethodGet, "/api/v1/sources/"+id, 0, nil, &source, http.StatusOK)
		liveGroupsJSON(t, base, http.MethodGet, "/api/v1/sources/"+id+"/topology", 0, nil, &topology, http.StatusOK)
		liveGroupsJSON(t, base, http.MethodGet, "/api/v1/sources/"+id+"/process/binding", 0, nil, &binding, http.StatusOK)
		liveGroupsJSON(t, base, http.MethodGet, "/api/v1/sources/"+id+"/process/status", 0, nil, &state, http.StatusOK)
		if !processLiveURL(source.Endpoint, "opc.tcp", strconv.Itoa(4870+index)) || strings.TrimRight(source.Endpoint, "/") != strings.TrimRight(owned.Endpoint, "/") || !source.Enabled || source.ConnectionState != "Connected" || source.SessionState != "Active" || source.SubscriptionState != "Active" || source.CatalogState != domain.CatalogReady ||
			!topology.Supported || len(topology.Ports) != 1 || len(topology.Ports[0].Devices) != 2 || topology.Ports[0].PortID != owned.PortID ||
			binding.SourceID != id || binding.SourceRevision != source.Revision || binding.Revision == 0 || binding.InstanceID != owned.InstanceID || binding.AgentURL != owned.AgentURL ||
			state.SourceID != id || state.SourceRevision != binding.SourceRevision || state.BindingRevision != binding.Revision || state.InstanceID != binding.InstanceID || state.Validate(binding.InstanceID) != nil ||
			state.State != "running" || state.DesiredState != "running" || state.PendingRequestID != "" || state.PendingOperation != nil || state.PID != owned.PID || state.Executable != resolved || state.WorkDir != dir {
			t.Fatalf("source/binding/agent does not identify the exact disposable child: source=%+v binding=%+v status=%+v", source, binding, state)
		}
		child, err := processLiveReadIdentity(owned.PID)
		if err != nil || child.Executable != resolved || child.WorkDir != dir || child.ParentPID != owned.AgentPID {
			t.Fatalf("wrong managed child %+v %v", child, err)
		}
		agent, err := processLiveReadIdentity(owned.AgentPID)
		if err != nil {
			t.Fatal(err)
		}
		anchors = append(anchors, agent)
		if key == "a" {
			anchors = append(anchors, child)
		}
		bindings[key] = binding
	}
	return fixture, bindings, anchors
}

func processLiveGroups(input []api.GroupView) map[string]domain.Group {
	result := make(map[string]domain.Group, len(input))
	for _, item := range input {
		result[item.ID] = item.Group
	}
	return result
}

func (c *processLiveController) status(ctx context.Context) (api.ProcessStatusView, error) {
	var result api.ProcessStatusView
	status, err := liveOperationsHTTP(ctx, c.Base, http.MethodGet, "/api/v1/sources/lower-b/process/status", "", nil, &result)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK || result.SourceID != c.Binding.SourceID || result.SourceRevision != c.Binding.SourceRevision || result.BindingRevision != c.Binding.Revision ||
		result.Executable != processLiveRoot+"/b/rcd_server" || result.WorkDir != processLiveRoot+"/b" {
		return result, fmt.Errorf("changed B process binding/status: HTTP=%d %+v", status, result)
	}
	return result, result.Validate(c.Binding.InstanceID)
}

func (c *processLiveController) action(ctx context.Context, action string) (domain.ProcessOperation, *processLiveRequest, error) {
	state, err := c.status(ctx)
	if err != nil {
		return domain.ProcessOperation{}, nil, err
	}
	if state.PendingRequestID != "" || state.PendingOperation != nil {
		return domain.ProcessOperation{}, nil, errors.New("a prior process request is still pending; refusing another action")
	}
	request := &processLiveRequest{Input: api.ProcessActionInput{Action: action, RequestID: "process-live-" + action + "-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SourceRevision: c.Binding.SourceRevision, BindingRevision: c.Binding.Revision}, AgentRevision: state.Revision, StartedAt: time.Now().UTC()}
	c.Requests = append(c.Requests, request)
	// One intentional dispatch. Even if its response is lost, await() only
	// performs GET/Resolve with this exact fingerprint, never another Action.
	var accepted domain.ProcessOperation
	status, postErr := liveOperationsHTTP(ctx, c.Base, http.MethodPost, "/api/v1/sources/lower-b/process/actions", request.AgentRevision, request.Input, &accepted)
	operation, err := c.await(ctx, request)
	if postErr != nil || (status != http.StatusAccepted && status != http.StatusOK) {
		return operation, request, fmt.Errorf("single %s dispatch HTTP=%d error=%v; original-ID reconciliation state=%s error=%v", action, status, postErr, operation.State, err)
	}
	want := domain.ProcessActionRequest{RequestID: request.Input.RequestID, InstanceID: c.Binding.InstanceID, Action: action, ExpectedRevision: request.AgentRevision}
	if accepted.SourceID != c.Binding.SourceID || accepted.SourceRevision != c.Binding.SourceRevision || accepted.BindingRevision != c.Binding.Revision || accepted.Validate(want) != nil {
		return operation, request, fmt.Errorf("invalid accepted operation response: %+v", accepted)
	}
	if err == nil && operation.State != "succeeded" {
		err = fmt.Errorf("%s failed: %+v", action, operation)
	}
	return operation, request, err
}

func (c *processLiveController) await(ctx context.Context, request *processLiveRequest) (domain.ProcessOperation, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	want := domain.ProcessActionRequest{RequestID: request.Input.RequestID, InstanceID: c.Binding.InstanceID, Action: request.Input.Action, ExpectedRevision: request.AgentRevision}
	for {
		var operation domain.ProcessOperation
		var raw json.RawMessage
		status, err := liveOperationsHTTP(waitCtx, c.Base, http.MethodGet, "/api/v1/sources/lower-b/process/operations/"+url.PathEscape(want.RequestID), "", nil, &raw)
		if err == nil && status == http.StatusNotFound {
			status, err = liveOperationsHTTP(waitCtx, c.Base, http.MethodPost, "/api/v1/sources/lower-b/process/operations/"+url.PathEscape(want.RequestID)+"/resolve", request.AgentRevision, request.Input, &raw)
		}
		if err == nil && (status == http.StatusOK || status == http.StatusAccepted) {
			if err := json.Unmarshal(raw, &operation); err != nil {
				return operation, fmt.Errorf("decode process operation: %w", err)
			}
			if operation.SourceID != c.Binding.SourceID || operation.SourceRevision != c.Binding.SourceRevision || operation.BindingRevision != c.Binding.Revision {
				return operation, errors.New("operation target/revisions changed")
			}
			if err := operation.Validate(want); err != nil {
				return operation, err
			}
			if operation.Terminal() {
				return operation, nil
			}
		}
		select {
		case <-waitCtx.Done():
			return operation, fmt.Errorf("original request %s remains unconfirmed: %w (last HTTP=%d err=%v)", want.RequestID, waitCtx.Err(), status, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *processLiveController) restoreRunning(ctx context.Context) error {
	for _, request := range c.Requests {
		if _, err := c.await(ctx, request); err != nil {
			return err
		}
	}
	for {
		var overview api.Overview
		status, err := liveOperationsHTTP(ctx, c.Base, http.MethodGet, "/api/v1/overview", "", nil, &overview)
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("check cleanup command locks HTTP=%d err=%v", status, err)
		}
		if overview.PendingCommands == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("commands still pending before cleanup: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	state, err := c.status(ctx)
	if err != nil {
		return err
	}
	if state.PendingOperation != nil || state.PendingRequestID != "" {
		return errors.New("unknown pending request blocks recovery start")
	}
	if state.State != "running" || state.DesiredState != "running" {
		if state.State != "stopped" && state.State != "failed" {
			return fmt.Errorf("cannot safely recover transitional state %s", state.State)
		}
		if _, _, err := c.action(ctx, "start"); err != nil {
			return err
		}
	}
	for {
		var source api.SourceView
		status, err := liveOperationsHTTP(ctx, c.Base, http.MethodGet, "/api/v1/sources/lower-b", "", nil, &source)
		if err == nil && status == http.StatusOK && source.ConnectionState == "Connected" && source.SessionState == "Active" && source.SubscriptionState == "Active" && source.CatalogState == domain.CatalogReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("B not reconnected after restoration: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func processLiveRead(t *testing.T, ctx context.Context, client *gopcua.Client, ns uint16, id string) *gu.DataValue {
	t.Helper()
	response, err := client.Read(ctx, &gu.ReadRequest{TimestampsToReturn: gu.TimestampsToReturnBoth, NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, id), AttributeID: gu.AttributeIDValue}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil {
		t.Fatalf("Read %s failed without recreating the Session: %#v %v", id, response, err)
	}
	return response.Results[0]
}

func processLiveValue(value *gu.DataValue, want float64, good bool) bool {
	if value == nil || value.Value == nil {
		return false
	}
	actual, err := strconv.ParseFloat(fmt.Sprint(value.Value.Value()), 64)
	if err != nil || actual != want {
		return false
	}
	if good {
		return domain.StatusCode(value.Status).IsGood()
	}
	return gu.StatusCode(uint32(value.Status)&0xffff0000) == gu.StatusBadNoCommunication
}

func processLiveAwaitPoint(t *testing.T, ctx context.Context, client *gopcua.Client, want float64, good bool) {
	t.Helper()
	var last *gu.DataValue
	liveDeviceFaultEventually(t, ctx, 35*time.Second, fmt.Sprintf("A_VALUE=%v good=%t", want, good), func() bool {
		last = processLiveRead(t, ctx, client, 2, "A_VALUE")
		return processLiveValue(last, want, good)
	})
	t.Logf("OPC Read %s", liveGroupsDescribeValue(last))
}

func processLiveAwaitNotification(t *testing.T, ctx context.Context, notifications <-chan *gopcua.PublishNotificationData, want float64, good bool, after time.Time) {
	t.Helper()
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	var last *gu.DataValue
	for {
		select {
		case notification, open := <-notifications:
			if !open {
				t.Fatal("original OPC subscription closed")
			}
			if notification == nil {
				continue
			}
			if notification.Error != nil {
				t.Fatalf("original subscription failed: %v", notification.Error)
			}
			change, ok := notification.Value.(*gu.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range change.MonitoredItems {
				if item != nil && item.ClientHandle == 1 {
					last = item.Value
					// Rust intentionally omits SourceTimestamp on pushed values to
					// avoid its library's duplicate sampling bug. Do not invent or
					// require that timestamp; fence stale queued notifications by
					// the manager's ServerTimestamp and separately check fresh lower
					// data after the action in processLiveAwaitSource.
					if processLiveValue(last, want, good) && (after.IsZero() || !last.ServerTimestamp.Before(after)) {
						t.Logf("original monitored item %s", liveGroupsDescribeValue(last))
						return
					}
				}
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			t.Fatalf("original subscription did not reach A_VALUE=%v good=%t after=%s last=%s", want, good, after, liveGroupsDescribeValue(last))
		}
	}
}

func processLiveAssertOtherSession(t *testing.T, ctx context.Context, client *gopcua.Client, notifications <-chan *gopcua.PublishNotificationData) {
	t.Helper()
	if value := processLiveRead(t, ctx, client, 2, "A_VALUE"); !processLiveValue(value, 11, true) {
		t.Fatalf("untouched Session lost Good11: %s", liveGroupsDescribeValue(value))
	}
	for {
		select {
		case notification, open := <-notifications:
			if !open {
				t.Fatal("untouched Session subscription closed")
			}
			if notification == nil {
				continue
			}
			if notification.Error != nil {
				t.Fatal(notification.Error)
			}
			if change, ok := notification.Value.(*gu.DataChangeNotification); ok {
				for _, item := range change.MonitoredItems {
					if item != nil && item.ClientHandle == 1 && !processLiveValue(item.Value, 11, true) {
						t.Fatalf("B lifecycle contaminated A subscription: %s", liveGroupsDescribeValue(item.Value))
					}
				}
			}
		default:
			return
		}
	}
}

func processLiveAssertRoutes(t *testing.T, ctx context.Context, base string, first, second *gopcua.Client, beforeFirst, beforeSecond api.SessionView) {
	t.Helper()
	for _, item := range []struct {
		client *gopcua.Client
		want   string
	}{{first, "lower-b"}, {second, "lower-a"}} {
		value := processLiveRead(t, ctx, item.client, 3, "Groups/device-a/SelectedSourceId")
		if !liveGroupsDataValueHasStatus(value, gu.StatusOK) || fmt.Sprint(value.Value.Value()) != item.want {
			t.Fatalf("selection silently changed: %s want %s", liveGroupsDescribeValue(value), item.want)
		}
	}
	var sessions struct {
		Items []api.SessionView `json:"items"`
	}
	liveGroupsJSON(t, base, http.MethodGet, "/api/v1/sessions", 0, nil, &sessions, http.StatusOK)
	for _, before := range []api.SessionView{beforeFirst, beforeSecond} {
		found := false
		for _, after := range sessions.Items {
			if after.ID == before.ID {
				found = true
				if !reflect.DeepEqual(after.Routes, before.Routes) {
					t.Fatalf("Session routes/revisions changed: before=%+v after=%+v", before.Routes, after.Routes)
				}
			}
		}
		if !found {
			t.Fatalf("original Session %s was replaced or lost", before.ID)
		}
	}
}

func processLiveAssertOwnedPID(t *testing.T, snapshot *domain.ProcessSnapshot, agentPID int) {
	t.Helper()
	if snapshot == nil || snapshot.State != "running" || snapshot.PID <= 1 {
		t.Fatalf("missing running snapshot %+v", snapshot)
	}
	identity, err := processLiveReadIdentity(snapshot.PID)
	if err != nil || identity.Executable != processLiveRoot+"/b/rcd_server" || identity.WorkDir != processLiveRoot+"/b" || identity.ParentPID != agentPID {
		t.Fatalf("new B PID is not the fixed owned child: %+v %v", identity, err)
	}
}

func processLiveAwaitSource(t *testing.T, ctx context.Context, base, id string, after time.Time) {
	t.Helper()
	liveDeviceFaultEventually(t, ctx, 40*time.Second, "fresh lower OPC data after lifecycle", func() bool {
		var source api.SourceView
		liveGroupsJSON(t, base, http.MethodGet, "/api/v1/sources/"+id, 0, nil, &source, http.StatusOK)
		return source.ConnectionState == "Connected" && source.SessionState == "Active" && source.SubscriptionState == "Active" && source.CatalogState == domain.CatalogReady && source.LastDataAt.After(after)
	})
}

func processLiveWaitSessionsGone(ctx context.Context, base string, ids ...string) error {
	for {
		var sessions struct {
			Items []api.SessionView `json:"items"`
		}
		status, err := liveOperationsHTTP(ctx, base, http.MethodGet, "/api/v1/sessions", "", nil, &sessions)
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("verify test Session cleanup HTTP=%d err=%v", status, err)
		}
		found := false
		for _, session := range sessions.Items {
			for _, id := range ids {
				found = found || session.ID == id
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("test Sessions not removed: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
