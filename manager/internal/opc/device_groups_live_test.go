package opc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

func TestLiveDeviceGroupsLegacyShapeAndSecureRead(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	apiURL := strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	if endpoint == "" || apiURL == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT and RCD_DEVICE_GROUPS_API for the isolated two-Rust PTY deployment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	upper, _ := connectLiveManagerAnonymous(t, endpoint, "device-groups-golden")
	wantNamespaces := []string{"http://opcfoundation.org/UA/", ApplicationNamespaceURI, BusinessNamespaceURI, ManagementNamespaceURI}
	if !reflect.DeepEqual(upper.Namespaces(), wantNamespaces) {
		t.Fatalf("manager NamespaceArray=%v want=%v", upper.Namespaces(), wantNamespaces)
	}
	upperCatalog, _, err := discoverCatalog(ctx, upper, domain.Source{ID: "manager", GroupID: "shared"}, namespaceIndex(upper.Namespaces()))
	if err != nil {
		t.Fatal(err)
	}
	public := make(map[string]domain.CatalogNode, len(upperCatalog.Nodes))
	for _, node := range upperCatalog.Nodes {
		public[node.Key.Identifier] = node
	}
	endpoints := map[string]string{"manager": endpoint}
	for _, sourceID := range []string{"lower-a", "lower-b"} {
		var source api.SourceView
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/"+sourceID, 0, nil, &source, http.StatusOK)
		endpoints[sourceID] = source.Endpoint
		lower, _ := connectLiveManagerAnonymous(t, source.Endpoint, "device-groups-golden-"+sourceID)
		catalog, _, err := discoverCatalog(ctx, lower, source.Source, namespaceIndex(lower.Namespaces()))
		if err != nil || catalog.Topology == nil || len(catalog.Topology.Ports) != 1 || len(catalog.Topology.Ports[0].Devices) != 2 {
			t.Fatalf("%s topology=%#v err=%v", sourceID, catalog.Topology, err)
		}
		for _, expected := range catalog.Nodes {
			actual, exists := public[expected.Key.Identifier]
			expected.GroupID, expected.PortID, expected.DeviceID, expected.Kind = "", "", "", ""
			if !exists || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("legacy node shape differs for %s/%s: lower=%#v upper=%#v", sourceID, expected.Key.Identifier, expected, actual)
			}
		}
		_ = lower.Close(ctx)
		t.Logf("%s: all %d legacy nodes retain URI/NodeId, BrowseName, type/rank/access, HasComponent parent and Method arguments", sourceID, len(catalog.Nodes))
	}
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lower-a", "lower-b", "manager"} {
		for _, username := range []string{"", "user1"} {
			auth := "anonymous"
			if username != "" {
				auth = "username"
			}
			t.Run(name+"/Basic256Sha256/"+auth, func(t *testing.T) {
				client := connectSecureClient(t, endpoints[name], pair, username, "pwd")
				ns, exists := namespaceIndex(client.Namespaces())[BusinessNamespaceURI]
				if !exists {
					t.Fatal("business namespace missing")
				}
				response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, "A_VALUE"), AttributeID: gu.AttributeIDValue}}})
				if err != nil || response == nil || len(response.Results) != 1 || !liveGroupsDataValueHasStatus(response.Results[0], gu.StatusOK) {
					t.Fatalf("secure read response=%#v err=%v", response, err)
				}
			})
		}
	}
}

