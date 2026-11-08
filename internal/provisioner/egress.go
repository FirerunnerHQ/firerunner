package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// natTable is the dedicated nftables table firerunner manages, isolated from any
// other host firewall rules.
const natTable = "firerunner"

// metaURL is GitHub's published inventory of its own IP ranges.
const metaURL = "https://api.github.com/meta"

// metaCategories are the api.github.com/meta keys firerunner understands.
var metaCategories = map[string]bool{"api": true, "actions": true, "git": true, "packages": true}

// EgressConfig describes what microVM guests are permitted to reach.
type EgressConfig struct {
	// Categories mixes GitHub /meta categories (api, actions, git, packages)
	// with pseudo-categories: "dns" (allow resolvers), "ntp" (allow time sync),
	// and "open" (no allowlist — blanket NAT).
	Categories []string
	// DNSServers are the resolver IPs guests may reach on port 53.
	DNSServers []string
	// RefreshInterval controls how often the /meta ranges are re-fetched. Zero
	// disables periodic refresh.
	RefreshInterval time.Duration
}

// Has reports whether the egress categories include category, e.g. "dns".
func (e EgressConfig) Has(category string) bool { return e.has(category) }

func (e EgressConfig) has(category string) bool {
	for _, c := range e.Categories {
		if strings.EqualFold(c, category) {
			return true
		}
	}
	return false
}

// open reports whether egress is unrestricted (blanket NAT, no allowlist).
func (e EgressConfig) open() bool { return e.has("open") }

// metaCats returns the GitHub /meta categories selected in the config.
func (e EgressConfig) metaCats() []string {
	var out []string
	for _, c := range e.Categories {
		if metaCategories[strings.ToLower(c)] {
			out = append(out, strings.ToLower(c))
		}
	}
	return out
}

// fetchMetaCIDRs fetches api.github.com/meta and returns the IPv4 CIDRs for the
// requested categories. IPv6 ranges are skipped (guests are IPv4-only).
func fetchMetaCIDRs(ctx context.Context, cl *http.Client, url string, categories []string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta API returned %s", resp.Status)
	}

	var meta map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode meta: %w", err)
	}

	seen := map[string]bool{}
	var cidrs []string
	for _, cat := range categories {
		raw, ok := meta[cat]
		if !ok {
			continue
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("decode meta category %q: %w", cat, err)
		}
		for _, c := range list {
			if strings.Contains(c, ":") { // skip IPv6
				continue
			}
			if !seen[c] {
				seen[c] = true
				cidrs = append(cidrs, c)
			}
		}
	}
	sort.Strings(cidrs)
	return cidrs, nil
}

