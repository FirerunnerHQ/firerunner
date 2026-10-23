// Package diag implements firerunner's operator-facing "status" and "doctor"
// subcommands. Both work purely by reading host state (files, /dev/kvm, sysctls,
// network interfaces, the work dir) so they can be run standalone on a runner
// host, alongside a live daemon, without any IPC or extra dependencies.
package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/solcreek/firerunner/internal/cacheserver"
	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/health"
	"github.com/solcreek/firerunner/internal/provisioner"
)

// StatusReport is the structured snapshot rendered by "status" (as text or JSON).
type StatusReport struct {
	Version   string      `json:"version"`
	ScaleSet  string      `json:"scale_set"`
	Group     string      `json:"group"`
	URL       string      `json:"url"`
	Labels    []string    `json:"labels"`
	Warm      int         `json:"warm"`
	Max       int         `json:"max"`
	VCPU      int         `json:"vcpu"`
	MemMiB    int         `json:"mem_mib"`
	Kernel    FileStat    `json:"kernel"`
	Golden    FileStat    `json:"golden"`
	ToolCache *FileStat   `json:"toolcache,omitempty"`
	Isolation string      `json:"isolation"`
	Network   NetInfo     `json:"network"`
	Cache     *CacheInfo  `json:"cache,omitempty"`
	Workdir   WorkdirInfo `json:"workdir"`
	Tiers     []TierInfo  `json:"tiers,omitempty"`
	// Listeners is the running daemon's per-tier listener health, absent
	// when the daemon has not published any.
	Listeners *ListenerInfo `json:"listeners,omitempty"`
	MicroVMs  VMSummary     `json:"microvms"`
}

// ListenerInfo is the daemon's published listener health.
type ListenerInfo struct {
	health.File
	// Stale is set when the file has not been rewritten recently: the
	// daemon is likely not running, so the states may be out of date.
	Stale bool `json:"stale"`
}

// listenerStaleAfter is how old the health file may get before status calls
// it stale; the daemon rewrites it every 10 s.
const listenerStaleAfter = 30 * time.Second

// readListeners loads the daemon's listener health from its work dir.
func readListeners(fc provisioner.FirecrackerConfig, now time.Time) (*ListenerInfo, error) {
	f, err := health.Read(filepath.Join(fc.WorkDir, health.FileName))
	if err != nil {
		return nil, err
	}
	return &ListenerInfo{File: f, Stale: now.Sub(f.WrittenAt) > listenerStaleAfter}, nil
}

// TierInfo is one configured runner tier as seen by status.
type TierInfo struct {
	Name        string   `json:"name"`
	Labels      []string `json:"labels,omitempty"`
	Provisioner string   `json:"provisioner"`
	// VCPU, MemMiB, Golden and ToolCache describe a firecracker tier.
	VCPU      int       `json:"vcpu,omitempty"`
	MemMiB    int       `json:"mem_mib,omitempty"`
	Golden    *FileStat `json:"golden,omitempty"`
	ToolCache *FileStat `json:"toolcache,omitempty"`
	// InstanceTypes and Market describe an ec2 tier.
	InstanceTypes []string `json:"instance_types,omitempty"`
	Market        string   `json:"market,omitempty"`
	Min           int      `json:"min"`
	Max           int      `json:"max"`
}

// FileStat describes an image file on disk.
type FileStat struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Bytes   int64  `json:"bytes,omitempty"`
	Human   string `json:"human,omitempty"`
	ModTime string `json:"mtime,omitempty"`
}

// describe renders a FileStat the way the text status expects.
func (f FileStat) describe() string {
	if f.Path == "" {
		return "(unset)"
	}
	if !f.Exists {
		return fmt.Sprintf("%s  MISSING", f.Path)
	}
	return fmt.Sprintf("%s  (%s, %s)", f.Path, f.Human, f.ModTime)
}

// CacheInfo describes the self-hosted dependency cache firerunner points
// microVMs at, when configured. It is nil when caching is off (the default),
// in which case jobs use GitHub's hosted cache.
type CacheInfo struct {
	// Mode is "gateway" (guest builds the URL from its host gateway and Port) or
	// "url" (an explicit CacheURL is used verbatim).
	Mode string `json:"mode"`
	// Port is the local cache-server port published to guests (gateway mode).
	Port int `json:"port,omitempty"`
	// URL is the explicit cache-server base URL (url mode).
	URL string `json:"url,omitempty"`
	// Stats is a live snapshot from the cache-server's /stats endpoint, when it
	// is reachable; nil otherwise (the cache is off, or the server is down).
	Stats *cacheserver.Stats `json:"stats,omitempty"`
}

// NetInfo captures the host network wiring for guest egress.
type NetInfo struct {
	ExtIface   string `json:"ext_iface"`
	ExtIfaceUp bool   `json:"ext_iface_up"`
	Egress     string `json:"egress"`
	Subnet     string `json:"subnet"`
	TapPrefix  string `json:"tap_prefix"`
	NFTTable   string `json:"nft_table"`
}

