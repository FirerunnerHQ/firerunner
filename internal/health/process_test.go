package health

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func TestReadProcess(t *testing.T) {
	start := time.Date(2026, 11, 14, 3, 0, 0, 0, time.FixedZone("x", 3600))
	// Burn a little CPU so the rusage reading has something to show.
	deadline := time.Now().Add(20 * time.Millisecond)
	for n := 0; time.Now().Before(deadline); n++ {
		_ = n * n
	}
	block := make(chan struct{})
	for range 10 {
		go func() { <-block }()
	}
	defer close(block)
	f, err := os.Open(os.DevNull) // one more descriptor than before
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	p := ReadProcess(start)
	if !p.StartedAt.Equal(start) || p.StartedAt.Location() != time.UTC {
		t.Errorf("StartedAt = %v, want %v in UTC", p.StartedAt, start)
	}
	if p.Goroutines < 11 {
		t.Errorf("Goroutines = %d, want at least the 10 blocked and this one", p.Goroutines)
	}
	if p.HeapBytes == 0 || p.GoBytes < p.HeapBytes {
		t.Errorf("HeapBytes = %d, GoBytes = %d: want a heap, inside what Go mapped", p.HeapBytes, p.GoBytes)
	}
	if p.CPUSeconds <= 0 {
		t.Errorf("CPUSeconds = %v, want the CPU spent", p.CPUSeconds)
	}
	if runtime.GOOS == "linux" && p.OpenFiles < 4 {
		t.Errorf("OpenFiles = %d, want stdin, stdout, stderr and %s at least", p.OpenFiles, os.DevNull)
	}
}
