//go:build !linux

package provisioner

// flushGuestConntrack is a no-op off Linux, where there are no microVMs.
func flushGuestConntrack(...string) error { return nil }
