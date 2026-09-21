package opc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/api"
)

// Read-only acceptance against the actual deployed lowers. No serial commands,
// default route changes or simulator are used by this test.
func TestLiveStationCommunicationTree(t *testing.T) {
	endpoint, group := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT"), os.Getenv("RCD_MANAGER_LIVE_GROUP")
	if endpoint == "" || group == "" {
		t.Skip("set live manager endpoint and group")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, _ := connectLiveManagerAnonymous(t, endpoint, "station-communication-acceptance")
	defer client.Close(context.Background())
	ns, ok := namespaceIndex(client.Namespaces())[ManagementNamespaceURI]
	if !ok {
		t.Fatal("management namespace missing")
	}
	browse := func(path string) []*gu.ReferenceDescription {
		t.Helper()
		refs, err := client.Node(gu.NewStringNodeID(ns, path)).References(ctx, id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassObject, true)
		if err != nil {
			t.Fatalf("browse %s: %v", path, err)
		}
		return refs
	}
	counts := map[uint32]int{}
	monitors := []*gu.MonitoredItemCreateRequest{}
	for _, member := range browse("Groups/" + group + "/Members") {
		source := member.BrowseName.Name
		root := "Groups/" + group + "/Members/" + managementPathComponent(source) + "/Ports"
		first := true
		for _, port := range browse(root) {
			portToken := managementPathComponent(port.BrowseName.Name)
			for _, device := range browse(root + "/" + portToken + "/Devices") {
				deviceToken := managementPathComponent(device.BrowseName.Name)
				path := root + "/" + portToken + "/Devices/" + deviceToken
				stateID := gu.NewStringNodeID(ns, path+"/CommunicationState")
				values := readLiveValues(t, ctx, client, []*gu.NodeID{stateID,
					gu.NewStringNodeID(ns, path+"/CommunicationStateText"),
					gu.NewStringNodeID(ns, path+"/CommunicationObservedAt"),
					gu.NewStringNodeID(ns, "Sources/"+source+"/Ports/"+portToken+"/Devices/"+deviceToken+"/CommunicationState"),
				})
				for i, value := range values {
					if value == nil || value.Status != gu.StatusOK || value.Value == nil {
						t.Fatalf("%s attribute%d: %#v", path, i, value)
					}
				}
				state, ok := values[0].Value.Value().(uint32)
				if !ok || state > 5 {
					t.Fatalf("invalid state %#v", values[0])
				}
				if state == 0 {
					t.Fatalf("%s has unknown/stale health after deployment warmup", path)
				}
				if values[3].Value.Value() != state {
					t.Fatalf("source/group health disagree at %s", path)
				}
				if observed, ok := values[2].Value.Value().(time.Time); !ok || time.Since(observed) > 15*time.Second {
					t.Fatalf("stale timestamp at %s: %v", path, values[2].Value.Value())
				}
				counts[state]++
				if first {
					first = false
					attrs, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{
						{NodeID: stateID, AttributeID: gu.AttributeIDDataType}, {NodeID: stateID, AttributeID: gu.AttributeIDAccessLevel},
					}})
					if err != nil || len(attrs.Results) != 2 {
						t.Fatalf("attributes: %v", err)
					}
					if fmt.Sprint(attrs.Results[0].Value.Value()) != "i=7" || attrs.Results[1].Value.Value() != uint8(1) {
						t.Fatalf("not read-only UInt32: %#v", attrs.Results)
					}
					monitors = append(monitors, gopcua.NewMonitoredItemCreateRequestWithDefaults(stateID, gu.AttributeIDValue, uint32(len(monitors)+1)))
					t.Logf("%s: %d %v observed=%v", path, state, values[1].Value.Value(), values[2].Value.Value())
				}
			}
		}
	}
	if len(monitors) == 0 {
		t.Fatal("no stations browsed")
	}
	notifications := make(chan *gopcua.PublishNotificationData, 32)
	sub, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel(context.Background())
	response, err := sub.Monitor(ctx, gu.TimestampsToReturnBoth, monitors...)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.StatusCode != gu.StatusOK {
			t.Fatal(result.StatusCode)
		}
	}
	received := map[uint32]bool{}
	for len(received) < len(monitors) {
		select {
		case <-ctx.Done():
			t.Fatal("timeout waiting for station subscriptions")
		case event := <-notifications:
			if event.Error != nil {
				t.Fatal(event.Error)
			}
			if change, ok := event.Value.(*gu.DataChangeNotification); ok {
				for _, item := range change.MonitoredItems {
					if item.Value == nil || item.Value.Status != gu.StatusOK {
						t.Fatalf("bad notification: %#v", item)
					}
					if _, ok := item.Value.Value.Value().(uint32); !ok {
						t.Fatal("notification is not UInt32")
					}
					received[item.ClientHandle] = true
				}
			}
		}
	}
	t.Logf("station state counts=%v; sources subscribed=%d", counts, len(monitors))
}

