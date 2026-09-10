package slot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

type claimThenErrorBackend struct {
	fakeBackend
	claim func()
}

func (b *claimThenErrorBackend) Start(ctx context.Context, spec runner.Spec) error {
	if err := b.fakeBackend.Start(ctx, spec); err != nil {
		return err
	}
	b.claim()
	return errors.New("launch response lost")
}

func TestClaimSurvivesStartObservationError(t *testing.T) {
	b := &claimThenErrorBackend{}
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	b.claim = func() {
		if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12"}) {
			t.Fatal("claim missing")
		}
	}
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.stopped) > 0 {
		t.Fatalf("claimed runner was stopped after lost start reply: %v", b.stopped)
	}
}

func TestAdoptRejectsNondirectoryActiveSlot(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "0001")); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	tab := NewTable(root, b, markerMaterialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	err := tab.Adopt(context.Background(), []string{"tentacle-0001.service"})
	if err == nil && len(tab.Active()) == 0 {
		t.Fatal("adoption succeeded with active unit neither tracked nor stopped")
	}
}

func TestAdoptHonorsDeadlineWithRunnerFIFO(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "0001")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, ".runner")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	tab := NewTable(root, &fakeBackend{}, markerMaterialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tab.Adopt(ctx, []string{"tentacle-0001.service"}) }()
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		// Release the blocked reader so the probe leaves no worker behind.
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
		if err == nil {
			f.Close()
		}
		<-done
		t.Fatal("adoption ignored caller deadline while reading .runner FIFO")
	}
}

func TestCancelledEnsureDoesNotWaitForCleanupDeadline(t *testing.T) {
	calls := make(chan struct{}, 3)
	tab, _, _ := newTestTable(t, &fakeBackend{}, WithCleanupTimeout(200*time.Millisecond), WithDiagContext(func(ctx context.Context, _ Slot) error { calls <- struct{}{}; <-ctx.Done(); return ctx.Err() }))
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	tab.ObserveExit("0001", nil)
	<-calls
	eventually(t, time.Second, func() bool { tab.mu.Lock(); defer tab.mu.Unlock(); return !tab.slots["0001"].cleaning })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = tab.Ensure(ctx, 0)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled Ensure waited %v for unrelated cleanup timeout", elapsed)
	}
}
