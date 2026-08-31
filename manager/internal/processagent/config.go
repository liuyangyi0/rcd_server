// Package processagent supervises one explicitly configured lower executable.
// It is deliberately independent of the lower OPC connection: a stopped lower
// must not take away the only interface capable of starting it again.
package processagent

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/domain"
)

type Config struct {
	InstanceID    string
	Executable    string
	WorkDir       string
	DataDir       string
	HTTPListen    string
	ReadyAddress  string
	AutoStart     bool
	RestartPolicy string
	StopTimeout   time.Duration
	StartTimeout  time.Duration
	StartGrace    time.Duration
	// Arguments is populated locally, never by a management HTTP request.
	Arguments []string
}

func Load(args []string) (Config, error) {
	cfg := Config{HTTPListen: "0.0.0.0:8090", AutoStart: true, RestartPolicy: "never", StopTimeout: 15 * time.Second, StartTimeout: 60 * time.Second, StartGrace: time.Second}
	fs := flag.NewFlagSet("rcd-manager agent run", flag.ContinueOnError)
	fs.StringVar(&cfg.InstanceID, "instance-id", "", "stable identifier of this one physical lower process")
	fs.StringVar(&cfg.Executable, "executable", "", "fixed lower executable path (not editable through HTTP)")
	fs.StringVar(&cfg.WorkDir, "work-dir", "", "fixed lower working directory; defaults to executable directory")
	fs.StringVar(&cfg.DataDir, "data-dir", "", "agent state and bounded process-output logs")
	fs.StringVar(&cfg.HTTPListen, "http-listen", cfg.HTTPListen, "independent process-control HTTP listener")
	fs.StringVar(&cfg.ReadyAddress, "ready-address", "", "optional TCP listen address to wait for; not an OPC health check")
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
