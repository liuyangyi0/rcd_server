package opc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

// This opt-in test owns its Rust process, configuration, PKI and random OPC
// port. It deliberately accepts no endpoint override and creates no serial
// device. All Calls are read-only Operations actions; it never submits a
// control command or changes a running installation. Execute only on the
// remote isolated Linux host, with an explicitly built Rust binary.
func TestTransportSequenceRealRustInterop(t *testing.T) {
	binary := os.Getenv("RCD_SEQUENCE_INTEROP_BINARY")
	if binary == "" {
		t.Skip("set RCD_SEQUENCE_INTEROP_BINARY to an isolated Rust binary")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolated Linux acceptance only")
	}
	endpoint, pair := startSequenceInteropRust(t, binary)
	for _, secure := range []bool{false, true} {
		name := "None"
		if secure {
			name = "Basic256Sha256"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("CanceledBeforeSend", func(t *testing.T) {
				client := connectSequenceInterop(t, endpoint, pair, secure)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				for attempt := 0; attempt < 8; attempt++ {
					canceled, stop := context.WithCancel(ctx)
					stop()
					_, err := client.Read(canceled, sequenceInteropReadRequest())
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled read %d: %v", attempt, err)
					}
					// A request canceled before any bytes are sent must not consume
					// an on-wire sequence number. The unchanged Rust receiver checks it.
					if err := sequenceInteropRead(ctx, client); err != nil {
						t.Fatalf("valid read after canceled request %d: %v", attempt, err)
					}
				}
				t.Log("8 canceled requests followed by 8 successful reads; same connection, no reconnect")
			})
			t.Run("ConcurrentReadCallSubscribeRenew", func(t *testing.T) {
				client := connectSequenceInterop(t, endpoint, pair, secure)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				ns, err := client.FindNamespace(ctx, domain.OperationsNamespaceURI)
				if err != nil {
					t.Fatal(err)
				}
				notifications := make(chan *gopcua.PublishNotificationData, 256)
				sub, err := client.Subscribe(ctx, &gopcua.SubscriptionParameters{
					Interval: 50 * time.Millisecond, MaxKeepAliveCount: 20, LifetimeCount: 1000,
				}, notifications)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer closeCancel()
					_ = sub.Cancel(closeCtx)
				})
				monitored, err := sub.Monitor(ctx, gu.TimestampsToReturnBoth,
					gopcua.NewMonitoredItemCreateRequestWithDefaults(gu.NewNumericNodeID(0, id.Server_ServerStatus_CurrentTime), gu.AttributeIDValue, 1))
				if err != nil || monitored == nil || len(monitored.Results) != 1 || monitored.Results[0] == nil || monitored.Results[0].StatusCode != gu.StatusOK {
					t.Fatalf("monitor current time: response=%+v error=%v", monitored, err)
				}
				var received, reads, calls, renews atomic.Int64
				errorCh := make(chan error, 32)
				workersCtx, stopWorkers := context.WithCancel(ctx)
				defer stopWorkers()
				var notificationWG sync.WaitGroup
				notificationWG.Add(1)
				go func() {
					defer notificationWG.Done()
					for {
						select {
						case <-workersCtx.Done():
							return
						case event := <-notifications:
							if event == nil {
								errorCh <- errors.New("nil subscription event")
								return
							}
							if event.Error != nil {
								errorCh <- fmt.Errorf("subscription: %w", event.Error)
								return
							}
							if change, ok := event.Value.(*gu.DataChangeNotification); ok {
								for _, item := range change.MonitoredItems {
									if item == nil || item.ClientHandle != 1 || item.Value == nil || uint32(item.Value.Status)&0xc0000000 != 0 || item.Value.Value == nil {
										errorCh <- fmt.Errorf("invalid current-time notification: %+v", item)
										return
									}
									received.Add(1)
								}
							}
						}
					}
				}()
				var wg sync.WaitGroup
				const workerCount, iterations = 6, 150
				for worker := 0; worker < workerCount; worker++ {
					wg.Add(1)
					go func(worker int) {
						defer wg.Done()
						ticker := time.NewTicker(20 * time.Millisecond)
						defer ticker.Stop()
						for n := 0; n < iterations; n++ {
							select {
							case <-ctx.Done():
								errorCh <- ctx.Err()
								return
							case <-ticker.C:
							}
							if worker%2 == 0 {
								if err := sequenceInteropRead(ctx, client); err != nil {
									errorCh <- fmt.Errorf("read worker %d iteration %d: %w", worker, n, err)
									return
								}
								reads.Add(1)
							} else {
								if err := sequenceInteropReadOnlyCall(ctx, client, ns, n%2 == 0); err != nil {
									errorCh <- fmt.Errorf("call worker %d iteration %d: %w", worker, n, err)
									return
								}
								calls.Add(1)
							}
						}
					}(worker)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					ticker := time.NewTicker(1100 * time.Millisecond)
					defer ticker.Stop()
					for n := 0; n < 3; n++ {
						select {
						case <-ctx.Done():
							errorCh <- ctx.Err()
							return
						case <-ticker.C:
						}
						// Exercise actual OpenSecureChannel renewals, not merely a
						// sleep that assumes the token was renewed successfully.
						if err := client.SecureChannel().Renew(ctx); err != nil {
							errorCh <- fmt.Errorf("renew %d: %w", n, err)
							return
						}
						renews.Add(1)
					}
				}()
				wg.Wait()
				stopWorkers()
				notificationWG.Wait()
				close(errorCh)
				for err := range errorCh {
					t.Error(err)
				}
				if reads.Load() != 450 || calls.Load() != 450 || renews.Load() != 3 || received.Load() < 2 {
					t.Fatalf("incomplete real traffic: reads=%d/450 calls=%d/450 renews=%d/3 notifications=%d (want >=2)", reads.Load(), calls.Load(), renews.Load(), received.Load())
				}
				if err := sequenceInteropRead(ctx, client); err != nil {
					t.Fatalf("final read on original connection: %v", err)
				}
				t.Logf("real Rust: reads=%d calls=%d renews=%d notifications=%d, no hardware commands or reconnect", reads.Load(), calls.Load(), renews.Load(), received.Load())
			})
		})
	}
}

