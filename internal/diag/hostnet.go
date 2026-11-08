package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// runCmd runs a read-only host command and returns its stdout and stderr;
// a failed command's error is its stderr. Tests replace it.
var runCmd = func(name string, args ...string) (stdout, stderr string, err error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", "", errCmdMissing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, errOut strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if err != nil && errOut.Len() > 0 {
		err = errors.New(strings.TrimSpace(errOut.String()))
	}
	return out.String(), errOut.String(), err
}

// readFile is os.ReadFile for paths under /proc; tests replace it.
var readProc = os.ReadFile

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

// firewallDrops reads the packet counts of the guest firewall's drop counters:
// guests sending as another address (spoof), to a non-public address
// (nonpublic), past their tracked-connection limit (conn_limit) or their
// new-connection rate (conn_rate). A job that hits a limit sees only
// connect timeouts, so these say why. A counter of a table that does not
// exist (the limits are off, or the daemon has not applied its network) is
// left out. Reading needs root.
func firewallDrops(table string) (map[string]uint64, error) {
	names := map[string]string{
		"guest_spoof_drop": "spoof", "nonpublic_drop": "nonpublic",
		"guest_conn_drop": "conn_limit", "guest_rate_drop": "conn_rate",
	}
	drops := map[string]uint64{}
	for _, t := range []string{table, table + "_conns"} {
		out, _, err := runCmd("nft", "-j", "list", "counters", "table", "ip", t)
		if err != nil {
			if strings.Contains(err.Error(), "No such file or directory") {
				continue
			}
			return nil, err
		}
		var doc struct {
			NFTables []struct {
				Counter *struct {
					Name    string `json:"name"`
					Packets uint64 `json:"packets"`
				} `json:"counter"`
			} `json:"nftables"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			return nil, fmt.Errorf("parse nft counters of %s: %w", t, err)
		}
		for _, e := range doc.NFTables {
			if e.Counter != nil && names[e.Counter.Name] != "" {
				drops[names[e.Counter.Name]] = e.Counter.Packets
			}
		}
	}
	return drops, nil
}

// forwardChecks look for a forward policy that drops guest egress. Docker and
// ufw set ip filter FORWARD's policy to drop, and the daemon inserts accepts
// for its taps there when it applies its network. Any other forward base
// chain that drops (Arch's stock inet filter, firewalld) drops guest packets
// too, and the daemon leaves it alone; so does a legacy-iptables DROP (Docker
// on iptables-legacy), which sits outside nftables.
func forwardChecks(fc provisioner.FirecrackerConfig) []Check {
	var out []Check
	listing, _, err := runCmd("nft", "-j", "list", "chains")
	switch {
	case errors.Is(err, errCmdMissing):
		// The nftables check already says nft is missing.
	case err != nil && strings.Contains(err.Error(), "Operation not permitted"):
		out = append(out, warn("forward-policy", "cannot read nftables without root; run doctor as root to check the forward policies"))
	case err != nil:
		out = append(out, warn("forward-policy", "cannot list nftables chains: %v", err))
	default:
		out = append(out, nftForwardCheck(fc, listing))
	}
	return append(out, legacyForwardChecks(fc)...)
}

// nftForwardCheck judges every IPv4-reaching forward base chain (ip and
// inet; guests have no IPv6) whose policy drops.
func nftForwardCheck(fc provisioner.FirecrackerConfig, listing string) Check {
	var doc struct {
		Nftables []struct {
			Chain *struct {
				Family, Table, Name, Hook, Policy string
			} `json:"chain"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(listing), &doc); err != nil {
		return warn("forward-policy", "cannot parse nft -j list chains: %v", err)
	}
	tap := fc.TapPrefix + "*"
	var ours, foreign []string
	for _, e := range doc.Nftables {
		c := e.Chain
		if c == nil || c.Hook != "forward" || c.Policy != "drop" || (c.Family != "ip" && c.Family != "inet") {
			continue
		}
		where := c.Family + " " + c.Table + " " + c.Name
		rules, _, err := runCmd("nft", "list", "chain", c.Family, c.Table, c.Name)
		switch {
		case err != nil:
			foreign = append(foreign, where+" (unreadable: "+err.Error()+")")
		case c.Family == "ip" && c.Table == "filter" && c.Name == "FORWARD":
			if !strings.Contains(rules, `comment "`+provisioner.ForwardComment(fc.TapPrefix)+`"`) {
				return warn("forward-policy", "ip filter FORWARD drops by default (Docker or ufw) and has no accepts for %s yet: the daemon inserts them when it applies its network; if microVMs never register, check they are there", tap)
			}
			ours = append(ours, where)
		case strings.Contains(rules, `"`+tap+`"`):
			ours = append(ours, where+" (has rules for "+tap+"; check they accept)")
		default:
			foreign = append(foreign, where)
		}
	}
	switch {
	case len(foreign) > 0:
		return fail("forward-policy", "%s drop forwarded traffic by default with no rule for %s, which the daemon does not touch: guests get no egress. Accept iifname %q and oifname %q established traffic there, or stop the firewall that owns it (e.g. Arch's nftables.service)", strings.Join(foreign, ", "), tap, tap, tap)
	case len(ours) > 0:
		return pass("forward-policy", "forward chains drop by default, with accepts for %s: %s", tap, strings.Join(ours, ", "))
	}
	return pass("forward-policy", "no forward chain drops by default")
}

// legacyForwardChecks reads the legacy iptables FORWARD policy, but only when
// legacy iptables is in use: listing it otherwise loads its modules and
// creates an empty table, a change a check must not make.
func legacyForwardChecks(fc provisioner.FirecrackerConfig) []Check {
	names, err := readProc("/proc/net/ip_tables_names")
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil // no legacy iptables at all
	case err != nil:
		return []Check{warn("forward-legacy", "cannot read /proc/net/ip_tables_names without root; run doctor as root to check legacy iptables")}
	case !slices.Contains(strings.Fields(string(names)), "filter"):
		return nil // legacy iptables holds no filter table
	}
	rules, _, err := runCmd("iptables-legacy", "-S", "FORWARD")
	switch {
	case errors.Is(err, errCmdMissing):
		return []Check{warn("forward-legacy", "a legacy iptables filter table exists but iptables-legacy is not installed to read it")}
	case err != nil:
		return []Check{warn("forward-legacy", "cannot read the legacy iptables FORWARD chain: %v", err)}
	case !strings.Contains(rules, "-P FORWARD DROP"):
		return nil
	}
	in := "-A FORWARD -i " + fc.TapPrefix + "+ -j ACCEPT"
	back := false
	for _, line := range strings.Split(rules, "\n") {
		back = back || strings.HasPrefix(line, "-A FORWARD -o "+fc.TapPrefix+"+ ") && strings.HasSuffix(strings.TrimSpace(line), "-j ACCEPT")
	}
	if strings.Contains(rules, in) && back {
		return []Check{pass("forward-legacy", "legacy iptables FORWARD drops by default, with accepts for %s", fc.TapPrefix+"+")}
	}
	return []Check{fail("forward-legacy", "legacy iptables FORWARD policy is DROP (Docker on iptables-legacy?), which firerunner's nftables rules cannot override: guests get no egress. Accept the taps there (iptables-legacy -I FORWARD -i %[1]s -j ACCEPT; iptables-legacy -I FORWARD -o %[1]s -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT) or run Docker on iptables-nft", fc.TapPrefix+"+")}
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
		conf, stderr, err := runCmd("debugfs", "-R", "cat /etc/resolv.conf", img)
		if errors.Is(err, errCmdMissing) {
			return append(out, warn("resolv", "debugfs (e2fsprogs) not installed: cannot read the golden images' /etc/resolv.conf"))
		}
		// debugfs exits 0 whatever happens; what went wrong is on stderr.
		if strings.TrimSpace(conf) == "" {
			if c, ok := resolvReadFailure(check, img, stderr, err); ok {
				out = append(out, c)
				continue
			}
		}
		var servers []string
		for _, line := range strings.Split(conf, "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
				servers = append(servers, f[1])
			}
		}
		if len(servers) == 0 {
			out = append(out, fail(check, "%s has no nameserver in /etc/resolv.conf: guests cannot resolve GitHub; rebuild it (images/build-rootfs.sh bakes --dns-servers in)", img))
			continue
		}
		var problems []string
		for _, ns := range servers {
			switch {
			case open:
			case !eg.Has("dns"):
				problems = append(problems, ns+" is unreachable: --egress has no dns category, so guests reach no resolver")
				continue
			case !slices.Contains(eg.DNSServers, ns):
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

// resolvReadFailure explains an empty read of a golden's /etc/resolv.conf
// from debugfs's stderr: the image would not open (a warning: say why), the
// file is missing (a failure), or it is a symlink, which debugfs does not
// follow and the guest resolves at boot (a warning: check by hand). ok is
// false for a file that is there and empty.
func resolvReadFailure(check, img, stderr string, err error) (Check, bool) {
	line := func(sub string) string {
		for _, l := range strings.Split(stderr, "\n") {
			if strings.Contains(l, sub) {
				return strings.TrimSpace(l)
			}
		}
		return ""
	}
	switch {
	case err != nil:
		return warn(check, "cannot read /etc/resolv.conf from %s: %v", img, err), true
	case line("while trying to open") != "":
		return warn(check, "debugfs cannot open %s: %s", img, line("while trying to open")), true
	case line("File not found") != "":
		return fail(check, "%s has no /etc/resolv.conf: guests cannot resolve GitHub; rebuild it (images/build-rootfs.sh bakes --dns-servers in)", img), true
	}
	if st, _, _ := runCmd("debugfs", "-R", "stat /etc/resolv.conf", img); strings.Contains(st, "Type: symlink") {
		dest := ""
		for _, l := range strings.Split(st, "\n") {
			if _, d, ok := strings.Cut(l, "link dest:"); ok {
				dest = strings.TrimSpace(d)
			}
		}
		return warn(check, "%s: /etc/resolv.conf is a symlink (to %s), resolved only when the guest boots; check that it names a resolver --dns-servers allows", img, or(dest, "?")), true
	}
	return Check{}, false
}
