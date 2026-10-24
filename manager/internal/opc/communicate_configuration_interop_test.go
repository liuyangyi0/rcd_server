package opc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"rcd_manager/internal/domain"
)

// Opt-in software-only acceptance. It owns a temporary directory and loopback
// server, generates disabled configuration, and never accepts a live endpoint.
func TestCommunicateConfigurationSetInterop(t *testing.T) {
	binary := os.Getenv("RCD_COMMUNICATE_BINARY")
	if binary == "" {
		t.Skip("requires isolated communication executable on 46")
	}
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	readExample := func(name string) string {
		t.Helper()
		b, e := os.ReadFile("../../../communicate/examples/" + name)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	original := strings.ReplaceAll(readExample("unit1-receiver.toml"), "unit1-receiver.csv", "points.csv")
	original = strings.Replace(original, "port = 14842", fmt.Sprintf("port = %d", port), 1)
	if strings.Contains(original, "enabled = true") {
		t.Fatal("unsafe fixture")
	}
	for name, value := range map[string]string{"config.toml": original, "points.csv": readExample("unit1-receiver.csv")} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	logFile, err := os.Create(filepath.Join(root, "startup.log"))
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, "run")
	process.Dir = root
	process.Stdout = logFile
	process.Stderr = logFile
	if err := process.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait(); _ = logFile.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("opc.tcp://127.0.0.1:%d/", port)
	for {
		conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if e == nil {
			conn.Close()
			break
		}
		if ctx.Err() != nil {
			b, _ := os.ReadFile(filepath.Join(root, "startup.log"))
			t.Fatalf("startup: %s", b)
		}
		time.Sleep(30 * time.Millisecond)
	}
	endpoints, err := gopcua.GetEndpoints(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := gopcua.SelectEndpoint(endpoints, ua.SecurityPolicyURINone, ua.MessageSecurityModeNone)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gopcua.NewClient(endpoint, gopcua.SecurityFromEndpoint(selected, ua.UserTokenTypeAnonymous), gopcua.AuthAnonymous(), gopcua.AutoReconnect(false))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	namespaces, err := client.NamespaceArray(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ns uint16
	for i, uri := range namespaces {
		if uri == domain.OperationsNamespaceURI {
			ns = uint16(i)
		}
	}
	if ns == 0 {
		t.Fatal("operations namespace missing")
	}
	call := func(request domain.OperationRequest, wantError bool) map[string]any {
		t.Helper()
		b, _ := json.Marshal(request)
		result, e := client.Call(ctx, &ua.CallMethodRequest{ObjectID: ua.NewStringNodeID(ns, "Operations"), MethodID: ua.NewStringNodeID(ns, "Operations/Execute"), InputArguments: []*ua.Variant{ua.MustVariant(string(b))}})
		if e != nil || result.StatusCode != ua.StatusOK || len(result.OutputArguments) != 1 {
			t.Fatalf("call %v %v", result, e)
		}
		var reply struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
			Error  struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if e = json.Unmarshal([]byte(result.OutputArguments[0].Value().(string)), &reply); e != nil {
			t.Fatal(e)
		}
		if wantError {
			if reply.OK || reply.Error.Code != "conflict" {
				t.Fatalf("expected conflict: %+v", reply)
			}
			return nil
		}
		if !reply.OK {
			t.Fatalf("operation rejected: %+v", reply)
		}
		if e = validateOperationResult(request, reply.Result); e != nil {
			t.Fatal("manager response validation", e)
		}
		var value map[string]any
		if e = json.Unmarshal(reply.Result, &value); e != nil {
			t.Fatal(e)
		}
		return value
	}
	call(domain.OperationRequest{Action: "list_configs"}, false)
	const name = "configuration-set.toml"
	initial := call(domain.OperationRequest{Action: "read_config", Name: name}, false)
	next := strings.ReplaceAll(readExample("unit9-receiver.toml"), "unit9-receiver.csv", "points.csv")
	next = strings.Replace(next, `instance_id = "unit9-receiver"`, `instance_id = "unit1-receiver"`, 1)
	next = strings.Replace(next, "port = 14842", fmt.Sprintf("port = %d", port), 1)
	content := fmt.Sprintf("config_toml = %q\npoints_csv = %q\n", next, readExample("unit9-receiver.csv"))
	validation := call(domain.OperationRequest{Action: "validate_config", Name: name, Content: &content}, false)
	if validation["valid"] != true {
		t.Fatal(validation)
	}
	request := domain.OperationRequest{Action: "write_config", Name: name, Content: &content, ExpectedRevision: initial["revision"].(string)}
	saved := call(request, false)
	if saved["restart_required"] != true {
		t.Fatal(saved)
	}
	call(request, true)
	backup := call(domain.OperationRequest{Action: "read_config", Name: name, BackupID: initial["revision"].(string)}, false)
	restore := backup["content"].(string)
	restored := call(domain.OperationRequest{Action: "write_config", Name: name, Content: &restore, ExpectedRevision: saved["revision"].(string)}, false)
	if restored["restart_required"] != false {
		t.Fatal(restored)
	}
	bytes, e := os.ReadFile(filepath.Join(root, "config.toml"))
	if e != nil || string(bytes) != original {
		t.Fatal("configuration not restored", e)
	}
	t.Log("real Rust OPC / existing Go result validator: list, paired validation, role switch, CAS, backup, restore passed; serial disabled")
}
