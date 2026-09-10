// Package systemd implements runner.Backend with transient systemd units
// created via systemd-run(1), plus sd_notify(3) readiness signalling for
// the daemon's own unit.
//
// Every knob (user, quotas, hardening properties) is passed to systemd-run
// as -p properties, so values come from configuration rather than a
// static unit file. The systemd-run and systemctl binaries are exec'd
// (overridable for tests); there is no D-Bus dependency.
package systemd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xinterface/tentacles/internal/env"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/slot"
)

// Options configures the systemd backend.
type Options struct {
	// SystemdRunBin is the systemd-run binary; empty defaults to
	// "systemd-run". Tests inject a fake via PATH.
	SystemdRunBin string
	// SystemctlBin is the systemctl binary; empty defaults to
	// "systemctl". Tests inject a fake via PATH.
	SystemctlBin string
	// Log receives structured backend diagnostics; nil defaults to
	// slog.Default().
	Log *slog.Logger
	// StopTimeout is the TimeoutStopSec value set on each slot unit.
	StopTimeout time.Duration
}

// runnerCacheSubdirs are the HOME-relative shared cache locations jobs
// may write despite ProtectHome=read-only. These exceptions let the runner
// use the host's toolchains and shared caches.
var runnerCacheSubdirs = []string{".cache", ".local/share/mise", "go/pkg/mod"}

// Backend starts, stops, and waits on transient tentacle-<id>.service
// units. All methods are safe for concurrent use.
type Backend struct {
	systemdRunBin string
	systemctlBin  string
	log           *slog.Logger
	stopTimeout   time.Duration

	mu      sync.Mutex
	started map[string]struct{} // units this backend has started
}

// New returns a Backend with the given options and binary defaults.
func New(opts Options) *Backend {
	runBin := opts.SystemdRunBin
	if runBin == "" {
		runBin = "systemd-run"
	}
	ctlBin := opts.SystemctlBin
	if ctlBin == "" {
		ctlBin = "systemctl"
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 30 * time.Second
	}
	return &Backend{
		systemdRunBin: runBin,
		systemctlBin:  ctlBin,
		log:           log,
		stopTimeout:   opts.StopTimeout,
		started:       make(map[string]struct{}),
	}
}

// Start launches run.sh in the slot described by spec as a transient unit.
// With Type=exec, systemd-run blocks until the service has exec'd, so a
// nil return means the runner process is running (or at least has forked).
func (b *Backend) Start(ctx context.Context, spec runner.Spec) (retErr error) {
	attempted := false
	defer func() {
		if retErr != nil && !attempted {
			retErr = fmt.Errorf("%w: %w", runner.ErrNotStarted, retErr)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if spec.User == "" {
		return fmt.Errorf("systemd: runner user is required")
	}
	identity, err := runner.ResolveIdentity(spec)
	if err != nil {
		return err
	}
	if identity.UID == 0 {
		return fmt.Errorf("systemd: runner user must be unprivileged")
	}
	if !validUnit(spec.UnitName) {
		return fmt.Errorf("systemd: invalid slot unit %q", spec.UnitName)
	}
	if !filepath.IsAbs(spec.SlotDir) || !filepath.IsAbs(spec.JITPath) {
		return fmt.Errorf("systemd: slot and JIT paths must be absolute")
	}
	vars := map[string]string{}
	if spec.EnvFile != "" {
		if !filepath.IsAbs(spec.EnvFile) {
			return fmt.Errorf("systemd: environment file must be absolute")
		}
		vars, err = env.ParseFile(spec.EnvFile)
		if err != nil {
			return fmt.Errorf("systemd: parse environment file: %w", err)
		}
	}
	if _, err := runner.ReadJIT(spec.JITPath); err != nil {
		return fmt.Errorf("systemd: JIT source: %w", err)
	}
	home := identity.Home
	if value, ok := vars["HOME"]; ok {
		home = value
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("systemd: runner HOME must be absolute")
	}
	caches := cachePaths(home)
	// Create caches as the job identity: no privileged traversal or chown of
	// directories a previous job can replace with symlinks.
	mkdirArgs := append([]string{"-p", "--"}, caches...)
	mkdir := runner.CommandContext(ctx, "/bin/mkdir", mkdirArgs...)
	mkdir.SysProcAttr.Credential = identity.Credential
	if out, err := mkdir.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd: prepare runner caches: %w: %s", err, tail(out))
	}
	if err := runner.PrepareSlot(ctx, spec, identity); err != nil {
		return err
	}
	args := b.startArgs(spec, caches...)
	cmd := runner.CommandContext(ctx, b.systemdRunBin, args...)
	attempted = true
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("systemd: start %s: %w: %s", spec.UnitName, err, tail(out))
	}
	b.mu.Lock()
	b.started[spec.UnitName] = struct{}{}
	b.mu.Unlock()
	b.log.Debug("slot unit started", "unit", spec.UnitName)
	return nil
}

