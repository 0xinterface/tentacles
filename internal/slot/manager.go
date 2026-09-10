package slot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hkust/tentacles/internal/cleanup"
	"github.com/hkust/tentacles/internal/runner"
)

// Default table tunables. The reconciler and app layers can override them
// with the With* options; production values come from config.
const (
	defaultAcquireGrace   = 3 * time.Minute
	defaultStopTimeout    = 30 * time.Second
	defaultCleanupTimeout = 60 * time.Second
	defaultStartTimeout   = 90 * time.Second
	defaultWorkDir        = "_work"

	maxSlotID = 9999
)

// tableOptions holds the tunables for a Table.
type tableOptions struct {
	acquireGrace   time.Duration
	stopTimeout    time.Duration
	cleanupTimeout time.Duration
	startTimeout   time.Duration
	startObserver  func(time.Duration)
	diagHook       func(Slot) error
	eventHook      func(Event, Slot)
	envFile        string
	user           string
	group          string
	cpuQuota       string
	memoryMax      string
	workDir        string
	usageSampler   func(unit string) (Usage, error)
	sampleInterval time.Duration
	gate           func(live []Slot) bool
	completionHook func(Completion)
}

// defaultSampleInterval is how often live slot units are polled for
// resource accounting when a usage sampler is configured.
const defaultSampleInterval = 30 * time.Second

// TableOption configures a Table. Options are applied in order; the last
// one wins for each field.
type TableOption func(*tableOptions)

// WithAcquireGrace sets the window a freshly started runner has to claim
// its first job. A process exit before the window closes without a job
// is classified as an acquire failure (plan §13), and a slot still in
// "starting" past the window may be stopped as surplus (plan §9). Idle
// slots are never reaped — idleness is the warm pool. Non-positive
// values disable both uses. Default 3m.
func WithAcquireGrace(d time.Duration) TableOption {
	return func(o *tableOptions) { o.acquireGrace = d }
}

// WithStopTimeout bounds each backend.Stop call issued by the Table.
// Default 30s.
func WithStopTimeout(d time.Duration) TableOption {
	return func(o *tableOptions) { o.stopTimeout = d }
}

// WithCleanupTimeout bounds the slot-directory wipe after a runner exits.
// Default 60s.
func WithCleanupTimeout(d time.Duration) TableOption {
	return func(o *tableOptions) { o.cleanupTimeout = d }
}

// WithStartTimeout bounds each backend.Start call. Default 90s.
func WithStartTimeout(d time.Duration) TableOption {
	return func(o *tableOptions) { o.startTimeout = d }
}

// WithStartObserver registers a callback invoked once per successful
// slot start with the total provision time (materialize + JIT mint +
// backend start). Used for the slot_start histogram.
func WithStartObserver(fn func(time.Duration)) TableOption {
	return func(o *tableOptions) { o.startObserver = fn }
}

// WithDiagHook registers a best-effort diagnostic callback invoked with
// the exiting slot before its directory is wiped. Errors are logged, not
// fatal. Used to ship _diag before cleanup.
func WithDiagHook(hook func(Slot) error) TableOption {
	return func(o *tableOptions) { o.diagHook = hook }
}

// WithEventHook registers a lifecycle event sink. See the Event constants
// for the vocabulary.
func WithEventHook(hook func(Event, Slot)) TableOption {
	return func(o *tableOptions) { o.eventHook = hook }
}

// WithEnvFile sets the environment file (systemd EnvironmentFile syntax)
// passed to the backend for every slot.
func WithEnvFile(path string) TableOption {
	return func(o *tableOptions) { o.envFile = path }
}

// WithRunUser sets the unix user (and optional group) the runner process
// runs as.
func WithRunUser(user, group string) TableOption {
	return func(o *tableOptions) { o.user, o.group = user, group }
}

// WithLimits sets the CPU quota and memory ceiling passed to the backend
// (systemd CPUQuota / MemoryMax syntax, e.g. "400%" and "8G").
func WithLimits(cpuQuota, memoryMax string) TableOption {
	return func(o *tableOptions) { o.cpuQuota, o.memoryMax = cpuQuota, memoryMax }
}

// WithWorkDir sets the work-directory name inside each slot. Default
// "_work".
func WithWorkDir(rel string) TableOption {
	return func(o *tableOptions) { o.workDir = rel }
}

