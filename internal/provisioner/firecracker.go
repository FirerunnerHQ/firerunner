package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

// FirecrackerConfig configures the direct-Firecracker provisioner. It
// deliberately depends on nothing beyond the firecracker binary and the Linux
// host tools (cp with reflink, ip, nft, sysctl) — no containerd, CNI or LVM.
type FirecrackerConfig struct {
	// Jobs binds VM addresses to host-observed jobs. Nil when local caching is off.
	Jobs VMJobs

	// Binary is the path to the firecracker executable.
	Binary string
	// KernelImage is the uncompressed guest kernel (vmlinux) path.
	KernelImage string
	// GoldenRootFS is the immutable base rootfs for the default tier. Each job
	// gets a reflink (copy-on-write) clone of it.
	GoldenRootFS string
	// BootArgs is the base kernel command line. A per-VM `ip=` clause is appended
	// at launch. `reboot=k` is required so the guest `reboot -f` triggers an
	// i8042 reset that makes the VMM exit.
	BootArgs string
	// WorkDir is where per-job rootfs clones and API sockets are created. It
	// should live on a reflink-capable filesystem (btrfs/XFS) for near-zero
	// clone cost.
	WorkDir string
	// TapPrefix is the prefix for per-job tap device names.
	TapPrefix string
	// NetBase is the second octet of the per-microVM /16 (default 16 ->
	// 172.16.x). Distinct values let multiple firerunner instances share a host
	// with non-overlapping guest subnets.
	NetBase int
	// NFTTable is the nftables table firerunner manages (default "firerunner").
	// A second instance must use a distinct name so the two rulesets do not
	// clobber each other.
	NFTTable string
	// ExtIface is the host's external interface used for microVM egress NAT
	// (e.g. eth0, enp2s0). Required for the guest to reach GitHub.
	ExtIface string
	// Egress restricts what microVM guests may reach on the network.
	Egress EgressConfig
	// LogDir, when set, receives one <runner>.log file per microVM capturing the
	// guest serial console. Because the microVM is destroyed after its single
	// job, this is how runner logs are forwarded off the ephemeral VM.
	LogDir string
	// MaxVMs bounds concurrent microVMs and sizes the per-VM network pool.
	MaxVMs int

	// Jailer, when true, launches every microVM under the Firecracker jailer,
	// which chroots the VMM, gives it its own PID namespace and drops it to an
	// unprivileged uid/gid. It is opt-in: firerunner already runs non-root and
	// Firecracker already installs seccomp filters by default, so the jailer is
	// an incremental defence-in-depth step whose cost is that it needs a root
	// launcher. No network namespace is used, so the host-namespace tap devices
	// work unchanged.
	Jailer bool
	// JailerBin is the path to the jailer executable (must match the firecracker
	// version). Defaults to "jailer".
	JailerBin string
	// ChrootBase is the jailer chroot base dir (default "/srv/jailer"). Each
	// microVM gets <ChrootBase>/firecracker/<id>/root.
	ChrootBase string
	// JailUID and JailGID are the unprivileged uid/gid the jailer drops the VMM
	// to. Required (>0) when Jailer is set.
	JailUID int
	JailGID int
	// CgroupLimits are cgroup v2 <file>=<value> settings the jailer applies to
	// each microVM's own cgroup (e.g. "memory.max=2147483648", "cpu.max=200000",
	// "pids.max=512"), bounding one VM's blast radius on the shared host. Only
	// used in jailer mode: each becomes a `--cgroup` flag and the jailer places
	// the VMM in <cgroupV2Mount>/<firecracker>/<id>, enabling the controllers it
	// needs. The jailer never reclaims that dir, so firerunner rmdirs the empty
	// cgroup on teardown.
	CgroupLimits []string
	// NetNS, when true, runs every microVM in its own network namespace: the
	// guest tap lives inside the namespace and a veth pair links it to the host,
	// so the tap is invisible to the host namespace and to peer microVMs. It
	// requires Jailer (creating a netns needs CAP_SYS_ADMIN, which the default
	// non-root deployment lacks; the jailer already runs as root and joins the
	// namespace natively via --netns).
	NetNS bool
	// ToolCacheImage, when set, is the host path to a read-only ext4 image that
	// mirrors GitHub's hosted-tool-cache layout (e.g. go/<ver>/x64/ plus the
	// x64.complete marker, labelled "hostedtoolcache"). firerunner attaches it to
	// every microVM as a shared read-only drive; the golden mounts it at
	// /opt/hostedtoolcache and points RUNNER_TOOL_CACHE at it, so setup-* actions
	// hit the pre-seeded cache instead of downloading. It is opt-in and a pure
	// accelerator: when unset (or when a requested toolchain is not in the image)
	// setup-* falls back to downloading, so jobs still pass either way.
	ToolCacheImage string

	// CachePort, when non-zero, is the TCP port of a `firerunner cache-server`
	// reachable on each microVM's host gateway IP. firerunner publishes it into
	// the guest via MMDS; the guest builds ACTIONS_RESULTS_URL from its default
	// gateway and this port so actions/cache (and the pnpm/go/setup-* caches
	// built on it) hit the self-hosted cache instead of GitHub's hosted cache.
	// Opt-in and off by default; jobs still pass without it (cache simply falls
	// back to a miss / GitHub's cache when the golden is not cache-redirected).
	CachePort int
	// CacheURL, when set, is an explicit base URL of a cache-server on this
	// host, used verbatim by the guest. It overrides CachePort. The server
	// authorizes by guest source IP against this daemon's jobs directory, so it
	// must not sit behind SNAT or a proxy. Opt-in and off by default.
	CacheURL string

	// MaxVMLifetime, when non-zero, is a host-side backstop on how long a single
	// microVM may run before firerunner kills its VMM to reclaim the network
	// slot and running-count it holds. A microVM normally self-destructs the
	// instant its one job ends, so this only fires on a genuinely stuck VMM
	// (hung guest, wedged job). It is deliberately generous — larger than any
	// legitimate job — so it never truncates real work; zero disables it.
	MaxVMLifetime time.Duration

	// ConnectTimeout, when non-zero, bounds how long a freshly booted microVM
	// may take to register with GitHub — the runner's "Listening for Jobs" on
	// the console, or "Running job:" when a job is dequeued before the idle
	// line ever prints — before firerunner kills it and frees its slot. A VM that
	// never connects — no route or DNS to GitHub, a rejected JIT config, a
	// broken golden — would otherwise sit idle holding a slot for the whole
	// MaxVMLifetime while the scheduler saw a healthy running VM. The kill is
	// reported as a boot failure so replenishment backs off instead of
	// hot-looping, and recovers on its own once the cause clears. Zero
	// disables it.
	ConnectTimeout time.Duration
}

