// Read-only live acceptance on 46. No OPC Write/Call or serial operation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func check(endpoint, fixtures string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eps, err := opcua.GetEndpoints(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	ep, err := opcua.SelectEndpoint(eps, ua.SecurityPolicyURINone, ua.MessageSecurityModeNone)
	if err != nil {
		return nil, err
	}
	c, err := opcua.NewClient(endpoint, opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeAnonymous), opcua.AuthAnonymous(), opcua.AutoReconnect(false))
	if err != nil {
		return nil, err
	}
	if err = c.Connect(ctx); err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	read := func(n *ua.NodeID, a ua.AttributeID) (*ua.DataValue, error) {
		r, e := c.Read(ctx, &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnBoth, NodesToRead: []*ua.ReadValueID{{NodeID: n, AttributeID: a}}})
		if e != nil {
			return nil, e
		}
		if r == nil || len(r.Results) != 1 || r.Results[0] == nil {
			return nil, fmt.Errorf("missing read result")
		}
		return r.Results[0], nil
	}
	nsValue, err := read(ua.NewNumericNodeID(0, id.Server_NamespaceArray), ua.AttributeIDValue)
	if err != nil {
		return nil, err
	}
	ns := uint16(0)
	for i, v := range nsValue.Value.Value().([]string) {
		if v == "urn:SerialServer" {
			ns = uint16(i)
		}
	}
	if ns == 0 {
		return nil, fmt.Errorf("business namespace missing")
	}
	// Compare every original receive/transmit name, not merely a few examples.
	expected := map[string]byte{}
	for _, f := range []struct {
		file   string
		access byte
	}{{"FromUnit1ToUnit9.Txt", 3}, {"FromUnit9ToUnit1.Txt", 1}} {
		body, e := os.ReadFile(filepath.Join(fixtures, f.file))
		if e != nil {
			return nil, e
		}
		for _, name := range strings.Fields(string(body)) {
			expected[name] = f.access
		}
	}
	if len(expected) != 54 {
		return nil, fmt.Errorf("unexpected golden point count: %d", len(expected))
	}
	verified := make([]map[string]any, 0, len(expected))
	for name, wantedAccess := range expected {
		n := ua.NewStringNodeID(ns, name)
		r, e := c.Read(ctx, &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnBoth, NodesToRead: []*ua.ReadValueID{
			{NodeID: n, AttributeID: ua.AttributeIDBrowseName}, {NodeID: n, AttributeID: ua.AttributeIDDataType},
			{NodeID: n, AttributeID: ua.AttributeIDAccessLevel}, {NodeID: n, AttributeID: ua.AttributeIDValue},
			{NodeID: n, AttributeID: ua.AttributeIDDescription},
		}})
		if e != nil {
			return nil, e
		}
		if r == nil || len(r.Results) != 5 {
			return nil, fmt.Errorf("missing point metadata: %s", name)
		}
		for _, i := range []int{0, 1, 2, 4} {
			if r.Results[i] == nil || r.Results[i].Status != ua.StatusOK || r.Results[i].Value == nil {
				return nil, fmt.Errorf("bad metadata %s: %d", name, i)
			}
		}
		browse, ok := r.Results[0].Value.Value().(*ua.QualifiedName)
		if !ok || browse.Name != name {
			return nil, fmt.Errorf("renamed point: %s", name)
		}
		typeID, ok := r.Results[1].Value.Value().(*ua.NodeID)
		wantType := uint32(id.String)
		if strings.HasSuffix(name, ".Fire") || strings.HasPrefix(name, "DATA.Header.") {
			wantType = id.UInt32
		}
		if !ok || typeID.Namespace() != 0 || typeID.IntID() != wantType {
			return nil, fmt.Errorf("wrong type: %s", name)
		}
		if r.Results[2].Value.Value() != wantedAccess {
			return nil, fmt.Errorf("wrong access: %s", name)
		}
		value := r.Results[3]
		if value == nil || value.Value == nil || value.Status != ua.StatusBadOutOfService {
			return nil, fmt.Errorf("disabled quality not preserved: %s: %+v", name, value)
		}
		if strings.HasSuffix(name, ".Fire") {
			description, ok := r.Results[4].Value.Value().(*ua.LocalizedText)
			if !ok || !strings.Contains(description.Text, "2") || !strings.Contains(description.Text, "红色报警") || !strings.Contains(description.Text, "闭锁") {
				return nil, fmt.Errorf("Fire enum description missing: %s", name)
			}
		}
		verified = append(verified, map[string]any{"name": name, "type_id": wantType, "access": wantedAccess, "status": uint32(value.Status)})
	}
	for name, want := range map[string]uint32{"InputRefreshIntervalMs": 5000, "InputTimeoutMs": 15000} {
		v, e := read(ua.NewStringNodeID(ns, name), ua.AttributeIDValue)
		if e != nil {
			return nil, e
		}
		if v.Status != ua.StatusOK || v.Value == nil || v.Value.Value() != want {
			return nil, fmt.Errorf("wrong interval: %s", name)
		}
	}
	// Legacy expanded names must no longer be published after the clean restart.
	for _, name := range []string{"rx9.DATA.Buildings.91UBH.Fire", "RcdSync.sync.TX.Buildings.11UBE.Fire", "RcdSync.sync.TX.Snapshot"} {
		v, e := read(ua.NewStringNodeID(ns, name), ua.AttributeIDValue)
		if e != nil {
			return nil, e
		}
		if v.Status != ua.StatusBadNodeIDUnknown {
			return nil, fmt.Errorf("old name still published: %s: %v", name, v.Status)
		}
	}
	node := ua.NewStringNodeID(ns, "TransmitSnapshot")
	access, err := read(node, ua.AttributeIDAccessLevel)
	if err != nil {
		return nil, err
	}
	if access.Status != ua.StatusOK || access.Value.Value() != byte(3) {
		return nil, fmt.Errorf("snapshot not writable")
	}
	value, err := read(node, ua.AttributeIDValue)
	if err != nil {
		return nil, err
	}
	state := map[string]any{}
	if value.Status != ua.StatusOK || value.Value == nil {
		return nil, fmt.Errorf("snapshot status unavailable")
	}
	if err = json.Unmarshal([]byte(value.Value.Value().(string)), &state); err != nil {
		return nil, err
	}
	if state["transmit_enabled"] != false || state["complete_writes"] != float64(0) || state["received_inputs"] != float64(0) {
		return nil, fmt.Errorf("unexpected enabled/filled mailbox: %v", state)
	}
	notifications := make(chan *opcua.PublishNotificationData, 8)
	sub, err := c.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: 250 * time.Millisecond}, notifications)
	if err != nil {
		return nil, err
	}
	defer sub.Cancel(context.Background())
	result, err := sub.Monitor(ctx, ua.TimestampsToReturnBoth, opcua.NewMonitoredItemCreateRequestWithDefaults(node, ua.AttributeIDValue, 1))
	if err != nil {
		return nil, err
	}
	if len(result.Results) != 1 || result.Results[0].StatusCode != ua.StatusOK {
		return nil, fmt.Errorf("monitor rejected")
	}
	for {
		select {
		case n := <-notifications:
			if n.Error != nil {
				return nil, n.Error
			}
			if d, ok := n.Value.(*ua.DataChangeNotification); ok {
				for _, item := range d.MonitoredItems {
					if item.ClientHandle == 1 {
						if item.Value.Status != ua.StatusOK {
							return nil, fmt.Errorf("snapshot notification bad quality")
						}
						return map[string]any{"endpoint": endpoint, "points": verified, "status": state, "subscription_status": uint32(item.Value.Status), "hardware_writes": 0}, nil
					}
				}
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
func main() {
	if len(os.Args) != 3 {
		panic("new evidence output path and original TXT fixture directory required")
	}
	var values []map[string]any
	for _, endpoint := range []string{"opc.tcp://192.168.10.46:4842/", "opc.tcp://192.168.10.46:4840"} {
		v, e := check(endpoint, os.Args[2])
		if e != nil {
			panic(e)
		}
		values = append(values, v)
	}
	f, e := os.OpenFile(os.Args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		panic(e)
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if e = encoder.Encode(values); e != nil {
		panic(e)
	}
	fmt.Println("atomic node Read/Subscribe verified on both endpoints; no writes")
}