// WithUsageSampler registers a callback that reads cumulative resource
// accounting for a unit (systemd CPUUsageNSec/MemoryPeak in the
// production backend). Non-nil enables per-job usage recording.
func WithUsageSampler(fn func(unit string) (Usage, error)) TableOption {
	return func(o *tableOptions) { o.usageSampler = fn }
}

// WithSampleInterval overrides the usage sampling period. Non-positive
// values fall back to the default 30s.
func WithSampleInterval(d time.Duration) TableOption {
	return func(o *tableOptions) { o.sampleInterval = d }
}

// WithGate registers the admission gate. Before each slot start it
// receives the live slots and reports whether the host budget can take
// another job. A refusal is backpressure: the start is retried on a
// later tick, and reported as EventAdmissionHold, not a failure. Nil
// admits everything.
func WithGate(fn func(live []Slot) bool) TableOption {
	return func(o *tableOptions) { o.gate = fn }
}

// WithCompletionHook registers a sink for per-job usage records, fired
// once per claimed slot exit. Unclaimed exits (acquire failures) are
// not completions.
func WithCompletionHook(fn func(Completion)) TableOption {
	return func(o *tableOptions) { o.completionHook = fn }
}

// slotRec is the internal bookkeeping for one slot. The embedded Slot is
// the public view; the remaining fields drive lifecycle decisions and
// usage attribution.
type slotRec struct {
	Slot
	busySince    time.Time // set when the runner first claims a job; zero = never busy
	provStart    time.Time // when provisioning began; drives starting-past-grace stops
	claimed      bool      // a JobStarted was seen for this slot
	queueWait    float64   // GitHub-reported queue wait at claim
	claimAt      time.Time // local time of the claim
	usageAtClaim Usage     // sampler reading at claim time
	lastUsage    Usage     // most recent sampler reading
	sampledOK    bool      // at least one sampler reading succeeded
}

// Table is the concrete Manager: it allocates slot IDs, materializes and
// starts runner slots through the injected backend, and wipes them on
// exit. It is safe for concurrent use; the reconciler is the only
// writer, but metrics/listers may read at any time.
type Table struct {
	root        string
	backend     runner.Backend
	materialize func(dst string) error
	jit         func(ctx context.Context, id ID) (runner.JIT, error)
	jitDir      string
	log         *slog.Logger
	opts        tableOptions

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	slots   map[ID]*slotRec
	desired int
	closed  bool
}

// NewTable builds a slot table rooted at root (the directory that holds
// the per-slot subdirectories "0001" … "9999"). materialize populates a
// fresh slot directory (e.g. copying the runner payload template); jit
// mints a one-job JIT config for a slot (returning the GitHub runner
// name); jitDir holds the 0600 JIT files, one per slot ID.
func NewTable(root string, backend runner.Backend, materialize func(dst string) error, jit func(ctx context.Context, id ID) (runner.JIT, error), jitDir string, log *slog.Logger, opts ...TableOption) *Table {
	if log == nil {
		log = slog.Default()
	}
	o := tableOptions{
		acquireGrace:   defaultAcquireGrace,
		stopTimeout:    defaultStopTimeout,
		cleanupTimeout: defaultCleanupTimeout,
		startTimeout:   defaultStartTimeout,
		workDir:        defaultWorkDir,
		sampleInterval: defaultSampleInterval,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.sampleInterval <= 0 {
		o.sampleInterval = defaultSampleInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Table{
		root:        root,
		backend:     backend,
		materialize: materialize,
		jit:         jit,
		jitDir:      jitDir,
		log:         log,
		opts:        o,
		ctx:         ctx,
		cancel:      cancel,
		slots:       make(map[ID]*slotRec),
	}
	if o.usageSampler != nil {
		go t.sampleLoop()
	}
	return t
}

// sampleLoop polls the usage sampler for every live slot so a job's
// resource consumption is known even though systemd garbage-collects
// the unit right after exit. CPU seconds are cumulative; peak memory is
// maintained by the kernel, so the latest reading is the peak so far.
func (t *Table) sampleLoop() {
	ticker := time.NewTicker(t.opts.sampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
		}
		type target struct {
			rec  *slotRec
			unit string
		}
		t.mu.Lock()
		var targets []target
		for _, rec := range t.slots {
			if rec.State == StateBusy || rec.State == StateIdle {
				targets = append(targets, target{rec: rec, unit: rec.Unit})
			}
		}
		t.mu.Unlock()
		for _, tg := range targets {
			u, err := t.opts.usageSampler(tg.unit)
			if err != nil {
				continue // unit may have just exited; the last sample stands
			}
			t.mu.Lock()
			tg.rec.lastUsage = u
			tg.rec.sampledOK = true
			t.mu.Unlock()
		}
	}
}

// Desired returns the last desired runner count pushed by the listener.
func (t *Table) Desired() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.desired
}

