package processagent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func configFixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	lower := filepath.Join(dir, "lower instance")
	if err := os.Mkdir(lower, 0o750); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(lower, "rcd_server")
	if err := os.WriteFile(executable, []byte("test fixture, never executed\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	return Config{InstanceID: "lower-a", Executable: executable, DataDir: filepath.Join(dir, "agent state"), HTTPListen: "127.0.0.1:0", AutoStart: true, RestartPolicy: "never", StopTimeout: 15 * time.Second, StartTimeout: time.Minute, StartGrace: time.Second}
}

func TestConfigLoadDefaultsAndCanonicalPaths(t *testing.T) {
	fixture := configFixture(t)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeExe, err := filepath.Rel(workingDirectory, fixture.Executable)
	if err != nil {
		t.Fatal(err)
	}
	relativeData, err := filepath.Rel(workingDirectory, fixture.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--instance-id", fixture.InstanceID, "--executable", relativeExe, "--data-dir", relativeData})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := filepath.EvalSymlinks(fixture.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Executable != executable || cfg.WorkDir != filepath.Dir(executable) || cfg.DataDir != fixture.DataDir {
		t.Fatalf("paths not normalized or cwd not pinned to executable: %#v", cfg)
	}
	if cfg.HTTPListen != "0.0.0.0:8090" || !cfg.AutoStart || cfg.RestartPolicy != "never" || cfg.StopTimeout != 15*time.Second || cfg.StartTimeout != time.Minute || cfg.StartGrace != time.Second || len(cfg.Arguments) != 0 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration parsing must not create or modify agent state: %v", err)
	}
}

func TestConfigLoadExplicitOptions(t *testing.T) {
	fixture := configFixture(t)
	workDir := t.TempDir()
	cfg, err := Load([]string{"--instance-id", "site.1:lower_A-2", "--executable", fixture.Executable, "--data-dir", fixture.DataDir,
		"--work-dir", workDir, "--http-listen", "127.0.0.1:0", "--ready-address", "127.0.0.1:4840", "--auto-start=false", "--restart-policy", "on-failure", "--stop-timeout", "2s", "--start-timeout", "3s"})
	if err != nil {
		t.Fatal(err)
	}
	workDir, err = filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "site.1:lower_A-2" || cfg.WorkDir != workDir || cfg.AutoStart || cfg.HTTPListen != "127.0.0.1:0" || cfg.ReadyAddress != "127.0.0.1:4840" || cfg.RestartPolicy != "on-failure" || cfg.StopTimeout != 2*time.Second || cfg.StartTimeout != 3*time.Second {
		t.Fatalf("explicit locally configured options were not retained: %#v", cfg)
	}
}

func TestConfigLoadRejectsUnknownOrPositionalArguments(t *testing.T) {
	fixture := configFixture(t)
	base := []string{"--instance-id", fixture.InstanceID, "--executable", fixture.Executable, "--data-dir", fixture.DataDir}
	for _, suffix := range [][]string{{"unexpected-command"}, {"--", "another-executable"}, {"--arbitrary-flag"}, {"--auto-start=not-a-bool"}, {"--stop-timeout=not-a-duration"}} {
		t.Run(strings.Join(suffix, " "), func(t *testing.T) {
			if cfg, err := Load(append(append([]string(nil), base...), suffix...)); err == nil {
				t.Fatalf("invalid launch arguments accepted: %#v", cfg)
			}
		})
	}
}

