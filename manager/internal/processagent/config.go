// Package processagent supervises one explicitly configured lower executable.
// It is deliberately independent of the lower OPC connection: a stopped lower
// must not take away the only interface capable of starting it again.
package processagent

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/domain"
)

type Config struct {
	InstanceID          string
	Executable          string
	WorkDir             string
	DataDir             string
	LogDir              string
	HTTPListen          string
	ReadyAddress        string
	MaintenanceEndpoint string
	GatewayInstanceID   string
	HostReleaseDir      string
	RemoteManagers      string
	AutoStart           bool
	RestartPolicy       string
	StopTimeout         time.Duration
	StartTimeout        time.Duration
	StartGrace          time.Duration
	// Arguments is populated locally, never by a management HTTP request.
	Arguments []string
}

func Load(args []string) (Config, error) {
	cfg := Config{HTTPListen: "0.0.0.0:8090", AutoStart: true, RestartPolicy: "never", StopTimeout: 15 * time.Second, StartTimeout: 60 * time.Second, StartGrace: time.Second}
	cfg.HostReleaseDir = os.Getenv("RCD_AGENT_HOST_RELEASE_DIR")
	cfg.RemoteManagers = os.Getenv("RCD_AGENT_REMOTE_MANAGERS")
	fs := flag.NewFlagSet("rcd-manager agent run", flag.ContinueOnError)
	// Only these lower path options are exposed locally. HTTP cannot provide
	// child arguments, and omitted options retain the legacy lower defaults.
	var lowerPaths [4]string
	pathNames := []string{"config", "state", "pki", "log"}
	for i, name := range pathNames {
		fs.StringVar(&lowerPaths[i], "lower-"+name+"-dir", "", "fixed lower "+name+" directory (must exist)")
	}
	fs.StringVar(&cfg.InstanceID, "instance-id", "", "stable identifier of this one physical lower process")
	fs.StringVar(&cfg.Executable, "executable", "", "fixed lower executable path (not editable through HTTP)")
	fs.StringVar(&cfg.WorkDir, "work-dir", "", "fixed lower working directory; defaults to executable directory")
	fs.StringVar(&cfg.DataDir, "data-dir", "", "agent state and bounded process-output logs")
	fs.StringVar(&cfg.LogDir, "log-dir", "", "agent/process logs; defaults to agent data directory")
	fs.StringVar(&cfg.HTTPListen, "http-listen", cfg.HTTPListen, "independent process-control HTTP listener")
	fs.StringVar(&cfg.ReadyAddress, "ready-address", "", "optional TCP listen address to wait for; not an OPC health check")
	fs.StringVar(&cfg.MaintenanceEndpoint, "maintenance-endpoint", "", "fixed loopback OPC endpoint for maintenance handshake (None/Anonymous)")
	fs.StringVar(&cfg.GatewayInstanceID, "gateway-instance-id", "", "expected stable Rust gateway identity; required with maintenance-endpoint")
	fs.StringVar(&cfg.HostReleaseDir, "host-release-dir", cfg.HostReleaseDir, "fixed binary directory sharing the publisher host lock; enables safe lifecycle")
	fs.StringVar(&cfg.RemoteManagers, "remote-managers", cfg.RemoteManagers, "comma-separated explicit manager IP allowlist; remote writes use lifecycle API only")
	fs.BoolVar(&cfg.AutoStart, "auto-start", cfg.AutoStart, "start the lower on first run; later runs restore persisted desired state")
	fs.StringVar(&cfg.RestartPolicy, "restart-policy", cfg.RestartPolicy, "never or on-failure; never changes OPC source selection")
	fs.DurationVar(&cfg.StopTimeout, "stop-timeout", cfg.StopTimeout, "grace period before killing only the owned child")
	fs.DurationVar(&cfg.StartTimeout, "start-timeout", cfg.StartTimeout, "maximum wait for the configured TCP readiness address")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, errors.New("unexpected agent arguments; only the configured executable can be launched")
	}
	var pathErr error
	fs.Visit(func(f *flag.Flag) {
		for i, name := range pathNames {
			if f.Name != "lower-"+name+"-dir" || pathErr != nil {
				continue
			}
			if strings.TrimSpace(lowerPaths[i]) == "" {
				pathErr = fmt.Errorf("--%s cannot be empty", f.Name)
				return
			}
			path, err := filepath.Abs(lowerPaths[i])
			if err == nil {
				path, err = filepath.EvalSymlinks(path)
			}
			if err == nil {
				var info os.FileInfo
				info, err = os.Stat(path)
				if err == nil && !info.IsDir() {
					err = errors.New("not a directory")
				}
			}
			if err != nil {
				pathErr = fmt.Errorf("--%s: %w", f.Name, err)
				return
			}
			cfg.Arguments = append(cfg.Arguments, "--"+name+"-dir", path)
		}
	})
	if pathErr != nil {
		return Config{}, pathErr
	}
	return cfg.normalized()
}