// WorkdirInfo describes the per-job rootfs work directory.
type WorkdirInfo struct {
	Path           string `json:"path"`
	FS             string `json:"fs"`
	ReflinkCapable bool   `json:"reflink_capable"`
}

// VMSummary aggregates the live microVMs on the host.
type VMSummary struct {
	Active           int  `json:"active"`
	FirecrackerProcs int  `json:"firecracker_procs"`
	Taps             int  `json:"taps"`
	Max              int  `json:"max"`
	VMs              []VM `json:"vms,omitempty"`
}

// VM is a single live microVM.
type VM struct {
	Name        string `json:"name"`
	RootfsBytes int64  `json:"rootfs_bytes"`
	RootfsHuman string `json:"rootfs_human"`
	AgeSeconds  int64  `json:"age_seconds"`
}

// DoctorReport is the structured result of the preflight health checks.
type DoctorReport struct {
	Version string  `json:"version"`
	Checks  []Check `json:"checks"`
	Total   int     `json:"total"`
	Failed  int     `json:"failed"`
	OK      bool    `json:"ok"`
}

// writeJSON marshals v with indentation and a trailing newline.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// Status writes a snapshot of the deployment: resolved config, golden/kernel
// images, network wiring and the microVMs currently on the host. When asJSON is
// set it emits a StatusReport as JSON (for dashboards/scripts); otherwise a
// human-readable table.
func Status(cfg *config.Config, version string, w io.Writer, asJSON bool) error {
	r := collectStatus(cfg, version)
	if asJSON {
		return writeJSON(w, r)
	}
	renderStatusText(r, w)
	return nil
}

// collectStatus gathers the deployment snapshot into a StatusReport, the single
// source of truth for both the JSON and text renderings.
func collectStatus(cfg *config.Config, version string) StatusReport {
	fc := cfg.Firecracker
	mode := "direct"
	if fc.Jailer {
		mode = "jailer"
		if fc.NetNS {
			mode = "jailer+netns"
		}
	}
	r := StatusReport{
		Version:   version,
		ScaleSet:  cfg.ScaleSetName,
		Group:     cfg.RunnerGroup,
		URL:       cfg.URL,
		Labels:    nonEmpty(append([]string{cfg.ScaleSetName}, cfg.Labels...)),
		Warm:      cfg.MinRunners,
		Max:       cfg.MaxRunners,
		VCPU:      cfg.VCPU,
		MemMiB:    cfg.MemMiB,
		Kernel:    statFile(fc.KernelImage),
		Golden:    statFile(fc.GoldenRootFS),
		Isolation: mode,
		Network: NetInfo{
			ExtIface:   fc.ExtIface,
			ExtIfaceUp: ifaceUp(fc.ExtIface),
			Egress:     egressDesc(fc.Egress.Categories),
			Subnet:     fmt.Sprintf("172.%d.0.0/16", fc.NetBase),
			TapPrefix:  fc.TapPrefix,
			NFTTable:   fc.NFTTable,
		},
		Workdir: WorkdirInfo{
			Path:           fc.WorkDir,
			FS:             fsKind(fc.WorkDir),
			ReflinkCapable: reflinkProbe(fc.WorkDir),
		},
	}
	if fc.ToolCacheImage != "" {
		f := statFile(fc.ToolCacheImage)
		r.ToolCache = &f
	}
	switch {
	case fc.CacheURL != "":
		r.Cache = &CacheInfo{Mode: "url", URL: fc.CacheURL}
	case fc.CachePort != 0:
		r.Cache = &CacheInfo{Mode: "gateway", Port: fc.CachePort}
	}
	if r.Cache != nil {
		r.Cache.Stats = fetchCacheStats(fc)
	}

	// Tier catalog, when one is configured. Each tier is its own scale set with
	// its own microVM shape and golden image, all sharing this host.
	for _, t := range cfg.Tiers {
		ti := TierInfo{
			Name:        t.Name,
			Labels:      t.Labels,
			Provisioner: t.ProvisionerName(),
			Min:         t.Min,
			Max:         t.Max,
		}
		if t.IsEC2() {
			if t.EC2 != nil {
				ti.InstanceTypes = t.EC2.InstanceTypes
				ti.Market = t.EC2.Market
			}
			r.Tiers = append(r.Tiers, ti)
			continue
		}
		ti.VCPU, ti.MemMiB = t.VCPU, t.MemMiB
		g := statFile(t.Golden)
		ti.Golden = &g
		if t.ToolCache != "" {
			f := statFile(t.ToolCache)
			ti.ToolCache = &f
		}
		r.Tiers = append(r.Tiers, ti)
	}

	if li, err := readListeners(fc, time.Now()); err == nil {
		r.Listeners = li
	}

	// Live microVMs, cross-checked three ways: work dirs (per-job rootfs clone +
	// socket), tap devices and firecracker processes.
	vms := activeVMs(fc)
	r.MicroVMs = VMSummary{
		Active:           len(vms),
		FirecrackerProcs: firecrackerProcs(fc),
		Taps:             countTaps(fc.TapPrefix),
		Max:              cfg.MaxRunners,
	}
	for _, v := range vms {
		r.MicroVMs.VMs = append(r.MicroVMs.VMs, VM{
			Name:        v.name,
			RootfsBytes: v.rootfsBytes,
			RootfsHuman: humanBytes(v.rootfsBytes),
			AgeSeconds:  int64(time.Since(v.started).Seconds()),
		})
	}
	return r
}

