// SPDX-License-Identifier: MIT
// Package testutil builds inert ELF-header fixtures. Never a deployment artifact.
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semaphoreui/semaphore/services/rcd"
)

func Package(t testing.TB, component, release, notes string) []byte {
	t.Helper()
	payload := make([]byte, 64)
	copy(payload, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(payload[16:], 2)
	binary.LittleEndian.PutUint16(payload[18:], 62)
	binary.LittleEndian.PutUint32(payload[20:], 1)
	binary.LittleEndian.PutUint16(payload[52:], 64)
	sum := sha256.Sum256(payload)
	dirty := true
	zero := 0
	one := 1
	state := map[string]rcd.SchemaRange{}
	for _, name := range map[string][]string{"lower": {"command_journal", "maintenance"}, "manager": {"manager_db"}, "agent": {"agent_state"}, "communicate": {}}[component] {
		state[name] = rcd.SchemaRange{Min: &zero, Max: &one}
	}
	m := rcd.Manifest{SchemaVersion: 1, Component: component, Version: "1.0.0", ReleaseID: release, Commit: strings.Repeat("a", 40), SourceSHA256: strings.Repeat("b", 64), Dirty: &dirty, BuiltAt: "2026-10-08T00:00:00Z", Target: "linux/amd64", BinarySHA256: hex.EncodeToString(sum[:]), BinarySize: 64, ReleaseNotes: &notes, Compatibility: rcd.Compatibility{AdapterMinVersion: "1.0.0", StateFormats: state, ComponentMinVersions: map[string]string{}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, file := range []struct {
		Name string
		Data []byte
	}{{"manifest.json", raw}, {"binary", payload}} {
		if err = tw.WriteHeader(&tar.Header{Name: file.Name, Size: int64(len(file.Data)), Mode: 0644, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(file.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
