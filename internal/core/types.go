// Package core holds the shared domain types for firerunner.
package core

import "errors"

// ErrRunnerBusy reports that GitHub refused to deregister a runner because it
// has already assigned that runner a job, even if the guest has not started it
// yet. The runner must be left to run the job.
var ErrRunnerBusy = errors.New("runner has a job assigned")

// ErrRemovalUnknown reports that a runner deregistration failed in a way that
// leaves open whether GitHub applied it (a timeout or lost response), and
// asking GitHub afterwards failed too. The runner may already be gone, so its
// VM must not be treated as able to take a job until that is settled.
var ErrRemovalUnknown = errors.New("runner removal outcome unknown")

// ErrHandedOff is the cause of a busy runner's cancelled launch when the
// process exits and leaves the runner's machine to the next one: the
// provisioner returns without ending the machine. Only a provisioner whose
// machines outlive the process, and that a later process finds again, may be
// given it.
var ErrHandedOff = errors.New("runner left to the next firerunner")

// RunnerSpec describes the shape of the ephemeral microVM that will host a
// single GitHub Actions job. One spec maps to one scale-set tier (e.g. the
// runs-on label "firerunner-4c8g" -> 4 vCPU / 8192 MiB / a Docker-less golden
// image).
type RunnerSpec struct {
	// Labels are the additional runs-on labels advertised for this tier.
	Labels []string
	// VCPU is the number of virtual CPUs given to the microVM.
	VCPU int
	// MemMiB is the guest memory size in MiB.
	MemMiB int
	// RootFS is the path to the immutable golden ext4 image for this tier. It
	// is never mutated; each job gets a reflink (copy-on-write) clone.
	RootFS string
	// ToolCache, when set, is the host path to a read-only "hostedtoolcache"
	// ext4 image attached to this tier's microVMs. It overrides the
	// process-global --toolcache for this tier; empty means fall back to the
	// global default (or no tool cache when neither is set).
	ToolCache string
}
