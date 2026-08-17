// Package app wires the daemon together: config, payload, scale-set
// listener, slot table, reconciler, metrics — and owns the run loop.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hkust/gh-runnerd/internal/config"
	"github.com/hkust/gh-runnerd/internal/logship"
	"github.com/hkust/gh-runnerd/internal/metrics"
	"github.com/hkust/gh-runnerd/internal/payload"
	"github.com/hkust/gh-runnerd/internal/process"
	"github.com/hkust/gh-runnerd/internal/reconcile"
	"github.com/hkust/gh-runnerd/internal/runner"
	"github.com/hkust/gh-runnerd/internal/scaleset"
	"github.com/hkust/gh-runnerd/internal/slot"
	"github.com/hkust/gh-runnerd/internal/systemd"
	"github.com/hkust/gh-runnerd/internal/version"
)

// Options are the process-level options from main. ScaleSet is a test
// seam: non-nil overrides the real adapter (used by integration tests).
type Options struct {
	ConfigPath string
	DryRun     bool
	ScaleSet   ScaleSet
}

// ScaleSet is the narrow surface app consumes from the scale-set adapter.
// *scaleset.Adapter satisfies it; tests substitute a fake.
type ScaleSet interface {
	EnsureScaleSet(ctx context.Context) error
	Run(ctx context.Context) error
	GenerateJIT(ctx context.Context, runnerName, workFolder string) (string, error)
	ScaleSetID() int
}

// Run loads the config, validates it, and — unless DryRun — runs the
// daemon until ctx is canceled. DryRun stops after validation.
func Run(ctx context.Context, opts Options) error {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if opts.DryRun {
		fmt.Printf("config %s valid (dry run)\n", opts.ConfigPath)
		return nil
	}
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("create state dirs: %w", err)
	}
	log := newLogger(cfg)
	log.Info("starting gh-runnerd",
		"version", version.String(),
		"scale_set", cfg.ScaleSet.Name,
		"backend", cfg.Runtime.Backend,
	)
	d, err := newDaemon(cfg, log, opts)
	if err != nil {
		return err
	}
	defer d.close()
	return d.run(ctx)
}

func newLogger(cfg *config.Config) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.Observability.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// desiredCell is the latest desired runner count pushed by the listener.
type desiredCell struct {
	mu sync.Mutex
	n  int
}

func (c *desiredCell) Store(n int) { c.mu.Lock(); c.n = n; c.mu.Unlock() }
func (c *desiredCell) Load() int   { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

type daemon struct {
	cfg     *config.Config
	log     *slog.Logger
	met     *metrics.Registry
	table   *slot.Table
	rec     *reconcile.Reconciler
	ss      ScaleSet
	backend runner.Backend
	httpSrv *http.Server
	desired *desiredCell
	reconCh chan struct{}
}

func newDaemon(cfg *config.Config, log *slog.Logger, opts Options) (*daemon, error) {
	d := &daemon{
		cfg:     cfg,
		log:     log,
		met:     metrics.NewRegistry(),
		desired: &desiredCell{},
		reconCh: make(chan struct{}, 1),
	}

	// Runner payload: download + verify + extract template.
	pm := payload.New(cfg.Paths.CacheDir, filepath.Join(cfg.Paths.StateDir, "template"), log)
	url := cfg.Runner.DownloadURL
	if url == "" {
		url = payload.DownloadURL(cfg.Runner.Version)
	}
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := pm.Ensure(pctx, cfg.Runner.Version, cfg.Runner.SHA256, url); err != nil {
		return nil, fmt.Errorf("ensure runner payload: %w", err)
	}

	// Events are wired before the scale set and table exist; the
	// closures dereference d.table lazily.
	requestReconcile := func() {
		select {
		case d.reconCh <- struct{}{}:
		default:
		}
	}
	ev := d.buildEvents(requestReconcile)

	if opts.ScaleSet != nil {
		d.ss = opts.ScaleSet
		// Test fakes receive the wired events so they can drive the daemon.
		if inj, ok := opts.ScaleSet.(interface{ SetEvents(scaleset.Events) }); ok {
			inj.SetEvents(ev)
		}
	} else {
		// Scale-set client (GitHub App auth).
		pem, err := os.ReadFile(cfg.GitHub.App.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read app private key: %w", err)
		}
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("hostname: %w", err)
		}
		// Upstream parseGitHubConfigFromURL rejects a bare github.com URL —
		// it must be scoped to the target (org or repo).
		ghURL := strings.TrimSuffix(cfg.GitHub.URL, "/") + "/" + cfg.GitHub.Scope.Owner
		if cfg.GitHub.Scope.Kind == "repository" && cfg.GitHub.Scope.Repository != "" {
			ghURL += "/" + cfg.GitHub.Scope.Repository
		}
		adapter, err := scaleset.New(scaleset.Config{
			GitHubURL:      ghURL,
			ClientID:       cfg.GitHub.App.ClientID,
			InstallationID: cfg.GitHub.App.InstallationID,
			PrivateKeyPEM:  string(pem),
			OwnerName:      cfg.GitHub.Scope.Owner,
			Repository:     cfg.GitHub.Scope.Repository,
			ScaleSetName:   cfg.ScaleSet.Name,
			RunnerGroup:    cfg.ScaleSet.RunnerGroup,
			ExtraLabels:    cfg.ScaleSet.ExtraLabels,
			MinRunners:     cfg.Capacity.MinRunners,
			MaxRunners:     cfg.Capacity.MaxRunners,
			DisableUpdate:  cfg.Runner.DisableUpdate,
			SystemVersion:  version.String(),
			SessionOwner:   hostname,
		}, ev, log.WithGroup("scaleset"))
		if err != nil {
			return nil, fmt.Errorf("create scale-set client: %w", err)
		}
		d.ss = adapter
	}

	// Provisioning backend.
	switch cfg.Runtime.Backend {
	case "systemd":
		d.backend = systemd.New(systemd.Options{
			Log:         log.WithGroup("systemd"),
			StopTimeout: cfg.Runtime.SlotStopTimeout,
		})
	default:
		d.backend = process.New(process.Options{
			EnvFile: cfg.Runner.EnvironmentFile,
			Log:     log.WithGroup("process"),
		})
	}

	// Slot table.
	group := cfg.Runner.Group
	if group == "" {
		group = cfg.Runner.User
	}
	d.table = slot.NewTable(
		filepath.Join(cfg.Paths.StateDir, "slots"),
		d.backend,
		d.materializeSlot(pm),
		d.mintJIT,
		cfg.Runtime.JitDir,
		log.WithGroup("slot"),
		slot.WithAcquireGrace(cfg.Runtime.AcquireGrace),
		slot.WithStopTimeout(cfg.Runtime.SlotStopTimeout),
		slot.WithCleanupTimeout(cfg.Runtime.CleanupTimeout),
		slot.WithStartTimeout(cfg.Runtime.SlotStartTimeout),
		slot.WithEnvFile(cfg.Runner.EnvironmentFile),
		slot.WithRunUser(cfg.Runner.User, group),
		slot.WithLimits(fmt.Sprintf("%d%%", cfg.Capacity.JobCPUQuotaPercent), cfg.Capacity.JobMemoryMax),
		slot.WithDiagHook(d.shipDiag),
		slot.WithEventHook(d.onSlotEvent),
	)
	d.rec = reconcile.New(d.table, cfg.Capacity.MinRunners, cfg.Capacity.MaxRunners, log.WithGroup("reconcile"))
	return d, nil
}

