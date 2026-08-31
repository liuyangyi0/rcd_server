package opc

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

// TestLiveLowerEndpointProbe is an opt-in read-only smoke test for the exact
// downstream endpoint shipped or migrated with a deployment. It verifies UA
// discovery, Session activation, NamespaceArray resolution and the complete
// legacy catalog browse without changing acquisition or sending commands.
func TestLiveLowerEndpointProbe(t *testing.T) {
	endpoint := os.Getenv("RCD_LOWER_LIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("set RCD_LOWER_LIVE_ENDPOINT to probe a live Rust lower")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := ProbeLower(ctx, LowerConfig{Source: domain.Source{
		ID: "live-lower-probe", GroupID: "live-lower-probe", Endpoint: endpoint, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range result.Stages {
		if !stage.OK {
			t.Fatalf("lower probe stage %s failed: %s", stage.Name, stage.Message)
		}
	}
	if result.EndpointCount == 0 || result.NamespaceCount < 3 || result.VariableCount == 0 || result.MethodCount == 0 {
		t.Fatalf("incomplete lower probe result: %#v", result)
	}
	t.Logf("lower %s: endpoints=%d namespaces=%d variables=%d methods=%d duration=%dms",
		endpoint, result.EndpointCount, result.NamespaceCount, result.VariableCount, result.MethodCount, result.DurationMS)
}

// TestLiveManagerAnonymousNone is an opt-in deployment smoke test. Keeping the
// OPC Session open with RCD_MANAGER_LIVE_HOLD makes it visible in the Web
// session drawer while an operator performs manual checks.
func TestLiveManagerAnonymousNone(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("set RCD_MANAGER_LIVE_ENDPOINT to run against a live manager")
	}
	hold := time.Duration(0)
	if text := os.Getenv("RCD_MANAGER_LIVE_HOLD"); text != "" {
		var err error
		hold, err = time.ParseDuration(text)
		if err != nil || hold < 0 {
			t.Fatalf("invalid RCD_MANAGER_LIVE_HOLD %q: %v", text, err)
		}
	}

	client, selectedURL := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-live-acceptance")

	readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	response, err := client.Read(readCtx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead: []*gu.ReadValueID{{
			NodeID: gu.NewNumericNodeID(0, id.Server_NamespaceArray), AttributeID: gu.AttributeIDValue,
		}},
	})
	cancelRead()
	if err != nil {
		t.Fatalf("read NamespaceArray: %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].Status != gu.StatusOK || response.Results[0].Value == nil {
		t.Fatalf("NamespaceArray response = %#v", response)
	}
	namespaces, ok := response.Results[0].Value.Value().([]string)
	if !ok {
		t.Fatalf("NamespaceArray value = %T", response.Results[0].Value.Value())
	}
	want := []string{
		"http://opcfoundation.org/UA/",
		ApplicationNamespaceURI,
		BusinessNamespaceURI,
		ManagementNamespaceURI,
	}
	if !reflect.DeepEqual(namespaces, want) {
		t.Fatalf("NamespaceArray\n got: %#v\nwant: %#v", namespaces, want)
	}
	t.Logf("connected to %s; NamespaceArray=%#v", selectedURL, namespaces)
	if hold > 0 {
		t.Logf("holding OPC Session open for %s", hold)
		timer := time.NewTimer(hold)
		defer timer.Stop()
		<-timer.C
	}
}

