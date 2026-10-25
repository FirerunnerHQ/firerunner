// Package config loads firerunner configuration from flags and environment.
// It intentionally uses only the standard library (flag/os) to keep the
// dependency surface minimal.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/schedule"
)

// Config is the fully-resolved firerunner configuration.
type Config struct {
	// GitHub scale-set registration.
	URL          string
	ScaleSetName string
	RunnerGroup  string
	Labels       []string
	MaxRunners   int
	MinRunners   int

	// Auth: either a PAT or a GitHub App (App preferred, per GitHub guidance).
	Token         string
	AppClientID   string
	AppInstallID  int64
	AppPrivateKey string

	// Runner microVM shape.
	VCPU   int
	MemMiB int

	// Tiers is an optional catalog of runner tiers served from this one
	// process. Each tier is its own GitHub scale set (developers select it with
	// runs-on: <name>) with its own microVM shape and golden image, while all
	// tiers share the host provisioner: kernel, tool cache, network and the
	// MaxRunners slot budget. When empty, EffectiveTiers derives a single tier
	// from the top-level scalar fields, so a zero-config deployment is unchanged.
	Tiers     []Tier
	TiersPath string

	CacheJobsDir string
	Firecracker  provisioner.FirecrackerConfig

	// JITAddr is the VPC IP:port where EC2 runner instances fetch their JIT
	// config; required when any tier uses the ec2 provisioner. JITTTL bounds
	// how long a config waits to be fetched.
	JITAddr string
	JITTTL  time.Duration
	// JITHost, when set, is the name instances are told to fetch from
	// instead of JITAddr's IP: a stable private DNS name that follows a
	// replaced control VM. The port stays JITAddr's.
	JITHost string
	// EC2Reaper is "on", "dry-run" or "off": whether instances and spot
	// requests this install left behind are cleaned up.
	EC2Reaper string
	// RunnerSweep is "on", "dry-run" or "off": whether offline runners this
	// install does not track are removed from its scale sets.
	RunnerSweep string
	// Owner names this install: the firerunner:owner tag on everything it
	// launches, and its scale-set session owner. Empty means the host name.
	Owner string
	// AWSRegion is the region EC2 tiers launch in; empty uses the SDK's
	// sources (env, shared config, then IMDS on the control VM).
	AWSRegion string

	LogLevel  string
	LogFormat string

	// offline skips checks that depend on this host's files (image paths), for
	// validating a catalog away from the machine that will run it.
	offline bool
}

// Provisioner names a tier may select.
const (
	ProvisionerFirecracker = "firecracker"
	ProvisionerEC2         = "ec2"
)

// EC2 market policies.
const (
	MarketSpot               = "spot"
	MarketSpotThenOnDemand   = "spot-then-on-demand"
	MarketOnDemand           = "on-demand"
	defaultEC2RootVolumeType = "gp3"
	ec2AMIIDPrefix           = "ami-"
)

// Tier is one runner tier: a named scale set with its own runner shape and
// warm/max bounds. Developers pick a tier by its name via runs-on; the operator
// defines the catalog.
//
// A Firecracker tier (the default) sets vcpu/mem, the golden image and
// (optionally) the tool cache; the kernel, network and --max-runners slot
// budget are shared by all Firecracker tiers in the process. An EC2 tier sets
// the ec2 block instead and launches one instance per job; it draws on no host
// resources, so it is bounded only by its own max.
type Tier struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels,omitempty"`
	// Provisioner selects how this tier's runners are launched: "firecracker"
	// (the default when empty) or "ec2".
	Provisioner string `json:"provisioner,omitempty"`
	VCPU        int    `json:"vcpu,omitempty"`
	MemMiB      int    `json:"mem_mib,omitempty"`
	Golden      string `json:"golden,omitempty"`
	// ToolCache optionally binds a read-only "hostedtoolcache" ext4 image to
	// this tier, overriding the process-global --toolcache/FR_TOOLCACHE. Empty
	// falls back to the global default. This lets a lean tier attach a drive
	// while a fat tier keeps its baked-in cache.
	ToolCache string `json:"toolcache,omitempty"`
	// EC2 configures an EC2 tier; it must be set exactly when Provisioner is
	// "ec2".
	EC2 *EC2Tier `json:"ec2,omitempty"`
	Min int      `json:"min"`
	Max int      `json:"max"`
	// Schedule, when set, replaces Min (and ec2.pool) during its windows;
	// outside them the tier keeps Min and ec2.pool.
	Schedule *schedule.Schedule `json:"schedule,omitempty"`
}

// BaseSizes are the tier's sizes outside every schedule window.
func (t Tier) BaseSizes() schedule.Sizes {
	s := schedule.Sizes{Min: t.Min}
	if t.EC2 != nil {
		s.Pool = t.EC2.Pool
	}
	return s
}