// DefaultBootArgs is a minimal, quiet serial console command line. reboot=k is
// mandatory for the self-destruct-on-reboot lifecycle.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"

// commandRunner runs an external command. It is a seam so tests can substitute
// a fake for cp / ip / firecracker without touching the host.
type commandRunner func(ctx context.Context, name string, args ...string) error

// commandOutputRunner runs an external command and captures its combined
// output. It is a seam (like commandRunner) so host-state probes such as
// `nft list chain` can be faked in tests.
type commandOutputRunner func(ctx context.Context, name string, args ...string) (string, error)

type VMJobs interface {
	Register(runner, ip string) error
	Release(runner string) error
}

// Firecracker is a Provisioner that talks to the Firecracker REST API directly
// over its unix socket using only the standard library.
type Firecracker struct {
	cfg     FirecrackerConfig
	log     *slog.Logger
	run     commandRunner
	runOut  commandOutputRunner
	http    *http.Client
	ipam    *ipam
	netOnce sync.Once
	netErr  error
}

// NewFirecracker returns a Firecracker provisioner, filling in defaults.
func NewFirecracker(cfg FirecrackerConfig, log *slog.Logger) *Firecracker {
	if cfg.Binary == "" {
		cfg.Binary = "firecracker"
	}
	if cfg.BootArgs == "" {
		cfg.BootArgs = DefaultBootArgs
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/var/tmp/firerunner"
	}
	if cfg.TapPrefix == "" {
		cfg.TapPrefix = "fr"
	}
	if cfg.NetBase == 0 {
		cfg.NetBase = 16
	}
	if cfg.NFTTable == "" {
		cfg.NFTTable = natTable
	}
	if cfg.JailerBin == "" {
		cfg.JailerBin = "jailer"
	}
	if cfg.ChrootBase == "" {
		cfg.ChrootBase = "/srv/jailer"
	}
	if cfg.MaxVMs < 1 {
		cfg.MaxVMs = 64
	}
	return &Firecracker{
		cfg:    cfg,
		log:    log,
		run:    execRun,
		runOut: execRunOut,
		http:   &http.Client{Timeout: 30 * time.Second},
		ipam:   newIPAM(cfg.MaxVMs),
	}
}

// Name implements Provisioner.
func (f *Firecracker) Name() string { return "firecracker" }

// FreeSlots implements SlotReporter with the network slot pool, which is the
// shared --max-runners budget every tier draws from.
func (f *Firecracker) FreeSlots() int { return f.ipam.available() }

