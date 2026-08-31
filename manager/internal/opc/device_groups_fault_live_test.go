package opc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Opt-in destructive protocol tests for the disposable Linux Rust/PTY fixture.
// No environment means no network, file mutation, or hardware access. This
// test is deliberately not parallel: the fixture has one physical b port.
func TestLiveDeviceGroupsCommandFaultsAndClosedSession(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	apiURL := strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	controlPath := os.Getenv("RCD_DEVICE_GROUPS_CONTROL")
	if endpoint == "" || apiURL == "" || controlPath == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT, RCD_DEVICE_GROUPS_API and RCD_DEVICE_GROUPS_CONTROL for the isolated Rust/PTY fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var topologyA, topologyB api.SourceTopologyView
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/lower-a/topology", 0, nil, &topologyA, http.StatusOK)
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/lower-b/topology", 0, nil, &topologyB, http.StatusOK)
	if !topologyA.Supported || !topologyB.Supported || len(topologyA.Ports) != 1 || len(topologyB.Ports) != 1 {
		t.Fatalf("need the isolated two-source Rust/PTY topology: a=%#v b=%#v", topologyA, topologyB)
	}
	// Prove the supplied control file belongs to the lower-b configured in this
	// manager, before changing even a fixture-only setting.
	var fixture struct {
		SerialTimeoutMS int `json:"serial_timeout_ms"`
		Sources         map[string]struct {
			PortID     string `json:"port_id"`
			SendNodeID string `json:"send_node_id"`
			ServerMode string `json:"requested_server_mode"`
		} `json:"sources"`
	}
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(controlPath), "Fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifest, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SerialTimeoutMS != 1500 || fixture.Sources["b"].PortID != topologyB.Ports[0].PortID || fixture.Sources["b"].SendNodeID != topologyB.Ports[0].SendNodeID || fixture.Sources["b"].ServerMode != "running" {
		t.Fatalf("control file does not identify the running 1500ms lower-b PTY fixture: %#v", fixture)
	}
	setACK := liveDeviceFaultControl(t, ctx, controlPath)
	legacySend := topologyA.Ports[0].SendNodeID
	physicalPort := strings.TrimSuffix(topologyB.Ports[0].SendNodeID, "_Send")
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	clientName := "device-faults-" + nonce
	client, _ := connectLiveManagerAnonymous(t, endpoint, clientName)
	sessionID := liveDeviceFaultSession(t, ctx, apiURL, clientName).ID
	liveDeviceFaultWrite(t, ctx, client, 3, "Groups/device-a/SelectedSourceId", "lower-b", gu.StatusOK)
	knownCommands := make(map[string]bool)

	for _, mode := range []string{"bad", "drop"} {
		setACK(mode, 100)
		started := time.Now()
		liveDeviceFaultWrite(t, ctx, client, 2, legacySend, "0109023010014D", gu.StatusOK)
		liveDeviceFaultAwaitSend(t, ctx, client, 2, legacySend, "3", false)
		failed := liveDeviceFaultAudit(t, ctx, apiURL, sessionID, physicalPort, knownCommands, domain.CommandFailed)
		if failed.StatusCode.IsGood() || failed.Error == "" {
			t.Fatalf("%s ACK failure lost bad quality/error in audit: %#v", mode, failed)
		}
		liveDeviceFaultNoPending(t, ctx, apiURL)
		// Exercise a real selection change, not just the PendingCommandCount
		// display, to prove the failed command released the group lock.
		liveDeviceFaultWrite(t, ctx, client, 3, "Groups/device-a/SelectedSourceId", "lower-a", gu.StatusOK)
		liveDeviceFaultWrite(t, ctx, client, 3, "Groups/device-a/SelectedSourceId", "lower-b", gu.StatusOK)
		setACK("success", 200)
		liveDeviceFaultWrite(t, ctx, client, 2, legacySend, "0109023010014D", gu.StatusOK)
		liveDeviceFaultAwaitSend(t, ctx, client, 2, legacySend, "1", true)
		recovered := liveDeviceFaultAudit(t, ctx, apiURL, sessionID, physicalPort, knownCommands, domain.CommandReceived)
		liveDeviceFaultNoPending(t, ctx, apiURL)
		t.Logf("ACK mode=%s: legacy _Send=3/bad, failed audit=%s pinned Session=%s group=device-a source=lower-b port=%s; selection unlocked and next command=%s succeeded (%s)", mode, failed.ID, sessionID, physicalPort, recovered.ID, time.Since(started))
	}

	observer, _ := connectLiveManagerAnonymous(t, endpoint, "device-fault-observer-"+nonce)
	initiatorName := "device-fault-close-" + nonce
	initiator, _ := connectLiveManagerAnonymous(t, endpoint, initiatorName)
	initiatorID := liveDeviceFaultSession(t, ctx, apiURL, initiatorName).ID
	liveDeviceFaultWrite(t, ctx, observer, 3, "Groups/device-a/SelectedSourceId", "lower-b", gu.StatusOK)
	liveDeviceFaultWrite(t, ctx, initiator, 3, "Groups/device-a/SelectedSourceId", "lower-b", gu.StatusOK)
	setACK("success", 1000)
	started := time.Now()
	liveDeviceFaultWrite(t, ctx, initiator, 3, "Groups/device-a/Send", "0109023010014D", gu.StatusOK)
	// Do not put REST polling between the send and CloseSession: the ACK is
	// intentionally delayed only 1000ms, below Rust's 1500ms serial timeout.
	closeCtx, closeCancel := context.WithTimeout(ctx, 3*time.Second)
	err = initiator.Close(closeCtx)
	closeCancel()
	if err != nil {
		t.Fatalf("CloseSession during command: %v", err)
	}
	liveDeviceFaultWrite(t, ctx, observer, 3, "Groups/device-a/SelectedSourceId", "lower-a", gu.StatusBadWaitingForResponse)
	t.Logf("initiator Session=%s closed; another Session still receives BadWaitingForResponse after %s", initiatorID, time.Since(started))
	liveDeviceFaultEventually(t, ctx, 5*time.Second, "initiator disappears from online Sessions", func() bool {
		var sessions struct {
			Items []api.SessionView `json:"items"`
		}
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sessions", 0, nil, &sessions, http.StatusOK)
		for _, session := range sessions.Items {
			if session.ID == initiatorID {
				return false
			}
		}
		return true
	})
	completed := liveDeviceFaultAudit(t, ctx, apiURL, initiatorID, physicalPort, knownCommands, domain.CommandReceived)
	liveDeviceFaultNoPending(t, ctx, apiURL)
	liveDeviceFaultWrite(t, ctx, observer, 3, "Groups/device-a/SelectedSourceId", "lower-a", gu.StatusOK)
	// A later selection on the surviving Session must not rewrite the closed
	// initiator's durable route/audit identity.
	var final api.CommandPage
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/commands?q="+url.QueryEscape(completed.ID)+"&limit=10", 0, nil, &final, http.StatusOK)
	if len(final.Items) != 1 || !reflect.DeepEqual(final.Items[0], completed) {
		t.Fatalf("closed Session command audit changed after another Session switched: before=%#v after=%#v", completed, final.Items)
	}
	t.Logf("delayed successful ACK released group lock after CloseSession; durable command=%s retains Session=%s group=%s source=%s port=%s", completed.ID, completed.SessionID, completed.GroupID, completed.SourceID, completed.Port)
}

