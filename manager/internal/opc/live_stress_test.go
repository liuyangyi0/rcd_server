package opc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"
)

// TestLiveManagerCommandLock verifies the real pending-command exclusion path.
// It intentionally writes to the configured test serial port.
func TestLiveManagerCommandLock(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	groupID := os.Getenv("RCD_MANAGER_LIVE_GROUP")
	sourceA := os.Getenv("RCD_MANAGER_LIVE_SOURCE_A")
	sourceB := os.Getenv("RCD_MANAGER_LIVE_SOURCE_B")
	sendID := os.Getenv("RCD_MANAGER_LIVE_SEND")
	if endpoint == "" || groupID == "" || sourceA == "" || sourceB == "" || sendID == "" {
		t.Skip("set live endpoint/group/source A/source B/send to run command-lock acceptance")
	}

	client, _ := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-command-lock")
	selectedNode := gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId")
	sendNode := gu.NewStringNodeID(2, sendID)
	writeString := func(ctx context.Context, node *gu.NodeID, value string) gu.StatusCode {
		response, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
			NodeID: node, AttributeID: gu.AttributeIDValue,
			Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(value)},
		}}})
		if err != nil || len(response.Results) != 1 {
			t.Fatalf("write %s=%q: response=%#v err=%v", node, value, response, err)
		}
		return response.Results[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if status := writeString(ctx, selectedNode, sourceA); status != gu.StatusOK {
		t.Fatalf("select initial source: %s", status)
	}
	if status := writeString(ctx, sendNode, "0B01"); status != gu.StatusOK {
		t.Fatalf("first command write: %s", status)
	}
	if status := writeString(ctx, sendNode, "0B01"); status != gu.StatusBadWaitingForResponse {
		t.Fatalf("concurrent command status=%s, want BadWaitingForResponse", status)
	}
	if status := writeString(ctx, selectedNode, sourceB); status != gu.StatusBadWaitingForResponse {
		t.Fatalf("selection during pending status=%s, want BadWaitingForResponse", status)
	}

	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("command did not reach a terminal state and release selection lock")
		case <-ticker.C:
			status := writeString(ctx, selectedNode, sourceB)
			if status == gu.StatusBadWaitingForResponse {
				continue
			}
			if status != gu.StatusOK {
				t.Fatalf("selection after command terminal status=%s", status)
			}
			if reset := writeString(ctx, selectedNode, sourceA); reset != gu.StatusOK {
				t.Fatalf("restore source status=%s", reset)
			}
			t.Log("concurrent command and selection were rejected; terminal ACK released the group lock")
			return
		}
	}
}

// TestLiveManagerRouteRefresh subscribes before changing the Session route and
// requires a second notification even when both real sources currently expose
// the same value. This catches failures in the targeted forced-resample path.
func TestLiveManagerRouteRefresh(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	groupID := os.Getenv("RCD_MANAGER_LIVE_GROUP")
	sourceA := os.Getenv("RCD_MANAGER_LIVE_SOURCE_A")
	sourceB := os.Getenv("RCD_MANAGER_LIVE_SOURCE_B")
	pointID := os.Getenv("RCD_MANAGER_LIVE_POINT")
	if endpoint == "" || groupID == "" || sourceA == "" || sourceB == "" || pointID == "" {
		t.Skip("set live endpoint/group/source A/source B/point to run route-refresh acceptance")
	}

	client, _ := connectLiveManagerAnonymous(t, endpoint, "rcd-manager-route-refresh")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	selectedNode := gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId")
	writeSelection := func(sourceID string) {
		response, err := client.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
			NodeID: selectedNode, AttributeID: gu.AttributeIDValue,
			Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(sourceID)},
		}}})
		if err != nil || len(response.Results) != 1 || response.Results[0] != gu.StatusOK {
			t.Fatalf("select %s: response=%#v err=%v", sourceID, response, err)
		}
	}
	writeSelection(sourceA)

	notify := make(chan *gopcua.PublishNotificationData, 16)
	subscription, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notify)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = subscription.Cancel(closeCtx)
	})
	response, err := subscription.Monitor(
		ctx, gu.TimestampsToReturnBoth,
		gopcua.NewMonitoredItemCreateRequestWithDefaults(gu.NewStringNodeID(2, pointID), gu.AttributeIDValue, 1),
	)
	if err != nil || len(response.Results) != 1 || response.Results[0].StatusCode != gu.StatusOK {
		t.Fatalf("monitor: response=%#v err=%v", response, err)
	}
	_ = awaitDataChange(t, notify)
	started := time.Now()
	writeSelection(sourceB)
	_ = awaitDataChange(t, notify)
	latency := time.Since(started)
	writeSelection(sourceA)
	t.Logf("same-value route refresh notification latency=%s", latency)
}

