package cacheserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"slices"
	"strings"
	"syscall"
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

// smallBufferServer serves h with a 4 KiB send buffer on each connection, so
// whether a response blocks does not depend on the host's TCP buffer sizes.
func smallBufferServer(h http.Handler) *httptest.Server {
	ts := httptest.NewUnstartedServer(h)
	ts.Listener = smallSendListener{ts.Listener}
	ts.Start()
	return ts
}

type smallSendListener struct{ net.Listener }

func (l smallSendListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// chunkRecorder is a ResponseWriter that can ReadFrom, recording each chunk it
// is handed: its size, and whether it reads straight from an *os.File, which
// is what keeps net/http on sendfile.
type chunkRecorder struct {
	httptest.ResponseRecorder
	sizes  []int64
	onFile []bool
}

func (c *chunkRecorder) ReadFrom(r io.Reader) (int64, error) {
	lr, ok := r.(*io.LimitedReader)
	_, file := lr.R.(*os.File)
	c.sizes, c.onFile = append(c.sizes, lr.N), append(c.onFile, ok && file)
	return io.Copy(&c.ResponseRecorder, r)
}

// ReadFrom hands a file over in idle-bound chunks, each straight from the file,
// as http.ServeContent's io.CopyN does, and leaves the outer limit settled.
func TestIdleWriterReadFromKeepsSendfile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "blob")
	if err != nil {
		t.Fatal(err)
	}
	const size = 2*idleChunk + 100
	if _, err := f.Write(make([]byte, size+50)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rec := &chunkRecorder{ResponseRecorder: *httptest.NewRecorder()}
	w := &idleWriter{ResponseWriter: rec, rc: http.NewResponseController(rec), idle: time.Minute}
	src := &io.LimitedReader{R: f, N: size}
	n, err := w.ReadFrom(src)
	if err != nil || n != size || src.N != 0 || rec.Body.Len() != size {
		t.Fatalf("copied %d (body %d), err %v, limit left %d; want %d, nil, 0", n, rec.Body.Len(), err, src.N, size)
	}
	if want := []int64{idleChunk, idleChunk, 100}; !slices.Equal(rec.sizes, want) {
		t.Fatalf("chunks %v, want %v", rec.sizes, want)
	}
	for i, ok := range rec.onFile {
		if !ok {
			t.Fatalf("chunk %d does not read from the file itself; sendfile would be lost", i)
		}
	}
}

// smallBufferClient reads through a 4 KiB receive buffer, so a server writing
// a large body blocks on the socket soon after the client stops reading.
func smallBufferClient() *http.Client {
	d := &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		_ = c.Control(func(fd uintptr) { err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
		return err
	}}
	return &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
}

// bigEntryURL saves an entry of n bytes and returns its download URL.
func bigEntryURL(t *testing.T, base string, n int) string {
	t.Helper()
	saveEntry(t, base, "42", "big", "v", n)
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta("42"), Key: "big", Version: "v"})
	if get["ok"] != true {
		t.Fatalf("get not ok: %v", get)
	}
	return get["signed_download_url"].(string)
}

// readPaced reads body in chunk-byte steps, gap apart, and returns how much
// arrived and the error that ended it (nil at EOF).
func readPaced(body io.Reader, chunk int, gap time.Duration) (int, error) {
	buf, total := make([]byte, chunk), 0
	for {
		n, err := io.ReadFull(body, buf)
		total += n
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		time.Sleep(gap)
	}
}

// A download the client stops reading ends after the idle bound; one it keeps
// reading slowly, for longer than the bound in all, does not.
func TestDownloadIdleBound(t *testing.T) {
	const size = 8 << 20
	for _, tc := range []struct {
		name     string
		stall    time.Duration // before reading at all
		gap      time.Duration // between 64 KiB reads
		complete bool
	}{
		{"stalled", time.Second, 0, false},
		{"steady", 0, 10 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newAuthorizedStore(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			s.SetBodyIdle(200 * time.Millisecond)
			ts := smallBufferServer(s)
			defer ts.Close()
			url := bigEntryURL(t, ts.URL, size)
			resp, err := smallBufferClient().Get(url)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			time.Sleep(tc.stall) // the client reads nothing for this long
			got, err := readPaced(resp.Body, 64<<10, tc.gap)
			if done := got == size && err == nil; done != tc.complete {
				t.Fatalf("read %d of %d bytes, err %v; want complete %v", got, size, err, tc.complete)
			}
		})
	}
}

// The write deadline goes with the request: a kept-alive connection's next
// response, after a pause longer than the bound, is not cut.
func TestDownloadIdleBoundEndsWithTheRequest(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetBodyIdle(100 * time.Millisecond)
	ts := httptest.NewServer(s)
	defer ts.Close()
	// Two chunks' worth, so the GET certainly leaves a write deadline behind.
	const size = 2 * idleChunk
	url := bigEntryURL(t, ts.URL, size)
	client := &http.Client{}
	reused := false
	// The second is a HEAD: its response writes no body, so nothing would push
	// a deadline the first response left behind, and its reply would fail.
	for i, method := range []string{http.MethodGet, http.MethodHead} {
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = reused || info.Reused }}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), method, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d (%s): %v", i, method, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := size
		if method == http.MethodHead {
			want = 0
		}
		if err != nil || resp.StatusCode != http.StatusOK || len(body) != want {
			t.Fatalf("request %d (%s): %d, %d bytes, err %v", i, method, resp.StatusCode, len(body), err)
		}
		time.Sleep(300 * time.Millisecond) // past the bound, between requests
	}
	if !reused {
		t.Fatal("the second request did not reuse the connection, so this proves nothing")
	}
}