// Launch implements Provisioner: reflink-clone the golden rootfs, allocate a
// per-VM network slot, create a tap, boot the microVM with the JIT config
// delivered via MMDS v2, then block until the guest self-destructs (reboot -f)
// and reap everything.
func (f *Firecracker) Launch(ctx context.Context, name, jitConfig string, spec core.RunnerSpec, onBusy func()) error {
	entered := time.Now()
	// Draw the slot before anything else: the scheduler stops counting this
	// launch as pending the moment Launch is entered, so FreeSlots must reflect
	// it from the first instruction on.
	slot, ok := f.ipam.acquire()
	if !ok {
		return fmt.Errorf("no free network slot (max %d microVMs)", f.cfg.MaxVMs)
	}

	vnet := slotNet(slot, f.cfg.TapPrefix, f.cfg.NetBase)
	registered := false
	defer func() {
		if registered {
			if err := f.cfg.Jobs.Release(name); err != nil {
				f.log.Error("cache identity revocation failed; quarantining slot", "slot", slot, "err", err)
				return
			}
		}
		f.ipam.release(slot)
	}()
	if f.cfg.Jobs != nil {
		if err := f.cfg.Jobs.Register(name, vnet.guestIP); err != nil {
			return fmt.Errorf("register VM identity: %w", err)
		}
		registered = true
	}

	if err := f.SetupNetwork(ctx); err != nil {
		return fmt.Errorf("setup network: %w", err)
	}
	if err := f.setupNet(ctx, vnet); err != nil {
		return fmt.Errorf("setup network for %s: %w", vnet.tap, err)
	}
	netDone := time.Now()
	defer func() { _ = f.teardownNet(context.WithoutCancel(ctx), vnet) }()

	var connect connectWatchdog
	console, closeConsole, err := f.openConsole(name, onBusy, connect.markConnected)
	if err != nil {
		return fmt.Errorf("open console log: %w", err)
	}
	defer closeConsole()

	cmd, sock, kernelPath, rootfsPath, cleanup, err := f.prepare(ctx, name, vnet, console, spec)
	if err != nil {
		return err
	}
	defer cleanup()
	prepared := time.Now()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	if err := waitForSocket(ctx, sock, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // reap the killed VMM so it leaves no zombie/fd/goroutine
		return fmt.Errorf("wait for api socket: %w", err)
	}

	bootArgs := composeBootArgs(f.cfg.BootArgs, vnet)
	if err := f.configure(ctx, sock, kernelPath, rootfsPath, f.fcToolCachePath(spec), vnet, bootArgs, jitConfig, spec); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // reap the killed VMM so it leaves no zombie/fd/goroutine
		return fmt.Errorf("configure microVM: %w", err)
	}

	f.log.Info("microVM started", "runner", name, "slot", slot, "guestIP", vnet.guestIP, "vcpu", spec.VCPU, "memMiB", spec.MemMiB,
		"net", netDone.Sub(entered).Round(time.Millisecond),
		"prepare", prepared.Sub(netDone).Round(time.Millisecond),
		"start", time.Since(prepared).Round(time.Millisecond))
	bootedAt := time.Now()

	// Host-side lifetime backstop: a healthy microVM self-destructs the instant
	// its one job ends, so a VM still alive after MaxVMLifetime is stuck and is
	// holding its slot and running-count hostage. Kill the VMM to reclaim them.
	var lifetimeKill atomic.Bool
	if f.cfg.MaxVMLifetime > 0 {
		timer := time.AfterFunc(f.cfg.MaxVMLifetime, func() {
			lifetimeKill.Store(true)
			f.log.Warn("microVM exceeded max lifetime; killing to reclaim its slot", "runner", name, "maxLifetime", f.cfg.MaxVMLifetime)
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		defer timer.Stop()
	}

	// Connect watchdog: a VM that boots but never registers with GitHub holds
	// its slot while looking healthy to the scheduler. Kill it at the deadline
	// and report a failure so replenishment backs off (see errNeverConnected).
	connect.arm(f.cfg.ConnectTimeout, func() {
		f.log.Warn("microVM never registered with GitHub; killing to reclaim its slot — check guest DNS/egress and the JIT config",
			"runner", name, "connectTimeout", f.cfg.ConnectTimeout)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer connect.stop()

	err = cmd.Wait() // returns when the guest reboots (self-destruct) or dies
	ranFor := time.Since(bootedAt)
	// Disarm before reading didKill: a deadline landing in the gap between
	// Wait returning and the deferred stop would otherwise record a kill of an
	// already-exited VMM and misreport its real exit as never-connected.
	connect.stop()
	if connect.didKill() {
		return wrapNeverConnected(name, f.cfg.ConnectTimeout)
	}
	if lifetimeKill.Load() {
		// A backstop kill is an intentional reclaim, not a boot failure: report
		// success so the scheduler simply replenishes the warm pool.
		f.log.Warn("microVM killed after exceeding max lifetime", "runner", name, "ranFor", ranFor.Round(time.Second))
		return nil
	}
	if failErr := classifyLaunchExit(err, ctx.Err(), ranFor, minBootRuntime); failErr != nil {
		f.log.Error("microVM wait failed", "runner", name, "err", failErr, "ranFor", ranFor.Round(time.Millisecond))
		return fmt.Errorf("wait for microVM %s: %w", name, failErr)
	}
	switch {
	case err == nil:
		f.log.Info("microVM exited", "runner", name)
	case ctx.Err() != nil:
		// We cancelled this VM ourselves (idle-drain / shutdown SIGKILLs the VMM
		// via exec.CommandContext); that death is expected, not a failure.
		f.log.Info("microVM cancelled", "runner", name)
	default:
		// Non-zero exit after a healthy runtime: the guest's reboot=k
		// self-destruct, which makes the VMM exit non-zero on some kernels.
		f.log.Info("microVM exited", "runner", name, "ranFor", ranFor.Round(time.Second))
	}
	return nil
}

// minBootRuntime is the shortest a microVM can plausibly run and still have
// booted, registered and done any work before its reboot=k self-destruct. A
// non-zero VMM exit sooner than this means the guest never came up, so Launch
// reports it as a failure instead of a normal self-destruct.
const minBootRuntime = 3 * time.Second

// classifyLaunchExit decides whether a VMM's exit is normal end-of-life or a
// real failure. A nil wait error, or any exit after we cancelled the VM
// (ctxErr != nil), is normal. The guest's reboot=k self-destruct makes the VMM
// exit non-zero, so a non-zero exit AFTER at least minRuntime is also normal;
// but a non-zero exit within minRuntime means the guest never booted (a broken
// golden, OOM before boot, ...) and is returned as an error so the caller can
// back off instead of hot-looping a relaunch. Any non-ExitError (I/O error,
// lost process) is always a failure.
func classifyLaunchExit(waitErr, ctxErr error, ranFor, minRuntime time.Duration) error {
	if waitErr == nil || ctxErr != nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if ranFor >= minRuntime {
			return nil
		}
		return fmt.Errorf("exited after %s (code %d): likely failed to boot: %w",
			ranFor.Round(time.Millisecond), exitErr.ExitCode(), waitErr)
	}
	return waitErr
}

// SetupNetwork installs host forwarding, the egress allowlist and NAT exactly
// once per process. It is safe to call from Launch and from main.
func (f *Firecracker) SetupNetwork(ctx context.Context) error {
	f.netOnce.Do(func() { f.netErr = f.applyNetwork(ctx) })
	return f.netErr
}

// RefreshNetwork periodically re-fetches GitHub's /meta ranges and re-applies
// the egress allowlist, until ctx is cancelled. It is a no-op when egress is
// open or refresh is disabled.
func (f *Firecracker) RefreshNetwork(ctx context.Context) {
	if f.cfg.Egress.open() || f.cfg.Egress.RefreshInterval <= 0 {
		return
	}
	t := time.NewTicker(f.cfg.Egress.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.applyNetwork(ctx); err != nil {
				f.log.Error("refresh egress allowlist", "err", err)
			}
		}
	}
}

// applyNetwork enables IPv4 forwarding and applies the nftables ruleset (egress
// allowlist + masquerade) atomically via `nft -f`.
func (f *Firecracker) applyNetwork(ctx context.Context) error {
	if f.cfg.ExtIface == "" {
		return errors.New("external interface is required for microVM egress")
	}

	rs := egressRuleset{
		MaxVMs:      f.cfg.MaxVMs,
		NetNS:       f.cfg.NetNS,
		Table:       f.cfg.NFTTable,
		IfacePrefix: f.cfg.TapPrefix,
		ExtIface:    f.cfg.ExtIface,
		VMCidr:      vmCIDRFor(f.cfg.NetBase),
		CachePort:   f.cfg.CachePort,
		DNSServers:  f.cfg.Egress.DNSServers,
		AllowDNS:    f.cfg.Egress.has("dns"),
		AllowNTP:    f.cfg.Egress.has("ntp"),
		Open:        f.cfg.Egress.open(),
	}
	if !rs.Open {
		cidrs, err := fetchMetaCIDRs(ctx, f.http, metaURL, f.cfg.Egress.metaCats())
		if err != nil {
			return fmt.Errorf("fetch GitHub meta ranges: %w", err)
		}
		rs.Allowed = cidrs
	}

	rules, err := buildNFTRuleset(rs)
	if err != nil {
		return err
	}
	if err := f.run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	if err := os.MkdirAll(f.cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("mkdir work dir: %w", err)
	}
	path := filepath.Join(f.cfg.WorkDir, "firerunner.nft")
	if err := os.WriteFile(path, []byte(rules), 0o600); err != nil {
		return fmt.Errorf("write nft ruleset: %w", err)
	}
	if err := f.run(ctx, "nft", "-f", path); err != nil {
		return err
	}
	if err := f.ensureHostForward(ctx); err != nil {
		return err
	}
	if err := f.ensureHostCacheInput(ctx); err != nil {
		return err
	}
	f.log.Info("egress network applied", "extIface", f.cfg.ExtIface, "open", rs.Open, "allowedCIDRs", len(rs.Allowed))
	return nil
}

// forwardComment tags the FORWARD accept rules firerunner adds to a foreign
// (ufw/docker/hardened) filter chain, so refreshes stay idempotent. It is keyed
// by tap prefix so multiple firerunner instances sharing a host each manage —
// and detect — their own accept rules instead of mistaking a peer's rules for
// their own and never inserting accepts for their tap devices.
func forwardComment(tapPrefix string) string {
	return "firerunner-egress-" + tapPrefix
}

// hostForwardScript returns an nft script that inserts accept rules for
// firerunner's tap interfaces into the host's ip/filter FORWARD chain.
//
// It is needed on hosts whose forward policy defaults to drop (ufw, docker,
// hardened hosts). firerunner's own table only masquerades in postrouting; a
// separate base chain's `accept` cannot override another base chain's `drop`
// verdict at the same hook, so the accept must live in the dropping chain
// itself. The allowlist `drop` in firerunner's own table still applies (drop is
// terminal across chains), so this does not widen egress beyond the policy.
func hostForwardScript(extIface, tapPrefix string) string {
	tap := tapPrefix + "*"
	comment := forwardComment(tapPrefix)
	return fmt.Sprintf(
		"insert rule ip filter FORWARD iifname %q oifname %q ct state established,related counter accept comment %q\n"+
			"insert rule ip filter FORWARD iifname %q oifname %q counter accept comment %q\n",
		extIface, tap, comment,
		tap, extIface, comment,
	)
}

// ensureHostForward keeps a foreign default-drop forward policy from silently
// blocking masqueraded microVM egress. It is best-effort and idempotent: it is
// a no-op when there is no ip/filter FORWARD chain (default-accept host), when
// the forward policy is not drop, or when firerunner's accept rules are already
// present.
func (f *Firecracker) ensureHostForward(ctx context.Context) error {
	out, err := f.runOut(ctx, "nft", "list", "chain", "ip", "filter", "FORWARD")
	if err != nil {
		// No ip/filter FORWARD chain (host without ufw/iptables-nft). A
		// default-accept forward policy needs nothing beyond our masquerade.
		f.log.Debug("no ip/filter FORWARD chain; skipping host forward accept", "err", err)
		return nil
	}
	if strings.Contains(out, forwardComment(f.cfg.TapPrefix)) {
		return nil // our accept rules are already present
	}
	if !strings.Contains(out, "policy drop") {
		return nil // permissive forward policy; masquerade is sufficient
	}
	path := filepath.Join(f.cfg.WorkDir, "firerunner-forward.nft")
	if err := os.WriteFile(path, []byte(hostForwardScript(f.cfg.ExtIface, f.cfg.TapPrefix)), 0o600); err != nil {
		return fmt.Errorf("write host forward ruleset: %w", err)
	}
	if err := f.run(ctx, "nft", "-f", path); err != nil {
		return fmt.Errorf("apply host forward accept: %w", err)
	}
	f.log.Info("added host forward accept for microVM egress",
		"extIface", f.cfg.ExtIface, "tapPrefix", f.cfg.TapPrefix)
	return nil
}

// cacheInputComment tags the INPUT accept rule firerunner adds for microVM ->
// host cache-server traffic, keyed by both tap prefix (so peer instances stay
// idempotent, same reasoning as forwardComment) and port. The port is part of
// the tag so that changing --cache-port is detected: the old port's rule no
// longer matches the wanted comment, so it is swept as stale and the new one
// inserted, instead of the port-less comment matching forever and leaving the
// old port open while never opening the new one.
func cacheInputComment(tapPrefix string, port int) string {
	return fmt.Sprintf("firerunner-cache-%s-%d", tapPrefix, port)
}

// cacheInputCommentPrefix matches this instance's cache-input rules on any port.
// The trailing "-" before the port keeps it from matching a different prefix
// whose name extends this one (letters-only prefixes plus a numeric port make
// the "-<port>" boundary unambiguous).
func cacheInputCommentPrefix(tapPrefix string) string {
	return "firerunner-cache-" + tapPrefix + "-"
}

// cacheInputScript returns an nft script that accepts guest -> host traffic to
// the local cache-server port on firerunner's tap interfaces.
func cacheInputScript(tapPrefix string, port int) string {
	tap := tapPrefix + "*"
	comment := cacheInputComment(tapPrefix, port)
	return fmt.Sprintf(
		"insert rule ip filter INPUT iifname %q tcp dport %d ct state new,established,related counter accept comment %q\n",
		tap, port, comment,
	)
}

// ensureHostCacheInput lets microVMs reach the self-hosted cache-server on their
// host gateway when the host's ip/filter INPUT policy defaults to drop (ufw,
// hardened hosts). It is best-effort and idempotent, and a no-op unless a local
// cache port is configured and the INPUT chain actually drops by default. A
// --cache-url-only deployment (same host, see CacheURL) is not opened here: the
// operator must accept that address and port on INPUT. It first
// sweeps any of this instance's cache rules left over from a previous, different
// --cache-port so a port change never strands the guest or leaves the old port
// open.
func (f *Firecracker) ensureHostCacheInput(ctx context.Context) error {
	if f.cfg.CachePort == 0 {
		return nil // no --cache-port; with --cache-url the operator must accept that address/port on INPUT
	}
	out, err := f.runOut(ctx, "nft", "-a", "list", "chain", "ip", "filter", "INPUT")
	if err != nil {
		// No ip/filter INPUT chain (default-accept host): the guest already
		// reaches the host gateway, nothing to open.
		f.log.Debug("no ip/filter INPUT chain; skipping host cache accept", "err", err)
		return nil
	}
	if err := f.removeStaleCacheInput(ctx, out); err != nil {
		return err
	}
	if strings.Contains(out, cacheInputComment(f.cfg.TapPrefix, f.cfg.CachePort)) {
		return nil // the rule for the current port is already present
	}
	if !strings.Contains(out, "policy drop") {
		return nil // permissive INPUT policy; the guest can already reach the host
	}
	path := filepath.Join(f.cfg.WorkDir, "firerunner-cache-input.nft")
	if err := os.WriteFile(path, []byte(cacheInputScript(f.cfg.TapPrefix, f.cfg.CachePort)), 0o600); err != nil {
		return fmt.Errorf("write host cache input ruleset: %w", err)
	}
	if err := f.run(ctx, "nft", "-f", path); err != nil {
		return fmt.Errorf("apply host cache input accept: %w", err)
	}
	f.log.Info("added host INPUT accept for microVM cache access",
		"tapPrefix", f.cfg.TapPrefix, "cachePort", f.cfg.CachePort)
	return nil
}

// removeStaleCacheInput deletes this instance's cache-input rules whose port no
// longer matches the configured one, using the rule handles in an `nft -a`
// listing. Rules for the current port are kept. It also removes the pre-port
// legacy comment ("firerunner-cache-<prefix>", written before the port was part
// of the tag) so upgrading from an older firerunner leaves no orphan rule.
func (f *Firecracker) removeStaleCacheInput(ctx context.Context, listing string) error {
	prefix := cacheInputCommentPrefix(f.cfg.TapPrefix)
	current := cacheInputComment(f.cfg.TapPrefix, f.cfg.CachePort)
	// The legacy tag has no "-<port>" suffix; match it as a whole quoted token so
	// it cannot also match a current "...-<port>" comment as a substring.
	legacy := `"firerunner-cache-` + f.cfg.TapPrefix + `"`
	for _, line := range strings.Split(listing, "\n") {
		stale := (strings.Contains(line, prefix) && !strings.Contains(line, current)) || strings.Contains(line, legacy)
		if !stale {
			continue
		}
		handle := parseNFTHandle(line)
		if handle == "" {
			continue
		}
		if err := f.run(ctx, "nft", "delete", "rule", "ip", "filter", "INPUT", "handle", handle); err != nil {
			return fmt.Errorf("delete stale cache input rule (handle %s): %w", handle, err)
		}
		f.log.Info("removed stale host INPUT cache rule after cache-port change",
			"handle", handle, "tapPrefix", f.cfg.TapPrefix, "cachePort", f.cfg.CachePort)
	}
	return nil
}

// parseNFTHandle extracts the rule handle from an `nft -a` listing line, which
// ends in "# handle N". It returns "" when the line carries no handle.
func parseNFTHandle(line string) string {
	const marker = "# handle "
	i := strings.LastIndex(line, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+len(marker):])
}

