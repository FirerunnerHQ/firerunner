package jitserver

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/firerunnerhq/firerunner/internal/lograte"
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
	// MaxConns bounds the connections held open across all addresses, as a
	// backstop for the daemon's file descriptors and memory. Every source
	// shares it, so it sits far above any realistic runner count: filling it
	// takes a thousand runners each holding its full MaxConnsPerSource. A
	// held connection costs one descriptor and a few KiB, and Go raises the
	// descriptor limit to the hard limit at start (systemd's is far higher).
	MaxConns = 4096
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

// Names of the caps a refusal is counted against, as Refused reports them
// and the warnings name them.
const (
	CapPerSource = "per_source"
	CapTotal     = "total"
)

// LimitListener returns ln with at most perSource connections open from one
// remote address and at most total open overall. A connection over either
// limit is closed as soon as it is accepted, with a reset where the
// transport allows one, so a refusal holds no slot and leaves no TIME_WAIT
// behind; the client sees a failed request (the guest retries its config
// fetch; the cache bundle and phase reports are best effort). A slot comes
// back when the server closes the connection. Refusals are counted by cap
// (Refused) and logged at most once a minute per cap (WarnTo).
func LimitListener(ln net.Listener, perSource, total int) *Limited {
	return &Limited{Listener: ln, perSource: perSource, total: total, open: map[netip.Addr]int{}}
}

// Limited is a listener with connection caps; see LimitListener.
type Limited struct {
	net.Listener
	perSource, total int
	exemptLocal      bool
	log              *slog.Logger
	warns            lograte.Limiter

	mu   sync.Mutex
	open map[netip.Addr]int
	n    int

	refusedSource, refusedTotal atomic.Uint64
	lastRefused                 atomic.Int64 // unix nanoseconds; 0 before the first
}

// ExemptLocal makes the caps neither count nor refuse a connection from this
// host itself (see fromThisHost), so local tools still get through when
// guests have filled them. Local processes are trusted with the host's
// descriptors anyway. Call before Accept.
func (l *Limited) ExemptLocal() { l.exemptLocal = true }

// WarnTo logs refusals to log, at most one a minute per cap, with how many
// went unlogged since the last. Call before Accept.
func (l *Limited) WarnTo(log *slog.Logger) { l.log = log }

// Limits returns the caps: per source address, and in all.
func (l *Limited) Limits() (perSource, total int) { return l.perSource, l.total }

// Refused returns the connections refused so far, by cap, and when the
// latest was (zero before the first).
func (l *Limited) Refused() (byCap map[string]uint64, last time.Time) {
	byCap = map[string]uint64{CapPerSource: l.refusedSource.Load(), CapTotal: l.refusedTotal.Load()}
	if ns := l.lastRefused.Load(); ns != 0 {
		last = time.Unix(0, ns)
	}
	return byCap, last
}

func (l *Limited) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.exemptLocal && fromThisHost(c) {
			return c, nil
		}
		src := sourceOf(c.RemoteAddr())
		if full := l.acquire(src); full != "" {
			// Counted before the close the client sees.
			l.refused(full, src)
			refuse(c)
			continue
		}
		return &limitedConn{Conn: c, release: func() { l.release(src) }}, nil
	}
}

// acquire takes a slot for src, or names the cap that is full.
func (l *Limited) acquire(src netip.Addr) (full string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n >= l.total {
		return CapTotal
	}
	if l.open[src] >= l.perSource {
		return CapPerSource
	}
	l.n++
	l.open[src]++
	return ""
}

func (l *Limited) release(src netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.open[src]--; l.open[src] <= 0 {
		delete(l.open, src)
	}
}

// refused counts a refusal at the cap named and warns about it, spaced out.
func (l *Limited) refused(capName string, src netip.Addr) {
	limit := l.total
	if capName == CapPerSource {
		l.refusedSource.Add(1)
		limit = l.perSource
	} else {
		l.refusedTotal.Add(1)
	}
	l.lastRefused.Store(time.Now().UnixNano())
	if l.log == nil {
		return
	}
	if held, ok := l.warns.Allow(capName); ok {
		l.log.Warn("refused a connection at its cap; its client sees a failed request",
			"cap", capName, "limit", limit, "source", src.String(), "unlogged_since_last", held)
	}
}

// fromThisHost reports whether c comes from this host: from a loopback
// address, or from the very address it reached. A guest's connection never
// does. TCP needs a completed handshake, whose reply to a forged local
// source stays on this host, and Linux drops a packet from outside that
// claims a local or loopback source (accept_local and route_localnet are off
// by default). On a Firecracker host the guest firewall also drops any
// source but the guest's own address (internal/provisioner/egress.go); an
// EC2 instance's cache proxy (images/assets/firerunner-cache-nginx.conf)
// connects from the instance's own VPC address, which is never the control
// VM's.
func fromThisHost(c net.Conn) bool {
	src := sourceOf(c.RemoteAddr())
	return src.IsValid() && (src.IsLoopback() || src == sourceOf(c.LocalAddr()))
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

// CloseWrite passes through to the TCP connection, which embedding the
// net.Conn interface would hide: net/http half-closes before its final close
// on an error reply, so the client reads the reply instead of a reset.
func (c *limitedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// ReadFrom passes through to the TCP connection as well, so a response
// streamed from a file still goes out by sendfile.
func (c *limitedConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}
