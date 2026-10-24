package opc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"
	"rcd_manager/internal/api"
)

// Requires test-only rules installed in a separate DB and explicit isolated ports.
// Only the isolated manager connection is stopped; no real lower is modified.
func TestLiveCalculationStateInputs(t *testing.T) {
	base, endpoint := os.Getenv("RCD_STATE_INPUTS_MUTATION_API"), os.Getenv("RCD_MANAGER_LIVE_ENDPOINT")
	if base != "http://127.0.0.1:18087" || endpoint != "opc.tcp://127.0.0.1:14840" {
		t.Skip("requires isolated state-input acceptance manager")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	call := func(method, path string, body any, revision uint64, target any) error {
		data, _ := json.Marshal(body)
		request, err := http.NewRequestWithContext(ctx, method, base+"/api/v1"+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", base)
		if revision != 0 {
			request.Header.Set("If-Match", fmt.Sprintf("\"%d\"", revision))
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			content, _ := io.ReadAll(response.Body)
			return fmt.Errorf("%s: HTTP%d %s", path, response.StatusCode, content)
		}
		if target != nil {
			return json.NewDecoder(response.Body).Decode(target)
		}
		return nil
	}
	setEnabled := func(enabled bool) error {
		var source api.SourceView
		if err := call("GET", "/sources/rcd-58", nil, 0, &source); err != nil {
			return err
		}
		return call("PUT", "/sources/rcd-58", api.SourceMutation{GroupID: source.GroupID, GroupBindings: source.GroupBindings, Name: source.Name, Endpoint: source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode, Username: source.Username, Enabled: enabled}, source.Revision, nil)
	}
	setDefault := func(source string) error {
		var group struct {
			Revision uint64 `json:"revision"`
		}
		if err := call("GET", "/groups/serial-acquisition", nil, 0, &group); err != nil {
			return err
		}
		return call("PUT", "/groups/serial-acquisition/default-source", map[string]string{"source_id": source}, group.Revision, nil)
	}
	defer func() {
		if err := setDefault("rcd-46"); err != nil {
			t.Error(err)
		}
		if err := setEnabled(true); err != nil {
			t.Error(err)
		}
	}()
	client, _ := connectLiveManagerAnonymous(t, endpoint, "state-inputs-live")
	defer client.Close(context.Background())
	names := []string{"ACCEPT_FIXED46", "ACCEPT_FIXED58", "ACCEPT_DEFAULT", "ACCEPT_ALARM58"}
	nodes := make([]*gu.NodeID, len(names))
	monitors := make([]*gu.MonitoredItemCreateRequest, len(names))
	for i, name := range names {
		nodes[i] = gu.NewStringNodeID(2, name)
		monitors[i] = gopcua.NewMonitoredItemCreateRequestWithDefaults(nodes[i], gu.AttributeIDValue, uint32(i+1))
	}
	notifications := make(chan *gopcua.PublishNotificationData, 64)
	sub, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel(context.Background())
	result, err := sub.Monitor(ctx, gu.TimestampsToReturnBoth, monitors...)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Results {
		if item.StatusCode != gu.StatusOK {
			t.Fatal(item.StatusCode)
		}
	}
	waitValue := func(handle, want uint32) {
		t.Helper()
		for {
			select {
			case <-ctx.Done():
				t.Fatalf("no subscription handle%d=%d", handle, want)
			case event := <-notifications:
				if event.Error != nil {
					t.Fatal(event.Error)
				}
				if change, ok := event.Value.(*gu.DataChangeNotification); ok {
					for _, item := range change.MonitoredItems {
						if item.ClientHandle == handle && item.Value != nil && item.Value.Value != nil && item.Value.Value.Value() == want {
							if item.Value.Status != gu.StatusOK {
								t.Fatal(item.Value.Status)
							}
							t.Logf("subscription %s=%d Good", names[handle-1], want)
							return
						}
					}
				}
			}
		}
	}
	assertRead := func(want []uint32) {
		t.Helper()
		values := readLiveValues(t, ctx, client, nodes)
		for i, value := range values {
			if value.Value == nil || value.Value.Value() != want[i] || value.Status != gu.StatusOK {
				t.Fatalf("%s want%d Good, got%#v", names[i], want[i], value)
			}
		}
	}
	waitValue(1, 1)
	assertRead([]uint32{1, 2, 1, 1})
	if err := setDefault("rcd-58"); err != nil {
		t.Fatal(err)
	}
	waitValue(3, 2)
	assertRead([]uint32{1, 2, 2, 1})
	if err := setDefault("rcd-46"); err != nil {
		t.Fatal(err)
	}
	waitValue(3, 1)
	if err := setEnabled(false); err != nil {
		t.Fatal(err)
	}
	waitValue(2, 5)
	assertRead([]uint32{1, 5, 1, 1})
	if err := setEnabled(true); err != nil {
		t.Fatal(err)
	}
	waitValue(2, 2)
	assertRead([]uint32{1, 2, 1, 1})
	t.Log("fixed/default routes, live OPC Read/Subscribe, offline 5 retained with Good quality, reconnect passed")
}
