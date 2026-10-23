package app

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
	adapter "rcd_manager/internal/opc"
	"rcd_manager/internal/platform/pki"
)

// Real OPC client/server on an ephemeral loopback endpoint, deterministic
// station snapshots only. Never opens a serial port or uses a field endpoint.
func TestStateOutputsOverOPCWithoutForwardingRules(t *testing.T) {
	r := stateRuntime(t)
	const name = "9CYE91GH201_COM1"
	content := strings.ReplaceAll(strings.Split(stateConfig, "[[rules]]")[0], "FIXED", name)
	activateTestCalculation(t, r, content)
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	endpoint := "opc.tcp://" + address
	server, err := adapter.NewUpstreamServer(adapter.UpstreamConfig{EndpointURL: endpoint, ListenAddress: address,
		CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath}, OPCBackend{Runtime: r})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.SetCatalogHandlers(server.ValidateCatalog, server.ReconcileCatalog); err != nil {
		t.Fatal(err)
	}
	r.SetCalculationRefreshHandler(func(names []string) { server.ForceCalculationRefresh(names) })
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var endpoints []*ua.EndpointDescription
	for i := 0; i < 50; i++ {
		endpoints, err = opcua.GetEndpoints(ctx, endpoint)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	selected, err := opcua.SelectEndpoint(endpoints, ua.SecurityPolicyURINone, ua.MessageSecurityModeNone)
	if err != nil {
		t.Fatal(err)
	}
	id := ua.NewStringNodeID(2, name)
	clients := make([]*opcua.Client, 2)
	channels := make([]chan *opcua.PublishNotificationData, 2)
	for i := range clients {
		client, err := opcua.NewClient(endpoint, opcua.SecurityFromEndpoint(selected, ua.UserTokenTypeAnonymous), opcua.AutoReconnect(false))
		if err != nil {
			t.Fatal(err)
		}
		if err = client.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		defer client.Close(context.Background())
		clients[i] = client
		channels[i] = make(chan *opcua.PublishNotificationData, 32)
		sub, err := client.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: 50 * time.Millisecond}, channels[i])
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Cancel(context.Background())
		item := opcua.NewMonitoredItemCreateRequestWithDefaults(id, ua.AttributeIDValue, 1)
		item.RequestedParameters.SamplingInterval = 1000
		result, err := sub.Monitor(ctx, ua.TimestampsToReturnBoth, item)
		if err != nil || result.Results[0].StatusCode != ua.StatusOK {
			t.Fatalf("monitor: %v %v", result, err)
		}
	}
	wait := func(want uint32, status ua.StatusCode) {
		t.Helper()
		for i, ch := range channels {
			deadline := time.NewTimer(3 * time.Second)
			found := false
			for !found {
				select {
				case msg := <-ch:
					if msg.Error != nil {
						t.Fatal(msg.Error)
					}
					if change, ok := msg.Value.(*ua.DataChangeNotification); ok {
						for _, item := range change.MonitoredItems {
							if item.Value.Value != nil && item.Value.Value.Value() == want && item.Value.Status == status {
								found = true
							}
						}
					}
				case <-deadline.C:
					t.Fatalf("session %d missed state %d / %v", i, want, status)
				}
			}
			deadline.Stop()
		}
	}
	r.runCalculationBatch()
	wait(1, ua.StatusOK)
	attrs, err := clients[0].Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{
		{NodeID: id, AttributeID: ua.AttributeIDDataType}, {NodeID: id, AttributeID: ua.AttributeIDAccessLevel}, {NodeID: id, AttributeID: ua.AttributeIDBrowseName},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(attrs.Results[0].Value.Value()) != "i=7" || attrs.Results[1].Value.Value() != uint8(1) || attrs.Results[2].Value.Value().(*ua.QualifiedName).Name != name {
		t.Fatalf("wrong UInt32/read-only/BrowseName: %#v", attrs.Results)
	}
	write, err := clients[0].Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: id, AttributeID: ua.AttributeIDValue,
		Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(uint32(3))}}}})
	if err != nil || write.Results[0] != ua.StatusBadNotWritable {
		t.Fatalf("state output accepted a write: %v %v", write, err)
	}
	r.OnLowerStatus(adapter.LowerStatus{SourceID: "source-a", Connection: adapter.ConnectionDisconnected, Session: adapter.SessionInactive, Epoch: 1, Catalog: domain.CatalogReady})
	r.runCalculationBatch()
	wait(5, ua.StatusOK)
	for _, client := range clients {
		read, err := client.Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{{NodeID: id, AttributeID: ua.AttributeIDValue}}, TimestampsToReturn: ua.TimestampsToReturnBoth})
		if err != nil || read.Results[0].Value.Value() != uint32(5) || read.Results[0].Status != ua.StatusOK {
			t.Fatalf("offline state: %v %v", read, err)
		}
	}
	activateTestCalculation(t, r, "")
	wait(0, ua.StatusBadOutOfService)
	// Re-adding the same binding revives the retained node without a restart.
	activateTestCalculation(t, r, content)
	r.runCalculationBatch()
	wait(5, ua.StatusOK)
}