// TestLiveManagerRustInterop exercises the business namespace through a live
// manager which has already discovered the real Rust server in this repository.
// It is opt-in because it intentionally sends a command to the configured test
// serial port. Use only with a disposable or disconnected test device.
func TestLiveManagerRustInterop(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	groupID := os.Getenv("RCD_MANAGER_LIVE_GROUP")
	sourceID := os.Getenv("RCD_MANAGER_LIVE_SOURCE")
	pointID := os.Getenv("RCD_MANAGER_LIVE_POINT")
	sendID := os.Getenv("RCD_MANAGER_LIVE_SEND")
	methodID := os.Getenv("RCD_MANAGER_LIVE_METHOD")
	if endpoint == "" || groupID == "" || sourceID == "" || pointID == "" || sendID == "" || methodID == "" {
		t.Skip("set RCD_MANAGER_LIVE_ENDPOINT/GROUP/SOURCE/POINT/SEND/METHOD to run real Rust interop")
	}

	client, selectedURL := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-rust-interop")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	deviceRefs, err := client.Node(gu.NewStringNodeID(2, "Devices")).References(
		ctx, id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassVariable, true,
	)
	if err != nil {
		t.Fatalf("browse Devices: %v", err)
	}
	if !hasReference(deviceRefs, pointID) || !hasReference(deviceRefs, sendID) {
		t.Fatalf("Devices does not expose point=%q and send=%q: %#v", pointID, sendID, deviceRefs)
	}
	methodRefs, err := client.Node(gu.NewStringNodeID(2, "Methods")).References(
		ctx, id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassMethod, true,
	)
	if err != nil {
		t.Fatalf("browse Methods: %v", err)
	}
	if !hasReference(methodRefs, methodID) {
		t.Fatalf("Methods does not expose %q: %#v", methodID, methodRefs)
	}

	pointNode := gu.NewStringNodeID(2, pointID)
	selectedNode := gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId")
	revisionNode := gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectionRevision")
	readResponse, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnBoth,
		NodesToRead: []*gu.ReadValueID{
			{NodeID: pointNode, AttributeID: gu.AttributeIDValue},
			{NodeID: selectedNode, AttributeID: gu.AttributeIDValue},
			{NodeID: revisionNode, AttributeID: gu.AttributeIDValue},
		},
	})
	if err != nil || len(readResponse.Results) != 3 {
		t.Fatalf("read business and route nodes: response=%#v err=%v", readResponse, err)
	}
	if readResponse.Results[0].Value == nil || readResponse.Results[0].Status == gu.StatusBadNodeIDUnknown {
		t.Fatalf("business point read = %#v", readResponse.Results[0])
	}
	if got, ok := readResponse.Results[1].Value.Value().(string); !ok || got != sourceID || readResponse.Results[1].Status != gu.StatusOK {
		t.Fatalf("SelectedSourceId = value=%#v status=%s", readResponse.Results[1].Value.Value(), readResponse.Results[1].Status)
	}
	revisionBefore, ok := readResponse.Results[2].Value.Value().(uint64)
	if !ok || readResponse.Results[2].Status != gu.StatusOK {
		t.Fatalf("SelectionRevision = %#v", readResponse.Results[2])
	}
	if os.Getenv("RCD_MANAGER_LIVE_EXPECT_NO_COMMUNICATION") != "" && readResponse.Results[0].Status != gu.StatusBadNoCommunication {
		t.Fatalf("expected serial-offline BadNoCommunication, got %s", readResponse.Results[0].Status)
	}

	notify := make(chan *gopcua.PublishNotificationData, 8)
	subscription, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notify)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = subscription.Cancel(closeCtx)
	})
	monitorResponse, err := subscription.Monitor(
		ctx, gu.TimestampsToReturnBoth,
		gopcua.NewMonitoredItemCreateRequestWithDefaults(pointNode, gu.AttributeIDValue, 1),
	)
	if err != nil || len(monitorResponse.Results) != 1 || monitorResponse.Results[0].StatusCode != gu.StatusOK {
		t.Fatalf("monitor point: response=%#v err=%v", monitorResponse, err)
	}
	notificationStatus := awaitDataChange(t, notify)
	if os.Getenv("RCD_MANAGER_LIVE_EXPECT_NO_COMMUNICATION") != "" && notificationStatus != gu.StatusBadNoCommunication {
		t.Fatalf("expected subscribed BadNoCommunication, got %s", notificationStatus)
	}

	selectionWrite, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
		NodeID: selectedNode, AttributeID: gu.AttributeIDValue,
		Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(sourceID)},
	}}})
	if err != nil || len(selectionWrite.Results) != 1 || selectionWrite.Results[0] != gu.StatusOK {
		t.Fatalf("write SelectedSourceId: response=%#v err=%v", selectionWrite, err)
	}
	revisionResponse, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead:        []*gu.ReadValueID{{NodeID: revisionNode, AttributeID: gu.AttributeIDValue}},
	})
	if err != nil || len(revisionResponse.Results) != 1 || revisionResponse.Results[0].Status != gu.StatusOK {
		t.Fatalf("read revised SelectionRevision: response=%#v err=%v", revisionResponse, err)
	}
	if revisionAfter, ok := revisionResponse.Results[0].Value.Value().(uint64); !ok || revisionAfter <= revisionBefore {
		t.Fatalf("SelectionRevision did not advance: before=%d after=%#v", revisionBefore, revisionResponse.Results[0].Value.Value())
	}
	if os.Getenv("RCD_MANAGER_LIVE_SKIP_COMMANDS") != "" {
		t.Logf("safe real Rust interop via %s passed; command write/call intentionally skipped", selectedURL)
		return
	}

	callResponse, err := client.Call(ctx, &gu.CallMethodRequest{
		ObjectID: gu.NewStringNodeID(2, "Methods"), MethodID: gu.NewStringNodeID(2, methodID),
		InputArguments: []*gu.Variant{gu.MustVariant("0B01")},
	})
	if err != nil || callResponse.StatusCode != gu.StatusOK || len(callResponse.OutputArguments) != 1 {
		t.Fatalf("call %s: response=%#v err=%v", methodID, callResponse, err)
	}
	if result, ok := callResponse.OutputArguments[0].Value().(string); !ok || result == "" {
		t.Fatalf("call %s result=%#v", methodID, callResponse.OutputArguments[0].Value())
	}

	commandWrite, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
		NodeID: gu.NewStringNodeID(2, sendID), AttributeID: gu.AttributeIDValue,
		Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant("0B01")},
	}}})
	if err != nil || len(commandWrite.Results) != 1 || commandWrite.Results[0] != gu.StatusOK {
		t.Fatalf("write %s: response=%#v err=%v", sendID, commandWrite, err)
	}

	t.Logf("real Rust interop via %s passed; point status=%s notification status=%s", selectedURL, readResponse.Results[0].Status, notificationStatus)
}

