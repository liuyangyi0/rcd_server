package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadFlagsOverrideDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load([]string{
		"--data-dir", dir,
		"--opc-listen", "127.0.0.1:14840",
		"--opc-advertise-host", "manager.local",
		"--http-listen", "127.0.0.1:18080",
		"--lower-publishing-interval", "40ms",
		"--lower-sampling-interval", "30ms",
		"--audit-retention", "1080h",
		"--max-audit-rows", "25000",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DatabasePath != filepath.Join(dir, "manager.db") {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.OPCAdvertiseHost != "manager.local" {
		t.Fatalf("OPCAdvertiseHost = %q", cfg.OPCAdvertiseHost)
	}
	if cfg.AuditRetention != 45*24*time.Hour || cfg.MaxAuditRows != 25000 {
		t.Fatalf("retention=%s max rows=%d", cfg.AuditRetention, cfg.MaxAuditRows)
	}
	if cfg.LowerPublishingInterval != 40*time.Millisecond || cfg.LowerSamplingInterval != 30*time.Millisecond {
		t.Fatalf("lower publishing=%s sampling=%s", cfg.LowerPublishingInterval, cfg.LowerSamplingInterval)
	}
}

func TestValidateRejectsUnboundedSmallAuditLimit(t *testing.T) {
	cfg := Defaults()
	cfg.MaxAuditRows = 999
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() unexpectedly accepted an audit row limit below 1000")
	}
}

func TestValidateRejectsInvalidAddress(t *testing.T) {
	cfg := Defaults()
	cfg.HTTPListen = "missing-port"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() unexpectedly succeeded")
	}
}

func TestValidateRejectsPendingTimeoutShorterThanOperationTimeout(t *testing.T) {
	cfg := Defaults()
	cfg.CommandTimeout = 10 * time.Second
	cfg.CommandPendingTTL = 5 * time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() unexpectedly accepted a pending timeout shorter than the operation timeout")
	}
}

func TestServiceArgumentsUseAbsoluteDataDirectory(t *testing.T) {
	cfg := Defaults()
	cfg.DataDir = filepath.Join("relative", "manager-data")
	cfg.OPCPassword = "must-not-be-persisted"
	args, err := cfg.ServiceArguments()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 2 || args[0] != "--data-dir" || args[1] != want || !filepath.IsAbs(args[1]) {
		t.Fatalf("service arguments = %#v", args)
	}
	for _, arg := range args {
		if arg == cfg.OPCPassword {
			t.Fatalf("service arguments contain OPC password")
		}
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--lower-publishing-interval 50ms") || !strings.Contains(joined, "--lower-sampling-interval 50ms") {
		t.Fatalf("service arguments omit lower OPC intervals: %s", joined)
	}
}
