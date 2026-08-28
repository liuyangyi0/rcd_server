package opc

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

type sessionIsolationBackend struct {
	mu       sync.RWMutex
	order    []string
	sessions map[string]domain.Session
	groups   []domain.Group
	sources  []domain.Source
	commands []commandRecord
}

func newSessionIsolationBackend() *sessionIsolationBackend {
	return &sessionIsolationBackend{
		sessions: make(map[string]domain.Session),
		groups:   []domain.Group{{ID: "g", DefaultSourceID: "a", Revision: 1}},
		sources:  []domain.Source{{ID: "a", GroupID: "g", Enabled: true, CatalogState: domain.CatalogReady}},
	}
}

func (b *sessionIsolationBackend) OpenSession(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.sessions[id]; !exists {
		b.order = append(b.order, id)
		b.sessions[id] = domain.Session{ID: id, Routes: map[string]domain.GroupRoute{
			"g": {GroupID: "g", SourceID: "a", Revision: 1},
		}}
	}
	return nil
}
func (b *sessionIsolationBackend) CloseSession(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}
func (b *sessionIsolationBackend) TouchSession(string) error { return nil }
func (b *sessionIsolationBackend) ReadSample(id string, _ domain.NodeKey) domain.Sample {
	b.mu.RLock()
	defer b.mu.RUnlock()
	value := uint32(0)
	for index, candidate := range b.order {
		if candidate == id {
			value = uint32(index + 1)
			break
		}
	}
	return domain.Sample{Type: domain.DataTypeUInt32, Value: value, StatusCode: domain.StatusGood, SourceTimestamp: time.Now().UTC()}
}

func (b *sessionIsolationBackend) WriteCommand(_ context.Context, sid string, key domain.NodeKey, value any) (domain.StatusCode, error) {
	b.mu.Lock()
	b.commands = append(b.commands, commandRecord{sessionID: sid, key: key, value: value})
	b.mu.Unlock()
	return domain.StatusGood, nil
}
func (b *sessionIsolationBackend) CallCommand(_ context.Context, sid string, key domain.NodeKey, hexData string) (string, domain.StatusCode, error) {
	b.mu.Lock()
	b.commands = append(b.commands, commandRecord{sessionID: sid, key: key, hexData: hexData})
	b.mu.Unlock()
	return "queued", domain.StatusGood, nil
}
func (b *sessionIsolationBackend) SelectSource(string, string, string, uint64) (domain.GroupRoute, error) {
	return domain.GroupRoute{}, nil
}
func (b *sessionIsolationBackend) SetDefaultSource(string, string, uint64) (domain.Group, error) {
	return domain.Group{}, nil
}
func (b *sessionIsolationBackend) Session(id string) (domain.Session, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	session, ok := b.sessions[id]
	if !ok {
		return domain.Session{}, domain.ErrSessionClosed
	}
	return session, nil
}
func (b *sessionIsolationBackend) Groups() []domain.Group                 { return b.groups }
func (b *sessionIsolationBackend) Sources() []domain.Source               { return b.sources }
func (b *sessionIsolationBackend) LowerStatus(string) (LowerStatus, bool) { return LowerStatus{}, true }
func (b *sessionIsolationBackend) SourcePointStatistics(string) PointStatistics {
	return PointStatistics{}
}
func (b *sessionIsolationBackend) GroupForNode(string) (string, bool) { return "g", true }
func (b *sessionIsolationBackend) PendingCount(string) int            { return 0 }