// buildNFTRuleset returns a complete nftables ruleset for the firerunner table.
// It atomically replaces the table (add/delete/define idiom) so it is idempotent
// across runs and refreshes.
//
// The ip table carries four concerns, all scoped to firerunner's own guest
// interfaces (IfacePrefix) so nothing here touches the host's other traffic:
//   - input: drop guest->host traffic so a guest cannot reach host-local
//     services, except the optional cache-server port on its gateway. The chain
//     uses policy accept and matches only guest interfaces, because a base-chain
//     drop is terminal for EVERY packet at the input hook (it would black-hole
//     the host's own SSH etc.).
//   - anti-spoof: on guest interfaces, drop any packet whose source is not a VM
//     address, so a guest cannot forge the host's or another network's IP.
//   - forward: the egress allowlist (or blanket NAT when open), which also
//     blocks guest-to-guest traffic. In every mode guests are also kept off
//     non-public networks (the host's LAN, the tailnet, link-local metadata)
//     and outbound mail ports; see guestEgressGuards.
//   - postrouting: masquerade guest egress out the external interface.
//
// A companion ip6 table drops all guest-originated IPv6 (guests are IPv4-only),
// as defense-in-depth. Both interface-keyed features are emitted only when
// IfacePrefix is set; an empty prefix would glob every host interface.
// It is pure so the generated policy can be asserted in tests.
func buildNFTRuleset(cfg egressRuleset) (string, error) {
	if cfg.IfacePrefix != "" {
		prefix, err := netip.ParsePrefix(cfg.VMCidr)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 16 || prefix != prefix.Masked() || prefix.Addr().As4()[0] != 172 || cfg.MaxVMs < 0 || cfg.MaxVMs > 256 {
			return "", fmt.Errorf("invalid guest source binding configuration: CIDR %q, pool %d (need 172.x.0.0/16 and 0..256 slots)", cfg.VMCidr, cfg.MaxVMs)
		}
	}
	table := cfg.Table
	if table == "" {
		table = natTable
	}
	var iface string
	if cfg.IfacePrefix != "" {
		iface = cfg.IfacePrefix + "*"
	}
	var b strings.Builder
	// Atomic replace: ensure the table exists, delete it, recreate fresh.
	fmt.Fprintf(&b, "add table ip %s\n", table)
	fmt.Fprintf(&b, "delete table ip %s\n", table)
	fmt.Fprintf(&b, "table ip %s {\n", table)

	if iface != "" {
		// Enumerate the pool, not just active VMs: refreshes cannot erase a live
		// binding and a new VM is protected before its interface comes up.
		b.WriteString("\tcounter guest_spoof_drop { }\n\tset guest_sources {\n\t\ttype ifname . ipv4_addr\n")
		pool := cfg.MaxVMs
		if pool <= 0 {
			pool = 256
		}
		prefix, err := netip.ParsePrefix(cfg.VMCidr)
		if err == nil && prefix.Addr().Is4() && prefix.Bits() == 16 && pool <= 256 {
			octets := prefix.Addr().As4()
			var pairs []string
			for slot := 0; slot < pool; slot++ {
				n := slotNet(slot, cfg.IfacePrefix, int(octets[1]))
				name := n.tap
				if cfg.NetNS {
					name = slotNetNS(n, int(octets[1]), cfg.IfacePrefix).hostVeth
				}
				pairs = append(pairs, fmt.Sprintf("\"%s\" . %s", name, n.guestIP))
			}
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(pairs, ", "))
		}
		b.WriteString("\t}\n")
	}
	fmt.Fprintf(&b, "\tset nonpublic {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { %s }\n\t}\n", strings.Join(nonPublicNets, ", "))

	if !cfg.Open {
		fmt.Fprintf(&b, "\tset allowed {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n")
		if len(cfg.Allowed) > 0 {
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(cfg.Allowed, ", "))
		}
		b.WriteString("\t}\n")

		if cfg.AllowDNS && len(cfg.DNSServers) > 0 {
			fmt.Fprintf(&b, "\tset dns {\n\t\ttype ipv4_addr\n\t\telements = { %s }\n\t}\n", strings.Join(cfg.DNSServers, ", "))
		}
	}

	// input: protect the host itself from guest-originated traffic.
	if iface != "" {
		b.WriteString("\tchain input {\n\t\ttype filter hook input priority 0; policy accept;\n")
		fmt.Fprintf(&b, "\t\tiifname \"%s\" ip saddr != %s drop\n", iface, cfg.VMCidr)
		fmt.Fprintf(&b, "\t\tiifname \"%s\" iifname . ip saddr != @guest_sources counter name guest_spoof_drop drop\n", iface)
		fmt.Fprintf(&b, "\t\tiifname \"%s\" ct state established,related accept\n", iface)
		if cfg.CachePort > 0 {
			fmt.Fprintf(&b, "\t\tiifname \"%s\" ip daddr %s tcp dport %d accept\n", iface, cfg.VMCidr, cfg.CachePort)
		}
		fmt.Fprintf(&b, "\t\tiifname \"%s\" drop\n", iface)
		b.WriteString("\t}\n")
	}

	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n")
	if iface != "" {
		fmt.Fprintf(&b, "\t\tiifname \"%s\" ip saddr != %s drop\n", iface, cfg.VMCidr)
		fmt.Fprintf(&b, "\t\tiifname \"%s\" iifname . ip saddr != @guest_sources counter name guest_spoof_drop drop\n", iface)
	}
	if !cfg.Open {
		fmt.Fprintf(&b, "\t\tip saddr %s ct state established,related accept\n", cfg.VMCidr)
		if cfg.AllowDNS && len(cfg.DNSServers) > 0 {
			fmt.Fprintf(&b, "\t\tip saddr %s udp dport 53 ip daddr @dns accept\n", cfg.VMCidr)
			fmt.Fprintf(&b, "\t\tip saddr %s tcp dport 53 ip daddr @dns accept\n", cfg.VMCidr)
		}
		// The guards precede the allowlist accept: an allowlisted range must not
		// open a mail port, nor a non-public address.
		guestEgressGuards(&b, cfg.VMCidr)
		fmt.Fprintf(&b, "\t\tip saddr %s ip daddr @allowed accept\n", cfg.VMCidr)
		if cfg.AllowNTP {
			fmt.Fprintf(&b, "\t\tip saddr %s udp dport 123 accept\n", cfg.VMCidr)
		}
		fmt.Fprintf(&b, "\t\tip saddr %s drop\n", cfg.VMCidr)
	} else {
		// Even with unrestricted egress, guests must not reach each other: a
		// per-VM /30 tap already blocks L2 peers, but in open mode the host would
		// otherwise route L3 traffic between guests on the same instance. Drop
		// intra-CIDR traffic while leaving all other forwarding (guest->internet)
		// to the accept policy.
		fmt.Fprintf(&b, "\t\tip saddr %s ip daddr %s drop\n", cfg.VMCidr, cfg.VMCidr)
		guestEgressGuards(&b, cfg.VMCidr)
	}
	b.WriteString("\t}\n")

	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s oifname \"%s\" masquerade\n", cfg.VMCidr, cfg.ExtIface)
	b.WriteString("\t}\n}\n")

	// ip6: guests are IPv4-only, so black-hole any IPv6 they emit (to the host
	// or forwarded) rather than relying on the absence of a v6 address. Keyed on
	// the guest interfaces with policy accept so host IPv6 is untouched.
	if iface != "" {
		fmt.Fprintf(&b, "add table ip6 %s\n", table)
		fmt.Fprintf(&b, "delete table ip6 %s\n", table)
		fmt.Fprintf(&b, "table ip6 %s {\n", table)
		fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority 0; policy accept;\n\t\tiifname \"%s\" drop\n\t}\n", iface)
		fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n\t\tiifname \"%s\" drop\n\t}\n", iface)
		b.WriteString("}\n")
	}
	writeConnLimit(&b, table, iface, cfg)
	return b.String(), nil
}

