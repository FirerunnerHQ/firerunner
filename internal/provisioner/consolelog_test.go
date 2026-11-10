package provisioner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A guest that writes without end to its console fills its log file only up
// to the cap, which ends with a note saying the rest was dropped; a
// directory budget with room for it changes nothing.
func TestConsoleLogIsCapped(t *testing.T) {
	for _, dirMax := range []int64{0, 1000} {
		t.Run(fmt.Sprintf("dir-max-%d", dirMax), func(t *testing.T) {
			dir := t.TempDir()
			f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 100, LogDirMaxBytes: dirMax}, testLogger())
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
		})
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

// dirLogBytes is what the .log files in dir take.
func dirLogBytes(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Error(err)
		return 0
	}
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Many guests writing at once share the directory budget: together their
// logs never take more than it, though each alone may write its full cap,
// and the old logs that make room for them are removed.
func TestConcurrentConsoleLogsShareDirBudget(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.log")
	if err := os.WriteFile(old, bytes.Repeat([]byte("z"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	const budget, perFile, guests = 3000, 1000, 8
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: perFile, LogDirMaxBytes: budget}, testLogger())
	ws := make([]io.Writer, guests)
	closers := make([]func(), guests)
	for i := range guests {
		w, closeFn, err := f.openConsole(fmt.Sprintf("r%d", i), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ws[i], closers[i] = w, closeFn
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range guests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 40 {
				if _, err := io.WriteString(ws[i], strings.Repeat("x", 50)); err != nil {
					t.Errorf("console write failed: %v", err)
					return
				}
				if n := dirLogBytes(t, dir); n > budget {
					t.Errorf("logs take %d bytes while guests write, over the budget of %d", n, budget)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := range guests {
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("r%d.log", i))); err != nil {
			t.Errorf("open log r%d is gone: %v", i, err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old.log was kept (err %v); it should have made room", err)
	}
	if n := dirLogBytes(t, dir); n > budget {
		t.Errorf("logs take %d bytes, over the budget of %d", n, budget)
	}
	for _, c := range closers {
		c()
	}
	if n := dirLogBytes(t, dir); n > budget {
		t.Errorf("closed logs take %d bytes, over the budget of %d", n, budget)
	}
}

// Making room never removes a log that is still open, even the oldest: an
// unlinked file still being written takes disk no listing shows. A closed
// log goes instead.
func TestPruneKeepsOpenConsoleLogs(t *testing.T) {
	dir := t.TempDir()
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 100, LogDirMaxBytes: 400}, testLogger())
	wa, closeA, err := f.openConsole("a", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeA()
	_, _ = io.WriteString(wa, strings.Repeat("a", 100))
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.log"), past, past); err != nil {
		t.Fatal(err)
	}
	wb, closeB, err := f.openConsole("b", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(wb, strings.Repeat("b", 100))
	closeB()
	// a (open, oldest) and b (closed) take 200 and a's note may take more,
	// so c's full log does not fit beside both: one of them must go.
	wc, closeC, err := f.openConsole("c", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeC()
	_, _ = io.WriteString(wc, strings.Repeat("c", 100))
	if _, err := os.Stat(filepath.Join(dir, "a.log")); err != nil {
		t.Errorf("the open log a.log was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.log")); !os.IsNotExist(err) {
		t.Errorf("closed b.log was kept (err %v); it should have made room", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "c.log")); err != nil || string(got) != strings.Repeat("c", 100) {
		t.Errorf("c.log = %q, %v; want its 100 bytes", got, err)
	}
}

// When the open logs have spent the budget and no closed log is left to
// remove, a log stops with a note naming the directory budget.
func TestConsoleLogStopsAtDirBudget(t *testing.T) {
	dir := t.TempDir()
	const budget = 300
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 200, LogDirMaxBytes: budget}, testLogger())
	wa, closeA, err := f.openConsole("a", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wb, closeB, err := f.openConsole("b", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		_, _ = io.WriteString(wa, strings.Repeat("a", 30))
		_, _ = io.WriteString(wb, strings.Repeat("b", 30))
	}
	closeA()
	closeB()
	got, err := os.ReadFile(filepath.Join(dir, "b.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), consoleDirCutNote) {
		t.Errorf("b.log does not end with the directory note:\n%q", got)
	}
	if n := dirLogBytes(t, dir); n > budget {
		t.Errorf("logs take %d bytes, over the budget of %d", n, budget)
	}
}

// With no per-file cap, the directory budget still bounds a single log.
func TestConsoleLogDirBudgetWithoutFileCap(t *testing.T) {
	dir := t.TempDir()
	const budget = 500
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogDirMaxBytes: budget}, testLogger())
	w, closeFn, err := f.openConsole("r", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		_, _ = io.WriteString(w, strings.Repeat("x", 30))
	}
	closeFn()
	got, err := os.ReadFile(filepath.Join(dir, "r.log"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) > budget || !strings.HasSuffix(string(got), consoleDirCutNote) {
		t.Errorf("log is %d bytes (budget %d) and ends %q", len(got), budget, got[max(0, len(got)-80):])
	}
}

// Opening a log makes room for a full one at once, before the guest
// writes, as it did before the budget was shared.
func TestOpenConsoleLogMakesRoomForFullLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"old.log", "mid.log", "new.log"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("z"), 300), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	// 900 bytes of logs, a budget of 1000 and 300 per log.
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 300, LogDirMaxBytes: 1000}, testLogger())
	_, closeFn, err := f.openConsole("next", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	for name, want := range map[string]bool{"old.log": false, "mid.log": true, "new.log": true, "next.log": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}

// A log still open is not truncated by a second open under its name, which
// would leave the budget counting bytes no longer on disk.
func TestConsoleLogOpenTwiceRefused(t *testing.T) {
	dir := t.TempDir()
	f := NewFirecracker(FirecrackerConfig{LogDir: dir, LogMaxBytes: 100, LogDirMaxBytes: 1000}, testLogger())
	w, closeFn, err := f.openConsole("r", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "kept")
	if _, _, err := f.openConsole("r", nil, nil); err == nil {
		t.Fatal("second open of an open log succeeded")
	}
	closeFn()
	if got, err := os.ReadFile(filepath.Join(dir, "r.log")); err != nil || string(got) != "kept" {
		t.Errorf("r.log = %q, %v; want what the first writer wrote", got, err)
	}
	// Once closed, the name may be used again.
	if _, closeFn, err := f.openConsole("r", nil, nil); err != nil {
		t.Errorf("reopen after close: %v", err)
	} else {
		closeFn()
	}
}
