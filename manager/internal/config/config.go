package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultOPCListen  = "0.0.0.0:4840"
	DefaultHTTPListen = "0.0.0.0:8080"
)

// Config contains process-level settings. Groups and lower OPC sources live in SQLite
// so they can be changed without restarting the service.
type Config struct {
	FleetConfigFile string
	DataDir                 string
	DatabasePath            string
	PKIDir                  string
	LogDir                  string
	OPCListen               string
	OPCAdvertiseHost        string
	OPCUsername             string
	OPCPassword             string
	HTTPListen              string
	LowerPublishingInterval time.Duration
	LowerSamplingInterval   time.Duration
	CommandTimeout          time.Duration
	CommandPendingTTL       time.Duration
	ShutdownTimeout         time.Duration
	AuditRetention          time.Duration
	MaxAuditRows            int
	MaxSources              int
	MaxUpstreamSessions     int
	Development             bool
}

func Defaults() Config {
	dataDir := defaultDataDir()
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}
	return Config{
		DataDir:                 dataDir,
		DatabasePath:            filepath.Join(dataDir, "manager.db"),
		PKIDir:                  filepath.Join(dataDir, "pki"),
		LogDir:                  filepath.Join(dataDir, "logs"),
		OPCListen:               DefaultOPCListen,
		OPCAdvertiseHost:        host,
		OPCUsername:             "user1",
		OPCPassword:             "pwd",
		HTTPListen:              DefaultHTTPListen,
		LowerPublishingInterval: 50 * time.Millisecond,
		LowerSamplingInterval:   50 * time.Millisecond,
		CommandTimeout:          5 * time.Second,
		CommandPendingTTL:       30 * time.Second,
		ShutdownTimeout:         15 * time.Second,
		AuditRetention:          90 * 24 * time.Hour,
		MaxAuditRows:            100000,
		MaxSources:              20,
		MaxUpstreamSessions:     10,
	}
}

// Load reads environment variables and then command-line flags. Flags take precedence.
func Load(args []string) (Config, error) {
	cfg := Defaults()
	applyEnv(&cfg)

	fs := flag.NewFlagSet("rcd-manager", flag.ContinueOnError)
	fs.StringVar(&cfg.FleetConfigFile,"fleet-config",cfg.FleetConfigFile,"path to machine inventory sync settings (separate from business configuration)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory for database, logs and PKI")
	fs.StringVar(&cfg.OPCListen, "opc-listen", cfg.OPCListen, "OPC UA listen address")
	fs.StringVar(&cfg.OPCAdvertiseHost, "opc-advertise-host", cfg.OPCAdvertiseHost, "host advertised in OPC UA endpoint URLs")
	fs.StringVar(&cfg.OPCUsername, "opc-username", cfg.OPCUsername, "legacy-compatible OPC username (anonymous access remains enabled)")
	fs.StringVar(&cfg.HTTPListen, "http-listen", cfg.HTTPListen, "HTTP listen address")
	fs.DurationVar(&cfg.LowerPublishingInterval, "lower-publishing-interval", cfg.LowerPublishingInterval, "publishing interval requested from lower OPC sources")
	fs.DurationVar(&cfg.LowerSamplingInterval, "lower-sampling-interval", cfg.LowerSamplingInterval, "sampling interval requested from lower OPC sources")
	fs.DurationVar(&cfg.CommandTimeout, "command-timeout", cfg.CommandTimeout, "downstream OPC method/write timeout")
	fs.DurationVar(&cfg.CommandPendingTTL, "command-pending-timeout", cfg.CommandPendingTTL, "maximum time a _Send command locks source selection")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "graceful shutdown timeout")
	fs.DurationVar(&cfg.AuditRetention, "audit-retention", cfg.AuditRetention, "retention period for command, management and source-state history")
	fs.IntVar(&cfg.MaxAuditRows, "max-audit-rows", cfg.MaxAuditRows, "maximum rows retained per operational history table")
	fs.BoolVar(&cfg.Development, "dev", cfg.Development, "enable development diagnostics")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.DataDir = filepath.Clean(cfg.DataDir)
	cfg.DatabasePath = filepath.Join(cfg.DataDir, "manager.db")
	cfg.PKIDir = filepath.Join(cfg.DataDir, "pki")
	cfg.LogDir = filepath.Join(cfg.DataDir, "logs")
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.FleetConfigFile != "" && !filepath.IsAbs(c.FleetConfigFile) { return errors.New("fleet configuration path must be absolute") }
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data directory is required")
	}
	if err := validateListen("OPC", c.OPCListen); err != nil {
		return err
	}
	if err := validateListen("HTTP", c.HTTPListen); err != nil {
		return err
	}
	if strings.TrimSpace(c.OPCAdvertiseHost) == "" {
		return errors.New("OPC advertise host is required")
	}
	if c.LowerPublishingInterval <= 0 || c.LowerSamplingInterval <= 0 || c.CommandTimeout <= 0 || c.CommandPendingTTL <= 0 || c.ShutdownTimeout <= 0 || c.AuditRetention <= 0 {
		return errors.New("timeouts must be positive")
	}
	if c.MaxAuditRows < 1000 {
		return errors.New("maximum audit rows must be at least 1000")
	}
	if c.CommandPendingTTL < c.CommandTimeout {
		return errors.New("command pending timeout must be greater than or equal to command timeout")
	}
	if c.MaxSources <= 0 || c.MaxUpstreamSessions <= 0 {
		return errors.New("capacity limits must be positive")
	}
	return nil
}

