package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/0xinterface/tentacles/internal/config"
	"github.com/0xinterface/tentacles/internal/history"
	"github.com/0xinterface/tentacles/internal/logship"
	"github.com/0xinterface/tentacles/internal/payload"
	"github.com/0xinterface/tentacles/internal/reconcile"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/scaleset"
	"github.com/0xinterface/tentacles/internal/slot"
)

const maxQueuedRefs = 1024
const queuedHintTTL = 10 * time.Minute

type poolRuntime struct {
	host    *daemon
	cfg     config.Pool
	log     *slog.Logger
	ss      ScaleSet
	backend runner.Backend
	table   *slot.Table
	desired desiredCell

	sessionUp   chan struct{}
	sessionOnce sync.Once

	queuedMu   sync.Mutex
	queuedRefs map[string]int
	queuedAt   map[string]time.Time

	retryMu    sync.Mutex
	retryAfter time.Time
	retryDelay time.Duration
}

func newPoolRuntime(host *daemon, cfg config.Pool) *poolRuntime {
	return &poolRuntime{
		host:       host,
		cfg:        cfg,
		log:        host.log.With("pool", cfg.ID),
		sessionUp:  make(chan struct{}),
		queuedRefs: make(map[string]int),
		queuedAt:   make(map[string]time.Time),
	}
}

// buildEvents translates one scale set's events into changes to its own slot
// table while nudging the shared host scheduler.
func (p *poolRuntime) buildEvents() scaleset.Events {
	return scaleset.Events{
		SessionStarted: func() {
			p.sessionOnce.Do(func() { close(p.sessionUp) })
		},
		Desired: func(n int) {
			p.desired.Store(reconcile.Clamp(
				n,
				p.cfg.Capacity.MinRunners,
				p.cfg.Capacity.MaxRunners,
			))
			p.host.requestReconcile()
		},
		JobStart: func(job scaleset.Job) {
			if p.table == nil {
				return
			}
			if p.table.Claim(slot.ClaimJob{
				RunnerName:       job.RunnerName,
				WorkflowRef:      job.WorkflowRef,
				RunID:            job.WorkflowRunID,
				QueueWaitSeconds: queueWait(job),
			}) {
				p.host.met.IncJobsStarted(p.cfg.ID)
				p.log.Info(
					"runner busy",
					"runner_name", job.RunnerName,
					"workflow_ref", job.WorkflowRef,
				)
				p.consumeQueuedRef(job.WorkflowRef)
				return
			}
			p.log.Warn("JobStarted for unknown runner", "runner_name", job.RunnerName)
		},
		JobEnd: func(job scaleset.Job) {
			if p.table != nil {
				p.table.MarkResult(job.RunnerName, job.Result)
			}
			p.log.Info("job completed", "runner_name", job.RunnerName, "result", job.Result)
		},
		Queued: p.noteQueued,
		MessageID: func(id int64) {
			p.host.met.SetLastMessageID(p.cfg.ID, id)
		},
		Session: func(err error) {
			if err != nil {
				p.log.Error("listener session ended", "err", err)
			}
		},
	}
}

func queueWait(job scaleset.Job) float64 {
	if job.QueueTime.IsZero() || job.RunnerAssignTime.IsZero() {
		return 0
	}
	wait := job.RunnerAssignTime.Sub(job.QueueTime).Seconds()
	if wait < 0 {
		return 0
	}
	return wait
}

func (p *poolRuntime) noteQueued(refs []string) {
	p.queuedMu.Lock()
	defer p.queuedMu.Unlock()
	now := time.Now()
	p.expireQueuedLocked(now)
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := p.queuedRefs[ref]; !ok && len(p.queuedRefs) >= maxQueuedRefs {
			continue
		}
		p.queuedRefs[ref] = min(p.queuedRefs[ref]+1, 9999)
		p.queuedAt[ref] = now
	}
}

func (p *poolRuntime) expireQueuedLocked(now time.Time) {
	for ref, count := range p.queuedRefs {
		at := p.queuedAt[ref]
		if count <= 0 || (!at.IsZero() && now.Sub(at) > queuedHintTTL) {
			delete(p.queuedRefs, ref)
			delete(p.queuedAt, ref)
		}
	}
}

func (p *poolRuntime) consumeQueuedRef(ref string) {
	p.queuedMu.Lock()
	defer p.queuedMu.Unlock()
	if p.queuedRefs[ref] <= 1 {
		delete(p.queuedRefs, ref)
		delete(p.queuedAt, ref)
		return
	}
	p.queuedRefs[ref]--
}

// candidateEstimate reserves the largest queued estimate in each dimension.
func (p *poolRuntime) candidateEstimate() (float64, uint64) {
	if p.host.hist == nil {
		return 0, 0
	}
	p.queuedMu.Lock()
	defer p.queuedMu.Unlock()
	p.expireQueuedLocked(time.Now())
	if len(p.queuedRefs) == 0 {
		cores, _ := p.host.hist.PredictCores(p.cfg.ID, "")
		memory, _ := p.host.hist.PredictMem(p.cfg.ID, "")
		return cores, memory
	}
	var cores float64
	var memory uint64
	for ref := range p.queuedRefs {
		predictedCores, _ := p.host.hist.PredictCores(p.cfg.ID, ref)
		predictedMemory, _ := p.host.hist.PredictMem(p.cfg.ID, ref)
		cores = max(cores, predictedCores)
		memory = max(memory, predictedMemory)
	}
	return cores, memory
}