func TestUpstreamServerReturnsDifferentValuesForSameNodeBySession(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	endpoint := fmt.Sprintf("opc.tcp://127.0.0.1:%d", port)
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	backend := newSessionIsolationBackend()
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: endpoint, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
		InsecureSkipVerify: true,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	catalog := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{
		"g": {
			"KKS": {
				Key:        domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "KKS"},
				BrowseName: "KKS", ParentPath: "Objects/Devices", Class: domain.NodeClassVariable,
				DataType: domain.DataTypeUInt32, ValueRank: -1,
			},
			"COM1_Send": {
				Key:        domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"},
				BrowseName: "COM1_Send", ParentPath: "Objects/Devices", Class: domain.NodeClassVariable,
				DataType: domain.DataTypeString, ValueRank: -1, Writable: true,
			},
			"COM1_SendCommand": {
				Key:        domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_SendCommand"},
				BrowseName: "COM1_SendCommand", ParentPath: "Objects/Methods", Class: domain.NodeClassMethod,
				ValueRank:       -1,
				InputArguments:  []domain.Argument{{Name: "HexData", DataType: domain.DataTypeString, ValueRank: -1}},
				OutputArguments: []domain.Argument{{Name: "Result", DataType: domain.DataTypeString, ValueRank: -1}},
			},
		},
	}}
	if err := server.ReconcileCatalog(catalog, backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServe() }()

	clientOne := connectAnonymousClient(t, endpoint)
	clientTwo := connectAnonymousClient(t, endpoint)
	namespaces := clientOne.Namespaces()
	if len(namespaces) < 4 || namespaces[2] != BusinessNamespaceURI || namespaces[3] != ManagementNamespaceURI {
		t.Fatalf("namespace order = %#v", namespaces)
	}
	deviceRefs, err := clientOne.Node(gu.NewStringNodeID(2, "Devices")).References(
		context.Background(), id.HasComponent, gu.BrowseDirectionForward, gu.NodeClassVariable, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceRefs) != 2 {
		t.Fatalf("Devices HasComponent refs = %#v", deviceRefs)
	}
	discovered, _, err := discoverCatalog(
		context.Background(), clientOne,
		domain.Source{ID: "loopback", GroupID: "g", Endpoint: endpoint}, namespaceIndex(namespaces),
	)
	if err != nil {
		t.Fatalf("loopback catalog discovery: %v", err)
	}
	if len(discovered.Nodes) != 3 {
		t.Fatalf("loopback catalog nodes = %#v", discovered.Nodes)
	}
	var discoveredMethod *domain.CatalogNode
	for i := range discovered.Nodes {
		if discovered.Nodes[i].Key.Identifier == "COM1_SendCommand" {
			discoveredMethod = &discovered.Nodes[i]
		}
	}
	if discoveredMethod == nil || len(discoveredMethod.InputArguments) != 1 || len(discoveredMethod.OutputArguments) != 1 {
		t.Fatalf("method signature = %#v", discoveredMethod)
	}
	read := func(client *gopcua.Client) uint32 {
		res, err := client.Read(context.Background(), &gu.ReadRequest{
			TimestampsToReturn: gu.TimestampsToReturnBoth,
			NodesToRead:        []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(2, "KKS"), AttributeID: gu.AttributeIDValue}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Results) != 1 || res.Results[0].Status != gu.StatusOK {
			t.Fatalf("read = %#v", res)
		}
		value, ok := res.Results[0].Value.Value().(uint32)
		if !ok {
			t.Fatalf("value = %T", res.Results[0].Value.Value())
		}
		return value
	}
	if left, right := read(clientOne), read(clientTwo); left != 1 || right != 2 {
		t.Fatalf("session-specific values = %d, %d", left, right)
	}
	writeValue := gu.MustVariant("AABB")
	writeResponse, err := clientOne.Write(context.Background(), &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{
		NodeID: gu.NewStringNodeID(2, "COM1_Send"), AttributeID: gu.AttributeIDValue,
		Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: writeValue},
	}}})
	if err != nil || len(writeResponse.Results) != 1 || writeResponse.Results[0] != gu.StatusOK {
		t.Fatalf("write response=%#v err=%v", writeResponse, err)
	}
	callArg := gu.MustVariant("0102")
	callResponse, err := clientOne.Call(context.Background(), &gu.CallMethodRequest{
		ObjectID: gu.NewStringNodeID(2, "Methods"), MethodID: gu.NewStringNodeID(2, "COM1_SendCommand"),
		InputArguments: []*gu.Variant{callArg},
	})
	if err != nil || callResponse.StatusCode != gu.StatusOK || len(callResponse.OutputArguments) != 1 || callResponse.OutputArguments[0].Value() != "queued" {
		t.Fatalf("call response=%#v err=%v", callResponse, err)
	}
	backend.mu.RLock()
	commandCount := len(backend.commands)
	backend.mu.RUnlock()
	if commandCount != 2 {
		t.Fatalf("proxied command count = %d", commandCount)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = clientOne.Close(closeCtx)
	_ = clientTwo.Close(closeCtx)
	cancel()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestUpstreamListenAddressIsIndependentFromAdvertisedEndpoint(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := reservation.Addr().String()
	_ = reservation.Close()
	advertisedEndpoint := "opc.tcp://manager.example:4840"
	actualEndpoint := "opc.tcp://" + listenAddress
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: advertisedEndpoint, ListenAddress: listenAddress,
		CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
		InsecureSkipVerify: true,
	}, newSessionIsolationBackend())
	if err != nil {
		t.Fatal(err)
	}
	if server.server.EndpointURL() != advertisedEndpoint || server.server.ListenAddress() != listenAddress {
		t.Fatalf("endpoint=%q listen=%q", server.server.EndpointURL(), server.server.ListenAddress())
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("server bound to %s did not stop", listenAddress)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var endpoints []*gu.EndpointDescription
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		endpoints, err = gopcua.GetEndpoints(ctx, actualEndpoint)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not bind %s independently of %s: %v", listenAddress, advertisedEndpoint, err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoint count = %d, want exact whitelist of 2", len(endpoints))
	}
}

