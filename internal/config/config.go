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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/provisioner"
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

	LogLevel  string
	LogFormat string
}

// Tier is one runner tier: a named scale set with its own microVM shape and
// warm/max bounds. Developers pick a tier by its name via runs-on; the operator
// defines the catalog. vcpu/mem, the golden image and (optionally) the tool
// cache vary per tier; the kernel and network are shared by all tiers in the
// process.
type Tier struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels,omitempty"`
	VCPU   int      `json:"vcpu"`
	MemMiB int      `json:"mem_mib"`
	Golden string   `json:"golden"`
	// ToolCache optionally binds a read-only "hostedtoolcache" ext4 image to
	// this tier, overriding the process-global --toolcache/FR_TOOLCACHE. Empty
	// falls back to the global default. This lets a lean tier attach a drive
	// while a fat tier keeps its baked-in cache.
	ToolCache string `json:"toolcache,omitempty"`
	Min       int    `json:"min"`
	Max       int    `json:"max"`
}

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

// Parse loads configuration exactly like FromFlags but without enforcing
// required-field validation, so diagnostic subcommands (status, doctor) can
// inspect and report on a partial or misconfigured deployment instead of
// refusing to start.
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("firerunner", flag.ContinueOnError)
	c := &Config{}
	var labels string
	er := &envReader{}

	fs.StringVar(&c.URL, "url", env("FR_URL", ""), "GitHub org or repo URL to register the scale set (required)")
	fs.StringVar(&c.ScaleSetName, "name", env("FR_NAME", "firerunner"), "scale set name; also the runs-on label")
	fs.StringVar(&c.RunnerGroup, "runner-group", env("FR_RUNNER_GROUP", "default"), "runner group name")
	fs.StringVar(&labels, "labels", env("FR_LABELS", ""), "extra comma-separated runs-on labels")
	fs.IntVar(&c.MaxRunners, "max-runners", er.int("FR_MAX_RUNNERS", 4), "max concurrent microVMs")
	fs.IntVar(&c.MinRunners, "min-runners", er.int("FR_MIN_RUNNERS", 0), "warm pool size: microVMs kept pre-booted and registered so jobs start without a cold boot (0 = on-demand; higher = faster pickup, more idle RAM)")

	fs.StringVar(&c.Token, "token", env("FR_TOKEN", ""), "GitHub PAT (use a GitHub App in production)")
	fs.StringVar(&c.AppClientID, "app-client-id", env("FR_APP_CLIENT_ID", ""), "GitHub App client id")
	fs.Int64Var(&c.AppInstallID, "app-installation-id", int64(er.int("FR_APP_INSTALLATION_ID", 0)), "GitHub App installation id")
	fs.StringVar(&c.AppPrivateKey, "app-private-key", env("FR_APP_PRIVATE_KEY", ""), "GitHub App private key (PEM path or contents)")

	fs.IntVar(&c.VCPU, "vcpu", er.int("FR_VCPU", 4), "vCPUs per microVM")
	fs.IntVar(&c.MemMiB, "mem-mib", er.int("FR_MEM_MIB", 8192), "guest memory (MiB) per microVM")

	fs.StringVar(&c.TiersPath, "tiers", env("FR_TIERS", ""), "path to a JSON tier catalog (array of {name,vcpu,mem_mib,golden,toolcache,min,max,labels}). When set, firerunner serves every tier from this one process and developers pick one with runs-on: <name>; --max-runners is the shared slot budget. When unset, a single tier is derived from --name/--vcpu/--mem-mib/--golden.")

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
	var jailerCgroup string
	fs.StringVar(&jailerCgroup, "jailer-cgroup", env("FR_JAILER_CGROUP", ""), "semicolon-separated cgroup v2 limits applied to each microVM via the jailer, each <file>=<value> (e.g. \"memory.max=2147483648;cpu.max=200000;pids.max=512\"); requires --jailer")
	fs.BoolVar(&c.Firecracker.NetNS, "netns", er.boolean("FR_NETNS", false), "run each microVM in its own network namespace (tap lives in the netns, veth uplink to the host); strongest network isolation, requires --jailer")
	fs.StringVar(&c.Firecracker.ToolCacheImage, "toolcache", env("FR_TOOLCACHE", ""), "path to a read-only ext4 image (labelled \"hostedtoolcache\", mirroring GitHub's tool-cache layout) attached to every microVM so setup-* actions hit a pre-seeded cache instead of downloading; opt-in accelerator, jobs fall back to downloading when unset")
	fs.StringVar(&c.CacheJobsDir, "cache-jobs-dir", env("FR_CACHE_JOBS_DIR", "/run/firerunner/jobs"), "private job identity directory shared with the local cache server; unique per daemon")
	fs.IntVar(&c.Firecracker.CachePort, "cache-port", er.int("FR_CACHE_PORT", 0), "TCP port of a firerunner cache-server reachable on each microVM's host gateway; when set, microVMs use it for actions/cache (dependency caching) instead of GitHub's hosted cache. Opt-in and off by default; the guest builds the URL from its default gateway and this port")
	fs.StringVar(&c.Firecracker.CacheURL, "cache-url", env("FR_CACHE_URL", ""), "explicit base URL of a firerunner cache-server (e.g. http://cache.internal:8099); overrides --cache-port for deployments where the cache-server is not on the microVM's host gateway. Opt-in and off by default")
	fs.DurationVar(&c.Firecracker.MaxVMLifetime, "max-vm-lifetime", er.duration("FR_MAX_VM_LIFETIME", 6*time.Hour), "host-side backstop: kill a microVM still running after this long to reclaim its slot (a healthy VM self-destructs when its job ends, so this only fires on a stuck VM). Keep it larger than any real job; 0 disables it")
	fs.DurationVar(&c.Firecracker.ConnectTimeout, "vm-connect-timeout", er.duration("FR_VM_CONNECT_TIMEOUT", 5*time.Minute), "kill a booted microVM whose runner has not registered with GitHub (console shows neither \"Listening for Jobs\" nor \"Running job:\") within this long, freeing its slot and backing off replenishment; catches VMs wedged by lost DNS/egress or a rejected JIT config that would otherwise hold a slot until --max-vm-lifetime. 0 disables it")

	var egress, dnsServers string
	var metaRefresh time.Duration
	fs.StringVar(&egress, "egress", env("FR_EGRESS", "api,actions,git,dns,packages,ntp"), "comma-separated egress allowlist: GitHub /meta categories (api,actions,git,packages) plus dns,ntp; or 'open' for no allowlist")
	fs.StringVar(&dnsServers, "dns-servers", env("FR_DNS_SERVERS", "1.1.1.1,8.8.8.8"), "comma-separated resolver IPs guests may reach")
	fs.DurationVar(&metaRefresh, "meta-refresh", er.duration("FR_META_REFRESH", 24*time.Hour), "how often to refresh GitHub /meta ranges (0 disables)")

	fs.StringVar(&c.LogLevel, "log-level", env("FR_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", env("FR_LOG_FORMAT", "text"), "log format: text or json")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// Surface any malformed FR_* env values now, so a typo fails closed instead
	// of silently running with a default.
	if err := er.err(); err != nil {
		return nil, err
	}
	if labels != "" {
		for _, l := range strings.Split(labels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				c.Labels = append(c.Labels, l)
			}
		}
	}
	c.Firecracker.CgroupLimits = splitSemi(jailerCgroup)
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
		Categories:      splitCSV(egress),
		DNSServers:      splitCSV(dnsServers),
		RefreshInterval: metaRefresh,
	}
	return c, nil
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