func renderStatusText(r StatusReport, w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "firerunner\t%s\n", r.Version)
	fmt.Fprintf(tw, "scale set\t%s  (group %s)\n", or(r.ScaleSet, "-"), or(r.Group, "-"))
	fmt.Fprintf(tw, "url\t%s\n", or(r.URL, "(unset)"))
	fmt.Fprintf(tw, "runs-on labels\t%s\n", strings.Join(r.Labels, ", "))
	fmt.Fprintf(tw, "capacity\twarm=%d  max=%d  vcpu=%d  mem=%dMiB\n", r.Warm, r.Max, r.VCPU, r.MemMiB)
	fmt.Fprintf(tw, "kernel\t%s\n", r.Kernel.describe())
	fmt.Fprintf(tw, "golden\t%s\n", r.Golden.describe())
	if r.ToolCache != nil {
		fmt.Fprintf(tw, "toolcache\t%s\n", r.ToolCache.describe())
	} else {
		fmt.Fprintf(tw, "toolcache\t(none; jobs download tools on demand)\n")
	}
	fmt.Fprintf(tw, "isolation\t%s\n", r.Isolation)
	fmt.Fprintf(tw, "network\text-iface=%s  egress=%s  subnet=%s  tap=%s*  nft-table=%s\n",
		ifaceStateStr(r.Network.ExtIface, r.Network.ExtIfaceUp), r.Network.Egress, r.Network.Subnet, r.Network.TapPrefix, r.Network.NFTTable)
	if r.Cache != nil {
		if r.Cache.Mode == "url" {
			fmt.Fprintf(tw, "dep cache\tself-hosted %s\n", r.Cache.URL)
		} else {
			fmt.Fprintf(tw, "dep cache\tself-hosted (guest gateway:%d)\n", r.Cache.Port)
		}
		if st := r.Cache.Stats; st != nil {
			size := humanBytes(st.Bytes)
			if st.MaxBytes > 0 {
				size += " / " + humanBytes(st.MaxBytes)
			}
			fmt.Fprintf(tw, "\t%d entries, %s, %s hit rate (%d hit / %d miss), %d evicted\n",
				st.Entries, size, hitRate(st.Hits, st.Misses), st.Hits, st.Misses, st.Evictions)
			if st.ArtifactRPCs > 0 || st.ArtifactErrors > 0 {
				fmt.Fprintf(tw, "\t%d artifact rpcs forwarded upstream, %d errors\n", st.ArtifactRPCs, st.ArtifactErrors)
			}
		}
	} else {
		fmt.Fprintf(tw, "dep cache\t(none; jobs use GitHub's hosted cache)\n")
	}
	fmt.Fprintf(tw, "workdir\t%s (%s)\n", r.Workdir.Path, r.Workdir.FS)
	tw.Flush()

	if len(r.Tiers) > 0 {
		fmt.Fprintf(w, "\ntiers: %d  (developers select one via runs-on: <name>; shared slot budget max=%d)\n", len(r.Tiers), r.Max)
		ttw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(ttw, "  RUNS-ON\tPROVISIONER\tVCPU\tMEM\tWARM\tMAX\tIMAGE\tTOOLCACHE")
		for _, t := range r.Tiers {
			if t.Golden == nil {
				// An ec2 tier: shaped by its instance types, not a microVM.
				fmt.Fprintf(ttw, "  %s\t%s\t-\t-\t%d\t%d\t%s (%s)\t-\n",
					t.Name, t.Provisioner, t.Min, t.Max, strings.Join(t.InstanceTypes, ","), t.Market)
				continue
			}
			tc := "(global default)"
			if t.ToolCache != nil {
				tc = t.ToolCache.describe()
			}
			fmt.Fprintf(ttw, "  %s\t%s\t%d\t%dMiB\t%d\t%d\t%s\t%s\n",
				t.Name, t.Provisioner, t.VCPU, t.MemMiB, t.Min, t.Max, t.Golden.describe(), tc)
		}
		ttw.Flush()
	}

	if l := r.Listeners; l != nil {
		age := humanDuration(time.Since(l.WrittenAt).Truncate(time.Second))
		note := ""
		if l.Stale {
			note = "  STALE: the daemon may not be running"
		}
		fmt.Fprintf(w, "\nlisteners: (pid %d, updated %s ago)%s\n", l.PID, age, note)
		ltw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(ltw, "  TIER\tSTATE\tRECONNECTS\tLAST MESSAGE\tLAST ERROR")
		for _, t := range l.Tiers {
			last := "never"
			if !t.LastMessage.IsZero() {
				last = humanDuration(time.Since(t.LastMessage).Truncate(time.Second)) + " ago"
			}
			state := string(t.State)
			if t.State == health.Reconnecting && t.BackoffSeconds > 0 {
				state += fmt.Sprintf(" (next in %.0fs)", t.BackoffSeconds)
			}
			fmt.Fprintf(ltw, "  %s\t%s\t%d\t%s\t%s\n", t.Name, state, t.Reconnects, last, or(truncate(t.LastError, 80), "-"))
		}
		ltw.Flush()
		for _, t := range l.Tiers {
			if g := t.GitHub; g != nil {
				fmt.Fprintf(w, "  %s at GitHub: available %d, acquired %d, assigned %d, running %d, registered %d (busy %d, idle %d); firerunner tracks %d\n",
					t.Name, g.Available, g.Acquired, g.Assigned, g.Running, g.Registered, g.Busy, g.Idle, g.Tracked)
			}
			if t.Drift != "" {
				fmt.Fprintf(w, "  %s DRIFT: %s\n", t.Name, t.Drift)
			}
			if sz := t.Sizes; sz != nil {
				window := "base sizes (outside every window)"
				if sz.Window != "" {
					window = "window " + sz.Window
				}
				fmt.Fprintf(w, "  %s schedule: %s: %d hot runner(s), pool %d, since %s\n",
					t.Name, window, sz.Min, sz.Pool, sz.Since.UTC().Format(time.RFC3339))
			}
			if t.Recycles > 0 || t.LostRunners > 0 {
				fmt.Fprintf(w, "  %s repairs: %d session recycle(s), %d lost runner(s) cancelled\n", t.Name, t.Recycles, t.LostRunners)
			}
		}
		if rp := l.Reaper; rp != nil {
			mode := ""
			if rp.DryRun {
				mode = " (dry run: nothing changed)"
			}
			fmt.Fprintf(w, "\nEC2 reaper: last sweep %s ago%s: %d instance(s) terminated, %d spot request(s) cancelled, %d left running a job, %d too young to judge\n",
				humanDuration(time.Since(rp.At).Truncate(time.Second)), mode, len(rp.Instances), len(rp.SpotRequests), len(rp.Busy), len(rp.Young))
			if rp.Error != "" {
				fmt.Fprintf(w, "  error: %s\n", truncate(rp.Error, 200))
			}
		}
	}

	fmt.Fprintf(w, "\nmicroVMs: %d active  (firecracker procs=%d, taps=%d, max=%d)\n",
		r.MicroVMs.Active, r.MicroVMs.FirecrackerProcs, r.MicroVMs.Taps, r.MicroVMs.Max)
	if len(r.MicroVMs.VMs) > 0 {
		vtw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(vtw, "  RUNNER\tROOTFS\tAGE")
		for _, v := range r.MicroVMs.VMs {
			fmt.Fprintf(vtw, "  %s\t%s\t%s\n", v.Name, v.RootfsHuman, humanDuration(time.Duration(v.AgeSeconds)*time.Second))
		}
		vtw.Flush()
	}
}