// Explicit opt-in and fixed isolated ports: do not alter production sources.
func TestLiveStationCommunicationReconnect(t *testing.T) {
	apiURL := os.Getenv("RCD_STATION_HEALTH_MUTATION_API")
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	if apiURL != "http://127.0.0.1:18087" || endpoint != "opc.tcp://127.0.0.1:14840" {
		t.Skip("requires explicit isolated manager mutation API and OPC ports")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, _ := connectLiveManagerAnonymous(t, endpoint, "station-health-reconnect")
	defer client.Close(context.Background())
	ns := namespaceIndex(client.Namespaces())[ManagementNamespaceURI]
	node := gu.NewStringNodeID(ns, "Groups/real-hardware/Members/rcd92-58/Ports/%2Fdev%2FttyWCH0/Devices/11/CommunicationState")
	notifications := make(chan *gopcua.PublishNotificationData, 64)
	sub, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel(context.Background())
	monitored, err := sub.Monitor(ctx, gu.TimestampsToReturnBoth, gopcua.NewMonitoredItemCreateRequestWithDefaults(node, gu.AttributeIDValue, 1))
	if err != nil || len(monitored.Results) != 1 || monitored.Results[0].StatusCode != gu.StatusOK {
		t.Fatalf("monitor: %v", err)
	}
	setEnabled := func(enabled bool) error {
		operationCtx, operationCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer operationCancel()
		url := apiURL + "/api/v1/sources/rcd92-58"
		request, _ := http.NewRequestWithContext(operationCtx, http.MethodGet, url, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		var source api.SourceView
		err = json.NewDecoder(response.Body).Decode(&source)
		response.Body.Close()
		if err != nil {
			return err
		}
		if source.Enabled == enabled {
			return nil
		}
		payload, _ := json.Marshal(api.SourceMutation{GroupID: source.GroupID, GroupBindings: source.GroupBindings,
			Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode,
			Username: source.Username, Enabled: enabled})
		request, _ = http.NewRequestWithContext(operationCtx, http.MethodPut, url, bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", fmt.Sprintf("\"%d\"", source.Revision))
		request.Header.Set("Origin", apiURL)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("set enabled: HTTP %d", response.StatusCode)
		}
		return nil
	}
	defer func() {
		if err := setEnabled(true); err != nil {
			t.Errorf("restore isolated source: %v", err)
		}
	}()
	wait := func(want uint32) {
		t.Helper()
		for {
			select {
			case <-ctx.Done():
				t.Fatalf("no subscribed transition to %d", want)
			case event := <-notifications:
				if event.Error != nil {
					t.Fatal(event.Error)
				}
				if change, ok := event.Value.(*gu.DataChangeNotification); ok {
					for _, item := range change.MonitoredItems {
						if item.Value != nil && item.Value.Value != nil && item.Value.Status == gu.StatusOK && item.Value.Value.Value() == want {
							t.Logf("subscribed state=%d quality=Good", want)
							return
						}
					}
				}
			}
		}
	}
	wait(2)
	if err := setEnabled(false); err != nil {
		t.Fatal(err)
	}
	wait(5)
	if err := setEnabled(true); err != nil {
		t.Fatal(err)
	}
	wait(2)
}
