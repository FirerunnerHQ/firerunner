package health

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrackerCountsFallbacks(t *testing.T) {
	tr := NewTracker("a")
	tr.FellBack("market")
	tr.FellBack("market")
	tr.FellBack("type")
	snap := tr.Snapshot()
	if got := snap.Fallbacks; got["market"] != 2 || got["type"] != 1 || len(got) != 2 {
		t.Fatalf("fallbacks = %v, want market 2, type 1", got)
	}
	tr.FellBack("subnet") // a snapshot is a copy, safe to read meanwhile
	if _, ok := snap.Fallbacks["subnet"]; ok {
		t.Fatal("a snapshot shares its fallbacks map with the tracker")
	}
}

func TestTrackerCountsInterruptions(t *testing.T) {
	tr := NewTracker("a")
	tr.Interrupted(true)
	tr.Interrupted(true)
	tr.Interrupted(false)
	snap := tr.Snapshot()
	if got := snap.Interruptions; got["job"] != 2 || got["idle"] != 1 || len(got) != 2 {
		t.Fatalf("interruptions = %v, want job 2, idle 1", got)
	}
	tr.Interrupted(false)
	if snap.Interruptions["idle"] != 1 {
		t.Fatal("a snapshot shares its interruptions map with the tracker")
	}
}

func TestTrackerTransitions(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewTracker("a")
	tr.now = func() time.Time { return now }

	if s := tr.Snapshot(); s.Name != "a" || s.State != Connected {
		t.Fatalf("initial = %+v", s)
	}
	now = now.Add(time.Second)
	tr.MessageOK()
	if s := tr.Snapshot(); !s.LastMessage.Equal(now) {
		t.Fatalf("LastMessage = %v, want %v", s.LastMessage, now)
	}

	now = now.Add(time.Second)
	tr.Reconnecting(errors.New("boom"), 4*time.Second)
	s := tr.Snapshot()
	if s.State != Reconnecting || s.BackoffSeconds != 4 || s.LastError != "boom" || !s.Since.Equal(now) {
		t.Fatalf("reconnecting = %+v", s)
	}
	// A second failure keeps Since at the start of the outage.
	start := now
	now = now.Add(time.Second)
	tr.Reconnecting(errors.New("again"), 8*time.Second)
	if s := tr.Snapshot(); !s.Since.Equal(start) || s.BackoffSeconds != 8 {
		t.Fatalf("second reconnect = %+v", s)
	}

	tr.Reconnected()
	s = tr.Snapshot()
	if s.State != Connected || s.Reconnects != 1 || s.BackoffSeconds != 0 || s.LastError != "again" {
		t.Fatalf("reconnected = %+v (the last error stays for diagnosis)", s)
	}

	tr.Failed(errors.New("401"))
	if s := tr.Snapshot(); s.State != Failed || s.LastError != "401" {
		t.Fatalf("failed = %+v", s)
	}
}

func TestWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	in := File{PID: 42, WrittenAt: time.Unix(2000, 0).UTC(), Tiers: []Tier{
		{Name: "a", State: Connected, Reconnects: 2, Since: time.Unix(1000, 0).UTC()},
		{Name: "b", State: Failed, LastError: "401 Unauthorized", Since: time.Unix(1500, 0).UTC()},
	}}
	if err := Write(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.PID != 42 || len(out.Tiers) != 2 || out.Tiers[1].State != Failed || out.Tiers[0].Reconnects != 2 {
		t.Fatalf("round trip = %+v", out)
	}
	// No temp files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want only the state file", len(entries))
	}
	if _, err := Read(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v", err)
	}
}