func sequenceInteropReadRequest() *gu.ReadRequest {
	return &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewNumericNodeID(0, id.Server_NamespaceArray), AttributeID: gu.AttributeIDValue}}}
}

func sequenceInteropRead(ctx context.Context, client *gopcua.Client) error {
	response, err := client.Read(ctx, sequenceInteropReadRequest())
	if err != nil {
		return err
	}
	if response == nil || len(response.Results) != 1 || response.Results[0] == nil || response.Results[0].Status != gu.StatusOK || response.Results[0].Value == nil {
		return fmt.Errorf("invalid NamespaceArray response: %+v", response)
	}
	return nil
}

func sequenceInteropReadOnlyCall(ctx context.Context, client *gopcua.Client, ns uint16, events bool) error {
	request := map[string]any{"action": "command_tracking_status"}
	if events {
		request = map[string]any{"action": "list_command_events", "manager_instance_id": "sequence-interop", "after": "0", "limit": 10}
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	response, err := client.Call(ctx, &gu.CallMethodRequest{
		ObjectID: gu.NewStringNodeID(ns, "Operations"), MethodID: gu.NewStringNodeID(ns, "Operations/Execute"),
		InputArguments: []*gu.Variant{gu.MustVariant(string(raw))},
	})
	if err != nil {
		return err
	}
	if response == nil || response.StatusCode != gu.StatusOK || len(response.OutputArguments) != 1 || response.OutputArguments[0] == nil {
		return fmt.Errorf("invalid Operations response: %+v", response)
	}
	payload, ok := response.OutputArguments[0].Value().(string)
	if !ok {
		return errors.New("Operations response is not a String")
	}
	_, err = decodeOperationEnvelope(payload)
	return err
}

func connectSequenceInterop(t *testing.T, endpoint string, pair pki.Pair, secure bool) *gopcua.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoints, err := gopcua.GetEndpoints(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	policy, mode := "None", gu.MessageSecurityModeNone
	if secure {
		policy, mode = "Basic256Sha256", gu.MessageSecurityModeSignAndEncrypt
	}
	selected, err := gopcua.SelectEndpoint(endpoints, policy, mode)
	if err != nil {
		t.Fatal(err)
	}
	opts := []gopcua.Option{gopcua.SecurityFromEndpoint(selected, gu.UserTokenTypeAnonymous), gopcua.AuthAnonymous(),
		gopcua.AutoReconnect(false), gopcua.RequestTimeout(5 * time.Second), gopcua.Lifetime(2 * time.Second),
		gopcua.SessionTimeout(time.Minute), gopcua.ApplicationURI(pki.ApplicationURI)}
	if secure {
		opts = append(opts, gopcua.CertificateFile(pair.CertificatePath), gopcua.PrivateKeyFile(pair.PrivateKeyPath))
	}
	client, err := gopcua.NewClient(endpoint, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = client.Close(closeCtx)
	})
	return client
}