// SizesAt returns the tier's hot-runner and pool sizes at a moment.
func (t Tier) SizesAt(now time.Time) schedule.Sizes {
	if t.Schedule == nil {
		return t.BaseSizes()
	}
	return t.Schedule.At(now, t.BaseSizes())
}

// MaxPool is the largest stopped pool the tier ever keeps; 0 means it keeps
// none.
func (t Tier) MaxPool() int {
	n := t.BaseSizes().Pool
	if t.Schedule != nil {
		n = max(n, t.Schedule.MaxPool())
	}
	return n
}

// MaxMin is the most hot runners the tier ever keeps.
func (t Tier) MaxMin() int {
	n := t.Min
	if t.Schedule != nil {
		n = max(n, t.Schedule.MaxMin())
	}
	return n
}

// EC2Tier is the launch template of an EC2 tier: one ephemeral instance per
// job, shaped by the first instance type that has capacity.
type EC2Tier struct {
	// InstanceTypes is tried in order when capacity runs out, e.g.
	// ["c8a.xlarge", "c7a.xlarge", "m8azn.xlarge"].
	InstanceTypes []string `json:"instance_types"`
	// AMI is an image ID ("ami-...") or the name of an SSM parameter that
	// holds one, resolved at launch time so an image rebuild needs no config
	// change.
	AMI string `json:"ami"`
	// Subnets are tried in order on insufficient capacity; one per AZ spreads
	// launches across zones.
	Subnets        []string `json:"subnets"`
	SecurityGroups []string `json:"security_groups"`
	// Market is "spot", "spot-then-on-demand" or "on-demand".
	Market string `json:"market"`
	// RootVolumeGiB sizes the root EBS volume; 0 keeps the AMI's size.
	RootVolumeGiB int `json:"root_volume_gib,omitempty"`
	// RootVolumeType is the root EBS volume type; empty means gp3.
	RootVolumeType string `json:"root_volume_type,omitempty"`
	// Pool is how many pre-warmed, stopped instances to keep ready. A job
	// starts one in seconds instead of booting a new instance; spot pools use
	// persistent requests and are still billed at the spot price. 0 disables
	// the pool, and every job cold-launches.
	Pool int `json:"pool,omitempty"`
}

// AMIFromSSM reports whether AMI names an SSM parameter rather than an image ID.
func (e EC2Tier) AMIFromSSM() bool { return !strings.HasPrefix(e.AMI, ec2AMIIDPrefix) }

// VolumeType returns the root volume type, defaulting to gp3.
func (e EC2Tier) VolumeType() string {
	if e.RootVolumeType == "" {
		return defaultEC2RootVolumeType
	}
	return e.RootVolumeType
}

// ProvisionerName returns the tier's provisioner, defaulting to firecracker.
func (t Tier) ProvisionerName() string {
	if t.Provisioner == "" {
		return ProvisionerFirecracker
	}
	return t.Provisioner
}

// IsEC2 reports whether the tier launches EC2 instances.
func (t Tier) IsEC2() bool { return t.ProvisionerName() == ProvisionerEC2 }

// Spec returns the microVM spec for the tier.
func (t Tier) Spec() core.RunnerSpec {
	return core.RunnerSpec{Labels: t.Labels, VCPU: t.VCPU, MemMiB: t.MemMiB, RootFS: t.Golden, ToolCache: t.ToolCache}
}

// EffectiveTiers returns the configured tier catalog, or a single tier
// synthesized from the top-level scalar config when no catalog is set. This
// keeps the zero-config, single-tier deployment behaving exactly as before.
func (c *Config) EffectiveTiers() []Tier {
	if len(c.Tiers) > 0 {
		return c.Tiers
	}
	return []Tier{{
		Name:   c.ScaleSetName,
		Labels: c.Labels,
		VCPU:   c.VCPU,
		MemMiB: c.MemMiB,
		Golden: c.Firecracker.GoldenRootFS,
		Min:    c.MinRunners,
		Max:    c.MaxRunners,
	}}
}

// RunnerSpec derives the microVM spec from the config.
func (c *Config) RunnerSpec() core.RunnerSpec {
	return core.RunnerSpec{
		Labels: c.Labels,
		VCPU:   c.VCPU,
		MemMiB: c.MemMiB,
		RootFS: c.Firecracker.GoldenRootFS,
	}
}

