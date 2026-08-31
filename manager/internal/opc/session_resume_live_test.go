package opc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

// Opt-in, isolated deployment only. The only injected failure closes this
// test's private TCP forwarding pair; no defaults, fixture controls, other
// Sessions, lower connections, or server settings are modified.
func TestLiveDeviceGroupsSessionResume(t *testing.T) {
	endpoint, apiURL := os.Getenv("RCD_DEVICE_GROUPS_ENDPOINT"), strings.TrimRight(os.Getenv("RCD_DEVICE_GROUPS_API"), "/")
	if endpoint == "" || apiURL == "" {
		t.Skip("set RCD_DEVICE_GROUPS_ENDPOINT and RCD_DEVICE_GROUPS_API for the isolated two-Rust PTY deployment")
	}
	all, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, test := range []struct {
		name string
		run  func(*testing.T, context.Context, *resumeLiveFixture)
	}{{"NativeAutoReconnect", resumeLiveNative}, {"RawPendingPublish", resumeLiveRaw}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(all, 50*time.Second)
			defer cancel()
			fixture := newResumeLiveFixture(t, ctx, all, endpoint, apiURL)
			test.run(t, ctx, fixture)
		})
	}
}

type resumeLiveFixture struct {
	client   *gopcua.Client
	original *gopcua.Session
	proxy    *resumeLiveProxy
	apiURL   string
	name     string
	before   api.SessionView
}