func (c Config) EnsureDirectories() error {
	for _, dir := range []string{c.DataDir, c.PKIDir, c.LogDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// ServiceArguments returns an explicit, working-directory-independent command
// line for the Windows Service Control Manager. Passwords are intentionally not
// included; service secrets must come from the machine environment or DPAPI-
// protected source records.
func (c Config) ServiceArguments() ([]string, error) {
	dataDir, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve service data directory: %w", err)
	}
	args := []string{
		"--data-dir", dataDir,
		"--opc-listen", c.OPCListen,
		"--opc-advertise-host", c.OPCAdvertiseHost,
		"--opc-username", c.OPCUsername,
		"--http-listen", c.HTTPListen,
		"--lower-publishing-interval", c.LowerPublishingInterval.String(),
		"--lower-sampling-interval", c.LowerSamplingInterval.String(),
		"--command-timeout", c.CommandTimeout.String(),
		"--command-pending-timeout", c.CommandPendingTTL.String(),
		"--shutdown-timeout", c.ShutdownTimeout.String(),
		"--audit-retention", c.AuditRetention.String(),
		"--max-audit-rows", strconv.Itoa(c.MaxAuditRows),
	}
	if c.Development {
		args = append(args, "--dev")
	}
	if c.FleetConfigFile != "" { args=append(args,"--fleet-config",c.FleetConfigFile) }
	return args, nil
}

func applyEnv(cfg *Config) {
	cfg.FleetConfigFile=os.Getenv("RCD_MANAGER_FLEET_CONFIG")
	setString := func(name string, dst *string) {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			*dst = value
		}
	}
	setString("RCD_MANAGER_DATA_DIR", &cfg.DataDir)
	setString("RCD_MANAGER_OPC_LISTEN", &cfg.OPCListen)
	setString("RCD_MANAGER_OPC_ADVERTISE_HOST", &cfg.OPCAdvertiseHost)
	setString("RCD_MANAGER_OPC_USERNAME", &cfg.OPCUsername)
	setString("RCD_MANAGER_OPC_PASSWORD", &cfg.OPCPassword)
	setString("RCD_MANAGER_HTTP_LISTEN", &cfg.HTTPListen)
	setDuration := func(name string, dst *time.Duration) {
		if value, ok := os.LookupEnv(name); ok {
			if parsed, err := time.ParseDuration(value); err == nil {
				*dst = parsed
			}
		}
	}
	setDuration("RCD_MANAGER_LOWER_PUBLISHING_INTERVAL", &cfg.LowerPublishingInterval)
	setDuration("RCD_MANAGER_LOWER_SAMPLING_INTERVAL", &cfg.LowerSamplingInterval)
	if value, ok := os.LookupEnv("RCD_MANAGER_DEV"); ok {
		if parsed, err := strconv.ParseBool(value); err == nil {
			cfg.Development = parsed
		}
	}
	if value, ok := os.LookupEnv("RCD_MANAGER_AUDIT_RETENTION"); ok {
		if parsed, err := time.ParseDuration(value); err == nil {
			cfg.AuditRetention = parsed
		}
	}
	if value, ok := os.LookupEnv("RCD_MANAGER_MAX_AUDIT_ROWS"); ok {
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.MaxAuditRows = parsed
		}
	}
}

func validateListen(label, address string) error {
	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("invalid %s listen address %q: %w", label, address, err)
	}
	return nil
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if base := os.Getenv("ProgramData"); base != "" {
			return filepath.Join(base, "RcdManager")
		}
	}
	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "RcdManager")
	}
	return filepath.Join(".", "data")
}
