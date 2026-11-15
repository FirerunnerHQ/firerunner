package diag

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// status shows the daemon's own goroutines, memory, files and CPU from its
// state file, and nothing for a daemon too old to write them.
func TestProcessInStatus(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	write := func(p *health.Process) {
		t.Helper()
		if err := health.Write(filepath.Join(dir, health.FileName), health.File{PID: 1, WrittenAt: now, Process: p}); err != nil {
			t.Fatal(err)
		}
	}
	write(&health.Process{StartedAt: now.Add(-2 * time.Hour), Goroutines: 412, HeapBytes: 24 << 20, GoBytes: 61 << 20, CPUSeconds: 93.25, OpenFiles: 57})
	want := "process: 412 goroutines, 24.0 MiB heap (61.0 MiB from the OS), 57 open files, 93.2s CPU in 2h0m up"
	if text := statusText(t, cfg); !strings.Contains(text, want) {
		t.Errorf("status lacks %q\n%s", want, text)
	}
	write(nil)
	if text := statusText(t, cfg); strings.Contains(text, "process:") {
		t.Errorf("status shows a process line for a daemon that wrote none\n%s", text)
	}
}
