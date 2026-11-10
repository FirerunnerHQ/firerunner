package jitserver

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Every runner in the runners' security group can reach the JIT port, and a
// job runs arbitrary code on its runner. These bound what one runner, or a
// few, can hold open, so the others still fetch their configs.
const (
	// MaxConnsPerSource bounds the connections one address holds open at
	// once. A guest makes its requests one after another (config, cache
	// bundle, phase reports), each on its own connection, so it needs one;
	// the rest is slack for a retry that overlaps a closing connection.
	MaxConnsPerSource = 4
	// MaxConns bounds the connections held open across all addresses. A
	// legitimate request holds its slot for milliseconds, so this binds only
	// when many sources each hold their full MaxConnsPerSource.
	MaxConns = 512
)

// The server's per-connection limits. Requests carry no body and every
// response is small, so each connection is one short exchange.
const (
	readHeaderTimeout = 2 * time.Second
	readTimeout       = 5 * time.Second
	writeTimeout      = 10 * time.Second
	maxHeaderBytes    = 4 << 10
)

// HTTPServer returns the http.Server to serve s with: short read and write
// deadlines, small headers, and no keep-alive, so a connection closes after
// its one response and gives its slot back (see LimitListener).
func (s *Server) HTTPServer() *http.Server {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	srv.SetKeepAlivesEnabled(false)
	return srv
}

// LimitListener returns ln with at most perSource connections open from one
// remote address and at most total open overall. A connection over either
// limit is closed as soon as it is accepted, with a reset where the
// transport allows one, so a refusal holds no slot and leaves no TIME_WAIT
// behind; the client sees a failed request and retries. A slot comes back
// when the server closes the connection.
func LimitListener(ln net.Listener, perSource, total int) net.Listener {
	return &limitListener{Listener: ln, perSource: perSource, total: total, open: map[netip.Addr]int{}}
}

type limitListener struct {
	net.Listener
	perSource, total int

	mu   sync.Mutex
	open map[netip.Addr]int
	n    int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		src := sourceOf(c.RemoteAddr())
		if !l.acquire(src) {
			refuse(c)
			continue
		}
		return &limitedConn{Conn: c, release: func() { l.release(src) }}, nil
	}
}

func (l *limitListener) acquire(src netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n >= l.total || l.open[src] >= l.perSource {
		return false
	}
	l.n++
	l.open[src]++
	return true
}

func (l *limitListener) release(src netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.open[src]--; l.open[src] <= 0 {
		delete(l.open, src)
	}
}

// refuse closes c with a reset when it is TCP: the slot was never taken, and
// nothing of the connection should outlive the close.
func refuse(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}

// sourceOf is the address a connection's slots are counted against. One
// that cannot be read shares the zero address's count with every other such
// connection.
func sourceOf(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.AddrPort().Addr().Unmap()
	}
	if a != nil {
		if ap, err := netip.ParseAddrPort(a.String()); err == nil {
			return ap.Addr().Unmap()
		}
	}
	return netip.Addr{}
}

// limitedConn gives its slot back on its first Close; net/http may close a
// connection more than once.
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
