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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0xinterface/tentacles/internal/config"
	"github.com/0xinterface/tentacles/internal/history"
	"github.com/0xinterface/tentacles/internal/logship"
	"github.com/0xinterface/tentacles/internal/metrics"
	"github.com/0xinterface/tentacles/internal/payload"
	"github.com/0xinterface/tentacles/internal/process"
	"github.com/0xinterface/tentacles/internal/reconcile"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/scaleset"
	"github.com/0xinterface/tentacles/internal/slot"
	"github.com/0xinterface/tentacles/internal/systemd"
	"github.com/0xinterface/tentacles/internal/version"
)

type Options struct {
	ConfigPath string
	DryRun     bool
	ScaleSet   ScaleSet

	// releasesURL overrides the releases API endpoint used to resolve
	// an unset runner.version; tests inject a fake server. Empty means
	// payload.ReleasesAPIURL.
	releasesURL string
	// numCPU and memAvailable are the host budget probes behind the
	// admission gate; tests inject fixed values.
	numCPU       func() int
	memAvailable func() (uint64, error)
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
	log.Info("starting tentacles",
		"version", version.String(),
		"scale_set", cfg.ScaleSet.Name,
		"backend", cfg.Runtime.Backend,
	)
	d, err := newDaemonContext(ctx, cfg, log, opts)
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

// defaultReconcileInterval is the safety-net reconcile tick. Events nudge
// reconcile immediately; the tick retries failed starts and replenishes
// the warm pool even if an event is lost.
const defaultReconcileInterval = 30 * time.Second

// sd_notify seam; swapped in tests to observe readiness signalling.
var sdNotify = systemd.SdNotify

type daemon struct {
	diagSem           chan struct{}
	cfg               *config.Config
	log               *slog.Logger
	met               *metrics.Registry
	table             *slot.Table
	rec               *reconcile.Reconciler
	ss                ScaleSet
	backend           runner.Backend
	httpSrv           *http.Server
	desired           *desiredCell
	reconCh           chan struct{}
	reconcileInterval time.Duration
	sessionUp         chan struct{}
	sessionOnce       sync.Once

	hist         *history.Store
	queuedRefs   map[string]int
	queuedAt     map[string]time.Time
	retryMu      sync.Mutex
	retryAfter   time.Time
	retryDelay   time.Duration
	queuedMu     sync.Mutex
	numCPU       func() int
	memAvailable func() (uint64, error)
}

func newDaemon(cfg *config.Config, log *slog.Logger, opts Options) (*daemon, error) {
	return newDaemonContext(context.Background(), cfg, log, opts)
}

func newDaemonContext(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) (*daemon, error) {
	identity, err := runner.ResolveIdentity(runner.Spec{User: cfg.Runner.User, Group: cfg.Runner.Group})
	if err != nil {
		return nil, fmt.Errorf("runner identity: %w", err)
	}
	if cfg.Runtime.Backend == config.BackendSystemd && identity.UID == 0 {
		return nil, errors.New("systemd jobs require a non-root runner user")
	}

	if cfg.Runtime.Backend == config.BackendSystemd && os.Geteuid() != 0 {
		return nil, errors.New("systemd backend requires the privileged supervisor service")
	}
	if cfg.Runtime.Backend == config.BackendSystemd {
		if err := systemd.CheckSupport(ctx); err != nil {
			return nil, err
		}
	}
	d := &daemon{
		cfg:               cfg,
		diagSem:           make(chan struct{}, 1),
		log:               log,
		met:               metrics.NewRegistry(),
		desired:           &desiredCell{},
		reconCh:           make(chan struct{}, 1),
		reconcileInterval: defaultReconcileInterval,
		sessionUp:         make(chan struct{}),
		queuedRefs:        make(map[string]int),
		numCPU:            func() int { return runtime.NumCPU() },
		memAvailable:      procMemAvailable,
	}

	if opts.numCPU != nil {
		d.numCPU = opts.numCPU
	}
	if opts.memAvailable != nil {
		d.memAvailable = opts.memAvailable
	}

	// Runner payload: download + verify + extract template. An unset
	// runner.version tracks the latest release: resolve the version and
	// its asset digest from the releases API.
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	runnerVersion, sha := cfg.Runner.Version, cfg.Runner.SHA256
	if runnerVersion == "" {
		api := opts.releasesURL
		if api == "" {
			api = payload.ReleasesAPIURL
		}
		resolved, digest, err := payload.ResolveLatest(pctx, api)
		if err != nil {
			return nil, fmt.Errorf("resolve latest runner version: %w", err)
		}
		runnerVersion, sha = resolved, digest
		log.Info("runner version resolved from latest release", "runner_version", runnerVersion)
	}
	pm := payload.New(cfg.Paths.CacheDir, filepath.Join(cfg.Paths.StateDir, "template"), log)
	url := cfg.Runner.DownloadURL
	if url == "" {
		url = payload.DownloadURL(runnerVersion)
	}
	if err := pm.Ensure(pctx, runnerVersion, sha, url); err != nil {
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
	case config.BackendSystemd:
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

	// Per-workflow usage history (admission gate input). A store that
	// cannot be opened only disables admission control; runs continue.
	hist, err := history.Open(filepath.Join(cfg.Paths.StateDir, "history.jsonl"))
	if err != nil {
		log.Warn("usage history unavailable; admission control stays inert", "err", err)
		hist = nil
	}
	d.hist = hist

	// Slot table.
	group := cfg.Runner.Group
	tableOpts := []slot.TableOption{
		slot.WithAcquireGrace(cfg.Runtime.AcquireGrace),
		slot.WithStartAllowed(func() bool { return d.acquireRetryWait() == 0 }),
		slot.WithStopTimeout(cfg.Runtime.SlotStopTimeout),
		slot.WithCleanupTimeout(cfg.Runtime.CleanupTimeout),
		slot.WithStartTimeout(cfg.Runtime.SlotStartTimeout),
		slot.WithWorkDir(cfg.Runner.WorkDirectory),
		slot.WithStartObserver(func(dur time.Duration) { d.met.ObserveSlotStart(dur.Seconds()) }),
		slot.WithLimits(fmt.Sprintf("%d%%", cfg.Capacity.JobCPUQuotaPercent), cfg.Capacity.JobMemoryMax),
		slot.WithDiagContext(d.shipDiagContext),
		slot.WithRunUser(cfg.Runner.User, group),
		slot.WithEnvFile(cfg.Runner.EnvironmentFile),
		slot.WithMaterializer(d.materializeSlotContext(pm)),
		slot.WithEventHook(d.onSlotEvent),
		slot.WithSampleInterval(cfg.Scaling.SampleInterval),
	}
	if u, ok := d.backend.(interface {
		Usage(string) (slot.Usage, error)
	}); ok {
		tableOpts = append(tableOpts, slot.WithUsageSampler(u.Usage))
	}
	if cfg.Scaling.AdmissionControl && d.hist != nil {
		tableOpts = append(tableOpts, slot.WithGate(d.admissionGate), slot.WithReservation(d.candidateEstimate))
		tableOpts = append(tableOpts, slot.WithCompletionHook(func(c slot.Completion) {
			err := d.hist.Append(history.Record{
				Slot:             string(c.ID),
				WorkflowRef:      c.WorkflowRef,
				RunID:            c.RunID,
				CPUSeconds:       c.CPUSeconds,
				PeakMemBytes:     c.PeakMemBytes,
				WallSeconds:      c.WallSeconds,
				QueueWaitSeconds: c.QueueWaitSeconds,
				Sampled:          c.Sampled,
				At:               time.Now(),
			})
			if err != nil {
				log.Warn("usage history append failed", "err", err)
			}
			if c.Sampled {
				d.met.ObserveJobCPU(c.CPUSeconds)
			}
			if c.PeakMemBytes > 0 {
				d.met.SetJobPeakMem(c.PeakMemBytes)
			}
			d.met.ObserveJobWall(c.WallSeconds)
		}))
	}
	d.table = slot.NewTable(
		filepath.Join(cfg.Paths.StateDir, "slots"),
		d.backend,
		d.materializeSlot(pm),
		d.mintJIT,
		cfg.Runtime.JitDir,
		log.WithGroup("slot"),
		tableOpts...,
	)
	d.rec = reconcile.New(d.table, cfg.Capacity.MinRunners, cfg.Capacity.MaxRunners, log.WithGroup("reconcile"))
	// Seed desired with the warm-pool floor BEFORE the listener can
	// push; the listener's first Desired event overwrites it. Doing this
	// here (not in run) removes a race where the boot seed clobbered a
	// statistics push that had already landed.
	d.desired.Store(cfg.Capacity.MinRunners)
	return d, nil
}

// buildEvents translates scale-set events into reconciler nudges, busy
// marks, and metrics. Scaling is statistics-driven only.
func (d *daemon) buildEvents(requestReconcile func()) scaleset.Events {
	return scaleset.Events{
		SessionStarted: func() {
			// Gate sd_notify READY on the first established session;
			// later reconnects do not re-signal.
			d.sessionOnce.Do(func() { close(d.sessionUp) })
		},
		Desired: func(n int) {
			d.desired.Store(n)
			requestReconcile()
		},
		JobStart: func(job scaleset.Job) {
			d.met.IncJobsStarted()
			if d.table == nil {
				return
			}
			if d.table.Claim(slot.ClaimJob{
				RunnerName:       job.RunnerName,
				WorkflowRef:      job.WorkflowRef,
				RunID:            job.WorkflowRunID,
				QueueWaitSeconds: queueWait(job),
			}) {
				d.log.Info("runner busy", "runner_name", job.RunnerName, "workflow_ref", job.WorkflowRef)
				d.consumeQueuedRef(job.WorkflowRef)
			} else {
				d.log.Warn("JobStarted for unknown runner", "runner_name", job.RunnerName)
			}
		},
		JobEnd: func(job scaleset.Job) {
			d.met.IncJobsCompleted(job.Result)
			d.log.Info("job completed", "runner_name", job.RunnerName, "result", job.Result)
		},
		Queued: func(refs []string) {
			d.noteQueued(refs)
		},
		MessageID: func(id int64) {
			d.met.SetLastMessageID(id)
		},
		Session: func(err error) {
			if err != nil {
				d.met.IncListenerErrors()
				d.log.Error("listener session ended", "err", err)
			}
		},
	}
}

// queueWait is the GitHub-reported wait from queue to runner assignment.
func queueWait(job scaleset.Job) float64 {
	if job.QueueTime.IsZero() || job.RunnerAssignTime.IsZero() {
		return 0
	}
	w := job.RunnerAssignTime.Sub(job.QueueTime).Seconds()
	if w < 0 {
		return 0
	}
	return w
}

// noteQueued records workflow refs seen waiting in the scale-set queue
// (JobAvailable messages). Best-effort hints for the admission gate.
const maxQueuedRefs = 1024
const queuedHintTTL = 10 * time.Minute

func (d *daemon) noteQueued(refs []string) {
	d.queuedMu.Lock()
	defer d.queuedMu.Unlock()
	if d.queuedRefs == nil {
		d.queuedRefs = make(map[string]int)
	}
	if d.queuedAt == nil {
		d.queuedAt = make(map[string]time.Time)
	}
	now := time.Now()
	d.expireQueuedLocked(now)
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := d.queuedRefs[ref]; !ok && len(d.queuedRefs) >= maxQueuedRefs {
			continue
		}
		d.queuedRefs[ref] = min(d.queuedRefs[ref]+1, 9999)
		d.queuedAt[ref] = now
	}
}

func (d *daemon) expireQueuedLocked(now time.Time) {
	for ref, n := range d.queuedRefs {
		at := d.queuedAt[ref]
		if n <= 0 || (!at.IsZero() && now.Sub(at) > queuedHintTTL) {
			delete(d.queuedRefs, ref)
			delete(d.queuedAt, ref)
		}
	}
}

func (d *daemon) consumeQueuedRef(ref string) {
	d.queuedMu.Lock()
	defer d.queuedMu.Unlock()
	if d.queuedRefs[ref] <= 1 {
		delete(d.queuedRefs, ref)
		delete(d.queuedAt, ref)
	} else {
		d.queuedRefs[ref]--
	}
}

// GitHub selects the job after launch. Reserve the largest queued estimate
// in each dimension, falling back to history's global estimate when empty.
func (d *daemon) candidateEstimate() (float64, uint64) {
	if d.hist == nil {
		return 0, 0
	}
	d.queuedMu.Lock()
	d.expireQueuedLocked(time.Now())
	refs := make([]string, 0, len(d.queuedRefs))
	for ref := range d.queuedRefs {
		refs = append(refs, ref)
	}
	d.queuedMu.Unlock()
	if len(refs) == 0 {
		refs = append(refs, "")
	}
	var cores float64
	var mem uint64
	for _, ref := range refs {
		c, _ := d.hist.PredictCores(ref)
		m, _ := d.hist.PredictMem(ref)
		cores = max(cores, c)
		mem = max(mem, m)
	}
	return cores, mem
}

// Every live runner reserves capacity, including those still registering
// or waiting for assignment. MemAvailable already excludes resident memory;
// charge only each runner's predicted remaining growth against it.
func (d *daemon) admissionGate(live []slot.Slot) bool {
	if d.hist == nil || len(live) == 0 {
		return true
	}
	candidateCores, candidateMem := d.candidateEstimate()
	usedCores, usedMem := candidateCores, candidateMem
	for _, s := range live {
		if !s.State.Live() {
			continue
		}
		cores, mem := s.ReservedCores, s.ReservedMemBytes
		if s.State == slot.StateBusy {
			cores, _ = d.hist.PredictCores(s.WorkflowRef)
			mem, _ = d.hist.PredictMem(s.WorkflowRef)
		} else {
			// New queue hints may be heavier than the estimate at provisioning.
			cores = max(cores, candidateCores)
			mem = max(mem, candidateMem)
		}
		usedCores += cores
		if mem > s.CurrentMemBytes {
			usedMem += mem - s.CurrentMemBytes
		}
	}
	if usedCores > float64(d.numCPU())*float64(d.cfg.Scaling.CPUTargetPercent)/100 {
		return false
	}
	if avail, err := d.memAvailable(); err == nil {
		limit := uint64(float64(avail) * float64(100-d.cfg.Scaling.MemoryMarginPercent) / 100)
		if usedMem > limit {
			return false
		}
	}
	return true
}

// procMemAvailable reads MemAvailable from /proc/meminfo. Unsupported
// platforms return an error; the gate then skips the memory check.
func procMemAvailable() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, errors.New("meminfo: MemAvailable not found")
}