// openConsole returns the writer for the guest serial console. When LogDir is
// set the console is teed to a per-runner file so logs survive the microVM.
// When onBusy is non-nil the stream is also scanned for the runner's
// "Running job:" marker, which fires onBusy exactly once the instant the guest
// dequeues its job — the only real-time, per-VM busy signal available (the
// scale-set control plane delivers JobStarted batched at completion).
func (f *Firecracker) openConsole(name string, onBusy, onConnected func()) (io.Writer, func(), error) {
	// The busy detector goes first and never returns an error; the slog writer
	// never errors either. The per-runner log file is wrapped so a write error
	// (ENOSPC, read-only /var) is swallowed rather than propagated: io.MultiWriter
	// aborts on the first error, and os/exec then stops copying the console
	// entirely, which would silently kill busy detection and make the next
	// SIGTERM reap a busy VM as if it were idle. Losing a log line is acceptable;
	// losing the busy signal is not.
	var writers []io.Writer
	if onBusy != nil {
		writers = append(writers, newBusyDetector(onBusy))
	}
	if onConnected != nil {
		writers = append(writers, newConnectDetector(onConnected))
	}
	writers = append(writers, &logWriter{log: f.log, runner: name, stream: "console"})
	closeFn := func() {}
	if f.cfg.LogDir != "" {
		if err := os.MkdirAll(f.cfg.LogDir, 0o755); err != nil {
			return nil, nil, err
		}
		file, err := os.Create(filepath.Join(f.cfg.LogDir, name+".log"))
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, tolerantWriter{w: file})
		closeFn = func() { _ = file.Close() }
	}
	if len(writers) == 1 {
		return writers[0], closeFn, nil
	}
	return io.MultiWriter(writers...), closeFn, nil
}

