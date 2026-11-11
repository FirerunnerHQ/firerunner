package jitserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/lograte"
)

// chanListener accepts the connections pushed into conns, in order.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// push hands c to whoever accepts from l, failing the test instead of
// blocking when the accepting side has stopped.
func (l *chanListener) push(t *testing.T, c net.Conn, stopped <-chan struct{}) {
	t.Helper()
	select {
	case l.conns <- c:
	case <-stopped:
		t.Fatal("the accepting side stopped before taking a connection")
	}
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 8098} }

// srcConn is the server's end of a pipe, appearing to come from src, and
// noting when it is closed.
type srcConn struct {
	net.Conn
	src    netip.Addr
	local  netip.Addr // the address it reached; the pipe's own when unset
	closed chan struct{}
	once   sync.Once
}

func (c *srcConn) RemoteAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(c.src, 40000))
}

func (c *srcConn) LocalAddr() net.Addr {
	if c.local.IsValid() {
		return net.TCPAddrFromAddrPort(netip.AddrPortFrom(c.local, 8099))
	}
	return c.Conn.LocalAddr()
}

func (c *srcConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// dial returns a connection from src for the server to accept, and the
// client's end of it.
func dial(src netip.Addr) (server *srcConn, client net.Conn) {
	s, c := net.Pipe()
	return &srcConn{Conn: s, src: src, closed: make(chan struct{})}, c
}

// acceptor accepts from l in the background, one connection at a time.
type acceptor struct {
	in      *chanListener
	l       *Limited
	got     chan net.Conn
	stopped chan struct{}
}

// startAcceptor starts accepting from a listener capped at perSource and
// total, after setup, when given, has configured it.
func startAcceptor(t *testing.T, perSource, total int, setup ...func(*Limited)) *acceptor {
	t.Helper()
	in := newChanListener()
	l := LimitListener(in, perSource, total)
	for _, f := range setup {
		f(l)
	}
	a := &acceptor{in: in, l: l, got: make(chan net.Conn), stopped: make(chan struct{})}
	go func() {
		defer close(a.stopped)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			a.got <- c
		}
	}()
	t.Cleanup(func() { _ = in.Close() })
	return a
}

// offer hands the listener a connection from src. It returns the accepted
// connection, or nil when the listener refused it by closing it.
func (a *acceptor) offer(t *testing.T, src netip.Addr) net.Conn {
	t.Helper()
	return a.offerTo(t, src, netip.Addr{})
}

// offerTo is offer for a connection that reached the address local.
func (a *acceptor) offerTo(t *testing.T, src, local netip.Addr) net.Conn {
	t.Helper()
	sc, cc := dial(src)
	sc.local = local
	t.Cleanup(func() { _ = cc.Close() })
	a.in.push(t, sc, a.stopped)
	select {
	case c := <-a.got:
		return c
	case <-sc.closed:
		return nil
	case <-a.stopped:
		t.Fatal("the listener stopped accepting")
		return nil
	}
}

func TestLimitListenerCapsPerSourceAndTotal(t *testing.T) {
	a := startAcceptor(t, 2, 3)
	srcA, srcB, srcC := guest, foreign, netip.MustParseAddr("10.0.1.50")

	a1, a2 := a.offer(t, srcA), a.offer(t, srcA)
	if a1 == nil || a2 == nil {
		t.Fatal("refused a source's first two connections, under the per-source cap of 2")
	}
	if a.offer(t, srcA) != nil {
		t.Fatal("accepted a source's third connection over the per-source cap of 2")
	}
	b1 := a.offer(t, srcB)
	if b1 == nil {
		t.Fatal("refused another source's first connection while the first source sat at its cap")
	}
	if a.offer(t, srcC) != nil {
		t.Fatal("accepted a fourth connection over the total cap of 3")
	}

	// Closing a connection gives its slot back, to any source.
	_ = a1.Close()
	c1 := a.offer(t, srcC)
	if c1 == nil {
		t.Fatal("refused a connection after one closed below the total cap")
	}

	// A second Close of the same connection gives nothing more back: open
	// are a2, b1 and c1, so the total cap of 3 still holds.
	_ = a1.Close()
	_ = a2.Close()
	_ = a2.Close()
	if a.offer(t, srcA) == nil {
		t.Fatal("refused a connection with two slots free")
	}
	if a.offer(t, srcA) != nil {
		t.Fatal("accepted a connection over the total cap of 3 after a double Close")
	}
}