func (cfg Config) normalized() (Config, error) {
	if !domain.ValidOperationRevision(cfg.InstanceID) {
		return Config{}, errors.New("--instance-id must be a stable nonempty identifier")
	}
	if strings.TrimSpace(cfg.Executable) == "" || strings.TrimSpace(cfg.DataDir) == "" {
		return Config{}, errors.New("--executable and --data-dir are required")
	}
	var err error
	cfg.Executable, err = filepath.Abs(cfg.Executable)
	if err != nil {
		return Config{}, err
	}
	cfg.Executable, err = filepath.EvalSymlinks(cfg.Executable)
	if err != nil {
		return Config{}, fmt.Errorf("resolve lower executable: %w", err)
	}
	info, err := os.Stat(cfg.Executable)
	if err != nil || !info.Mode().IsRegular() {
		return Config{}, errors.New("lower executable must be an existing regular file")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Dir(cfg.Executable)
	}
	cfg.WorkDir, err = filepath.Abs(cfg.WorkDir)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkDir, err = filepath.EvalSymlinks(cfg.WorkDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve lower working directory: %w", err)
	}
	info, err = os.Stat(cfg.WorkDir)
	if err != nil || !info.IsDir() {
		return Config{}, errors.New("lower working directory must exist")
	}
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return Config{}, err
	}
	if cfg.LogDir == "" {
		cfg.LogDir = cfg.DataDir
	} else {
		cfg.LogDir, err = filepath.Abs(cfg.LogDir)
		if err != nil {
			return Config{}, err
		}
	}
	if err := validateAddress(cfg.HTTPListen, true); err != nil {
		return Config{}, fmt.Errorf("invalid agent HTTP listen address: %w", err)
	}
	if cfg.ReadyAddress != "" {
		if err := validateAddress(cfg.ReadyAddress, false); err != nil {
			return Config{}, fmt.Errorf("invalid readiness address: %w", err)
		}
	}
	if cfg.RestartPolicy != "never" && cfg.RestartPolicy != "on-failure" {
		return Config{}, errors.New("restart policy must be never or on-failure")
	}
	if cfg.HostReleaseDir != "" {
		if !filepath.IsAbs(cfg.HostReleaseDir) || cfg.MaintenanceEndpoint == "" {
			return Config{}, errors.New("safe lifecycle needs an absolute host-release-dir and maintenance endpoint")
		}
		cfg.HostReleaseDir, err = filepath.EvalSymlinks(cfg.HostReleaseDir)
		if err != nil {
			return Config{}, err
		}
		info, err := os.Stat(cfg.HostReleaseDir)
		if err != nil || !info.IsDir() {
			return Config{}, errors.New("host release directory must exist")
		}
	}
	if cfg.RemoteManagers != "" {
		if cfg.HostReleaseDir == "" {
			return Config{}, errors.New("remote managers require safe lifecycle")
		}
		for _, host := range strings.Split(cfg.RemoteManagers, ",") {
			ip := net.ParseIP(strings.TrimSpace(host))
			if ip == nil || ip.IsUnspecified() {
				return Config{}, errors.New("remote managers must be explicit IP addresses")
			}
		}
	}
	if cfg.MaintenanceEndpoint != "" || cfg.GatewayInstanceID != "" {
		u, err := url.Parse(cfg.MaintenanceEndpoint)
		if err != nil || u.Scheme != "opc.tcp" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			(u.Path != "" && u.Path != "/") || !domain.ValidOperationRevision(cfg.GatewayInstanceID) {
			return Config{}, errors.New("maintenance requires a fixed OPC endpoint and gateway instance identity")
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || validateAddress(u.Host, false) != nil {
			return Config{}, errors.New("maintenance endpoint must use an explicit loopback IP and port")
		}
	}
	if cfg.StopTimeout <= 0 || cfg.StopTimeout > 5*time.Minute || cfg.StartTimeout <= 0 || cfg.StartTimeout > 5*time.Minute || cfg.StartGrace <= 0 || cfg.StartGrace > cfg.StartTimeout {
		return Config{}, errors.New("agent timeouts must be positive, at most five minutes, and start grace must fit the start timeout")
	}
	cfg.Arguments = append([]string(nil), cfg.Arguments...)
	return cfg, nil
}

func validateAddress(address string, allowZero bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !allowZero && strings.TrimSpace(host) == "" {
		return errors.New("readiness host must be explicit")
	}
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("port must be numeric")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || (!allowZero && n == 0) {
		return errors.New("port is outside the supported range")
	}
	return nil
}
