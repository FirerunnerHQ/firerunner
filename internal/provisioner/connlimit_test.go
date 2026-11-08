package provisioner

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// The limits live in a table that a refresh never deletes, so a guest's
// counts survive an allowlist refresh, and they cover both the forward and
// the host input path.
func TestBuildNFTRulesetConnLimits(t *testing.T) {
	out := mustNFTRuleset(t, egressRuleset{
		ExtIface: "eth0", IfacePrefix: "fr", VMCidr: vmCIDR, CachePort: 8099,
		Allowed:   []string{"140.82.112.0/20"},
		ConnLimit: 4096, ConnRate: 100,
	})
	count := "ct state new add @guest_conns { ip saddr ct count over 4096 } counter name guest_conn_drop drop"
	rate := "ct state new add @guest_rate { ip saddr limit rate over 100/second burst 1000 packets } counter name guest_rate_drop drop"
	for _, want := range []string{
		"add table ip firerunner_conns\n",
		"add set ip firerunner_conns guest_conns { type ipv4_addr; size 65536; flags dynamic; }\n",
		"add set ip firerunner_conns guest_rate { type ipv4_addr; size 65536; flags dynamic; }\n",
		"add chain ip firerunner_conns forward { type filter hook forward priority 1; policy accept; }\n",
		"add chain ip firerunner_conns input { type filter hook input priority 1; policy accept; }\n",
		"add rule ip firerunner_conns forward ip saddr " + vmCIDR + " " + count + "\n",
		"add rule ip firerunner_conns forward ip saddr " + vmCIDR + " " + rate + "\n",
		`add rule ip firerunner_conns input iifname "fr*" ip saddr ` + vmCIDR + " " + count + "\n",
		`add rule ip firerunner_conns input iifname "fr*" ip saddr ` + vmCIDR + " " + rate + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "delete table ip firerunner_conns") {
		t.Error("a refresh deletes the limit table, which resets every guest's counts")
	}

	reset := mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Open: true, ConnLimit: 5, ResetConnLimit: true})
	if i, j := strings.Index(reset, "delete table ip firerunner_conns\n"), strings.LastIndex(reset, "add table ip firerunner_conns\n"); i < 0 || j < i {
		t.Errorf("ResetConnLimit: want the table deleted, then written again, in:\n%s", reset)
	}
	if strings.Contains(reset, "guest_rate") || strings.Contains(reset, "add rule ip firerunner_conns input") {
		t.Errorf("ConnLimit only, without an interface prefix: want a forward-only count in:\n%s", reset)
	}
	if off := mustNFTRuleset(t, egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Open: true}); strings.Contains(off, "_conns") {
		t.Errorf("no limits still writes a limit table:\n%s", off)
	}
}

// netRecorder fakes the host for applyNetwork and records what runs.
type netRecorder struct {
	runs        [][]string
	conntrackMx string
}

func (r *netRecorder) install(f *Firecracker) {
	f.run = func(_ context.Context, name string, args ...string) error {
		r.runs = append(r.runs, append([]string{name}, args...))
		return nil
	}
	f.runOut = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "sysctl" && slices.Equal(args, []string{"-n", "net.netfilter.nf_conntrack_max"}) {
			return r.conntrackMx + "\n", nil
		}
		return "", errors.New("no such chain")
	}
}

func (r *netRecorder) ran(cmd ...string) bool {
	return slices.ContainsFunc(r.runs, func(c []string) bool { return slices.Equal(c, cmd) })
}

