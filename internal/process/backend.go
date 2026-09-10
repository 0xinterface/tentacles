// Package process implements a runner.Backend that starts the official
// runner as a direct child process. It is the fallback backend for tests
// and development hosts without systemd.
package process

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0xinterface/tentacles/internal/env"
	"github.com/0xinterface/tentacles/internal/runner"
)

// defaultStopTimeout is how long Stop waits after SIGTERM before escalating
// to SIGKILL.
const defaultStopTimeout = 30 * time.Second

// Options configures the backend.
type Options struct {
	// EnvFile is a systemd EnvironmentFile-format file layered over the
	// daemon environment for every started runner. Empty disables it.
	EnvFile string
	// Log receives structured lifecycle events. Nil means discard.
	Log *slog.Logger
}

// Option tweaks backend behavior.
type Option func(*options)

type options struct {
	stopTimeout time.Duration
}

// WithStopTimeout overrides the default 30s SIGTERM grace period used by
// Stop before it escalates to SIGKILL.
func WithStopTimeout(d time.Duration) Option {
	return func(o *options) { o.stopTimeout = d }
}

// unit is one started runner process.
type unit struct {
	cmd    *exec.Cmd
	exited chan struct{} // closed by the Wait goroutine once the process is gone
}

// Backend starts and stops runner processes. It is safe for concurrent use.
type Backend struct {
	envVars map[string]string
	envErr  error
	log     *slog.Logger
	stop    time.Duration

	mu       sync.Mutex
	units    map[string]*unit
	starting chan struct{}
}

// New creates a backend. The environment file, if any, is parsed once here;
// a parse error is stored and returned by Start.
func New(opts Options, extra ...Option) *Backend {
	o := options{stopTimeout: defaultStopTimeout}
	for _, fn := range extra {
		fn(&o)
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	b := &Backend{
		log:      log,
		stop:     o.stopTimeout,
		units:    make(map[string]*unit),
		starting: make(chan struct{}, 1),
	}
	if opts.EnvFile != "" {
		b.envVars, b.envErr = env.ParseFile(opts.EnvFile)
	}
	return b
}

// Start forks run.sh for spec in a new process group and registers it under
// spec.UnitName. It returns once the process has been exec'd, not when the
// runner is idle.
func (b *Backend) Start(ctx context.Context, spec runner.Spec) (retErr error) {
	attempted := false
	defer func() {
		if retErr != nil && !attempted {
			retErr = fmt.Errorf("%w: %w", runner.ErrNotStarted, retErr)
		}
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(spec.UnitName) == "" {
		return errors.New("process: unit name must not be empty")
	}
	if b.envErr != nil {
		return fmt.Errorf("process: parse environment file: %w", b.envErr)
	}
	envVars := b.envVars
	if spec.EnvFile != "" {
		var err error
		envVars, err = env.ParseFile(spec.EnvFile)
		if err != nil {
			return fmt.Errorf("process: parse environment file: %w", err)
		}
	}
	identity, err := runner.ResolveIdentity(spec)
	if err != nil {
		return err
	}
	select {
	case b.starting <- struct{}{}:
		defer func() { <-b.starting }()
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	if old, ok := b.units[spec.UnitName]; ok {
		select {
		case <-old.exited:
		default:
			b.mu.Unlock()
			return fmt.Errorf("process: unit %q already running", spec.UnitName)
		}
	}
	b.mu.Unlock()
	// Keep the source private to the supervisor. The process backend has no
	// systemd credential store, so prepare a separate 0600 runner-owned copy.
	data, err := runner.ReadJIT(spec.JITPath)
	if err != nil {
		return fmt.Errorf("process: read JIT source: %w", err)
	}
	credential, err := os.CreateTemp(spec.SlotDir, ".jit-*")
	if err != nil {
		return err
	}
	credentialPath := credential.Name()
	started := false
	defer func() {
		if !started {
			_ = os.Remove(credentialPath)
		}
	}()
	if _, err := credential.Write(data); err != nil {
		_ = credential.Close()
		return err
	}
	if err := credential.Close(); err != nil {
		return err
	}
	if err := runner.PrepareSlot(ctx, spec, identity); err != nil {
		return err
	}
	spec.JITPath = credentialPath
	cmd := runner.BuildCommand(spec)
	cmd.Env = env.Apply(os.Environ(), envVars)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: identity.Credential}
	if err := ctx.Err(); err != nil {
		return err
	}
	attempted = true
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: process start unit %q: %w", runner.ErrNotStarted, spec.UnitName, err)
	}
	u := &unit{cmd: cmd, exited: make(chan struct{})}
	b.mu.Lock()
	b.units[spec.UnitName] = u
	b.mu.Unlock()
	started = true
	go func() {
		_ = cmd.Wait()
		// A wrapper exiting does not imply its children exited. End any remaining
		// group members before confirming the unit is gone to the slot table.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// SIGKILL delivery can precede actual exit (for example during disk I/O).
		// Retain the unit until every group member has stopped executing.
		b.waitForGroupExit(cmd.Process.Pid)
		_ = os.Remove(credentialPath)
		close(u.exited)
	}()
	b.log.Info("runner process started", "unit", spec.UnitName, "pid", cmd.Process.Pid, "slot_dir", spec.SlotDir)
	return nil
}

// Active lists the unit names whose processes have not exited yet.
func (b *Backend) Active(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.units))
	for name, u := range b.units {
		select {
		case <-u.exited:
			// process gone; not active
		default:
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Wait blocks until the unit's process has exited. A unit that exited
// before Wait was called returns immediately. Unknown units are an error.
func (b *Backend) Wait(ctx context.Context, unitName string) error {
	u, err := b.lookup(unitName)
	if err != nil {
		return err
	}
	select {
	case <-u.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop terminates the unit's process group: SIGTERM first, then SIGKILL
// after the stop timeout, waiting for exit between and after. It returns
// once the process group is gone.
func (b *Backend) Stop(ctx context.Context, unitName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A missing registry entry proves no process was launched only after
	// any concurrent Start has finished registering its process.
	select {
	case b.starting <- struct{}{}:
		defer func() { <-b.starting }()
	case <-ctx.Done():
		return ctx.Err()
	}
	u, err := b.lookup(unitName)
	if err != nil {
		return nil
	}
	select {
	case <-u.exited:
		return nil
	default:
	}

	pgid := u.cmd.Process.Pid // Setpgid makes the child its own group leader
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("process: SIGTERM unit %q: %w", unitName, err)
	}

	timer := time.NewTimer(b.stop)
	defer timer.Stop()
	select {
	case <-u.exited:
		return nil
	case <-timer.C:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return ctx.Err()
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("process: SIGKILL unit %q: %w", unitName, err)
	}
	select {
	case <-u.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) lookup(unitName string) (*unit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.units[unitName]
	if !ok {
		return nil, fmt.Errorf("unknown unit %q", unitName)
	}
	return u, nil
}

// waitForGroupExit ignores zombies, which cannot touch slot files and may
// remain under a container init that does not reap adopted descendants.
func (b *Backend) waitForGroupExit(pgid int) {
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := runner.CommandContext(ctx, "/bin/ps", "-axo", "pgid=,stat=")
		out, err := cmd.Output()
		cancel()
		if err == nil {
			alive, valid := false, true
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				fields := strings.Fields(line)
				if len(fields) != 2 {
					valid = false
					break
				}
				group, parseErr := strconv.Atoi(fields[0])
				if parseErr != nil {
					valid = false
					break
				}
				if group == pgid && !strings.HasPrefix(fields[1], "Z") {
					alive = true
				}
			}
			if valid && !alive {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}