// FromFlags parses configuration from the given argument list, falling back to
// FR_* environment variables. It validates required fields.
func FromFlags(args []string) (*Config, error) {
	c, err := Parse(args)
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// flagVals holds the flag values Parse turns into Config fields after parsing.
type flagVals struct {
	labels, jailerCgroup, egress, dnsServers string
	metaRefresh                              time.Duration
}

// FlagSet returns the daemon's flags, registered exactly as Parse registers
// them, with their defaults read from the current environment. It configures
// nothing: it lists the flags, for help output and for tests that walk them.
func FlagSet() *flag.FlagSet {
	return newFlagSet(&Config{}, &flagVals{}, &envReader{})
}

// newFlagSet registers every daemon flag, writing into c and v. Typed FR_*
// values that do not parse are collected in er.
func newFlagSet(c *Config, v *flagVals, er *envReader) *flag.FlagSet {
	fs := flag.NewFlagSet("firerunner", flag.ContinueOnError)

	fs.StringVar(&c.URL, "url", env("FR_URL", ""), "GitHub org or repo URL to register the scale set (required)")
	fs.StringVar(&c.ScaleSetName, "name", env("FR_NAME", "firerunner"), "scale set name; also the runs-on label")
	fs.StringVar(&c.RunnerGroup, "runner-group", env("FR_RUNNER_GROUP", "default"), "runner group name")
	fs.StringVar(&v.labels, "labels", env("FR_LABELS", ""), "extra comma-separated runs-on labels")
	fs.IntVar(&c.MaxRunners, "max-runners", er.int("FR_MAX_RUNNERS", 4), "max concurrent microVMs")
	fs.IntVar(&c.MinRunners, "min-runners", er.int("FR_MIN_RUNNERS", 0), "warm pool size: microVMs kept pre-booted and registered so jobs start without a cold boot (0 = on-demand; higher = faster pickup, more idle RAM)")

	// Credentials are not flag defaults: the flag package prints defaults in
	// -h output and on every parse error, which would echo FR_TOKEN and
	// FR_APP_PRIVATE_KEY to stderr. They fall back to the env after parsing.
	fs.StringVar(&c.Token, "token", "", "GitHub PAT (use a GitHub App in production); default FR_TOKEN")
	fs.StringVar(&c.AppClientID, "app-client-id", env("FR_APP_CLIENT_ID", ""), "GitHub App client id")
	fs.Int64Var(&c.AppInstallID, "app-installation-id", int64(er.int("FR_APP_INSTALLATION_ID", 0)), "GitHub App installation id")
	fs.StringVar(&c.AppPrivateKey, "app-private-key", "", "GitHub App private key (PEM path or contents); default FR_APP_PRIVATE_KEY")

	fs.IntVar(&c.VCPU, "vcpu", er.int("FR_VCPU", 4), "vCPUs per microVM")
	fs.IntVar(&c.MemMiB, "mem-mib", er.int("FR_MEM_MIB", 8192), "guest memory (MiB) per microVM")

	fs.StringVar(&c.TiersPath, "tiers", env("FR_TIERS", ""), "path to a JSON tier catalog (array of {name,labels,provisioner,min,max} plus {vcpu,mem_mib,golden,toolcache} for a firecracker tier or an ec2 block for an ec2 tier). When set, firerunner serves every tier from this one process and developers pick one with runs-on: <name>; --max-runners is the slot budget shared by the firecracker tiers. When unset, a single firecracker tier is derived from --name/--vcpu/--mem-mib/--golden.")

	fs.StringVar(&c.Firecracker.Binary, "firecracker-bin", env("FR_FIRECRACKER_BIN", "firecracker"), "path to firecracker binary")
	fs.StringVar(&c.Firecracker.KernelImage, "kernel", env("FR_KERNEL", ""), "path to guest kernel (vmlinux) (required)")
	fs.StringVar(&c.Firecracker.GoldenRootFS, "golden", env("FR_GOLDEN", ""), "path to immutable golden rootfs (required)")
	fs.StringVar(&c.Firecracker.WorkDir, "workdir", env("FR_WORKDIR", "/var/tmp/firerunner"), "reflink-capable work dir for per-job rootfs")
	fs.StringVar(&c.Firecracker.TapPrefix, "tap-prefix", env("FR_TAP_PREFIX", "fr"), "prefix for per-job tap devices; must be unique per instance when several firerunners share a host")
	fs.IntVar(&c.Firecracker.NetBase, "net-base", er.int("FR_NET_BASE", 16), "second octet of the per-microVM /16 (172.<base>.x); must differ per instance sharing a host")
	fs.StringVar(&c.Firecracker.NFTTable, "nft-table", env("FR_NFT_TABLE", "firerunner"), "nftables table name; must be unique per instance when several firerunners share a host")
	fs.StringVar(&c.Firecracker.BootArgs, "boot-args", env("FR_BOOT_ARGS", ""), "kernel command line (default keeps reboot=k)")
	fs.StringVar(&c.Firecracker.ExtIface, "ext-iface", env("FR_EXT_IFACE", ""), "host external interface for microVM egress NAT (required)")
	fs.StringVar(&c.Firecracker.LogDir, "log-dir", env("FR_LOG_DIR", ""), "directory for per-runner console logs (off-VM log forwarding)")

	fs.BoolVar(&c.Firecracker.Jailer, "jailer", er.boolean("FR_JAILER", false), "run each microVM under the Firecracker jailer (chroot + PID ns + privilege drop); opt-in, requires a root launcher and --jail-uid/--jail-gid")
	fs.StringVar(&c.Firecracker.JailerBin, "jailer-bin", env("FR_JAILER_BIN", "jailer"), "path to the jailer binary (must match the firecracker version)")
	fs.StringVar(&c.Firecracker.ChrootBase, "chroot-base", env("FR_CHROOT_BASE", "/srv/jailer"), "jailer chroot base dir")
	fs.IntVar(&c.Firecracker.JailUID, "jail-uid", er.int("FR_JAIL_UID", 0), "uid the jailer drops Firecracker to (required with --jailer)")
	fs.IntVar(&c.Firecracker.JailGID, "jail-gid", er.int("FR_JAIL_GID", 0), "gid the jailer drops Firecracker to (required with --jailer)")
	fs.StringVar(&v.jailerCgroup, "jailer-cgroup", env("FR_JAILER_CGROUP", ""), "semicolon-separated cgroup v2 limits applied to each microVM via the jailer, each <file>=<value> (e.g. \"memory.max=2147483648;cpu.max=200000;pids.max=512\"); requires --jailer")
	fs.BoolVar(&c.Firecracker.NetNS, "netns", er.boolean("FR_NETNS", false), "run each microVM in its own network namespace (tap lives in the netns, veth uplink to the host); strongest network isolation, requires --jailer")
	fs.StringVar(&c.Firecracker.ToolCacheImage, "toolcache", env("FR_TOOLCACHE", ""), "path to a read-only ext4 image (labelled \"hostedtoolcache\", mirroring GitHub's tool-cache layout) attached to every microVM so setup-* actions hit a pre-seeded cache instead of downloading; opt-in accelerator, jobs fall back to downloading when unset")
	fs.StringVar(&c.CacheJobsDir, "cache-jobs-dir", env("FR_CACHE_JOBS_DIR", "/run/firerunner/jobs"), "private job identity directory shared with the local cache server; unique per daemon")
	fs.IntVar(&c.Firecracker.CachePort, "cache-port", er.int("FR_CACHE_PORT", 0), "TCP port of a firerunner cache-server reachable on each microVM's host gateway; when set, microVMs use it for actions/cache (dependency caching) instead of GitHub's hosted cache. Opt-in and off by default; the guest builds the URL from its default gateway and this port")
	fs.StringVar(&c.Firecracker.CacheURL, "cache-url", env("FR_CACHE_URL", ""), "explicit base URL of a firerunner cache-server on this host (e.g. http://host:8099); overrides --cache-port. The server must share --cache-jobs-dir and see guest source IPs unchanged, so a remote server behind SNAT or a proxy is not supported. Opt-in and off by default")
	fs.DurationVar(&c.Firecracker.MaxVMLifetime, "max-vm-lifetime", er.duration("FR_MAX_VM_LIFETIME", 6*time.Hour), "host-side backstop: kill a microVM still running after this long to reclaim its slot (a healthy VM self-destructs when its job ends, so this only fires on a stuck VM). Keep it larger than any real job; 0 disables it")
	fs.DurationVar(&c.Firecracker.ConnectTimeout, "vm-connect-timeout", er.duration("FR_VM_CONNECT_TIMEOUT", 5*time.Minute), "kill a booted microVM whose runner has not registered with GitHub (console shows neither \"Listening for Jobs\" nor \"Running job:\") within this long, freeing its slot and backing off replenishment; catches VMs wedged by lost DNS/egress or a rejected JIT config that would otherwise hold a slot until --max-vm-lifetime. 0 disables it")

	fs.StringVar(&c.JITAddr, "jit-addr", env("FR_JIT_ADDR", ""), "VPC IP:port where EC2 runner instances fetch their JIT config, served once to each instance's private IP so no secret goes into user-data (e.g. 10.0.0.5:8098); required with an ec2 tier. Bind the control VM's VPC address, never a public one")
	fs.StringVar(&c.AWSRegion, "aws-region", env("FR_AWS_REGION", ""), "AWS region for ec2 tiers; defaults to AWS_REGION/shared config, then the control VM's own region from IMDS")
	fs.StringVar(&c.JITHost, "jit-host", env("FR_JIT_HOST", ""), "DNS name EC2 runner instances use for the JIT endpoint instead of --jit-addr's IP (e.g. control.firerunner.internal), so a replaced control VM keeps one address; it must resolve to --jit-addr's IP inside the VPC. The port is --jit-addr's")
	fs.DurationVar(&c.JITTTL, "jit-ttl", er.duration("FR_JIT_TTL", 5*time.Minute), "how long an EC2 instance's JIT config waits to be fetched before it expires")
	fs.StringVar(&c.Owner, "owner", env("FR_OWNER", ""), "name of this install: tagged as firerunner:owner on every instance it launches, so its pool and reaper find them again. Defaults to the host name; set a stable value when the control VM can be replaced. Setting it also keeps the tiers' scale sets registered across restarts (without it, a scale set this process created is deleted when it stops)")
	fs.StringVar(&c.RunnerSweep, "runner-sweep", env("FR_RUNNER_SWEEP", "on"), "every 5 minutes, remove offline runners in this install's scale sets that it does not track (left by a launch that died before connecting, or by an earlier process), once two sweeps in a row find them: on, dry-run (log only), or off")
	fs.StringVar(&c.EC2Reaper, "ec2-reaper", env("FR_EC2_REAPER", "on"), "clean up EC2 instances and spot requests this install left behind (after a crash or restart, including pool instances left mid-claim): on, dry-run (log only), or off (nothing cleans them up)")

	fs.StringVar(&v.egress, "egress", env("FR_EGRESS", "api,actions,git,dns,packages,ntp"), "comma-separated egress allowlist: GitHub /meta categories (api,actions,git,packages) plus dns,ntp; or 'open' for no allowlist")
	fs.StringVar(&v.dnsServers, "dns-servers", env("FR_DNS_SERVERS", "1.1.1.1,8.8.8.8"), "comma-separated resolver IPs guests may reach")
	fs.DurationVar(&v.metaRefresh, "meta-refresh", er.duration("FR_META_REFRESH", 24*time.Hour), "how often to refresh GitHub /meta ranges (0 disables)")

	fs.StringVar(&c.LogLevel, "log-level", env("FR_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", env("FR_LOG_FORMAT", "text"), "log format: text or json")
	return fs
}

// Parse loads configuration exactly like FromFlags but without enforcing
// required-field validation, so diagnostic subcommands (status, doctor) can
// inspect and report on a partial or misconfigured deployment instead of
// refusing to start.
func Parse(args []string) (*Config, error) {
	c, v, er := &Config{}, &flagVals{}, &envReader{}
	fs := newFlagSet(c, v, er)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.Token == "" {
		c.Token = env("FR_TOKEN", "")
	}
	if c.AppPrivateKey == "" {
		c.AppPrivateKey = env("FR_APP_PRIVATE_KEY", "")
	}
	// Surface any malformed FR_* env values now, so a typo fails closed instead
	// of silently running with a default.
	if err := er.err(); err != nil {
		return nil, err
	}
	if v.labels != "" {
		for _, l := range strings.Split(v.labels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				c.Labels = append(c.Labels, l)
			}
		}
	}
	c.Firecracker.CgroupLimits = splitSemi(v.jailerCgroup)
	// The provisioner's per-VM network pool is bounded by the runner capacity.
	// With a tier catalog this is the slot budget shared across all tiers.
	c.Firecracker.MaxVMs = c.MaxRunners
	if c.TiersPath != "" {
		tiers, err := loadTiers(c.TiersPath)
		if err != nil {
			return nil, err
		}
		c.Tiers = tiers
	}
	c.Firecracker.Egress = provisioner.EgressConfig{
		Categories:      splitCSV(v.egress),
		DNSServers:      splitCSV(v.dnsServers),
		RefreshInterval: v.metaRefresh,
	}
	return c, nil
}

// CheckCatalog loads and validates a tier catalog on its own, with the
// per-tier rules the daemon applies, so an edit can be checked without
// credentials or the host that will run it: image paths are not checked for
// existence. maxRunners is the Firecracker slot budget the catalog will run
// under (--max-runners).
func CheckCatalog(path string, maxRunners int) ([]Tier, error) {
	tiers, err := loadTiers(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Tiers: tiers, MaxRunners: maxRunners, offline: true}
	if err := c.validateTiers(); err != nil {
		return nil, err
	}
	return tiers, nil
}

// loadTiers reads and decodes a JSON tier-catalog file.
func loadTiers(path string) ([]Tier, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tiers file: %w", err)
	}
	var tiers []Tier
	if err := json.Unmarshal(b, &tiers); err != nil {
		return nil, fmt.Errorf("parse tiers file %s: %w", path, err)
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("tiers file %s defines no tiers", path)
	}
	return tiers, nil
}