// TestLiveManagerSessionIsolation verifies two simultaneous anonymous upstream
// Sessions can select different members of the same group without leaking the
// route or subscription sample between Sessions. It does not send device
// commands and is safe to run against a live acquisition server.
func TestLiveManagerSessionIsolation(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	groupID := os.Getenv("RCD_MANAGER_LIVE_GROUP")
	sourceA := os.Getenv("RCD_MANAGER_LIVE_SOURCE_A")
	sourceB := os.Getenv("RCD_MANAGER_LIVE_SOURCE_B")
	pointID := os.Getenv("RCD_MANAGER_LIVE_POINT")
	if endpoint == "" || groupID == "" || sourceA == "" || sourceB == "" || pointID == "" {
		t.Skip("set RCD_MANAGER_LIVE_ENDPOINT/GROUP/SOURCE_A/SOURCE_B/POINT to run Session isolation")
	}
	if sourceA == sourceB {
		t.Fatal("Session isolation requires two different sources")
	}

	clientA, _ := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-isolation-a")
	clientB, _ := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-isolation-b")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	selectedNode := gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId")
	pointNode := gu.NewStringNodeID(2, pointID)

	for _, target := range []struct {
		client *gopcua.Client
		source string
	}{{clientA, sourceA}, {clientB, sourceB}} {
		response, err := target.client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
			NodeID: selectedNode, AttributeID: gu.AttributeIDValue,
			Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(target.source)},
		}}})
		if err != nil || len(response.Results) != 1 || response.Results[0] != gu.StatusOK {
			t.Fatalf("select source %s: response=%#v err=%v", target.source, response, err)
		}
	}

	for _, target := range []struct {
		client *gopcua.Client
		source string
	}{{clientA, sourceA}, {clientB, sourceB}} {
		response, err := target.client.Read(ctx, &gu.ReadRequest{
			TimestampsToReturn: gu.TimestampsToReturnBoth,
			NodesToRead: []*gu.ReadValueID{
				{NodeID: selectedNode, AttributeID: gu.AttributeIDValue},
				{NodeID: pointNode, AttributeID: gu.AttributeIDValue},
			},
		})
		if err != nil || len(response.Results) != 2 {
			t.Fatalf("read Session route %s: response=%#v err=%v", target.source, response, err)
		}
		if selected, ok := response.Results[0].Value.Value().(string); !ok || selected != target.source || response.Results[0].Status != gu.StatusOK {
			t.Fatalf("Session route = value=%#v status=%s, want %s", response.Results[0].Value.Value(), response.Results[0].Status, target.source)
		}
		if response.Results[1].Status == gu.StatusBadNodeIDUnknown || response.Results[1].Status == gu.StatusBadConfigurationError {
			t.Fatalf("Session %s point status=%s", target.source, response.Results[1].Status)
		}

		notify := make(chan *gopcua.PublishNotificationData, 8)
		subscription, err := target.client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notify)
		if err != nil {
			t.Fatalf("subscribe Session %s: %v", target.source, err)
		}
		t.Cleanup(func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			_ = subscription.Cancel(closeCtx)
		})
		monitorResponse, err := subscription.Monitor(
			ctx, gu.TimestampsToReturnBoth,
			gopcua.NewMonitoredItemCreateRequestWithDefaults(pointNode, gu.AttributeIDValue, 1),
		)
		if err != nil || len(monitorResponse.Results) != 1 || monitorResponse.Results[0].StatusCode != gu.StatusOK {
			t.Fatalf("monitor Session %s: response=%#v err=%v", target.source, monitorResponse, err)
		}
		_ = awaitDataChange(t, notify)
	}
	t.Logf("Session isolation passed: %s and %s retained independent routes", sourceA, sourceB)
}

