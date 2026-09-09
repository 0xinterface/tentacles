// Package slot tracks the runner slots on this host: allocation,
// lifecycle state, and the manager contract the reconciler drives.
package slot

import (
	"context"
	"time"
)

// ID is a short, stable, zero-padded slot identifier: "0001" … "00NN".
type ID string

// State is the lifecycle state of a slot.
type State string

const (
	StateEmpty    State = "empty"
	StateStarting State = "starting"
	StateIdle     State = "idle" // run.sh up, not yet busy
	StateBusy     State = "busy" // JobStarted seen, or process running after claim
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

// Live reports whether the slot counts toward the "actual" runner count.
func (s State) Live() bool {
	return s == StateStarting || s == StateIdle || s == StateBusy
}

// Event is a slot lifecycle event reported through the event hook. The
// zero Event is not a valid event.
type Event string

const (
	// EventStarted fires once per successful slot start, when the slot
	// reaches idle.
	EventStarted Event = "started"
	// EventExited fires when a runner process exits on its own after
	// having run a job (or past the acquire grace without one).
	EventExited Event = "exited"
	// EventStopped fires when the Table stops a surplus slot.
	EventStopped Event = "stopped"
	// EventAcquireFailure fires when a slot start fails, or a runner
	// exits before claiming a job within the acquire grace (plan §13).
	EventAcquireFailure Event = "acquire_failure"
)

// Slot is one runner position on the host.
type Slot struct {
	ID         ID
	Dir        string
	Unit       string // backend unit/process name, e.g. tentacle-0001.service
	RunnerName string // name registered with GitHub (JIT runner name)
	State      State
	StartedAt  time.Time
	JITPath    string
}

// Manager owns the slot table. The reconciler is the only writer; the
// manager must still be safe for concurrent reads (metrics, listing).
type Manager interface {
	// Desired is the last desired runner count pushed by the listener.
	Desired() int
	// Active returns slots in starting|idle|busy.
	Active() []Slot
	// Ensure converges the live slot count toward desired: starting
	// missing slots (sequentially) and stopping surplus idle slots.
	// It must be idempotent: redelivery of the same desired count is a
	// no-op. Busy slots are never stopped.
	Ensure(ctx context.Context, desired int) error
	// MarkBusy flags the slot whose runner claimed a job so scale-down
	// will not kill it.
	MarkBusy(id ID)
	// ObserveExit records a runner process exit and releases the slot
	// for reuse (after cleanup).
	ObserveExit(id ID, err error)
}
