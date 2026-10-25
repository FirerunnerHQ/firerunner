// Package health tracks each tier's listener connection and publishes it to a
// state file, so `status` and `doctor` (separate processes) can see a tier that
// is reconnecting or has failed while the daemon keeps serving the others.
package health

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is a tier listener's connection state.
type State string

const (
	// Connected: a message session is open and being polled.
	Connected State = "connected"
	// Reconnecting: the session ended and a new one is being opened.
	Reconnecting State = "reconnecting"
	// Failed: the tier stopped on an error that retrying cannot fix. Rejected
	// credentials stop only this tier; a deleted scale set restarts the daemon.
	Failed State = "failed"
)

// Tier is one tier's listener health.
type Tier struct {
	Name  string `json:"name"`
	State State  `json:"state"`
	// Reconnects counts sessions re-opened since the daemon started.
	Reconnects int `json:"reconnects"`
	// BackoffSeconds is the wait before the next reconnect attempt, while
	// reconnecting.
	BackoffSeconds float64 `json:"backoff_seconds,omitempty"`
	// LastMessage is when GetMessage last succeeded (a message or an empty
	// long poll).
	LastMessage time.Time `json:"last_message,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Since       time.Time `json:"since"`

	// GitHub is the scale set's statistics from the latest message, with
	// firerunner's own count of runners for comparison.
	GitHub *GitHubStats `json:"github,omitempty"`
	// Drift describes any current disagreement between GitHub and
	// firerunner; empty when they agree.
	Drift string `json:"drift,omitempty"`
	// Recycles counts sessions the reconciler recycled to recover jobs.
	Recycles int `json:"recycles,omitempty"`
	// LostRunners counts runners cancelled because GitHub no longer listed
	// them.
	LostRunners int `json:"lost_runners,omitempty"`
	// OfflineRemoved counts offline runners removed because firerunner did
	// not track them.
	OfflineRemoved int `json:"offline_removed,omitempty"`
	// OfflineWouldRemove is how many the last dry-run sweep would have
	// removed.
	OfflineWouldRemove int `json:"offline_would_remove,omitempty"`
	// Sizes are the hot runners and pool size the tier's schedule set last;
	// nil for a tier without a schedule.
	Sizes *Sizes `json:"sizes,omitempty"`
	// Pending is the jobs GitHub assigned that have not started, when any.
	Pending *Pending `json:"pending,omitempty"`
	// Launches counts EC2 launches by path: "pool", "cold", or "kept" (a
	// cold launch that left the pool for jobs ec2.pool_for names).
	Launches map[string]int `json:"launches,omitempty"`
}

// Pending is a tier's assigned-but-not-started jobs.
type Pending struct {
	Jobs    int            `json:"jobs"`
	ByEvent map[string]int `json:"by_event,omitempty"`
	// Kept is how many ec2.pool_for keeps the pool for.
	Kept int `json:"kept,omitempty"`
}

// Sizes are a scheduled tier's current sizes and the window that set them
// (empty: outside every window, at the tier's base sizes).
type Sizes struct {
	Min    int       `json:"min"`
	Pool   int       `json:"pool"`
	Window string    `json:"window,omitempty"`
	Since  time.Time `json:"since"`
}

// GitHubStats is a scale set's statistics as GitHub reported them.
type GitHubStats struct {
	Available  int       `json:"available"`
	Acquired   int       `json:"acquired"`
	Assigned   int       `json:"assigned"`
	Running    int       `json:"running"`
	Registered int       `json:"registered"`
	Busy       int       `json:"busy"`
	Idle       int       `json:"idle"`
	At         time.Time `json:"at"`
	// Tracked is how many runners firerunner itself has live.
	Tracked int `json:"tracked"`
}

// Tracker records one tier's health. It is safe for concurrent use.
type Tracker struct {
	mu  sync.Mutex
	t   Tier
	now func() time.Time
}

// NewTracker returns a tracker for the named tier, starting connected.
func NewTracker(name string) *Tracker {
	tr := &Tracker{now: time.Now}
	tr.t = Tier{Name: name, State: Connected, Since: tr.now()}
	return tr
}

func (tr *Tracker) set(s State) {
	if tr.t.State != s {
		tr.t.State = s
		tr.t.Since = tr.now()
	}
}

// MessageOK records a successful GetMessage.
func (tr *Tracker) MessageOK() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.LastMessage = tr.now()
	tr.t.BackoffSeconds = 0
	tr.set(Connected)
}

// Reconnecting records that the session ended with err and the next attempt
// waits backoff.
func (tr *Tracker) Reconnecting(err error, backoff time.Duration) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.LastError = errString(err)
	tr.t.BackoffSeconds = backoff.Seconds()
	tr.set(Reconnecting)
}

// Reconnected records a newly opened session.
func (tr *Tracker) Reconnected() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.Reconnects++
	tr.t.BackoffSeconds = 0
	tr.set(Connected)
}

// Failed records a fatal error; the tier no longer listens.
func (tr *Tracker) Failed(err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.LastError = errString(err)
	tr.t.BackoffSeconds = 0
	tr.set(Failed)
}

// GitHubStats records the latest statistics and firerunner's own count.
func (tr *Tracker) GitHubStats(st GitHubStats) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.GitHub = &st
}

// SetDrift records the current drift description; empty clears it.
func (tr *Tracker) SetDrift(d string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.Drift = d
}

// SetSizes records the sizes a schedule applied.
func (tr *Tracker) SetSizes(sz Sizes) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.Sizes = &sz
}

// SetPending records the pending jobs; nil or empty clears them.
func (tr *Tracker) SetPending(p *Pending) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if p != nil && p.Jobs == 0 {
		p = nil
	}
	tr.t.Pending = p
}

// Launched counts an EC2 launch by path.
func (tr *Tracker) Launched(path string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.t.Launches == nil {
		tr.t.Launches = map[string]int{}
	}
	tr.t.Launches[path]++
}

// OfflineRemoved counts an untracked offline runner removed.
func (tr *Tracker) OfflineRemoved() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.OfflineRemoved++
}

// OfflineWouldRemove records what a dry-run sweep would have removed.
func (tr *Tracker) OfflineWouldRemove(n int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.OfflineWouldRemove = n
}

// Recycled counts a session recycled by the reconciler.
func (tr *Tracker) Recycled() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.Recycles++
}

// LostRunner counts a runner cancelled because GitHub no longer listed it.
func (tr *Tracker) LostRunner() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.t.LostRunners++
}

// Snapshot returns the current health.
func (tr *Tracker) Snapshot() Tier {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	t := tr.t
	if t.GitHub != nil {
		g := *t.GitHub
		t.GitHub = &g
	}
	if t.Pending != nil {
		p := *t.Pending
		p.ByEvent = maps.Clone(p.ByEvent)
		t.Pending = &p
	}
	t.Launches = maps.Clone(t.Launches)
	return t
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// File is the published state of every tier in one daemon.
type File struct {
	PID       int       `json:"pid"`
	WrittenAt time.Time `json:"written_at"`
	Tiers     []Tier    `json:"tiers"`
	// Reaper is the EC2 reaper's latest sweep, when one runs.
	Reaper *Reaper `json:"reaper,omitempty"`
}

// Reaper is what the EC2 reaper's latest sweep did.
type Reaper struct {
	At           time.Time `json:"at"`
	DryRun       bool      `json:"dry_run,omitempty"`
	Instances    []string  `json:"instances,omitempty"`
	SpotRequests []string  `json:"spot_requests,omitempty"`
	Busy         []string  `json:"busy,omitempty"`
	Young        []string  `json:"young,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// FileName is the state file's name inside the daemon's work dir.
const FileName = "listener-health.json"

// Write publishes f at path atomically, so a reader never sees a partial file.
func Write(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".listener-health-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Read loads the state file at path.
func Read(path string) (File, error) {
	var f File
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}