// Doctor runs preflight health checks. When asJSON is set it emits a
// DoctorReport as JSON; otherwise one PASS/WARN/FAIL line each. It returns a
// non-nil error if any check FAILs, so the command exits non-zero either way.
func Doctor(cfg *config.Config, version string, w io.Writer, asJSON bool) error {
	r := runDoctor(cfg, version)
	if asJSON {
		if err := writeJSON(w, r); err != nil {
			return err
		}
	} else {
		for _, c := range r.Checks {
			fmt.Fprintf(w, "%s %-12s %s\n", c.Level, c.Name, c.Detail)
		}
		fmt.Fprintf(w, "\n%d checks, %d failed\n", r.Total, r.Failed)
	}
	if r.Failed > 0 {
		return fmt.Errorf("%d check(s) failed", r.Failed)
	}
	return nil
}

// runDoctor executes the preflight checks and folds them into a DoctorReport.
func runDoctor(cfg *config.Config, version string) DoctorReport {
	fc := cfg.Firecracker
	var checks []Check

	// Privileges. firerunner's default path runs unprivileged, but jailer/netns
	// need root (creating a netns/chroot needs CAP_SYS_ADMIN).
	euid := os.Geteuid()
	switch {
	case fc.Jailer && euid != 0:
		checks = append(checks, fail("privileges", "--jailer needs root; running as uid %d", euid))
	case euid == 0:
		checks = append(checks, pass("privileges", "root"))
	default:
		checks = append(checks, pass("privileges", "uid %d (non-root; ok for direct mode)", euid))
	}

	// KVM: the hard requirement. No /dev/kvm, no microVMs.
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		checks = append(checks, fail("kvm", "/dev/kvm not usable: %v", err))
	} else {
		f.Close()
		checks = append(checks, pass("kvm", "/dev/kvm read-write"))
	}

	checks = append(checks, binVersion("firecracker", fc.Binary))
	if fc.Jailer {
		checks = append(checks, binVersion("jailer", fc.JailerBin))
	}

	checks = append(checks, fileCheck("kernel", fc.KernelImage, 1<<20)) // >=1MiB
	var goldens []string
	if len(cfg.Tiers) > 0 {
		// Tier-catalog mode: each tier has its own golden (and optional
		// toolcache); the top-level --golden may be unset, so check per tier
		// instead of failing on an empty top-level path.
		for _, t := range cfg.Tiers {
			if t.IsEC2() {
				// An ec2 tier boots no image from this host.
				continue
			}
			checks = append(checks, fileCheck("golden["+t.Name+"]", t.Golden, 64<<20))
			goldens = append(goldens, t.Golden)
			if t.ToolCache != "" {
				checks = append(checks, fileCheck("toolcache["+t.Name+"]", t.ToolCache, 1<<20))
			}
		}
	} else {
		checks = append(checks, fileCheck("golden", fc.GoldenRootFS, 64<<20)) // >=64MiB
		goldens = append(goldens, fc.GoldenRootFS)
		if fc.ToolCacheImage != "" {
			checks = append(checks, fileCheck("toolcache", fc.ToolCacheImage, 1<<20))
		}
	}

	// External interface for egress NAT.
	if fc.ExtIface == "" {
		checks = append(checks, fail("ext-iface", "--ext-iface is unset; microVMs cannot reach GitHub"))
	} else if _, err := net.InterfaceByName(fc.ExtIface); err != nil {
		checks = append(checks, fail("ext-iface", "%q not found: %v", fc.ExtIface, err))
	} else {
		checks = append(checks, pass("ext-iface", "%s up", fc.ExtIface))
	}

	// IP forwarding: the host routes guest egress through ext-iface.
	if v := strings.TrimSpace(readFile("/proc/sys/net/ipv4/ip_forward")); v == "1" {
		checks = append(checks, pass("ip_forward", "enabled"))
	} else {
		checks = append(checks, warn("ip_forward", "net.ipv4.ip_forward=%s; enable it (sysctl -w net.ipv4.ip_forward=1) or guest egress fails", or(v, "0")))
	}

	// nftables drives the NAT + egress allowlist.
	if _, err := exec.LookPath("nft"); err != nil {
		checks = append(checks, warn("nftables", "nft not found in PATH; egress NAT rules cannot be applied"))
	} else {
		checks = append(checks, pass("nftables", "nft present"))
	}

	// Work dir: writable and, ideally, reflink-capable so rootfs clones are cheap.
	checks = append(checks, workdirCheck(fc.WorkDir))
	// Reflink only works within one filesystem, so a golden on a different
	// device than the workdir silently defeats it however capable the workdir is.
	checks = append(checks, reflinkLocalityCheck(fc.WorkDir, goldens))
	// Log dir (only when off-VM console log forwarding is configured).
	if fc.LogDir != "" {
		checks = append(checks, logDirCheck(fc.LogDir))
	}

	// GitHub auth: presence/parse first, then a live read-only verification.
	apiBase := githubAPIBase(cfg.URL)
	auth := authCheck(cfg)
	checks = append(checks, auth)
	if auth.Level == levelPass {
		checks = append(checks, authVerify(cfg, apiBase)...)
	}

	// GitHub API reachability (best-effort; WARN offline, never FAIL).
	checks = append(checks, apiReach(apiBase))

	// Self-hosted dependency cache (only when configured).
	if c := cacheCheck(fc); c != nil {
		checks = append(checks, *c)
	}

	checks = append(checks, listenerChecks(fc, time.Now())...)

	failed := 0
	for _, c := range checks {
		if c.Level == levelFail {
			failed++
		}
	}
	return DoctorReport{
		Version: version,
		Checks:  checks,
		Total:   len(checks),
		Failed:  failed,
		OK:      failed == 0,
	}
}