// splitCSV splits a comma-separated list, trimming spaces and dropping empties.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitSemi splits a semicolon-separated list, trimming spaces and dropping
// empties. Semicolons (not commas) separate cgroup limits because cgroup values
// such as cpuset.cpus=0-3,5 legitimately contain commas.
func splitSemi(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// UsesFirecracker reports whether any effective tier boots Firecracker
// microVMs, and so needs the host kernel, network and slot budget. A catalog of
// only EC2 tiers runs on a host without KVM.
func (c *Config) UsesFirecracker() bool {
	for _, t := range c.EffectiveTiers() {
		if !t.IsEC2() {
			return true
		}
	}
	return false
}

func (c *Config) validate() error {
	fc := c.UsesFirecracker()
	switch {
	case c.URL == "":
		return fmt.Errorf("--url is required")
	case fc && c.Firecracker.KernelImage == "":
		return fmt.Errorf("--kernel is required")
	case c.Firecracker.GoldenRootFS == "" && len(c.Tiers) == 0:
		return fmt.Errorf("--golden is required (or configure a tier catalog with --tiers)")
	case fc && c.Firecracker.ExtIface == "":
		return fmt.Errorf("--ext-iface is required for microVM egress")
	case c.MaxRunners < 1:
		return fmt.Errorf("--max-runners must be >= 1")
	case c.VCPU < 1:
		return fmt.Errorf("--vcpu must be >= 1")
	case c.MemMiB < 128:
		return fmt.Errorf("--mem-mib must be >= 128")
	case c.Token == "" && c.AppClientID == "":
		return fmt.Errorf("provide --token or GitHub App credentials")
	case c.Token == "" && c.AppInstallID == 0:
		return fmt.Errorf("--app-installation-id is required with --app-client-id (or set --token)")
	case c.Firecracker.Jailer && (c.Firecracker.JailUID < 1 || c.Firecracker.JailGID < 1):
		return fmt.Errorf("--jail-uid and --jail-gid (>0) are required when --jailer is set")
	case len(c.Firecracker.CgroupLimits) > 0 && !c.Firecracker.Jailer:
		return fmt.Errorf("--jailer-cgroup requires --jailer")
	case c.Firecracker.NetNS && !c.Firecracker.Jailer:
		return fmt.Errorf("--netns requires --jailer")
	case c.Firecracker.NetNS && c.Firecracker.CachePort != 0 && c.Firecracker.CacheURL == "":
		return fmt.Errorf("--cache-port cannot be reached from inside --netns (the guest gateway lives in the namespace, not the host); use --cache-url with a host address the guest reaches without SNAT instead")
	}
	if len(c.Tiers) > 0 {
		if err := c.validateTiers(); err != nil {
			return err
		}
	}
	if err := c.validateJIT(); err != nil {
		return err
	}
	switch c.EC2Reaper {
	case "on", "dry-run", "off":
	default:
		return fmt.Errorf("--ec2-reaper %q must be on, dry-run or off", c.EC2Reaper)
	}
	switch c.RunnerSweep {
	case "on", "dry-run", "off":
	default:
		return fmt.Errorf("--runner-sweep %q must be on, dry-run or off", c.RunnerSweep)
	}
	if o := c.Owner; o != "" && (len(o) > 128 || strings.HasPrefix(strings.ToLower(o), "aws:") || strings.TrimSpace(o) != o) {
		return fmt.Errorf("--owner %q must be at most 128 characters, not start with aws:, and have no surrounding spaces", o)
	}
	if c.Firecracker.ToolCacheImage != "" {
		if _, err := os.Stat(c.Firecracker.ToolCacheImage); err != nil {
			return fmt.Errorf("--toolcache image %q not accessible: %w", c.Firecracker.ToolCacheImage, err)
		}
	}
	if c.Firecracker.CachePort != 0 && (c.Firecracker.CachePort < 1 || c.Firecracker.CachePort > 65535) {
		return fmt.Errorf("--cache-port %d out of range (1-65535)", c.Firecracker.CachePort)
	}
	if p := c.Firecracker.TapPrefix; p != "" {
		// Interface names cap at 15 chars and firerunner appends up to 4
		// ("v<slot>h" for the veth), so bound the prefix at 11. Letters only:
		// a digit-free prefix plus the always-numeric slot means two instances
		// with different prefixes can never derive the same tap/veth name.
		if len(p) > 11 {
			return fmt.Errorf("--tap-prefix %q too long (max 11 chars)", p)
		}
		for _, r := range p {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return fmt.Errorf("--tap-prefix %q must be letters only ([a-zA-Z]); digits or symbols risk interface-name collisions between instances sharing a host", p)
			}
		}
	}
	if c.Firecracker.MaxVMLifetime != 0 && c.Firecracker.MaxVMLifetime < time.Minute {
		return fmt.Errorf("--max-vm-lifetime %s is too small; use 0 to disable or a value >= 1m so real jobs are never truncated", c.Firecracker.MaxVMLifetime)
	}
	if c.Firecracker.ConnectTimeout != 0 && c.Firecracker.ConnectTimeout < 30*time.Second {
		return fmt.Errorf("--vm-connect-timeout %s is too small; use 0 to disable or a value >= 30s so a slow but healthy boot is never killed", c.Firecracker.ConnectTimeout)
	}
	if c.Firecracker.ConnectTimeout != 0 && c.Firecracker.MaxVMLifetime != 0 && c.Firecracker.ConnectTimeout >= c.Firecracker.MaxVMLifetime {
		return fmt.Errorf("--vm-connect-timeout %s must be shorter than --max-vm-lifetime %s", c.Firecracker.ConnectTimeout, c.Firecracker.MaxVMLifetime)
	}
	if (c.Firecracker.CachePort != 0 || c.Firecracker.CacheURL != "") && !filepath.IsAbs(c.CacheJobsDir) {
		return fmt.Errorf("--cache-jobs-dir must be an absolute path")
	}
	if c.Firecracker.CacheURL != "" {
		u, err := url.Parse(c.Firecracker.CacheURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("--cache-url %q must be an absolute http(s) URL", c.Firecracker.CacheURL)
		}
	}
	for _, l := range c.Firecracker.CgroupLimits {
		if !strings.Contains(l, "=") {
			return fmt.Errorf("--jailer-cgroup entry %q must be <file>=<value>", l)
		}
	}
	return nil
}