const (
	// DefaultGuestConnLimit is how many tracked connections one guest may hold
	// at once by default. A job rarely holds more than a few hundred.
	DefaultGuestConnLimit = 4096
	// DefaultGuestConnRate is how many new connections per second one guest
	// may open by default, with bursts of ten times that.
	DefaultGuestConnRate = 100

	// conntrackTimeWait is how long, in seconds, the kernel keeps a closed TCP
	// connection (nf_conntrack_tcp_timeout_time_wait). The count limit no
	// longer sees such a connection, so only the rate bounds them.
	conntrackTimeWait = 120
	// conntrackHostReserve is room in the table for the host's own traffic.
	conntrackHostReserve = 65536
)

// MaxGuestConnLimit bounds --guest-conn-limit and --guest-conn-rate, well
// inside the 32 bits nftables gives a count and a burst (ten times the rate).
const MaxGuestConnLimit = 1 << 20

// conntrackEntries is how many conntrack entries maxVMs guests can occupy
// under the limits, plus conntrackHostReserve: each guest's open connections,
// and the closed ones its rate and burst leave behind for conntrackTimeWait
// seconds. 0 means a limit is off, and so no bound.
func conntrackEntries(maxVMs, limit, rate int) int {
	if limit == 0 || rate == 0 {
		return 0
	}
	return maxVMs*(limit+rate*conntrackTimeWait+10*rate) + conntrackHostReserve
}

// writeConnLimit bounds what one guest can put in the host's conntrack
// table. That table is shared by every guest and the host: once it is full,
// new connections are dropped for all of them, the daemon's own GitHub and
// AWS traffic included. Two rules cover new connections in any protocol, in
// both directions a guest can open one (forwarded, and to the host):
//
//   - a count: at most ConnLimit tracked connections per guest address. The
//     kernel stops counting a TCP connection once it closes, but keeps its
//     entry for conntrackTimeWait seconds;
//   - a rate: at most ConnRate new connections per second, which bounds those
//     closed entries to ConnRate * conntrackTimeWait.
//
// They live in their own table, <table>_conns, written with add commands that
// leave an existing one in place: the main table is deleted and rebuilt on
// every allowlist refresh, and with it any count of the connections a guest
// already holds, which would hand the guest a fresh allowance each time. The
// first write of a process passes ResetConnLimit to rebuild it, so a changed
// limit or layout takes effect. Its chains run after the main table's
// (priority 1), so a spoofed source is dropped there before it can be counted
// against the guest it names.
func writeConnLimit(b *strings.Builder, table, iface string, cfg egressRuleset) {
	t := table + "_conns"
	if cfg.ResetConnLimit {
		fmt.Fprintf(b, "add table ip %s\n", t)
		fmt.Fprintf(b, "delete table ip %s\n", t)
	}
	if cfg.ConnLimit <= 0 && cfg.ConnRate <= 0 {
		return
	}
	fmt.Fprintf(b, "add table ip %s\n", t)
	var rules []string
	if cfg.ConnRate > 0 {
		fmt.Fprintf(b, "add set ip %s guest_rate { type ipv4_addr; size 65536; flags dynamic; }\n", t)
		fmt.Fprintf(b, "add counter ip %s guest_rate_drop\n", t)
		rules = append(rules, fmt.Sprintf("ct state new add @guest_rate { ip saddr limit rate over %d/second burst %d packets } counter name guest_rate_drop drop", cfg.ConnRate, 10*cfg.ConnRate))
	}
	if cfg.ConnLimit > 0 {
		fmt.Fprintf(b, "add set ip %s guest_conns { type ipv4_addr; size 65536; flags dynamic; }\n", t)
		fmt.Fprintf(b, "add counter ip %s guest_conn_drop\n", t)
		rules = append(rules, fmt.Sprintf("ct state new add @guest_conns { ip saddr ct count over %d } counter name guest_conn_drop drop", cfg.ConnLimit))
	}
	fmt.Fprintf(b, "add chain ip %s forward { type filter hook forward priority 1; policy accept; }\n", t)
	fmt.Fprintf(b, "flush chain ip %s forward\n", t)
	fmt.Fprintf(b, "add chain ip %s input { type filter hook input priority 1; policy accept; }\n", t)
	fmt.Fprintf(b, "flush chain ip %s input\n", t)
	for _, rule := range rules {
		fmt.Fprintf(b, "add rule ip %s forward ip saddr %s %s\n", t, cfg.VMCidr, rule)
		if iface != "" {
			fmt.Fprintf(b, "add rule ip %s input iifname \"%s\" ip saddr %s %s\n", t, iface, cfg.VMCidr, rule)
		}
	}
}

