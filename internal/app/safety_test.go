package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/config"
	"github.com/0xinterface/tentacles/internal/history"
	"github.com/0xinterface/tentacles/internal/metrics"
	"github.com/0xinterface/tentacles/internal/process"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/slot"
)

func admissionFixture(t *testing.T, record history.Record, memory uint64) (*daemon, *poolRuntime) {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history"))
	if err != nil {
		t.Fatal(err)
	}
	record.Pool = testPoolID
	if err := store.Append(record); err != nil {
		t.Fatal(err)
	}
	host := &daemon{
		cfg: &config.Config{Scaling: config.Scaling{
			CPUTargetPercent:    90,
			MemoryMarginPercent: 20,
		}},
		hist:         store,
		numCPU:       func() int { return 4 },
		memAvailable: func() (uint64, error) { return memory, nil },
	}
	pool := &poolRuntime{
		host:       host,
		cfg:        config.Pool{ID: testPoolID},
		queuedRefs: make(map[string]int),
		queuedAt:   make(map[string]time.Time),
	}
	return host, pool
}

func TestAdmissionReservesUnclaimedSlots(t *testing.T) {
	host, pool := admissionFixture(t, history.Record{
		WorkflowRef:  "ci",
		CPUSeconds:   20,
		WallSeconds:  10,
		PeakMemBytes: 1 << 30,
		Sampled:      true,
		At:           time.Now(),
	}, 100<<30)
	for _, state := range []slot.State{slot.StateStarting, slot.StateIdle} {
		live := []slot.Slot{{Pool: testPoolID, State: state}}
		if host.admissionGate(pool, live) {
			t.Errorf("admitted beyond CPU budget with %s reservation", state)
		}
	}
}

func TestAdmissionDoesNotDoubleCountResidentMemory(t *testing.T) {
	host, pool := admissionFixture(t, history.Record{
		WorkflowRef:  "ci",
		CPUSeconds:   1,
		WallSeconds:  10,
		PeakMemBytes: 4 << 30,
		Sampled:      true,
		At:           time.Now(),
	}, 5<<30)
	live := []slot.Slot{{
		Pool:            testPoolID,
		State:           slot.StateBusy,
		WorkflowRef:     "ci",
		CurrentMemBytes: 4 << 30,
	}}
	if !host.admissionGate(pool, live) {
		t.Fatal("charged resident memory twice")
	}
}

func TestFailureDoesNotImmediatelyRequeue(t *testing.T) {
	host := &daemon{
		met:     metrics.NewRegistry(),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		reconCh: make(chan struct{}, 1),
	}
	pool := newPoolRuntime(host, config.Pool{ID: testPoolID})
	pool.onSlotEvent(slot.EventAcquireFailure, slot.Slot{ID: "0001"})
	select {
	case <-host.reconCh:
		t.Fatal("failure immediately requeued itself")
	default:
	}
}

