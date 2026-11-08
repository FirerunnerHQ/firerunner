//go:build linux && netns

// The netns suite loads the real generated ruleset into the real kernel and
// proves, packet by packet, what a guest can and cannot reach. It builds a
// throwaway topology of network namespaces (a router holding the ruleset, two
// guests, a "WAN" carrying public, LAN, tailnet and link-local addresses, and a
// DNAT'd resolver like the one the CI hosts run), so nothing touches the host's
// own namespace or firewall. It needs root, ip, nft, bash and timeout.
//
// Run with: make netns-test   (sudo go test -tags netns ./internal/provisioner/)
package provisioner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	labCIDR   = "172.19.0.0/16"
	labPrefix = "fk"
	labCache  = 8099
)

// lab is one isolated topology. The router namespace stands in for the CI host:
// guest taps fk0/fk1, an uplink ext0 to the WAN, and a veth to a resolver
// namespace that guest DNS is DNAT'd to.
type lab struct {
	router, guest0, guest1, wan, resolver string
}

func requireNetNS(t *testing.T) {
	t.Helper()
	missing := ""
	if os.Geteuid() != 0 {
		missing = "needs root to create network namespaces"
	}
	for _, bin := range []string{"ip", "nft", "bash", "timeout"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = bin + " not on PATH"
		}
	}
	if missing != "" {
		if os.Getenv("FR_REQUIRE_NETNS") == "1" {
			t.Fatal(missing)
		}
		t.Skip(missing)
	}
}

