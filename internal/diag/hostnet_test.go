package diag

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// cmdResult is what a faked command prints and returns.
type cmdResult struct {
	stdout, stderr string
	err            error
}

// fakeHost answers runCmd by the longest "name arg…" prefix in cmds, readProc
// from procs (missing: not exist), and lookupVia by resolver, for the test's
// lifetime. An unknown command is not installed.
func fakeHost(t *testing.T, cmds map[string]cmdResult, procs map[string]string, answers map[string]error) {
	t.Helper()
	oldRun, oldLookup, oldProc := runCmd, lookupVia, readProc
	t.Cleanup(func() { runCmd, lookupVia, readProc = oldRun, oldLookup, oldProc })
	runCmd = func(name string, args ...string) (string, string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		best := ""
		for k := range cmds {
			if strings.HasPrefix(key, k) && len(k) > len(best) {
				best = k
			}
		}
		if best == "" {
			return "", "", errCmdMissing
		}
		r := cmds[best]
		return r.stdout, r.stderr, r.err
	}
	readProc = func(path string) ([]byte, error) {
		v, ok := procs[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		if v == "EACCES" {
			return nil, os.ErrPermission
		}
		return []byte(v), nil
	}
	lookupVia = func(ns, _ string) error { return answers[ns] }
}

func byName(cs []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}

// chains is an nft -j list chains document with the given base chains, each
// "family table name hook policy".
func chains(specs ...string) string {
	var parts []string
	for _, s := range specs {
		f := strings.Fields(s)
		parts = append(parts, `{"chain":{"family":"`+f[0]+`","table":"`+f[1]+`","name":"`+f[2]+`","handle":1,"type":"filter","hook":"`+f[3]+`","prio":0,"policy":"`+f[4]+`"}}`)
	}
	return `{"nftables":[{"metainfo":{"version":"1.1.7"}},` + strings.Join(parts, ",") + `]}`
}

func TestForwardChecks(t *testing.T) {
	fc := provisioner.FirecrackerConfig{TapPrefix: "fr"}
	ours := `iifname "fr*" oifname "eth0" counter accept comment "` + provisioner.ForwardComment("fr") + `"`
	peers := `iifname "frx*" oifname "eth0" counter accept comment "` + provisioner.ForwardComment("frx") + `"`
	for _, c := range []struct {
		name    string
		cmds    map[string]cmdResult
		procs   map[string]string
		check   string
		level   string
		contain string
	}{
		{"nothing drops", map[string]cmdResult{"nft -j list chains": {stdout: chains("ip filter FORWARD forward accept", "ip filter INPUT input drop")}}, nil,
			"forward-policy", levelPass, "no forward chain drops"},
		{"docker drop with our accepts", map[string]cmdResult{
			"nft -j list chains":               {stdout: chains("ip filter FORWARD forward drop")},
			"nft list chain ip filter FORWARD": {stdout: ours}}, nil,
			"forward-policy", levelPass, "with accepts for fr*: ip filter FORWARD"},
		{"docker drop without them", map[string]cmdResult{
			"nft -j list chains":               {stdout: chains("ip filter FORWARD forward drop")},
			"nft list chain ip filter FORWARD": {stdout: "policy drop;"}}, nil,
			"forward-policy", levelWarn, "no accepts for fr* yet"},
		{"only a peer prefix's accepts", map[string]cmdResult{
			"nft -j list chains":               {stdout: chains("ip filter FORWARD forward drop")},
			"nft list chain ip filter FORWARD": {stdout: peers}}, nil,
			"forward-policy", levelWarn, "no accepts for fr* yet"},
		{"arch inet filter drops", map[string]cmdResult{
			"nft -j list chains":                 {stdout: chains("inet filter forward forward drop")},
			"nft list chain inet filter forward": {stdout: "policy drop;"}}, nil,
			"forward-policy", levelFail, "inet filter forward drop forwarded traffic"},
		{"inet filter drops, with rules for the taps", map[string]cmdResult{
			"nft -j list chains":                 {stdout: chains("inet filter forward forward drop")},
			"nft list chain inet filter forward": {stdout: `policy drop; iifname "fr*" accept`}}, nil,
			"forward-policy", levelPass, "inet filter forward (has rules for fr*; check they accept)"},
		{"legacy drop with only the inbound accept", map[string]cmdResult{
			"nft -j list chains":         {stdout: chains()},
			"iptables-legacy -S FORWARD": {stdout: "-P FORWARD DROP\n-A FORWARD -i fr+ -j ACCEPT\n"}}, map[string]string{"/proc/net/ip_tables_names": "filter\n"},
			"forward-legacy", levelFail, "legacy iptables FORWARD policy is DROP"},
		{"ip6 drop is not ours to judge", map[string]cmdResult{"nft -j list chains": {stdout: chains("ip6 filter FORWARD forward drop")}}, nil,
			"forward-policy", levelPass, "no forward chain drops"},
		{"not root", map[string]cmdResult{"nft -j list chains": {err: errors.New("Error: Operation not permitted")}}, nil,
			"forward-policy", levelWarn, "without root"},
		{"legacy drop", map[string]cmdResult{
			"nft -j list chains":         {stdout: chains()},
			"iptables-legacy -S FORWARD": {stdout: "-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n"}}, map[string]string{"/proc/net/ip_tables_names": "filter\nnat\n"},
			"forward-legacy", levelFail, "iptables-legacy -I FORWARD -i fr+ -j ACCEPT"},
		{"legacy drop after the fix", map[string]cmdResult{
			"nft -j list chains": {stdout: chains()},
			"iptables-legacy -S FORWARD": {stdout: "-P FORWARD DROP\n-A FORWARD -o fr+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n" +
				"-A FORWARD -i fr+ -j ACCEPT\n-A FORWARD -j DOCKER-USER\n"}}, map[string]string{"/proc/net/ip_tables_names": "filter\n"},
			"forward-legacy", levelPass, "with accepts for fr+"},
		{"legacy unreadable", map[string]cmdResult{"nft -j list chains": {stdout: chains()}}, map[string]string{"/proc/net/ip_tables_names": "EACCES"},
			"forward-legacy", levelWarn, "without root"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeHost(t, c.cmds, c.procs, nil)
			got := byName(forwardChecks(fc))
			if f, ok := got[c.check]; !ok || f.Level != c.level || !strings.Contains(f.Detail, c.contain) {
				t.Fatalf("%s = %+v, want %s containing %q (all: %+v)", c.check, f, c.level, c.contain, got)
			}
		})
	}
	t.Run("legacy iptables unused is never run", func(t *testing.T) {
		ran := false
		fakeHost(t, map[string]cmdResult{"nft -j list chains": {stdout: chains()}}, map[string]string{"/proc/net/ip_tables_names": ""}, nil)
		inner := runCmd
		runCmd = func(name string, args ...string) (string, string, error) {
			ran = ran || name == "iptables-legacy"
			return inner(name, args...)
		}
		if _, ok := byName(forwardChecks(fc))["forward-legacy"]; ok || ran {
			t.Fatalf("legacy check reported or ran (ran=%v) with no legacy filter table: listing it would create one", ran)
		}
	})
}

