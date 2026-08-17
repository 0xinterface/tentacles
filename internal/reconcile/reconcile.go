// Package reconcile turns the listener's desired runner count into
// slot-table actions, clamping it to the configured capacity window.
package reconcile

import (
	"context"
	"log/slog"

	"github.com/hkust/gh-runnerd/internal/slot"
)

// Clamp bounds v to [lo, hi]. The caller is expected to pass lo <= hi;
// if lo > hi the range is empty and hi is returned.
func Clamp(v, lo, hi int) int {
	if lo > hi {
		return hi
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Manager is the slot-table surface the reconciler drives: the slot
// Manager contract plus SetDesired, which records the clamped desired
// count so it is observable while convergence is in flight.
type Manager interface {
	slot.Manager
	SetDesired(n int)
}

// Reconciler clamps the raw desired runner count into the capacity
// window and converges the slot table toward it. Reconcile is
// single-threaded by design: the listener pushes desired counts, and the
// slot table's own watcher goroutines push process exits; both funnel
// into Reconcile calls.
type Reconciler struct {
	mgr        Manager
	minRunners int
	maxRunners int
	log        *slog.Logger
}

// New returns a Reconciler that drives mgr within the capacity window
// [minRunners, maxRunners].
func New(mgr Manager, minRunners, maxRunners int, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{mgr: mgr, minRunners: minRunners, maxRunners: maxRunners, log: log}
}

// Reconcile clamps desiredRaw into [minRunners, maxRunners], records the
// clamped value on the manager, and converges the table toward it.
func (r *Reconciler) Reconcile(ctx context.Context, desiredRaw int) error {
	desired := Clamp(desiredRaw, r.minRunners, r.maxRunners)
	r.log.Info("reconcile", "desired", desired, "desired_raw", desiredRaw)
	r.mgr.SetDesired(desired)
	return r.mgr.Ensure(ctx, desired)
}

// Plan is the pure decision helper for a reconcile tick. Given the
// desired count and the live slot states (starting|idle|busy), it
// returns how many slots to start and the indexes into live to stop.
// Surplus slots are stopped only when idle or starting (a starting slot
// is by definition past the brief provision call and has not claimed a
// job); busy slots are never stopped. When fewer slots must stop than
// there are idle ones, the preference order (oldest first) is the
// caller's job — Plan returns indexes in live order.
func Plan(desired int, live []slot.State) (starts int, stops []int) {
	n := len(live)
	if n < desired {
		return desired - n, nil
	}
	if n == desired {
		return 0, nil
	}
	excess := n - desired
	// Prefer idle slots over starting ones; never busy.
	for _, pass := range []slot.State{slot.StateIdle, slot.StateStarting} {
		for i, st := range live {
			if len(stops) == excess {
				return 0, stops
			}
			if st == pass {
				stops = append(stops, i)
			}
		}
	}
	return 0, stops
}