func sh(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (l lab) in(ns string, args ...string) []string {
	return append([]string{"ip", "netns", "exec", ns}, args...)
}

// newLab builds the topology and registers its teardown. Deleting a namespace
// removes its veth ends, so the namespaces are all there is to clean up.
func newLab(t *testing.T) lab {
	t.Helper()
	p := fmt.Sprintf("frt%d", os.Getpid()%100000)
	l := lab{router: p + "r", guest0: p + "g0", guest1: p + "g1", wan: p + "w", resolver: p + "d"}
	for _, ns := range []string{l.router, l.guest0, l.guest1, l.wan, l.resolver} {
		_ = exec.Command("ip", "netns", "del", ns).Run() // leftovers from a killed run
		sh(t, "ip", "netns", "add", ns)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
		sh(t, l.in(ns, "ip", "link", "set", "lo", "up")...)
	}
	link := func(rIf, rAddr, ns, nsAddr string) {
		sh(t, "ip", "link", "add", rIf, "netns", l.router, "type", "veth", "peer", "name", "eth0", "netns", ns)
		sh(t, l.in(l.router, "ip", "addr", "add", rAddr, "dev", rIf)...)
		sh(t, l.in(l.router, "ip", "link", "set", rIf, "up")...)
		sh(t, l.in(ns, "ip", "addr", "add", nsAddr, "dev", "eth0")...)
		sh(t, l.in(ns, "ip", "link", "set", "eth0", "up")...)
	}
	link("fk0", "172.19.0.1/30", l.guest0, "172.19.0.2/30")
	link("fk1", "172.19.1.1/30", l.guest1, "172.19.1.2/30")
	link("ext0", "198.51.100.1/24", l.wan, "198.51.100.2/24")
	link("vresolv0", "10.53.0.1/30", l.resolver, "10.53.0.2/30")
	sh(t, l.in(l.guest0, "ip", "route", "add", "default", "via", "172.19.0.1")...)
	sh(t, l.in(l.guest1, "ip", "route", "add", "default", "via", "172.19.1.1")...)
	sh(t, l.in(l.resolver, "ip", "route", "add", "default", "via", "10.53.0.1")...)
	sh(t, l.in(l.router, "ip", "route", "add", "default", "via", "198.51.100.2")...)
	sh(t, l.in(l.router, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")...)
	// The WAN answers for the internet and for every private network a CI host
	// can see: a public host, the home LAN, other RFC1918 ranges, the tailnet's
	// CGNAT range, a link-local metadata address and a documentation (TEST-NET)
	// address. The public stand-ins are genuinely global addresses: nothing
	// leaves the lab, so they only ever reach the WAN namespace.
	for _, a := range []string{"1.0.0.1", "9.9.9.9", "203.0.113.10", "192.168.4.10", "10.0.0.5", "172.31.0.5", "100.100.100.100", "169.254.169.254"} {
		sh(t, l.in(l.wan, "ip", "addr", "add", a+"/32", "dev", "eth0")...)
	}
	return l
}

func (l lab) load(t *testing.T, name, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	sh(t, l.in(l.router, "nft", "-f", path)...)
}

// guestDNAT mirrors the CI hosts' guest-dns.nft: guest DNS to any resolver is
// redirected to a resolver in its own namespace, which makes it a forwarded
// packet to a private address.
const guestDNAT = `table ip guest_dns {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "fk*" tcp dport 53 dnat to 10.53.0.2
    iifname "fk*" udp dport 53 dnat to 10.53.0.2
  }
}
`

// publishedPort mirrors a host service that rewrites a port to a private
// address, as Docker or Kubernetes publish a container's port. Only the host's
// DNS redirect is exempt from the non-public drop; this one must not be.
const publishedPort = `table ip published {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    tcp dport 8080 dnat to 10.0.0.5:443
  }
}
`

// reachable dials ip:port over TCP from a namespace. Nothing listens anywhere,
// so a packet that arrives is answered with a RST (connection refused) while a
// dropped one times out: the two outcomes separate "delivered" from "filtered"
// without running servers.
func (l lab) reachable(t *testing.T, ns, ip string, port int) bool {
	t.Helper()
	ok, err := dial(ns, ip, port)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// dial is reachable without a *testing.T, so probes can run on goroutines.
func dial(ns, ip string, port int) (bool, error) {
	cmd := exec.Command("ip", "netns", "exec", ns, "timeout", "2", "bash", "-c",
		fmt.Sprintf("exec 3<>/dev/tcp/%s/%d", ip, port))
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "Connection refused") {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 124 {
		return false, nil
	}
	return false, fmt.Errorf("dial %s:%d from %s: %v\n%s", ip, port, ns, err, out)
}

type probe struct {
	name string
	ip   string
	port int
	want bool
}

// check dials every probe at once, then reports each as a subtest. A filtered
// probe waits out its whole timeout, so all of them run concurrently: t.Parallel
// would cap them at GOMAXPROCS and queue the timeouts in batches.
func (l lab) check(t *testing.T, probes []probe) {
	t.Helper()
	type result struct {
		got bool
		err error
	}
	results := make([]result, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := dial(l.guest0, p.ip, p.port)
			results[i] = result{got, err}
		}()
	}
	wg.Wait()
	for i, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			r := results[i]
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.got != p.want {
				t.Errorf("guest -> %s:%d reachable = %v, want %v", p.ip, p.port, r.got, p.want)
			}
		})
	}
}

// guestBoundaries are the destinations a guest must never reach in any egress
// mode: the host's private networks, the tailnet, link-local metadata, outbound
// mail, other guests and the host itself.
var guestBoundaries = []probe{
	{"home LAN", "192.168.4.10", 443, false},
	{"RFC1918 10/8", "10.0.0.5", 443, false},
	{"RFC1918 172.16/12", "172.31.0.5", 443, false},
	{"tailnet CGNAT", "100.100.100.100", 443, false},
	{"link-local metadata", "169.254.169.254", 80, false},
	{"resolver without DNAT", "10.53.0.2", 53, false},
	{"documentation TEST-NET-3", "203.0.113.10", 443, false},
	{"smtp", "1.0.0.1", 25, false},
	{"smtps", "1.0.0.1", 465, false},
	{"submission", "1.0.0.1", 587, false},
	{"alt submission", "1.0.0.1", 2525, false},
	{"other guest", "172.19.1.2", 443, false},
	{"host uplink address", "198.51.100.1", 22, false},
	{"host gateway ssh", "172.19.0.1", 22, false},
	{"cache server on gateway", "172.19.0.1", labCache, true},
}

