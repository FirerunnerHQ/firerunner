package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
func buildNFTRuleset(cfg egressRuleset) string {
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
	return b.String()
}

// nonPublicNets are the IPv4 ranges that are not globally reachable (RFC 6890):
// the host's LAN and any other RFC1918 network, the tailnet's CGNAT range,
// link-local (cloud metadata), loopback, multicast and reserved space. A CI
// guest runs untrusted code; nothing it should reach lives in these ranges.
var nonPublicNets = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

// mailPorts are the SMTP ports a guest must never open a connection to, so a
// job cannot send mail (spam) from the host's address.
const mailPorts = "25, 465, 587, 2525"

// guestEgressGuards writes the forward rules that hold in every egress mode:
// guests reach no non-public network and no mail port. The forward hook sees a
// packet after prerouting DNAT, so a destination the HOST rewrote (the CI
// hosts redirect guest DNS to a resolver on a private veth) is exempt from the
// non-public drop: a guest cannot create a DNAT entry, only the host's own
// rules can. In allowlist mode these come after the explicit resolver accept,
// so a configured LAN resolver stays reachable on port 53 only.
//
// ⚠️ The exemption must test the dnat BIT: "ct status != dnat" compiles to an
// exact compare against 0x20, which every real connection (confirmed, seen-
// reply, ...) fails, so it would drop host-DNAT'd traffic too.
func guestEgressGuards(b *strings.Builder, vmCidr string) {
	fmt.Fprintf(b, "\t\tip saddr %s ip daddr @nonpublic ct status & dnat == 0 drop\n", vmCidr)
	fmt.Fprintf(b, "\t\tip saddr %s tcp dport { %s } drop\n", vmCidr, mailPorts)
}

// egressRuleset is the fully-resolved input to buildNFTRuleset.
type egressRuleset struct {
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
}