// SetDesired records the desired runner count. The reconciler calls this
// before Ensure so the value is observable even while convergence is in
// flight.
func (t *Table) SetDesired(n int) {
	t.mu.Lock()
	t.desired = n
	t.mu.Unlock()
}

// WorkDir returns the absolute work-directory path for a slot ID (the
// JIT work folder, plan §8). It is the single source of truth for the
// configured work-directory name.
func (t *Table) WorkDir(id ID) string {
	return filepath.Join(t.root, string(id), t.opts.workDir)
}

// Active returns the slots that count toward the actual runner count
// (starting, idle, busy), oldest ID first.
func (t *Table) Active() []Slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(func(st State) bool { return st.Live() })
}

// Snapshot returns a copy of every tracked slot (including stopping),
// oldest ID first.
func (t *Table) Snapshot() []Slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(func(State) bool { return true })
}

func (t *Table) snapshotLocked(keep func(State) bool) []Slot {
	out := make([]Slot, 0, len(t.slots))
	for _, rec := range t.slots {
		if keep(rec.State) {
			out = append(out, rec.Slot)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Ensure converges the live slot count toward desired. Missing slots are
// started sequentially; surplus idle/starting slots are stopped (oldest
// first). It is idempotent: redelivery of the same desired count is a
// no-op. Busy slots are never stopped. A failed start leaves desired
// unsatisfied and is retried on the next tick.
func (t *Table) Ensure(ctx context.Context, desired int) error {
	for {
		t.mu.Lock()
		live := t.countLiveLocked()
		closed := t.closed
		t.mu.Unlock()
		if closed || live >= desired {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.startOne(ctx) == "" {
			// Start failed (logged and emitted); do not spin.
			break
		}
	}
	t.stopSurplus(desired)
	return nil
}

func (t *Table) countLiveLocked() int {
	n := 0
	for _, rec := range t.slots {
		if rec.State.Live() {
			n++
		}
	}
	return n
}

// startOne provisions and starts a single slot. It returns the new slot
// ID, or "" if the start failed (already logged and emitted as an
// "acquire_failure" event).
func (t *Table) startOne(ctx context.Context) ID {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ""
	}
	id, ok := t.nextIDLocked()
	if !ok {
		t.mu.Unlock()
		t.log.Error("no free slot ids", "max", maxSlotID)
		return ""
	}
	t0 := time.Now()
	rec := &slotRec{Slot: Slot{
		ID:    id,
		Dir:   filepath.Join(t.root, string(id)),
		Unit:  unitName(id),
		State: StateStarting,
	}, provStart: t0}
	t.slots[id] = rec
	t.mu.Unlock()

	// Admission gate (backpressure, not failure): the host budget says
	// this slot would overshoot predicted usage. Free the reserved ID
	// and stop; Ensure breaks and the next reconcile tick retries.
	if t.opts.gate != nil && !t.opts.gate(t.Active()) {
		t.log.Info("slot start held by admission gate", "slot", id)
		t.mu.Lock()
		delete(t.slots, id)
		rec.State = StateFailed
		snap := rec.Slot
		t.mu.Unlock()
		t.emit(EventAdmissionHold, snap)
		return ""
	}

	// fail records the failure, frees the ID for retry, removes anything
	// the failed attempt left behind (a partial materialization would
	// wedge the ID: payload.CopySlot refuses an existing destination),
	// and emits the acquire_failure event with a failed-state snapshot.
	fail := func(err error) {
		t.log.Warn("slot start failed", "slot", id, "err", err)
		t.mu.Lock()
		delete(t.slots, id)
		rec.State = StateFailed
		snap := rec.Slot
		t.mu.Unlock()
		_ = os.RemoveAll(rec.Dir)
		if rec.JITPath != "" {
			_ = os.Remove(rec.JITPath)
		}
		t.emit(EventAcquireFailure, snap)
	}

	if err := t.materialize(rec.Dir); err != nil {
		fail(fmt.Errorf("materialize slot: %w", err))
		return ""
	}
	jit, err := t.jit(ctx, id)
	if err != nil {
		fail(fmt.Errorf("mint JIT: %w", err))
		return ""
	}
	rec.RunnerName = jit.RunnerName
	rec.JITPath = filepath.Join(t.jitDir, string(id)+".jit")
	if err := runner.WriteJIT(rec.JITPath, jit.Encoded); err != nil {
		fail(fmt.Errorf("write JIT: %w", err))
		return ""
	}
	spec := runner.Spec{
		SlotDir:   rec.Dir,
		JITPath:   rec.JITPath,
		EnvFile:   t.opts.envFile,
		User:      t.opts.user,
		Group:     t.opts.group,
		CPUQuota:  t.opts.cpuQuota,
		MemoryMax: t.opts.memoryMax,
		UnitName:  rec.Unit,
	}
	startCtx, cancel := context.WithTimeout(ctx, t.opts.startTimeout)
	defer cancel()
	if err := t.backend.Start(startCtx, spec); err != nil {
		fail(fmt.Errorf("backend start: %w", err))
		// The backend may have partially forked the process; stop it.
		if serr := t.backend.Stop(context.Background(), rec.Unit); serr != nil {
			t.log.Debug("best-effort stop after failed start", "slot", id, "err", serr)
		}
		return ""
	}

	rec.StartedAt = time.Now()
	rec.State = StateIdle
	snap := rec.Slot
	t.log.Info("slot started", "slot", id, "unit", rec.Unit, "runner_name", rec.RunnerName)
	t.emit(EventStarted, snap)
	if t.opts.startObserver != nil {
		t.opts.startObserver(time.Since(t0))
	}
	go t.watch(rec)
	return id
}

// nextIDLocked returns the lowest unused zero-padded slot ID.
func (t *Table) nextIDLocked() (ID, bool) {
	for n := 1; n <= maxSlotID; n++ {
		id := ID(fmt.Sprintf("%04d", n))
		if _, ok := t.slots[id]; !ok {
			return id, true
		}
	}
	return "", false
}

// stopSurplus stops the oldest surplus slots, never busy ones. If the
// only live slots are busy, nothing is stopped.
func (t *Table) stopSurplus(desired int) {
	type candidate struct {
		rec  *slotRec
		snap Slot
	}
	var candidates []candidate

	t.mu.Lock()
	excess := t.countLiveLocked() - desired
	if excess > 0 {
		for _, rec := range t.slots {
			switch rec.State {
			case StateIdle:
				// Eligible immediately.
			case StateStarting:
				// Plan §9: stop only slots "in idle or starting-past-grace".
				if t.opts.acquireGrace > 0 && time.Since(rec.provStart) <= t.opts.acquireGrace {
					continue
				}
			default:
				continue
			}
			candidates = append(candidates, candidate{rec: rec, snap: rec.Slot})
		}
		// Oldest first; StartedAt ties broken by ID for determinism.
		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].rec.StartedAt.Equal(candidates[j].rec.StartedAt) {
				return candidates[i].rec.StartedAt.Before(candidates[j].rec.StartedAt)
			}
			return candidates[i].rec.ID < candidates[j].rec.ID
		})
		if len(candidates) > excess {
			candidates = candidates[:excess]
		}
		for _, c := range candidates {
			c.rec.State = StateStopping
		}
	}
	t.mu.Unlock()

	for _, c := range candidates {
		t.log.Info("stopping surplus slot", "slot", c.rec.ID)
		stopCtx, cancel := context.WithTimeout(context.Background(), t.opts.stopTimeout)
		err := t.backend.Stop(stopCtx, c.rec.Unit)
		cancel()
		if err != nil {
			t.log.Warn("stop surplus slot failed", "slot", c.rec.ID, "err", err)
			t.mu.Lock()
			if cur, ok := t.slots[c.rec.ID]; ok && cur == c.rec && c.rec.State == StateStopping {
				c.rec.State = StateIdle
			}
			t.mu.Unlock()
			continue
		}
		t.emit(EventStopped, c.snap)
	}
}