// tolerantWriter wraps a writer so a failed Write is swallowed and reported as a
// full, successful write. It shields an io.MultiWriter (and the os/exec copier
// draining the guest console) from a best-effort sink — the per-runner log
// file — whose failure must not tear down the console stream that also carries
// the busy-detection signal.
type tolerantWriter struct{ w io.Writer }

func (t tolerantWriter) Write(p []byte) (int, error) {
	_, _ = t.w.Write(p)
	return len(p), nil
}

// busyMarker is printed by the in-guest Actions runner to the serial console
// the instant it dequeues a job (e.g. "Running job: build").
const busyMarker = "Running job:"

// connectedMarker is printed by the runner once it has registered with GitHub
// and is waiting for work. A job can in principle be dequeued so fast that the
// listener's idle line never appears, so busyMarker also counts as connected.
const connectedMarker = "Listening for Jobs"

// markerDetector is an io.Writer that scans the guest console stream for any
// of its markers and calls onHit exactly once when one appears. It keeps a
// small carryover tail so a marker split across two Writes is still matched,
// and always reports the full write as consumed so it never breaks the
// enclosing io.MultiWriter. exec.Cmd funnels the guest's stdout and stderr
// through a single fd when both point at the same writer, so Writes here are
// serialized and need no additional locking.
type markerDetector struct {
	markers [][]byte
	onHit   func()
	fired   bool
	tail    []byte
	keep    int // longest marker length - 1: the tail needed to bridge a split
}

func newMarkerDetector(onHit func(), markers ...string) *markerDetector {
	d := &markerDetector{onHit: onHit}
	for _, m := range markers {
		d.markers = append(d.markers, []byte(m))
		if n := len(m) - 1; n > d.keep {
			d.keep = n
		}
	}
	return d
}

// newBusyDetector fires once when the guest runner dequeues a job.
func newBusyDetector(onBusy func()) *markerDetector { return newMarkerDetector(onBusy, busyMarker) }

// newConnectDetector fires once when the guest runner has registered with
// GitHub (or gone straight to running a job).
func newConnectDetector(onConnected func()) *markerDetector {
	return newMarkerDetector(onConnected, connectedMarker, busyMarker)
}

func (d *markerDetector) Write(p []byte) (int, error) {
	if !d.fired {
		buf := append(d.tail, p...)
		for _, m := range d.markers {
			if bytes.Contains(buf, m) {
				d.fired = true
				d.tail = nil
				d.onHit()
				return len(p), nil
			}
		}
		// Carry over exactly the bytes a marker could straddle — and nothing when
		// no marker can straddle a boundary (keep == 0), so an idle stream never
		// accumulates.
		if len(buf) > d.keep {
			d.tail = append(d.tail[:0], buf[len(buf)-d.keep:]...)
		} else {
			d.tail = append(d.tail[:0], buf...)
		}
	}
	return len(p), nil
}