func startSequenceInteropRust(t *testing.T, binary string) (string, pki.Pair) {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "rcd_server")
	input, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(executable, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal(copyErr, closeErr)
	}
	configDir := filepath.Join(dir, "config")
	pkiDir := filepath.Join(dir, "pki")
	logDir := filepath.Join(dir, "logs")
	// The lower intentionally requires explicitly selected runtime locations
	// to exist before startup. State shares this fixture's config directory,
	// avoiding both implicit defaults and a false state-migration requirement.
	for _, path := range []string{configDir, pkiDir, logDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", `[server]
primary_ip = "127.0.0.1:11001"
secondary_ip = "127.0.0.1:11002"
run_on = "primary"
current_run = "primary"
[serial]
baud_rate = 28800
data_bits = "Eight"
parity = "None"
stop_bits = "One"
flow_control = "none"
[settings]
timeout = 50
poll_interval_ms = 5
command_retry_count = 3
parity_switch_delay_ms = 5
transmit_settle_guard_ms = 5
[calculation]
mode = "manager"
`)
	portID := filepath.Join(dir, "NONEXISTENT-SERIAL-PORT")
	write("rcd_sequence.csv", fmt.Sprintf("com,%s,,,,,,,\nbaud_rate,28800,,,,,,,\nis_special,false,,,,,,,\ncom_index,1,,,,,,,\ndevice_id,1,,,,,,,\ndata_len,1,,,,,,,\nkks_prefix,SEQUENCE_,,,,,,,\nkks,type_,f_type,byte_index,bit_index,def,max,min,lh\nStatus,uint,status,1,0.7,0,255,0,0\n", portID))
	state, err := json.Marshal(map[string]any{"schema_version": 1, "revision": "1", "ports": map[string]any{portID: map[string]any{"enabled": false, "device_read_enabled": map[string]bool{"1": false}}}})
	if err != nil {
		t.Fatal(err)
	}
	write("operations.json", string(state))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	endpoint := fmt.Sprintf("opc.tcp://127.0.0.1:%d/", port)
	logPath := filepath.Join(dir, "lower.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "--config-dir", configDir, "--state-dir", configDir, "--pki-dir", pkiDir, "--log-dir", logDir)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, logFile, logFile
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "RCD_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("RCD_OPCUA_PORT=%d", port))
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill() // only the test-owned isolated child
			<-done
		}
		_ = logFile.Close()
		if t.Failed() {
			content, _ := os.ReadFile(logPath)
			if len(content) > 16000 {
				content = content[len(content)-16000:]
			}
			t.Logf("isolated Rust log:\n%s", content)
		}
		if _, err := os.Stat(portID); !os.IsNotExist(err) {
			t.Errorf("isolated serial path unexpectedly exists: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var lastErr error
	for ctx.Err() == nil {
		probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
		_, lastErr = gopcua.GetEndpoints(probeCtx, endpoint)
		probeCancel()
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("isolated Rust did not become ready: %v", lastErr)
	}
	pair, err := pki.Ensure(filepath.Join(dir, "client-pki"), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, pair
}
