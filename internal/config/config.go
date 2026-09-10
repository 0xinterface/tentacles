// Package config loads, validates, and exposes the tentacles YAML
// configuration. The YAML layout is fixed by the implementation plan §6;
// do not add fields without updating the example config.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hkust/tentacles/internal/env"
)

// HardCapMaxRunners is the compiled ceiling for capacity.max_runners.
// It exists so a typo cannot ask the host for a thousand runner
// processes; raise it only with the host's resources in mind.
const HardCapMaxRunners = 32

// AllowUnverifiedPayloadEnv, when set to "1", skips the runner.sha256
// requirement so an unverified payload can be fetched. It matches
// internal/payload.AllowUnverifiedEnv; keep the values in lockstep.
const AllowUnverifiedPayloadEnv = "TENTACLES_ALLOW_UNVERIFIED_PAYLOAD"

// Default directories and values applied by Load when the YAML omits them.
const (
	DefaultGitHubURL   = "https://github.com"
	DefaultRunnerGroup = "Default"
	DefaultWorkDir     = "_work"
	DefaultRunnerUser  = "gha-runner"
	DefaultStateDir    = "/var/lib/tentacles"
	DefaultCacheDir    = "/var/cache/tentacles"
	DefaultLogDir      = "/var/log/tentacles"
	DefaultJitDir      = "/run/tentacles"
	DefaultBackend     = "systemd"
	DefaultListen      = "127.0.0.1:9090"
	DefaultLogLevel    = "info"
)

// Backend names for runtime.backend.
const (
	BackendSystemd = "systemd"
	BackendProcess = "process"
)

// Default timeouts per implementation plan §6.
const (
	DefaultSlotStartTimeout = 90 * time.Second
	DefaultSlotStopTimeout  = 30 * time.Second
	DefaultCleanupTimeout   = 60 * time.Second
	DefaultAcquireGrace     = 3 * time.Minute
)

// MaxCPUQuotaPercent is the upper bound for capacity.job_cpu_quota_percent
// (systemd CPUQuota accepts a ceiling of 100 * NumCPU; 100*1024 covers any
// plausible host).
const MaxCPUQuotaPercent = 100 * 1024

// Config is the root of the tentacles YAML document.
type Config struct {
	GitHub        GitHub        `yaml:"github"`
	ScaleSet      ScaleSet      `yaml:"scale_set"`
	Capacity      Capacity      `yaml:"capacity"`
	Runner        Runner        `yaml:"runner"`
	Paths         Paths         `yaml:"paths"`
	Runtime       Runtime       `yaml:"runtime"`
	Scaling       Scaling       `yaml:"scaling"`
	Observability Observability `yaml:"observability"`
}

// Admission-gate defaults (plan §9/§13).
const (
	DefaultCPUTargetPercent    = 90
	DefaultMemoryMarginPercent = 20
	DefaultSampleInterval      = 30 * time.Second
)

// GitHub holds the GitHub App credentials and the target scope.
type GitHub struct {
	URL   string `yaml:"url"`
	App   App    `yaml:"app"`
	Scope Scope  `yaml:"scope"`
}