func liveDeviceFaultWrite(t *testing.T, ctx context.Context, client *gopcua.Client, ns uint16, identifier, value string, want gu.StatusCode) {
	t.Helper()
	response, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{NodeID: gu.NewStringNodeID(ns, identifier), AttributeID: gu.AttributeIDValue, Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(value)}}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] != want {
		t.Fatalf("write %s=%q want=%s response=%#v err=%v", identifier, value, want, response, err)
	}
}

func liveDeviceFaultAwaitSend(t *testing.T, ctx context.Context, client *gopcua.Client, ns uint16, identifier, want string, good bool) {
	t.Helper()
	var last *gu.DataValue
	defer func() {
		if !t.Failed() {
			return
		}
		if last == nil {
			t.Logf("command %s never returned a DataValue", identifier)
			return
		}
		var actual any
		if last.Value != nil {
			actual = last.Value.Value()
		}
		t.Logf("last command read %s=%v status=%s (0x%08X); want=%s good=%v", identifier, actual, last.Status, uint32(last.Status), want, good)
	}()
	liveDeviceFaultEventually(t, ctx, 12*time.Second, "command "+identifier+" reaches "+want, func() bool {
		response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, identifier), AttributeID: gu.AttributeIDValue}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil {
			t.Fatalf("read command %s response=%#v err=%v", identifier, response, err)
		}
		last = response.Results[0]
		if !liveDeviceFaultResultMatches(last, want, good) {
			return false
		}
		t.Logf("command read %s=%s status=%s (0x%08X)", identifier, want, last.Status, uint32(last.Status))
		return true
	})
}