// --- checks ---------------------------------------------------------------

// listenerChecks reports each tier's listener health from the running
// daemon's state file: FAIL for a tier that stopped on a fatal error, WARN for
// one reconnecting. With no state file (doctor run before the daemon starts)
// or a stale one, there is nothing current to judge, so it only says so.
func listenerChecks(fc provisioner.FirecrackerConfig, now time.Time) []Check {
	li, err := readListeners(fc, now)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return []Check{pass("listeners", "no listener state published (daemon not running yet)")}
	case err != nil:
		return []Check{warn("listeners", "unreadable listener state: %v", err)}
	case li.Stale:
		return []Check{warn("listeners", "listener state last updated %s ago; is the daemon running?", humanDuration(now.Sub(li.WrittenAt).Truncate(time.Second)))}
	}
	var checks []Check
	for _, t := range li.Tiers {
		name := "listener[" + t.Name + "]"
		switch t.State {
		case health.Failed:
			checks = append(checks, fail(name, "stopped taking jobs: %s", truncate(t.LastError, 200)))
		case health.Reconnecting:
			checks = append(checks, warn(name, "reconnecting since %s ago (%d reconnects so far): %s",
				humanDuration(now.Sub(t.Since).Truncate(time.Second)), t.Reconnects, truncate(t.LastError, 200)))
		default:
			if t.Drift != "" {
				checks = append(checks, warn(name, "connected, but GitHub and firerunner disagree: %s", truncate(t.Drift, 200)))
				continue
			}
			checks = append(checks, pass(name, "connected, %d reconnects", t.Reconnects))
		}
	}
	return checks
}

// truncate shortens s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

