// Package jobevent is the record of what happened to a job: when GitHub
// assigned it, what ran it, and how each stage went. It is what usage, limits
// and billing are built on, so an event names the job and the machine, and never
// holds a credential.
//
// Events come from the scale-set listener and the provisioner, which know
// different halves of a job. Join them on Runner, or on JobID where it is set.
package jobevent

import "time"

// Kind says what happened.
type Kind string

// Kinds, roughly in the order a job meets them.
const (
	Assigned     Kind = "assigned"      // GitHub gave the job to the scale set
	Launched     Kind = "launched"      // a machine was started for a runner
	Served       Kind = "served"        // the runner received its config
	Listening    Kind = "listening"     // the runner connected to GitHub
	Running      Kind = "running"       // the runner took a job
	Exited       Kind = "exited"        // the runner process ended
	Started      Kind = "started"       // GitHub reports the job started
	Completed    Kind = "completed"     // GitHub reports the job ended
	InstanceGone Kind = "instance_gone" // the machine went away
)

// Event is one thing that happened. Fields a source does not know stay empty.
type Event struct {
	Kind Kind
	At   time.Time
	Tier string

	Runner string

	// From GitHub.
	JobID      string
	Repository string // owner/name
	Workflow   string // the job's workflow ref
	JobName    string
	EventName  string // push, pull_request, ...
	RunID      int64
	Labels     []string
	QueuedAt   time.Time // zero on a JIT scale set: its messages omit it
	Result     string

	// From the provisioner.
	Instance     string
	InstanceType string
	Market       string // spot or on-demand
	Subnet       string
	Zone         string
	Path         string // cold, pool or kept: how the machine was supplied
	ExitCode     int
	Reason       string
}

// Sink takes events. Record is called on the launch path and the listener's
// message loop, so it must not block or fail: a slow store loses detail, never a
// job. A nil Sink records nothing.
type Sink interface {
	Record(Event)
}

// Record sends e to s, stamping the time if e has none.
func Record(s Sink, e Event) {
	if s == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.Record(e)
}
