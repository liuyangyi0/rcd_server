// SPDX-License-Identifier: MIT
package rcd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestKey(t *testing.T) HostKey {
	t.Helper()
	public, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	key, e := ssh.NewPublicKey(public)
	if e != nil {
		t.Fatal(e)
	}
	return HostKey{Algorithm: key.Type(), Fingerprint: ssh.FingerprintSHA256(key), PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}
}
func setupTestRequest() HostSetupRequest {
	return HostSetupRequest{Name: "fixture", Address: "192.0.2.10", SSHPort: 22, SSHUser: "operator", SSHKeyID: 4, ServiceAccount: "operator"}
}
func TestHostSetupRequestBoundary(t *testing.T) {
	good := setupTestRequest()
	if good.Validate() != nil {
		t.Fatal("valid request")
	}
	for _, edit := range []func(*HostSetupRequest){func(r *HostSetupRequest) { r.Address = "host;sh" }, func(r *HostSetupRequest) { r.ServiceAccount = "root" }, func(r *HostSetupRequest) { r.SSHPort = 0 }, func(r *HostSetupRequest) { r.SSHUser = "u\nx" }, func(r *HostSetupRequest) { r.Name = "x\ny" }} {
		bad := good
		edit(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid request", bad)
		}
	}
}
func TestHostSetupReadOnlyCredentialsDoNotPermitFirstInstall(t *testing.T) {
	req := setupTestRequest()
	if req.Validate() != nil {
		t.Fatal("read-only enrollment should be permitted")
	}
	if req.CanInstall() {
		t.Fatal("ordinary SSH without sudo permitted installation")
	}
	req.BecomeKeyID = 2
	if !req.CanInstall() {
		t.Fatal("explicit sudo credential refused")
	}
	req.BecomeKeyID = 0
	req.SSHUser = "root"
	if !req.CanInstall() {
		t.Fatal("explicit root SSH refused")
	}
}
func TestHostSetupPublicJSONDoesNotExposeAdmissionOrKeys(t *testing.T) {
	s := HostSetup{ID: strings.Repeat("a", 64), Nonce: "secret-nonce", RequestKey: "key", UserID: 2, InventoryID: 3, HostKeys: []HostKey{setupTestKey(t)}, Install: &HostSetupInstall{RequestKey: "install-secret"}}
	raw, _ := json.Marshal(s)
	for _, hidden := range []string{"secret-nonce", "install-secret", "public_key", "inventory_id", "user_id"} {
		if strings.Contains(string(raw), hidden) {
			t.Fatal("exposed internal identity", string(raw))
		}
	}
}
func TestHostSetupHostKeyMismatchNeverOverwritesTrust(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, ".ssh", "known_hosts")
	first := HostSetup{Request: setupTestRequest(), HostKeys: []HostKey{setupTestKey(t)}}
	if e := ConfirmHostKeysAt(first, file); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if e = ConfirmHostKeysAt(first, file); e != nil {
		t.Fatal(e)
	}
	same, _ := os.ReadFile(file)
	if string(raw) != string(same) {
		t.Fatal("duplicate trust appended")
	}
	changed := first
	changed.HostKeys = []HostKey{setupTestKey(t)}
	if ConfirmHostKeysAt(changed, file) == nil {
		t.Fatal("changed host key accepted")
	}
	after, _ := os.ReadFile(file)
	if string(raw) != string(after) {
		t.Fatal("changed trust file")
	}
}
func TestHostSetupHostKeyIntegrityAndUnsafeTrustPath(t *testing.T) {
	key := setupTestKey(t)
	if ValidateHostKeys([]HostKey{key}) != nil {
		t.Fatal("key invalid")
	}
	bad := key
	bad.Fingerprint = "SHA256:wrong"
	if ValidateHostKeys([]HostKey{bad}) == nil {
		t.Fatal("mismatching fingerprint")
	}
	directory := t.TempDir()
	link := filepath.Join(directory, "linked")
	if e := os.Symlink(directory, link); e != nil {
		t.Fatal(e)
	}
	if ConfirmHostKeysAt(HostSetup{Request: setupTestRequest(), HostKeys: []HostKey{key}}, filepath.Join(link, "known_hosts")) == nil {
		t.Fatal("symlink trust directory")
	}
}
