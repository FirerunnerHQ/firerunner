package diag

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// runCmd runs a read-only host command and returns its stdout; tests replace
// it.
var runCmd = func(name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", errCmdMissing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		err = errors.New(strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}

var errCmdMissing = errors.New("not installed")

// lookupVia asks the resolver at ns:53 for name, as a guest would; tests
// replace it.
var lookupVia = func(ns, name string) error {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", net.JoinHostPort(ns, "53"))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := r.LookupHost(ctx, name)
	return err
}

// forwardChecks look for a forward policy that drops guest egress and that
// firerunner cannot fix itself. Docker and ufw set the ip filter FORWARD
// policy to drop; in nftables the daemon inserts accepts for its taps when it
// applies its network, but a legacy-iptables DROP (Docker on iptables-legacy)
// sits outside nftables, and its verdict still drops every guest packet.
func forwardChecks(fc provisioner.FirecrackerConfig) []Check {
	tap := fc.TapPrefix + "*"
	var out []Check
	nft, err := runCmd("nft", "list", "chain", "ip", "filter", "FORWARD")
	switch {
	case errors.Is(err, errCmdMissing):
		// The nftables check already says nft is missing.
	case err != nil && strings.Contains(err.Error(), "Operation not permitted"):
		out = append(out, warn("forward-policy", "cannot read nftables without root; run doctor as root to check the FORWARD policy"))
	case err != nil:
		out = append(out, pass("forward-policy", "no ip filter FORWARD chain: nothing drops forwarded guest traffic"))
	case !strings.Contains(nft, "policy drop"):
		out = append(out, pass("forward-policy", "ip filter FORWARD accepts by default"))
	case strings.Contains(nft, provisioner.ForwardComment(fc.TapPrefix)):
		out = append(out, pass("forward-policy", "ip filter FORWARD drops by default (Docker or ufw), with firerunner's accepts for %s", tap))
	default:
		out = append(out, warn("forward-policy", "ip filter FORWARD drops by default (Docker or ufw) and has no accepts for %s yet: the daemon inserts them when it applies its network; if microVMs never register, check they are there", tap))
	}
	legacy, err := runCmd("iptables-legacy", "-S", "FORWARD")
	if err == nil && strings.Contains(legacy, "-P FORWARD DROP") {
		out = append(out, fail("forward-legacy", "legacy iptables FORWARD policy is DROP (Docker on iptables-legacy?), which firerunner's nftables rules cannot override: guests get no egress. Accept the taps there (iptables-legacy -I FORWARD -i %s -j ACCEPT; iptables-legacy -I FORWARD -o %s -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT) or run Docker on iptables-nft", fc.TapPrefix+"+", fc.TapPrefix+"+"))
	}
	return out
}

// resolvChecks read each golden image's /etc/resolv.conf, which guests use as
// baked (see #3): it must name a resolver, one the egress allowlist lets
// guests reach, and one that answers from this host's network.
func resolvChecks(cfg *config.Config, goldens map[string]string) []Check {
	eg := cfg.Firecracker.Egress
	open := eg.Has("open")
	var out []Check
	names := make([]string, 0, len(goldens))
	for name := range goldens {
		names = append(names, name)
	}
	slices.Sort(names)
	asked := map[string]error{}
	for _, name := range names {
		img, check := goldens[name], "resolv["+name+"]"
		conf, err := runCmd("debugfs", "-R", "cat /etc/resolv.conf", img)
		if errors.Is(err, errCmdMissing) {
			return append(out, warn("resolv", "debugfs (e2fsprogs) not installed: cannot read the golden images' /etc/resolv.conf"))
		}
		if err != nil {
			out = append(out, warn(check, "cannot read /etc/resolv.conf from %s: %v", img, err))
			continue
		}
		var servers []string
		for _, line := range strings.Split(conf, "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
				servers = append(servers, f[1])
			}
		}
		if len(servers) == 0 {
			out = append(out, fail(check, "%s has no nameserver in /etc/resolv.conf (empty, missing, or a symlink debugfs does not follow): guests cannot resolve GitHub; rebuild it (images/build-rootfs.sh bakes --dns-servers in)", img))
			continue
		}
		var problems []string
		for _, ns := range servers {
			if !open && (!eg.Has("dns") || !slices.Contains(eg.DNSServers, ns)) {
				problems = append(problems, ns+" is not in --dns-servers "+strings.Join(eg.DNSServers, ",")+" (the egress allowlist drops it)")
				continue
			}
			err, done := asked[ns]
			if !done {
				err = lookupVia(ns, "github.com")
				asked[ns] = err
			}
			if err != nil {
				problems = append(problems, ns+" does not answer from this host's network ("+err.Error()+")")
			}
		}
		switch {
		case len(problems) == len(servers):
			out = append(out, fail(check, "guests cannot resolve GitHub: %s; rebuild the image with --dns-servers this network allows (#3)", strings.Join(problems, "; ")))
		case len(problems) > 0:
			out = append(out, warn(check, "nameservers %s; %s", strings.Join(servers, ", "), strings.Join(problems, "; ")))
		default:
			out = append(out, pass(check, "nameservers %s answer for github.com", strings.Join(servers, ", ")))
		}
	}
	return out
}