// validateTiers checks a tier catalog: each tier must be well-formed and
// uniquely named, no Firecracker tier's max may exceed the shared slot budget
// (--max-runners) — or it could never reach that max — and the Firecracker
// tiers' warm pools must fit within the budget so they can all stay warm at
// once. EC2 tiers draw on no host slots, so only their own bounds apply.
func (c *Config) validateTiers() error {
	seen := make(map[string]bool, len(c.Tiers))
	totalMin := 0
	for i, t := range c.Tiers {
		switch {
		case t.Name == "":
			return fmt.Errorf("tier %d: name is required", i)
		case seen[t.Name]:
			return fmt.Errorf("tier %q: duplicate name", t.Name)
		case t.Max < 1:
			return fmt.Errorf("tier %q: max must be >= 1", t.Name)
		case t.Min < 0:
			return fmt.Errorf("tier %q: min must be >= 0", t.Name)
		case t.Min > t.Max:
			return fmt.Errorf("tier %q: min (%d) must not exceed max (%d)", t.Name, t.Min, t.Max)
		}
		seen[t.Name] = true
		if t.Schedule != nil {
			for _, w := range t.Schedule.Windows {
				switch {
				case w.Min != nil && *w.Min > t.Max:
					return fmt.Errorf("tier %q: schedule window %s %s: min (%d) must not exceed max (%d)", t.Name, w.Days, w.Hours, *w.Min, t.Max)
				case w.Pool != nil && *w.Pool > 0 && t.ProvisionerName() != ProvisionerEC2:
					return fmt.Errorf("tier %q: schedule window %s %s: pool needs an ec2 tier", t.Name, w.Days, w.Hours)
				}
			}
		}
		switch t.ProvisionerName() {
		case ProvisionerFirecracker:
			if err := c.validateFirecrackerTier(t); err != nil {
				return err
			}
			// Windows of different tiers may overlap: budget for the most.
			totalMin += t.MaxMin()
		case ProvisionerEC2:
			if err := validateEC2Tier(t); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tier %q: unknown provisioner %q (want %q or %q)", t.Name, t.Provisioner, ProvisionerFirecracker, ProvisionerEC2)
		}
	}
	if totalMin > c.MaxRunners {
		return fmt.Errorf("tiers' warm pools sum to %d but --max-runners (the shared slot budget) is %d", totalMin, c.MaxRunners)
	}
	return nil
}

