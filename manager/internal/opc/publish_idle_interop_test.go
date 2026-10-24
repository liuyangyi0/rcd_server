package opc

import (
	"context"
	"os"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/stats"
	gu "github.com/gopcua/opcua/ua"
)

// Owns an isolated real Rust server with a disabled nonexistent serial port.
// Never connects to an installed field endpoint or sends a hardware command.
func TestIdlePublishRealRustInterop(t *testing.T) {
	binary := os.Getenv("RCD_SEQUENCE_INTEROP_BINARY")
	if binary == "" {
		t.Skip("set RCD_SEQUENCE_INTEROP_BINARY to an isolated Rust binary")
	}
	endpoint, pair := startSequenceInteropRust(t, binary)
	for _, secure := range []bool{false, true} {
		name := "None"
		if secure {
			name = "Basic256Sha256"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() { t.Logf("protocol error counters: %s", stats.Error().String()) }()
			client := connectSequenceInterop(t, endpoint, pair, secure)
			ctx, cancel := context.WithTimeout(context.Background(), 370*time.Second)
			defer cancel()
			session, channel := client.Session(), client.SecureChannel()
			notifications := make(chan *gopcua.PublishNotificationData, 32)
			sub, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{
				Interval: 50 * time.Millisecond, MaxKeepAliveCount: 3000, LifetimeCount: 10000,
			}, notifications)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("revised interval=%v keepalive=%d lifetime=%d", sub.RevisedPublishingInterval, sub.RevisedMaxKeepAliveCount, sub.RevisedLifetimeCount)
			// A constant NamespaceArray avoids data changes concealing an idle Publish.
			res, err := sub.Monitor(ctx, gu.TimestampsToReturnBoth, gopcua.NewMonitoredItemCreateRequestWithDefaults(gu.NewNumericNodeID(0, 2255), gu.AttributeIDValue, 1))
			if err != nil || res == nil || len(res.Results) != 1 || res.Results[0].StatusCode != gu.StatusOK {
				t.Fatalf("monitor: %+v %v", res, err)
			}
			t.Cleanup(func() {
				stop, c := context.WithTimeout(context.Background(), 3*time.Second)
				defer c()
				_ = sub.Cancel(stop)
			})
			started := time.Now()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			initial := false
			for time.Since(started) < 330*time.Second {
				select {
				case n := <-notifications:
					if n == nil || n.Error != nil {
						t.Fatalf("notification after %v: %+v", time.Since(started), n)
					}
					initial = true
				case <-ticker.C:
					if client.State() != gopcua.Connected || client.Session() != session || client.SecureChannel() != channel {
						t.Fatalf("original connection lost after %v: state=%v", time.Since(started), client.State())
					}
					if int(time.Since(started).Seconds())%20 == 0 {
						readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
						err := sequenceInteropRead(readCtx, client)
						stop()
						if err != nil {
							t.Fatalf("read during idle Publish after %v: %v", time.Since(started), err)
						}
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if !initial {
				t.Fatal("no initial notification")
			}
			if err := sequenceInteropRead(ctx, client); err != nil {
				t.Fatal(err)
			}
			t.Log("330s idle subscription survived on original Session/channel with no reconnect")
		})
	}
}
