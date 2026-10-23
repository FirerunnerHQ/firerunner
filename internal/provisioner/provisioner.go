// Package provisioner turns a JIT runner config into a running, ephemeral
// microVM and reaps it when the job is done.
package provisioner

import (
	"context"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// Provisioner boots exactly one ephemeral microVM per call and blocks until it
// exits. The GitHub runner inside the guest is JIT/ephemeral: it picks up a
// single job, then the guest issues `reboot -f`, which makes Firecracker exit
// (Firecracker's VMM terminates on guest reboot, not on halt/poweroff).
//
// Implementations MUST clean up all per-job resources (rootfs clone, tap
// device, API socket) before returning, whether the VM exited cleanly or the
// context was cancelled.
type Provisioner interface {
	// Launch boots a microVM for the given runner name and JIT config and
	// blocks until the VM exits or ctx is cancelled. onBusy, if non-nil, is
	// called once the guest runner dequeues its job (detected from the serial
	// console); the scheduler uses it to protect an in-flight VM from the
	// shutdown drain. onBusy may be called at most once and may be nil.
	Launch(ctx context.Context, name, jitConfig string, spec core.RunnerSpec, onBusy func()) error
	// Name returns the provisioner implementation name (for logging).
	Name() string
}

// SlotReporter is implemented by provisioners whose Launch draws from a
// host-wide pool that every tier shares (the --max-runners slot budget). The
// scheduler uses it to tell GitHub how many jobs a tier can really hold right
// now, rather than the tier's static max, so a host at the pool cap stops being
// assigned work that a sibling host could run.
type SlotReporter interface {
	// FreeSlots returns the number of microVMs that could be launched right
	// now before the shared pool is exhausted.
	FreeSlots() int
}
