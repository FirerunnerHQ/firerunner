package diag

import (
	"errors"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// fakeHost answers runCmd by "name arg…" and lookupVia by resolver, for the
// test's lifetime.
func fakeHost(t *testing.T, cmds map[string]string, cmdErrs map[string]error, answers map[string]error) {
	t.Helper()
	oldRun, oldLookup := runCmd, lookupVia
	t.Cleanup(func() { runCmd, lookupVia = oldRun, oldLookup })
	runCmd = func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		for k, err := range cmdErrs {
			if strings.HasPrefix(key, k) {
				return "", err
			}
		}
		for k, out := range cmds {
			if strings.HasPrefix(key, k) {
				return out, nil
			}
		}
		return "", errCmdMissing
	}
	lookupVia = func(ns, _ string) error { return answers[ns] }
}

func levels(cs []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}

func TestForwardChecks(t *testing.T) {
	fc := provisioner.FirecrackerConfig{TapPrefix: "fr"}
	dropping := "table ip filter {\n\tchain FORWARD {\n\t\ttype filter hook forward priority filter; policy drop;\n"
	for _, c := range []struct {
		name    string
		cmds    map[string]string
		errs    map[string]error
		check   string
		level   string
		contain string
	}{
		{"no chain", nil, map[string]error{"nft list": errors.New("No such file or directory")}, "forward-policy", levelPass, "no ip filter FORWARD chain"},
		{"accepting", map[string]string{"nft list": "chain FORWARD { policy accept; }"}, nil, "forward-policy", levelPass, "accepts by default"},
		{"dropping with our accepts", map[string]string{"nft list": dropping + "iifname \"fr*\" accept comment \"" + provisioner.ForwardComment("fr") + "\"\n"}, nil, "forward-policy", levelPass, "with firerunner's accepts for fr*"},
		{"dropping without them", map[string]string{"nft list": dropping}, nil, "forward-policy", levelWarn, "no accepts for fr* yet"},
		{"not root", nil, map[string]error{"nft list": errors.New("Error: Operation not permitted")}, "forward-policy", levelWarn, "without root"},
		{"legacy drop", map[string]string{"nft list": "chain FORWARD { policy accept; }", "iptables-legacy -S FORWARD": "-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n"}, nil, "forward-legacy", levelFail, "iptables-legacy -I FORWARD -i fr+ -j ACCEPT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeHost(t, c.cmds, c.errs, nil)
			got := levels(forwardChecks(fc))
			if f, ok := got[c.check]; !ok || f.Level != c.level || !strings.Contains(f.Detail, c.contain) {
				t.Fatalf("%s = %+v, want %s containing %q (all: %+v)", c.check, f, c.level, c.contain, got)
			}
		})
	}
	t.Run("legacy accept", func(t *testing.T) {
		fakeHost(t, map[string]string{"nft list": "policy accept;", "iptables-legacy -S FORWARD": "-P FORWARD ACCEPT\n"}, nil, nil)
		if _, ok := levels(forwardChecks(fc))["forward-legacy"]; ok {
			t.Fatal("an accepting legacy FORWARD chain reported")
		}
	})
}

func TestResolvChecks(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{Egress: provisioner.EgressConfig{Categories: []string{"actions", "dns"}, DNSServers: []string{"1.1.1.1", "8.8.8.8"}}}}
	timeout := errors.New("i/o timeout")
	for _, c := range []struct {
		name, conf string
		answers    map[string]error
		level      string
		contain    string
	}{
		{"answers", "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", nil, levelPass, "nameservers 1.1.1.1, 8.8.8.8 answer"},
		{"empty", "", nil, levelFail, "has no nameserver"},
		{"outside the allowlist", "nameserver 192.168.4.1\n", nil, levelFail, "192.168.4.1 is not in --dns-servers 1.1.1.1,8.8.8.8"},
		{"blocked upstream (#3)", "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", map[string]error{"1.1.1.1": timeout, "8.8.8.8": timeout}, levelFail, "guests cannot resolve GitHub"},
		{"one of two answers", "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", map[string]error{"8.8.8.8": timeout}, levelWarn, "8.8.8.8 does not answer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeHost(t, map[string]string{"debugfs -R cat /etc/resolv.conf": c.conf}, nil, c.answers)
			f := levels(resolvChecks(cfg, map[string]string{"vm": "/img/golden.ext4"}))["resolv[vm]"]
			if f.Level != c.level || !strings.Contains(f.Detail, c.contain) {
				t.Fatalf("resolv[vm] = %+v, want %s containing %q", f, c.level, c.contain)
			}
		})
	}
	t.Run("open egress skips the allowlist", func(t *testing.T) {
		open := &config.Config{Firecracker: provisioner.FirecrackerConfig{Egress: provisioner.EgressConfig{Categories: []string{"open"}}}}
		fakeHost(t, map[string]string{"debugfs -R cat /etc/resolv.conf": "nameserver 192.168.4.1\n"}, nil, nil)
		if f := levels(resolvChecks(open, map[string]string{"vm": "/img"}))["resolv[vm]"]; f.Level != levelPass {
			t.Fatalf("resolv[vm] = %+v, want PASS with open egress", f)
		}
	})
	t.Run("no debugfs", func(t *testing.T) {
		fakeHost(t, nil, nil, nil)
		if f := levels(resolvChecks(cfg, map[string]string{"vm": "/img"}))["resolv"]; f.Level != levelWarn {
			t.Fatalf("resolv = %+v, want a warning that debugfs is missing", f)
		}
	})
}
