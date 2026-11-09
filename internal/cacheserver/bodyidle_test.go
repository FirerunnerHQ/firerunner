package cacheserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// putPaced uploads one block through a pipe, steps bytes written gap apart,
// and returns the status, or 0 when the server dropped the connection. The
// sleeps pace the client; nothing is asserted on timing.
func putPaced(t *testing.T, url string, steps int, gap time.Duration) int {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < steps; i++ {
			if _, err := pw.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(gap)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPut, url+"&comp=block&blockid=YjA=", pr)
	resp, err := http.DefaultClient.Do(req)
	pr.CloseWithError(io.ErrClosedPipe) // stop the writer whatever happened
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A body that stops sending is cut after the idle bound, and the same pause
// passes when the bound is off.
func TestStalledBodyIsCut(t *testing.T) {
	for _, tc := range []struct {
		idle time.Duration
		ok   bool
	}{
		{100 * time.Millisecond, false},
		{0, true},
	} {
		s, err := newAuthorizedStore(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		s.SetBodyIdle(tc.idle)
		ts := httptest.NewServer(s)
		url := reserve(t, ts.URL, "a")
		code := putPaced(t, url, 2, 600*time.Millisecond)
		ts.Close()
		if got := code == http.StatusCreated; got != tc.ok {
			t.Errorf("idle %v, body pausing 600ms -> status %d, want success %v", tc.idle, code, tc.ok)
		}
	}
}

// An upload that keeps sending is never cut, even when it takes far longer
// than the idle bound in all.
func TestSteadyBodyOutlastsIdleBound(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBodyIdle(300 * time.Millisecond)
	ts := httptest.NewServer(s)
	defer ts.Close()
	url := reserve(t, ts.URL, "a")
	if code := putPaced(t, url, 12, 50*time.Millisecond); code != http.StatusCreated {
		t.Fatalf("steady 600ms upload with a 300ms idle bound -> %d, want 201", code)
	}
}

// Once the body is read, the bound is gone: a handler that keeps working
// past it, such as committing a block list, keeps its context. idleBody
// relies on net/http clearing the read deadline when it starts its
// background read at EOF; a deadline left there would cancel the request.
func TestBodyIdleEndsAtEOF(t *testing.T) {
	const idle = 50 * time.Millisecond
	ctxErr := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := &idleBody{ReadCloser: r.Body, rc: http.NewResponseController(w), idle: idle}
		if _, err := io.Copy(io.Discard, b); err != nil {
			ctxErr <- err
			return
		}
		select {
		case <-r.Context().Done():
			ctxErr <- r.Context().Err()
		case <-time.After(10 * idle):
			ctxErr <- nil
		}
	}))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/octet-stream", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if err := <-ctxErr; err != nil {
		t.Fatalf("request after its body was read: %v, want it still live", err)
	}
}

// The idle bound is on by default.
func TestBodyIdleDefault(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := time.Duration(s.bodyIdle.Load()); got != DefaultBodyIdle {
		t.Fatalf("default body idle %v, want %v", got, DefaultBodyIdle)
	}
}
