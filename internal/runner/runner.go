// Package runner defines how a runner process is started on a slot: the
// JIT config material and the provisioning backend contract.
package runner

import "context"

// JIT is a minted just-in-time runner configuration. Encoded is a secret:
// never log it, never write it anywhere but a 0600 file on tmpfs.
type JIT struct {
	Encoded string // treat as secret
	// RunnerName is the name GitHub reports for this runner (used for
	// JobStarted/JobCompleted correlation).
	RunnerName string
}

// Spec describes one runner start: where it lives, who runs it, and the
// resource limits to apply. Backends map these onto their native
// mechanism (systemd properties, exec attrs, ...).
type Spec struct {
	SlotDir   string // slot root (contains run.sh, bin/, _work/)
	JITPath   string // 0600 file holding the encoded JIT config
	EnvFile   string // systemd EnvironmentFile with PATH/HOME/mise
	User      string // unix user for the runner process
	Group     string // unix group ("" = same as user)
	CPUQuota  string // e.g. "400%" (systemd CPUQuota syntax)
	MemoryMax string // e.g. "8G" (systemd MemoryMax syntax)
	UnitName  string // e.g. "tentacle-0001.service"
}

// Backend starts and stops runner processes. Implementations:
// systemd transient units (production) and a plain child process
// (tests/dev). All methods are safe for concurrent use.
type Backend interface {
	// Start launches run.sh in the slot described by spec. It returns
	// once the process/unit has been forked (Type=exec semantics: at
	// least exec'd), not when the runner is idle.
	Start(ctx context.Context, spec Spec) error
	// Stop terminates the unit/process and returns after it is gone.
	Stop(ctx context.Context, unit string) error
	// Wait blocks until the unit/process has exited.
	Wait(ctx context.Context, unit string) error
	// Active lists currently-running unit names owned by this backend
	// (e.g. tentacle-0001.service). Used for boot adoption.
	Active(ctx context.Context) ([]string, error)
}