// Opt-in only: this test sends commands to the disposable two-Rust/PTY
// fixture. Never point these environment variables at a production manager.
func TestLiveDeviceGroupsReadSubscribeWebAndCommands(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	apiURL := strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	if endpoint == "" || apiURL == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT and RCD_DEVICE_GROUPS_API for the isolated two-Rust PTY deployment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	wireLog := ""
	wireOffset := 0
	if control := os.Getenv("RCD_DEVICE_GROUPS_CONTROL"); control != "" {
		wireLog = filepath.Join(filepath.Dir(control), "commands.jsonl")
		data, err := os.ReadFile(wireLog)
		if err != nil {
			t.Fatal(err)
		}
		wireOffset = len(data)
	}
	var sourceTopology api.SourceTopologyView
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/lower-a/topology", 0, nil, &sourceTopology, http.StatusOK)
	if !sourceTopology.Supported || len(sourceTopology.Ports) != 1 || len(sourceTopology.Ports[0].Devices) != 2 {
		t.Fatalf("need real two-device Rust topology: %#v", sourceTopology)
	}
	var groupB api.GroupView
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/groups/device-b", 0, nil, &groupB, http.StatusOK)
	originalDefaultB := groupB.DefaultSourceID
	t.Cleanup(func() {
		var current api.GroupView
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/groups/device-b", 0, nil, &current, http.StatusOK)
		if current.DefaultSourceID != originalDefaultB {
			liveGroupsJSON(t, apiURL, http.MethodPut, "/api/v1/groups/device-b/default-source", current.Revision, map[string]string{"source_id": originalDefaultB}, nil, http.StatusOK)
		}
	})
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	firstName, secondName := "device-groups-first-"+nonce, "device-groups-second-"+nonce
	first, _ := connectLiveManagerAnonymous(t, endpoint, firstName)
	second, _ := connectLiveManagerAnonymous(t, endpoint, secondName)
	// Do not leak an in-flight fixture command into the next opt-in test if an
	// assertion aborts between Write and its terminal ACK.
	t.Cleanup(func() {
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			var overview api.Overview
			liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/overview", 0, nil, &overview, http.StatusOK)
			if overview.PendingCommands == 0 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("fixture command remains pending after cleanup timeout")
	})
	read := func(client *gopcua.Client, ns uint16, identifier string) *gu.DataValue {
		t.Helper()
		response, err := client.Read(ctx, &gu.ReadRequest{TimestampsToReturn: gu.TimestampsToReturnBoth, NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(ns, identifier), AttributeID: gu.AttributeIDValue}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil {
			t.Fatalf("read %q: response=%#v err=%v", identifier, response, err)
		}
		return response.Results[0]
	}
	write := func(client *gopcua.Client, ns uint16, identifier, value string, want gu.StatusCode) {
		t.Helper()
		response, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{NodeID: gu.NewStringNodeID(ns, identifier), AttributeID: gu.AttributeIDValue, Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(value)}}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] != want {
			t.Fatalf("write %s=%s want=%s response=%#v err=%v", identifier, value, want, response, err)
		}
	}
	assertPoint := func(client *gopcua.Client, identifier string, want float64) {
		t.Helper()
		var last *gu.DataValue
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			last = read(client, 2, identifier)
			if liveGroupsNumberMatches(last, want) {
				if last.Status != gu.StatusOK {
					t.Logf("point %s accepted Good with information bits: %s", identifier, liveGroupsDescribeValue(last))
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("point %s want=%v last=%s", identifier, want, liveGroupsDescribeValue(last))
	}
	selectSource := func(client *gopcua.Client, group, source string) {
		write(client, 3, "Groups/"+group+"/SelectedSourceId", source, gu.StatusOK)
	}
	selectSource(first, "device-a", "lower-a")
	selectSource(first, "device-b", "lower-a")
	selectSource(second, "device-a", "lower-a")
	selectSource(second, "device-b", "lower-a")
	assertPoint(first, "A_VALUE", 11)
	assertPoint(first, "B_VALUE", 22)
	assertPoint(second, "A_VALUE", 11)
	notifications := liveGroupsSubscribe(t, ctx, first, "A_VALUE", "B_VALUE")
	liveGroupsAwaitValue(t, notifications, 1, 11)
	selectSource(first, "device-a", "lower-b")
	liveGroupsAwaitValue(t, notifications, 1, 101)
	assertPoint(first, "A_VALUE", 101)
	assertPoint(first, "B_VALUE", 22)
	assertPoint(second, "A_VALUE", 11)
	assertPoint(second, "B_VALUE", 22)
	t.Log("Read/Subscribe: one Session switches device-a; device-b and the other Session retain lower-a")

	// A shared physical-port ACK is not a single selected source once its two
	// devices have different routes and no command history exists.
	if value := read(first, 2, sourceTopology.Ports[0].SendNodeID); value.Status != gu.StatusBadConfigurationError {
		t.Fatalf("split physical-port ACK must be ambiguous before a command, got %#v", value)
	}
	selectSource(second, "device-a", "lower-b")
	var sessions struct {
		Items []api.SessionView `json:"items"`
	}
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sessions", 0, nil, &sessions, http.StatusOK)
	var firstSession api.SessionView
	for _, session := range sessions.Items {
		if session.ClientName == firstName {
			firstSession = session
		}
	}
	if firstSession.ID == "" {
		t.Fatalf("Session %s missing from REST listing", firstName)
	}
	selectionPath := "/api/v1/sessions/" + url.PathEscape(firstSession.ID) + "/groups/device-a/selection"
	revision := firstSession.Routes["device-a"].Revision
	liveGroupsJSON(t, apiURL, http.MethodPut, selectionPath, revision, map[string]string{"source_id": "lower-a"}, nil, http.StatusOK)
	liveGroupsJSON(t, apiURL, http.MethodPut, selectionPath, revision, map[string]string{"source_id": "lower-b"}, nil, http.StatusConflict)
	assertPoint(first, "A_VALUE", 11)
	assertPoint(second, "A_VALUE", 101)
	t.Log("REST selection affects only its target Session; stale revision returns HTTP 409")

	write(first, 3, "Groups/device-b/DefaultSourceId", "lower-b", gu.StatusOK)
	assertPoint(first, "B_VALUE", 22)
	assertPoint(second, "B_VALUE", 22)
	third, _ := connectLiveManagerAnonymous(t, endpoint, "device-groups-new-"+nonce)
	assertPoint(third, "B_VALUE", 202)
	_ = third.Close(ctx)
	t.Log("default change affects only a newly activated Session")

	selectSource(first, "device-a", "lower-b")
	selectSource(first, "device-b", "lower-b")
	write(first, 3, "Groups/device-a/Send", "0109023010014D", gu.StatusOK)
	write(first, 3, "Groups/device-b/Send", "0209023010014E", gu.StatusBadWaitingForResponse)
	write(first, 3, "Groups/device-a/SelectedSourceId", "lower-a", gu.StatusBadWaitingForResponse)
	write(first, 3, "Groups/device-a/DefaultSourceId", "lower-a", gu.StatusBadWaitingForResponse)
	selectSource(first, "device-b", "lower-a")
	awaitAck := func(ns uint16, identifier string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		var last *gu.DataValue
		for time.Now().Before(deadline) {
			last = read(first, ns, identifier)
			if liveDeviceFaultResultMatches(last, "1", true) {
				t.Logf("ACK %s: %s", identifier, liveGroupsDescribeValue(last))
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("ACK not successful for %s: last=%s", identifier, liveGroupsDescribeValue(last))
	}
	awaitAck(3, "Groups/device-a/Send")
	t.Log("same source+physical port rejects concurrent device commands; only the pending logical group is selection-locked")

	write(first, 2, sourceTopology.Ports[0].SendNodeID, "0109023010014D", gu.StatusOK)
	awaitAck(2, sourceTopology.Ports[0].SendNodeID)
	var commands api.CommandPage
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/commands?limit=20", 0, nil, &commands, http.StatusOK)
	matched := false
	for _, command := range commands.Items {
		if command.SessionID == firstSession.ID && command.GroupID == "device-a" && command.SourceID == "lower-b" && command.State == "Received" {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("legacy public port command did not retain device-a/lower-b routing: %#v", commands.Items)
	}
	write(first, 3, "Groups/device-a/Send", "7F0902301001CB", gu.StatusBadConfigurationError)
	call, err := first.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(3, "Groups/device-b"), MethodID: gu.NewStringNodeID(3, "Groups/device-b/SendCommand"), InputArguments: []*gu.Variant{gu.MustVariant("0209023010014E")}})
	if err != nil || call == nil || call.StatusCode != gu.StatusOK || len(call.OutputArguments) != 1 {
		t.Fatalf("group Method proxy response=%#v err=%v", call, err)
	}
	if wireLog != "" {
		liveGroupsCheckWireCommands(t, wireLog, wireOffset)
	}
	t.Log("legacy port alias resolves the selected source's actual port; unknown device rejected; group Method accepted by real Rust")
	_ = first.Close(ctx)
	liveGroupsJSON(t, apiURL, http.MethodPut, selectionPath, revision+1, map[string]string{"source_id": "lower-a"}, nil, http.StatusNotFound)
	newSession, _ := connectLiveManagerAnonymous(t, endpoint, "device-groups-after-close-"+nonce)
	assertPoint(newSession, "A_VALUE", 11)
	t.Log("CloseSession removes the REST selection target and a replacement Session starts with default device-a")
}

// Opt-in only: changes one value in the disposable PTY fixture and measures the
// complete fixture-control -> Rust serial poll -> lower OPC -> manager -> upper
// OPC notification path. The fixture's 100 ms control-file scan is deliberately
// included, so this is a conservative end-to-end measurement.
func TestLiveDeviceGroupsFastUpdateLatency(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	controlPath := os.Getenv("RCD_DEVICE_GROUPS_CONTROL")
	if endpoint == "" || controlPath == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT and RCD_DEVICE_GROUPS_CONTROL for the isolated PTY deployment")
	}
	original, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveGroupsWriteAtomic(t, controlPath, original) })
	var control map[string]any
	if err := json.Unmarshal(original, &control); err != nil {
		t.Fatal(err)
	}
	source, ok := control["a"].(map[string]any)
	if !ok {
		t.Fatalf("fixture control has no source a: %#v", control)
	}
	values, ok := source["values"].(map[string]any)
	if !ok {
		t.Fatalf("fixture source a has no values: %#v", source)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, _ := connectLiveManagerAnonymous(t, endpoint, "device-groups-fast-latency")
	writeResponse, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
		NodeID: gu.NewStringNodeID(3, "Groups/device-a/SelectedSourceId"), AttributeID: gu.AttributeIDValue,
		Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant("lower-a")},
	}}})
	if err != nil || writeResponse == nil || len(writeResponse.Results) != 1 || writeResponse.Results[0] != gu.StatusOK {
		t.Fatalf("select lower-a: response=%#v err=%v", writeResponse, err)
	}
	notifications := liveGroupsSubscribe(t, ctx, client, "A_VALUE")
	initial, err := strconv.ParseFloat(fmt.Sprint(values["1"]), 64)
	if err != nil {
		t.Fatalf("parse initial fixture value: %v", err)
	}
	liveGroupsAwaitValue(t, notifications, 1, initial)

	const sampleCount = 20
	durations := make([]time.Duration, 0, sampleCount)
	for index := 0; index < sampleCount; index++ {
		target := float64(31 + index%2)
		values["1"] = target
		encoded, err := json.MarshalIndent(control, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, '\n')
		started := time.Now()
		liveGroupsWriteAtomic(t, controlPath, encoded)
		liveGroupsAwaitValue(t, notifications, 1, target)
		durations = append(durations, time.Since(started))
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[len(sorted)/2]
	p95 := sorted[(len(sorted)*95+99)/100-1]
	max := sorted[len(sorted)-1]
	t.Logf("50ms end-to-end samples=%d p50=%s p95=%s max=%s all=%v", len(sorted), p50, p95, max, durations)
	if p95 > 500*time.Millisecond {
		t.Fatalf("end-to-end notification P95=%s exceeds 500ms", p95)
	}
}

