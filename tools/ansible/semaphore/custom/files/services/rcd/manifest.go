// SPDX-License-Identifier: MIT
package rcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxManifestBytes = 64 << 10
const MaxBinaryBytes int64 = 512 << 20
const MaxArchiveBytes int64 = 256 << 20

var ErrInvalid = errors.New("invalid RCD package")
var ErrTooLarge = errors.New("RCD package exceeds size limit")
var ErrConflict = errors.New("RCD revision or release content conflict")
var ErrNotFound = errors.New("RCD package not found")
var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

type Compatibility struct {
	AdapterMinVersion    string                 `json:"adapter_min_version"`
	StateFormats         map[string]SchemaRange `json:"state_formats"`
	ComponentMinVersions map[string]string      `json:"component_min_versions"`
}

type SchemaRange struct {
	Min *int `json:"min"`
	Max *int `json:"max"`
}

type Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	Component     string        `json:"component"`
	Version       string        `json:"version"`
	ReleaseID     string        `json:"release_id"`
	Commit        string        `json:"commit"`
	SourceSHA256  string        `json:"source_sha256"`
	Dirty         *bool         `json:"dirty"`
	BuiltAt       string        `json:"built_at"`
	Target        string        `json:"target"`
	BinarySHA256  string        `json:"binary_sha256"`
	BinarySize    int64         `json:"binary_size"`
	Compatibility Compatibility `json:"compatibility"`
	ReleaseNotes  *string       `json:"release_notes"`
}

func IsComponent(s string) bool { return IsBuiltinComponent(s) || IsApplicationComponent(s) }

// Preserve the actual Rust/Go build target string; compare normalized CPU names
// only for compatibility. Rust reports x86_64/aarch64, Go reports amd64/arm64.
func Architecture(target string) string {
	switch target {
	case "linux/amd64", "linux/x86_64":
		return "amd64"
	case "linux/arm64", "linux/aarch64":
		return "arm64"
	}
	return ""
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 || !IsComponent(m.Component) || !identifier.MatchString(m.Version) || !identifier.MatchString(m.ReleaseID) {
		return fmt.Errorf("%w: schema, component or release identity", ErrInvalid)
	}
	if !digestPattern.MatchString(m.BinarySHA256) || !digestPattern.MatchString(m.SourceSHA256) || !commitPattern.MatchString(m.Commit) || m.Dirty == nil {
		return fmt.Errorf("%w: complete build identity required", ErrInvalid)
	}
	if _, err := time.Parse(time.RFC3339, m.BuiltAt); err != nil {
		return fmt.Errorf("%w: built_at needs a timezone", ErrInvalid)
	}
	if Architecture(m.Target) == "" {
		return fmt.Errorf("%w: unsupported target", ErrInvalid)
	}
	if m.BinarySize < 64 || m.BinarySize > MaxBinaryBytes {
		return fmt.Errorf("%w: binary size", ErrInvalid)
	}
	c := m.Compatibility
	if !versionPattern.MatchString(c.AdapterMinVersion) || c.ComponentMinVersions == nil || c.StateFormats == nil {
		return fmt.Errorf("%w: explicit compatibility requirements required", ErrInvalid)
	}
	formats := map[string][]string{"lower": {"command_journal", "maintenance"}, "manager": {"manager_db"}, "agent": {"agent_state"}, "communicate": {}}[m.Component]
	if len(c.StateFormats) != len(formats) {
		return fmt.Errorf("%w: state format coverage", ErrInvalid)
	}
	for _, name := range formats {
		bounds, ok := c.StateFormats[name]
		if !ok || bounds.Min == nil || bounds.Max == nil || *bounds.Min < 0 || *bounds.Max < *bounds.Min || *bounds.Max > 2147483647 {
			return fmt.Errorf("%w: state format bounds", ErrInvalid)
		}
	}
	for component, version := range c.ComponentMinVersions {
		if !IsComponent(component) || component == m.Component || !versionPattern.MatchString(version) {
			return fmt.Errorf("%w: component dependency", ErrInvalid)
		}
	}
	if m.ReleaseNotes == nil || len(*m.ReleaseNotes) > 16<<10 || strings.ContainsRune(*m.ReleaseNotes, 0) {
		return fmt.Errorf("%w: release notes", ErrInvalid)
	}
	return nil
}

// StrictJSON also rejects duplicate keys; DisallowUnknownFields alone does not.
func StrictJSON(raw []byte, value any) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("%w: JSON is not UTF-8", ErrInvalid)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueJSON(d, 0); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 12 {
		return errors.New("JSON nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return errors.New("invalid JSON delimiter")
	}
	keys := map[string]bool{}
	for d.More() {
		if delimiter == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] || strings.ToLower(name) != name {
				return errors.New("duplicate or non-canonical JSON key")
			}
			keys[name] = true
		}
		if err := uniqueJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func (m Manifest) Canonical() ([]byte, string) {
	raw, _ := json.Marshal(m)
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:])
}