// startArgs builds the exact systemd-run argument vector for a slot,
// ending in a static shell that reads systemd's private credential copy.
func (b *Backend) startArgs(spec runner.Spec, caches ...string) []string {
	args := []string{
		"--collect",
		"--expand-environment=no",
		"--unit", spec.UnitName,
		"--description", "GitHub Actions runner slot",
		"-p", "Type=exec",
	}
	addProp := func(key, value string) {
		if value != "" {
			args = append(args, "-p", key+"="+value)
		}
	}
	addProp("User", spec.User)
	addProp("Group", spec.Group)
	addProp("WorkingDirectory", spec.SlotDir)
	addProp("EnvironmentFile", spec.EnvFile)
	addProp("LoadCredential", "jit:"+spec.JITPath)
	addProp("CPUQuota", spec.CPUQuota)
	addProp("MemoryMax", spec.MemoryMax)
	paths := append([]string{spec.SlotDir, "/tmp"}, caches...)
	for i, path := range paths {
		paths[i] = quotePath(path)
	}
	args = append(args,
		"-p", "Nice=5",
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec="+strconv.FormatFloat(b.stopTimeout.Seconds(), 'f', -1, 64),
		"-p", "TasksMax=4096",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		"-p", "CPUAccounting=yes",
		"-p", "MemoryAccounting=yes",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=read-only",
		"-p", "ReadWritePaths="+strings.Join(paths, " "),
		"-p", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"-p", "LockPersonality=yes",
		"/bin/sh", "-c", runner.CredentialScript(),
	)
	return args
}

// systemd-run passes literal path values over D-Bus; systemd itself escapes
// percent specifiers when persisting the unit. Doubling them here would change
// the actual pathname. The list parser unquotes but does not decode C escapes.
func quotePath(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func cachePaths(home string) []string {
	paths := make([]string, 0, len(runnerCacheSubdirs))
	for _, sub := range runnerCacheSubdirs {
		paths = append(paths, filepath.Join(home, sub))
	}
	return paths
}

func validUnit(unit string) bool {
	if !strings.HasPrefix(unit, "tentacle-") || !strings.HasSuffix(unit, ".service") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "tentacle-"), ".service")
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Stop terminates the unit and returns once it is gone. The started set
// is left untouched here; it is reconciled by the next Active scan.
func (b *Backend) Stop(ctx context.Context, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, b.stopTimeout+10*time.Second)
	defer cancel()
	if !validUnit(unit) {
		return fmt.Errorf("systemd: invalid slot unit %q", unit)
	}
	cmd := runner.CommandContext(ctx, b.systemctlBin, "stop", unit)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("systemd: stop %s: %w: %s", unit, err, tail(out))
	}
	b.log.Debug("slot unit stopped", "unit", unit)
	return nil
}