// Opt-in inventory/quality check for the real 28800-baud lower gateway. It is
// read-only: all business Variables are read once and one is subscribed at the
// requested 50 ms intervals to verify the server's revised values.
func TestLiveFastLowerQualityAndIntervals(t *testing.T) {
	endpoint := os.Getenv("RCD_FAST_LOWER_ENDPOINT")
	if endpoint == "" {
		t.Skip("set RCD_FAST_LOWER_ENDPOINT for the real lower OPC gateway")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := connectLiveConfiguredEndpointAnonymous(t, endpoint, "fast-lower-quality")
	catalog, _, err := discoverCatalog(ctx, client, domain.Source{ID: "fast-lower", GroupID: "quality"}, namespaceIndex(client.Namespaces()))
	if err != nil {
		t.Fatal(err)
	}
	variables := make([]*gu.NodeID, 0, len(catalog.Nodes))
	businessNS, exists := namespaceIndex(client.Namespaces())[BusinessNamespaceURI]
	if !exists {
		t.Fatal("lower business namespace is missing")
	}
	for _, node := range catalog.Nodes {
		if node.Class == domain.NodeClassVariable {
			variables = append(variables, gu.NewStringNodeID(businessNS, node.Key.Identifier))
		}
	}
	if len(variables) == 0 {
		t.Fatal("lower catalog has no Variables")
	}

	statusCounts := make(map[gu.StatusCode]int)
	good, bad := 0, 0
	for start := 0; start < len(variables); start += 500 {
		end := start + 500
		if end > len(variables) {
			end = len(variables)
		}
		request := &gu.ReadRequest{TimestampsToReturn: gu.TimestampsToReturnBoth}
		for _, nodeID := range variables[start:end] {
			request.NodesToRead = append(request.NodesToRead, &gu.ReadValueID{NodeID: nodeID, AttributeID: gu.AttributeIDValue})
		}
		response, err := client.Read(ctx, request)
		if err != nil || response == nil || len(response.Results) != end-start {
			t.Fatalf("read batch %d:%d response=%#v err=%v", start, end, response, err)
		}
		for _, value := range response.Results {
			statusCounts[value.Status]++
			if domain.StatusCode(value.Status).IsGood() {
				good++
			} else {
				bad++
			}
		}
	}

	notifications := make(chan *gopcua.PublishNotificationData, 16)
	subscription, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 50 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = subscription.Cancel(closeCtx)
	})
	if subscription.RevisedPublishingInterval != 50*time.Millisecond {
		t.Fatalf("lower revised publishing interval=%s want=50ms", subscription.RevisedPublishingInterval)
	}
	item := gopcua.NewMonitoredItemCreateRequestWithDefaults(variables[0], gu.AttributeIDValue, 1)
	item.RequestedParameters.SamplingInterval = 50
	response, err := subscription.Monitor(ctx, gu.TimestampsToReturnBoth, item)
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0].StatusCode != gu.StatusOK {
		t.Fatalf("lower monitor response=%#v err=%v", response, err)
	}
	if response.Results[0].RevisedSamplingInterval != 50 {
		t.Fatalf("lower revised sampling interval=%vms want=50ms", response.Results[0].RevisedSamplingInterval)
	}
	awaitDataChange(t, notifications)
	t.Logf("real lower variables=%d good=%d bad=%d statuses=%v publishing=%s sampling=%vms", len(variables), good, bad, statusCounts, subscription.RevisedPublishingInterval, response.Results[0].RevisedSamplingInterval)
	if good == 0 {
		t.Fatal("real lower returned no Good business Variables")
	}
}

