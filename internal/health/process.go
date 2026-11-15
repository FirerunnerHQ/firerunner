package health

import (
	"os"
	"runtime/metrics"
	"syscall"
	"time"
)

// Process is the daemon's own use of its machine: what sizing how many
// tenants and tiers one daemon serves is measured from.
type Process struct {
	StartedAt  time.Time `json:"started_at"`
	Goroutines uint64    `json:"goroutines"`
	// HeapBytes is the heap's objects, live and not yet swept; GoBytes is
	// all the memory the Go runtime has mapped from the OS.
	HeapBytes uint64 `json:"heap_bytes"`
	GoBytes   uint64 `json:"go_bytes"`
	// CPUSeconds is the user and system CPU time used since the start.
	CPUSeconds float64 `json:"cpu_seconds"`
	// OpenFiles counts open file descriptors, sockets included; 0 where
	// /proc/self/fd cannot be read.
	OpenFiles int `json:"open_files,omitempty"`
}

var processSamples = []metrics.Sample{
	{Name: "/sched/goroutines:goroutines"},
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/memory/classes/total:bytes"},
}

// ReadProcess reports this process, started at start. It does not stop the
// world, so it is cheap enough for every health write.
func ReadProcess(start time.Time) Process {
	s := make([]metrics.Sample, len(processSamples))
	copy(s, processSamples)
	metrics.Read(s)
	p := Process{StartedAt: start.UTC()}
	if v := s[0].Value; v.Kind() == metrics.KindUint64 {
		p.Goroutines = v.Uint64()
	}
	if v := s[1].Value; v.Kind() == metrics.KindUint64 {
		p.HeapBytes = v.Uint64()
	}
	if v := s[2].Value; v.Kind() == metrics.KindUint64 {
		p.GoBytes = v.Uint64()
	}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		p.CPUSeconds = tv(ru.Utime) + tv(ru.Stime)
	}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		p.OpenFiles = len(fds) - 1 // ReadDir's own descriptor
	}
	return p
}

func tv(t syscall.Timeval) float64 {
	return float64(t.Sec) + float64(t.Usec)/1e6
}
