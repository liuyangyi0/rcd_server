package opc

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/api"
)

// Opt-in only: this test browses the management namespace of the disposable
// two-Rust/PTY deployment and changes only the new client's Session route.
func TestLiveGroupMemberTreeBrowsesPhysicalBindingsAndControlsSelection(t *testing.T) {
	endpoint := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT")
	apiURL := strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	if endpoint == "" || apiURL == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT and RCD_DEVICE_GROUPS_API for the isolated two-Rust PTY deployment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, _ := connectLiveManagerAnonymous(t, endpoint, "member-tree-live")
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	namespaces := namespaceIndex(client.Namespaces())
	managementNS, ok := namespaces[ManagementNamespaceURI]
	if !ok {
		t.Fatalf("management namespace missing from %v", client.Namespaces())
	}

	membersID := gu.NewStringNodeID(managementNS, "Groups/device-a/Members")
	references, err := client.Node(membersID).References(ctx, id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassObject, true)
	if err != nil {
		t.Fatalf("browse group member sources: %v", err)
	}
	browsed := make(map[string]bool, len(references))
	for _, reference := range references {
		browsed[reference.BrowseName.Name] = true
	}
	for _, sourceID := range []string{"lower-a", "lower-b"} {
		if !browsed[sourceID] {
			t.Fatalf("source %s missing below Groups/device-a/Members: %#v", sourceID, references)
		}
	}

	read := func(identifier string) any {
		t.Helper()
		response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{
			NodeID: gu.NewStringNodeID(managementNS, identifier), AttributeID: gu.AttributeIDValue,
		}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] == nil || response.Results[0].Status != gu.StatusOK || response.Results[0].Value == nil {
			t.Fatalf("read %s: response=%#v err=%v", identifier, response, err)
		}
		return response.Results[0].Value.Value()
	}
	writeSelection := func(target *gopcua.Client, sourceID string) {
		t.Helper()
		response, err := target.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
			NodeID: gu.NewStringNodeID(managementNS, "Groups/device-a/SelectedSourceId"), AttributeID: gu.AttributeIDValue,
			Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(sourceID)},
		}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] != gu.StatusOK {
			t.Fatalf("select %s: response=%#v err=%v", sourceID, response, err)
		}
	}

	for _, sourceID := range []string{"lower-a", "lower-b"} {
		var topology api.SourceTopologyView
		liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/"+sourceID+"/topology", 0, nil, &topology, http.StatusOK)
		var matchedPort *api.TopologyPortView
		var matchedDevice *api.TopologyDeviceView
		for portIndex := range topology.Ports {
			for deviceIndex := range topology.Ports[portIndex].Devices {
				device := &topology.Ports[portIndex].Devices[deviceIndex]
				if device.GroupID == "device-a" {
					matchedPort, matchedDevice = &topology.Ports[portIndex], device
					break
				}
			}
		}
		if matchedPort == nil || matchedDevice == nil {
			t.Fatalf("source %s has no device-a physical binding: %#v", sourceID, topology)
		}
		sourcePath := "Groups/device-a/Members/" + managementPathComponent(sourceID)
		devicePath := sourcePath + "/Ports/" + managementPathComponent(matchedPort.PortID) + "/Devices/" + managementPathComponent(matchedDevice.DeviceID)
		checks := map[string]any{
			sourcePath + "/SourceId":        sourceID,
			sourcePath + "/Selectable":      true,
			sourcePath + "/ConnectionState": "Connected",
			sourcePath + "/CatalogState":    "Ready",
			sourcePath + "/Ports/" + managementPathComponent(matchedPort.PortID) + "/PortId": matchedPort.PortID,
			devicePath + "/DeviceId":              matchedDevice.DeviceID,
			devicePath + "/DeviceAddress":         matchedDevice.DeviceAddress,
			devicePath + "/AddressKnown":          true,
			devicePath + "/DeviceName":            matchedDevice.Name,
			devicePath + "/KKSPrefix":             matchedDevice.KKSPrefix,
			devicePath + "/PointCount":            uint32(matchedDevice.PointCount + matchedDevice.CalculationCount),
			devicePath + "/RawPointCount":         uint32(matchedDevice.PointCount),
			devicePath + "/CalculationPointCount": uint32(matchedDevice.CalculationCount),
			devicePath + "/Configured":            true,
			devicePath + "/ExplicitlyBound":       matchedDevice.Bound,
			devicePath + "/PresentInTopology":     true,
			devicePath + "/Discovered":            true,
		}
		for identifier, want := range checks {
			if got := read(identifier); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s=%#v (%T), want %#v (%T)", identifier, got, got, want, want)
			}
		}
		expected := read(sourcePath + "/ExpectedPointCount").(uint32)
		provided := read(sourcePath + "/ProvidedPointCount").(uint32)
		missing := read(sourcePath + "/MissingPointCount").(uint32)
		complete := read(sourcePath + "/CompleteCoverage").(bool)
		if provided+missing != expected || complete != (missing == 0) {
			t.Fatalf("%s coverage expected=%d provided=%d missing=%d complete=%v", sourceID, expected, provided, missing, complete)
		}
	}

	var sourceBefore api.SourceView
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/sources/lower-a", 0, nil, &sourceBefore, http.StatusOK)
	nameChanges, _ := subscribeMemberValue(t, ctx, client,
		gu.NewStringNodeID(managementNS, "Groups/device-a/Members/lower-a/SourceName"), 700)
	if got := awaitMemberString(t, nameChanges, 5*time.Second); got != sourceBefore.Name {
		t.Fatalf("initial member SourceName=%q want=%q", got, sourceBefore.Name)
	}
	updatedName := sourceBefore.Name + " [hot-update]"
	mutation := api.SourceMutation{
		ID: sourceBefore.ID, GroupID: sourceBefore.GroupID, GroupBindings: sourceBefore.GroupBindings,
		Name: updatedName, Endpoint: sourceBefore.Endpoint, SecurityPolicy: sourceBefore.SecurityPolicy,
		SecurityMode: sourceBefore.SecurityMode, Username: sourceBefore.Username, Enabled: sourceBefore.Enabled,
	}
	var sourceUpdated api.SourceView
	liveGroupsJSON(t, apiURL, http.MethodPut, "/api/v1/sources/lower-a", sourceBefore.Revision, mutation, &sourceUpdated, http.StatusOK)
	if got := awaitMemberString(t, nameChanges, 5*time.Second); got != updatedName {
		t.Fatalf("hot-updated member SourceName=%q want=%q", got, updatedName)
	}
	t.Cleanup(func() {
		mutation.Name = sourceBefore.Name
		var restored api.SourceView
		liveGroupsJSON(t, apiURL, http.MethodPut, "/api/v1/sources/lower-a", sourceUpdated.Revision, mutation, &restored, http.StatusOK)
	})

	writeSelection(client, "lower-a")
	if got := read("Groups/device-a/Members/lower-a/IsSelected"); got != true {
		t.Fatalf("lower-a IsSelected=%#v after selecting lower-a", got)
	}
	if got := read("Groups/device-a/Members/lower-b/IsSelected"); got != false {
		t.Fatalf("lower-b IsSelected=%#v after selecting lower-a", got)
	}
	observer, _ := connectLiveManagerAnonymous(t, endpoint, "member-tree-observer")
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	writeSelection(observer, "lower-a")
	selectedBID := gu.NewStringNodeID(managementNS, "Groups/device-a/Members/lower-b/IsSelected")
	subscribeSelected := func(target *gopcua.Client, handle uint32) (<-chan *gopcua.PublishNotificationData, *gopcua.Subscription) {
		t.Helper()
		notifications := make(chan *gopcua.PublishNotificationData, 16)
		subscription, err := target.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 100 * time.Millisecond}, notifications)
		if err != nil {
			t.Fatalf("subscribe IsSelected: %v", err)
		}
		response, err := subscription.Monitor(ctx, gu.TimestampsToReturnBoth,
			gopcua.NewMonitoredItemCreateRequestWithDefaults(selectedBID, gu.AttributeIDValue, handle))
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0].StatusCode != gu.StatusOK {
			t.Fatalf("monitor IsSelected: response=%#v err=%v", response, err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = subscription.Cancel(cleanup)
		})
		return notifications, subscription
	}
	selectedChanges, _ := subscribeSelected(client, 701)
	observerChanges, _ := subscribeSelected(observer, 702)
	if got := awaitMemberSelected(t, selectedChanges, 5*time.Second); got {
		t.Fatal("target Session initial lower-b IsSelected=true, want false")
	}
	if got := awaitMemberSelected(t, observerChanges, 5*time.Second); got {
		t.Fatal("observer Session initial lower-b IsSelected=true, want false")
	}

	writeSelection(client, "lower-b")
	if got := awaitMemberSelected(t, selectedChanges, 5*time.Second); !got {
		t.Fatal("target Session subscription did not change lower-b IsSelected to true")
	}
	assertNoMemberSelectedChange(t, observerChanges, 500*time.Millisecond)
	if got := read("Groups/device-a/SelectedSourceId"); got != "lower-b" {
		t.Fatalf("SelectedSourceId=%#v after selecting lower-b", got)
	}
	if got := read("Groups/device-a/Members/lower-a/IsSelected"); got != false {
		t.Fatalf("lower-a IsSelected=%#v after selecting lower-b", got)
	}
	if got := read("Groups/device-a/Members/lower-b/IsSelected"); got != true {
		t.Fatalf("lower-b IsSelected=%#v after selecting lower-b", got)
	}
	t.Log("browsed source -> physical port -> device tree; IsSelected subscription changed only in the selected OPC Session")
}