func connectLiveConfiguredEndpointAnonymous(t *testing.T, endpoint, sessionName string) *gopcua.Client {
	t.Helper()
	discoveryCtx, discoveryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	endpoints, err := gopcua.GetEndpoints(discoveryCtx, endpoint)
	discoveryCancel()
	if err != nil {
		t.Fatalf("GetEndpoints(%s): %v", endpoint, err)
	}
	selected, err := gopcua.SelectEndpoint(endpoints, "None", gu.MessageSecurityModeNone)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gopcua.NewClient(
		lowerDialEndpoint(endpoint, selected.EndpointURL),
		gopcua.SecurityFromEndpoint(selected, gu.UserTokenTypeAnonymous),
		gopcua.AuthAnonymous(), gopcua.AutoReconnect(false), gopcua.SessionName(sessionName),
	)
	if err != nil {
		t.Fatal(err)
	}
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := client.Connect(connectCtx); err != nil {
		connectCancel()
		t.Fatal(err)
	}
	connectCancel()
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = client.Close(closeCtx)
	})
	return client
}

func liveGroupsWriteAtomic(t *testing.T, path string, data []byte) {
	t.Helper()
	temporary, err := os.CreateTemp(filepath.Dir(path), ".fast-latency-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		t.Fatal(err)
	}
}