func (c *Config) validateFirecrackerTier(t Tier) error {
	switch {
	case t.EC2 != nil:
		return fmt.Errorf("tier %q: an ec2 block needs \"provisioner\": %q", t.Name, ProvisionerEC2)
	case t.VCPU < 1:
		return fmt.Errorf("tier %q: vcpu must be >= 1", t.Name)
	case t.MemMiB < 128:
		return fmt.Errorf("tier %q: mem_mib must be >= 128", t.Name)
	case t.Golden == "":
		return fmt.Errorf("tier %q: golden is required", t.Name)
	case t.Max > c.MaxRunners:
		return fmt.Errorf("tier %q: max (%d) exceeds --max-runners (%d), the shared slot budget, so the tier can never reach its max", t.Name, t.Max, c.MaxRunners)
	}
	if t.ToolCache != "" && !c.offline {
		if _, err := os.Stat(t.ToolCache); err != nil {
			return fmt.Errorf("tier %q: toolcache image %q not accessible: %w", t.Name, t.ToolCache, err)
		}
	}
	return nil
}

// UsesEC2 reports whether any effective tier launches EC2 instances.
func (c *Config) UsesEC2() bool {
	for _, t := range c.EffectiveTiers() {
		if t.IsEC2() {
			return true
		}
	}
	return false
}

