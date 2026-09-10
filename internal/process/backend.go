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
	"os/user"
	"runtime"
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

	mu    sync.Mutex
	units map[string]*unit
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
		log:   log,
		stop:  o.stopTimeout,
		units: make(map[string]*unit),
	}
	if opts.EnvFile != "" {
		b.envVars, b.envErr = env.ParseFile(opts.EnvFile)
	}
	return b
}

// Start forks run.sh for spec in a new process group and registers it under
// spec.UnitName. It returns once the process has been exec'd, not when the
// runner is idle.
func (b *Backend) Start(ctx context.Context, spec runner.Spec) error {
	if strings.TrimSpace(spec.UnitName) == "" {
		return errors.New("process: unit name must not be empty")
	}
	if b.envErr != nil {
		return fmt.Errorf("process: parse environment file: %w", b.envErr)
	}

	b.mu.Lock()
	if old, ok := b.units[spec.UnitName]; ok {
		select {
		case <-old.exited:
			// previous incarnation finished; the name may be reused
		default:
			b.mu.Unlock()
			return fmt.Errorf("process: unit %q already running", spec.UnitName)
		}
	}
	b.mu.Unlock()

	cmd := runner.BuildCommand(spec)
	cmd.Env = env.Apply(os.Environ(), b.envVars)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if runtime.GOOS == "linux" {
		if err := applyCredential(cmd, spec); err != nil {
			return fmt.Errorf("process: resolve credentials: %w", err)
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process: start unit %q: %w", spec.UnitName, err)
	}

	u := &unit{cmd: cmd, exited: make(chan struct{})}
	b.mu.Lock()
	b.units[spec.UnitName] = u
	b.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		close(u.exited)
	}()
	b.log.Info("runner process started", "unit", spec.UnitName, "pid", cmd.Process.Pid, "slot_dir", spec.SlotDir)
	return nil
}

// Active lists the unit names whose processes have not exited yet.
func (b *Backend) Active(ctx context.Context) ([]string, error) {
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
	u, err := b.lookup(unitName)
	if err != nil {
		return err
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

// applyCredential drops privileges for the child when the daemon runs as
// root and spec asks for a different unix user. Only meaningful on linux;
// the caller guards on runtime.GOOS.
func applyCredential(cmd *exec.Cmd, spec runner.Spec) error {
	if os.Getuid() != 0 || spec.User == "" {
		return nil
	}
	u, err := user.Lookup(spec.User)
	if err != nil {
		return fmt.Errorf("lookup user %q: %w", spec.User, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("parse uid %q for user %q: %w", u.Uid, spec.User, err)
	}
	gid, err := resolveGID(spec.Group, u)
	if err != nil {
		return err
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	return nil
}

func resolveGID(group string, u *user.User) (uint32, error) {
	if group == "" {
		return parseGID(u.Gid) // primary group of the resolved user
	}
	if gid, err := strconv.ParseUint(group, 10, 32); err == nil {
		return uint32(gid), nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, fmt.Errorf("lookup group %q: %w", group, err)
	}
	return parseGID(g.Gid)
}

func parseGID(s string) (uint32, error) {
	gid, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse gid %q: %w", s, err)
	}
	return uint32(gid), nil
}