func TestEgressNetNSOpen(t *testing.T) {
	requireNetNS(t)
	l := newLab(t)
	l.load(t, "guest-dns.nft", guestDNAT)
	l.load(t, "published.nft", publishedPort)
	l.load(t, "firerunner.nft", mustNFTRuleset(t, egressRuleset{
		Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0",
		VMCidr: labCIDR, CachePort: labCache, Open: true,
	}))
	l.check(t, append([]probe{
		{"public https", "1.0.0.1", 443, true},
		{"other public host", "9.9.9.9", 443, true},
		// 8.8.8.8 is routed nowhere in the lab: only the host's DNAT to the
		// private resolver can deliver it.
		{"host-DNAT'd DNS", "8.8.8.8", 53, true},
		{"a host's published port to a private address", "9.9.9.9", 8080, false},
	}, guestBoundaries...))
}

func TestEgressNetNSAllowlist(t *testing.T) {
	requireNetNS(t)
	l := newLab(t)
	l.load(t, "firerunner.nft", mustNFTRuleset(t, egressRuleset{
		Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0",
		VMCidr: labCIDR, CachePort: labCache,
		Allowed:    []string{"1.0.0.0/24"},
		DNSServers: []string{"192.168.4.10"},
		AllowDNS:   true,
		AllowNTP:   true,
	}))
	l.check(t, append([]probe{
		{"allowlisted https", "1.0.0.1", 443, true},
		{"public host outside the allowlist", "9.9.9.9", 443, false},
		// An explicitly configured resolver stays reachable on 53 even when it
		// is on the LAN, and only on 53.
		{"configured LAN resolver", "192.168.4.10", 53, true},
	}, guestBoundaries...))
}