func TestUpstreamSecureEndpointSupportsAnonymousAndUsername(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := reservation.Addr().String()
	_ = reservation.Close()
	endpoint := "opc.tcp://" + listenAddress
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: endpoint, ListenAddress: listenAddress,
		CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
		Username: "operator", Password: "secret", InsecureSkipVerify: true,
	}, newSessionIsolationBackend())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("secure server did not stop")
		}
	})

	anonymous := connectSecureClient(t, endpoint, pair, "", "")
	username := connectSecureClient(t, endpoint, pair, "operator", "secret")
	for label, client := range map[string]*gopcua.Client{"anonymous": anonymous, "username": username} {
		namespaces := client.Namespaces()
		if len(namespaces) != 4 || namespaces[2] != BusinessNamespaceURI || namespaces[3] != ManagementNamespaceURI {
			t.Fatalf("%s secure NamespaceArray = %#v", label, namespaces)
		}
	}
}

func connectAnonymousClient(t *testing.T, endpoint string) *gopcua.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var endpoints []*gu.EndpointDescription
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		endpoints, err = gopcua.GetEndpoints(ctx, endpoint)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	ep, err := gopcua.SelectEndpoint(endpoints, "None", gu.MessageSecurityModeNone)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gopcua.NewClient(endpoint,
		gopcua.SecurityFromEndpoint(ep, gu.UserTokenTypeAnonymous),
		gopcua.AuthAnonymous(), gopcua.AutoReconnect(false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	return client
}

func connectSecureClient(t *testing.T, endpoint string, pair pki.Pair, username, password string) *gopcua.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var endpoints []*gu.EndpointDescription
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		endpoints, err = gopcua.GetEndpoints(ctx, endpoint)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("get secure endpoints: %v", err)
	}
	authType := gu.UserTokenTypeAnonymous
	if username != "" {
		authType = gu.UserTokenTypeUserName
	}
	selected, err := gopcua.SelectEndpoint(endpoints, "Basic256Sha256", gu.MessageSecurityModeSignAndEncrypt)
	if err != nil {
		t.Fatal(err)
	}
	options := []gopcua.Option{
		gopcua.SecurityFromEndpoint(selected, authType),
		gopcua.CertificateFile(pair.CertificatePath),
		gopcua.PrivateKeyFile(pair.PrivateKeyPath),
		gopcua.ApplicationURI(pki.ApplicationURI),
		gopcua.AutoReconnect(false),
	}
	if username == "" {
		options = append(options, gopcua.AuthAnonymous())
	} else {
		options = append(options, gopcua.AuthUsername(username, password))
	}
	client, err := gopcua.NewClient(endpoint, options...)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("connect secure endpoint as %v: %v", authType, err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		_ = client.Close(closeCtx)
	})
	return client
}
