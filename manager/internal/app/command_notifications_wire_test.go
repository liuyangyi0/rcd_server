package app

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
	adapter "rcd_manager/internal/opc"
	"rcd_manager/internal/platform/pki"
)

// Real upstream OPC stack and client, deterministic lower transport. Only an
// ephemeral loopback endpoint is used; no environment/field endpoints accepted.
func TestConsecutiveCommandResultsOverOPC(t *testing.T) {
	r, lowers := deviceRuntime(t)
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
	server, err := adapter.NewUpstreamServer(adapter.UpstreamConfig{EndpointURL: endpoint, ListenAddress: address, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath}, OPCBackend{Runtime: r})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.ReconcileCatalog(r.CatalogUnion(), r.routes.Groups(), r.routes.Sources()); err != nil {
		t.Fatal(err)
	}
	r.SetCommandResultHandler(server.NotifyCommandResult)
	stopped := make(chan error, 1)
	go func() { stopped <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-stopped })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var endpoints []*ua.EndpointDescription
	for i := 0; i < 50; i++ {
		endpoints, err = opcua.GetEndpoints(ctx, endpoint)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	selected, err := opcua.SelectEndpoint(endpoints, ua.SecurityPolicyURINone, ua.MessageSecurityModeNone)
	if err != nil {
		t.Fatal(err)
	}
	client, err := opcua.NewClient(endpoint, opcua.SecurityFromEndpoint(selected, ua.UserTokenTypeAnonymous), opcua.AutoReconnect(false))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	notifications := make(chan *opcua.PublishNotificationData, 100)
	sub, err := client.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: 100 * time.Millisecond}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel(context.Background())
	ids := []*ua.NodeID{ua.NewStringNodeID(2, "ttya_Send"), ua.NewStringNodeID(3, "Groups/device-one/Send")}
	items := make([]*ua.MonitoredItemCreateRequest, 2)
	for i, id := range ids {
		items[i] = opcua.NewMonitoredItemCreateRequestWithDefaults(id, ua.AttributeIDValue, uint32(i+1))
		items[i].RequestedParameters.SamplingInterval = 1000
		items[i].RequestedParameters.QueueSize = 16
	}
	response, err := sub.Monitor(ctx, ua.TimestampsToReturnBoth, items...)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.StatusCode != ua.StatusOK {
			t.Fatal(result.StatusCode)
		}
	}
	write := func(id *ua.NodeID) {
		t.Helper()
		result, err := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: id, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant("01 06 00 01")}}}})
		if err != nil || result.Results[0] != ua.StatusOK {
			t.Fatalf("write=%v %v", result, err)
		}
	}
	for _, ack := range []string{"1", "3", "timeout"} {
		t.Run(ack, func(t *testing.T) {
			lowers["a"].mu.Lock()
			lowers["a"].writeHook = func() {
				if ack != "timeout" {
					r.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: ack, StatusCode: domain.StatusGood, Epoch: 1})
				}
			}
			lowers["a"].mu.Unlock()
			for i := 0; i < 3; i++ {
				write(ids[0])
				if ack == "timeout" {
					r.sweepExpiredCommands(time.Now().Add(r.options.CommandPendingTTL + time.Second))
				}
			}
			want, status := ack, ua.StatusOK
			if ack == "3" {
				status = ua.StatusBadUnexpectedError
			}
			if ack == "timeout" {
				want, status = "3", ua.StatusBadTimeout
			}
			counts := map[uint32]int{}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for counts[1] < 3 || counts[2] < 3 {
				select {
				case msg := <-notifications:
					if msg.Error != nil {
						t.Fatal(msg.Error)
					}
					if change, ok := msg.Value.(*ua.DataChangeNotification); ok {
						for _, item := range change.MonitoredItems {
							if item.Value.Value != nil && fmt.Sprint(item.Value.Value.Value()) == want && item.Value.Status == status {
								counts[item.ClientHandle]++
							}
						}
					}
				case <-deadline.C:
					t.Fatalf("lost consecutive results: %v", counts)
				}
			}
			result, err := client.Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{{NodeID: ua.NewStringNodeID(3, "Groups/device-one/CommandResult"), AttributeID: ua.AttributeIDValue}}})
			if err != nil || result.Results[0].Status != ua.StatusOK {
				t.Fatalf("diagnostic=%v %v", result, err)
			}
		})
	}
	t.Run("queued-burst-before-first-ack", func(t *testing.T) {
		lowers["a"].mu.Lock()
		lowers["a"].writeHook = nil
		lowers["a"].mu.Unlock()
		lowers["a"].recordMu.Lock()
		before := lowers["a"].writes
		lowers["a"].recordMu.Unlock()
		for i := 0; i < 3; i++ {
			write(ids[0])
		}
		readCount := func(name string) uint32 {
			t.Helper()
			result, err := client.Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{{NodeID: ua.NewStringNodeID(3, "Groups/device-one/"+name), AttributeID: ua.AttributeIDValue}}})
			if err != nil || len(result.Results) != 1 || result.Results[0].Status != ua.StatusOK {
				t.Fatalf("read %s: %v %v", name, result, err)
			}
			return result.Results[0].Value.Value().(uint32)
		}
		if readCount("QueuedCommandCount") != 2 || readCount("ExecutingCommandCount") != 1 || readCount("CommandQueueCapacityPerPort") != 64 {
			t.Fatal("wrong OPC queue counts")
		}
		for i := 1; i <= 3; i++ {
			want := before + i
			awaitQueueCondition(t, func() bool {
				lowers["a"].recordMu.Lock()
				defer lowers["a"].recordMu.Unlock()
				return lowers["a"].writes == want
			})
			r.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: "1", StatusCode: domain.StatusGood, Epoch: 1})
		}
		awaitQueueCondition(t, func() bool { return r.routes.TotalPending() == 0 })
		r.wg.Wait()
		if readCount("QueuedCommandCount") != 0 || readCount("ExecutingCommandCount") != 0 {
			t.Fatal("queue not drained")
		}
	})
}