// materializeSlot copies the payload template into a fresh slot dir. It
// refuses to materialize when the state filesystem is nearly full.
// The Table times the whole successful start for the slot_start histogram.
func (d *daemon) materializeSlot(pm *payload.Manager) func(dst string) error {
	return func(dst string) error { return d.materializeSlotContext(pm)(context.Background(), dst) }
}

func (d *daemon) materializeSlotContext(pm *payload.Manager) func(context.Context, string) error {
	return func(ctx context.Context, dst string) error {
		// Refuse new slots under 10% free on the state filesystem.
		// The listener stays up while local provisioning is held.
		free, total, err := diskUsage(d.cfg.Paths.StateDir)
		if err == nil && total > 0 && free*10 < total {
			return fmt.Errorf("disk watermark: only %.1f%% free on %s", 100*float64(free)/float64(total), d.cfg.Paths.StateDir)
		}
		return pm.CopySlotIntoContext(ctx, dst)
	}
}

// mintJIT mints a JIT config named <scale-set>-<slot>-<rand> for the
// slot's work folder (the Table owns that path). The encoded value is a
// secret; it never gets logged.
func (d *daemon) mintJIT(ctx context.Context, id slot.ID) (runner.JIT, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return runner.JIT{}, fmt.Errorf("rand: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s", d.cfg.ScaleSet.Name, id, hex.EncodeToString(b[:]))
	encoded, err := d.ss.GenerateJIT(ctx, name, d.table.WorkDir(id))
	if err != nil {
		return runner.JIT{}, fmt.Errorf("generate jit: %w", err)
	}
	return runner.JIT{Encoded: encoded, RunnerName: name}, nil
}

