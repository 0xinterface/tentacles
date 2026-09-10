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
	"github.com/0xinterface/tentacles/internal/reconcile"
	"github.com/0xinterface/tentacles/internal/runner"
	"github.com/0xinterface/tentacles/internal/slot"
)

func TestAdmissionReservesUnclaimedSlots(t *testing.T) {
	h, err := history.Open(filepath.Join(t.TempDir(), "history"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Append(history.Record{WorkflowRef: "ci", CPUSeconds: 20, WallSeconds: 10, PeakMemBytes: 1 << 30, Sampled: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: &config.Config{Scaling: config.Scaling{CPUTargetPercent: 90}}, hist: h, numCPU: func() int { return 4 }, memAvailable: func() (uint64, error) { return 100 << 30, nil }}
	for _, state := range []slot.State{slot.StateStarting, slot.StateIdle} {
		if d.admissionGate([]slot.Slot{{State: state}}) {
			t.Errorf("admitted beyond CPU budget with %s reservation", state)
		}
	}
}

func TestAdmissionDoesNotDoubleCountResidentMemory(t *testing.T) {
	h, err := history.Open(filepath.Join(t.TempDir(), "history"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Append(history.Record{WorkflowRef: "ci", CPUSeconds: 1, WallSeconds: 10, PeakMemBytes: 4 << 30, Sampled: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: &config.Config{Scaling: config.Scaling{CPUTargetPercent: 90}}, hist: h, numCPU: func() int { return 4 }, memAvailable: func() (uint64, error) { return 5 << 30, nil }}
	if !d.admissionGate([]slot.Slot{{State: slot.StateBusy, WorkflowRef: "ci", CurrentMemBytes: 4 << 30}}) {
		t.Fatal("charged resident memory twice")
	}
}

func TestFailureDoesNotImmediatelyRequeue(t *testing.T) {
	d := &daemon{met: metrics.NewRegistry(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), reconCh: make(chan struct{}, 1)}
	d.onSlotEvent(slot.EventAcquireFailure, slot.Slot{ID: "0001"})
	select {
	case <-d.reconCh:
		t.Fatal("failure immediately requeued itself")
	default:
	}
}

func TestAdmissionHoldMetric(t *testing.T) {
	d := &daemon{met: metrics.NewRegistry()}
	d.onSlotEvent(slot.EventAdmissionHold, slot.Slot{})
	w := httptest.NewRecorder()
	d.met.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "tentacles_admission_holds_total 1\n") {
		t.Fatal("admission hold not counted")
	}
}

type unavailableBackend struct{}

func (unavailableBackend) Start(context.Context, runner.Spec) error {
	return errors.New("unexpected Start")
}
func (unavailableBackend) Stop(context.Context, string) error       { return errors.New("unexpected Stop") }
func (unavailableBackend) Wait(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
func (unavailableBackend) Active(context.Context) ([]string, error) {
	return nil, errors.New("bus unavailable")
}

func TestBootObservationFailureStopsDaemon(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "0001")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "job")
	if err := os.WriteFile(sentinel, []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	b := unavailableBackend{}
	d := &daemon{cfg: &config.Config{Observability: config.Observability{Listen: "127.0.0.1:0"}, Runtime: config.Runtime{SlotStopTimeout: time.Second}}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), met: metrics.NewRegistry(), ss: &fakeScaleSet{}, backend: b}
	d.table = slot.NewTable(root, b, func(string) error { return errors.New("unexpected materialize") }, nil, t.TempDir(), d.log)
	defer d.close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := d.run(ctx); err == nil || !strings.Contains(err.Error(), "adopt") {
		t.Fatalf("want failed adoption, got %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal(err)
	}
}

func TestDesiredPushesRespectAcquireBackoff(t *testing.T) {
	var attempts atomic.Int32
	cfg := &config.Config{Capacity: config.Capacity{MinRunners: 1, MaxRunners: 1}, Runtime: config.Runtime{SlotStopTimeout: time.Second}, Observability: config.Observability{Listen: "127.0.0.1:0"}}
	d := &daemon{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), met: metrics.NewRegistry(), backend: process.New(process.Options{}), desired: &desiredCell{}, reconCh: make(chan struct{}, 1), reconcileInterval: time.Millisecond, sessionUp: make(chan struct{})}
	d.desired.Store(1)
	d.ss = &fakeScaleSet{runFn: func(ctx context.Context, _ *fakeScaleSet) error {
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				d.requestReconcile()
			}
		}
	}}
	d.table = slot.NewTable(t.TempDir(), d.backend, func(string) error { attempts.Add(1); return errors.New("payload unavailable") }, nil, t.TempDir(), d.log, slot.WithEventHook(d.onSlotEvent))
	d.rec = reconcile.New(d.table, 1, 1, d.log)
	defer d.close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := d.run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("made %d attempts during the first backoff interval", n)
	}
}

func TestQueuedHintsExpireAndAreBounded(t *testing.T) {
	d := &daemon{}
	refs := make([]string, maxQueuedRefs+10)
	for i := range refs {
		refs[i] = fmt.Sprintf("workflow-%d", i)
	}
	d.noteQueued(refs)
	if len(d.queuedRefs) != maxQueuedRefs {
		t.Fatalf("queue hint bound: %d", len(d.queuedRefs))
	}
	d.queuedMu.Lock()
	for ref := range d.queuedAt {
		d.queuedAt[ref] = time.Now().Add(-2 * queuedHintTTL)
	}
	d.queuedMu.Unlock()
	d.noteQueued([]string{"fresh"})
	if len(d.queuedRefs) != 1 || d.queuedRefs["fresh"] != 1 {
		t.Fatalf("stale hints retained: %d", len(d.queuedRefs))
	}
	d.consumeQueuedRef("fresh")
	if len(d.queuedRefs) != 0 || len(d.queuedAt) != 0 {
		t.Fatal("consumed hint retained")
	}
}
