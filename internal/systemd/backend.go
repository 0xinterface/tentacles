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
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hkust/tentacles/internal/runner"
	"github.com/hkust/tentacles/internal/slot"
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

// lookupUser resolves a unix user; swapped in tests.
var lookupUser = user.Lookup

// runnerCacheSubdirs are the HOME-relative shared cache locations jobs
// may write (plan §12: "~/.cache, mise, go/pkg/mod are allowed and
// desirable") despite ProtectHome=read-only. Without them the runner
// user cannot use the host toolchain (acceptance criterion 8).
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
func (b *Backend) Start(ctx context.Context, spec runner.Spec) error {
	args := b.startArgs(spec)
	cmd := exec.CommandContext(ctx, b.systemdRunBin, args...)
	out, err := cmd.CombinedOutput()
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
// ending in the shell that reads the JIT config from its file so the
// secret never appears in a long-lived argv.
func (b *Backend) startArgs(spec runner.Spec) []string {
	args := []string{
		"--collect",
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
	addProp("CPUQuota", spec.CPUQuota)
	addProp("MemoryMax", spec.MemoryMax)
	paths := append([]string{spec.SlotDir, "/tmp"}, b.writableCachePaths(spec)...)
	args = append(args,
		"-p", "Nice=5",
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec="+strconv.Itoa(int(b.stopTimeout.Seconds())),
		"-p", "TasksMax=4096",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		"-p", "CPUAccounting=yes",
		"-p", "MemoryAccounting=yes",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=read-only",
		"-p", "ReadWritePaths="+strings.Join(paths, ":"),
		"-p", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"-p", "LockPersonality=yes",
		"/bin/sh", "-c", runner.JITScript(spec.JITPath), "--", spec.JITPath,
	)
	return args
}

// writableCachePaths returns the runner user's shared cache directories
// (plan §12) to exempt from ProtectHome=read-only, creating them first
// so they exist and — when the daemon runs as root — belong to the
// runner user (systemd auto-creates missing ReadWritePaths entries as
// root-owned, which the runner user then cannot write). Unresolvable
// users and uncreatable paths are skipped; the host bootstrap (plan
// §12) is the fallback.
func (b *Backend) writableCachePaths(spec runner.Spec) []string {
	if spec.User == "" {
		return nil
	}
	u, err := lookupUser(spec.User)
	if err != nil {
		b.log.Debug("runner user unresolvable; shared caches not writable", "user", spec.User, "err", err)
		return nil
	}
	var paths []string
	for _, sub := range runnerCacheSubdirs {
		p := filepath.Join(u.HomeDir, sub)
		if err := os.MkdirAll(p, 0o755); err != nil {
			b.log.Debug("runner cache dir unavailable", "path", p, "err", err)
			continue
		}
		b.chownIfRoot(p, u)
		paths = append(paths, p)
	}
	return paths
}

// chownIfRoot hands a pre-created cache directory to the runner user.
// Best-effort: failures are logged at debug and the path stays listed.
func (b *Backend) chownIfRoot(path string, u *user.User) {
	if os.Geteuid() != 0 {
		return
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return
	}
	if err := os.Chown(path, uid, gid); err != nil {
		b.log.Debug("cache dir chown failed", "path", path, "err", err)
	}
}

// Stop terminates the unit and returns once it is gone. The started set
// is left untouched here; it is reconciled by the next Active scan.
func (b *Backend) Stop(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, b.systemctlBin, "stop", unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd: stop %s: %w: %s", unit, err, tail(out))
	}
	b.log.Debug("slot unit stopped", "unit", unit)
	return nil
}

// Wait blocks until the unit has exited. On hosts whose systemctl lacks
// the wait verb (stderr "Unknown operation"), it falls back to polling
// is-active every 250ms until the unit reports inactive or failed.
func (b *Backend) Wait(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, b.systemctlBin, "wait", unit)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if !bytes.Contains(out, []byte("Unknown operation")) {
		return fmt.Errorf("systemd: wait %s: %w: %s", unit, err, tail(out))
	}
	b.log.Debug("systemctl wait unsupported, polling is-active", "unit", unit)
	return b.pollUntilGone(ctx, unit)
}

// pollUntilGone polls systemctl is-active until the unit is inactive or
// failed, or the context is done.
func (b *Backend) pollUntilGone(ctx context.Context, unit string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := b.isActive(ctx, unit)
		if err != nil {
			// CommandContext kills the subprocess on cancellation and
			// reports "signal: killed"; surface the real reason.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
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

// isActive returns the current systemd unit state. is-active exits
// non-zero for inactive/failed units but still prints the state, so the
// output is authoritative regardless of the exit status.
func (b *Backend) isActive(ctx context.Context, unit string) (string, error) {
	cmd := exec.CommandContext(ctx, b.systemctlBin, "is-active", unit)
	out, err := cmd.CombinedOutput()
	state := strings.TrimSpace(string(out))
	if state == "" {
		if err != nil {
			return "", fmt.Errorf("systemd: is-active %s: %w", unit, err)
		}
		return "", fmt.Errorf("systemd: is-active %s: empty output", unit)
	}
	return state, nil
}

// Active lists the running tentacle-*.service units on the host (used for
// boot adoption). Command errors — e.g. no systemd running — are returned
// honestly so the caller can decide how to treat them. The scan also
// prunes the started set to units that are still around.
func (b *Backend) Active(ctx context.Context) ([]string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, b.systemctlBin,
		"list-units", "tentacle-*.service", "--no-legend", "--plain", "--no-pager")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemd: list-units: %w: %s", err, tail(stderr.Bytes()))
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if name := fields[0]; strings.HasSuffix(name, ".service") {
			units = append(units, name)
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
	cmd := exec.CommandContext(ctx, b.systemctlBin, "show", unit,
		"--property=CPUUsageNSec", "--property=MemoryPeak")
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
