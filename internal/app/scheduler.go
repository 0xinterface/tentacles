package app

import (
	"context"
	"fmt"
	"time"

	"github.com/0xinterface/tentacles/internal/slot"
)

// reconcilePools first removes per-pool surplus, then allocates free host
// slots round-robin. Starts are serialized here, making Capacity.MaxRunners
// the single authoritative ceiling for the host.
func (d *daemon) reconcilePools(ctx context.Context) error {
	for _, pool := range d.pools {
		desired := pool.desired.Load()
		pool.table.SetDesired(desired)
		live := len(pool.table.Active())
		target := min(live, desired)
		if err := pool.table.Ensure(ctx, target); err != nil {
			return fmt.Errorf("pool %q maintenance: %w", pool.cfg.ID, err)
		}
	}

	total := len(d.allActive())
	if total >= d.cfg.Capacity.MaxRunners || len(d.pools) == 0 {
		return nil
	}

	withoutProgress := 0
	for total < d.cfg.Capacity.MaxRunners && withoutProgress < len(d.pools) {
		if err := ctx.Err(); err != nil {
			return err
		}
		pool := d.pools[d.scheduleCursor%len(d.pools)]
		d.scheduleCursor = (d.scheduleCursor + 1) % len(d.pools)
		before := len(pool.table.Active())
		if before >= pool.desired.Load() || pool.acquireRetryWait() > 0 {
			withoutProgress++
			continue
		}
		if err := pool.table.Ensure(ctx, before+1); err != nil {
			return fmt.Errorf("pool %q scale up: %w", pool.cfg.ID, err)
		}
		after := len(pool.table.Active())
		if after <= before {
			withoutProgress++
			continue
		}
		total += after - before
		withoutProgress = 0
	}
	return nil
}

func (d *daemon) allActive() []slot.Slot {
	active := make([]slot.Slot, 0, d.cfg.Capacity.MaxRunners)
	for _, pool := range d.pools {
		active = append(active, pool.table.Active()...)
	}
	return active
}

// admissionGate charges every live runner on the host, not only runners from
// the pool requesting the next slot.
func (d *daemon) admissionGate(pool *poolRuntime, live []slot.Slot) bool {
	if d.hist == nil || len(live) == 0 {
		return true
	}
	candidateCores, candidateMemory := pool.candidateEstimate()
	usedCores := candidateCores
	usedMemory := candidateMemory
	for _, runnerSlot := range live {
		if !runnerSlot.State.Live() {
			continue
		}
		cores := runnerSlot.ReservedCores
		memory := runnerSlot.ReservedMemBytes
		if runnerSlot.State == slot.StateBusy {
			cores, _ = d.hist.PredictCores(runnerSlot.Pool, runnerSlot.WorkflowRef)
			memory, _ = d.hist.PredictMem(runnerSlot.Pool, runnerSlot.WorkflowRef)
		} else if runnerSlot.Pool == pool.cfg.ID {
			// New hints can refine only this pool's unclaimed reservations.
			cores = max(cores, candidateCores)
			memory = max(memory, candidateMemory)
		}
		usedCores += cores
		if memory > runnerSlot.CurrentMemBytes {
			usedMemory += memory - runnerSlot.CurrentMemBytes
		}
	}
	cpuLimit := float64(d.numCPU()) * float64(d.cfg.Scaling.CPUTargetPercent) / 100
	if usedCores > cpuLimit {
		return false
	}
	if available, err := d.memAvailable(); err == nil {
		memoryLimit := uint64(
			float64(available) * float64(100-d.cfg.Scaling.MemoryMarginPercent) / 100,
		)
		if usedMemory > memoryLimit {
			return false
		}
	}
	return true
}

// nextRetryWait returns the earliest blocked pool retry that could satisfy
// unmet demand. Zero means no retry timer is needed.
func (d *daemon) nextRetryWait() time.Duration {
	var earliest time.Duration
	for _, pool := range d.pools {
		if len(pool.table.Active()) >= pool.desired.Load() {
			continue
		}
		wait := pool.acquireRetryWait()
		if wait <= 0 {
			continue
		}
		if earliest == 0 || wait < earliest {
			earliest = wait
		}
	}
	return earliest
}
