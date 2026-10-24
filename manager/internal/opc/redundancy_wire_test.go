package opc

import (
	"context"
	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"
	"net/url"
	"os"
	"testing"
	"time"
)

// Opt-in isolated manager only. browser-smoke creates this group with disabled
// sources and enables its policy. No source selection or device command is sent.
func TestRedundancyIsolatedWire(t *testing.T) {
	endpoint := os.Getenv("RCD_REDUNDANCY_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("isolated endpoint not configured")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "4840" || parsed.Port() == "4841" {
		t.Fatal("isolated loopback endpoint required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, _ := connectLiveManagerAnonymous(t, endpoint, "redundancy-a")
	b, _ := connectLiveManagerAnonymous(t, endpoint, "redundancy-b")
	ns, ok := namespaceIndex(a.Namespaces())[ManagementNamespaceURI]
	if !ok {
		t.Fatal("missing namespace")
	}
	root := "Groups/browser-group"
	refs, err := a.Node(gu.NewStringNodeID(ns, root)).References(ctx, id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassVariable, true)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ref := range refs {
		names[ref.BrowseName.Name] = true
	}
	for _, name := range []string{"AutomaticSelection", "AutoSwitchEnabled", "DefaultAutomatic", "DataDifferenceAlarmActive", "DataDifferenceCount", "DataComparisonState", "LastAutoSwitchReason"} {
		if !names[name] {
			t.Fatal("missing node", name)
		}
	}
	mode := gu.NewStringNodeID(ns, root+"/AutomaticSelection")
	readMode := func(c *gopcua.Client) bool {
		t.Helper()
		v := readLiveValues(t, ctx, c, []*gu.NodeID{mode})[0]
		if v.Status != gu.StatusOK || v.Value == nil {
			t.Fatal(v)
		}
		value, ok := v.Value.Value().(bool)
		if !ok {
			t.Fatal(v)
		}
		return value
	}
	if !readMode(a) || !readMode(b) {
		t.Fatal("new sessions did not inherit automatic policy")
	}
	notifications := make(chan *gopcua.PublishNotificationData, 16)
	sub, err := a.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 100 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel(context.Background())
	result, err := sub.Monitor(ctx, gu.TimestampsToReturnBoth, gopcua.NewMonitoredItemCreateRequestWithDefaults(mode, gu.AttributeIDValue, 1))
	if err != nil || result.Results[0].StatusCode != gu.StatusOK {
		t.Fatal(result, err)
	}
	write, err := a.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{NodeID: mode, AttributeID: gu.AttributeIDValue, Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(false)}}}})
	if err != nil || write.Results[0] != gu.StatusOK {
		t.Fatal(write, err)
	}
	if readMode(a) || !readMode(b) {
		t.Fatal("session-scoped write leaked")
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal("mode notification timeout")
		case n := <-notifications:
			if n.Error != nil {
				t.Fatal(n.Error)
			}
			if change, ok := n.Value.(*gu.DataChangeNotification); ok {
				for _, item := range change.MonitoredItems {
					if item.Value.Status == gu.StatusOK && item.Value.Value != nil && item.Value.Value.Value() == false {
						t.Log("Browse/Read/Write/Subscribe passed; two anonymous sessions isolated")
						return
					}
				}
			}
		}
	}
}
