package opc

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"
	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

// Exercise the real route transaction, not a fake always-successful selector.
// No lower connection or serial/hardware operation exists in this fixture.
type selectionMethodBackend struct {
	fakeBackend
	routes *domain.RouteTable
	calls  atomic.Int32
}

func newSelectionMethodBackend(t *testing.T) *selectionMethodBackend {
	t.Helper()
	routes, err := domain.NewRouteTable(
		[]domain.Group{{ID: "g", DefaultSourceID: "a"}, {ID: "h", DefaultSourceID: "other"}},
		[]domain.Source{
			{ID: "a", GroupID: "g", Enabled: true, CatalogState: domain.CatalogReady},
			{ID: "b", GroupID: "g", Enabled: true, CatalogState: domain.CatalogCached},
			{ID: "disabled", GroupID: "g", Enabled: false, CatalogState: domain.CatalogReady},
			{ID: "conflict", GroupID: "g", Enabled: true, CatalogState: domain.CatalogConflict},
			{ID: "discovering", GroupID: "g", Enabled: true, CatalogState: domain.CatalogDiscovering},
			{ID: "other", GroupID: "h", Enabled: true, CatalogState: domain.CatalogReady},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return &selectionMethodBackend{routes: routes}
}

func (b *selectionMethodBackend) OpenSession(id string) error {
	_, err := b.routes.OpenSessionWithAutomatic(id, map[string]bool{"g": true})
	return err
}
func (b *selectionMethodBackend) CloseSession(id string)       { b.routes.CloseSession(id) }
func (b *selectionMethodBackend) TouchSession(id string) error { return b.routes.TouchSession(id) }
func (b *selectionMethodBackend) Session(id string) (domain.Session, error) {
	return b.routes.Session(id)
}
func (b *selectionMethodBackend) Groups() []domain.Group    { return b.routes.Groups() }
func (b *selectionMethodBackend) Sources() []domain.Source  { return b.routes.Sources() }
func (b *selectionMethodBackend) PendingCount(g string) int { return b.routes.PendingCount(g) }
func (b *selectionMethodBackend) SelectSource(sid, group, source string, revision uint64) (domain.GroupRoute, error) {
	b.calls.Add(1)
	return b.routes.Select(sid, group, source, revision)
}
func (b *selectionMethodBackend) LowerStatus(source string) (LowerStatus, bool) {
	connection, session := ConnectionDisconnected, SessionInactive
	if source == "a" {
		connection, session = ConnectionConnected, SessionActive
	}
	return LowerStatus{SourceID: source, Connection: connection, Session: session}, true
}

func TestSelectSourceMethodRejectsMalformedArgumentsWithoutChangingRoute(t *testing.T) {
	b := newSelectionMethodBackend(t)
	if err := b.OpenSession("s"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []awu.Variant
		want awu.StatusCode
	}{
		{"missing", nil, awu.BadArgumentsMissing},
		{"missing_revision", []awu.Variant{"b"}, awu.BadArgumentsMissing},
		{"extra", []awu.Variant{"b", uint64(1), true}, awu.BadTooManyArguments},
		{"source_type", []awu.Variant{uint32(1), uint64(1)}, awu.BadTypeMismatch},
		{"revision_uint32", []awu.Variant{"b", uint32(1)}, awu.BadTypeMismatch},
		{"revision_float", []awu.Variant{"b", float64(1)}, awu.BadTypeMismatch},
		{"revision_string", []awu.Variant{"b", "1"}, awu.BadTypeMismatch},
		{"empty_source", []awu.Variant{"", uint64(1)}, awu.BadInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := callSelectSource(b, "s", "g", tc.args)
			if result.StatusCode != tc.want || len(result.OutputArguments) != 0 {
				t.Fatalf("result=%#v", result)
			}
		})
	}
	if b.calls.Load() != 0 {
		t.Fatal("invalid input reached the route transaction")
	}
	if route, _ := b.routes.Route("s", "g"); route.SourceID != "a" || route.Revision != 1 {
		t.Fatalf("route=%#v", route)
	}
	result := callSelectSource(b, "closed", "g", []awu.Variant{"b", uint64(1)})
	if result.StatusCode != awu.BadSessionIDInvalid {
		t.Fatalf("closed Session=%#v", result)
	}
}

func TestSelectSourceMethodWireCASIsolationMembershipAndLegacyCompatibility(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "opc.tcp://" + listener.Addr().String()
	_ = listener.Close()
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	b := newSelectionMethodBackend(t)
	// Disposable self-signed clients are allowed only in this loopback fixture.
	server, err := NewUpstreamServer(UpstreamConfig{EndpointURL: endpoint, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath, InsecureSkipVerify: true}, b)
	if err != nil {
		t.Fatal(err)
	}
	catalog := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{}}
	if err := server.ReconcileCatalog(catalog, b.Groups(), b.Sources()); err != nil {
		t.Fatal(err)
	}
	// Reconciliation is idempotent and retains Method identity.
	methodID := awu.NewNodeIDString(3, "Groups/g/SelectSource")
	original, _ := server.server.NamespaceManager().FindNode(methodID)
	if err := server.ReconcileCatalog(catalog, b.Groups(), b.Sources()); err != nil {
		t.Fatal(err)
	}
	if current, _ := server.server.NamespaceManager().FindNode(methodID); current != original {
		t.Fatal("Method was recreated")
	}
	for _, tc := range []struct {
		suffix string
		names  []string
		types  []awu.NodeID
	}{
		{"_Input", []string{"SourceId", "ExpectedRevision"}, []awu.NodeID{awu.DataTypeIDString, awu.DataTypeIDUInt64}},
		{"_Output", []string{"SelectedSourceId", "SelectionRevision"}, []awu.NodeID{awu.DataTypeIDString, awu.DataTypeIDUInt64}},
	} {
		n, ok := server.server.NamespaceManager().FindNode(awu.NewNodeIDString(3, "Groups/g/SelectSource"+tc.suffix))
		if !ok {
			t.Fatal("Method argument node missing")
		}
		args := n.(*awserver.VariableNode).Value().Value.([]awu.ExtensionObject)
		if len(args) != 2 {
			t.Fatalf("arguments=%#v", args)
		}
		for i, raw := range args {
			a := raw.(awu.Argument)
			if a.Name != tc.names[i] || a.DataType != tc.types[i] || a.ValueRank != awu.ValueRankScalar {
				t.Fatalf("argument=%#v", a)
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server failed to stop")
		}
	})
	first, second := connectAnonymousClient(t, endpoint), connectAnonymousClient(t, endpoint)
	t.Cleanup(func() { _ = first.Close(context.Background()); _ = second.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	read := func(client *gopcua.Client, identifier string, attribute gu.AttributeID) *gu.DataValue {
		t.Helper()
		r, e := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{NodeID: gu.NewStringNodeID(3, identifier), AttributeID: attribute}}})
		if e != nil || r == nil || len(r.Results) != 1 || r.Results[0] == nil || r.Results[0].Status != gu.StatusOK {
			t.Fatalf("read %s=%#v err=%v", identifier, r, e)
		}
		return r.Results[0]
	}
	call := func(client *gopcua.Client, source string, revision uint64, want gu.StatusCode) *gu.CallMethodResult {
		t.Helper()
		r, e := client.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(3, "Groups/g"), MethodID: gu.NewStringNodeID(3, "Groups/g/SelectSource"), InputArguments: []*gu.Variant{gu.MustVariant(source), gu.MustVariant(revision)}})
		if e != nil || r == nil || r.StatusCode != want {
			t.Fatalf("call source=%s rev=%d result=%#v err=%v", source, revision, r, e)
		}
		if want != gu.StatusOK && len(r.OutputArguments) != 0 {
			t.Fatal("failed selection returned success outputs")
		}
		return r
	}
	value := func(client *gopcua.Client, field string) any {
		return read(client, "Groups/g/"+field, gu.AttributeIDValue).Value.Value()
	}
	members := value(first, "MemberSourceIds").([]string)
	if !reflect.DeepEqual(members, []string{"a", "b", "conflict", "disabled", "discovering"}) {
		t.Fatalf("members=%v", members)
	}
	description := read(first, "Groups/g/MemberSourceIds", gu.AttributeIDDescription).Value.Value().(*gu.LocalizedText).Text
	if !strings.Contains(description, "offline, disabled or unselectable") {
		t.Fatalf("description=%q", description)
	}
	for _, tc := range []struct {
		source     string
		selectable bool
		connection string
	}{
		{"a", true, "Connected"}, {"b", true, "Disconnected"}, {"disabled", false, "Disconnected"}, {"conflict", false, "Disconnected"}, {"discovering", false, "Disconnected"},
	} {
		if got := read(first, "Groups/g/Members/"+tc.source+"/Selectable", gu.AttributeIDValue).Value.Value(); got != tc.selectable {
			t.Fatalf("%s selectable=%v", tc.source, got)
		}
		if got := read(first, "Groups/g/Members/"+tc.source+"/ConnectionState", gu.AttributeIDValue).Value.Value(); got != tc.connection {
			t.Fatalf("%s connection=%v", tc.source, got)
		}
	}
	result := call(first, "b", 1, gu.StatusOK)
	if len(result.OutputArguments) != 2 || result.OutputArguments[0].Value() != "b" || result.OutputArguments[1].Value() != uint64(2) {
		t.Fatalf("outputs=%#v", result)
	}
	if value(first, "SelectedSourceId") != "b" || value(second, "SelectedSourceId") != "a" || value(second, "SelectionRevision") != uint64(1) {
		t.Fatal("Session isolation lost")
	}
	if got := read(first, "Groups/h/SelectedSourceId", gu.AttributeIDValue).Value.Value(); got != "other" {
		t.Fatal("other group changed")
	}
	var sid string
	for _, session := range b.routes.Sessions() {
		if session.Routes["g"].SourceID == "b" {
			sid = session.ID
		}
	}
	if sid == "" {
		t.Fatal("initiator Session not found")
	}
	if route, _ := b.routes.Route(sid, "g"); route.Automatic {
		t.Fatal("manual Method retained automatic participation")
	}
	// A Web update completed after this client read revision 2.
	if _, err := b.SelectSource(sid, "g", "a", 2); err != nil {
		t.Fatal(err)
	}
	call(first, "b", 2, gu.StatusBadInvalidState)
	if value(first, "SelectedSourceId") != "a" || value(first, "SelectionRevision") != uint64(3) {
		t.Fatal("stale caller overwrote Web")
	}
	legacy, err := first.Write(ctx, &gu.WriteRequest{NodesToWrite: []*gu.WriteValue{{NodeID: gu.NewStringNodeID(3, "Groups/g/SelectedSourceId"), AttributeID: gu.AttributeIDValue, Value: &gu.DataValue{EncodingMask: gu.DataValueValue, Value: gu.MustVariant("b")}}}})
	if err != nil || legacy == nil || len(legacy.Results) != 1 || legacy.Results[0] != gu.StatusOK || value(first, "SelectionRevision") != uint64(4) {
		t.Fatalf("legacy write=%#v err=%v", legacy, err)
	}
	for _, source := range []string{"disabled", "conflict", "discovering", "other"} {
		call(first, source, 4, gu.StatusBadConfigurationError)
	}
	call(first, "missing", 4, gu.StatusBadNotFound)
	if value(first, "SelectionRevision") != uint64(4) {
		t.Fatal("rejected target changed revision")
	}
	command, err := b.routes.BeginCommand(sid, "g", "fixture-port", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	call(first, "a", 4, gu.StatusBadWaitingForResponse)
	call(second, "b", 1, gu.StatusBadWaitingForResponse)
	if _, err := b.routes.CommitCommandFinish(command.ID); err != nil {
		t.Fatal(err)
	}
	// The same stale revision submitted concurrently must commit exactly once.
	type outcome struct {
		result *gu.CallMethodResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, source := range []string{"a", "b"} {
		go func(source string) {
			r, e := first.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(3, "Groups/g"), MethodID: gu.NewStringNodeID(3, "Groups/g/SelectSource"), InputArguments: []*gu.Variant{gu.MustVariant(source), gu.MustVariant(uint64(4))}})
			outcomes <- outcome{r, e}
		}(source)
	}
	success, conflict := 0, 0
	for range 2 {
		out := <-outcomes
		if out.err != nil || out.result == nil {
			t.Fatalf("concurrent Call=%#v", out)
		}
		switch out.result.StatusCode {
		case gu.StatusOK:
			success++
		case gu.StatusBadInvalidState:
			conflict++
		default:
			t.Fatalf("concurrent result=%#v", out.result)
		}
	}
	if success != 1 || conflict != 1 || value(first, "SelectionRevision") != uint64(5) {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	wrongObject, err := first.Call(ctx, &gu.CallMethodRequest{ObjectID: gu.NewStringNodeID(3, "Groups/h"), MethodID: gu.NewStringNodeID(3, "Groups/g/SelectSource"), InputArguments: []*gu.Variant{gu.MustVariant("a"), gu.MustVariant(uint64(5))}})
	if err != nil || wrongObject == nil || wrongObject.StatusCode != gu.StatusBadMethodInvalid {
		t.Fatalf("wrong ObjectID=%#v err=%v", wrongObject, err)
	}
	// Exercise the same Method on the already-supported secure endpoint.
	secure := connectSecureClient(t, endpoint, pair, "", "")
	call(secure, "b", 1, gu.StatusOK)
	if value(first, "SelectionRevision") != uint64(5) {
		t.Fatal("secure Session changed another Session")
	}
	t.Log(fmt.Sprintf("CAS Method, legacy Write, members, pending fence, concurrent stale calls and secure Session verified on %s", endpoint))
}