func liveDeviceFaultResultMatches(value *gu.DataValue, want string, good bool) bool {
	if value == nil || value.Value == nil || fmt.Sprint(value.Value.Value()) != want {
		return false
	}
	// Rust may attach the Overflow information bit when Waiting and the ACK
	// share a publish interval. Keep that flag intact: quality is determined
	// by the severity bits, not by equality with the all-zero Good code.
	status := domain.StatusCode(value.Status)
	if good {
		return status.IsGood()
	}
	return status.IsBad()
}

func TestLiveDeviceFaultResultMatchesStatusSeverity(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  gu.StatusCode
		value   string
		want    string
		good    bool
		matches bool
	}{
		{name: "plain Good", status: gu.StatusOK, value: "1", want: "1", good: true, matches: true},
		{name: "Rust Good with Overflow", status: 0x00000080, value: "1", want: "1", good: true, matches: true},
		{name: "Good with DataValue Overflow", status: 0x00000480, value: "1", want: "1", good: true, matches: true},
		{name: "Good with semantic change", status: 0x00004000, value: "1", want: "1", good: true, matches: true},
		{name: "Good subcode", status: gu.StatusGoodClamped, value: "1", want: "1", good: true, matches: true},
		{name: "wrong ACK with Good Overflow", status: 0x00000080, value: "3", want: "1", good: true},
		{name: "Uncertain is not success", status: 0x40000480, value: "1", want: "1", good: true},
		{name: "Bad is not success", status: gu.StatusBadNoCommunication, value: "1", want: "1", good: true},
		{name: "Bad terminal", status: gu.StatusBadUnexpectedError, value: "3", want: "3", matches: true},
		{name: "Bad terminal with Overflow", status: 0x80010080, value: "3", want: "3", matches: true},
		{name: "Good is not failure", status: 0x00000080, value: "3", want: "3"},
		{name: "Uncertain is not failure", status: 0x40000480, value: "3", want: "3"},
		{name: "reserved severity is not success", status: 0xC0000080, value: "1", want: "1", good: true},
		{name: "reserved severity is not failure", status: 0xC0000080, value: "3", want: "3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := &gu.DataValue{Value: gu.MustVariant(test.value), Status: test.status}
			if got := liveDeviceFaultResultMatches(value, test.want, test.good); got != test.matches {
				t.Fatalf("value=%s status=0x%08X want=%s good=%v matched=%v want=%v", test.value, uint32(test.status), test.want, test.good, got, test.matches)
			}
			if value.Status != test.status {
				t.Fatal("checking quality must preserve every status information bit")
			}
		})
	}
	for _, value := range []*gu.DataValue{nil, {Status: gu.StatusOK}} {
		if liveDeviceFaultResultMatches(value, "1", true) || liveDeviceFaultResultMatches(value, "3", false) {
			t.Fatal("missing DataValue/Variant must not count as a terminal ACK")
		}
	}
}

func liveDeviceFaultSession(t *testing.T, ctx context.Context, apiURL, name string) api.SessionView {
	t.Helper()
	var found api.SessionView
	liveDeviceFaultEventually(t, ctx, 5*time.Second, "online Session "+name, func() bool {
		var sessions struct {
			Items []api.SessionView `json:"items"`
		}
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sessions", 0, nil, &sessions, http.StatusOK)
		for _, session := range sessions.Items {
			if session.ClientName == name {
				found = session
				return true
			}
		}
		return false
	})
	return found
}