// TestHeldConnectionsDoNotStarveAnotherSource: one source opening many
// connections and sending nothing, as a job could, holds at most its
// per-source share, and another runner still gets its config through the
// real server.
func TestHeldConnectionsDoNotStarveAnotherSource(t *testing.T) {
	s := New(0, nil)
	if err := s.Register(guest, "runner-1", "the-config", nil); err != nil {
		t.Fatal(err)
	}
	in := newChanListener()
	// The total leaves room for one connection beyond the holder's share.
	l := LimitListener(in, MaxConnsPerSource, MaxConnsPerSource+1)
	srv := s.HTTPServer()
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); <-served })

	holder := netip.MustParseAddr("10.0.1.66")
	// It sends nothing: the ones the listener admits stay open until the
	// server's header deadline, and the rest are closed at once.
	for range 3 * MaxConnsPerSource {
		sc, cc := dial(holder)
		t.Cleanup(func() { _ = cc.Close() })
		in.push(t, sc, served)
	}
	// Accept runs one connection at a time, so pushing the guest's
	// connection waits until every holder connection has been decided.
	sc, cc := dial(guest)
	t.Cleanup(func() { _ = cc.Close() })
	in.push(t, sc, served)
	go func() { _, _ = io.WriteString(cc, "GET /jit HTTP/1.1\r\nHost: jit\r\n\r\n") }()
	resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
	if err != nil {
		t.Fatalf("the guest's request failed while another source held connections: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "the-config" {
		t.Fatalf("guest fetch = %d %q, want 200 with its config", resp.StatusCode, body)
	}
	if !resp.Close {
		t.Fatal("the response keeps the connection alive; each connection should carry one request")
	}
}