// The first apply of a process rebuilds the limit table, so a changed limit
// takes effect; refreshes keep it, and its counts.
func TestApplyNetworkResetsConnLimitsOnce(t *testing.T) {
	work := t.TempDir()
	f := NewFirecracker(FirecrackerConfig{
		ExtIface: "eth0", WorkDir: work, MaxVMs: 2, Egress: EgressConfig{Categories: []string{"open"}},
		GuestConnLimit: 10, GuestConnRate: 1,
	}, testLogger())
	var r netRecorder
	r.conntrackMx = "1000000"
	r.install(f)
	rules := func() string {
		t.Helper()
		if err := f.applyNetwork(context.Background()); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(work + "/firerunner.nft")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if first := rules(); !strings.Contains(first, "delete table ip firerunner_conns") {
		t.Errorf("first apply keeps a limit table from an earlier process:\n%s", first)
	}
	if again := rules(); strings.Contains(again, "delete table ip firerunner_conns") {
		t.Errorf("a refresh deletes the limit table and its counts:\n%s", again)
	}
}

// nf_conntrack_max is raised to fit every guest at its limits, and never
// lowered.
func TestApplyNetworkRaisesConntrackMax(t *testing.T) {
	for _, tc := range []struct {
		have  string
		raise bool
	}{
		{"262144", true},
		{"2000000", false},
	} {
		f := NewFirecracker(FirecrackerConfig{
			ExtIface: "eth0", WorkDir: t.TempDir(), MaxVMs: 30, Egress: EgressConfig{Categories: []string{"open"}},
			GuestConnLimit: DefaultGuestConnLimit, GuestConnRate: DefaultGuestConnRate,
		}, testLogger())
		r := netRecorder{conntrackMx: tc.have}
		r.install(f)
		if err := f.applyNetwork(context.Background()); err != nil {
			t.Fatal(err)
		}
		// 30 guests * (4096 open + 100/s * 120 s closed + a burst of 1000)
		// + 65536 for the host.
		raised := r.ran("sysctl", "-w", "net.netfilter.nf_conntrack_max=578416")
		if raised != tc.raise {
			t.Errorf("nf_conntrack_max %s: raised = %v, want %v (ran %q)", tc.have, raised, tc.raise, r.runs)
		}
	}
}

// A guest's conntrack entries are removed when its network is torn down, so
// a connection a job left open neither stays in the shared table nor counts
// against the next guest on the address.
func TestGuestConntrackFlushedAtTeardown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  [][]string
		netns bool
	}{
		{"limited", 10, [][]string{{"172.16.3.2"}}, false},
		{"limited netns", 10, [][]string{{"172.16.3.2"}}, true},
		{"no limits", 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFirecracker(FirecrackerConfig{ExtIface: "eth0", WorkDir: t.TempDir(), NetNS: tc.netns, GuestConnLimit: tc.limit}, testLogger())
			var r netRecorder
			r.install(f)
			var flushed [][]string
			f.flushConntrack = func(ips ...string) error { flushed = append(flushed, ips); return nil }
			n := slotNet(3, "fr", 16)
			if err := f.setupNet(context.Background(), n); err != nil {
				t.Fatal(err)
			}
			if flushed != nil {
				t.Errorf("setup listed the conntrack table: %q", flushed)
			}
			_ = f.teardownNet(context.Background(), n)
			if !slices.EqualFunc(flushed, tc.want, slices.Equal) {
				t.Errorf("flushed %q, want %q", flushed, tc.want)
			}
		})
	}
}

// At startup every slot's entries go in one listing, for a daemon that died
// mid-job and never tore its guests down.
func TestCleanupStaleFlushesEverySlotOnce(t *testing.T) {
	f := NewFirecracker(FirecrackerConfig{ExtIface: "eth0", WorkDir: t.TempDir(), MaxVMs: 3, TapPrefix: "frx", GuestConnRate: 5}, testLogger())
	var r netRecorder
	r.install(f)
	var flushed [][]string
	f.flushConntrack = func(ips ...string) error { flushed = append(flushed, ips); return nil }
	f.CleanupStale(context.Background())
	want := [][]string{{"172.16.0.2", "172.16.1.2", "172.16.2.2"}}
	if !slices.EqualFunc(flushed, want, slices.Equal) {
		t.Errorf("flushed %q, want %q", flushed, want)
	}
}
