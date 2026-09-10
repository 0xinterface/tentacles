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
	// exits before claiming a job within the acquire grace.
	EventAcquireFailure Event = "acquire_failure"
	// EventAdmissionHold fires when the admission gate declines a new
	// slot because predicted usage exceeds the host budget. It is
	// backpressure, not a failure: the start is retried on a later tick.
	EventAdmissionHold Event = "admission_hold"
)

// Usage is one sample of a slot unit's resource accounting.
type Usage struct {
	CPUSeconds      float64 // cumulative CPU time since unit start
	PeakMemBytes    uint64  // peak memory of the unit
	CurrentMemBytes uint64  // current memory, used to avoid double-counting MemAvailable
}

// ClaimJob is the workflow identity of the job a runner just claimed
// (from the JobStarted scale-set message).
type ClaimJob struct {
	RunnerName       string
	WorkflowRef      string // owner/repo/.github/workflows/x.yml@ref
	RunID            int64
	QueueWaitSeconds float64 // GitHub-reported queue time, 0 when unknown
}

// Completion is the usage record of one finished job: the identity it
// was claimed with plus the measured resource consumption of its slot
// unit. It is the input to the per-workflow usage history.
type Completion struct {
	ID               ID
	RunnerName       string
	WorkflowRef      string
	RunID            int64
	Result           string // empty: the result may arrive after exit
	CPUSeconds       float64
	PeakMemBytes     uint64
	WallSeconds      float64
	QueueWaitSeconds float64
	// Sampled reports whether the backend supplied resource accounting.
	// CPU and memory are zero-values when false.
	Sampled bool
}

// Slot is one runner position on the host.
type Slot struct {
	ID               ID
	Dir              string
	Unit             string // backend unit/process name, e.g. tentacle-0001.service
	RunnerName       string // name registered with GitHub (JIT runner name)
	WorkflowRef      string // workflow the current job belongs to, set on claim
	RunID            int64
	State            State
	StartedAt        time.Time
	JITPath          string
	CurrentMemBytes  uint64
	ReservedCores    float64
	ReservedMemBytes uint64
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
