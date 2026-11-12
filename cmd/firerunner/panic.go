package main

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// errPanicked marks a panic recovered in a tenant's loop or a tier's run:
// that tenant, or the install, starts again; the others keep serving.
var errPanicked = errors.New("panicked")

// recovered logs a recovered panic with its stack, and returns it as an
// error. what says where it happened.
func recovered(p any, what string, log *slog.Logger) error {
	log.Error("recovered a panic: starting again; this is a bug, report it with this stack",
		"in", what, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
	return fmt.Errorf("%s %w: %v", what, errPanicked, p)
}
