package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
	adapter "rcd_manager/internal/opc"
	"rcd_manager/internal/platform/pki"
	"rcd_manager/internal/store"
)

// Explicit software-only Rust fixture; no environment-supplied endpoint and
// no serial worker. Run on 46 with RCD_SYNC_FIXTURE pointing to unit_fixture.
func TestSynchronizationInputsThroughManagerToRust(t *testing.T) {
	fixture := os.Getenv("RCD_SYNC_FIXTURE")
	if fixture == "" {
		t.Skip("requires isolated Rust unit_fixture executable on 46")
	}
	address := func() string {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		a := l.Addr().String()
		_ = l.Close()
		return a
	}
	root := t.TempDir()
	lowerAddress := address()
	_, port, _ := net.SplitHostPort(lowerAddress)
	template, e := os.ReadFile("../../../communicate/examples/unit1-receiver.toml")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(template), "bind_ip = \"127.0.0.1\"") || strings.Contains(string(template), "enabled = true") {
		t.Fatal("unsafe fixture template")
	}
	csv, e := os.ReadFile("../../../communicate/examples/unit1-receiver.csv")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "config.toml"), []byte(strings.Replace(string(template), "port = 14842", "port = "+port, 1)), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "unit1-receiver.csv"), csv, 0600); e != nil {
		t.Fatal(e)
	}
	process := exec.Command(fixture, root)
	process.Env = append(os.Environ(), "RCD_SYNC_FIXTURE_LIFETIME_SECONDS=90")
	log, e := os.Create(filepath.Join(root, "rust.log"))
	if e != nil {
		t.Fatal(e)
	}
	process.Stdout = log
	process.Stderr = log
	if e = process.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait(); _ = log.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	for deadline := time.Now().Add(30 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", lowerAddress, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(filepath.Join(root, "rust.log"))
			t.Fatalf("Rust fixture not listening: %v; log=%s", err, logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	repo, e := store.Open(ctx, ":memory:", store.PassthroughProtector{})
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	group, e := repo.CreateGroup(ctx, domain.Group{ID: "sync", Name: "Sync"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = repo.CreateSource(ctx, domain.Source{ID: "rust", GroupID: "sync", Name: "Isolated Rust", Endpoint: "opc.tcp://" + lowerAddress + "/", SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, "")
	if e != nil {
		t.Fatal(e)
	}
	group.DefaultSourceID = "rust"
	_, e = repo.UpdateGroup(ctx, group, group.Revision)
	if e != nil {
		t.Fatal(e)
	}
	pair, e := pki.Ensure(t.TempDir(), "127.0.0.1")
	if e != nil {
		t.Fatal(e)
	}
	r, e := NewRuntime(ctx, repo, RuntimeOptions{CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
		PublishingInterval: 100 * time.Millisecond, SamplingInterval: 100 * time.Millisecond, CommandTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	upperAddress := address()
	endpoint := "opc.tcp://" + upperAddress
	server, e := adapter.NewUpstreamServer(adapter.UpstreamConfig{EndpointURL: endpoint, ListenAddress: upperAddress, CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath}, OPCBackend{Runtime: r})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.SetCatalogHandlers(server.ValidateCatalog, server.ReconcileCatalog); e != nil {
		t.Fatal(e)
	}
	if e = r.Start(ctx); e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	defer func() { _ = server.Close(); <-done }()
	wait := func(label string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		state, _ := r.LowerStatus("rust")
		sample, _ := r.cache.Get("rust", domain.NodeKey{NamespaceURI: adapter.BusinessNamespaceURI, Identifier: "DATA.Header.detectorTemp3"})
		logs, _ := os.ReadFile(filepath.Join(root, "rust.log"))
		t.Fatalf("timeout: %s; input=%+v; lower=%+v; catalog=%+v; rust=%s", label, sample, state, r.CatalogUnion().Conflicts, logs)
	}
	wait("lower discovery", func() bool { return len(r.CatalogUnion().Groups["sync"]) == 58 })
	wait("lower subscription ready", func() bool {
		s, ok := r.LowerStatus("rust")
		return ok && s.Connection == adapter.ConnectionConnected && s.Session == adapter.SessionActive && s.Subscription == adapter.SubscriptionActive
	})
	endpoints, e := opcua.GetEndpoints(ctx, endpoint)
	if e != nil {
		t.Fatal(e)
	}
	selected, e := opcua.SelectEndpoint(endpoints, ua.SecurityPolicyURINone, ua.MessageSecurityModeNone)
	if e != nil {
		t.Fatal(e)
	}
	client, e := opcua.NewClient(endpoint, opcua.SecurityFromEndpoint(selected, ua.UserTokenTypeAnonymous), opcua.AuthAnonymous(), opcua.AutoReconnect(false))
	if e != nil {
		t.Fatal(e)
	}
	if e = client.Connect(ctx); e != nil {
		t.Fatal(e)
	}
	defer client.Close(context.Background())
	read := func(name string) *ua.DataValue {
		t.Helper()
		out, e := client.Read(ctx, &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnBoth, NodesToRead: []*ua.ReadValueID{{NodeID: ua.NewStringNodeID(2, name), AttributeID: ua.AttributeIDValue}}})
		if e != nil || out == nil || len(out.Results) != 1 {
			t.Fatal(out, e)
		}
		return out.Results[0]
	}
	write := func(name string, value any) {
		t.Helper()
		out, e := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: ua.NewStringNodeID(2, name), AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(value)}}}})
		if e != nil || out == nil || len(out.Results) != 1 || out.Results[0] != ua.StatusOK {
			s, _ := r.LowerStatus("rust")
			t.Fatalf("write %s: %v %v lower=%+v", name, out, e, s)
		}
	}
	state := func() map[string]any {
		t.Helper()
		v := read("TransmitStatus")
		result := map[string]any{}
		if uint32(v.Status)&0xC0000000 == 0 && v.Value != nil {
			_ = json.Unmarshal([]byte(fmt.Sprint(v.Value.Value())), &result)
		}
		return result
	}
	values := map[string]any{}
	var names []string
	for name, node := range r.CatalogUnion().Groups["sync"] {
		if !node.Writable || name == "TransmitSnapshot" {
			continue
		}
		names = append(names, name)
		if node.DataType == domain.DataTypeUInt32 {
			values[name] = uint32(7)
		} else {
			length := 8
			if strings.HasSuffix(name, ".HW") {
				length = 969
			}
			values[name] = strings.Repeat("00", length)
		}
	}
	sort.Strings(names)
	if len(names) != 39 {
		t.Fatalf("input count=%d", len(names))
	}
	for _, name := range names[:len(names)-1] {
		write(name, values[name])
	}
	wait("incomplete", func() bool { return state()["received_inputs"] == float64(38) })
	if state()["snapshot_fresh"] != false {
		t.Fatal("incomplete input was ready")
	}
	write(names[len(names)-1], values[names[len(names)-1]])
	wait("ready", func() bool { return state()["snapshot_fresh"] == true })
	if state()["input_refresh_interval_ms"] != float64(5000) || state()["input_timeout_ms"] != float64(15000) {
		t.Fatal("defaults")
	}
	name := "DATA.Header.detectorTemp3"
	// A client supplies one enum, not five raw bytes. Readback proves the
	// selected lower accepted its typed value through the manager.
	fireName := "DATA.Buildings.11UBE.Fire"
	write(fireName, uint32(2))
	wait("fire enum forwarded", func() bool { v := read(fireName); return v.Value != nil && v.Value.Value() == uint32(2) })
	for _, invalid := range []any{uint32(0), uint32(9), "2"} {
		out, err := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: ua.NewStringNodeID(2, fireName), AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(invalid)}}}})
		if err != nil || out == nil || len(out.Results) != 1 || out.Results[0] == ua.StatusOK {
			t.Fatalf("invalid Fire accepted: %+v %v", out, err)
		}
	}
	description, err := client.Read(ctx, &ua.ReadRequest{NodesToRead: []*ua.ReadValueID{{NodeID: ua.NewStringNodeID(2, fireName), AttributeID: ua.AttributeIDDescription}}})
	if err != nil || len(description.Results) != 1 || description.Results[0].Value == nil || !strings.Contains(fmt.Sprint(description.Results[0].Value.Value()), "2=红色报警") {
		t.Fatal("manager lost enum description", description, err)
	}
	write(name, uint32(42))
	wait("changed input", func() bool {
		v := read(name)
		// Good may carry the Overflow information bit when intermediate changes
		// coalesce. Preserve that bit; it does not make the newest value Bad.
		return uint32(v.Status)&0xC0000000 == 0 && v.Value != nil && v.Value.Value() == uint32(42)
	})
	wait("changed input accepted", func() bool { return state()["revision"] == float64(41) })
	time.Sleep(5 * time.Second)
	write(name, uint32(42))
	// Status/Value subscriptions need not notify timestamp-only changes. The
	// lower's revision proves the equal write was accepted; the mailbox unit
	// tests independently verify renewal of its expiration deadline.
	wait("equal write refresh", func() bool { return state()["revision"] == float64(42) })
	// Only one field refreshed: the others must expire even while the manager
	// continues subscribing and reading status. No cached periodic replay.
	time.Sleep(11 * time.Second)
	wait("stale stop", func() bool { return state()["snapshot_fresh"] == false && state()["stale_inputs"].(float64) > 0 })
	var batch []*ua.WriteValue
	for _, name := range names {
		batch = append(batch, &ua.WriteValue{NodeID: ua.NewStringNodeID(2, name), AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(values[name])}})
	}
	response, err := client.Write(ctx, &ua.WriteRequest{NodesToWrite: batch})
	if err != nil || response == nil || len(response.Results) != len(batch) {
		t.Fatal("batch", response, err)
	}
	for _, status := range response.Results {
		if status != ua.StatusOK {
			t.Fatalf("parallel batch rejected: %v", status)
		}
	}
	wait("fresh recovery", func() bool { return state()["snapshot_fresh"] == true && state()["revision"] == float64(81) })
	// One OPC Write commits every field atomically; invalid/conflicting retries
	// must preserve the accepted snapshot. This uses no serial worker.
	request, e := json.Marshal(map[string]any{"revision": state()["revision"], "fields": values})
	if e != nil {
		t.Fatal(e)
	}
	write("TransmitSnapshot", string(request))
	wait("atomic mode", func() bool { return state()["input_mode"] == "atomic" })
	revision := state()["revision"]
	reject := func(node string, value any) {
		t.Helper()
		out, err := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: ua.NewStringNodeID(2, node), AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(value)}}}})
		if err != nil || out == nil || len(out.Results) != 1 || out.Results[0] == ua.StatusOK {
			t.Fatalf("expected rejected write: %v %v", out, err)
		}
	}
	reject("TransmitSnapshot", string(request))
	reject(name, uint32(99))
	if state()["revision"] != revision {
		t.Fatal("rejected write mutated snapshot")
	}
	values[name] = uint32(55)
	request, _ = json.Marshal(map[string]any{"revision": revision, "fields": values})
	write("TransmitSnapshot", string(request))
	wait("atomic committed", func() bool { return read(name).Value.Value() == uint32(55) })
	if r.routes.TotalPending() != 0 {
		t.Fatal("data writes leaked command reservations")
	}
	t.Log("client -> manager -> Rust: 39 original-name inputs, Fire enum, partial guard, immediate change, equal-value 5s refresh, 15s expiry, recovery; no hardware")
}
