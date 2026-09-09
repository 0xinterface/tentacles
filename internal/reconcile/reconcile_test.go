package reconcile

import (
	"context"
	"sync"
	"testing"

	"github.com/hkust/gh-runnerd/internal/slot"
)

// fakeManager records SetDesired/Ensure calls for Reconcile tests.
type fakeManager struct {
	mu      sync.Mutex
	desired int
	ensured []int
}

func (f *fakeManager) SetDesired(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desired = n
}

func (f *fakeManager) Desired() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.desired
}

func (f *fakeManager) Active() []slot.Slot { return nil }

func (f *fakeManager) Ensure(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, n)
	return nil
}

func (f *fakeManager) MarkBusy(slot.ID)           {}
func (f *fakeManager) ObserveExit(slot.ID, error) {}

func (f *fakeManager) lastEnsured() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ensured) == 0 {
		return -1
	}
	return f.ensured[len(f.ensured)-1]
}

func TestClamp(t *testing.T) {
	tests := []struct {
		name string
		v    int
		lo   int
		hi   int
		want int
	}{
		{"in range", 2, 1, 4, 2},
		{"below lo", 0, 1, 4, 1},
		{"above hi", 5, 1, 4, 4},
		{"at lo", 1, 1, 4, 1},
		{"at hi", 4, 1, 4, 4},
		{"equal bounds", 2, 4, 4, 4},
		{"zero range", 0, 0, 0, 0},
		{"empty range returns hi", 2, 3, 1, 1},
		{"negative below", -5, -2, 0, -2},
		{"negative above", 5, -2, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clamp(tt.v, tt.lo, tt.hi); got != tt.want {
				t.Errorf("Clamp(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
			}
		})
	}
}

func TestReconcileCallsSetDesiredWithClampedValue(t *testing.T) {
	mgr := &fakeManager{}
	r := New(mgr, 1, 4, nil)
	ctx := context.Background()

	// Above max: clamp to 4.
	if err := r.Reconcile(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if mgr.Desired() != 4 {
		t.Errorf("desired = %d, want 4", mgr.Desired())
	}
	if got := mgr.lastEnsured(); got != 4 {
		t.Errorf("Ensure called with %d, want 4", got)
	}

	// Below min: clamp to 1.
	if err := r.Reconcile(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if mgr.Desired() != 1 {
		t.Errorf("desired = %d, want 1", mgr.Desired())
	}
	if got := mgr.lastEnsured(); got != 1 {
		t.Errorf("Ensure called with %d, want 1", got)
	}

	// In range: passed through.
	if err := r.Reconcile(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if mgr.Desired() != 3 {
		t.Errorf("desired = %d, want 3", mgr.Desired())
	}
	if got := mgr.lastEnsured(); got != 3 {
		t.Errorf("Ensure called with %d, want 3", got)
	}
}

func TestReconcileMaxBelowMin(t *testing.T) {
	mgr := &fakeManager{}
	// Degenerate window: lo > hi. Clamp returns hi, so desired is 2.
	r := New(mgr, 4, 2, nil)
	if err := r.Reconcile(context.Background(), 99); err != nil {
		t.Fatal(err)
	}
	if mgr.Desired() != 2 {
		t.Errorf("desired = %d, want 2", mgr.Desired())
	}
}