const (
	levelPass = "PASS"
	levelWarn = "WARN"
	levelFail = "FAIL"
)

type Check struct {
	Level  string `json:"level"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func pass(name, f string, a ...any) Check { return Check{levelPass, name, fmt.Sprintf(f, a...)} }
func warn(name, f string, a ...any) Check { return Check{levelWarn, name, fmt.Sprintf(f, a...)} }
func fail(name, f string, a ...any) Check { return Check{levelFail, name, fmt.Sprintf(f, a...)} }

// binVersion resolves an executable and captures its --version banner.
func binVersion(name, bin string) Check {
	path, err := exec.LookPath(bin)
	if err != nil {
		return fail(name, "%q not found: %v", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return warn(name, "%s (version probe failed: %v)", path, err)
	}
	return pass(name, "%s", firstLine(string(out)))
}

// fileCheck verifies a path exists, is readable and is at least minBytes.
func fileCheck(name, path string, minBytes int64) Check {
	if path == "" {
		return fail(name, "path is unset")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fail(name, "%s: %v", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fail(name, "%s not readable: %v", path, err)
	}
	f.Close()
	if fi.Size() < minBytes {
		return warn(name, "%s is only %s (looks too small)", path, humanBytes(fi.Size()))
	}
	return pass(name, "%s (%s)", path, humanBytes(fi.Size()))
}

func workdirCheck(dir string) Check {
	if dir == "" {
		return fail("workdir", "path is unset")
	}
	// A diagnostic must not mutate host state, so don't create the dir; report
	// whether it exists and is usable. The provisioner creates it on first run.
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		// The provisioner will mkdir it on first run, which only succeeds if the
		// nearest existing ancestor is writable — probe that rather than guess.
		if !parentWritable(dir) {
			return fail("workdir", "%s does not exist and no existing ancestor is writable by uid %d; firerunner cannot create it on first run", dir, os.Geteuid())
		}
		return warn("workdir", "%s does not exist yet; firerunner creates it on first run (parent writable, reflink capability unverified until then)", dir)
	case err != nil:
		return fail("workdir", "%s not accessible: %v", dir, err)
	case !info.IsDir():
		return fail("workdir", "%s exists but is not a directory", dir)
	}
	// Writability and reflink support are probed as the INVOKING user, which is
	// only meaningful when doctor runs as the service user; say so.
	uid := os.Geteuid()
	probe := filepath.Join(dir, ".firerunner-doctor")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fail("workdir", "%s not writable as uid %d: %v", dir, uid, err)
	}
	os.Remove(probe)
	kind := fsKind(dir)
	if !reflinkProbe(dir) {
		return warn("workdir", "%s is %s; reflink clones unsupported here, each microVM copies the full golden (slow)", dir, kind)
	}
	return pass("workdir", "%s writable as uid %d, %s (reflink-capable)", dir, uid, kind)
}

// logDirCheck reports whether the off-VM console log directory is usable. Like
// workdirCheck it never creates the dir; the provisioner does so on first run.
func logDirCheck(dir string) Check {
	uid := os.Geteuid()
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		if !parentWritable(dir) {
			return fail("log-dir", "%s does not exist and no existing ancestor is writable by uid %d; console log forwarding will fail", dir, uid)
		}
		return warn("log-dir", "%s does not exist yet; firerunner creates it on first run (parent writable)", dir)
	case err != nil:
		return fail("log-dir", "%s not accessible: %v", dir, err)
	case !info.IsDir():
		return fail("log-dir", "%s exists but is not a directory", dir)
	}
	probe := filepath.Join(dir, ".firerunner-doctor")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fail("log-dir", "%s not writable as uid %d: %v", dir, uid, err)
	}
	os.Remove(probe)
	return pass("log-dir", "%s writable as uid %d", dir, uid)
}

// reflinkLocalityCheck warns when any golden lives on a different filesystem
// than the workdir: reflink (copy-on-write) clones cannot cross a device
// boundary, so such a golden is copied in full for every microVM even when the
// workdir filesystem is otherwise reflink-capable.
func reflinkLocalityCheck(workdir string, goldens []string) Check {
	wdev, ok := deviceID(workdir)
	if !ok {
		return warn("reflink-locality", "cannot determine the filesystem device of %s", workdir)
	}
	var elsewhere []string
	for _, g := range goldens {
		if g == "" {
			continue
		}
		if gdev, ok := deviceID(g); ok && gdev != wdev {
			elsewhere = append(elsewhere, g)
		}
	}
	if len(elsewhere) > 0 {
		return warn("reflink-locality", "golden(s) on a different filesystem than %s, so cross-device reflink is impossible and each microVM copies the full golden (slow): %s", workdir, strings.Join(elsewhere, ", "))
	}
	return pass("reflink-locality", "golden(s) share the workdir filesystem; reflink clones are possible")
}

// parentWritable reports whether the nearest existing ancestor of path is a
// writable directory, by creating and removing a probe file there. It is how
// doctor predicts whether the provisioner's mkdir on first run will succeed.
func parentWritable(path string) bool {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return false
			}
			probe := filepath.Join(parent, ".firerunner-doctor")
			if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
				return false
			}
			os.Remove(probe)
			return true
		}
		if parent == "/" || parent == "." {
			return false
		}
	}
}

// deviceID returns the filesystem device id of path, walking up to the nearest
// existing ancestor when path itself does not exist yet (e.g. a workdir the
// provisioner has not created). The bool is false when nothing along the path
// can be stat'd.
func deviceID(path string) (uint64, bool) {
	for p := path; ; p = filepath.Dir(p) {
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err == nil {
			return uint64(st.Dev), true
		}
		if p == "/" || p == "." || p == "" {
			return 0, false
		}
	}
}

func authCheck(cfg *config.Config) Check {
	if cfg.Token == "" {
		if cfg.AppClientID == "" {
			return fail("auth", "no --token and no GitHub App credentials")
		}
		if cfg.AppInstallID == 0 {
			return fail("auth", "GitHub App set but --app-installation-id is missing")
		}
	}
	// Startup refuses a configured key that does not parse even beside a
	// token, so doctor does too.
	if _, err := cfg.ResolvePrivateKey(); err != nil {
		return fail("auth", "GitHub App private key: %v", err)
	}
	if cfg.Token != "" {
		return pass("auth", "PAT configured")
	}
	return pass("auth", "GitHub App configured (client %s, installation %d)", cfg.AppClientID, cfg.AppInstallID)
}

// fetchCacheStats best-effort reads the cache-server's /stats snapshot for the
// status report. It returns nil on any error (server down, wrong endpoint) so
// status still renders without live numbers.
func fetchCacheStats(fc provisioner.FirecrackerConfig) *cacheserver.Stats {
	var base string
	switch {
	case fc.CacheURL != "":
		base = strings.TrimRight(fc.CacheURL, "/")
	case fc.CachePort != 0:
		base = fmt.Sprintf("http://127.0.0.1:%d", fc.CachePort)
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stats", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var st cacheserver.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil
	}
	return &st
}

// hitRate renders hits/(hits+misses) as a percentage, or "n/a" before any
// lookups.
func hitRate(hits, misses uint64) string {
	total := hits + misses
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", float64(hits)/float64(total)*100)
}

// cacheCheck probes the self-hosted dependency cache when one is configured, and
// is nil otherwise. In gateway mode the cache-server binds 0.0.0.0 on the host,
// so doctor probes it on localhost; in url mode it probes the given URL. Any
// HTTP response means the server is listening (there is no health endpoint), so
// a 404 still passes. It never FAILs (the cache is a pure accelerator).
func cacheCheck(fc provisioner.FirecrackerConfig) *Check {
	var probe string
	switch {
	case fc.CacheURL != "":
		probe = fc.CacheURL
	case fc.CachePort != 0:
		probe = fmt.Sprintf("http://127.0.0.1:%d/", fc.CachePort)
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c := warn("dep-cache", "cache-server %s unreachable: %v (run 'firerunner cache-server')", probe, err)
		return &c
	}
	resp.Body.Close()
	c := pass("dep-cache", "cache-server reachable at %s (needs a --cache-redirect golden)", probe)
	return &c
}

func apiReach(url string) Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return warn("github-api", "%s unreachable: %v", url, err)
	}
	resp.Body.Close()
	// Any HTTP response proves reachability (a 401/404 still means we reached
	// the host); only a server-side failure or transport error is a concern.
	if resp.StatusCode >= 500 {
		return warn("github-api", "%s returned HTTP %d", url, resp.StatusCode)
	}
	return pass("github-api", "%s reachable (HTTP %d)", url, resp.StatusCode)
}

// --- host probes ----------------------------------------------------------

type vmInfo struct {
	name        string
	rootfsBytes int64
	started     time.Time
}

// activeVMs enumerates per-job work dirs (each holds a rootfs clone + API
// socket), matching the provisioner's own layout.
func activeVMs(fc provisioner.FirecrackerConfig) []vmInfo {
	var out []vmInfo
	socks, _ := filepath.Glob(filepath.Join(fc.WorkDir, "*", "fc.sock"))
	if fc.Jailer {
		js, _ := filepath.Glob(filepath.Join(fc.ChrootBase, "firecracker", "*", "root", "run", "firecracker.socket"))
		socks = append(socks, js...)
	}
	for _, sock := range socks {
		dir := filepath.Dir(sock)
		name := filepath.Base(dir)
		if strings.HasSuffix(sock, "firecracker.socket") {
			// <base>/firecracker/<id>/root/run/firecracker.socket -> id.
			name = filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(sock))))
		}
		v := vmInfo{name: name}
		if fi, err := os.Stat(sock); err == nil {
			v.started = fi.ModTime()
		}
		rootfs := filepath.Join(dir, "rootfs.ext4")
		if st, err := os.Stat(rootfs); err == nil {
			v.rootfsBytes = actualBytes(st)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// firecrackerProcs counts running firecracker processes that belong to THIS
// firerunner instance, so a peer instance sharing the host (e.g. a second
// scale set) does not inflate the count. Ownership is decided from each VMM's
// /proc entry: in direct mode its cmdline carries the instance work dir
// (--api-sock <WorkDir>/<id>/fc.sock); in jailer mode it is chrooted under the
// instance's ChrootBase.
func firecrackerProcs(fc provisioner.FirecrackerConfig) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !isPID(e.Name()) {
			continue
		}
		if strings.TrimSpace(readFile(filepath.Join("/proc", e.Name(), "comm"))) != "firecracker" {
			continue
		}
		if procBelongsToInstance(e.Name(), fc) {
			n++
		}
	}
	return n
}

// procBelongsToInstance reports whether the firecracker process /proc/<pid>
// was launched by this firerunner instance.
func procBelongsToInstance(pid string, fc provisioner.FirecrackerConfig) bool {
	cmdline := strings.ReplaceAll(readFile(filepath.Join("/proc", pid, "cmdline")), "\x00", " ")
	// os.Readlink fails (and returns "") for a peer instance's chroot owned by
	// a different jail uid, which correctly excludes it.
	rootLink, _ := os.Readlink(filepath.Join("/proc", pid, "root"))
	return procMatchesInstance(cmdline, rootLink, fc)
}

// procMatchesInstance is the pure ownership test split out for testing.
//   - Direct mode: --api-sock <WorkDir>/<id>/fc.sock embeds the work dir in the
//     firecracker cmdline.
//   - Jailer mode: the VMM is chrooted under <ChrootBase>/firecracker/<id>/root,
//     so its root symlink resolves inside ChrootBase.
//
// Both markers are matched at a path boundary (a trailing separator) so a peer
// instance whose WorkDir/ChrootBase merely shares a string prefix — e.g.
// /var/tmp/firerunner vs /var/tmp/firerunner-kaikhq — is not miscounted.
func procMatchesInstance(cmdline, rootLink string, fc provisioner.FirecrackerConfig) bool {
	if fc.WorkDir != "" && strings.Contains(cmdline, filepath.Clean(fc.WorkDir)+"/") {
		return true
	}
	if fc.ChrootBase != "" && rootLink != "" && strings.HasPrefix(rootLink, filepath.Clean(fc.ChrootBase)+"/") {
		return true
	}
	return false
}

func countTaps(prefix string) int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return -1
	}
	n := 0
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, prefix) {
			n++
		}
	}
	return n
}

// --- small helpers --------------------------------------------------------

func statFile(path string) FileStat {
	f := FileStat{Path: path}
	if path == "" {
		return f
	}
	fi, err := os.Stat(path)
	if err != nil {
		return f
	}
	f.Exists = true
	f.Bytes = fi.Size()
	f.Human = humanBytes(fi.Size())
	f.ModTime = fi.ModTime().Format("2006-01-02 15:04")
	return f
}

func ifaceUp(name string) bool {
	if name == "" {
		return false
	}
	_, err := net.InterfaceByName(name)
	return err == nil
}

func ifaceStateStr(name string, up bool) string {
	if name == "" {
		return "(unset)"
	}
	if !up {
		return name + "(missing)"
	}
	return name
}

func egressDesc(cats []string) string {
	if len(cats) == 0 {
		// An empty category list is not "open": buildNFTRuleset emits an empty
		// @allowed set, so every new guest connection is dropped. Report it as
		// the deny-all it actually is rather than the opposite.
		return "none (deny-all)"
	}
	return strings.Join(cats, "+")
}

func fsKind(dir string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "unknown"
	}
	switch int64(st.Type) {
	case 0x9123683E:
		return "btrfs"
	case 0x58465342:
		return "xfs"
	case 0xEF53:
		return "ext2/3/4"
	case 0x01021994:
		return "tmpfs"
	case 0x2FC12FC1:
		return "zfs"
	default:
		return fmt.Sprintf("fstype=0x%x", uint64(st.Type))
	}
}

// reflinkProbe reports whether dir's filesystem can actually make reflink
// (copy-on-write) clones, by cloning a probe file within dir with
// cp --reflink=always. It uses =always -- not the =auto the provisioner uses
// for the golden->workdir clone -- precisely so an unsupported filesystem fails
// loudly here instead of silently falling back to a full copy: this is ground
// truth about the filesystem's capability rather than a guess from the fs type;
// XFS without reflink=1 or ZFS without block cloning would otherwise be
// mis-reported as capable. Cloning inside dir keeps the probe on the workdir's
// own filesystem; whether the golden shares that filesystem is a separate
// question (see reflinkLocalityCheck). Returns false if dir is not writable or
// cp is unavailable.
func reflinkProbe(dir string) bool {
	src, err := os.CreateTemp(dir, ".firerunner-reflink-*")
	if err != nil {
		return false
	}
	srcPath := src.Name()
	_, _ = src.WriteString("probe")
	_ = src.Close()
	defer os.Remove(srcPath)
	dstPath := srcPath + ".clone"
	defer os.Remove(dstPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "cp", "--reflink=always", srcPath, dstPath).Run() == nil
}

// actualBytes reports on-disk usage (st_blocks*512), so a reflink/sparse clone
// shows what it really consumes rather than its apparent size.
func actualBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// isPID reports whether name is all digits (a /proc PID directory).
func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
