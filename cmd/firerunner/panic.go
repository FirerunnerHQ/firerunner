package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"
)

// errPanicked marks a panic recovered in a tenant's loop or a tier's run:
// that tenant, or the install, starts again; the others keep serving.
var errPanicked = errors.New("panicked")

// exitProcess ends the daemon; tests replace it.
var exitProcess = os.Exit

// guardAfterPanic bounds what follows a recovered panic. The panic may have
// left a lock held (much of the scheduler unlocks without defer), and a
// stop that waits on it would hang for ever where the panic used to end the
// process. Unless settled closes within limit, it logs and exits non-zero,
// for the supervisor to restart the daemon: no worse than the crash it
// replaces. A nil settled never closes; a limit of 0 is jobLifetimeSlack.
// The channel it returns closes once the guard is done.
func guardAfterPanic(settled <-chan struct{}, limit time.Duration, what string, log *slog.Logger) <-chan struct{} {
	if limit <= 0 {
		limit = jobLifetimeSlack
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTimer(limit)
		defer t.Stop()
		select {
		case <-settled:
		case <-t.C:
			select {
			case <-settled: // settled as the time ran out
			default:
				log.Error("still not stopped after a recovered panic; exiting for a restart", "in", what, "after", limit)
				exitProcess(1)
			}
		}
	}()
	return done
}

// recovered logs a recovered panic with its stack, and returns it as an
// error. what says where it happened.
func recovered(p any, what string, log *slog.Logger) error {
	log.Error("recovered a panic: starting again; this is a bug, report it with this stack",
		"in", what, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
	return fmt.Errorf("%s %w: %v", what, errPanicked, p)
}