// buildEvents translates scale-set events into reconciler nudges, busy
// marks, and metrics. Scaling is statistics-driven only (plan §8).
func (d *daemon) buildEvents(requestReconcile func()) scaleset.Events {
	return scaleset.Events{
		Desired: func(n int) {
			d.desired.Store(n)
			requestReconcile()
		},
		JobStart: func(runnerName string) {
			d.met.IncJobsStarted()
			if d.table == nil {
				return
			}
			if d.table.MarkBusyByRunner(runnerName) {
				d.log.Info("runner busy", "runner_name", runnerName)
			} else {
				d.log.Warn("JobStarted for unknown runner", "runner_name", runnerName)
			}
		},
		JobEnd: func(runnerName, result string) {
			d.met.IncJobsCompleted(result)
			d.log.Info("job completed", "runner_name", runnerName, "result", result)
		},
		Session: func(err error) {
			if err != nil {
				d.met.IncListenerErrors()
				d.log.Error("listener session ended", "err", err)
			}
		},
	}
}

// materializeSlot copies the payload template into a fresh slot dir,
// timing the operation for the slot_start histogram. It refuses to
// materialize when the state filesystem is nearly full (plan §16).
func (d *daemon) materializeSlot(pm *payload.Manager) func(dst string) error {
	return func(dst string) error {
		// Disk watermark (plan §16): refuse new slots under 10% free on
		// the state filesystem; the listener stays up so capacity can drop.
		free, total, err := diskUsage(d.cfg.Paths.StateDir)
		if err == nil && total > 0 && free*10 < total {
			return fmt.Errorf("disk watermark: only %.1f%% free on %s", 100*float64(free)/float64(total), d.cfg.Paths.StateDir)
		}
		t0 := time.Now()
		if err := pm.CopySlot(dst); err != nil {
			return err
		}
		d.met.ObserveSlotStart(time.Since(t0).Seconds())
		return nil
	}
}

// mintJIT mints a JIT config named <scale-set>-<slot>-<rand> for the
// slot's work folder. The encoded value is a secret; it never gets logged.
func (d *daemon) mintJIT(ctx context.Context, id slot.ID) (runner.JIT, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return runner.JIT{}, fmt.Errorf("rand: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s", d.cfg.ScaleSet.Name, id, hex.EncodeToString(b[:]))
	work := filepath.Join(d.cfg.Paths.StateDir, "slots", string(id), d.cfg.Runner.WorkDirectory)
	t0 := time.Now()
	encoded, err := d.ss.GenerateJIT(ctx, name, work)
	if err != nil {
		return runner.JIT{}, fmt.Errorf("generate jit: %w", err)
	}
	d.met.ObserveSlotStart(time.Since(t0).Seconds())
	return runner.JIT{Encoded: encoded, RunnerName: name}, nil
}