func TestSourceOf(t *testing.T) {
	mapped := netip.MustParseAddr("::ffff:10.0.1.17")
	for _, tc := range []struct {
		addr net.Addr
		want netip.Addr
	}{
		{net.TCPAddrFromAddrPort(netip.AddrPortFrom(guest, 1)), guest},
		{net.TCPAddrFromAddrPort(netip.AddrPortFrom(mapped, 1)), guest},
		{&net.UnixAddr{Name: "@", Net: "unix"}, netip.Addr{}},
		{nil, netip.Addr{}},
	} {
		if got := sourceOf(tc.addr); got != tc.want {
			t.Errorf("sourceOf(%v) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestRefusedTCPConnectionGetsNoResponse runs the listener over real
// loopback TCP: a connection over the per-source cap is closed without an
// answer, and the one holding the slot is still served.
func TestRefusedTCPConnectionGetsNoResponse(t *testing.T) {
	s := New(0, nil)
	if err := s.Register(netip.MustParseAddr("127.0.0.1"), "runner-1", "the-config", nil); err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := s.HTTPServer()
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(LimitListener(tcp, 1, MaxConns)) }()
	t.Cleanup(func() { _ = srv.Close(); <-served })

	held, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	// Accept runs in order, so the held connection takes the one slot
	// before the second is decided.
	refused, err := net.Dial("tcp", tcp.Addr().String())
	switch {
	case errors.Is(err, syscall.ECONNRESET):
		// Refused, and reset, before Dial returned: the behaviour under test.
	case err != nil:
		t.Fatal(err)
	default:
		t.Cleanup(func() { _ = refused.Close() })
		_, _ = io.WriteString(refused, "GET /jit HTTP/1.1\r\nHost: jit\r\n\r\n")
		n, err := refused.Read(make([]byte, 1))
		if n != 0 || err == nil {
			t.Fatalf("connection over the cap read %d bytes, err %v; want it closed with no response", n, err)
		}
	}

	_, _ = io.WriteString(held, "GET /jit HTTP/1.1\r\nHost: jit\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(held), nil)
	if err != nil {
		t.Fatalf("the connection holding the slot was not served: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "the-config" {
		t.Fatalf("held fetch = %d %q, want 200 with its config", resp.StatusCode, body)
	}
}

// An accepted connection still exposes the TCP connection's ReadFrom, which
// net/http needs to stream a file's response by sendfile.
func TestLimitedConnKeepsReadFrom(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LimitListener(tcp, 1, 1)
	defer ln.Close()
	client, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rf, ok := c.(io.ReaderFrom)
	if !ok {
		t.Fatal("the accepted connection hides ReadFrom")
	}
	if n, err := rf.ReadFrom(strings.NewReader("hello")); n != 5 || err != nil {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "hello" {
		t.Fatalf("client read %q, %v", got, err)
	}
}

// Each refusal is counted against the cap that refused it, and the latest's
// time kept; an accepted connection counts nothing.
func TestLimitListenerCountsRefusalsByCap(t *testing.T) {
	a := startAcceptor(t, 1, 2)
	if byCap, last := a.l.Refused(); byCap[CapPerSource] != 0 || byCap[CapTotal] != 0 || !last.IsZero() {
		t.Fatalf("before any refusal: %v, last %v", byCap, last)
	}
	before := time.Now()
	if a.offer(t, guest) == nil {
		t.Fatal("refused the first connection")
	}
	if a.offer(t, guest) != nil {
		t.Fatal("accepted a second connection over the per-source cap of 1")
	}
	if a.offer(t, foreign) == nil {
		t.Fatal("refused another source's first connection")
	}
	other := netip.MustParseAddr("10.0.1.50")
	for range 2 {
		if a.offer(t, other) != nil {
			t.Fatal("accepted a connection over the total cap of 2")
		}
	}
	byCap, last := a.l.Refused()
	if byCap[CapPerSource] != 1 || byCap[CapTotal] != 2 {
		t.Fatalf("refused %v, want 1 at %s and 2 at %s", byCap, CapPerSource, CapTotal)
	}
	if last.Before(before) {
		t.Fatalf("latest refusal at %v, before the refusals began at %v", last, before)
	}
}

// Refusals are logged at most once per interval for each cap, with the
// number left unlogged in between.
func TestRefusalWarningsAreSpaced(t *testing.T) {
	var buf syncBuffer
	now := time.Unix(1000, 0)
	a := startAcceptor(t, 1, 2, func(l *Limited) {
		l.WarnTo(slog.New(slog.NewJSONHandler(&buf, nil)))
		l.warns.Now = func() time.Time { return now }
	})
	if a.offer(t, guest) == nil || a.offer(t, foreign) == nil {
		t.Fatal("refused a first connection")
	}
	for range 3 {
		if a.offer(t, guest) != nil {
			t.Fatal("accepted a connection over the caps")
		}
	}
	// The total cap is full, so these were refused at it; let a minute pass.
	now = now.Add(lograte.Every)
	if a.offer(t, guest) != nil {
		t.Fatal("accepted a connection over the caps")
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Level, Cap string
			Held       uint64 `json:"unlogged_since_last"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		got = append(got, fmt.Sprintf("%s %s %d", rec.Level, rec.Cap, rec.Held))
	}
	if want := []string{"WARN total 0", "WARN total 2"}; !slices.Equal(got, want) {
		t.Fatalf("warnings %q, want %q", got, want)
	}
}

// With ExemptLocal, a connection from this host, by loopback or from the
// address it reached, is neither counted nor refused, and takes no slot:
// local tools still get through when guests fill the caps. Without it,
// loopback is capped like anyone.
func TestExemptLocalBypassesTheCaps(t *testing.T) {
	loop4, loop6 := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")
	self := netip.MustParseAddr("10.0.0.5")
	a := startAcceptor(t, 1, 1, (*Limited).ExemptLocal)
	if a.offer(t, guest) == nil {
		t.Fatal("refused the first guest connection")
	}
	var local []net.Conn
	for range 3 {
		for _, c := range []net.Conn{a.offer(t, loop4), a.offer(t, loop6), a.offerTo(t, self, self)} {
			if c == nil {
				t.Fatal("refused a connection from this host with the caps full")
			}
			local = append(local, c)
		}
	}
	// Closing them gives back no slot they never took.
	for _, c := range local {
		_ = c.Close()
	}
	if a.offer(t, foreign) != nil {
		t.Fatal("accepted a guest over the total cap once local connections closed")
	}
	// A guest that reached a different address is a guest, whatever its source.
	if a.offerTo(t, foreign, self) != nil {
		t.Fatal("accepted a guest connection over the cap")
	}
	// Nor is one whose addresses cannot be read, though both read as the
	// same zero address.
	sc, cc := net.Pipe()
	t.Cleanup(func() { _ = cc.Close() })
	closed := make(chan struct{})
	go func() { _, _ = cc.Read(make([]byte, 1)); close(closed) }()
	a.in.push(t, sc, a.stopped)
	select {
	case c := <-a.got:
		_ = c.Close()
		t.Fatal("accepted a connection of unknown origin over the cap")
	case <-a.stopped:
		t.Fatal("the listener stopped accepting")
	case <-closed: // refused
	}
	if byCap, _ := a.l.Refused(); byCap[CapTotal] != 3 || byCap[CapPerSource] != 0 {
		t.Fatalf("refused %v, want only the three guest connections, at the total cap", byCap)
	}

	plain := startAcceptor(t, 1, 1)
	if plain.offer(t, loop4) == nil {
		t.Fatal("refused the first loopback connection")
	}
	if plain.offer(t, loop4) != nil {
		t.Fatal("accepted a loopback connection over the cap without ExemptLocal")
	}
}

// syncBuffer is a bytes.Buffer safe to write from the accepting goroutine
// while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