// Wait confirms exit only after a successful, well-formed state query.
// Communication errors and malformed output never establish that a job ended.
func (b *Backend) Wait(ctx context.Context, unit string) error {
	if !validUnit(unit) {
		return fmt.Errorf("systemd: invalid slot unit %q", unit)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := b.isActive(ctx, unit)
		if err != nil {
			return err
		}
		if state == "inactive" || state == "failed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *Backend) isActive(ctx context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := runner.CommandContext(ctx, b.systemctlBin, "show", unit, "--property=LoadState", "--property=ActiveState")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("systemd: observe %s: %w: %s", unit, err, tail(stderr.Bytes()))
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		_, duplicate := values[key]
		if !ok || (key != "LoadState" && key != "ActiveState") || duplicate {
			return "", fmt.Errorf("systemd: malformed state observation for %s", unit)
		}
		values[key] = value
	}
	switch values["LoadState"] {
	case "loaded", "not-found", "error", "masked", "bad-setting", "merged", "stub":
	default:
		return "", fmt.Errorf("systemd: unknown load state for %s", unit)
	}
	state := values["ActiveState"]
	switch state {
	case "active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing":
		return state, nil
	default:
		return "", fmt.Errorf("systemd: unknown active state for %s", unit)
	}
}

// Active lists the running tentacle-*.service units on the host (used for
// boot adoption). Command errors — e.g. no systemd running — are returned
// honestly so the caller can decide how to treat them. The scan also
// prunes the started set to units that are still around.
func (b *Backend) Active(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := runner.CommandContext(ctx, b.systemctlBin,
		"list-units", "tentacle-*.service", "--no-legend", "--plain", "--no-pager", "--full", "--all")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemd: list-units: %w: %s", err, tail(stderr.Bytes()))
	}
	units := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 || !validUnit(fields[0]) {
			return nil, fmt.Errorf("systemd: malformed list-units output")
		}
		switch fields[2] {
		case "active", "activating", "deactivating", "reloading", "maintenance", "refreshing", "inactive", "failed":
			// Include inactive/failed units as well: adoption must clean their slot
			// directories through the same confirmed-exit path as running units.
			units = append(units, fields[0])
		default:
			return nil, fmt.Errorf("systemd: unknown list-units state %q", fields[2])
		}
	}
	b.mu.Lock()
	for unit := range b.started {
		if !slices.Contains(units, unit) {
			delete(b.started, unit)
		}
	}
	b.mu.Unlock()
	return units, nil
}

// Usage reads cumulative CPU time and peak memory for a unit from
// systemd's accounting (CPUAccounting=yes and MemoryAccounting=yes are
// set on every slot unit). The Table samples this while a job runs so
// usage can be attributed at exit, before systemd garbage-collects the
// unit.
func (b *Backend) Usage(unit string) (slot.Usage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := runner.CommandContext(ctx, b.systemctlBin, "show", unit,
		"--property=CPUUsageNSec", "--property=MemoryPeak", "--property=MemoryCurrent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return slot.Usage{}, fmt.Errorf("systemd: show %s: %w: %s", unit, err, tail(out))
	}
	var u slot.Usage
	for _, line := range strings.Split(string(out), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch name {
		case "CPUUsageNSec":
			ns, err := strconv.ParseInt(value, 10, 64)
			if err == nil && ns > 0 {
				u.CPUSeconds = float64(ns) / 1e9
			}
		case "MemoryCurrent":
			if bytes, err := strconv.ParseUint(value, 10, 64); err == nil {
				u.CurrentMemBytes = bytes
			}
		case "MemoryPeak":
			if bytes, err := strconv.ParseUint(value, 10, 64); err == nil {
				u.PeakMemBytes = bytes
			}
		}
	}
	return u, nil
}

// tail returns the last ~2KB of combined command output for error
// messages, trimmed of surrounding whitespace.
func tail(out []byte) string {
	const maxTail = 2 * 1024
	if len(out) > maxTail {
		out = out[len(out)-maxTail:]
	}
	return strings.TrimSpace(string(out))
}