func liveGroupsCheckWireCommands(t *testing.T, path string, offset int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err != nil || len(data) < offset {
			t.Fatalf("fixture command log read: bytes=%d offset=%d err=%v", len(data), offset, err)
		}
		counts := make(map[string]int)
		for _, line := range bytes.Split(data[offset:], []byte{'\n'}) {
			var event struct {
				Event    string `json:"event"`
				Source   string `json:"source"`
				Body     string `json:"body_hex"`
				Accepted bool   `json:"accepted_by_fixture"`
			}
			if json.Unmarshal(line, &event) != nil || event.Event != "command_received" {
				continue
			}
			body := strings.ToUpper(strings.ReplaceAll(event.Body, " ", ""))
			if !event.Accepted || strings.HasPrefix(body, "7F") || (event.Source == "b" && strings.HasPrefix(body, "02")) {
				t.Fatalf("unexpected wire dispatch despite command rejection: %s", line)
			}
			counts[event.Source+":"+body]++
		}
		if counts["b:0109023010014D"] >= 2 && counts["a:0209023010014E"] >= 1 {
			t.Log("PTY wire log confirms both device-a writes reached lower-b and device-b Method reached lower-a with original payload bytes intact")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("fixture did not observe all expected physical source/port command payloads")
}

func liveGroupsJSON(t *testing.T, base, method, path string, revision uint64, body, output any, want int) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", base)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if revision > 0 {
		request.Header.Set("If-Match", fmt.Sprintf("\"%d\"", revision))
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, response.StatusCode, want, data)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, data)
		}
	}
}

