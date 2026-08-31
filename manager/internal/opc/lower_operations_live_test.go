package opc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

type liveOperationsDevice struct {
	ID          string `json:"device_id"`
	ReadEnabled bool   `json:"read_enabled"`
	AppliedRead bool   `json:"applied_read_enabled"`
}

type liveOperationsPort struct {
	ID             string                 `json:"port_id"`
	Enabled        bool                   `json:"enabled"`
	AppliedEnabled bool                   `json:"applied_enabled"`
	Open           bool                   `json:"open"`
	Pending        int                    `json:"pending_commands"`
	ControlPending bool                   `json:"control_pending"`
	Devices        []liveOperationsDevice `json:"devices"`
}

type liveOperationsStatus struct {
	SchemaVersion  int                  `json:"schema_version"`
	SourceID       string               `json:"source_id"`
	SourceRevision uint64               `json:"source_revision"`
	Revision       string               `json:"revision"`
	Status         string               `json:"status"`
	Ports          []liveOperationsPort `json:"ports"`
}

// Explicitly destructive, remote opt-in only. This test changes only the
// matched disposable lower-b PTY's acquisition flags and ACK fixture settings.
// It never changes group defaults, a different Session, or configuration text.
func TestLiveDeviceGroupsLowerOperationsCommandInterlock(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	apiURL := strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	controlPath := os.Getenv("RCD_DEVICE_GROUPS_CONTROL")
	if os.Getenv("RCD_LOWER_OPERATIONS_LIVE") != "1" || endpoint == "" || apiURL == "" || controlPath == "" {
		t.Skip("set RCD_LOWER_OPERATIONS_LIVE=1 and RCD_DEVICE_GROUPS_ENDPOINT/API/CONTROL for the disposable Rust/PTY fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var fixture struct {
		SerialTimeoutMS int `json:"serial_timeout_ms"`
		Sources         map[string]struct {
			Endpoint   string `json:"endpoint"`
			PortID     string `json:"port_id"`
			SendNodeID string `json:"send_node_id"`
			ServerMode string `json:"requested_server_mode"`
		} `json:"sources"`
	}
	manifest, err := os.Open(filepath.Join(filepath.Dir(controlPath), "Fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(io.LimitReader(manifest, 128<<10)).Decode(&fixture)
	manifest.Close()
	if err != nil || fixture.SerialTimeoutMS != 1500 {
		t.Fatalf("need the isolated 1500ms serial fixture: err=%v timeout=%d", err, fixture.SerialTimeoutMS)
	}
	sources := make(map[string]api.SourceView)
	baselines := make(map[string]liveOperationsStatus)
	for _, key := range []string{"a", "b"} {
		id := "lower-" + key
		var source api.SourceView
		var topology api.SourceTopologyView
		var capabilities domain.OperationsCapabilities
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/"+id, 0, nil, &source, http.StatusOK)
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/"+id+"/topology", 0, nil, &topology, http.StatusOK)
		owned := fixture.Sources[key]
		if owned.Endpoint == "" || source.Endpoint != owned.Endpoint || owned.ServerMode != "running" || !topology.Supported || len(topology.Ports) != 1 || len(topology.Ports[0].Devices) != 2 || topology.Ports[0].PortID != owned.PortID || topology.Ports[0].SendNodeID != owned.SendNodeID {
			t.Fatalf("%s does not match the disposable two-device fixture: source=%#v topology=%#v fixture=%#v", id, source, topology, owned)
		}
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/"+id+"/operations/capabilities", 0, nil, &capabilities, http.StatusOK)
		if !capabilities.Supported || capabilities.SchemaVersion != 1 || !capabilities.HasFeature("acquisition") || capabilities.SourceID != id || capabilities.SourceRevision != source.Revision {
			t.Fatalf("%s capabilities are not pinned to the expected source: %#v", id, capabilities)
		}
		state, err := liveOperationsReadStatus(ctx, apiURL, id)
		if err != nil || state.SchemaVersion != 1 || state.SourceID != id || state.SourceRevision != source.Revision || len(state.Ports) != 1 || state.Ports[0].ID != owned.PortID {
			t.Fatalf("%s operations snapshot=%#v err=%v", id, state, err)
		}
		port := state.Ports[0]
		if !port.Enabled || !port.AppliedEnabled || !port.Open || port.Pending != 0 || port.ControlPending || !reflect.DeepEqual(port.Devices, []liveOperationsDevice{{"1", true, true}, {"2", true, true}}) {
			t.Fatalf("start only with both fixture stations enabled and idle: %s=%#v", id, state)
		}
		sources[key], baselines[key] = source, state
	}
	setACK := liveDeviceFaultControl(t, ctx, controlPath)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	clientName := "lower-operations-" + nonce
	upper, _ := connectLiveManagerAnonymous(t, endpoint, clientName)
	lowerA, _ := connectLiveManagerAnonymous(t, sources["a"].Endpoint, clientName+"-a")
	lowerB, _ := connectLiveManagerAnonymous(t, sources["b"].Endpoint, clientName+"-b")
	nsB, ok := namespaceIndex(lowerB.Namespaces())[domain.OperationsNamespaceURI]
	if !ok {
		t.Fatal("Rust operations namespace missing")
	}
	sessionID := liveDeviceFaultSession(t, ctx, apiURL, clientName).ID
	// Registered after the clients, so restoration runs while they are alive.
	// Its independent budget also runs when the test's 90s deadline is exhausted.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cleanupCancel()
		if err := liveOperationsRestore(cleanupCtx, apiURL, baselines["b"]); err != nil {
			t.Errorf("restore lower-b physical acquisition flags: %v", err)
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := upper.Close(closeCtx); err != nil {
			t.Errorf("close operations test Session: %v", err)
		}
		closeCancel()
		removedCtx, removedCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer removedCancel()
		for {
			var sessions struct {
				Items []api.SessionView `json:"items"`
			}
			status, err := liveOperationsHTTP(removedCtx, apiURL, http.MethodGet, "/api/v1/sessions", "", nil, &sessions)
			if err != nil || status != http.StatusOK {
				t.Errorf("verify operations Session cleanup: HTTP=%d err=%v", status, err)
				return
			}
			found := false
			for _, session := range sessions.Items {
				found = found || session.ID == sessionID
			}
			if !found {
				return
			}
			select {
			case <-removedCtx.Done():
				t.Error("closed operations test Session remains in the online list")
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	liveDeviceFaultWrite(t, ctx, upper, 3, "Groups/device-a/SelectedSourceId", "lower-b", gu.StatusOK)
	liveDeviceFaultWrite(t, ctx, upper, 3, "Groups/device-b/SelectedSourceId", "lower-b", gu.StatusOK)
	portID := fixture.Sources["b"].PortID
	sendID := fixture.Sources["b"].SendNodeID
	knownCommands := make(map[string]bool)
	readEnabled := false
	if _, err := liveOperationsApply(ctx, apiURL, baselines["b"], "device-read", "1", readEnabled); err != nil {
		t.Fatal(err)
	}
	liveOperationsAwaitPoint(t, ctx, upper, "A_VALUE", 0, false)
	liveOperationsAwaitPoint(t, ctx, lowerB, "A_VALUE", 0, false)
	liveOperationsAwaitPoint(t, ctx, upper, "B_VALUE", 202, true)
	liveOperationsAwaitPoint(t, ctx, lowerA, "A_VALUE", 11, true)
	setACK("success", 300)
	liveDeviceFaultWrite(t, ctx, upper, 3, "Groups/device-a/Send", "0109023010014D", gu.StatusOK)
	liveDeviceFaultAwaitSend(t, ctx, upper, 3, "Groups/device-a/Send", "1", true)
	liveDeviceFaultAudit(t, ctx, apiURL, sessionID, strings.TrimSuffix(sendID, "_Send"), knownCommands, domain.CommandReceived)
	liveDeviceFaultNoPending(t, ctx, apiURL)
	liveOperationsAwaitPoint(t, ctx, upper, "A_VALUE", 0, false)
	t.Log("device 1 stopped reading; its command completed with a real ACK while station 2 and the other source remained Good")
	current, err := liveOperationsReadStatus(ctx, apiURL, "lower-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := liveOperationsApply(ctx, apiURL, current, "device-read", "1", true); err != nil {
		t.Fatal(err)
	}
	liveOperationsAwaitPoint(t, ctx, upper, "A_VALUE", 101, true)
	setACK("success", 1000)
	for _, item := range []struct{ suffix, action, device string }{
		{"port", "set_port_enabled", ""},
		{"device-read", "set_device_read_enabled", "1"},
	} {
		before, err := liveOperationsReadStatus(ctx, apiURL, "lower-b")
		if err != nil {
			t.Fatal(err)
		}
		liveDeviceFaultWrite(t, ctx, upper, 3, "Groups/device-a/Send", "0109023010014D", gu.StatusOK)
		// Wait for the actual worker's Waiting value before probing both layers.
		// Each action gets a fresh 1000ms ACK window, below the 1500ms timeout.
		liveDeviceFaultAwaitSend(t, ctx, lowerB, 2, sendID, "2", true)
		request := domain.OperationRequest{Action: item.action, PortID: portID, DeviceID: item.device, Enabled: &readEnabled, ExpectedRevision: before.Revision}
		_, err = liveOperationsCall(ctx, lowerB, nsB, request)
		var busy *domain.OperationError
		if !errors.As(err, &busy) || busy.Code != "busy" || busy.CurrentRevision != before.Revision {
			t.Fatalf("direct lower %s during ACK must return JSON busy at revision=%s, got %v", item.action, before.Revision, err)
		}
		body := liveOperationsControlBody(before, item.device, false)
		var response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		status, err := liveOperationsHTTP(ctx, apiURL, http.MethodPut, "/api/v1/sources/lower-b/operations/"+item.suffix, before.Revision, body, &response)
		if err != nil || status != http.StatusConflict || response.Error.Code != "pending_command" {
			t.Fatalf("manager %s during ACK: HTTP=%d error=%s transport=%v; want 409 pending_command", item.action, status, response.Error.Code, err)
		}
		liveDeviceFaultAwaitSend(t, ctx, upper, 3, "Groups/device-a/Send", "1", true)
		liveDeviceFaultAudit(t, ctx, apiURL, sessionID, strings.TrimSuffix(sendID, "_Send"), knownCommands, domain.CommandReceived)
		liveDeviceFaultNoPending(t, ctx, apiURL)
		after, err := liveOperationsReadStatus(ctx, apiURL, "lower-b")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected %s changed revision/desired/applied/open state: before=%#v after=%#v err=%v", item.action, before, after, err)
		}
		t.Logf("%s: direct OPC JSON busy and manager HTTP 409 pending_command; revision=%s and physical flags unchanged; ACK completed", item.action, before.Revision)
	}
	untouched, err := liveOperationsReadStatus(ctx, apiURL, "lower-a")
	if err != nil || !reflect.DeepEqual(untouched, baselines["a"]) {
		t.Fatalf("physical operations changed another source: before=%#v after=%#v err=%v", baselines["a"], untouched, err)
	}
	liveOperationsAwaitPoint(t, ctx, upper, "A_VALUE", 101, true)
	liveOperationsAwaitPoint(t, ctx, upper, "B_VALUE", 202, true)
	liveOperationsAwaitPoint(t, ctx, lowerA, "A_VALUE", 11, true)
}

// This optional case only creates its own readonly subscription. It neither
// reconfigures nor sends acquisition/control commands to the legacy lower.
func TestLiveDeviceGroupsLowerOperationsLegacyCapabilities(t *testing.T) {
	endpoint := os.Getenv("RCD_LOWER_OPERATIONS_LEGACY_ENDPOINT")
	if os.Getenv("RCD_LOWER_OPERATIONS_LIVE") != "1" || endpoint == "" {
		t.Skip("set RCD_LOWER_OPERATIONS_LIVE=1 and RCD_LOWER_OPERATIONS_LEGACY_ENDPOINT for readonly old-lower compatibility")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	supervisor, err := NewLowerSupervisor(LowerConfig{Source: domain.Source{ID: "legacy-operations-probe", GroupID: "legacy-probe", Endpoint: endpoint}, OperationTimeout: 10 * time.Second}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("legacy readonly supervisor exit: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("legacy readonly supervisor did not exit after cancellation")
		}
	})
	liveDeviceFaultEventually(t, ctx, 35*time.Second, "legacy subscription becomes active", func() bool {
		status := supervisor.Status()
		return status.Connection == ConnectionConnected && status.Subscription == SubscriptionActive && status.Catalog == domain.CatalogReady
	})
	epoch := supervisor.Status().Epoch
	capabilities, err := supervisor.OperationsCapabilities(ctx, epoch)
	if err != nil || capabilities.Supported || capabilities.Reason == "" || len(capabilities.Features) != 0 {
		t.Fatalf("legacy capabilities=%#v err=%v; want explicit supported=false", capabilities, err)
	}
	_, err = supervisor.ExecuteOperation(ctx, domain.OperationRequest{Action: "status"}, epoch)
	var unsupported *domain.OperationError
	if !errors.As(err, &unsupported) || unsupported.Code != "unsupported" {
		t.Fatalf("legacy readonly status request=%v; want unsupported, not a connection failure", err)
	}
	if supervisor.Status().Connection != ConnectionConnected || supervisor.Status().Epoch != epoch {
		t.Fatal("unsupported operations disrupted the legacy data connection")
	}
	t.Log("legacy lower retains its live data subscription and reports operations supported=false")
}

func liveOperationsHTTP(ctx context.Context, base, method, path, revision string, body, output any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Origin", base)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if revision != "" {
		request.Header.Set("If-Match", strconv.Quote(revision))
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return response.StatusCode, err
	}
	if len(data) > 2<<20 {
		return response.StatusCode, fmt.Errorf("%s response exceeds limit", path)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return response.StatusCode, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return response.StatusCode, nil
}

func liveOperationsReadStatus(ctx context.Context, base, source string) (liveOperationsStatus, error) {
	var result liveOperationsStatus
	status, err := liveOperationsHTTP(ctx, base, http.MethodGet, "/api/v1/sources/"+source+"/operations/status", "", nil, &result)
	if err == nil && (status != http.StatusOK || result.Revision == "" || len(result.Ports) != 1) {
		err = fmt.Errorf("%s status HTTP=%d invalid snapshot=%#v", source, status, result)
	}
	return result, err
}

func liveOperationsControlBody(state liveOperationsStatus, device string, enabled bool) map[string]any {
	body := map[string]any{"source_revision": state.SourceRevision, "port_id": state.Ports[0].ID, "enabled": enabled}
	if device != "" {
		body["device_id"] = device
	}
	return body
}

// One dispatch only. A timeout must never cause this helper to resubmit a
// possibly applied operation; it polls actual state and lets cleanup inspect it.
func liveOperationsApply(ctx context.Context, base string, before liveOperationsStatus, suffix, device string, enabled bool) (liveOperationsStatus, error) {
	var response liveOperationsStatus
	status, err := liveOperationsHTTP(ctx, base, http.MethodPut, "/api/v1/sources/"+before.SourceID+"/operations/"+suffix, before.Revision, liveOperationsControlBody(before, device, enabled), &response)
	if err != nil || status != http.StatusOK || (response.Status != "Applied" && response.Status != "Pending") {
		return response, fmt.Errorf("set %s %s=%t: HTTP=%d response=%#v err=%v", before.SourceID, suffix, enabled, status, response, err)
	}
	revision, err := strconv.ParseUint(before.Revision, 10, 64)
	if err != nil || response.Revision != strconv.FormatUint(revision+1, 10) {
		return response, fmt.Errorf("control revision did not advance once: %s -> %s", before.Revision, response.Revision)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		state, err := liveOperationsReadStatus(waitCtx, base, before.SourceID)
		if err != nil {
			return state, err
		}
		port := state.Ports[0]
		applied := port.Enabled == enabled && port.AppliedEnabled == enabled && port.Open == enabled
		if device != "" {
			applied = false
			for _, item := range port.Devices {
				if item.ID == device {
					applied = item.ReadEnabled == enabled && item.AppliedRead == enabled
				}
			}
		}
		if !port.ControlPending && applied {
			if state.Revision != response.Revision || state.SourceRevision != before.SourceRevision {
				return state, fmt.Errorf("physical/source revision changed while awaiting the captured control")
			}
			return state, nil
		}
		select {
		case <-waitCtx.Done():
			return state, fmt.Errorf("control was not applied: %w snapshot=%#v", waitCtx.Err(), state)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func liveOperationsRestore(ctx context.Context, base string, original liveOperationsStatus) error {
	var state liveOperationsStatus
	for {
		var err error
		state, err = liveOperationsReadStatus(ctx, base, original.SourceID)
		if err != nil {
			return err
		}
		var group api.GroupView
		status, err := liveOperationsHTTP(ctx, base, http.MethodGet, "/api/v1/groups/device-a", "", nil, &group)
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("check pending before restoration: HTTP=%d err=%v", status, err)
		}
		if state.Ports[0].Pending == 0 && !state.Ports[0].ControlPending && group.PendingCommands == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("physical command/control still pending during restoration: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if state.SourceRevision != original.SourceRevision || state.Ports[0].ID != original.Ports[0].ID {
		return errors.New("source configuration changed; refusing to restore through a different physical target")
	}
	port, wanted := state.Ports[0], original.Ports[0]
	if port.Enabled != wanted.Enabled || port.AppliedEnabled != wanted.AppliedEnabled || port.Open != wanted.Open {
		var err error
		state, err = liveOperationsApply(ctx, base, state, "port", "", wanted.Enabled)
		if err != nil {
			return err
		}
	}
	for _, device := range wanted.Devices {
		for _, actual := range state.Ports[0].Devices {
			if actual.ID == device.ID && (actual.ReadEnabled != device.ReadEnabled || actual.AppliedRead != device.AppliedRead) {
				var err error
				state, err = liveOperationsApply(ctx, base, state, "device-read", device.ID, device.ReadEnabled)
				if err != nil {
					return err
				}
			}
		}
	}
	if !reflect.DeepEqual(state.Ports, original.Ports) {
		return fmt.Errorf("restoration flags differ: original=%#v actual=%#v", original.Ports, state.Ports)
	}
	return nil
}

func liveOperationsCall(ctx context.Context, client *gopcua.Client, ns uint16, request domain.OperationRequest) (json.RawMessage, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	response, err := client.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(ns, "Operations"), MethodID: gu.NewStringNodeID(ns, "Operations/Execute"), InputArguments: []*gu.Variant{gu.MustVariant(string(payload))}})
	if err != nil {
		return nil, err
	}
	if response == nil || response.StatusCode != gu.StatusOK || len(response.OutputArguments) != 1 || response.OutputArguments[0] == nil {
		return nil, fmt.Errorf("business rejection must retain a successful OPC Call: %#v", response)
	}
	content, ok := response.OutputArguments[0].Value().(string)
	if !ok {
		return nil, errors.New("operations response is not a String")
	}
	return decodeOperationEnvelope(content)
}

func liveOperationsAwaitPoint(t *testing.T, ctx context.Context, client *gopcua.Client, identifier string, expected float64, good bool) {
	t.Helper()
	ns, ok := namespaceIndex(client.Namespaces())[BusinessNamespaceURI]
	if !ok {
		t.Fatal("business namespace missing")
	}
	liveDeviceFaultEventually(t, ctx, 8*time.Second, "operations point "+identifier, func() bool {
		response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, identifier), AttributeID: gu.AttributeIDValue}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil {
			t.Fatalf("read operations point %s: response=%#v err=%v", identifier, response, err)
		}
		sample := response.Results[0]
		if !good {
			return gu.StatusCode(uint32(sample.Status)&0xffff0000) == gu.StatusBadNoCommunication
		}
		if !domain.StatusCode(sample.Status).IsGood() || sample.Value == nil {
			return false
		}
		value, err := strconv.ParseFloat(fmt.Sprint(sample.Value.Value()), 64)
		return err == nil && value == expected
	})
}