// TestLiveManagerSoak opens the configured number of simultaneous Sessions and
// subscribes every legacy Device variable. It is opt-in and intended to run via
// nohup on a disposable remote test host.
func TestLiveManagerSoak(t *testing.T) {
	endpoint := os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	groupID := os.Getenv("RCD_MANAGER_LIVE_GROUP")
	sourceA := os.Getenv("RCD_MANAGER_LIVE_SOURCE_A")
	sourceB := os.Getenv("RCD_MANAGER_LIVE_SOURCE_B")
	durationText := os.Getenv("RCD_MANAGER_LIVE_SOAK_DURATION")
	if endpoint == "" || groupID == "" || sourceA == "" || sourceB == "" || durationText == "" {
		t.Skip("set live endpoint/group/source A/source B/soak duration to run the full-subscription soak")
	}
	duration, err := time.ParseDuration(durationText)
	if err != nil || duration <= 0 {
		t.Fatalf("invalid soak duration %q: %v", durationText, err)
	}
	clientCount := 10
	if text := os.Getenv("RCD_MANAGER_LIVE_CLIENTS"); text != "" {
		clientCount, err = strconv.Atoi(text)
		if err != nil || clientCount < 1 || clientCount > 10 {
			t.Fatalf("invalid client count %q", text)
		}
	}
	switchInterval := 15 * time.Minute
	if text := os.Getenv("RCD_MANAGER_LIVE_SWITCH_INTERVAL"); text != "" {
		switchInterval, err = time.ParseDuration(text)
		if err != nil || switchInterval <= 0 {
			t.Fatalf("invalid switch interval %q", text)
		}
	}

	var notifications atomic.Uint64
	var monitoredValues atomic.Uint64
	var notificationErrors atomic.Uint64
	clientValues := make([]*atomic.Uint64, clientCount)
	for index := range clientValues {
		clientValues[index] = &atomic.Uint64{}
	}
	clients := make([]*gopcua.Client, 0, clientCount)
	selectedSources := make([]string, 0, clientCount)
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer setupCancel()

	var nodeIDs []*gu.NodeID
	for clientIndex := 0; clientIndex < clientCount; clientIndex++ {
		client, _ := connectLiveManagerAnonymous(t, endpoint, fmt.Sprintf("rcd-manager-soak-%02d", clientIndex+1))
		clients = append(clients, client)
		if nodeIDs == nil {
			references, browseErr := client.Node(gu.NewStringNodeID(2, "Devices")).References(
				setupCtx, id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassVariable, true,
			)
			if browseErr != nil {
				t.Fatalf("browse Devices: %v", browseErr)
			}
			for _, reference := range references {
				if reference != nil && reference.NodeID != nil && reference.NodeID.NodeID != nil {
					nodeIDs = append(nodeIDs, reference.NodeID.NodeID)
				}
			}
			if len(nodeIDs) < 5000 {
				t.Fatalf("browsed only %d Device variables", len(nodeIDs))
			}
		}

		sourceID := sourceA
		if clientIndex%2 == 1 {
			sourceID = sourceB
		}
		selectedSources = append(selectedSources, sourceID)
		selection, writeErr := client.Write(setupCtx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
			NodeID: gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId"), AttributeID: gu.AttributeIDValue,
			Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(sourceID)},
		}}})
		if writeErr != nil || len(selection.Results) != 1 || selection.Results[0] != gu.StatusOK {
			t.Fatalf("client %d select %s: response=%#v err=%v", clientIndex+1, sourceID, selection, writeErr)
		}

		notify := make(chan *gopcua.PublishNotificationData, 4096)
		subscription, subscribeErr := client.Subscribe(setupCtx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notify)
		if subscribeErr != nil {
			t.Fatalf("client %d subscribe: %v", clientIndex+1, subscribeErr)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = subscription.Cancel(ctx)
		})
		go func(index int) {
			for notification := range notify {
				if notification == nil {
					continue
				}
				if notification.Error != nil {
					notificationErrors.Add(1)
					continue
				}
				notifications.Add(1)
				if change, ok := notification.Value.(*gu.DataChangeNotification); ok {
					count := uint64(len(change.MonitoredItems))
					monitoredValues.Add(count)
					clientValues[index].Add(count)
				}
			}
		}(clientIndex)

		for start := 0; start < len(nodeIDs); start += 500 {
			end := min(start+500, len(nodeIDs))
			requests := make([]*gu.MonitoredItemCreateRequest, 0, end-start)
			for index := start; index < end; index++ {
				requests = append(requests, gopcua.NewMonitoredItemCreateRequestWithDefaults(nodeIDs[index], gu.AttributeIDValue, uint32(index+1)))
			}
			response, monitorErr := subscription.Monitor(setupCtx, gu.TimestampsToReturnBoth, requests...)
			if monitorErr != nil || len(response.Results) != len(requests) {
				t.Fatalf("client %d monitor batch %d: results=%d err=%v", clientIndex+1, start/500, len(response.Results), monitorErr)
			}
			for resultIndex, result := range response.Results {
				if result.StatusCode != gu.StatusOK {
					t.Fatalf("client %d monitor item %d status=%s", clientIndex+1, start+resultIndex, result.StatusCode)
				}
			}
		}
		t.Logf("client=%d source=%s monitored=%d", clientIndex+1, sourceID, len(nodeIDs))
	}

	started := time.Now()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	switchTicker := time.NewTicker(switchInterval)
	defer switchTicker.Stop()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	switchIndex := 0
	for {
		select {
		case <-ticker.C:
			t.Logf("SOAK elapsed=%s clients=%d nodes_per_client=%d notifications=%d values=%d errors=%d",
				time.Since(started).Round(time.Second), clientCount, len(nodeIDs), notifications.Load(), monitoredValues.Load(), notificationErrors.Load())
		case <-switchTicker.C:
			index := switchIndex % len(clients)
			switchIndex++
			target := sourceA
			if selectedSources[index] == sourceA {
				target = sourceB
			}
			before := clientValues[index].Load()
			refreshStarted := time.Now()
			switchCtx, switchCancel := context.WithTimeout(context.Background(), 30*time.Second)
			response, switchErr := clients[index].Write(switchCtx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
				NodeID: gu.NewStringNodeID(3, "Groups/"+groupID+"/SelectedSourceId"), AttributeID: gu.AttributeIDValue,
				Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(target)},
			}}})
			if switchErr != nil || len(response.Results) != 1 || response.Results[0] != gu.StatusOK {
				switchCancel()
				t.Fatalf("route refresh client %d to %s: response=%#v err=%v", index+1, target, response, switchErr)
			}
			selectedSources[index] = target
			want := before + uint64(len(nodeIDs))
			for clientValues[index].Load() < want && switchCtx.Err() == nil {
				time.Sleep(5 * time.Millisecond)
			}
			latency := time.Since(refreshStarted)
			got := clientValues[index].Load() - before
			switchCancel()
			if got < uint64(len(nodeIDs)) {
				t.Fatalf("route refresh client %d delivered %d/%d values in %s", index+1, got, len(nodeIDs), latency)
			}
			t.Logf("ROUTE_REFRESH client=%d source=%s values=%d full_delivery=%s within_500ms=%t",
				index+1, target, got, latency, latency <= 500*time.Millisecond)
		case <-timer.C:
			if notificationErrors.Load() != 0 {
				t.Fatalf("soak completed with %d subscription errors", notificationErrors.Load())
			}
			t.Logf("SOAK PASS duration=%s clients=%d nodes_per_client=%d notifications=%d values=%d",
				time.Since(started).Round(time.Second), clientCount, len(nodeIDs), notifications.Load(), monitoredValues.Load())
			return
		}
	}
}