// JITEndpoint is the host:port instances are told to fetch their JIT config
// from: --jit-host with --jit-addr's port, or --jit-addr itself.
func (c *Config) JITEndpoint() string {
	if c.JITHost == "" {
		return c.JITAddr
	}
	ap, err := netip.ParseAddrPort(c.JITAddr)
	if err != nil {
		return c.JITAddr
	}
	return net.JoinHostPort(c.JITHost, strconv.Itoa(int(ap.Port())))
}

// validHostname reports whether s is a plain DNS name: dot-separated labels
// of letters, digits and inner hyphens.
func validHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return false
			}
		}
	}
	return true
}

// validateJIT requires a reachable JIT endpoint for EC2 tiers. Instances are
// told this address in user-data, so it must be a concrete unicast IP they can
// route to: not a wildcard, loopback or a hostname to resolve.
func (c *Config) validateJIT() error {
	if c.JITAddr == "" {
		if c.UsesEC2() {
			return fmt.Errorf("--jit-addr is required with an ec2 tier")
		}
		return nil
	}
	ap, err := netip.ParseAddrPort(c.JITAddr)
	if err != nil {
		return fmt.Errorf("--jit-addr %q must be <ip>:<port>: %w", c.JITAddr, err)
	}
	switch ip := ap.Addr().Unmap(); {
	case ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast():
		return fmt.Errorf("--jit-addr %q must be the control VM's VPC address, which instances can reach", c.JITAddr)
	case ap.Port() == 0:
		return fmt.Errorf("--jit-addr %q needs a non-zero port", c.JITAddr)
	}
	if c.JITHost != "" {
		if _, err := netip.ParseAddr(c.JITHost); err == nil || !validHostname(c.JITHost) {
			return fmt.Errorf("--jit-host %q must be a DNS name (the IP goes in --jit-addr)", c.JITHost)
		}
	}
	if c.JITTTL < 30*time.Second {
		return fmt.Errorf("--jit-ttl %s is too small; an instance needs time to boot before it asks (use >= 30s)", c.JITTTL)
	}
	return nil
}