func (c *Config) validate() error {
	switch {
	case c.URL == "":
		return fmt.Errorf("--url is required")
	case c.Firecracker.KernelImage == "":
		return fmt.Errorf("--kernel is required")
	case c.Firecracker.GoldenRootFS == "" && len(c.Tiers) == 0:
		return fmt.Errorf("--golden is required (or configure a tier catalog with --tiers)")
	case c.Firecracker.ExtIface == "":
		return fmt.Errorf("--ext-iface is required for microVM egress")
	case c.MaxRunners < 1:
		return fmt.Errorf("--max-runners must be >= 1")
	case c.VCPU < 1:
		return fmt.Errorf("--vcpu must be >= 1")
	case c.MemMiB < 128:
		return fmt.Errorf("--mem-mib must be >= 128")
	case c.Token == "" && c.AppClientID == "":
		return fmt.Errorf("provide --token or GitHub App credentials")
	case c.Firecracker.Jailer && (c.Firecracker.JailUID < 1 || c.Firecracker.JailGID < 1):
		return fmt.Errorf("--jail-uid and --jail-gid (>0) are required when --jailer is set")
	case len(c.Firecracker.CgroupLimits) > 0 && !c.Firecracker.Jailer:
		return fmt.Errorf("--jailer-cgroup requires --jailer")
	case c.Firecracker.NetNS && !c.Firecracker.Jailer:
		return fmt.Errorf("--netns requires --jailer")
	case c.Firecracker.NetNS && c.Firecracker.CachePort != 0 && c.Firecracker.CacheURL == "":
		return fmt.Errorf("--cache-port cannot be reached from inside --netns (the guest gateway lives in the namespace, not the host); use --cache-url with a routable address instead")
	}
	if len(c.Tiers) > 0 {
		if err := c.validateTiers(); err != nil {
			return err
		}
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
// uniquely named, no tier's max may exceed the shared slot budget
// (--max-runners) — or it could never reach that max — and the tiers' warm
// pools must fit within the budget so they can all stay warm at once.
func (c *Config) validateTiers() error {
	seen := make(map[string]bool, len(c.Tiers))
	totalMin := 0
	for i, t := range c.Tiers {
		switch {
		case t.Name == "":
			return fmt.Errorf("tier %d: name is required", i)
		case seen[t.Name]:
			return fmt.Errorf("tier %q: duplicate name", t.Name)
		case t.VCPU < 1:
			return fmt.Errorf("tier %q: vcpu must be >= 1", t.Name)
		case t.MemMiB < 128:
			return fmt.Errorf("tier %q: mem_mib must be >= 128", t.Name)
		case t.Golden == "":
			return fmt.Errorf("tier %q: golden is required", t.Name)
		case t.Max < 1:
			return fmt.Errorf("tier %q: max must be >= 1", t.Name)
		case t.Min < 0:
			return fmt.Errorf("tier %q: min must be >= 0", t.Name)
		case t.Min > t.Max:
			return fmt.Errorf("tier %q: min (%d) must not exceed max (%d)", t.Name, t.Min, t.Max)
		case t.Max > c.MaxRunners:
			return fmt.Errorf("tier %q: max (%d) exceeds --max-runners (%d), the shared slot budget, so the tier can never reach its max", t.Name, t.Max, c.MaxRunners)
		}
		if t.ToolCache != "" {
			if _, err := os.Stat(t.ToolCache); err != nil {
				return fmt.Errorf("tier %q: toolcache image %q not accessible: %w", t.Name, t.ToolCache, err)
			}
		}
		seen[t.Name] = true
		totalMin += t.Min
	}
	if totalMin > c.MaxRunners {
		return fmt.Errorf("tiers' warm pools sum to %d but --max-runners (the shared slot budget) is %d", totalMin, c.MaxRunners)
	}
	return nil
}

// configured value may be either the PEM contents or a path to a PEM file.
func (c *Config) ResolvePrivateKey() (string, error) {
	if c.AppPrivateKey == "" {
		return "", nil
	}
	if strings.Contains(c.AppPrivateKey, "PRIVATE KEY") {
		return c.AppPrivateKey, nil
	}
	// Anything without a PEM header is treated as a path to a PEM file. Never
	// echo the raw value in an error: if the operator pasted key material that
	// failed the header check above, os.ReadFile's PathError would carry the
	// key verbatim into doctor --json (the README documents `doctor --json |
	// jq`) and into journald. Report the failure category instead.
	if strings.ContainsAny(c.AppPrivateKey, "\n\r") {
		return "", errors.New(`app private key is neither valid PEM (missing "PRIVATE KEY" header) nor a file path`)
	}
	b, err := os.ReadFile(c.AppPrivateKey)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return "", errors.New("app private key file does not exist")
		case errors.Is(err, os.ErrPermission):
			return "", errors.New("app private key file is not readable (permission denied)")
		default:
			return "", errors.New("app private key file could not be read")
		}
	}
	return string(b), nil
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