func awaitMemberSelected(t *testing.T, notifications <-chan *gopcua.PublishNotificationData, timeout time.Duration) bool {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case notification := <-notifications:
			if notification == nil || notification.Error != nil {
				continue
			}
			change, ok := notification.Value.(*gu.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range change.MonitoredItems {
				if item == nil || item.Value == nil || item.Value.Status != gu.StatusOK || item.Value.Value == nil {
					continue
				}
				value, ok := item.Value.Value.Value().(bool)
				if ok {
					return value
				}
			}
		case <-timer.C:
			t.Fatal("timed out waiting for IsSelected data change")
		}
	}
}

func subscribeMemberValue(t *testing.T, ctx context.Context, client *gopcua.Client, nodeID *gu.NodeID, handle uint32) (<-chan *gopcua.PublishNotificationData, *gopcua.Subscription) {
	t.Helper()
	notifications := make(chan *gopcua.PublishNotificationData, 16)
	subscription, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 100 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatalf("subscribe member value: %v", err)
	}
	response, err := subscription.Monitor(ctx, gu.TimestampsToReturnBoth,
		gopcua.NewMonitoredItemCreateRequestWithDefaults(nodeID, gu.AttributeIDValue, handle))
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0].StatusCode != gu.StatusOK {
		t.Fatalf("monitor member value: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = subscription.Cancel(cleanup)
	})
	return notifications, subscription
}

func awaitMemberString(t *testing.T, notifications <-chan *gopcua.PublishNotificationData, timeout time.Duration) string {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case notification := <-notifications:
			if notification == nil || notification.Error != nil {
				continue
			}
			change, ok := notification.Value.(*gu.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range change.MonitoredItems {
				if item == nil || item.Value == nil || item.Value.Value == nil {
					continue
				}
				if item.Value.Status != gu.StatusOK {
					t.Fatalf("member metadata subscription returned transient bad status %s", item.Value.Status)
				}
				value, ok := item.Value.Value.Value().(string)
				if ok {
					return value
				}
			}
		case <-timer.C:
			t.Fatal("timed out waiting for member string data change")
		}
	}
}

func assertNoMemberSelectedChange(t *testing.T, notifications <-chan *gopcua.PublishNotificationData, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case notification := <-notifications:
			if notification == nil || notification.Error != nil {
				continue
			}
			change, ok := notification.Value.(*gu.DataChangeNotification)
			if ok && len(change.MonitoredItems) > 0 {
				t.Fatalf("unselected observer Session received IsSelected data change: %#v", change.MonitoredItems)
			}
		case <-timer.C:
			return
		}
	}
}