// connectWatchdog kills a microVM whose runner has not registered with GitHub
// within a deadline. It is created before the VMM exists (the console detector
// that reports the connect is wired first) and armed once the VM is booted;
// markConnected before arm simply leaves it inert. All three entry points run
// on different goroutines (console copier, Launch, timer), hence the mutex.
type connectWatchdog struct {
	mu        sync.Mutex
	timer     *time.Timer
	connected bool
	stopped   bool
	killed    bool
}

// arm starts the deadline; kill runs (once, on the timer goroutine) if the
// runner has not connected by then. A non-positive timeout disables it.
func (w *connectWatchdog) arm(timeout time.Duration, kill func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if timeout <= 0 || w.connected || w.stopped || w.timer != nil {
		return
	}
	w.timer = time.AfterFunc(timeout, func() {
		// timer.Stop can return false with this callback already scheduled, so
		// the flags — not Stop's result — are what make markConnected and stop
		// authoritative. Once Launch is unwinding (stopped), the VMM has been
		// waited on; a kill here would target a reaped, possibly recycled PID.
		w.mu.Lock()
		if w.connected || w.stopped {
			w.mu.Unlock()
			return
		}
		w.killed = true
		w.mu.Unlock()
		kill()
	})
}

// markConnected records that the runner registered and disarms the deadline.
func (w *connectWatchdog) markConnected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connected = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

// stop disarms the deadline without recording a connect (Launch is returning).
// After it returns the kill path can no longer run, even if the timer had
// already fired and its callback is waiting on the mutex.
func (w *connectWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

// didKill reports whether the deadline fired and the VMM was killed for never
// connecting.
func (w *connectWatchdog) didKill() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.killed
}

// errNeverConnected is returned by Launch when the connect watchdog killed the
// VM. It is a Launch failure on purpose: the scheduler then backs off before
// replenishing, so a host-wide cause (no DNS, no egress) yields a paced retry
// loop with a clear error instead of every slot silently re-wedging at once.
var errNeverConnected = errors.New("runner never registered with GitHub")

func wrapNeverConnected(name string, timeout time.Duration) error {
	return fmt.Errorf("microVM %s: %w within %s", name, errNeverConnected, timeout)
}

// apiStep is a single Firecracker configuration API call.
type apiStep struct {
	path string
	body any
}

// buildAPISteps returns the ordered Firecracker API sequence for a microVM.
// It is a pure function so tests can assert the exact order and payloads —
// notably that /mmds (the JIT secret) is set before InstanceStart, and that
// InstanceStart is always last.
func buildAPISteps(kernelImage, bootArgs, rootfs, toolcache, tap, guestMAC, jit string, cache map[string]any, spec core.RunnerSpec) []apiStep {
	steps := []apiStep{
		{"/boot-source", map[string]any{
			"kernel_image_path": kernelImage,
			"boot_args":         bootArgs,
		}},
		{"/drives/rootfs", map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   rootfs,
			"is_root_device": true,
			"is_read_only":   false,
		}},
	}
	// Optional shared read-only tool-cache drive. Read-only means the same host
	// image can back many microVMs at once without conflict.
	if toolcache != "" {
		steps = append(steps, apiStep{"/drives/toolcache", map[string]any{
			"drive_id":       "toolcache",
			"path_on_host":   toolcache,
			"is_root_device": false,
			"is_read_only":   true,
		}})
	}
	mmds := map[string]any{"jitconfig": jit}
	if len(cache) > 0 {
		mmds["cache"] = cache
	}
	return append(steps,
		apiStep{"/machine-config", map[string]any{
			"vcpu_count":   spec.VCPU,
			"mem_size_mib": spec.MemMiB,
		}},
		apiStep{"/network-interfaces/eth0", map[string]any{
			"iface_id":      "eth0",
			"host_dev_name": tap,
			"guest_mac":     guestMAC,
		}},
		apiStep{"/mmds/config", map[string]any{
			"version":            "V2",
			"network_interfaces": []string{"eth0"},
			"ipv4_address":       "169.254.169.254",
		}},
		apiStep{"/mmds", mmds},
		apiStep{"/actions", map[string]any{"action_type": "InstanceStart"}},
	)
}

// configure runs the Firecracker API sequence. MMDS config and data MUST be set
// before InstanceStart so the JIT secret is available to the guest at boot. The
// kernel and rootfs paths are given as Firecracker will see them (host paths for
// a direct launch, in-jail paths such as /vmlinux under the jailer).
func (f *Firecracker) configure(ctx context.Context, sock, kernelPath, rootfs, toolcache string, n vmNet, bootArgs, jit string, spec core.RunnerSpec) error {
	cl := newUnixClient(sock)
	for _, s := range buildAPISteps(kernelPath, bootArgs, rootfs, toolcache, n.tap, n.guestMAC, jit, f.cacheMMDS(), spec) {
		if err := putJSON(ctx, cl, s.path, s.body); err != nil {
			return fmt.Errorf("PUT %s: %w", s.path, err)
		}
	}
	return nil
}

// cacheMMDS returns the cache configuration firerunner publishes to the guest
// via MMDS, or nil when self-hosted dependency caching is not enabled. When a
// full URL is configured the guest uses it verbatim; otherwise the guest builds
// the URL from its default gateway and the given port.
func (f *Firecracker) cacheMMDS() map[string]any {
	switch {
	case f.cfg.CacheURL != "":
		return map[string]any{"url": f.cfg.CacheURL}
	case f.cfg.CachePort != 0:
		return map[string]any{"port": strconv.Itoa(f.cfg.CachePort)}
	default:
		return nil
	}
}

// toolCacheImage returns the host path to the tool-cache image for a tier: the
// tier's own image (spec.ToolCache) when set, otherwise the process-global
// default (--toolcache / FR_TOOLCACHE), or "" when neither is configured.
func (f *Firecracker) toolCacheImage(spec core.RunnerSpec) string {
	if spec.ToolCache != "" {
		return spec.ToolCache
	}
	return f.cfg.ToolCacheImage
}

// fcToolCachePath returns the tool-cache image path as Firecracker will see it
// for a given tier, or "" when no tool cache applies. Under the jailer the
// image is staged inside the chroot, so the VMM sees the in-jail path;
// otherwise it opens the host path directly.
func (f *Firecracker) fcToolCachePath(spec core.RunnerSpec) string {
	img := f.toolCacheImage(spec)
	if img == "" {
		return ""
	}
	if f.cfg.Jailer {
		return "/toolcache.ext4"
	}
	return img
}