// TestLiveManagerNoDefaultRoute verifies that clearing a group's default is a
// real, externally observable state: a newly activated Session gets an empty
// route and business reads fail with BadConfigurationError rather than silently
// choosing a source.
func TestLiveManagerNoDefaultRoute(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	groupID := os.Getenv("RCD_MANAGER_LIVE_GROUP")
	pointID := os.Getenv("RCD_MANAGER_LIVE_POINT")
	if endpoint == "" || groupID == "" || pointID == "" {
		t.Skip("set RCD_MANAGER_LIVE_ENDPOINT/GROUP/POINT after clearing the default source")
	}
	client, _ := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-no-default")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnNeither,
		NodesToRead: []*gu.ReadValueID{
			{NodeID: gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId"), AttributeID: gu.AttributeIDValue},
			{NodeID: gu.NewStringNodeID(2, pointID), AttributeID: gu.AttributeIDValue},
		},
	})
	if err != nil || len(response.Results) != 2 {
		t.Fatalf("read no-default route: response=%#v err=%v", response, err)
	}
	if selected, ok := response.Results[0].Value.Value().(string); !ok || selected != "" || response.Results[0].Status != gu.StatusOK {
		t.Fatalf("new Session selected source = value=%#v status=%s", response.Results[0].Value.Value(), response.Results[0].Status)
	}
	if response.Results[1].Status != gu.StatusBadConfigurationError {
		t.Fatalf("business read without a default = %s", response.Results[1].Status)
	}
}

func connectLiveManagerAnonymous(t *testing.T, endpoint, sessionName string) (*gopcua.Client, string) {
	t.Helper()
	discoveryCtx, cancelDiscovery := context.WithTimeout(context.Background(), 10*time.Second)
	endpoints, err := gopcua.GetEndpoints(discoveryCtx, endpoint)
	cancelDiscovery()
	if err != nil {
		t.Fatalf("GetEndpoints(%s): %v", endpoint, err)
	}
	selected, err := gopcua.SelectEndpoint(endpoints, "None", gu.MessageSecurityModeNone)
	if err != nil {
		t.Fatalf("select anonymous None endpoint: %v", err)
	}
	client, err := gopcua.NewClient(
		selected.EndpointURL,
		gopcua.SecurityFromEndpoint(selected, gu.UserTokenTypeAnonymous),
		gopcua.AuthAnonymous(),
		gopcua.AutoReconnect(false),
		gopcua.SessionName(sessionName),
	)
	if err != nil {
		t.Fatal(err)
	}
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 10*time.Second)
	if err := client.Connect(connectCtx); err != nil {
		cancelConnect()
		t.Fatalf("connect %s: %v", selected.EndpointURL, err)
	}
	cancelConnect()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Close(ctx)
	})
	return client, selected.EndpointURL
}

func hasReference(references []*gu.ReferenceDescription, identifier string) bool {
	want := gu.NewStringNodeID(2, identifier).String()
	for _, reference := range references {
		if reference != nil && reference.NodeID != nil && reference.NodeID.NodeID != nil && reference.NodeID.NodeID.String() == want {
			return true
		}
	}
	return false
}

func awaitDataChange(t *testing.T, notifications <-chan *gopcua.PublishNotificationData) gu.StatusCode {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case notification := <-notifications:
			if notification == nil || notification.Error != nil {
				continue
			}
			change, ok := notification.Value.(*gu.DataChangeNotification)
			if !ok || len(change.MonitoredItems) == 0 || change.MonitoredItems[0].Value == nil {
				continue
			}
			return change.MonitoredItems[0].Value.Status
		case <-timer.C:
			t.Fatal("timed out waiting for initial data change")
		}
	}
}
