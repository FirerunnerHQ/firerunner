package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleMeta = `{
	"verifiable_password_authentication": false,
	"api": ["192.30.252.0/22", "2606:50c0::/32"],
	"actions": ["13.64.0.0/16", "192.30.252.0/22"],
	"git": ["140.82.112.0/20"],
	"packages": ["140.82.121.0/24"],
	"web": ["1.2.3.0/24"]
}`

func TestFetchMetaCIDRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleMeta))
	}))
	defer srv.Close()

	cidrs, err := fetchMetaCIDRs(context.Background(), srv.Client(), srv.URL, []string{"api", "actions", "git", "packages"})
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(cidrs, ",")
	for _, want := range []string{"192.30.252.0/22", "13.64.0.0/16", "140.82.112.0/20", "140.82.121.0/24"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing CIDR %q in %v", want, cidrs)
		}
	}
	// IPv6 must be filtered out.
	if strings.Contains(joined, ":") {
		t.Errorf("IPv6 range not filtered: %v", cidrs)
	}
	// "web" was not requested, so must be absent.
	if strings.Contains(joined, "1.2.3.0/24") {
		t.Errorf("unrequested category leaked: %v", cidrs)
	}
	// Dedup: 192.30.252.0/22 appears in both api and actions but once here.
	if n := strings.Count(joined, "192.30.252.0/22"); n != 1 {
		t.Errorf("duplicate CIDR count = %d, want 1", n)
	}
}

func TestFetchMetaCIDRsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := fetchMetaCIDRs(context.Background(), srv.Client(), srv.URL, []string{"api"}); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

// TestBuildNFTRulesetCustomTable asserts a second instance can own an isolated
// nft table so the two rulesets never clobber each other.
func TestBuildNFTRulesetCustomTable(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{
		Table:    "firerunner_node",
		ExtIface: "enp2s0",
		VMCidr:   "172.17.0.0/16",
		Open:     true,
	})
	for _, want := range []string{
		"add table ip firerunner_node",
		"delete table ip firerunner_node",
		"table ip firerunner_node {",
		"172.17.0.0/16",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ruleset missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "table ip firerunner {") {
		t.Fatalf("custom table leaked the default name:\n%s", out)
	}
}

func TestBuildNFTRulesetAllowlist(t *testing.T) {
	rs := egressRuleset{
		ExtIface:   "enp2s0",
		VMCidr:     vmCIDR,
		Allowed:    []string{"140.82.112.0/20", "13.64.0.0/16"},
		DNSServers: []string{"1.1.1.1", "8.8.8.8"},
		AllowDNS:   true,
		AllowNTP:   true,
	}
	out := mustNFTRuleset(t, rs)

	// Atomic table-replace idiom.
	for _, want := range []string{
		"add table ip firerunner",
		"delete table ip firerunner",
		"table ip firerunner {",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing table-replace line %q", want)
		}
	}
	// Allowlist set + elements. auto-merge lets nftables fold the overlapping
	// CIDRs GitHub publishes in /meta (real nft rejects conflicting intervals).
	if !strings.Contains(out, "set allowed {") || !strings.Contains(out, "flags interval") || !strings.Contains(out, "auto-merge") {
		t.Errorf("missing allowed set:\n%s", out)
	}
	for _, cidr := range rs.Allowed {
		if !strings.Contains(out, cidr) {
			t.Errorf("allowed set missing %q", cidr)
		}
	}
	// DNS set + rules.
	if !strings.Contains(out, "set dns {") || !strings.Contains(out, "udp dport 53 ip daddr @dns accept") {
		t.Errorf("missing dns handling:\n%s", out)
	}
	// NTP rule.
	if !strings.Contains(out, "udp dport 123 accept") {
		t.Errorf("missing ntp rule:\n%s", out)
	}
	// Forward chain, allowlist accept and default drop.
	if !strings.Contains(out, "hook forward") || !strings.Contains(out, "ip daddr @allowed accept") {
		t.Errorf("missing forward allow rule:\n%s", out)
	}
	if !strings.Contains(out, vmCIDR+" drop") {
		t.Errorf("missing default drop for guests:\n%s", out)
	}
	// Masquerade always present.
	if !strings.Contains(out, "masquerade") || !strings.Contains(out, `oifname "enp2s0"`) {
		t.Errorf("missing masquerade:\n%s", out)
	}
}