// validateEC2Tier rejects Firecracker-only fields on an EC2 tier, so a tier
// never half-describes a microVM it will not boot, and checks the ec2 block.
func validateEC2Tier(t Tier) error {
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"vcpu", t.VCPU != 0},
		{"mem_mib", t.MemMiB != 0},
		{"golden", t.Golden != ""},
		{"toolcache", t.ToolCache != ""},
	} {
		if f.set {
			return fmt.Errorf("tier %q: %s is a firecracker field; an ec2 tier is shaped by ec2.instance_types", t.Name, f.name)
		}
	}
	e := t.EC2
	if e == nil {
		return fmt.Errorf("tier %q: provisioner %q needs an ec2 block", t.Name, ProvisionerEC2)
	}
	if err := nonEmptyUnique(e.InstanceTypes); err != nil {
		return fmt.Errorf("tier %q: ec2.instance_types: %w", t.Name, err)
	}
	if err := nonEmptyUnique(e.Subnets); err != nil {
		return fmt.Errorf("tier %q: ec2.subnets: %w", t.Name, err)
	}
	if err := nonEmptyUnique(e.SecurityGroups); err != nil {
		return fmt.Errorf("tier %q: ec2.security_groups: %w", t.Name, err)
	}
	switch {
	case strings.TrimSpace(e.AMI) == "" || e.AMI == ec2AMIIDPrefix:
		return fmt.Errorf("tier %q: ec2.ami is required (an image ID or an SSM parameter name)", t.Name)
	case e.Market != MarketSpot && e.Market != MarketSpotThenOnDemand && e.Market != MarketOnDemand:
		return fmt.Errorf("tier %q: ec2.market %q must be %q, %q or %q", t.Name, e.Market, MarketSpot, MarketSpotThenOnDemand, MarketOnDemand)
	case e.RootVolumeGiB < 0:
		return fmt.Errorf("tier %q: ec2.root_volume_gib must be >= 0", t.Name)
	case e.Pool < 0:
		return fmt.Errorf("tier %q: ec2.pool must be >= 0", t.Name)
	}
	// io1/io2 need a provisioned IOPS value, which the tier does not carry.
	switch e.VolumeType() {
	case "gp2", "gp3":
	default:
		return fmt.Errorf("tier %q: ec2.root_volume_type %q must be gp2 or gp3", t.Name, e.RootVolumeType)
	}
	return nil
}

// nonEmptyUnique requires at least one entry, with no blanks or repeats.
func nonEmptyUnique(vals []string) error {
	if len(vals) == 0 {
		return errors.New("at least one entry is required")
	}
	seen := make(map[string]bool, len(vals))
	for _, v := range vals {
		switch {
		case strings.TrimSpace(v) == "":
			return errors.New("entries must not be empty")
		case seen[v]:
			return fmt.Errorf("duplicate entry %q", v)
		}
		seen[v] = true
	}
	return nil
}

// NewLogger builds a slog.Logger from the level and format strings.
func NewLogger(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envReader parses typed environment variables while accumulating parse errors
// instead of silently falling back to the default. A malformed FR_* value is a
// misconfiguration the operator must see, not something to paper over — Parse
// surfaces any accumulated errors so firerunner fails closed rather than running
// with unintended defaults.
type envReader struct {
	errs []string
}

func (r *envReader) fail(key, val string, err error) {
	r.errs = append(r.errs, fmt.Sprintf("%s=%q: %v", key, val, err))
}

func (r *envReader) err() error {
	if len(r.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid environment configuration: %s", strings.Join(r.errs, "; "))
}

func (r *envReader) int(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			r.fail(key, v, err)
			return def
		}
		return n
	}
	return def
}

func (r *envReader) boolean(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			r.fail(key, v, err)
			return def
		}
		return b
	}
	return def
}

func (r *envReader) duration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			r.fail(key, v, err)
			return def
		}
		return d
	}
	return def
}