func (d *daemon) shipDiag(s slot.Slot) error {
	if !d.cfg.Observability.ShipDiag {
		return nil
	}
	dest, err := logship.ShipDiag(s.Dir, d.cfg.Paths.LogDir, s.RunnerName)
	if err != nil {
		return err
	}
	if dest != "" {
		d.log.Info("shipped runner diagnostics", "slot", s.ID, "dest", dest)
	}
	return nil
}

func (d *daemon) onSlotEvent(event string, s slot.Slot) {
	switch event {
	case "acquire_failure":
		d.met.IncAcquireFailures()
		d.log.Warn("slot acquire failure", "slot", s.ID, "runner_name", s.RunnerName)
	case "started":
		d.log.Info("slot started", "slot", s.ID, "runner_name", s.RunnerName, "unit", s.Unit)
	case "exited":
		d.log.Info("slot exited", "slot", s.ID, "runner_name", s.RunnerName, "unit", s.Unit)
	case "stopped":
		d.log.Info("slot stopped", "slot", s.ID, "runner_name", s.RunnerName, "unit", s.Unit)
	}
}

func (d *daemon) run(ctx context.Context) error {
	// Metrics endpoint.
	mux := http.NewServeMux()
	mux.Handle("/metrics", d.met.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	d.httpSrv = &http.Server{Addr: d.cfg.Observability.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", d.httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("listen metrics: %w", err)
	}
	go func() {
		if err := d.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("metrics server failed", "err", err)
		}
	}()
	defer d.httpSrv.Shutdown(context.WithoutCancel(ctx))

	// Scale set must exist before we can listen or mint JITs.
	if err := d.ss.EnsureScaleSet(ctx); err != nil {
		return fmt.Errorf("ensure scale set: %w", err)
	}
	d.log.Info("scale set ready", "scale_set", d.cfg.ScaleSet.Name, "scale_set_id", d.ss.ScaleSetID())

	// Boot adoption: reconcile leftover units and slot dirs (plan §13).
	if units, err := d.backend.Active(ctx); err != nil {
		d.log.Warn("cannot list active units; skipping boot adoption", "err", err)
	} else if err := d.table.Adopt(ctx, units); err != nil {
		d.log.Error("boot adoption failed", "err", err)
	}

	// Listener supervisor with backoff (plan §8: 1s, 2s, 5s, 15s, 30s cap).
	// Run blocks; it returns on error or ctx cancel and never deletes the
	// scale set. Session close on graceful exit is the adapter's defer.
	backoff := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		for i := 0; ; i++ {
			err := d.ss.Run(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				d.met.IncListenerErrors()
				d.log.Error("listener run failed", "err", err)
			}
			wait := backoff[min(i, len(backoff)-1)]
			d.log.Warn("listener restarting", "backoff", wait.String())
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
	}()

	// Type=notify readiness (plan §11): config validated, payload
	// verified/extracted, scale set ensured, boot adoption done, listener
	// session goroutine running. No-op without NOTIFY_SOCKET.
	if err := systemd.SdNotify("READY=1"); err != nil {
		d.log.Warn("sd_notify failed", "err", err)
	}

	// Metrics state sync.
	syncStop := make(chan struct{})
	go func() {
		defer close(syncStop)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.met.SetDesired(d.table.Desired())
				counts := map[slot.State]int{}
				for _, s := range d.table.Snapshot() {
					counts[s.State]++
				}
				for _, st := range []slot.State{slot.StateEmpty, slot.StateStarting, slot.StateIdle, slot.StateBusy, slot.StateStopping, slot.StateFailed} {
					d.met.SetActual(string(st), counts[st])
				}
			}
		}
	}()

	// Reconcile loop; also drives the warm pool at boot (min_runners).
	d.desired.Store(d.cfg.Capacity.MinRunners)
	d.requestReconcile()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("shutting down")
			d.shutdownSlots()
			<-listenerDone
			<-syncStop
			return nil
		case <-d.reconCh:
			n := d.desired.Load()
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := d.rec.Reconcile(rctx, n); err != nil {
				d.log.Error("reconcile failed", "desired", n, "err", err)
			}
			cancel()
		}
	}
}

func (d *daemon) requestReconcile() {
	select {
	case d.reconCh <- struct{}{}:
	default:
	}
}

// shutdownSlots stops idle/starting slots; busy runners finish their job
// (one-job runners exit naturally; boot adoption covers the rest).
func (d *daemon) shutdownSlots() {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), d.cfg.Runtime.SlotStopTimeout)
	defer cancel()
	if err := d.table.Ensure(sctx, 0); err != nil {
		d.log.Error("stopping idle slots on shutdown", "err", err)
	}
}

func (d *daemon) close() {
	if d.table != nil {
		d.table.Close()
	}
}

// diskUsage reports free and total bytes on the filesystem holding p.
// It returns an error on unsupported platforms; callers treat failure as
// "cannot check" and proceed.
func diskUsage(p string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