func TestBuildNFTRulesetOpen(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Open: true})
	// Open mode has no allowlist, but must still block guest-to-guest traffic.
	if strings.Contains(out, "@allowed") {
		t.Errorf("open mode must not have an allowlist:\n%s", out)
	}
	wantDrop := "ip saddr " + vmCIDR + " ip daddr " + vmCIDR + " drop"
	if !strings.Contains(out, wantDrop) {
		t.Errorf("open mode must drop intra-CIDR guest-to-guest traffic (%q):\n%s", wantDrop, out)
	}
	if !strings.Contains(out, "policy accept") {
		t.Errorf("open mode forward chain must default-accept guest->internet:\n%s", out)
	}
	if !strings.Contains(out, "masquerade") {
		t.Errorf("open mode still needs masquerade:\n%s", out)
	}
}

func TestBuildNFTRulesetNoDNSNoNTP(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{
		ExtIface: "eth0",
		VMCidr:   vmCIDR,
		Allowed:  []string{"140.82.112.0/20"},
	})
	if strings.Contains(out, "set dns") || strings.Contains(out, "dport 53") {
		t.Errorf("dns must be absent when not allowed:\n%s", out)
	}
	if strings.Contains(out, "dport 123") {
		t.Errorf("ntp must be absent when not allowed:\n%s", out)
	}
	if !strings.Contains(out, "ip daddr @allowed accept") {
		t.Errorf("allowlist still expected:\n%s", out)
	}
}

// TestBuildNFTRulesetHostHardening covers the guest->host protections: an input
// chain that drops guest-originated traffic (except the cache port), interface-
// keyed anti-spoof on both input and forward, and an ip6 drop table. All are
// keyed on the guest interface prefix so the host's own traffic is untouched.
func TestBuildNFTRulesetHostHardening(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{
		ExtIface:    "eth0",
		IfacePrefix: "fr",
		VMCidr:      vmCIDR,
		CachePort:   8099,
		Allowed:     []string{"140.82.112.0/20"},
	})
	for _, want := range []string{
		// input chain, policy accept (never black-holes host input).
		"chain input {",
		"hook input priority 0; policy accept;",
		// anti-spoof on guest interfaces, in both input and forward.
		`iifname "fr*" ip saddr != ` + vmCIDR + " drop",
		// cache port reachable on the gateway despite the input drop.
		`iifname "fr*" ip daddr ` + vmCIDR + " tcp dport 8099 accept",
		// default guest->host drop.
		`iifname "fr*" drop`,
		// ip6 defense-in-depth table.
		"add table ip6 firerunner",
		"table ip6 firerunner {",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hardened ruleset missing %q in:\n%s", want, out)
		}
	}
	// The anti-spoof rule must appear in the forward chain too (not only input).
	if n := strings.Count(out, `iifname "fr*" ip saddr != `+vmCIDR+" drop"); n < 2 {
		t.Errorf("anti-spoof should be in both input and forward chains, found %d", n)
	}
}

// TestBuildNFTRulesetNoIfacePrefix guards the empty-prefix fallback: without a
// known guest interface the interface-keyed rules must be omitted entirely
// rather than globbing every host interface with "*".
func TestBuildNFTRulesetNoIfacePrefix(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Allowed: []string{"140.82.112.0/20"}})
	for _, absent := range []string{"chain input {", "table ip6", `iifname "*"`, "saddr != "} {
		if strings.Contains(out, absent) {
			t.Errorf("empty IfacePrefix must not emit %q:\n%s", absent, out)
		}
	}
}

// TestBuildNFTRulesetOpenHardened confirms host protection applies even in open
// egress mode: guests still must not reach host services except the cache.
func TestBuildNFTRulesetOpenHardened(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", IfacePrefix: "fr", VMCidr: vmCIDR, Open: true})
	if !strings.Contains(out, "chain input {") || !strings.Contains(out, `iifname "fr*" drop`) {
		t.Errorf("open mode must still protect the host with an input drop:\n%s", out)
	}
	if !strings.Contains(out, "table ip6 firerunner") {
		t.Errorf("open mode must still drop guest IPv6:\n%s", out)
	}
}

