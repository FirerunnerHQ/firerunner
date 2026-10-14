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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// reachable dials ip:port over TCP from a namespace. Nothing listens anywhere,
// so a packet that arrives is answered with a RST (connection refused) while a
// dropped one times out: the two outcomes separate "delivered" from "filtered"
// without running servers.
func (l lab) reachable(t *testing.T, ns, ip string, port int) bool {
	t.Helper()
	cmd := exec.Command("ip", "netns", "exec", ns, "timeout", "2", "bash", "-c",
		fmt.Sprintf("exec 3<>/dev/tcp/%s/%d", ip, port))
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "Connection refused") {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 124 {
		return false
	}
	t.Fatalf("dial %s:%d from %s: %v\n%s", ip, port, ns, err, out)
	return false
}

type probe struct {
	name string
	ip   string
	port int
	want bool
}

func (l lab) check(t *testing.T, probes []probe) {
	t.Helper()
	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			if got := l.reachable(t, l.guest0, p.ip, p.port); got != p.want {
				t.Errorf("guest -> %s:%d reachable = %v, want %v", p.ip, p.port, got, p.want)
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
	l.load(t, "firerunner.nft", buildNFTRuleset(egressRuleset{
		Table: "fr_test", IfacePrefix: labPrefix, ExtIface: "ext0",
		VMCidr: labCIDR, CachePort: labCache, Open: true,
	}))
	l.check(t, append([]probe{
		{"public https", "1.0.0.1", 443, true},
		{"other public host", "9.9.9.9", 443, true},
		// 8.8.8.8 is routed nowhere in the lab: only the host's DNAT to the
		// private resolver can deliver it.
		{"host-DNAT'd DNS", "8.8.8.8", 53, true},
	}, guestBoundaries...))
}

func TestEgressNetNSAllowlist(t *testing.T) {
	requireNetNS(t)
	l := newLab(t)
	l.load(t, "firerunner.nft", buildNFTRuleset(egressRuleset{
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
					l.load(t, "firerunner.nft", buildNFTRuleset(cfg))
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
