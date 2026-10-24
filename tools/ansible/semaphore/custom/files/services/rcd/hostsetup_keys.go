// SPDX-License-Identifier: MIT
package rcd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var hostKeysWrite sync.Mutex

func ValidateHostKeys(keys []HostKey) error {
	if len(keys) < 1 || len(keys) > 5 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if len(k.PublicKey) > 16384 || strings.ContainsAny(k.PublicKey, "\r\n\x00") {
			return ErrInvalid
		}
		switch k.Algorithm {
		case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		default:
			return ErrInvalid
		}
		key, _, options, rest, e := ssh.ParseAuthorizedKey([]byte(k.PublicKey))
		if e != nil || len(options) > 0 || len(rest) > 0 || key.Type() != k.Algorithm || ssh.FingerprintSHA256(key) != k.Fingerprint || seen[k.Algorithm] {
			return ErrInvalid
		}
		seen[k.Algorithm] = true
	}
	return nil
}

// Trust only the complete, explicitly confirmed scan. This updates the native
// SSH client's known_hosts, not global SSH configuration. Changed existing
// keys are never replaced, even when a new onboarding request is submitted.
func ConfirmHostKeys(s HostSetup) error {
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	return ConfirmHostKeysAt(s, filepath.Join(home, ".ssh", "known_hosts"))
}
func ConfirmHostKeysAt(s HostSetup, path string) error {
	if s.Request.Validate() != nil || ValidateHostKeys(s.HostKeys) != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalid
	}
	hostKeysWrite.Lock()
	defer hostKeysWrite.Unlock()
	directory := filepath.Dir(path)
	for parent := directory; ; parent = filepath.Dir(parent) {
		info, e := os.Lstat(parent)
		if e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("known_hosts 目录不安全")
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	if e := os.MkdirAll(directory, 0700); e != nil {
		return e
	}
	info, e := os.Lstat(path)
	if e == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0) {
		return fmt.Errorf("known_hosts 文件不安全")
	}
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	file, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer file.Close()
	callback, e := knownhosts.New(path)
	if e != nil {
		return e
	}
	address := net.JoinHostPort(s.Request.Address, strconv.Itoa(s.Request.SSHPort))
	remote := &net.TCPAddr{IP: net.ParseIP(s.Request.Address), Port: s.Request.SSHPort}
	additions := []string{}
	// Validate every candidate first; a late conflict must not partially trust
	// a scan whose other keys the operator needs to investigate.
	for _, candidate := range s.HostKeys {
		key, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(candidate.PublicKey))
		e = callback(address, remote, key)
		if e == nil {
			continue
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(e, &keyErr) || len(keyErr.Want) > 0 {
			return fmt.Errorf("%w: 已有 SSH 主机指纹与新扫描不一致，需人工核查；未覆盖原信任记录", ErrConflict)
		}
		additions = append(additions, knownhosts.Line([]string{knownhosts.Normalize(address)}, key))
	}
	if len(additions) > 0 {
		if _, e = file.WriteString("\n" + strings.Join(additions, "\n") + "\n"); e != nil {
			return e
		}
		if e = file.Sync(); e != nil {
			return e
		}
	}
	return nil
}
