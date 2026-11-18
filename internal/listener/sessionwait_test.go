package listener

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sessionOpener answers a session conflict the first busy times, then opens.
func sessionOpener(busy int, other error) (func(context.Context) (int, error), *int) {
	tries := 0
	return func(context.Context) (int, error) {
		tries++
		if other != nil {
			return 0, other
		}
		if tries <= busy {
			return 0, errors.New("409 Conflict: the runner scale set already has an active session")
		}
		return tries, nil
	}, &tries
}

// TryOpenSession opens once: false while another holds the session, an
// error for anything else; ReleaseSession closes one opened, for another to
// take, and Close and CloseSession hold no session to close.
func TestTryAndReleaseSession(t *testing.T) {
	held := true
	var opened []*fakeSession
	s := &ScaleSet{log: testLogger(), scaleSetID: 4, created: false}
	s.tryOpen = func(context.Context) (session, error) {
		if held {
			return nil, errors.New("409 Conflict: the runner scale set already has an active session")
		}
		f := &fakeSession{}
		opened = append(opened, f)
		return f, nil
	}
	ctx := context.Background()
	s.CloseSession(ctx) // none open: nothing to close
	if ok, err := s.TryOpenSession(ctx); ok || err != nil {
		t.Fatalf("held: %v, %v", ok, err)
	}
	held = false
	if ok, err := s.TryOpenSession(ctx); !ok || err != nil || len(opened) != 1 {
		t.Fatalf("free: %v, %v, %d opened", ok, err, len(opened))
	}
	if ok, _ := s.TryOpenSession(ctx); !ok || len(opened) != 1 {
		t.Fatal("an open session opened again")
	}
	s.ReleaseSession(ctx)
	if !opened[0].closed.Load() || s.session != nil {
		t.Fatal("not released")
	}
	s.ReleaseSession(ctx) // none open
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s.tryOpen = func(context.Context) (session, error) { return nil, errors.New("401 Unauthorized") }
	if ok, err := s.TryOpenSession(ctx); ok || err == nil {
		t.Fatalf("another error: %v, %v", ok, err)
	}
}

// Without wait, a conflict is retried as long as a crashed process's hold
// lasts, then fails; with it, for as long as the context lasts, as a standby
// for another daemon's session.
func TestRetrySession(t *testing.T) {
	ctx := context.Background()
	open, tries := sessionOpener(sessionAttempts+5, nil)
	if _, err := retrySession(ctx, open, false, time.Millisecond, testLogger()); err == nil || *tries != sessionAttempts {
		t.Fatalf("without wait: %v after %d tries, want the conflict after %d", err, *tries, sessionAttempts)
	}

	open, tries = sessionOpener(sessionAttempts+5, nil)
	got, err := retrySession(ctx, open, true, time.Millisecond, testLogger())
	if err != nil || got != sessionAttempts+6 {
		t.Fatalf("with wait: %d, %v after %d tries, want the session at try %d", got, err, *tries, sessionAttempts+6)
	}

	cctx, cancel := context.WithCancel(ctx)
	open, _ = sessionOpener(1<<30, nil)
	done := make(chan error, 1)
	go func() {
		_, err := retrySession(cctx, open, true, time.Millisecond, testLogger())
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("with wait, cancelled: %v, want the context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("with wait: did not stop with its context")
	}

	other := errors.New("401 Unauthorized")
	open, tries = sessionOpener(0, other)
	if _, err := retrySession(ctx, open, true, time.Millisecond, testLogger()); !errors.Is(err, other) || *tries != 1 {
		t.Fatalf("another error: %v after %d tries, want it at once", err, *tries)
	}
}

// Only a creation that failed because the name exists is taken for another
// daemon's creation; any other failure is one.
func TestIsCreateRace(t *testing.T) {
	for msg, want := range map[string]bool{
		"409 Conflict: runner scale set already exists":    true,
		"A runner scale set with that name already exists": true,
		"403 Forbidden":                        false,
		"422 Unprocessable Entity: bad labels": false,
	} {
		if got := isCreateRace(errors.New(msg)); got != want {
			t.Errorf("isCreateRace(%q) = %v, want %v", msg, got, want)
		}
	}
}
