package provisioner

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A guest that writes without end to its console fills its log file only up
// to the cap, which ends with a note saying the rest was dropped.
func TestConsoleLogIsCapped(t *testing.T) {
	dir := t.TempDir()
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 100}, testLogger())
	w, closeFn, err := f.openConsole("r1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, err := io.WriteString(w, strings.Repeat("x", 30)); err != nil {
			t.Fatalf("console write failed, which would stop the console copy: %v", err)
		}
	}
	closeFn()
	got, err := os.ReadFile(filepath.Join(dir, "r1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("x", 100) + consoleCutNote; string(got) != want {
		t.Fatalf("log is %d bytes, want the cap of 100 then the note:\n%q", len(got), got)
	}
	info, err := os.Stat(filepath.Join(dir, "r1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode %o, want 600: the console can carry what a job printed", perm)
	}
}

// With no cap, every byte is kept.
func TestConsoleLogUncapped(t *testing.T) {
	var b bytes.Buffer
	w := newCappedWriter(&b, 0)
	_, _ = io.WriteString(w, strings.Repeat("y", 1000))
	if b.Len() != 1000 {
		t.Fatalf("kept %d of 1000 bytes with no cap", b.Len())
	}
}

// Old logs are removed, oldest first, so the directory stays within its
// budget with room for the next runner's log.
func TestOldConsoleLogsArePruned(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"old.log", "mid.log", "new.log"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("z"), 100), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), bytes.Repeat([]byte("n"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	// 300 bytes of logs, a budget of 350 and 100 per log: room for the new
	// one means at most 250, so the oldest goes.
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 100, LogDirMaxBytes: 350}, testLogger())
	_, closeFn, err := f.openConsole("next", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	closeFn()
	for name, want := range map[string]bool{"old.log": false, "mid.log": true, "new.log": true, "next.log": true, "notes.txt": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}