// nonPublicNets are the IPv4 ranges that are not globally reachable (RFC 6890):
// the host's LAN and any other RFC1918 network, the tailnet's CGNAT range,
// link-local (cloud metadata), loopback, documentation (TEST-NET-1/2/3),
// benchmarking, multicast and reserved space. A CI guest runs untrusted code;
// nothing it should reach lives in these ranges. 192.88.99.0/24 (deprecated
// 6to4 relay anycast) is deliberately absent: RFC 6890 marks it global.
var nonPublicNets = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

// mailPorts are the SMTP ports a guest must never open a connection to, so a
// job cannot send mail (spam) from the host's address.
const mailPorts = "25, 465, 587, 2525"

// guestEgressGuards writes the forward rules that hold in every egress mode:
// guests reach no non-public network and no mail port. The forward hook sees a
// packet after prerouting DNAT, so DNS the HOST rewrote (the CI hosts redirect
// guest DNS to a resolver on a private veth) is exempt from the non-public
// drop: a guest cannot create a DNAT entry, only the host's own rules can.
// Only DNS is exempt, over TCP or UDP to port 53 as the guest sent it. A host
// has other DNAT rules (Docker's or Kubernetes' published ports, which
// rewrite to private container addresses), and exempting every DNAT'd flow
// would let a guest reach the services behind them. In allowlist mode these
// come after the explicit resolver accept, so a configured LAN resolver stays
// reachable on port 53 only.
//
// ⚠️ The exemption must test the dnat BIT: "ct status != dnat" compiles to an
// exact compare against 0x20, which every real connection (confirmed, seen-
// reply, ...) fails, so it would drop host-DNAT'd traffic too.
func guestEgressGuards(b *strings.Builder, vmCidr string) {
	fmt.Fprintf(b, "\t\tip saddr %s ip daddr @nonpublic ct status & dnat == 0 drop\n", vmCidr)
	fmt.Fprintf(b, "\t\tip saddr %s ip daddr @nonpublic meta l4proto != { tcp, udp } drop\n", vmCidr)
	fmt.Fprintf(b, "\t\tip saddr %s ip daddr @nonpublic ct original proto-dst != 53 drop\n", vmCidr)
	fmt.Fprintf(b, "\t\tip saddr %s tcp dport { %s } drop\n", vmCidr, mailPorts)
}

// egressRuleset is the fully-resolved input to buildNFTRuleset.
type egressRuleset struct {
	MaxVMs int
	NetNS  bool
	// Table is the nftables table name to manage. Empty falls back to natTable
	// so a second firerunner instance can own an isolated table.
	Table string
	// IfacePrefix is the guest interface (tap/veth) name prefix. When set it
	// enables the input host-protection chain, interface-keyed anti-spoof, and
	// the ip6 drop table. Empty disables all interface-keyed rules (which would
	// otherwise glob every host interface).
	IfacePrefix string
	ExtIface    string
	VMCidr      string
	Allowed     []string
	DNSServers  []string
	// CachePort, when >0 and IfacePrefix is set, lets guests reach the host
	// cache-server on this TCP port via their gateway despite the input drop.
	CachePort int
	AllowDNS  bool
	AllowNTP  bool
	Open      bool
	// ConnLimit caps each guest's tracked connections and ConnRate its new
	// connections per second; 0 writes no such limit. ResetConnLimit rebuilds
	// the limit table rather than keeping its counts. See writeConnLimit.
	ConnLimit      int
	ConnRate       int
	ResetConnLimit bool
}
