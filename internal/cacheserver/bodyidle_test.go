package cacheserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// putPaced sends a PUT body to url through a pipe, steps bytes written gap apart,
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
	req, _ := http.NewRequest(http.MethodPut, url, pr)
	resp, err := http.DefaultClient.Do(req)
	pr.CloseWithError(io.ErrClosedPipe) // stop the writer whatever happened
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A body that stops sending is cut after the idle bound and answered as an
// incomplete upload, not a server error; the same pause passes when the
// bound is off.
func TestStalledBodyIsCut(t *testing.T) {
	for _, tc := range []struct {
		idle time.Duration
		want int
	}{
		{100 * time.Millisecond, http.StatusBadRequest},
		{0, http.StatusCreated},
	} {
		s, err := newAuthorizedStore(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		s.SetBodyIdle(tc.idle)
		ts := httptest.NewServer(s)
		url := reserve(t, ts.URL, "a")
		code := putPaced(t, url+"&comp=block&blockid=YjA=", 2, 600*time.Millisecond)
		ts.Close()
		if code != tc.want {
			t.Errorf("idle %v, body pausing 600ms -> status %d, want %d", tc.idle, code, tc.want)
		}
	}
}

// A stalled block list is an incomplete upload too, not a server error.
func TestStalledBlockListIsCut(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBodyIdle(100 * time.Millisecond)
	ts := httptest.NewServer(s)
	defer ts.Close()
	url := reserve(t, ts.URL, "a")
	if code := putPaced(t, url+"&comp=blocklist", 2, 600*time.Millisecond); code != http.StatusBadRequest {
		t.Fatalf("block list pausing 600ms with a 100ms idle bound -> %d, want 400", code)
	}
}

// An upload that keeps sending is never cut, even when it takes far longer
// than the idle bound in all.
func TestSteadyBodyOutlastsIdleBound(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBodyIdle(time.Second)
	ts := httptest.NewServer(s)
	defer ts.Close()
	url := reserve(t, ts.URL, "a")
	if code := putPaced(t, url+"&comp=block&blockid=YjA=", 20, 100*time.Millisecond); code != http.StatusCreated {
		t.Fatalf("steady 2s upload with a 1s idle bound -> %d, want 201", code)
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
		if _, err := io.ReadAll(b); err != nil {
			ctxErr <- err
			return
		}
		// A reader that checks for more after EOF must not set the
		// deadline again on the connection net/http is now reading.
		if _, err := b.Read(make([]byte, 1)); err != io.EOF {
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