func TestConfigNormalizedRejectsInvalidPathsIdentityAndTimeouts(t *testing.T) {
	fixture := configFixture(t)
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"missing executable", func(c *Config) { c.Executable = "" }},
		{"nonexistent executable", func(c *Config) { c.Executable += ".missing" }},
		{"directory is not executable file", func(c *Config) { c.Executable = filepath.Dir(c.Executable) }},
		{"missing state directory", func(c *Config) { c.DataDir = " \t " }},
		{"nonexistent cwd", func(c *Config) { c.WorkDir = filepath.Join(fixture.DataDir, "missing") }},
		{"file is not cwd", func(c *Config) { c.WorkDir = c.Executable }},
		{"empty identity", func(c *Config) { c.InstanceID = "" }},
		{"oversized identity", func(c *Config) { c.InstanceID = strings.Repeat("a", 129) }},
		{"whitespace identity", func(c *Config) { c.InstanceID = "lower a" }},
		{"control in identity", func(c *Config) { c.InstanceID = "lower\na" }},
		{"path in identity", func(c *Config) { c.InstanceID = "lower/a" }},
		{"invalid restart policy", func(c *Config) { c.RestartPolicy = "always" }},
		{"zero stop timeout", func(c *Config) { c.StopTimeout = 0 }},
		{"negative stop timeout", func(c *Config) { c.StopTimeout = -time.Second }},
		{"unbounded stop timeout", func(c *Config) { c.StopTimeout = 5*time.Minute + 1 }},
		{"zero start timeout", func(c *Config) { c.StartTimeout = 0 }},
		{"unbounded start timeout", func(c *Config) { c.StartTimeout = 5*time.Minute + 1 }},
		{"zero start grace", func(c *Config) { c.StartGrace = 0 }},
		{"negative start grace", func(c *Config) { c.StartGrace = -time.Second }},
		{"grace exceeds startup budget", func(c *Config) { c.StartGrace = c.StartTimeout + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixture
			test.change(&cfg)
			if normalized, err := cfg.normalized(); err == nil {
				t.Fatalf("invalid configuration accepted: %#v", normalized)
			}
		})
	}
	fixture.InstanceID = strings.Repeat("a", 128)
	fixture.StopTimeout, fixture.StartTimeout, fixture.StartGrace = 5*time.Minute, 5*time.Minute, 5*time.Minute
	if _, err := fixture.normalized(); err != nil {
		t.Fatalf("inclusive identifier/timeout bounds rejected: %v", err)
	}
}

func TestConfigNormalizedValidatesNumericAddressPorts(t *testing.T) {
	fixture := configFixture(t)
	for _, field := range []string{"HTTPListen", "ReadyAddress"} {
		for _, address := range []string{"127.0.0.1", "127.0.0.1:", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:not-a-service", "[::1]:65536"} {
			t.Run(field+"/"+address, func(t *testing.T) {
				cfg := fixture
				if field == "HTTPListen" {
					cfg.HTTPListen = address
				} else {
					cfg.ReadyAddress = address
				}
				if _, err := cfg.normalized(); err == nil {
					t.Fatalf("invalid %s=%q must fail before any child can start", field, address)
				}
			})
		}
	}
	fixture.ReadyAddress = "127.0.0.1:0"
	if _, err := fixture.normalized(); err == nil {
		t.Fatal("readiness cannot connect to an ephemeral zero port")
	}
	fixture.ReadyAddress = ":4840"
	if _, err := fixture.normalized(); err == nil {
		t.Fatal("readiness requires an explicit host")
	}
	for _, address := range []string{"127.0.0.1:1", "localhost:65535", "[::1]:4840"} {
		fixture.ReadyAddress = address
		if _, err := fixture.normalized(); err != nil {
			t.Fatalf("valid readiness address %q rejected: %v", address, err)
		}
	}
}

func TestConfigNormalizedResolvesSymlinksAndCopiesArguments(t *testing.T) {
	fixture := configFixture(t)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "lower-link")
	if err := os.Symlink(fixture.Executable, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	fixture.Arguments = []string{"locally-fixed-argument", "literal-value"}
	fixture.Executable = link
	cfg, err := fixture.normalized()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Executable != executable || cfg.WorkDir != filepath.Dir(executable) || !reflect.DeepEqual(cfg.Arguments, fixture.Arguments) {
		t.Fatalf("executable symlink was not pinned to its real directory: %#v", cfg)
	}
	fixture.Arguments[0] = "caller mutation"
	if cfg.Arguments[0] != "locally-fixed-argument" {
		t.Fatal("normalized launch arguments alias the caller's mutable slice")
	}
	cfg.Arguments[1] = "normalized mutation"
	if fixture.Arguments[1] != "literal-value" {
		t.Fatal("normalized arguments can mutate the input configuration")
	}
	workLink := filepath.Join(linkDir, "work-link")
	if err := os.Symlink(filepath.Dir(executable), workLink); err != nil {
		t.Fatal(err)
	}
	fixture.WorkDir = workLink
	cfg, err = fixture.normalized()
	if err != nil || cfg.WorkDir != filepath.Dir(executable) {
		t.Fatalf("explicit cwd symlink not canonicalized: %#v err=%v", cfg, err)
	}
}