func liveGroupsSubscribe(t *testing.T, ctx context.Context, client *gopcua.Client, identifiers ...string) <-chan *gopcua.PublishNotificationData {
	t.Helper()
	notifications := make(chan *gopcua.PublishNotificationData, 64)
	subscription, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 50 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	if subscription.RevisedPublishingInterval != 50*time.Millisecond {
		t.Fatalf("revised publishing interval=%s want=50ms", subscription.RevisedPublishingInterval)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = subscription.Cancel(closeCtx)
	})
	requests := make([]*gu.MonitoredItemCreateRequest, 0, len(identifiers))
	for i, identifier := range identifiers {
		request := gopcua.NewMonitoredItemCreateRequestWithDefaults(gu.NewStringNodeID(2, identifier), gu.AttributeIDValue, uint32(i+1))
		request.RequestedParameters.SamplingInterval = 50
		requests = append(requests, request)
	}
	response, err := subscription.Monitor(ctx, gu.TimestampsToReturnBoth, requests...)
	if err != nil || response == nil || len(response.Results) != len(identifiers) {
		t.Fatalf("monitor=%#v err=%v", response, err)
	}
	for _, result := range response.Results {
		if result.StatusCode != gu.StatusOK {
			t.Fatal(result.StatusCode)
		}
		if result.RevisedSamplingInterval != 50 {
			t.Fatalf("revised sampling interval=%vms want=50ms", result.RevisedSamplingInterval)
		}
	}
	return notifications
}

func liveGroupsAwaitValue(t *testing.T, notifications <-chan *gopcua.PublishNotificationData, handle uint32, want float64) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	var last *gu.DataValue
	var received, matchingHandle int
	for {
		select {
		case notification, ok := <-notifications:
			if !ok {
				t.Fatal("subscription closed")
			}
			if notification == nil {
				continue
			}
			if notification.Error != nil {
				t.Fatal(notification.Error)
			}
			change, ok := notification.Value.(*gu.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range change.MonitoredItems {
				received++
				if item != nil && item.ClientHandle == handle {
					matchingHandle++
					last = item.Value
				}
				if liveGroupsMonitoredValueMatches(item, handle, want) {
					t.Logf("subscription handle=%d matched %s", handle, liveGroupsDescribeValue(item.Value))
					return
				}
			}
		case <-timer.C:
			t.Fatalf("no subscription notification for handle=%d value=%v: received=%d matching_handle=%d last=%s", handle, want, received, matchingHandle, liveGroupsDescribeValue(last))
		}
	}
}

func liveGroupsDataValueHasStatus(value *gu.DataValue, want gu.StatusCode) bool {
	if value == nil || value.Value == nil {
		return false
	}
	// Like liveDeviceFaultResultMatches for ACKs, a request for Good quality
	// accepts its information bits (including Rust's Overflow flag). Keep the
	// complete status untouched, and still require exact explicitly named Bad
	// or Uncertain codes in tests of quality propagation.
	if want == gu.StatusOK {
		return domain.StatusCode(value.Status).IsGood()
	}
	return value.Status == want
}

func liveGroupsNumberMatches(value *gu.DataValue, want float64) bool {
	if !liveGroupsDataValueHasStatus(value, gu.StatusOK) {
		return false
	}
	actual, err := strconv.ParseFloat(fmt.Sprint(value.Value.Value()), 64)
	return err == nil && actual == want
}