// sourceProbe sends a TCP SYN with an explicit source. We assert the nft counter
// rather than a connection timeout: a spoofed return route or rp_filter could
// otherwise make a broken firewall look correct. No packet leaves the lab.
func (l lab) sourceProbe(t *testing.T, source, destination string) {
	t.Helper()
	const script = `import socket,struct,sys
src,dst=sys.argv[1:]
def checksum(b):
 s=sum(struct.unpack('!%dH'%(len(b)//2),b));s=(s>>16)+(s&65535);s+=(s>>16);return (~s)&65535
tcp=struct.pack('!HHLLBBHHH',42345,8099,1,0,80,2,65535,0,0)
pseudo=socket.inet_aton(src)+socket.inet_aton(dst)+struct.pack('!BBH',0,6,len(tcp))
tcp=tcp[:16]+struct.pack('!H',checksum(pseudo+tcp))+tcp[18:]
ip=struct.pack('!BBHHHBBH4s4s',69,0,40,1,0,64,6,0,socket.inet_aton(src),socket.inet_aton(dst))
s=socket.socket(socket.AF_INET,socket.SOCK_RAW,socket.IPPROTO_RAW)
s.sendto(ip+tcp,(dst,0))
`
	sh(t, l.in(l.guest0, "python3", "-c", script, source, destination)...)
}
func (l lab) spoofCount(t *testing.T) uint64 {
	t.Helper()
	out, err := exec.Command("ip", "netns", "exec", l.router, "nft", "-j", "list", "counter", "ip", "fr_test", "guest_spoof_drop").CombinedOutput()
	if err != nil {
		t.Fatalf("counter: %v %s", err, out)
	}
	var doc struct {
		NFTables []struct {
			Counter *struct {
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, entry := range doc.NFTables {
		if entry.Counter != nil {
			return entry.Counter.Packets
		}
	}
	t.Fatalf("missing spoof counter: %s", out)
	return 0
}

func TestEgressNetNSSourceSpoofing(t *testing.T) {
	requireNetNS(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 required for packet injection")
	}
	for _, veth := range []bool{false, true} {
		for _, open := range []bool{false, true} {
			t.Run(fmt.Sprintf("veth=%t/open=%t", veth, open), func(t *testing.T) {
				l := newLab(t)
				iface := "fk0"
				if veth {
					// The host's policy observes the netns host-veth, carrying the original
					// guest source. Rename the lab ingress to exercise that exact binding.
					sh(t, l.in(l.router, "ip", "link", "set", "fk0", "down")...)
					sh(t, l.in(l.router, "ip", "link", "set", "fk0", "name", "fkv0h")...)
					sh(t, l.in(l.router, "ip", "link", "set", "fkv0h", "up")...)
					iface = "fkv0h"
				}
				sh(t, l.in(l.router, "sysctl", "-q", "-w", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf."+iface+".rp_filter=0")...)
				cfg := egressRuleset{Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0", VMCidr: labCIDR, CachePort: labCache, MaxVMs: 2, NetNS: veth, Open: open, Allowed: []string{"1.0.0.0/24"}}
				for refresh := 0; refresh < 2; refresh++ {
					l.load(t, "firerunner.nft", mustNFTRuleset(t, cfg))
					if !l.reachable(t, l.guest0, "172.19.0.1", labCache) {
						t.Fatal("legitimate cache connection blocked")
					}
					if got := l.spoofCount(t); got != 0 {
						t.Fatalf("legitimate source counted: %d", got)
					}
					for _, destination := range []string{"172.19.0.1", "1.0.0.1"} {
						before := l.spoofCount(t)
						l.sourceProbe(t, "172.19.1.2", destination)
						if after := l.spoofCount(t); after <= before {
							t.Fatalf("spoof on %s to %s escaped source check (%d -> %d)", iface, destination, before, after)
						}
					}
				}
			})
		}
	}
}

// start runs a long-lived command in a namespace, waits for the first line it
// prints and stops it when the test ends.
func (l lab) start(t *testing.T, ns string, args ...string) {
	t.Helper()
	cmd := exec.Command("ip", append([]string{"netns", "exec", ns}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("%s: no ready line: %v", strings.Join(args, " "), err)
	}
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("%s: %q", strings.Join(args, " "), line)
	}
}

// A guest holding its limit of connections cannot open another, while a
// different guest still can, and an allowlist refresh, which rebuilds the
// main table, does not hand it a fresh allowance.
func TestEgressNetNSConnLimit(t *testing.T) {
	requireNetNS(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 required for held connections")
	}
	const limit = 5
	l := newLab(t)
	cfg := egressRuleset{
		Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0",
		VMCidr: labCIDR, CachePort: labCache, Allowed: []string{"1.0.0.0/24"},
		ConnLimit: limit,
	}
	l.load(t, "firerunner.nft", mustNFTRuleset(t, cfg))
	// The kernel completes handshakes into the backlog, so the listener never
	// needs to accept.
	l.start(t, l.wan, "python3", "-c", `import socket,time
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("1.0.0.1",8080));s.listen(128);print("ready",flush=True);time.sleep(600)`)
	if !l.reachable(t, l.guest1, "1.0.0.1", 8080) {
		t.Fatal("listener unreachable before any limit applies")
	}
	l.start(t, l.guest0, "python3", "-c", fmt.Sprintf(`import socket,time
held=[socket.create_connection(("1.0.0.1",8080),timeout=5) for _ in range(%d)]
print("ready",flush=True);time.sleep(600)`, limit))

	if l.reachable(t, l.guest0, "1.0.0.1", 8080) {
		t.Fatalf("guest opened connection %d past its limit of %d", limit+1, limit)
	}
	if !l.reachable(t, l.guest1, "1.0.0.1", 8080) {
		t.Fatal("another guest's limit blocked this guest")
	}
	l.load(t, "firerunner.nft", mustNFTRuleset(t, cfg))
	if l.reachable(t, l.guest0, "1.0.0.1", 8080) {
		t.Fatal("a ruleset refresh reset the guest's connection count")
	}

	// Removing the guest's conntrack entries, as teardown does, frees its
	// allowance even while the connections it held are still open on its side.
	fd, err := os.Open("/var/run/netns/" + l.router)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := flushConntrackIn(int(fd.Fd()), "172.19.0.2"); err != nil {
		t.Fatalf("flush guest conntrack: %v", err)
	}
	if !l.reachable(t, l.guest0, "1.0.0.1", 8080) {
		t.Fatal("guest still at its limit after its conntrack entries were removed")
	}
}

// listen starts a listener on the WAN's public address 1.0.0.1:port.
func (l lab) listen(t *testing.T, port int) {
	t.Helper()
	l.start(t, l.wan, "python3", "-c", fmt.Sprintf(`import socket,time
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("1.0.0.1",%d));s.listen(4096);print("ready",flush=True)
while True: s.accept()[0].close()`, port))
}

// churn opens and closes n connections from a namespace as fast as it can,
// and returns how many connected and how many seconds that took.
func (l lab) churn(t *testing.T, ns string, n int) (ok int, secs float64) {
	t.Helper()
	out, err := exec.Command("ip", "netns", "exec", ns, "python3", "-c", fmt.Sprintf(`import socket,time
ok=0;start=time.monotonic()
for _ in range(%d):
    try: socket.create_connection(("1.0.0.1",8080),timeout=0.2).close();ok+=1
    except OSError: pass
print(ok,time.monotonic()-start)`, n)).Output()
	if err != nil {
		t.Fatalf("churn from %s: %v", ns, err)
	}
	if _, err := fmt.Sscan(string(out), &ok, &secs); err != nil {
		t.Fatalf("churn output %q: %v", out, err)
	}
	return ok, secs
}

// Opening and closing connections in a loop slips past the count, since a
// closed connection is not counted while conntrack still keeps it; the rate
// limit bounds it, for that guest only.
func TestEgressNetNSConnRate(t *testing.T) {
	requireNetNS(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 required")
	}
	l := newLab(t)
	l.load(t, "firerunner.nft", mustNFTRuleset(t, egressRuleset{
		Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0",
		VMCidr: labCIDR, CachePort: labCache, Allowed: []string{"1.0.0.0/24"},
		ConnLimit: 1000, ConnRate: 5,
	}))
	l.listen(t, 8080)
	// A burst of 50 passes, then 5 a second.
	ok, secs := l.churn(t, l.guest0, 200)
	if most := 50 + int(5*secs) + 5; ok > most {
		t.Fatalf("%d of 200 connections in %.1fs, want at most %d at 5/s after a burst of 50", ok, secs, most)
	}
	if ok < 50 {
		t.Fatalf("only %d connections passed, want the burst of 50", ok)
	}
	if ok, _ := l.churn(t, l.guest1, 10); ok != 10 {
		t.Fatalf("another guest's rate limited this guest: %d of 10 connected", ok)
	}
}

// Packets that spoof another guest's address are dropped by the main table
// before the limit chains see them (priority 1), so they cannot spend that
// guest's allowance.
func TestEgressNetNSSpoofedSourceSpendsNoAllowance(t *testing.T) {
	requireNetNS(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 required for packet injection")
	}
	l := newLab(t)
	sh(t, l.in(l.router, "sysctl", "-q", "-w", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.fk0.rp_filter=0")...)
	l.load(t, "firerunner.nft", mustNFTRuleset(t, egressRuleset{
		Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0",
		VMCidr: labCIDR, CachePort: labCache, MaxVMs: 2, Allowed: []string{"1.0.0.0/24"},
		ConnLimit: 1000, ConnRate: 1, // a burst of 10
	}))
	l.listen(t, 8080)
	for range 30 {
		l.sourceProbe(t, "172.19.1.2", "1.0.0.1")
	}
	if ok, _ := l.churn(t, l.guest1, 5); ok != 5 {
		t.Fatalf("spoofed packets spent the guest's allowance: %d of 5 connected", ok)
	}
}