func TestAdmissionHoldMetric(t *testing.T) {
	host := &daemon{
		met: metrics.NewRegistry(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	host.met.RegisterPool(testPoolID)
	pool := newPoolRuntime(host, config.Pool{ID: testPoolID})
	pool.onSlotEvent(slot.EventAdmissionHold, slot.Slot{})
	response := httptest.NewRecorder()
	host.met.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	want := `tentacles_admission_holds_total{pool="test"} 1`
	if !strings.Contains(response.Body.String(), want) {
		t.Fatal("admission hold not counted")
	}
}

type unavailableBackend struct{}

func (unavailableBackend) Start(context.Context, runner.Spec) error {
	return errors.New("unexpected Start")
}

func (unavailableBackend) Stop(context.Context, string) error {
	return errors.New("unexpected Stop")
}

func (unavailableBackend) Wait(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (unavailableBackend) Active(context.Context) ([]string, error) {
	return nil, errors.New("bus unavailable")
}

func testDaemonConfig() *config.Config {
	return &config.Config{
		Capacity: config.Capacity{MaxRunners: 1},
		Runtime: config.Runtime{
			SlotStopTimeout: time.Second,
		},
		Observability: config.Observability{Listen: "127.0.0.1:0"},
	}
}

func newBareDaemon(cfg *config.Config) *daemon {
	return &daemon{
		cfg:               cfg,
		log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		met:               metrics.NewRegistry(),
		pools:             []*poolRuntime{},
		reconCh:           make(chan struct{}, 1),
		reconcileInterval: time.Millisecond,
	}
}

func TestBootObservationFailureStopsDaemon(t *testing.T) {
	root := t.TempDir()
	slotDir := filepath.Join(root, "0001")
	if err := os.Mkdir(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(slotDir, "job")
	if err := os.WriteFile(sentinel, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	host := newBareDaemon(testDaemonConfig())
	pool := newPoolRuntime(host, config.Pool{ID: testPoolID})
	pool.ss = &fakeScaleSet{}
	pool.backend = unavailableBackend{}
	pool.table = slot.NewTable(
		root,
		pool.backend,
		func(string) error { return errors.New("unexpected materialize") },
		nil,
		t.TempDir(),
		pool.log,
		slot.WithNamespace(testPoolID),
	)
	host.pools = append(host.pools, pool)
	defer host.close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := host.run(ctx); err == nil || !strings.Contains(err.Error(), "adoption") {
		t.Fatalf("want failed adoption, got %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal(err)
	}
}

func TestDesiredPushesRespectAcquireBackoff(t *testing.T) {
	var attempts atomic.Int32
	host := newBareDaemon(testDaemonConfig())
	pool := newPoolRuntime(host, config.Pool{
		ID: testPoolID,
		Capacity: config.PoolCapacity{
			MinRunners: 1,
			MaxRunners: 1,
		},
	})
	pool.desired.Store(1)
	pool.ss = &fakeScaleSet{runFn: func(ctx context.Context, _ *fakeScaleSet) error {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				host.requestReconcile()
			}
		}
	}}
	pool.backend = process.New(process.Options{})
	pool.table = slot.NewTable(
		t.TempDir(),
		pool.backend,
		func(string) error {
			attempts.Add(1)
			return errors.New("payload unavailable")
		},
		nil,
		t.TempDir(),
		pool.log,
		slot.WithNamespace(testPoolID),
		slot.WithStartAllowed(func() bool { return pool.acquireRetryWait() == 0 }),
		slot.WithEventHook(pool.onSlotEvent),
	)
	host.pools = append(host.pools, pool)
	defer host.close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := host.run(ctx); err != nil {
		t.Fatal(err)
	}
	if count := attempts.Load(); count != 1 {
		t.Fatalf("made %d attempts during the first backoff interval", count)
	}
}

func TestQueuedHintsExpireAndAreBounded(t *testing.T) {
	host := newBareDaemon(testDaemonConfig())
	pool := newPoolRuntime(host, config.Pool{ID: testPoolID})
	refs := make([]string, maxQueuedRefs+10)
	for i := range refs {
		refs[i] = fmt.Sprintf("workflow-%d", i)
	}
	pool.noteQueued(refs)
	if len(pool.queuedRefs) != maxQueuedRefs {
		t.Fatalf("queue hint bound: %d", len(pool.queuedRefs))
	}
	pool.queuedMu.Lock()
	for ref := range pool.queuedAt {
		pool.queuedAt[ref] = time.Now().Add(-2 * queuedHintTTL)
	}
	pool.queuedMu.Unlock()
	pool.noteQueued([]string{"fresh"})
	if len(pool.queuedRefs) != 1 || pool.queuedRefs["fresh"] != 1 {
		t.Fatalf("stale hints retained: %d", len(pool.queuedRefs))
	}
	pool.consumeQueuedRef("fresh")
	if len(pool.queuedRefs) != 0 || len(pool.queuedAt) != 0 {
		t.Fatal("consumed hint retained")
	}
}

func TestListenerErrorsCountedOncePerRunFailure(t *testing.T) {
	host := newBareDaemon(testDaemonConfig())
	host.met.RegisterPool(testPoolID)
	pool := newPoolRuntime(host, config.Pool{ID: testPoolID})
	attempts := 0
	pool.ss = &fakeScaleSet{runFn: func(ctx context.Context, _ *fakeScaleSet) error {
		attempts++
		if attempts >= 3 {
			<-ctx.Done()
			return ctx.Err()
		}
		return errors.New("boom")
	}}
	pool.backend = process.New(process.Options{})
	pool.table = slot.NewTable(
		t.TempDir(),
		pool.backend,
		func(string) error { return errors.New("unexpected materialize") },
		nil,
		t.TempDir(),
		pool.log,
		slot.WithNamespace(testPoolID),
	)
	host.pools = append(host.pools, pool)
	defer host.close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := host.run(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts < 3 {
		t.Fatalf("listener attempted %d runs, want at least 3", attempts)
	}
	response := httptest.NewRecorder()
	host.met.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	want := `tentacles_listener_errors_total{pool="test"} 2`
	if !strings.Contains(response.Body.String(), want) {
		t.Fatalf("listener errors not counted once per failure:\n%s", response.Body.String())
	}
}

func TestOnCompletionCountsUnknownResult(t *testing.T) {
	host := newBareDaemon(testDaemonConfig())
	host.met.RegisterPool(testPoolID)
	pool := newPoolRuntime(host, config.Pool{ID: testPoolID})
	pool.onCompletion(slot.Completion{Result: "success"})
	pool.onCompletion(slot.Completion{CPUSeconds: 2, Sampled: true})
	response := httptest.NewRecorder()
	host.met.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		`tentacles_jobs_completed_total{pool="test",result="success"} 1`,
		`tentacles_jobs_completed_total{pool="test",result="unknown"} 1`,
		`tentacles_job_cpu_seconds_count{pool="test"} 1`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("missing %s:\n%s", want, response.Body.String())
		}
	}
}