func TestEgressConfigHelpers(t *testing.T) {
	e := EgressConfig{Categories: []string{"api", "Actions", "dns", "open"}}
	if !e.open() {
		t.Error("open() should be true")
	}
	if !e.has("DNS") {
		t.Error("has() should be case-insensitive")
	}
	cats := e.metaCats()
	if len(cats) != 2 {
		t.Fatalf("metaCats = %v, want api+actions", cats)
	}
	joined := strings.Join(cats, ",")
	if !strings.Contains(joined, "api") || !strings.Contains(joined, "actions") {
		t.Errorf("metaCats = %v", cats)
	}
}

// forwardChain returns the forward chain's rule lines, in order.
func forwardChain(t *testing.T, ruleset string) []string {
	t.Helper()
	start := strings.Index(ruleset, "chain forward {")
	if start < 0 {
		t.Fatalf("no forward chain in:\n%s", ruleset)
	}
	body := ruleset[start:]
	body = body[:strings.Index(body, "\t}\n")]
	var rules []string
	for _, l := range strings.Split(body, "\n")[1:] {
		if l = strings.TrimSpace(l); l != "" {
			rules = append(rules, l)
		}
	}
	return rules
}

// ruleIndex returns the position of the first forward rule containing substr,
// failing the test when there is none.
func ruleIndex(t *testing.T, rules []string, substr string) int {
	t.Helper()
	for i, r := range rules {
		if strings.Contains(r, substr) {
			return i
		}
	}
	t.Fatalf("no forward rule contains %q:\n%s", substr, strings.Join(rules, "\n"))
	return -1
}

// TestBuildNFTRulesetGuestEgressGuards pins the rules that hold in EVERY egress
// mode: guests reach no non-public network (LAN, tailnet, link-local
// metadata) and no outbound mail port. Before these, open mode forwarded a
// guest to anything the host could route to, the home LAN included.
func TestBuildNFTRulesetGuestEgressGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		rs   egressRuleset
	}{
		{"open", egressRuleset{ExtIface: "eth0", IfacePrefix: "fr", VMCidr: vmCIDR, Open: true}},
		{"allowlist", egressRuleset{ExtIface: "eth0", IfacePrefix: "fr", VMCidr: vmCIDR, Allowed: []string{"140.82.112.0/20"}}},
		{"no iface prefix", egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Open: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := mustNFTRuleset(t, tc.rs)
			if !strings.Contains(out, "set nonpublic {") || !strings.Contains(out, "flags interval") {
				t.Fatalf("missing nonpublic set:\n%s", out)
			}
			for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8", "224.0.0.0/4", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
				if !strings.Contains(out, cidr) {
					t.Errorf("nonpublic set missing %s", cidr)
				}
			}
			rules := forwardChain(t, out)
			ruleIndex(t, rules, "ip saddr "+vmCIDR+" ip daddr @nonpublic ct status & dnat == 0 drop")
			ruleIndex(t, rules, "ip saddr "+vmCIDR+" tcp dport { 25, 465, 587, 2525 } drop")
		})
	}
}

// TestBuildNFTRulesetDNATExemptionTestsTheBit guards a trap verified on real
// nft 1.1.3: "ct status != dnat" compiles to an exact compare against 0x20,
// which every real connection fails, so it dropped host-DNAT'd guest DNS (the
// CI hosts redirect it to a resolver on a private veth).
func TestBuildNFTRulesetDNATExemptionTestsTheBit(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Open: true})
	if strings.Contains(out, "!= dnat") {
		t.Fatalf("dnat exemption must test the bit (& dnat == 0), not compare the whole status:\n%s", out)
	}
}