// jailChrootRoot returns the host path to a microVM's jail root, where its
// kernel, rootfs and API socket live: <base>/firecracker/<id>/root.
func jailChrootRoot(base, id string) string {
	return filepath.Join(base, "firecracker", id, "root")
}

// cgroupV2Mount is the standard unified cgroup v2 hierarchy mount point. When
// CgroupLimits are set the jailer places each microVM's cgroup under
// <mount>/<exec-file-basename>/<id>. It is a var so tests can redirect it.
var cgroupV2Mount = "/sys/fs/cgroup"

// jailCgroupDir returns the host path to a jailed microVM's cgroup, created by
// the jailer when CgroupLimits are set: <cgroupV2Mount>/<firecracker>/<id>. The
// jailer leaves the (empty) dir behind on exit, so firerunner reclaims it.
func jailCgroupDir(binary, id string) string {
	return filepath.Join(cgroupV2Mount, filepath.Base(binary), id)
}

// buildJailerArgs returns the jailer argv that launches one jailed Firecracker.
// The jailer chroots into <ChrootBase>/firecracker/<id>/root, drops to
// JailUID:JailGID and execs Firecracker (passing --id itself). Firecracker's API
// socket defaults to /run/firecracker.socket inside the jail. When nsPath is
// empty no --netns is passed and the jailed VMM stays in the host network
// namespace so the per-slot host tap devices attach unchanged; when set the
// jailer joins that named network namespace, where the per-slot tap lives. With
// cgroup-version 2 and no CgroupLimits the jailer creates no cgroup (systemd
// already scopes the service); each CgroupLimit adds a --cgroup flag so the
// jailer places the VMM in its own cgroup and applies the limit. --new-pid-ns
// puts the VMM in its own PID namespace so it cannot see or signal host
// processes (and dies with the namespace). It is a pure function so the argv can
// be asserted in tests.
func buildJailerArgs(cfg FirecrackerConfig, id, nsPath string) []string {
	args := []string{
		"--id", id,
		"--exec-file", cfg.Binary,
		"--uid", strconv.Itoa(cfg.JailUID),
		"--gid", strconv.Itoa(cfg.JailGID),
		"--cgroup-version", "2",
		"--chroot-base-dir", cfg.ChrootBase,
		"--new-pid-ns",
	}
	if nsPath != "" {
		args = append(args, "--netns", nsPath)
	}
	for _, c := range cfg.CgroupLimits {
		args = append(args, "--cgroup", c)
	}
	return args
}