func liveDeviceFaultAudit(t *testing.T, ctx context.Context, apiURL, sessionID, port string, known map[string]bool, want domain.CommandState) domain.PendingCommand {
	t.Helper()
	var found domain.PendingCommand
	liveDeviceFaultEventually(t, ctx, 12*time.Second, "command audit "+sessionID+" becomes "+string(want), func() bool {
		var page api.CommandPage
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/commands?q="+url.QueryEscape(sessionID)+"&limit=50", 0, nil, &page, http.StatusOK)
		for _, command := range page.Items {
			if command.SessionID != sessionID || known[command.ID] {
				continue
			}
			if command.GroupID != "device-a" || command.SourceID != "lower-b" || command.Port != port || command.Kind != "Write" {
				t.Fatalf("command route/audit was redirected: %#v", command)
			}
			if command.State == domain.CommandWaiting {
				return false
			}
			if command.State != want || command.FinishedAt.IsZero() || command.FinishedAt.Before(command.StartedAt) || (want == domain.CommandReceived && !command.StatusCode.IsGood()) {
				t.Fatalf("unexpected command terminal audit want=%s: %#v", want, command)
			}
			found = command
			return true
		}
		return false
	})
	known[found.ID] = true
	return found
}

func liveDeviceFaultNoPending(t *testing.T, ctx context.Context, apiURL string) {
	t.Helper()
	liveDeviceFaultEventually(t, ctx, 5*time.Second, "device-a pending count becomes zero", func() bool {
		var group api.GroupView
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/groups/device-a", 0, nil, &group, http.StatusOK)
		return group.PendingCommands == 0
	})
}

func liveDeviceFaultEventually(t *testing.T, ctx context.Context, timeout time.Duration, description string, predicate func() bool) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := waitCtx.Err(); err != nil {
			t.Fatalf("waiting for %s: %v", description, err)
		}
		if predicate() {
			return
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("waiting for %s: %v", description, waitCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Only ack_mode/delay_ms on b are changed. Cleanup restores the original bytes,
// not a reconstructed configuration. Sibling event/manifest files are read only.
func liveDeviceFaultControl(t *testing.T, ctx context.Context, path string) func(string, int) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		t.Fatal("fixture control must be a regular JSON file of at most 64 KiB")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var originalValue any
	if err := json.Unmarshal(original, &originalValue); err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(original, &document); err != nil || document == nil {
		t.Fatalf("invalid fixture control: %v", err)
	}
	settings := make(map[string]json.RawMessage)
	if raw := document["b"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
			t.Fatalf("invalid b control: %v", err)
		}
	}
	eventPath := filepath.Join(filepath.Dir(path), "events.jsonl")
	publish := func(raw []byte, want any, waitCtx context.Context) error {
		stat, err := os.Stat(eventPath)
		if err != nil {
			return err
		}
		if err := liveDeviceFaultReplace(path, raw, info.Mode().Perm()); err != nil {
			return err
		}
		return liveDeviceFaultControlApplied(waitCtx, eventPath, stat.Size(), want)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		// Restoration must not depend on the fixture still being alive or its
		// event log remaining available after a failing test.
		stat, statErr := os.Stat(eventPath)
		if err := liveDeviceFaultReplace(path, original, info.Mode().Perm()); err != nil {
			t.Errorf("restore original fixture control %s: %v", path, err)
			return
		}
		if statErr != nil {
			t.Errorf("original control restored, but cannot check fixture acknowledgement: %v", statErr)
			return
		}
		if err := liveDeviceFaultControlApplied(cleanupCtx, eventPath, stat.Size(), originalValue); err != nil {
			t.Errorf("original control restored, but fixture acknowledgement failed: %v", err)
			return
		}
		t.Log("original fixture control restored atomically and acknowledged")
	})
	return func(mode string, delay int) {
		t.Helper()
		settings["ack_mode"], _ = json.Marshal(mode)
		settings["delay_ms"], _ = json.Marshal(delay)
		document["b"], err = json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		var want any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		if err := publish(raw, want, waitCtx); err != nil {
			t.Fatalf("set fixture ACK mode=%s delay=%d: %v", mode, delay, err)
		}
	}
}

func liveDeviceFaultReplace(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".device-fault-control-*.json")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func liveDeviceFaultControlApplied(ctx context.Context, path string, offset int64, want any) error {
	for {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return err
		}
		scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
		scanner.Buffer(make([]byte, 4096), 256<<10)
		matched := false
		for scanner.Scan() {
			var event struct {
				Event   string `json:"event"`
				Control any    `json:"control"`
				Error   string `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if event.Event == "control_rejected" {
				file.Close()
				return fmt.Errorf("fixture rejected control: %s", event.Error)
			}
			if event.Event == "control_applied" && reflect.DeepEqual(event.Control, want) {
				matched = true
				break
			}
		}
		err = scanner.Err()
		file.Close()
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fixture did not acknowledge atomic control update: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