// TestBuildNFTRulesetAllowlistOrder pins the allowlist forward order: an
// explicitly configured resolver is accepted (even on the LAN) before the
// guards, and the guards precede the allowlist accept so no allowlisted range
// can open a mail port or a non-public address. NTP and the final drop follow.
func TestBuildNFTRulesetAllowlistOrder(t *testing.T) {
	rules := forwardChain(t, mustNFTRuleset(t, egressRuleset{
		ExtIface: "eth0", IfacePrefix: "fr", VMCidr: vmCIDR,
		Allowed:    []string{"140.82.112.0/20"},
		DNSServers: []string{"192.168.4.1"},
		AllowDNS:   true,
		AllowNTP:   true,
	}))
	order := []string{
		"ip saddr != " + vmCIDR + " drop",
		"ct state established,related accept",
		"udp dport 53 ip daddr @dns accept",
		"tcp dport 53 ip daddr @dns accept",
		"@nonpublic",
		"dport { 25, 465, 587, 2525 } drop",
		"ip daddr @allowed accept",
		"udp dport 123 accept",
		"ip saddr " + vmCIDR + " drop",
	}
	prev := -1
	for _, want := range order {
		i := ruleIndex(t, rules, want)
		if i <= prev {
			t.Fatalf("forward rule %q at %d, want after %d:\n%s", want, i, prev, strings.Join(rules, "\n"))
		}
		prev = i
	}
	if prev != len(rules)-1 {
		t.Errorf("the default drop must be the last forward rule:\n%s", strings.Join(rules, "\n"))
	}
}

// TestBuildNFTRulesetOpenOrder pins open mode: anti-spoof, the guest-to-guest
// drop, then the guards, with the chain's accept policy for everything else.
func TestBuildNFTRulesetOpenOrder(t *testing.T) {
	rules := forwardChain(t, mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", IfacePrefix: "fr", VMCidr: vmCIDR, Open: true}))
	order := []string{
		"ip saddr != " + vmCIDR + " drop",
		"iifname . ip saddr != @guest_sources",
		"ip daddr " + vmCIDR + " drop",
		"@nonpublic",
		"dport { 25, 465, 587, 2525 } drop",
	}
	prev := -1
	for _, want := range order {
		i := ruleIndex(t, rules, want)
		if i <= prev {
			t.Fatalf("forward rule %q at %d, want after %d:\n%s", want, i, prev, strings.Join(rules, "\n"))
		}
		prev = i
	}
	if len(rules) != len(order)+1 { // +1: the chain's type/hook/policy line
		t.Errorf("open mode forward chain has unexpected rules:\n%s", strings.Join(rules, "\n"))
	}
}

func TestGuestSourceBindings(t *testing.T) {
	for _, netns := range []bool{false, true} {
		for _, open := range []bool{false, true} {
			t.Run(fmt.Sprintf("netns=%t/open=%t", netns, open), func(t *testing.T) {
				cfg := egressRuleset{IfacePrefix: "custom", VMCidr: "172.19.0.0/16", ExtIface: "eth0", MaxVMs: 2, NetNS: netns, Open: open, CachePort: 8099}
				rules := mustNFTRuleset(t, cfg)
				for slot := 0; slot < 2; slot++ {
					name := fmt.Sprintf("custom%d", slot)
					if netns {
						name = fmt.Sprintf("customv%dh", slot)
					}
					if !strings.Contains(rules, fmt.Sprintf("\"%s\" . 172.19.%d.2", name, slot)) {
						t.Fatal("missing exact source binding")
					}
				}
				if strings.Count(rules, "iifname . ip saddr != @guest_sources") != 2 {
					t.Fatal("both input and forward need bindings")
				}
				if i := strings.Index(rules, "iifname . ip saddr != @guest_sources"); i < 0 || i > strings.Index(rules, "ct state established,related accept") {
					t.Fatal("source check must precede established accept")
				}
				if mustNFTRuleset(t, cfg) != rules {
					t.Fatal("refresh lost source bindings")
				}
			})
		}
	}
}

func mustNFTRuleset(t *testing.T, cfg egressRuleset) string {
	t.Helper()
	rules, err := buildNFTRuleset(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}
func TestInvalidSourceBindingsFailBeforeRules(t *testing.T) {
	for _, cfg := range []egressRuleset{
		{IfacePrefix: "fr", VMCidr: "invalid"},
		{IfacePrefix: "fr", VMCidr: "172.16.0.0/24"},
		{IfacePrefix: "fr", VMCidr: "172.16.1.0/16"},
		{IfacePrefix: "fr", VMCidr: "10.16.0.0/16"},
		{IfacePrefix: "fr", VMCidr: "::/16"},
		{IfacePrefix: "fr", VMCidr: vmCIDR, MaxVMs: 257},
		{IfacePrefix: "fr", VMCidr: vmCIDR, MaxVMs: -1},
	} {
		if rules, err := buildNFTRuleset(cfg); err == nil || rules != "" {
			t.Fatalf("invalid bindings emitted rules: %+v %v", cfg, err)
		}
	}
}