// prepare stages a microVM's rootfs and returns the command to launch it, the
// host path to its API socket, the kernel and rootfs paths as Firecracker will
// see them, and a cleanup that removes everything staged. When Jailer is set it
// builds the chroot and stages the kernel and rootfs inside it (owned by the
// jail user); otherwise it uses a flat per-job work dir and launches Firecracker
// directly. When NetNS is set (jailer only) the jailer is told to join the
// slot's network namespace via --netns.
func (f *Firecracker) prepare(ctx context.Context, name string, vnet vmNet, console io.Writer, spec core.RunnerSpec) (cmd *exec.Cmd, sock, kernelPath, rootfsPath string, cleanup func(), err error) {
	if f.cfg.Jailer {
		idDir := filepath.Join(f.cfg.ChrootBase, "firecracker", name)
		root := jailChrootRoot(f.cfg.ChrootBase, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, "", "", "", nil, fmt.Errorf("mkdir jail root: %w", err)
		}
		cleanup = func() {
			_ = os.RemoveAll(idDir)
			// The jailer leaves the microVM's (now empty) cgroup dir behind.
			if len(f.cfg.CgroupLimits) > 0 {
				_ = os.Remove(jailCgroupDir(f.cfg.Binary, name))
			}
		}
		kernel := filepath.Join(root, "vmlinux")
		rootfs := filepath.Join(root, "rootfs.ext4")
		if err := f.run(ctx, "cp", "--reflink=auto", f.cfg.KernelImage, kernel); err != nil {
			cleanup()
			return nil, "", "", "", nil, fmt.Errorf("stage kernel into jail: %w", err)
		}
		if err := f.run(ctx, "cp", "--reflink=auto", spec.RootFS, rootfs); err != nil {
			cleanup()
			return nil, "", "", "", nil, fmt.Errorf("reflink golden rootfs into jail: %w", err)
		}
		// The dropped-privilege Firecracker must own the writable rootfs and be
		// able to read the kernel; the jailer only chowns the chroot dir and the
		// /dev nodes it creates, not the files we stage.
		for _, p := range []string{kernel, rootfs} {
			if err := os.Chown(p, f.cfg.JailUID, f.cfg.JailGID); err != nil {
				cleanup()
				return nil, "", "", "", nil, fmt.Errorf("chown %s to jail user: %w", p, err)
			}
		}
		// Stage the tier's read-only tool cache into the jail so the chrooted VMM
		// can open it at /toolcache.ext4. reflink keeps this near-free on a
		// reflink-capable filesystem; the jail user needs read access.
		if img := f.toolCacheImage(spec); img != "" {
			tc := filepath.Join(root, "toolcache.ext4")
			if err := f.run(ctx, "cp", "--reflink=auto", img, tc); err != nil {
				cleanup()
				return nil, "", "", "", nil, fmt.Errorf("stage tool cache into jail: %w", err)
			}
			if err := os.Chown(tc, f.cfg.JailUID, f.cfg.JailGID); err != nil {
				cleanup()
				return nil, "", "", "", nil, fmt.Errorf("chown tool cache to jail user: %w", err)
			}
		}
		var nsPath string
		if f.cfg.NetNS {
			nsPath = slotNetNS(vnet, f.cfg.NetBase, f.cfg.TapPrefix).nsPath
		}
		sock = filepath.Join(root, "run", "firecracker.socket")
		cmd = exec.CommandContext(ctx, f.cfg.JailerBin, buildJailerArgs(f.cfg, name, nsPath)...)
		cmd.Stdout, cmd.Stderr = console, console
		cmd.Env = sanitizedEnv()
		return cmd, sock, "/vmlinux", "/rootfs.ext4", cleanup, nil
	}

	jobDir := filepath.Join(f.cfg.WorkDir, name)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return nil, "", "", "", nil, fmt.Errorf("mkdir job dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(jobDir) }
	rootfs := filepath.Join(jobDir, "rootfs.ext4")
	if err := f.run(ctx, "cp", "--reflink=auto", spec.RootFS, rootfs); err != nil {
		cleanup()
		return nil, "", "", "", nil, fmt.Errorf("reflink golden rootfs: %w", err)
	}
	sock = filepath.Join(jobDir, "fc.sock")
	cmd = exec.CommandContext(ctx, f.cfg.Binary, "--api-sock", sock, "--id", name)
	cmd.Stdout, cmd.Stderr = console, console
	cmd.Env = sanitizedEnv()
	return cmd, sock, f.cfg.KernelImage, rootfs, cleanup, nil
}

// sanitizedEnv returns a minimal environment for the Firecracker/jailer child so
// the firerunner daemon's own secrets (FR_TOKEN, FR_APP_PRIVATE_KEY, …) never
// leak into the VMM process environment, which is world-readable via
// /proc/<pid>/environ. Only PATH (needed to resolve helper binaries) plus HOME
// and TMPDIR are forwarded.
func sanitizedEnv() []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{"PATH=" + path}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// CleanupStale reclaims host resources left behind by microVMs from a previous,
// uncleanly terminated run (SIGKILL, OOM, power loss). It is safe to call once at
// startup because the process owns no microVMs yet, so any matching tap device or
// job dir is definitively stale. Orphaned firecracker processes are reaped by the
// systemd cgroup on crash; this closes what survives that: per-slot tap devices
// (a host-namespace resource whose lifetime isn't bound to the VMM, so leaks here
// eventually exhaust the slot space) and per-job work dirs (reflink rootfs clones
// and sockets whose teardown defer never ran).
func (f *Firecracker) CleanupStale(ctx context.Context) {
	for slot := 0; slot < f.cfg.MaxVMs; slot++ {
		vnet := slotNet(slot, f.cfg.TapPrefix, f.cfg.NetBase)
		if f.cfg.NetNS {
			// The tap lives inside the per-slot netns; a leftover namespace is
			// the definitive marker of a stale slot. Deleting it cascades to the
			// tap, the veth and the guest route.
			if _, err := os.Stat(slotNetNS(vnet, f.cfg.NetBase, f.cfg.TapPrefix).nsPath); err != nil {
				continue // not present
			}
			if err := f.teardownNet(ctx, vnet); err != nil {
				f.log.Warn("remove stale netns", "slot", slot, "err", err)
				continue
			}
			f.log.Info("removed stale netns", "slot", slot)
			continue
		}
		if _, err := net.InterfaceByName(vnet.tap); err != nil {
			continue // not present
		}
		if err := f.teardownNet(ctx, vnet); err != nil {
			f.log.Warn("remove stale tap", "tap", vnet.tap, "err", err)
			continue
		}
		f.log.Info("removed stale tap", "tap", vnet.tap)
	}

	// A job dir is uniquely identified by its firecracker API socket, so globbing
	// for fc.sock avoids ever touching the nft rulesets or logs dir in WorkDir.
	socks, _ := filepath.Glob(filepath.Join(f.cfg.WorkDir, "*", "fc.sock"))
	for _, sock := range socks {
		dir := filepath.Dir(sock)
		if err := os.RemoveAll(dir); err != nil {
			f.log.Warn("remove stale job dir", "dir", dir, "err", err)
			continue
		}
		f.log.Info("removed stale job dir", "dir", dir)
	}

	// Under the jailer each microVM leaves a chroot at <base>/firecracker/<id>;
	// any that survive startup are stale (the process owns no microVMs yet).
	if f.cfg.Jailer {
		jails, _ := filepath.Glob(filepath.Join(f.cfg.ChrootBase, "firecracker", "*"))
		for _, dir := range jails {
			if err := os.RemoveAll(dir); err != nil {
				f.log.Warn("remove stale jail dir", "dir", dir, "err", err)
				continue
			}
			f.log.Info("removed stale jail dir", "dir", dir)
		}
	}

	// With cgroup limits each microVM also leaves an (empty) cgroup at
	// <cgroupV2Mount>/<firecracker>/<id> the jailer never reclaims; rmdir any
	// that survive startup. The parent cgroup dir also holds controller pseudo-
	// files (memory.max, pids.max, ...), so only per-microVM subdirectories are
	// swept.
	if f.cfg.Jailer && len(f.cfg.CgroupLimits) > 0 {
		cgs, _ := filepath.Glob(filepath.Join(cgroupV2Mount, filepath.Base(f.cfg.Binary), "*"))
		for _, dir := range cgs {
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				continue
			}
			if err := os.Remove(dir); err != nil {
				f.log.Warn("remove stale cgroup dir", "dir", dir, "err", err)
				continue
			}
			f.log.Info("removed stale cgroup dir", "dir", dir)
		}
	}
}

// setupNet provisions a microVM's networking: either a host-namespace tap
// (default) or, when NetNS is set, a private per-slot network namespace holding
// the tap with a veth uplink to the host.
func (f *Firecracker) setupNet(ctx context.Context, n vmNet) error {
	cmds := tapUpCommands(n)
	if f.cfg.NetNS {
		cmds = netnsUpCommands(n, slotNetNS(n, f.cfg.NetBase, f.cfg.TapPrefix), f.cfg.JailUID, f.cfg.JailGID)
	}
	for _, c := range cmds {
		if err := f.run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// teardownNet reverses setupNet. In NetNS mode deleting the namespace cascades
// to the tap and the netns veth end (its host peer is auto-removed, taking the
// guest route with it); the host-side link and route deletions are best-effort
// belt-and-suspenders. Otherwise it removes the host tap.
func (f *Firecracker) teardownNet(ctx context.Context, n vmNet) error {
	if f.cfg.NetNS {
		s := slotNetNS(n, f.cfg.NetBase, f.cfg.TapPrefix)
		err := f.run(ctx, "ip", "netns", "del", s.ns)
		_ = f.run(ctx, "ip", "link", "del", s.hostVeth)
		_ = f.run(ctx, "ip", "route", "del", n.guestIP+"/32")
		return err
	}
	for _, c := range tapDownCommands(n.tap) {
		if err := f.run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// --- small stdlib helpers ---

func execRun(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(out))
	}
	return nil
}

func execRunOut(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

func newUnixClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func putJSON(ctx context.Context, cl *http.Client, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("firecracker returned %s", resp.Status)
	}
	return nil
}

func waitForSocket(ctx context.Context, sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(sock); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s did not appear within %s", sock, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// logWriter adapts firecracker's serial/stdio output to structured logs.
type logWriter struct {
	log    *slog.Logger
	runner string
	stream string
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.Debug("firecracker", "runner", w.runner, "stream", w.stream, "line", string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}