// App is the GitHub App used for authentication.
type App struct {
	ClientID       string `yaml:"client_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// Scope selects the organization or repository that owns the scale set.
type Scope struct {
	Kind       string `yaml:"kind"` // organization | repository
	Owner      string `yaml:"owner"`
	Repository string `yaml:"repository"`
}

// ScaleSet describes the GitHub Actions runner scale set to own.
type ScaleSet struct {
	Name        string   `yaml:"name"`
	RunnerGroup string   `yaml:"runner_group"`
	ExtraLabels []string `yaml:"extra_labels"`
}

// Capacity bounds how many runner processes the host may run and their
// per-slot resource limits.
type Capacity struct {
	MinRunners         int    `yaml:"min_runners"`
	MaxRunners         int    `yaml:"max_runners"`
	JobCPUQuotaPercent int    `yaml:"job_cpu_quota_percent"`
	JobMemoryMax       string `yaml:"job_memory_max"`
}

// Runner pins the official actions/runner payload and how slots run it.
type Runner struct {
	Version         string `yaml:"version"`
	DownloadURL     string `yaml:"download_url"`
	SHA256          string `yaml:"sha256"`
	WorkDirectory   string `yaml:"work_directory"`
	DisableUpdate   bool   `yaml:"disable_update"`
	User            string `yaml:"user"`
	Group           string `yaml:"group"` // slot unit group; "" means same as user
	EnvironmentFile string `yaml:"environment_file"`
}

// Paths are the daemon's on-disk homes.
type Paths struct {
	StateDir string `yaml:"state_dir"`
	CacheDir string `yaml:"cache_dir"`
	LogDir   string `yaml:"log_dir"`
}

// Runtime tunes slot lifecycle behavior.
type Runtime struct {
	Backend          string        `yaml:"backend"`
	SlotStartTimeout time.Duration `yaml:"slot_start_timeout"`
	SlotStopTimeout  time.Duration `yaml:"slot_stop_timeout"`
	CleanupTimeout   time.Duration `yaml:"cleanup_timeout"`
	AcquireGrace     time.Duration `yaml:"acquire_grace"`
	JitDir           string        `yaml:"jit_dir"`
}

// Scaling tunes the history-based admission gate (plan §9/§13). When
// admission_control is on, a new slot is held back whenever the
// predicted resource usage of all busy slots plus the incoming job
// would exceed the host budget derived from these targets.
type Scaling struct {
	AdmissionControl    bool          `yaml:"admission_control"`
	CPUTargetPercent    int           `yaml:"cpu_target_percent"`
	MemoryMarginPercent int           `yaml:"memory_margin_percent"`
	SampleInterval      time.Duration `yaml:"sample_interval"`
}

// Observability configures the metrics endpoint and logging.
type Observability struct {
	Listen   string `yaml:"listen"`
	LogLevel string `yaml:"log_level"`
	ShipDiag bool   `yaml:"ship_diag"`
}

var (
	// labelRe matches a GitHub Actions runner label: letters, digits,
	// underscore first, then dots, dashes and underscores.
	labelRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// versionRe matches a bare X.Y.Z release version.
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	// sha256Re matches a 64-character lowercase or uppercase hex digest.
	sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// systemdRuntimeDir is probed to decide whether the systemd backend is
// available on this host. Overridden in tests.
var systemdRuntimeDir = "/run/systemd/system"

// Validate checks every fail-closed rule from implementation plan §6 and
// returns a joined error listing ALL failures, never just the first one.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// Capacity.
	if c.Capacity.MaxRunners < 1 {
		fail("capacity.max_runners must be >= 1 (got %d)", c.Capacity.MaxRunners)
	}
	if c.Capacity.MinRunners > c.Capacity.MaxRunners {
		fail("capacity.min_runners (%d) must be <= capacity.max_runners (%d)", c.Capacity.MinRunners, c.Capacity.MaxRunners)
	}
	if c.Capacity.MaxRunners > HardCapMaxRunners {
		fail("capacity.max_runners (%d) exceeds the hard cap of %d", c.Capacity.MaxRunners, HardCapMaxRunners)
	}
	if c.Capacity.JobCPUQuotaPercent <= 0 || c.Capacity.JobCPUQuotaPercent > MaxCPUQuotaPercent {
		fail("capacity.job_cpu_quota_percent must be in (0, %d] (got %d)", MaxCPUQuotaPercent, c.Capacity.JobCPUQuotaPercent)
	}
	if c.Capacity.JobMemoryMax == "" {
		fail("capacity.job_memory_max must not be empty")
	}

	// GitHub App credentials.
	if c.GitHub.App.InstallationID <= 0 {
		fail("github.app.installation_id must be > 0 (got %d)", c.GitHub.App.InstallationID)
	}
	if c.GitHub.App.ClientID == "" {
		fail("github.app.client_id must not be empty")
	}
	if c.GitHub.App.PrivateKeyPath == "" {
		fail("github.app.private_key_path must not be empty")
	} else if _, err := os.Stat(c.GitHub.App.PrivateKeyPath); err != nil {
		fail("github.app.private_key_path %q is not readable: %v", c.GitHub.App.PrivateKeyPath, err)
	}

	// Scope.
	switch c.GitHub.Scope.Kind {
	case "organization":
	case "repository":
		if c.GitHub.Scope.Repository == "" {
			fail("github.scope.repository must be set when scope.kind is %q", c.GitHub.Scope.Kind)
		}
	default:
		fail("github.scope.kind must be %q or %q (got %q)", "organization", "repository", c.GitHub.Scope.Kind)
	}
	if c.GitHub.Scope.Owner == "" {
		fail("github.scope.owner must not be empty")
	}

	// Scale set.
	if !labelRe.MatchString(c.ScaleSet.Name) {
		fail("scale_set.name %q is not a valid Actions label (must match %s)", c.ScaleSet.Name, labelRe.String())
	}

	// Runner payload. An unset version means "track the latest
	// release": the daemon resolves the version and its asset digest
	// from the GitHub releases API at startup (plan §10). A pinned
	// sha256 requires a pinned version — a digest cannot constrain a
	// version that moves with every release.
	dynamicVersion := c.Runner.Version == ""
	if !dynamicVersion && !versionRe.MatchString(c.Runner.Version) {
		fail("runner.version %q must look like X.Y.Z (e.g. 2.328.0), or be unset to track the latest release", c.Runner.Version)
	}
	if dynamicVersion && c.Runner.SHA256 != "" {
		fail("runner.sha256 cannot be pinned while runner.version tracks the latest release; set runner.version too")
	}
	if !dynamicVersion && os.Getenv(AllowUnverifiedPayloadEnv) != "1" {
		if !sha256Re.MatchString(c.Runner.SHA256) {
			fail("runner.sha256 must be a 64-character hex digest (got %d characters); set %s=1 to allow an unverified payload", len(c.Runner.SHA256), AllowUnverifiedPayloadEnv)
		}
	}
	if c.Runner.EnvironmentFile == "" {
		fail("runner.environment_file must not be empty")
	} else if _, err := os.Stat(c.Runner.EnvironmentFile); err != nil {
		fail("runner.environment_file %q does not exist: %v", c.Runner.EnvironmentFile, err)
	} else if vars, err := env.ParseFile(c.Runner.EnvironmentFile); err != nil {
		fail("runner.environment_file %q is not a valid environment file: %v", c.Runner.EnvironmentFile, err)
	} else if err := env.Validate(vars); err != nil {
		fail("runner.environment_file %q: %v", c.Runner.EnvironmentFile, err)
	}

	// Runtime backend.
	switch c.Runtime.Backend {
	case "systemd":
		if _, err := os.Stat(systemdRuntimeDir); err != nil {
			fail("runtime.backend %q requires %s (systemd), which is not present: %v", c.Runtime.Backend, systemdRuntimeDir, err)
		}
	case "process":
	default:
		fail("runtime.backend must be %q or %q (got %q)", "systemd", "process", c.Runtime.Backend)
	}

	// Paths.
	if c.Paths.StateDir == "" {
		fail("paths.state_dir must not be empty")
	}
	if c.Paths.CacheDir == "" {
		fail("paths.cache_dir must not be empty")
	}
	if c.Paths.LogDir == "" {
		fail("paths.log_dir must not be empty")
	}

	// Timeouts.
	if c.Runtime.SlotStartTimeout <= 0 {
		fail("runtime.slot_start_timeout must be > 0 (got %s)", c.Runtime.SlotStartTimeout)
	}
	if c.Runtime.SlotStopTimeout <= 0 {
		fail("runtime.slot_stop_timeout must be > 0 (got %s)", c.Runtime.SlotStopTimeout)
	}
	if c.Runtime.CleanupTimeout <= 0 {
		fail("runtime.cleanup_timeout must be > 0 (got %s)", c.Runtime.CleanupTimeout)
	}
	if c.Runtime.AcquireGrace <= 0 {
		fail("runtime.acquire_grace must be > 0 (got %s)", c.Runtime.AcquireGrace)
	}

	// Scaling (admission gate). Checked only when the gate is enabled:
	// a zero Scaling struct means the caller built the config directly
	// (tests) and Load's defaults never ran.
	if c.Scaling.AdmissionControl {
		if c.Scaling.CPUTargetPercent <= 0 || c.Scaling.CPUTargetPercent > 100 {
			fail("scaling.cpu_target_percent must be in (0, 100] (got %d)", c.Scaling.CPUTargetPercent)
		}
		if c.Scaling.MemoryMarginPercent < 0 || c.Scaling.MemoryMarginPercent > 90 {
			fail("scaling.memory_margin_percent must be in [0, 90] (got %d)", c.Scaling.MemoryMarginPercent)
		}
		if c.Scaling.SampleInterval <= 0 {
			fail("scaling.sample_interval must be > 0 (got %s)", c.Scaling.SampleInterval)
		}
	}

	// Observability.
	if _, _, err := net.SplitHostPort(c.Observability.Listen); err != nil {
		fail("observability.listen %q is not a valid host:port: %v", c.Observability.Listen, err)
	}
	switch c.Observability.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("observability.log_level must be one of debug, info, warn, error (got %q)", c.Observability.LogLevel)
	}

	return errors.Join(errs...)
}

// EnsureDirs creates the daemon's directory tree: state, cache, logs,
// the slot and template subdirectories, and the runtime JIT dir (tmpfs
// in production). All directories are created 0755 with MkdirAll and
// then probed for writability (plan §6: "state/cache directories
// writable" — MkdirAll alone succeeds on existing read-only dirs).
func (c *Config) EnsureDirs() error {
	dirs := []string{
		c.Paths.StateDir,
		c.Paths.CacheDir,
		c.Paths.LogDir,
		filepath.Join(c.Paths.StateDir, "slots"),
		filepath.Join(c.Paths.StateDir, "template"),
		c.Runtime.JitDir,
	}
	for _, d := range dirs {
		if d == "" {
			return fmt.Errorf("cannot create directory: empty path")
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", d, err)
		}
		if err := probeWritable(d); err != nil {
			return fmt.Errorf("directory %s is not writable: %w", d, err)
		}
	}
	return nil
}

// probeWritable verifies the daemon can create a file in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
