package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseValid returns a fully valid Config. Every path that Validate
// touches (private key, environment file) lives under t.TempDir.
func baseValid(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	pem := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(pem, []byte("-----BEGIN RSA PRIVATE KEY-----\ndummy\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "runner.env")
	if err := os.WriteFile(env, []byte("PATH=/usr/bin:/bin\nHOME=/home/gha-runner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Config{
		GitHub: GitHub{
			URL:   "https://github.com",
			App:   App{ClientID: "Iv1.test", InstallationID: 42, PrivateKeyPath: pem},
			Scope: Scope{Kind: "organization", Owner: "my-org"},
		},
		ScaleSet: ScaleSet{Name: "debian-host", RunnerGroup: "Default"},
		Capacity: Capacity{MinRunners: 0, MaxRunners: 4, JobCPUQuotaPercent: 400, JobMemoryMax: "8G"},
		Runner: Runner{
			Version:         "2.328.0",
			SHA256:          strings.Repeat("ab", 32),
			WorkDirectory:   "_work",
			DisableUpdate:   true,
			User:            "gha-runner",
			EnvironmentFile: env,
		},
		Paths: Paths{
			StateDir: filepath.Join(dir, "state"),
			CacheDir: filepath.Join(dir, "cache"),
			LogDir:   filepath.Join(dir, "log"),
		},
		Runtime: Runtime{
			Backend:          "process",
			JitDir:           filepath.Join(dir, "run"),
			SlotStartTimeout: 90 * time.Second,
			SlotStopTimeout:  30 * time.Second,
			CleanupTimeout:   60 * time.Second,
			AcquireGrace:     3 * time.Minute,
		},
		Observability: Observability{Listen: "127.0.0.1:9090", LogLevel: "info", ShipDiag: true},
	}
}

// writeConfig writes body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate(t *testing.T) {
	systemdDirPresent := func(t *testing.T) {
		t.Helper()
		old := systemdRuntimeDir
		systemdRuntimeDir = t.TempDir() // exists
		t.Cleanup(func() { systemdRuntimeDir = old })
	}
	systemdDirAbsent := func(t *testing.T) {
		t.Helper()
		old := systemdRuntimeDir
		systemdRuntimeDir = filepath.Join(t.TempDir(), "does-not-exist")
		t.Cleanup(func() { systemdRuntimeDir = old })
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		setup   func(*testing.T)
		wantErr string // substring of the joined error; "" means want nil
	}{
		{"valid", func(*Config) {}, nil, ""},
		{"max_runners at hard cap", func(c *Config) { c.Capacity.MaxRunners = HardCapMaxRunners }, nil, ""},
		{"repository scope with repository", func(c *Config) { c.GitHub.Scope.Kind = "repository"; c.GitHub.Scope.Repository = "my-repo" }, nil, ""},
		{"systemd backend with runtime dir", func(c *Config) { c.Runtime.Backend = "systemd" }, systemdDirPresent, ""},

		{"max_runners zero", func(c *Config) { c.Capacity.MaxRunners = 0 }, nil, "capacity.max_runners must be >= 1"},
		{"min_runners above max", func(c *Config) { c.Capacity.MinRunners = 5; c.Capacity.MaxRunners = 4 }, nil, "min_runners (5) must be <= capacity.max_runners (4)"},
		{"max_runners over hard cap", func(c *Config) { c.Capacity.MaxRunners = HardCapMaxRunners + 1 }, nil, "hard cap of 32"},
		{"cpu quota zero", func(c *Config) { c.Capacity.JobCPUQuotaPercent = 0 }, nil, "job_cpu_quota_percent"},
		{"cpu quota negative", func(c *Config) { c.Capacity.JobCPUQuotaPercent = -400 }, nil, "job_cpu_quota_percent"},
		{"cpu quota over ceiling", func(c *Config) { c.Capacity.JobCPUQuotaPercent = MaxCPUQuotaPercent + 1 }, nil, "job_cpu_quota_percent"},
		{"memory max empty", func(c *Config) { c.Capacity.JobMemoryMax = "" }, nil, "job_memory_max"},
		{"installation id zero", func(c *Config) { c.GitHub.App.InstallationID = 0 }, nil, "installation_id must be > 0"},
		{"installation id negative", func(c *Config) { c.GitHub.App.InstallationID = -1 }, nil, "installation_id must be > 0"},
		{"client id empty", func(c *Config) { c.GitHub.App.ClientID = "" }, nil, "client_id must not be empty"},
		{"private key path empty", func(c *Config) { c.GitHub.App.PrivateKeyPath = "" }, nil, "private_key_path must not be empty"},
		{"private key missing", func(c *Config) { c.GitHub.App.PrivateKeyPath = filepath.Join(c.Paths.StateDir, "nope.pem") }, nil, "private_key_path"},
		{"scope kind invalid", func(c *Config) { c.GitHub.Scope.Kind = "enterprise" }, nil, `scope.kind must be "organization" or "repository"`},
		{"scope kind empty", func(c *Config) { c.GitHub.Scope.Kind = "" }, nil, "scope.kind"},
		{"repository kind without repository", func(c *Config) { c.GitHub.Scope.Kind = "repository" }, nil, "repository must be set"},
		{"owner empty", func(c *Config) { c.GitHub.Scope.Owner = "" }, nil, "owner must not be empty"},
		{"label with space", func(c *Config) { c.ScaleSet.Name = "bad name" }, nil, "scale_set.name"},
		{"label leading dash", func(c *Config) { c.ScaleSet.Name = "-bad" }, nil, "scale_set.name"},
		{"label with slash", func(c *Config) { c.ScaleSet.Name = "bad/name" }, nil, "scale_set.name"},
		{"version empty", func(c *Config) { c.Runner.Version = "" }, nil, "runner.version"},
		{"version two parts", func(c *Config) { c.Runner.Version = "2.328" }, nil, "runner.version"},
		{"version prefixed", func(c *Config) { c.Runner.Version = "v2.328.0" }, nil, "runner.version"},
		{"version non-numeric", func(c *Config) { c.Runner.Version = "latest" }, nil, "runner.version"},
		{"sha256 empty", func(c *Config) { c.Runner.SHA256 = "" }, nil, "runner.sha256"},
		{"sha256 too short", func(c *Config) { c.Runner.SHA256 = strings.Repeat("ab", 20) }, nil, "runner.sha256"},
		{"sha256 not hex", func(c *Config) { c.Runner.SHA256 = strings.Repeat("zz", 32) }, nil, "runner.sha256"},
		{"sha256 uppercase hex", func(c *Config) { c.Runner.SHA256 = strings.Repeat("AB", 32) }, nil, ""},
		{"environment file empty", func(c *Config) { c.Runner.EnvironmentFile = "" }, nil, "environment_file must not be empty"},
		{"environment file missing", func(c *Config) { c.Runner.EnvironmentFile = filepath.Join(c.Paths.StateDir, "nope.env") }, nil, "environment_file"},
		{"backend invalid", func(c *Config) { c.Runtime.Backend = "docker" }, nil, `runtime.backend must be "systemd" or "process"`},
		{"backend empty", func(c *Config) { c.Runtime.Backend = "" }, nil, "runtime.backend"},
		{"systemd backend without runtime dir", func(c *Config) { c.Runtime.Backend = "systemd" }, systemdDirAbsent, "runtime.backend"},
		{"state dir empty", func(c *Config) { c.Paths.StateDir = "" }, nil, "state_dir must not be empty"},
		{"cache dir empty", func(c *Config) { c.Paths.CacheDir = "" }, nil, "cache_dir must not be empty"},
		{"log dir empty", func(c *Config) { c.Paths.LogDir = "" }, nil, "log_dir must not be empty"},
		{"start timeout zero", func(c *Config) { c.Runtime.SlotStartTimeout = 0 }, nil, "slot_start_timeout"},
		{"stop timeout zero", func(c *Config) { c.Runtime.SlotStopTimeout = 0 }, nil, "slot_stop_timeout"},
		{"cleanup timeout zero", func(c *Config) { c.Runtime.CleanupTimeout = 0 }, nil, "cleanup_timeout"},
		{"acquire grace zero", func(c *Config) { c.Runtime.AcquireGrace = 0 }, nil, "acquire_grace"},
		{"listen not host:port", func(c *Config) { c.Observability.Listen = "localhost" }, nil, "observability.listen"},
		{"listen empty", func(c *Config) { c.Observability.Listen = "" }, nil, "observability.listen"},
		{"log level invalid", func(c *Config) { c.Observability.LogLevel = "verbose" }, nil, "log_level"},
		{"log level empty", func(c *Config) { c.Observability.LogLevel = "" }, nil, "log_level"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			c := baseValid(t)
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateJoinsAllErrors proves Validate reports every failure at
// once (errors.Join), not just the first one.
func TestValidateJoinsAllErrors(t *testing.T) {
	c := &Config{} // nothing set, nothing valid
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want errors for a zero Config")
	}
	for _, want := range []string{
		"max_runners", "installation_id", "client_id", "private_key_path",
		"scope.kind", "owner", "scale_set.name", "version", "sha256",
		"environment_file", "backend", "state_dir", "cache_dir", "log_dir",
		"job_cpu_quota_percent", "job_memory_max", "slot_start_timeout",
		"slot_stop_timeout", "cleanup_timeout", "acquire_grace",
		"observability.listen", "log_level",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q:\n%v", want, err)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	body := `github:
  app:
    installation_id: 7
  scope:
    kind: organization
    owner: my-org
scale_set:
  name: debian-host
runner:
  version: 2.328.0
`
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if c.GitHub.URL != "https://github.com" {
		t.Errorf("GitHub.URL = %q, want default https://github.com", c.GitHub.URL)
	}
	if c.ScaleSet.RunnerGroup != "Default" {
		t.Errorf("ScaleSet.RunnerGroup = %q, want Default", c.ScaleSet.RunnerGroup)
	}
	if c.Capacity.MinRunners != 0 || c.Capacity.MaxRunners != 0 {
		t.Errorf("Capacity = %+v, want zeros", c.Capacity)
	}
	if c.Runner.WorkDirectory != "_work" {
		t.Errorf("Runner.WorkDirectory = %q, want _work", c.Runner.WorkDirectory)
	}
	if c.Runner.User != "gha-runner" {
		t.Errorf("Runner.User = %q, want gha-runner", c.Runner.User)
	}
	if !c.Runner.DisableUpdate {
		t.Error("Runner.DisableUpdate = false, want default true")
	}
	if c.Paths.StateDir != "/var/lib/gh-runnerd" || c.Paths.CacheDir != "/var/cache/gh-runnerd" || c.Paths.LogDir != "/var/log/gh-runnerd" {
		t.Errorf("Paths = %+v, want plan §4 defaults", c.Paths)
	}
	if c.Runtime.Backend != "systemd" {
		t.Errorf("Runtime.Backend = %q, want default systemd", c.Runtime.Backend)
	}
	if c.Runtime.JitDir != "/run/gh-runnerd" {
		t.Errorf("Runtime.JitDir = %q, want /run/gh-runnerd", c.Runtime.JitDir)
	}
	if c.Runtime.SlotStartTimeout != 90*time.Second || c.Runtime.SlotStopTimeout != 30*time.Second ||
		c.Runtime.CleanupTimeout != 60*time.Second || c.Runtime.AcquireGrace != 3*time.Minute {
		t.Errorf("Runtime timeouts = %+v, want 90s/30s/60s/3m", c.Runtime)
	}
	if c.Observability.Listen != "127.0.0.1:9090" || c.Observability.LogLevel != "info" || !c.Observability.ShipDiag {
		t.Errorf("Observability = %+v, want 127.0.0.1:9090/info/true", c.Observability)
	}
}

func TestLoadOverridesAndNormalization(t *testing.T) {
	body := `github:
  url: https://github.com/
  app:
    client_id: Iv1.abc
    installation_id: 42
    private_key_path: /tmp/app.pem
  scope:
    kind: repository
    owner: my-org
    repository: my-repo
scale_set:
  name: debian-host
  runner_group: Custom
  extra_labels: [foo, bar]
capacity:
  min_runners: 1
  max_runners: 8
  job_cpu_quota_percent: 200
  job_memory_max: 16G
runner:
  version: 2.328.0
  sha256: ` + strings.Repeat("ab", 32) + `
  work_directory: work
  disable_update: false
  user: some-user
  group: some-group
  environment_file: /tmp/runner.env
paths:
  state_dir: /tmp/state
  cache_dir: /tmp/cache
  log_dir: /tmp/log
runtime:
  backend: process
  slot_start_timeout: 90s
  slot_stop_timeout: 30s
  cleanup_timeout: 1m
  acquire_grace: 3m
  jit_dir: /tmp/jit
observability:
  listen: 0.0.0.0:9091
  log_level: debug
  ship_diag: false
`
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if c.GitHub.URL != "https://github.com" {
		t.Errorf("GitHub.URL = %q, want trailing slash trimmed", c.GitHub.URL)
	}
	if c.GitHub.App.ClientID != "Iv1.abc" || c.GitHub.App.InstallationID != 42 || c.GitHub.App.PrivateKeyPath != "/tmp/app.pem" {
		t.Errorf("GitHub.App = %+v", c.GitHub.App)
	}
	if c.GitHub.Scope.Kind != "repository" || c.GitHub.Scope.Owner != "my-org" || c.GitHub.Scope.Repository != "my-repo" {
		t.Errorf("GitHub.Scope = %+v", c.GitHub.Scope)
	}
	if c.ScaleSet.RunnerGroup != "Custom" || len(c.ScaleSet.ExtraLabels) != 2 {
		t.Errorf("ScaleSet = %+v", c.ScaleSet)
	}
	if c.Capacity.MinRunners != 1 || c.Capacity.MaxRunners != 8 || c.Capacity.JobCPUQuotaPercent != 200 || c.Capacity.JobMemoryMax != "16G" {
		t.Errorf("Capacity = %+v", c.Capacity)
	}
	if c.Runner.WorkDirectory != "work" || c.Runner.DisableUpdate || c.Runner.User != "some-user" || c.Runner.Group != "some-group" {
		t.Errorf("Runner = %+v", c.Runner)
	}
	if c.Paths.StateDir != "/tmp/state" || c.Paths.CacheDir != "/tmp/cache" || c.Paths.LogDir != "/tmp/log" {
		t.Errorf("Paths = %+v", c.Paths)
	}
	if c.Runtime.Backend != "process" || c.Runtime.JitDir != "/tmp/jit" {
		t.Errorf("Runtime = %+v", c.Runtime)
	}
	if c.Runtime.SlotStartTimeout != 90*time.Second || c.Runtime.SlotStopTimeout != 30*time.Second ||
		c.Runtime.CleanupTimeout != time.Minute || c.Runtime.AcquireGrace != 3*time.Minute {
		t.Errorf("Runtime timeouts = %+v, want 90s/30s/1m/3m", c.Runtime)
	}
	if c.Observability.Listen != "0.0.0.0:9091" || c.Observability.LogLevel != "debug" || c.Observability.ShipDiag {
		t.Errorf("Observability = %+v, want explicit false preserved", c.Observability)
	}
}

func TestLoadStrictDecode(t *testing.T) {
	base := `github:
  app:
    installation_id: 7
  scope:
    kind: organization
    owner: my-org
scale_set:
  name: debian-host
runner:
  version: 2.328.0
`
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unknown top-level field", base + "wat: 1\n", "field wat not found"},
		{"unknown field under github", "github:\n  url: https://x\n  wat: 1\n", "field wat not found"},
		{"unknown field under capacity", "github:\n  app:\n    installation_id: 7\nscale_set:\n  name: debian-host\ncapacity:\n  wat: 1\n", "field wat not found"},
		{"unknown field under scale_set", "github:\n  app:\n    installation_id: 7\nscale_set:\n  name: debian-host\n  wat: 1\n", "field wat not found"},
		{"unknown field under runner", "github:\n  app:\n    installation_id: 7\nscale_set:\n  name: debian-host\nrunner:\n  version: 2.328.0\n  wat: 1\n", "field wat not found"},
		{"multiple documents", "github: {}\n---\ngithub: {}\n", "multiple YAML documents"},
		{"empty file", "", "no YAML documents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("Load() = nil, want error for missing file")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load() error = %q, want read failure context", err)
	}
}

func TestValidateSha256EnvEscape(t *testing.T) {
	t.Run("allow unverified payload", func(t *testing.T) {
		t.Setenv(AllowUnverifiedPayloadEnv, "1")
		c := baseValid(t)
		c.Runner.SHA256 = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() with %s=1 and empty sha256 = %v, want nil", AllowUnverifiedPayloadEnv, err)
		}
	})
	t.Run("non-1 value does not escape", func(t *testing.T) {
		t.Setenv(AllowUnverifiedPayloadEnv, "0")
		c := baseValid(t)
		c.Runner.SHA256 = ""
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("Validate() with %s=0 = %v, want sha256 error", AllowUnverifiedPayloadEnv, err)
		}
	})
}

func TestEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	c := &Config{
		Paths: Paths{
			StateDir: filepath.Join(dir, "state"),
			CacheDir: filepath.Join(dir, "cache"),
			LogDir:   filepath.Join(dir, "log"),
		},
		Runtime: Runtime{JitDir: filepath.Join(dir, "run")},
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() = %v", err)
	}
	want := []string{
		filepath.Join(dir, "state"),
		filepath.Join(dir, "cache"),
		filepath.Join(dir, "log"),
		filepath.Join(dir, "state", "slots"),
		filepath.Join(dir, "state", "template"),
		filepath.Join(dir, "run"),
	}
	for _, d := range want {
		fi, err := os.Stat(d)
		if err != nil {
			t.Errorf("EnsureDirs() did not create %s: %v", d, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s exists but is not a directory", d)
		}
	}
}

func TestEnsureDirsEmptyPath(t *testing.T) {
	if err := (&Config{}).EnsureDirs(); err == nil {
		t.Fatal("EnsureDirs() on zero Config = nil, want error for empty path")
	}
}

// TestExampleConfigRoundTrips guarantees the shipped example config
// Loads and passes Validate from the repo root — the exact path --dry-run
// walks. The example leaves sha256 empty (plan §6 shape), so the
// unverified-payload escape must be set; the app's --dry-run should do
// the same or the operator fills in the digest.
func TestExampleConfigRoundTrips(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root) // the example's paths are repo-relative
	t.Setenv(AllowUnverifiedPayloadEnv, "1")

	c, err := Load("configs/config.example.yaml")
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(example) = %v, want nil (--dry-run must pass)", err)
	}
	if c.Runtime.Backend != "process" {
		t.Errorf("example backend = %q, want process for local dry-run", c.Runtime.Backend)
	}
	if c.Capacity.MaxRunners != 4 {
		t.Errorf("example max_runners = %d, want 4", c.Capacity.MaxRunners)
	}
	// The referenced files must exist next to the example.
	if _, err := os.Stat(c.GitHub.App.PrivateKeyPath); err != nil {
		t.Errorf("example private_key_path %q missing: %v", c.GitHub.App.PrivateKeyPath, err)
	}
	if _, err := os.Stat(c.Runner.EnvironmentFile); err != nil {
		t.Errorf("example environment_file %q missing: %v", c.Runner.EnvironmentFile, err)
	}
}

func TestHardCap(t *testing.T) {
	if HardCapMaxRunners != 32 {
		t.Fatalf("HardCapMaxRunners = %d, want 32", HardCapMaxRunners)
	}
}

// TestEnsureDirsRejectsUnwritableDir: plan §6 — "state/cache directories
// writable" fails closed; an existing read-only directory must not pass
// (MkdirAll alone would succeed on it).
func TestEnsureDirsRejectsUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure does not apply to root")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0o755) // let t.TempDir clean up
	c := &Config{
		Paths:   Paths{StateDir: blocked, CacheDir: filepath.Join(dir, "cache"), LogDir: filepath.Join(dir, "log")},
		Runtime: Runtime{JitDir: filepath.Join(dir, "run")},
	}
	err := c.EnsureDirs()
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("EnsureDirs() = %v, want not-writable error", err)
	}
}

// TestValidateRejectsEnvFileMissingRequiredVars: plan §6 — the
// environment file must carry PATH and HOME, or the "systemd does not
// see mise/node" failure mode comes back.
func TestValidateRejectsEnvFileMissingRequiredVars(t *testing.T) {
	c := baseValid(t)
	envFile := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(envFile, []byte("LANG=C.UTF-8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Runner.EnvironmentFile = envFile
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate: expected error for env file without PATH/HOME")
	}
	for _, want := range []string{"PATH", "HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error %q missing %q", err, want)
		}
	}
}

// TestValidateRejectsMalformedEnvFile: a broken KEY=VALUE line fails at
// validation time, not at the first slot start.
func TestValidateRejectsMalformedEnvFile(t *testing.T) {
	c := baseValid(t)
	envFile := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(envFile, []byte("this line has no equals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Runner.EnvironmentFile = envFile
	if err := c.Validate(); err == nil {
		t.Fatal("Validate: expected error for malformed env file")
	}
}