func liveGroupsMonitoredValueMatches(item *gu.MonitoredItemNotification, handle uint32, want float64) bool {
	return item != nil && item.ClientHandle == handle && liveGroupsNumberMatches(item.Value, want)
}

func liveGroupsDescribeValue(value *gu.DataValue) string {
	if value == nil {
		return "<no DataValue>"
	}
	var actual any
	if value.Value != nil {
		actual = value.Value.Value()
	}
	return fmt.Sprintf("value=%v status=0x%08X source_timestamp=%s server_timestamp=%s", actual, uint32(value.Status), value.SourceTimestamp.Format(time.RFC3339Nano), value.ServerTimestamp.Format(time.RFC3339Nano))
}

func TestLiveDeviceGroupsDisconnectAndRecoverWithoutFailover(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	controlPath := os.Getenv("RCD_DEVICE_GROUPS_CONTROL")
	if endpoint == "" || controlPath == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT and RCD_DEVICE_GROUPS_CONTROL for the isolated Rust fixture")
	}
	original, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	var control map[string]map[string]any
	if err := json.Unmarshal(original, &control); err != nil {
		t.Fatal(err)
	}
	if control["b"] == nil {
		control["b"] = make(map[string]any)
	}
	publishControl := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(controlPath+".next", raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(controlPath+".next", controlPath); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { publishControl(original) })
	updateControl := func() {
		t.Helper()
		data, err := json.Marshal(control)
		if err != nil {
			t.Fatal(err)
		}
		publishControl(data)
	}
	client, _ := connectLiveManagerAnonymous(t, endpoint, "device-groups-disconnect")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	writeSelection := func(source string) {
		t.Helper()
		response, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{NodeID: gu.NewStringNodeID(3, "Groups/device-a/SelectedSourceId"), AttributeID: gu.AttributeIDValue, Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(source)}}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] != gu.StatusOK {
			t.Fatalf("select offline/online %s: %#v %v", source, response, err)
		}
	}
	readValue := func(namespace uint16, identifier string) *gu.DataValue {
		t.Helper()
		response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(namespace, identifier), AttributeID: gu.AttributeIDValue}}})
		if err != nil || response == nil || len(response.Results) != 1 {
			t.Fatalf("read %s: %#v %v", identifier, response, err)
		}
		return response.Results[0]
	}
	await := func(status gu.StatusCode, want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		var last *gu.DataValue
		for time.Now().Before(deadline) {
			last = readValue(2, "A_VALUE")
			if liveGroupsDataValueHasStatus(last, status) && fmt.Sprint(last.Value.Value()) == want {
				t.Logf("A_VALUE matched requested %s: %s", status, liveGroupsDescribeValue(last))
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("A_VALUE want=%s/%s last=%s", want, status, liveGroupsDescribeValue(last))
	}
	writeSelection("lower-b")
	await(gu.StatusOK, "101")
	control["b"]["server_mode"] = "stopped"
	updateControl()
	await(gu.StatusBadNoCommunication, "101")
	if value := readValue(3, "Groups/device-a/SelectedSourceId"); value.Status != gu.StatusOK || value.Value.Value() != "lower-b" {
		t.Fatalf("disconnection silently changed route: %#v", value)
	}
	if value := readValue(2, "B_VALUE"); !liveGroupsDataValueHasStatus(value, gu.StatusOK) || fmt.Sprint(value.Value.Value()) != "22" {
		t.Fatalf("other device group stopped with source b: %s", liveGroupsDescribeValue(value))
	}
	writeSelection("lower-a")
	await(gu.StatusOK, "11")
	writeSelection("lower-b")
	await(gu.StatusBadNoCommunication, "101")
	control["b"]["server_mode"] = "running"
	control["b"]["values"] = map[string]int{"1": 103, "2": 202}
	updateControl()
	await(gu.StatusOK, "103")
	if value := readValue(3, "Groups/device-a/SelectedSourceId"); value.Value.Value() != "lower-b" {
		t.Fatalf("recovery changed route: %#v", value)
	}
	publishControl(original)
	await(gu.StatusOK, "101")
	t.Log("Rust source stopped/restarted: last value retained with BadNoCommunication, offline source selectable, no automatic failover, new epoch samples restored")
}
