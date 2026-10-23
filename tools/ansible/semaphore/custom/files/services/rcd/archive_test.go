// SPDX-License-Identifier: MIT
package rcd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (Manifest, []byte) {
	t.Helper()
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	binary.LittleEndian.PutUint16(data[18:], 62)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	sum := sha256.Sum256(data)
	dirty := true
	notes := "测试包，绝不执行"
	zero := 0
	one := 1
	m := Manifest{SchemaVersion: 1, Component: "manager", Version: "1.0.0", ReleaseID: "test-001", Commit: strings.Repeat("a", 40), SourceSHA256: strings.Repeat("b", 64), Dirty: &dirty, BuiltAt: "2026-10-08T00:00:00Z", Target: "linux/amd64", BinarySHA256: hex.EncodeToString(sum[:]), BinarySize: int64(len(data)), ReleaseNotes: &notes, Compatibility: Compatibility{AdapterMinVersion: "1.0.0", StateFormats: map[string]SchemaRange{"manager_db": {Min: &zero, Max: &one}}, ComponentMinVersions: map[string]string{}}}
	return m, data
}

type member struct {
	name   string
	data   []byte
	kind   byte
	format tar.Format
	link   string
}

func TestCommunicationManifestRequiresExplicitEmptyStateFormats(t *testing.T) {
	m, _ := fixture(t)
	m.Component = "communicate"
	m.Target = "linux/x86_64"
	m.Compatibility.StateFormats = map[string]SchemaRange{}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Compatibility.StateFormats = nil
	if err := m.Validate(); err == nil {
		t.Fatal("null state_formats cannot substitute for explicit no-database declaration")
	}
	zero := 0
	m.Compatibility.StateFormats = map[string]SchemaRange{"manager_db": {Min: &zero, Max: &zero}}
	if err := m.Validate(); err == nil {
		t.Fatal("communication cannot inherit the manager database")
	}
}

func pack(t *testing.T, entries []member) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		format := entry.format
		if format == 0 {
			format = tar.FormatUSTAR
		}
		size := int64(len(entry.data))
		if kind == tar.TypeSymlink || kind == tar.TypeLink || kind == tar.TypeDir {
			size = 0
		}
		h := &tar.Header{Name: entry.name, Size: size, Mode: 0644, Typeflag: kind, Format: format, Linkname: entry.link}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func TestRCDArchiveValidation(t *testing.T) {
	m, data := fixture(t)
	raw, _ := json.Marshal(m)
	valid := []member{{name: "manifest.json", data: raw}, {name: "binary", data: data}}
	for _, order := range [][]member{valid, {valid[1], valid[0]}} {
		v, err := ValidateArchive(context.Background(), bytes.NewReader(pack(t, order)), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if v.Manifest.Component != "manager" || len(v.ID) != 64 {
			t.Fatal(v)
		}
		info, err := os.Stat(filepath.Join(v.Directory, "binary"))
		if err != nil || info.Mode().Perm()&0111 != 0 {
			t.Fatal("uploaded binary must not be executable", err)
		}
	}
	cases := map[string][]member{
		"path-traversal": {valid[0], {name: "../binary", data: data}},
		"absolute-path":  {valid[0], {name: "/binary", data: data}},
		"duplicate":      {valid[0], valid[1], valid[1]},
		"extra-config":   {valid[0], valid[1], {name: "config.toml", data: []byte("secret")}},
		"symlink":        {valid[0], {name: "binary", kind: tar.TypeSymlink, link: "/bin/sh"}},
		"hardlink":       {valid[0], {name: "binary", kind: tar.TypeLink, link: "manifest.json"}},
		"directory":      {valid[0], {name: "binary", kind: tar.TypeDir}},
		"missing":        {valid[0]},
		"invalid-elf":    {valid[0], {name: "binary", data: bytes.Repeat([]byte("x"), 64)}},
		"gnu":            {{name: "manifest.json", data: raw, format: tar.FormatGNU}, valid[1]},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := ValidateArchive(context.Background(), bytes.NewReader(pack(t, entries)), dir); err == nil {
				t.Fatal("accepted invalid archive")
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatal("failed upload left files")
			}
		})
	}
	good := pack(t, valid)
	for name, archive := range map[string][]byte{"truncated": good[:len(good)-3], "concatenated-gzip": append(append([]byte{}, good...), good...), "trailing-garbage": append(append([]byte{}, good...), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateArchive(context.Background(), bytes.NewReader(archive), t.TempDir()); err == nil {
				t.Fatal("accepted invalid gzip")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateArchive(ctx, bytes.NewReader(good), t.TempDir()); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestRCDManifestStrict(t *testing.T) {
	m, _ := fixture(t)
	raw, _ := json.Marshal(m)
	for name, value := range map[string][]byte{
		"unknown":          append([]byte(`{"install_command":"rm",`), raw[1:]...),
		"duplicate":        append([]byte(`{"component":"lower",`), raw[1:]...),
		"case-alias":       append([]byte(`{"COMPONENT":"lower",`), raw[1:]...),
		"trailing":         append(append([]byte{}, raw...), []byte(` {}`)...),
		"nested-duplicate": []byte(`{"a":{"min":0,"min":1}}`),
		"null-required":    bytes.Replace(raw, []byte(`"dirty":true`), []byte(`"dirty":null`), 1),
		"missing-range":    bytes.Replace(raw, []byte(`"state_formats":{"manager_db":{"min":0,"max":1}}`), []byte(`"state_formats":{}`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			var decoded Manifest
			err := StrictJSON(value, &decoded)
			if err == nil {
				err = decoded.Validate()
			}
			if err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}

func TestRCDArchiveBoundsAndArchitecture(t *testing.T) {
	m, data := fixture(t)
	for _, name := range []string{"manifest.json", "binary"} {
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		tw := tar.NewWriter(gz)
		size := MaxBinaryBytes + 1
		if name == "manifest.json" {
			size = MaxManifestBytes + 1
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		// Deliberately only a header: reject the declared size before allocation.
		_ = tw.Close()
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateArchive(context.Background(), bytes.NewReader(compressed.Bytes()), t.TempDir()); !errors.Is(err, ErrTooLarge) {
			t.Fatal("size limit", name, err)
		}
	}
	m.Target = "linux/arm64"
	raw, _ := json.Marshal(m)
	if _, err := ValidateArchive(context.Background(), bytes.NewReader(pack(t, []member{{name: "manifest.json", data: raw}, {name: "binary", data: data}})), t.TempDir()); err == nil {
		t.Fatal("wrong architecture accepted")
	}
}

// Invoked remotely after the Python builder creates the same contract fixture.
func TestRCDPythonPackageInterop(t *testing.T) {
	path := os.Getenv("RCD_PACKAGE_INTEROP")
	if path == "" {
		t.Skip("Python fixture path not supplied")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = ValidateArchive(context.Background(), f, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