// watch blocks on the backend until the unit exits, then hands the slot
// to ObserveExit for cleanup. On Table.Close the backend wait is
// cancelled and the slot is left in place for boot adoption.
func (t *Table) watch(rec *slotRec) {
	err := t.backend.Wait(t.ctx, rec.Unit)
	if t.ctx.Err() != nil {
		return // Table closed; leave unit and dir for boot adoption.
	}
	t.ObserveExit(rec.ID, err)
}

// MarkBusy flags the slot as busy so scale-down will not stop it. Only
// starting and idle slots transition; stopping/empty slots are left
// alone.
func (t *Table) MarkBusy(id ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.slots[id]; ok {
		if rec.State == StateStarting || rec.State == StateIdle {
			rec.State = StateBusy
			rec.busySince = time.Now()
		}
	}
}

// Claim records that a runner claimed a job (JobStarted) and marks its
// slot busy so scale-down will not stop it. It reports whether a slot
// with that runner name was found.
func (t *Table) Claim(job ClaimJob) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rec := range t.slots {
		if rec.RunnerName != job.RunnerName {
			continue
		}
		if rec.State == StateStarting || rec.State == StateIdle {
			rec.State = StateBusy
			rec.busySince = time.Now()
			rec.WorkflowRef = job.WorkflowRef
			rec.RunID = job.RunID
			rec.claimed = true
			rec.claimAt = time.Now()
			rec.queueWait = job.QueueWaitSeconds
			rec.usageAtClaim = rec.lastUsage
		}
		return true
	}
	return false
}

