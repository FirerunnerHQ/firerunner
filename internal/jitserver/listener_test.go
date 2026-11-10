package jitserver

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
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
	closed chan struct{}
	once   sync.Once
}

func (c *srcConn) RemoteAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(c.src, 40000))
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
	got     chan net.Conn
	stopped chan struct{}
}

func startAcceptor(t *testing.T, perSource, total int) *acceptor {
	t.Helper()
	in := newChanListener()
	l := LimitListener(in, perSource, total)
	a := &acceptor{in: in, got: make(chan net.Conn), stopped: make(chan struct{})}
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
	sc, cc := dial(src)
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