func (d *daemon) shipDiag(s slot.Slot) error { return d.shipDiagContext(context.Background(), s) }

func (d *daemon) shipDiagContext(ctx context.Context, s slot.Slot) error {
	if !d.cfg.Observability.ShipDiag {
		return nil
	}
	select {
	case d.diagSem <- struct{}{}:
		defer func() { <-d.diagSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	maxAge, maxBytes := d.cfg.Observability.DiagMaxAge, d.cfg.Observability.DiagMaxBytes
	if maxAge == 0 {
		maxAge = config.DefaultDiagMaxAge
	}
	if maxBytes == 0 {
		maxBytes = config.DefaultDiagMaxBytes
	}
	if err := logship.Prune(ctx, d.cfg.Paths.LogDir, maxAge, maxBytes); err != nil {
		return err
	}
	dest, err := logship.ShipDiagContext(ctx, s.Dir, d.cfg.Paths.LogDir, s.RunnerName)
	if err != nil {
		return err
	}
	if dest != "" {
		d.log.Info("shipped runner diagnostics", "slot", s.ID, "dest", dest)
	}
	return logship.Prune(ctx, d.cfg.Paths.LogDir, maxAge, maxBytes)
}

func (d *daemon) onSlotEvent(event slot.Event, s slot.Slot) {
	switch event {
	case slot.EventAcquireFailure:
		d.met.IncAcquireFailures()
		d.log.Warn("slot acquire failure", "slot", s.ID, "runner_name", s.RunnerName)
		d.recordAcquireFailure()
	case slot.EventAdmissionHold:
		d.met.IncAdmissionHolds()
	case slot.EventStarted:
		d.log.Info("slot started", "slot", s.ID, "runner_name", s.RunnerName, "unit", s.Unit)
	case slot.EventExited:
		d.log.Info("slot exited", "slot", s.ID, "runner_name", s.RunnerName, "unit", s.Unit)
		d.requestReconcile()
	case slot.EventStopped:
		d.log.Info("slot stopped", "slot", s.ID, "runner_name", s.RunnerName, "unit", s.Unit)
		d.requestReconcile()
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
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := d.httpSrv.Shutdown(shutdownCtx); err != nil {
			_ = d.httpSrv.Close()
		}
	}()

	// Scale set must exist before we can listen or mint JITs.
	if err := d.ss.EnsureScaleSet(ctx); err != nil {
		return fmt.Errorf("ensure scale set: %w", err)
	}
	d.log.Info("scale set ready", "scale_set", d.cfg.ScaleSet.Name, "scale_set_id", d.ss.ScaleSetID())

	// Boot adoption: reconcile leftover units and slot dirs.
	adoptCtx, adoptCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer adoptCancel()
	if units, err := d.backend.Active(adoptCtx); err != nil {
		return fmt.Errorf("boot adoption: list active units: %w", err)
	} else if err := d.table.Adopt(adoptCtx, units); err != nil {
		return fmt.Errorf("boot adoption: %w", err)
	}

	// Listener supervisor with backoff: 1s, 2s, 5s, 15s, 30s cap.
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

	// Type=notify readiness: config validated, payload prepared,
	// scale set ensured, boot adoption done, AND the
	// listener session started. No-op without NOTIFY_SOCKET; never
	// signalled when the daemon shuts down before a session came up.
	go func() {
		select {
		case <-d.sessionUp:
		case <-ctx.Done():
			return
		}
		if err := sdNotify("READY=1"); err != nil {
			d.log.Warn("sd_notify failed", "err", err)
		}
	}()

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
	// Desired pushes, local exit watchers, acquisition retry timers, and
	// the safety-net tick all funnel through here. newDaemon seeded desired
	// with min_runners, so the boot nudge starts the warm pool without
	// racing the listener's first push.
	d.requestReconcile()
	tick := time.NewTicker(d.reconcileInterval)
	retryTimer := time.NewTimer(time.Hour)
	if !retryTimer.Stop() {
		<-retryTimer.C
	}
	defer retryTimer.Stop()
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("shutting down")
			d.shutdownSlots()
			<-listenerDone
			<-syncStop
			return nil
		case <-d.reconCh:
		case <-tick.C:
		case <-retryTimer.C:
		}
		if delay := d.acquireRetryWait(); delay > 0 {
			retryTimer.Reset(delay)
			continue
		}
		n := d.desired.Load()
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := d.rec.Reconcile(rctx, n); err != nil {
			d.log.Error("reconcile failed", "desired", n, "err", err)
		}
		cancel()
		if delay := d.acquireRetryWait(); delay > 0 {
			retryTimer.Reset(delay)
		}
	}
}

// Persistent failures back off from one second to thirty seconds. A
// successful launch alone does not reset the delay: rapid unclaimed exits
// are still acquisition failures. A sustained healthy interval resets it.
func (d *daemon) recordAcquireFailure() {
	d.retryMu.Lock()
	defer d.retryMu.Unlock()
	if d.retryDelay == 0 || time.Since(d.retryAfter) > 3*time.Minute {
		d.retryDelay = time.Second
	} else {
		d.retryDelay = min(d.retryDelay*2, 30*time.Second)
	}
	d.retryAfter = time.Now().Add(d.retryDelay)
}
func (d *daemon) acquireRetryWait() time.Duration {
	d.retryMu.Lock()
	defer d.retryMu.Unlock()
	return max(0, time.Until(d.retryAfter))
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