// ObserveExit records a runner process exit and releases the slot: the
// diag hook runs (best-effort), the JIT file and slot directory are
// wiped, and the ID is freed for reuse.
func (t *Table) ObserveExit(id ID, err error) {
	t.mu.Lock()
	rec, ok := t.slots[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	prev := rec.State
	if prev == StateEmpty || prev == StateFailed {
		t.mu.Unlock()
		return
	}
	rec.State = StateStopping
	snap := rec.Slot
	t.mu.Unlock()

	t.log.Info("slot exited", "slot", id, "prev_state", prev, "err", err)

	// Per-job usage record for claimed slots: the input to the workflow
	// usage history. CPU is the delta since the claim (the agent's
	// registration cost is not the job's); peak memory is the unit peak.
	if rec.claimed && t.opts.completionHook != nil {
		cpu := rec.lastUsage.CPUSeconds - rec.usageAtClaim.CPUSeconds
		if cpu < 0 {
			cpu = 0
		}
		var wallSeconds float64
		if !rec.claimAt.IsZero() {
			wallSeconds = time.Since(rec.claimAt).Seconds()
		}
		t.opts.completionHook(Completion{
			ID:               id,
			RunnerName:       rec.RunnerName,
			WorkflowRef:      rec.WorkflowRef,
			RunID:            rec.RunID,
			CPUSeconds:       cpu,
			PeakMemBytes:     rec.lastUsage.PeakMemBytes,
			WallSeconds:      wallSeconds,
			QueueWaitSeconds: rec.queueWait,
			Sampled:          rec.sampledOK,
		})
	}

	t.wipe(id, rec.Dir)

	t.mu.Lock()
	delete(t.slots, id)
	t.mu.Unlock()

	// Teardowns we initiated are already reported as "stopped" or
	// "acquire_failure". Natural exits are "exited", except a quick
	// never-busy exit, which is an acquire failure (plan §13).
	if prev != StateStopping {
		if t.quickNeverBusyExit(rec, prev) {
			t.emit(EventAcquireFailure, snap)
		} else {
			t.emit(EventExited, snap)
		}
	}
}

// quickNeverBusyExit reports whether an exit counts as an acquire
// failure per plan §13: the process exited before any JobStarted and
// before the acquire grace elapsed. Adopted slots that were running a
// job count as busy (boot adoption is conservative).
func (t *Table) quickNeverBusyExit(rec *slotRec, prev State) bool {
	if t.opts.acquireGrace <= 0 {
		return false
	}
	if prev == StateBusy || !rec.busySince.IsZero() {
		return false
	}
	start := rec.StartedAt
	if start.IsZero() {
		start = rec.provStart
	}
	return time.Since(start) < t.opts.acquireGrace
}

// wipe runs the post-exit teardown for a slot: diag hook, JIT removal,
// and directory removal, bounded by the cleanup timeout.
func (t *Table) wipe(id ID, dir string) {
	if t.opts.diagHook != nil {
		snap := Slot{ID: id, Dir: dir, State: StateStopping}
		if err := t.opts.diagHook(snap); err != nil {
			t.log.Warn("slot diag hook failed", "slot", id, "err", err)
		}
	}
	// Shred credential leftovers before the tree goes (plan §10).
	if err := cleanup.ShredCredentials(dir); err != nil {
		t.log.Warn("slot credential shred failed", "slot", id, "err", err)
	}
	if t.jitDir != "" {
		jitPath := filepath.Join(t.jitDir, string(id)+".jit")
		if err := os.Remove(jitPath); err != nil && !os.IsNotExist(err) {
			t.log.Warn("slot JIT removal failed", "slot", id, "err", err)
		}
	}
	if err := removeAllBounded(dir, t.opts.cleanupTimeout); err != nil {
		t.log.Warn("slot cleanup failed", "slot", id, "err", err)
	}
}

// Adopt reconciles the table with pre-existing state at boot, given the
// backend's currently-active unit names. A slot directory whose unit is
// running is adopted as busy (conservative); a directory with no running
// unit is wiped; a running unit with no directory is stopped. Already
// tracked slots are left untouched.
func (t *Table) Adopt(ctx context.Context, units []string) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil
	}

	running := make(map[string]bool, len(units))
	for _, u := range units {
		running[u] = true
	}

	entries, err := os.ReadDir(t.root)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("adopt: list slot dirs: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := ID(e.Name())
		if !validID(id) {
			t.log.Warn("ignoring non-slot directory", "dir", e.Name())
			continue
		}
		dir := filepath.Join(t.root, string(id))
		t.mu.Lock()
		_, tracked := t.slots[id]
		t.mu.Unlock()
		if tracked {
			continue
		}
		if running[unitName(id)] {
			rec := &slotRec{Slot: Slot{
				ID:         id,
				Dir:        dir,
				Unit:       unitName(id),
				RunnerName: readRunnerName(dir),
				State:      StateBusy,
				StartedAt:  time.Now(),
			}}
			t.mu.Lock()
			t.slots[id] = rec
			t.mu.Unlock()
			t.log.Info("adopted running slot", "slot", id, "unit", rec.Unit)
			go t.watch(rec)
			continue
		}
		t.log.Warn("wiping orphan slot directory", "slot", id)
		t.wipe(id, dir)
	}

	for _, u := range units {
		id, ok := idFromUnit(u)
		if !ok {
			continue
		}
		t.mu.Lock()
		_, tracked := t.slots[id]
		t.mu.Unlock()
		if tracked {
			continue
		}
		dir := filepath.Join(t.root, string(id))
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			t.log.Warn("stopping unit without slot directory", "unit", u)
			if err := t.backend.Stop(ctx, u); err != nil {
				t.log.Warn("stop unitless slot failed", "unit", u, "err", err)
			}
		}
	}
	return nil
}