func (p *poolRuntime) mintJIT(ctx context.Context, id slot.ID) (runner.JIT, error) {
	var random [3]byte
	if _, err := rand.Read(random[:]); err != nil {
		return runner.JIT{}, fmt.Errorf("rand: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s", p.cfg.ScaleSet.Name, id, hex.EncodeToString(random[:]))
	encoded, err := p.ss.GenerateJIT(ctx, name, p.table.WorkDir(id))
	if err != nil {
		return runner.JIT{}, fmt.Errorf("generate jit: %w", err)
	}
	return runner.JIT{Encoded: encoded, RunnerName: name}, nil
}

func (p *poolRuntime) materializeSlotContext(pm *payload.Manager) func(context.Context, string) error {
	return func(ctx context.Context, dst string) error {
		free, total, err := diskUsage(p.host.cfg.Paths.StateDir)
		if err == nil && total > 0 && free*10 < total {
			return fmt.Errorf(
				"disk watermark: only %.1f%% free on %s",
				100*float64(free)/float64(total),
				p.host.cfg.Paths.StateDir,
			)
		}
		return pm.CopySlotIntoContext(ctx, dst)
	}
}

func (p *poolRuntime) shipDiag(ctx context.Context, runnerSlot slot.Slot) error {
	if !p.host.cfg.Observability.ShipDiag {
		return nil
	}
	select {
	case p.host.diagSem <- struct{}{}:
		defer func() { <-p.host.diagSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	maxAge := p.host.cfg.Observability.DiagMaxAge
	if maxAge == 0 {
		maxAge = config.DefaultDiagMaxAge
	}
	maxBytes := p.host.cfg.Observability.DiagMaxBytes
	if maxBytes == 0 {
		maxBytes = config.DefaultDiagMaxBytes
	}
	logDir := p.host.cfg.PoolLogDir(p.cfg.ID)
	if err := logship.Prune(ctx, logDir, maxAge, maxBytes); err != nil {
		return err
	}
	dest, err := logship.ShipDiagContext(ctx, runnerSlot.Dir, logDir, runnerSlot.RunnerName)
	if err != nil {
		return err
	}
	if dest != "" {
		p.log.Info("shipped runner diagnostics", "slot", runnerSlot.ID, "dest", dest)
	}
	return logship.Prune(ctx, logDir, maxAge, maxBytes)
}

func (p *poolRuntime) onSlotEvent(event slot.Event, runnerSlot slot.Slot) {
	switch event {
	case slot.EventAcquireFailure:
		p.host.met.IncAcquireFailures(p.cfg.ID)
		p.log.Warn("slot acquire failure", "slot", runnerSlot.ID, "runner_name", runnerSlot.RunnerName)
		p.recordAcquireFailure()
	case slot.EventAdmissionHold:
		p.host.met.IncAdmissionHolds(p.cfg.ID)
	case slot.EventStarted:
		p.log.Info(
			"slot started",
			"slot", runnerSlot.ID,
			"runner_name", runnerSlot.RunnerName,
			"unit", runnerSlot.Unit,
		)
	case slot.EventExited:
		p.log.Info(
			"slot exited",
			"slot", runnerSlot.ID,
			"runner_name", runnerSlot.RunnerName,
			"unit", runnerSlot.Unit,
		)
		p.host.requestReconcile()
	case slot.EventStopped:
		p.log.Info(
			"slot stopped",
			"slot", runnerSlot.ID,
			"runner_name", runnerSlot.RunnerName,
			"unit", runnerSlot.Unit,
		)
		p.host.requestReconcile()
	}
}

func (p *poolRuntime) onCompletion(completion slot.Completion) {
	if p.host.hist != nil {
		if err := p.host.hist.Append(history.Record{
			Pool:             p.cfg.ID,
			Slot:             string(completion.ID),
			WorkflowRef:      completion.WorkflowRef,
			RunID:            completion.RunID,
			CPUSeconds:       completion.CPUSeconds,
			PeakMemBytes:     completion.PeakMemBytes,
			WallSeconds:      completion.WallSeconds,
			QueueWaitSeconds: completion.QueueWaitSeconds,
			Sampled:          completion.Sampled,
			At:               time.Now(),
		}); err != nil {
			p.log.Warn("usage history append failed", "err", err)
		}
	}
	if completion.Sampled {
		p.host.met.ObserveJobCPU(p.cfg.ID, completion.CPUSeconds)
	}
	if completion.PeakMemBytes > 0 {
		p.host.met.SetJobPeakMem(p.cfg.ID, completion.PeakMemBytes)
	}
	p.host.met.ObserveJobWall(p.cfg.ID, completion.WallSeconds)
	result := completion.Result
	if result == "" {
		result = "unknown"
	}
	p.host.met.IncJobsCompleted(p.cfg.ID, result)
}

// Persistent failures back off independently, so one unhealthy organization
// cannot pause starts for every other pool.
func (p *poolRuntime) recordAcquireFailure() {
	p.retryMu.Lock()
	defer p.retryMu.Unlock()
	if p.retryDelay == 0 || time.Since(p.retryAfter) > 3*time.Minute {
		p.retryDelay = time.Second
	} else {
		p.retryDelay = min(p.retryDelay*2, 30*time.Second)
	}
	p.retryAfter = time.Now().Add(p.retryDelay)
}

func (p *poolRuntime) acquireRetryWait() time.Duration {
	p.retryMu.Lock()
	defer p.retryMu.Unlock()
	return max(0, time.Until(p.retryAfter))
}