func newResumeLiveFixture(t *testing.T, ctx, all context.Context, endpoint, apiURL string) *resumeLiveFixture {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "opc.tcp" || parsed.Host == "" {
		t.Fatalf("invalid manager endpoint %q: %v", endpoint, err)
	}
	endpoints, err := gopcua.GetEndpoints(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := gopcua.SelectEndpoint(endpoints, "None", gu.MessageSecurityModeNone)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := newResumeLiveProxy(all, parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	f := &resumeLiveFixture{proxy: proxy, apiURL: apiURL, name: "rcd-session-resume-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	// Each cleanup phase has an independent budget: a failed OPC close must
	// not turn REST/proxy checks into failures caused by an expired context.
	// Work is bounded by two minutes, followed by at most 17s of cleanup.
	// CloseSession also deletes raw subscriptions unknown to the publisher.
	t.Cleanup(func() {
		if f.client != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			if f.original != nil && f.client.State() != gopcua.Connected {
				ready, stop := context.WithTimeout(cleanup, 3*time.Second)
				if err := resumeLiveWait(ready, func() (bool, error) { return f.client.State() == gopcua.Connected, nil }); err != nil {
					t.Logf("cleanup could not await a connected channel: %v", err)
				}
				stop()
			}
			if f.original != nil && f.client.Session() != nil && f.client.Session() != f.original {
				_ = f.client.CloseSession(cleanup)
				_ = f.client.ActivateSession(cleanup, f.original)
			}
			if err := f.client.CloseSession(cleanup); err != nil {
				t.Errorf("CloseSession: %v", err)
			}
			cancel()
			cleanup, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			if err := f.client.Close(cleanup); err != nil {
				t.Errorf("Close client: %v", err)
			}
			cancel()
		}
		apiCleanup, stopAPI := context.WithTimeout(context.Background(), 4*time.Second)
		if err := resumeLiveWait(apiCleanup, func() (bool, error) {
			session, err := f.session(apiCleanup)
			return session == nil, err
		}); err != nil {
			t.Errorf("test Session remains in REST after CloseSession: %v", err)
		}
		stopAPI()
		proxyCleanup, stopProxy := context.WithTimeout(context.Background(), 3*time.Second)
		if err := proxy.close(proxyCleanup); err != nil {
			t.Errorf("proxy workers did not exit: %v", err)
		}
		stopProxy()
	})
	f.client, err = gopcua.NewClient("opc.tcp://"+proxy.listener.Addr().String()+parsed.EscapedPath(),
		gopcua.SecurityFromEndpoint(selected, gu.UserTokenTypeAnonymous), gopcua.AuthAnonymous(),
		gopcua.AutoReconnect(true), gopcua.ReconnectInterval(250*time.Millisecond),
		gopcua.SessionTimeout(time.Minute), gopcua.RequestTimeout(15*time.Second),
		gopcua.ApplicationName(f.name), gopcua.SessionName(f.name))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	f.original = f.client.Session()
	for group, source := range map[string]string{"device-a": "lower-b", "device-b": "lower-a"} {
		f.selectSource(t, ctx, group, source)
	}
	f.readPoints(t, ctx, 101)
	if err := resumeLiveWait(ctx, func() (bool, error) {
		session, err := f.session(ctx)
		if session != nil {
			f.before = *session
		}
		return session != nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if f.before.Routes["device-a"].SourceID != "lower-b" || f.before.Routes["device-b"].SourceID != "lower-a" {
		t.Fatalf("unexpected initial routes: %+v", f.before.Routes)
	}
	return f
}

func (f *resumeLiveFixture) session(ctx context.Context) (*api.SessionView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.apiURL+"/api/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Session list HTTP %d", response.StatusCode)
	}
	var page struct {
		Items []api.SessionView `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&page); err != nil {
		return nil, err
	}
	var found *api.SessionView
	for i := range page.Items {
		if page.Items[i].ClientName == f.name || (f.before.ID != "" && page.Items[i].ID == f.before.ID) {
			if found != nil {
				return nil, errors.New("test client has multiple Sessions after reconnect")
			}
			found = &page.Items[i]
		}
	}
	return found, nil
}

func (f *resumeLiveFixture) selectSource(t *testing.T, ctx context.Context, group, source string) {
	t.Helper()
	response, err := resumeLiveService[*gu.WriteResponse](ctx, f.client, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
		NodeID: gu.NewStringNodeID(3, "Groups/"+group+"/SelectedSourceId"), AttributeID: gu.AttributeIDValue,
		Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant(source)},
	}}})
	if err != nil || response == nil || len(response.Results) != 1 || !resumeLiveGood(response.Results[0]) {
		t.Fatalf("Session selection %s=%s: response=%+v err=%v", group, source, response, err)
	}
}

func (f *resumeLiveFixture) readPoints(t *testing.T, parent context.Context, wantA float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var last *gu.ReadResponse
	err := resumeLiveWait(ctx, func() (bool, error) {
		var err error
		// Client.Read fills mandatory ReadValueID.DataEncoding structures;
		// direct Send with nil encodings would emit a truncated Read frame.
		last, err = f.client.Read(ctx, &gu.ReadRequest{
			TimestampsToReturn: gu.TimestampsToReturnBoth, NodesToRead: []*gu.ReadValueID{
				{NodeID: gu.NewStringNodeID(2, "A_VALUE"), AttributeID: gu.AttributeIDValue},
				{NodeID: gu.NewStringNodeID(2, "B_VALUE"), AttributeID: gu.AttributeIDValue},
			}})
		return err == nil && last != nil && len(last.Results) == 2 && resumeLiveValue(last.Results[0], wantA) && resumeLiveValue(last.Results[1], 22), err
	})
	if err != nil {
		t.Fatalf("Read A_VALUE=%g/B_VALUE=22: last=%+v err=%v", wantA, last, err)
	}
}

func (f *resumeLiveFixture) cutAndResume(t *testing.T, parent context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	if err := resumeLiveWait(ctx, func() (bool, error) { return f.proxy.hasPending(), nil }); err != nil {
		t.Fatalf("no forwarded outstanding Publish before cut: %v", err)
	}
	// Read on the same channel is a service-processing barrier after the
	// observed Publish. Retain the original probe's bounded 80ms queue window;
	// a 10s keep-alive prevents a quiet Publish completing during this window.
	f.readPoints(t, ctx, 101)
	select {
	case <-time.After(80 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	connection, err := f.proxy.cutPending()
	if err != nil {
		t.Fatal(err)
	}
	err = resumeLiveWait(ctx, func() (bool, error) {
		session, err := f.session(ctx)
		if err != nil || session == nil {
			return false, err
		}
		if session.ID != f.before.ID || !reflect.DeepEqual(session.Routes, f.before.Routes) {
			return false, fmt.Errorf("reconnect replaced Session/routes: before=%+v after=%+v", f.before, session)
		}
		return f.proxy.connections() > connection && f.client.State() == gopcua.Connected && session.RemoteAddress != f.before.RemoteAddress, nil
	})
	if err != nil {
		t.Fatalf("original Session did not reattach: %v", err)
	}
	f.readPoints(t, ctx, 101)
	t.Logf("new TCP resumed original Session %s with all group routes/revisions intact", f.before.ID)
}

func (f *resumeLiveFixture) assertRoutes(t *testing.T, ctx context.Context, source string, switches uint64) {
	t.Helper()
	expected := f.before.Session
	expected.Routes = make(map[string]domain.GroupRoute, len(f.before.Routes))
	for group, route := range f.before.Routes {
		expected.Routes[group] = route
	}
	route := expected.Routes["device-a"]
	route.SourceID, route.Revision = source, route.Revision+switches
	expected.Routes["device-a"] = route
	session, err := f.session(ctx)
	if err != nil || session == nil || session.ID != f.before.ID || !reflect.DeepEqual(session.Routes, expected.Routes) {
		t.Fatalf("switch changed another group or Session: expected=%+v actual=%+v err=%v", expected, session, err)
	}
}

func resumeLiveNative(t *testing.T, ctx context.Context, f *resumeLiveFixture) {
	notifications := make(chan *gopcua.PublishNotificationData, 64)
	subscription, err := f.client.Subscribe(ctx, &gopcua.SubscriptionParameters{Interval: 250 * time.Millisecond, MaxKeepAliveCount: 40, LifetimeCount: 400}, notifications)
	if err != nil {
		t.Fatal(err)
	}
	originalID := subscription.SubscriptionID
	response, err := subscription.Monitor(ctx, gu.TimestampsToReturnBoth, resumeLiveMonitors()...)
	resumeLiveCheckMonitors(t, response, err)
	resumeLiveAwaitNative(t, ctx, notifications, originalID, 101, true)
	f.cutAndResume(t, ctx)
	for index, source := range []string{"lower-a", "lower-b"} {
		want := []float64{11, 101}[index]
		f.selectSource(t, ctx, "device-a", source)
		resumeLiveAwaitNative(t, ctx, notifications, originalID, want, false)
		f.readPoints(t, ctx, want)
		f.assertRoutes(t, ctx, source, uint64(index+1))
	}
	t.Logf("original subscription %d automatically delivered 101 -> 11 -> 101; device-b stayed 22", originalID)
}

func resumeLiveAwaitNative(t *testing.T, parent context.Context, notifications <-chan *gopcua.PublishNotificationData, subID uint32, wantA float64, needB bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	seenA, seenB := false, !needB
	for !seenA || !seenB {
		select {
		case <-ctx.Done():
			t.Fatalf("original subscription %d stalled; want A=%g B=22: %v", subID, wantA, ctx.Err())
		case notification, open := <-notifications:
			if !open {
				t.Fatal("original subscription notification channel closed")
			}
			if notification == nil {
				continue
			}
			if notification.Error != nil {
				t.Logf("transient Publish during reconnect: %v", notification.Error)
				continue
			}
			if notification.SubscriptionID != subID {
				t.Fatalf("original subscription %d was recreated as %d", subID, notification.SubscriptionID)
			}
			if change, ok := notification.Value.(*gu.DataChangeNotification); ok {
				for _, item := range change.MonitoredItems {
					if item == nil {
						continue
					}
					if item.ClientHandle == 1 && resumeLiveValue(item.Value, wantA) {
						seenA = true
					}
					if item.ClientHandle == 2 {
						if !resumeLiveValue(item.Value, 22) {
							t.Fatalf("other device changed during device-a selection: %+v", item.Value)
						}
						seenB = true
					}
				}
			}
		}
	}
}

func resumeLiveRaw(t *testing.T, ctx context.Context, f *resumeLiveFixture) {
	// Deliberately bypass Client.Subscribe: the raw server subscription is not
	// registered with gopcua's automatic Publish loop, even after its fix.
	created, err := resumeLiveService[*gu.CreateSubscriptionResponse](ctx, f.client, &gu.CreateSubscriptionRequest{
		RequestedPublishingInterval: 250, RequestedMaxKeepAliveCount: 40, RequestedLifetimeCount: 400, PublishingEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	subID := created.SubscriptionID
	response, err := resumeLiveService[*gu.CreateMonitoredItemsResponse](ctx, f.client, &gu.CreateMonitoredItemsRequest{
		SubscriptionID: subID, TimestampsToReturn: gu.TimestampsToReturnBoth, ItemsToCreate: resumeLiveMonitors()})
	resumeLiveCheckMonitors(t, response, err)
	seen := make(map[uint32]*gu.NotificationMessage)
	initial := resumeLiveAwaitRaw(t, ctx, f.client, subID, 101, true, nil, seen)
	oldDone := make(chan error, 1)
	go func() {
		_, err := resumeLiveService[*gu.PublishResponse](ctx, f.client, &gu.PublishRequest{
			SubscriptionAcknowledgements: []*gu.SubscriptionAcknowledgement{{SubscriptionID: subID, SequenceNumber: initial.SequenceNumber}},
		})
		oldDone <- err
	}()
	f.cutAndResume(t, ctx)
	select {
	case err := <-oldDone:
		if err == nil {
			t.Fatal("old Publish completed before the injected cut; queued-request case was not exercised")
		}
	case <-ctx.Done():
		t.Fatal("old-channel Publish did not terminate")
	}
	var previous *gu.NotificationMessage
	for index, source := range []string{"lower-a", "lower-b"} {
		want := []float64{11, 101}[index]
		f.selectSource(t, ctx, "device-a", source)
		var acks []*gu.SubscriptionAcknowledgement
		if previous != nil {
			acks = []*gu.SubscriptionAcknowledgement{{SubscriptionID: subID, SequenceNumber: previous.SequenceNumber}}
		}
		message := resumeLiveAwaitRaw(t, ctx, f.client, subID, want, false, acks, seen)
		republished, err := resumeLiveService[*gu.RepublishResponse](ctx, f.client, &gu.RepublishRequest{SubscriptionID: subID, RetransmitSequenceNumber: message.SequenceNumber})
		if err != nil || republished == nil || !reflect.DeepEqual(republished.NotificationMessage, message) {
			t.Fatalf("Republish %d did not preserve the original message: response=%+v err=%v", message.SequenceNumber, republished, err)
		}
		if previous != nil {
			_, err := resumeLiveService[*gu.RepublishResponse](ctx, f.client, &gu.RepublishRequest{SubscriptionID: subID, RetransmitSequenceNumber: previous.SequenceNumber})
			if !errors.Is(err, gu.StatusBadMessageNotAvailable) {
				t.Fatalf("ACKed sequence %d remained available: %v", previous.SequenceNumber, err)
			}
		}
		t.Logf("original raw subscription %d value=%g sequence=%d; Republish identical", subID, want, message.SequenceNumber)
		previous = message
		f.readPoints(t, ctx, want)
		f.assertRoutes(t, ctx, source, uint64(index+1))
	}
}

func resumeLiveAwaitRaw(t *testing.T, parent context.Context, client *gopcua.Client, subID uint32, want float64, needB bool, acks []*gu.SubscriptionAcknowledgement, seen map[uint32]*gu.NotificationMessage) *gu.NotificationMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 14*time.Second)
	defer cancel()
	seenA, seenB := false, !needB
	observe := func(message *gu.NotificationMessage, republished bool) bool {
		if message == nil || len(message.NotificationData) == 0 {
			return false // Keep-alives advertise the next sequence without consuming it.
		}
		if message.SequenceNumber == 0 {
			t.Fatal("data notification has sequence zero")
		}
		if old, exists := seen[message.SequenceNumber]; exists {
			if !reflect.DeepEqual(old, message) {
				t.Fatalf("sequence %d reused for different notifications: old=%+v new=%+v", message.SequenceNumber, old, message)
			}
		} else {
			// Republish can recover an older unseen message after a newer
			// Publish. Only freshly published data must advance the sequence.
			if !republished {
				for sequence := range seen {
					if sequence >= message.SequenceNumber {
						t.Fatalf("new notification sequence regressed: seen=%d current=%d", sequence, message.SequenceNumber)
					}
				}
			}
			seen[message.SequenceNumber] = message
		}
		for _, extension := range message.NotificationData {
			if extension == nil {
				continue
			}
			if change, ok := extension.Value.(*gu.DataChangeNotification); ok {
				for _, item := range change.MonitoredItems {
					if item != nil && item.ClientHandle == 1 && resumeLiveValue(item.Value, want) {
						seenA = true
					}
					if item != nil && item.ClientHandle == 2 {
						if !resumeLiveValue(item.Value, 22) {
							t.Fatalf("raw subscription changed device-b: %+v", item.Value)
						}
						seenB = true
					}
				}
			}
		}
		return seenA && seenB
	}
	for ctx.Err() == nil {
		response, err := resumeLiveService[*gu.PublishResponse](ctx, client, &gu.PublishRequest{SubscriptionAcknowledgements: acks})
		if err != nil || response == nil || response.SubscriptionID != subID || len(response.Results) != len(acks) {
			t.Fatalf("raw Publish sub=%d: response=%+v err=%v", subID, response, err)
		}
		for _, result := range response.Results {
			if !resumeLiveGood(result) {
				t.Fatalf("Publish acknowledgement failed: %v", result)
			}
		}
		acks = nil
		if observe(response.NotificationMessage, false) {
			return response.NotificationMessage
		}
		for _, sequence := range response.AvailableSequenceNumbers {
			if _, known := seen[sequence]; known {
				continue
			}
			republished, err := resumeLiveService[*gu.RepublishResponse](ctx, client, &gu.RepublishRequest{SubscriptionID: subID, RetransmitSequenceNumber: sequence})
			if err != nil || republished == nil || republished.NotificationMessage == nil || republished.NotificationMessage.SequenceNumber != sequence {
				t.Fatalf("available sequence %d could not be republished: response=%+v err=%v", sequence, republished, err)
			}
			if observe(republished.NotificationMessage, true) {
				return republished.NotificationMessage
			}
		}
	}
	t.Fatalf("raw original subscription did not deliver A_VALUE=%g: %v", want, ctx.Err())
	return nil
}

func resumeLiveMonitors() []*gu.MonitoredItemCreateRequest {
	items := make([]*gu.MonitoredItemCreateRequest, 0, 2)
	for i, name := range []string{"A_VALUE", "B_VALUE"} {
		item := gopcua.NewMonitoredItemCreateRequestWithDefaults(gu.NewStringNodeID(2, name), gu.AttributeIDValue, uint32(i+1))
		item.RequestedParameters.SamplingInterval, item.RequestedParameters.QueueSize = 250, 16
		items = append(items, item)
	}
	return items
}

func resumeLiveCheckMonitors(t *testing.T, response *gu.CreateMonitoredItemsResponse, err error) {
	t.Helper()
	if err != nil || response == nil || len(response.Results) != 2 {
		t.Fatalf("create monitored items: response=%+v err=%v", response, err)
	}
	for _, result := range response.Results {
		if result == nil || !resumeLiveGood(result.StatusCode) {
			t.Fatalf("bad monitored item: %+v", result)
		}
	}
}

func resumeLiveService[T gu.Response](ctx context.Context, client *gopcua.Client, request gu.Request) (T, error) {
	var result T
	err := client.Send(ctx, request, func(response gu.Response) error {
		var ok bool
		result, ok = response.(T)
		if !ok || response.Header() == nil {
			return fmt.Errorf("unexpected response %T to %T", response, request)
		}
		if !resumeLiveGood(response.Header().ServiceResult) {
			return response.Header().ServiceResult
		}
		return nil
	})
	return result, err
}

func resumeLiveGood(status gu.StatusCode) bool { return uint32(status)&0xc0000000 == 0 }

func resumeLiveValue(value *gu.DataValue, want float64) bool {
	if value == nil || value.Value == nil || !resumeLiveGood(value.Status) {
		return false
	}
	actual, err := strconv.ParseFloat(fmt.Sprint(value.Value.Value()), 64)
	return err == nil && actual == want
}

func resumeLiveWait(ctx context.Context, condition func() (bool, error)) error {
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		if done, err := condition(); done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type resumeLiveProxy struct {
	listener net.Listener
	mu       sync.Mutex
	pairs    map[*resumeLivePair]bool
	accepted int
	closed   bool
	wg       sync.WaitGroup
}

type resumeLivePair struct {
	client, server net.Conn
	proxy          *resumeLiveProxy
	sequence       int
	pending        map[uint32]bool // false until the whole Publish frame is forwarded
	responses      int
	once           sync.Once
}

func newResumeLiveProxy(ctx context.Context, target string) (*resumeLiveProxy, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &resumeLiveProxy{listener: listener, pairs: make(map[*resumeLivePair]bool)}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			pair := &resumeLivePair{client: client, server: server, proxy: p, pending: make(map[uint32]bool)}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				pair.close()
				return
			}
			p.accepted++
			pair.sequence = p.accepted
			p.pairs[pair] = true
			p.wg.Add(2)
			p.mu.Unlock()
			go pair.forward(server, client, true)
			go pair.forward(client, server, false)
		}
	}()
	return p, nil
}

func (pair *resumeLivePair) forward(destination, source net.Conn, requests bool) {
	defer pair.proxy.wg.Done()
	defer pair.close()
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(source, header); err != nil {
			return
		}
		size := binary.LittleEndian.Uint32(header[4:])
		if size < 8 || size > 4<<20 {
			return
		}
		frame := make([]byte, int(size))
		copy(frame, header)
		if _, err := io.ReadFull(source, frame[8:]); err != nil {
			return
		}
		var requestID uint32
		publish := false
		// This proxy is deliberately limited to the selected None endpoint.
		// MSG headers have channel/token/sequence/request IDs before byte 24.
		if string(frame[:4]) == "MSGF" && len(frame) >= 24 {
			requestID = binary.LittleEndian.Uint32(frame[20:24])
			typeID := new(gu.ExpandedNodeID)
			if requests {
				_, err := typeID.Decode(frame[24:])
				publish = err == nil && typeID.NodeID != nil && typeID.NodeID.IntID() == id.PublishRequest_Encoding_DefaultBinary
			}
			pair.proxy.mu.Lock()
			if publish {
				pair.pending[requestID] = false
			} else if !requests {
				if _, pending := pair.pending[requestID]; pending {
					delete(pair.pending, requestID)
					pair.responses++
				}
			}
			pair.proxy.mu.Unlock()
		}
		if _, err := io.Copy(destination, bytes.NewReader(frame)); err != nil {
			return
		}
		if publish {
			pair.proxy.mu.Lock()
			if _, pending := pair.pending[requestID]; pending {
				pair.pending[requestID] = true
			}
			pair.proxy.mu.Unlock()
		}
	}
}

func (pair *resumeLivePair) close() {
	pair.once.Do(func() {
		_ = pair.client.Close()
		_ = pair.server.Close()
		pair.proxy.mu.Lock()
		delete(pair.proxy.pairs, pair)
		pair.proxy.mu.Unlock()
	})
}

func (p *resumeLiveProxy) hasPending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for pair := range p.pairs {
		for _, forwarded := range pair.pending {
			if forwarded && pair.responses > 0 {
				return true
			}
		}
	}
	return false
}

func (p *resumeLiveProxy) cutPending() (int, error) {
	p.mu.Lock()
	var selected *resumeLivePair
	if len(p.pairs) == 1 {
		for pair := range p.pairs {
			for _, forwarded := range pair.pending {
				if forwarded && pair.responses > 0 {
					selected = pair
				}
			}
		}
	}
	p.mu.Unlock()
	if selected == nil {
		return 0, errors.New("refusing cut: require one private connection with a forwarded, unanswered Publish")
	}
	selected.close()
	return selected.sequence, nil
}

func (p *resumeLiveProxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

func (p *resumeLiveProxy) close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	pairs := make([]*resumeLivePair, 0, len(p.pairs))
	for pair := range p.pairs {
		pairs = append(pairs, pair)
	}
	p.mu.Unlock()
	_ = p.listener.Close()
	for _, pair := range pairs {
		pair.close()
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