// Close cancels the watcher goroutines. Running units and slot
// directories are left in place so boot adoption can recover them on the
// next start.
func (t *Table) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.cancel()
	t.mu.Unlock()
}

func (t *Table) emit(event Event, s Slot) {
	if t.opts.eventHook != nil {
		t.opts.eventHook(event, s)
	}
}

// unitName maps a slot ID to its backend unit name, e.g. "0001" →
// "tentacle-0001.service".
func unitName(id ID) string {
	return "tentacle-" + string(id) + ".service"
}

// idFromUnit extracts the slot ID from a unit name, e.g.
// "tentacle-0001.service" → "0001". It returns false for names that are
// not ours.
func idFromUnit(unit string) (ID, bool) {
	const prefix = "tentacle-"
	const suffix = ".service"
	if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, suffix) {
		return "", false
	}
	id := ID(strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix))
	if !validID(id) {
		return "", false
	}
	return id, true
}

// validID reports whether s is a zero-padded slot ID in "0001" … "9999".
func validID(id ID) bool {
	s := string(id)
	if len(s) != 4 {
		return false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= maxSlotID
}

// readRunnerName extracts the runner name the agent persisted in the
// slot's .runner file at registration, so an adopted in-flight job can
// still be correlated with JobStarted messages. Any parse failure
// yields "" (the pre-adoption behavior).
func readRunnerName(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".runner"))
	if err != nil {
		return ""
	}
	var cfg struct {
		AgentName string `json:"agentName"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.AgentName
}

// removeAllBounded removes dir, giving up after timeout. If the removal
// hangs past the deadline the caller gets a timeout error (the runaway
// removal goroutine is abandoned, not waited on).
func removeAllBounded(dir string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- os.RemoveAll(dir) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("removing %s exceeded %v", dir, timeout)
	}
}
