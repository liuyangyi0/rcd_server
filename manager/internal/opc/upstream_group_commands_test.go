package opc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

type groupCommandTestBackend struct {
	*sessionIsolationBackend
	commandMu sync.Mutex
	values    map[string]string
}

func (b *groupCommandTestBackend) ReadGroupCommand(sessionID, groupID string) domain.Sample {
	b.commandMu.Lock()
	defer b.commandMu.Unlock()
	value, exists := b.values[sessionID+"\x00"+groupID]
	if !exists {
		value = "IDLE"
	}
	return domain.Sample{Type: domain.DataTypeString, Value: value, StatusCode: domain.StatusGood}
}

func (b *groupCommandTestBackend) WriteGroupCommand(_ context.Context, sessionID, groupID string, value any) (domain.StatusCode, error) {
	b.commandMu.Lock()
	defer b.commandMu.Unlock()
	b.values[sessionID+"\x00"+groupID] = value.(string)
	return domain.StatusGood, nil
}

func (b *groupCommandTestBackend) CallGroupCommand(ctx context.Context, sessionID, groupID, value string) (string, domain.StatusCode, error) {
	status, err := b.WriteGroupCommand(ctx, sessionID, groupID, value)
	return "queued:" + groupID, status, err
}

func TestUpstreamGroupCommandsAreScopedBySessionAndLogicalGroup(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "opc.tcp://" + reservation.Addr().String()
	_ = reservation.Close()
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	backend := &groupCommandTestBackend{sessionIsolationBackend: newSessionIsolationBackend(), values: map[string]string{}}
	backend.groups = []domain.Group{{ID: "g"}, {ID: "device-a"}, {ID: "device-b"}}
	backend.sources[0].GroupBindings = []domain.GroupBinding{
		{GroupID: "device-a", PortID: "COM1", DeviceID: "1"},
		{GroupID: "device-b", PortID: "COM1", DeviceID: "2"},
	}
	server, err := NewUpstreamServer(UpstreamConfig{EndpointURL: endpoint, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath}, backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ReconcileCatalog(domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{}}, backend.groups, backend.sources); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("group command server failed to stop")
		}
	})
	first := connectAnonymousClient(t, endpoint)
	second := connectAnonymousClient(t, endpoint)
	t.Cleanup(func() { _ = first.Close(context.Background()); _ = second.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	read := func(client *gopcua.Client, group string) string {
		t.Helper()
		response, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(3, "Groups/"+group+"/Send"), AttributeID: gu.AttributeIDValue}}})
		if err != nil || response == nil || len(response.Results) != 1 || response.Results[0].Status != gu.StatusOK {
			t.Fatalf("read group=%s response=%#v err=%v", group, response, err)
		}
		return response.Results[0].Value.Value().(string)
	}
	response, err := first.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{NodeID: gu.NewStringNodeID(3, "Groups/device-a/Send"), AttributeID: gu.AttributeIDValue, Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant("01AA")}}}})
	if err != nil || response == nil || len(response.Results) != 1 || response.Results[0] != gu.StatusOK {
		t.Fatalf("group write=%#v err=%v", response, err)
	}
	if read(first, "device-a") != "01AA" || read(first, "device-b") != "IDLE" || read(second, "device-a") != "IDLE" {
		t.Fatal("group command value leaked across Session or group")
	}
	call, err := second.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(3, "Groups/device-b"), MethodID: gu.NewStringNodeID(3, "Groups/device-b/SendCommand"), InputArguments: []*gu.Variant{gu.MustVariant("02BB")}})
	if err != nil || call == nil || call.StatusCode != gu.StatusOK || len(call.OutputArguments) != 1 || call.OutputArguments[0].Value() != "queued:device-b" {
		t.Fatalf("group Call=%#v err=%v", call, err)
	}
	if read(second, "device-b") != "02BB" || read(first, "device-b") != "IDLE" {
		t.Fatal("group method did not route by its own Session and group")
	}
	members, err := first.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(3, "Groups/device-a/MemberSourceIds"), AttributeID: gu.AttributeIDValue}}})
	if err != nil || members == nil || len(members.Results) != 1 || members.Results[0].Status != gu.StatusOK {
		t.Fatalf("members read=%#v err=%v", members, err)
	}
	ids, ok := members.Results[0].Value.Value().([]string)
	if !ok || len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("mapped source missing from MemberSourceIds: %#v", members.Results[0].Value.Value())
	}
}