func TestResolvChecks(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{Egress: provisioner.EgressConfig{Categories: []string{"actions", "dns"}, DNSServers: []string{"1.1.1.1", "8.8.8.8"}}}}
	timeout := errors.New("i/o timeout")
	banner := "debugfs 1.47.4 (2-Aug-2026)\n"
	cat := "debugfs -R cat /etc/resolv.conf"
	for _, c := range []struct {
		name    string
		cmds    map[string]cmdResult
		answers map[string]error
		level   string
		contain string
	}{
		{"answers", map[string]cmdResult{cat: {stdout: "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", stderr: banner}}, nil, levelPass, "nameservers 1.1.1.1, 8.8.8.8 answer"},
		{"empty file", map[string]cmdResult{cat: {stderr: banner}, "debugfs -R stat": {stdout: "Inode: 12   Type: regular\n"}}, nil, levelFail, "has no nameserver"},
		{"no file", map[string]cmdResult{cat: {stderr: banner + "/etc/resolv.conf: File not found by ext2_lookup\n"}}, nil, levelFail, "has no /etc/resolv.conf"},
		{"image will not open", map[string]cmdResult{cat: {stderr: banner + "debugfs: Bad magic number in super-block while trying to open /img\ncat: Filesystem not open\n"}}, nil, levelWarn, "cannot open /img: debugfs: Bad magic number"},
		{"symlink", map[string]cmdResult{cat: {stderr: banner + "cat: Attempt to read block from filesystem resulted in short read\n"},
			"debugfs -R stat": {stdout: "Inode: 40   Type: symlink    Mode:  0777\nFast link dest: \"../run/systemd/resolve/stub-resolv.conf\"\n"}}, nil, levelWarn, "is a symlink (to \"../run/systemd/resolve/stub-resolv.conf\")"},
		{"outside the allowlist", map[string]cmdResult{cat: {stdout: "nameserver 192.168.4.1\n"}}, nil, levelFail, "192.168.4.1 is not in --dns-servers 1.1.1.1,8.8.8.8"},
		{"blocked upstream (#3)", map[string]cmdResult{cat: {stdout: "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"}}, map[string]error{"1.1.1.1": timeout, "8.8.8.8": timeout}, levelFail, "guests cannot resolve GitHub"},
		{"one of two answers", map[string]cmdResult{cat: {stdout: "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"}}, map[string]error{"8.8.8.8": timeout}, levelWarn, "8.8.8.8 does not answer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeHost(t, c.cmds, nil, c.answers)
			f := byName(resolvChecks(cfg, map[string]string{"vm": "/img"}))["resolv[vm]"]
			if f.Level != c.level || !strings.Contains(f.Detail, c.contain) {
				t.Fatalf("resolv[vm] = %+v, want %s containing %q", f, c.level, c.contain)
			}
		})
	}
	t.Run("no dns category", func(t *testing.T) {
		nodns := &config.Config{Firecracker: provisioner.FirecrackerConfig{Egress: provisioner.EgressConfig{Categories: []string{"actions"}, DNSServers: []string{"1.1.1.1"}}}}
		fakeHost(t, map[string]cmdResult{cat: {stdout: "nameserver 1.1.1.1\n"}}, nil, nil)
		if f := byName(resolvChecks(nodns, map[string]string{"vm": "/img"}))["resolv[vm]"]; f.Level != levelFail || !strings.Contains(f.Detail, "no dns category") {
			t.Fatalf("resolv[vm] = %+v, want FAIL naming the missing dns category", f)
		}
	})
	t.Run("open egress skips the allowlist", func(t *testing.T) {
		open := &config.Config{Firecracker: provisioner.FirecrackerConfig{Egress: provisioner.EgressConfig{Categories: []string{"open"}}}}
		fakeHost(t, map[string]cmdResult{cat: {stdout: "nameserver 192.168.4.1\n"}}, nil, nil)
		if f := byName(resolvChecks(open, map[string]string{"vm": "/img"}))["resolv[vm]"]; f.Level != levelPass {
			t.Fatalf("resolv[vm] = %+v, want PASS with open egress", f)
		}
	})
	t.Run("no debugfs", func(t *testing.T) {
		fakeHost(t, nil, nil, nil)
		if f := byName(resolvChecks(cfg, map[string]string{"vm": "/img"}))["resolv"]; f.Level != levelWarn {
			t.Fatalf("resolv = %+v, want a warning that debugfs is missing", f)
		}
	})
}
