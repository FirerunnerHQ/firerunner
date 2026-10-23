package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/firerunnerhq/firerunner/internal/config"
)

// checkReport is the result of "firerunner config check". It never carries a
// credential: auth is reported by kind only.
type checkReport struct {
	OK bool `json:"ok"`
	// Mode is "daemon" (the full configuration, as startup validates it) or
	// "catalog" (one tier catalog file on its own).
	Mode  string      `json:"mode"`
	Error string      `json:"error,omitempty"`
	Notes []string    `json:"notes,omitempty"`
	URL   string      `json:"url,omitempty"`
	Auth  string      `json:"auth,omitempty"`
	Tiers []checkTier `json:"tiers,omitempty"`
}

type checkTier struct {
	Name          string   `json:"name"`
	Provisioner   string   `json:"provisioner"`
	Labels        []string `json:"labels,omitempty"`
	Min           int      `json:"min"`
	Max           int      `json:"max"`
	VCPU          int      `json:"vcpu,omitempty"`
	MemMiB        int      `json:"mem_mib,omitempty"`
	Golden        string   `json:"golden,omitempty"`
	InstanceTypes []string `json:"instance_types,omitempty"`
	Market        string   `json:"market,omitempty"`
}

const configUsage = `usage:
  firerunner config check [--json] [daemon flags]
      validate the full configuration (flags and FR_* env) exactly as the
      daemon does at startup, without contacting GitHub or AWS
  firerunner config check [--json] --catalog FILE [--max-runners N]
      validate one tier catalog on its own: no credentials needed, and image
      paths are not checked for existence. N is the firecracker slot budget
      it will run under (default FR_MAX_RUNNERS, else 4)
exit status: 0 valid, 1 invalid, 2 usage error
`

// configCmd runs "firerunner config ..." and returns the exit status.
func configCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprint(stderr, configUsage)
		return 2
	}
	// The catalog's slot budget defaults like the daemon's: FR_MAX_RUNNERS,
	// else 4.
	maxRunners := 4
	if v := os.Getenv("FR_MAX_RUNNERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(stderr, "FR_MAX_RUNNERS=%q: %v\n", v, err)
			return 2
		}
		maxRunners = n
	}
	asJSON, catalog := false, ""
	var rest []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			asJSON = true
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, configUsage)
			return 0
		case a == "--catalog" || a == "-catalog":
			if i+1 >= len(args) {
				fmt.Fprint(stderr, "--catalog needs a file\n"+configUsage)
				return 2
			}
			i++
			catalog = args[i]
		case strings.HasPrefix(a, "--catalog="):
			catalog = strings.TrimPrefix(a, "--catalog=")
		default:
			rest = append(rest, a)
		}
	}

	var r checkReport
	if catalog != "" {
		r.Mode = "catalog"
		for i := 0; i < len(rest); i++ {
			a, val := rest[i], ""
			switch {
			case (a == "--max-runners" || a == "-max-runners") && i+1 < len(rest):
				i++
				val = rest[i]
			case strings.HasPrefix(a, "--max-runners="), strings.HasPrefix(a, "-max-runners="):
				val = a[strings.Index(a, "=")+1:]
			default:
				fmt.Fprintf(stderr, "--catalog takes only --max-runners, not %q\n%s", a, configUsage)
				return 2
			}
			n, err := strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(stderr, "--max-runners %q: %v\n", val, err)
				return 2
			}
			maxRunners = n
		}
		if maxRunners < 1 {
			fmt.Fprintf(stderr, "--max-runners must be >= 1, as the daemon requires\n")
			return 2
		}
		tiers, err := config.CheckCatalog(catalog, maxRunners)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.OK = true
			r.Tiers = checkTiers(tiers)
			r.Notes = append(r.Notes, fmt.Sprintf("checked against a firecracker slot budget of %d (--max-runners)", maxRunners),
				"image paths were not checked; run without --catalog on the host to check them")
		}
	} else {
		r.Mode = "daemon"
		// A flag or FR_* value that does not even parse is a usage error,
		// as for the daemon; anything the daemon's validation refuses is
		// "invalid".
		if _, err := config.Parse(rest); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 2
		}
		cfg, err := config.FromFlags(rest)
		if err == nil {
			// Startup resolves the App private key before contacting
			// anything; so does the check. Its error never echoes the key.
			_, err = cfg.ResolvePrivateKey()
		}
		if err != nil {
			r.Error = err.Error()
		} else {
			r.OK = true
			r.URL = cfg.URL
			r.Auth = "token"
			if cfg.AppClientID != "" {
				r.Auth = "github-app"
			}
			r.Tiers = checkTiers(cfg.EffectiveTiers())
		}
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
	} else {
		renderCheck(stdout, r)
	}
	if !r.OK {
		return 1
	}
	return 0
}

func checkTiers(tiers []config.Tier) []checkTier {
	out := make([]checkTier, 0, len(tiers))
	for _, t := range tiers {
		ct := checkTier{Name: t.Name, Provisioner: t.ProvisionerName(), Labels: t.Labels, Min: t.Min, Max: t.Max}
		if t.IsEC2() {
			ct.InstanceTypes, ct.Market = t.EC2.InstanceTypes, t.EC2.Market
		} else {
			ct.VCPU, ct.MemMiB, ct.Golden = t.VCPU, t.MemMiB, t.Golden
		}
		out = append(out, ct)
	}
	return out
}

func renderCheck(w io.Writer, r checkReport) {
	if !r.OK {
		fmt.Fprintf(w, "invalid (%s): %s\n", r.Mode, r.Error)
		return
	}
	fmt.Fprintf(w, "valid (%s): %d tier(s)\n", r.Mode, len(r.Tiers))
	if r.URL != "" {
		fmt.Fprintf(w, "url %s, auth %s\n", r.URL, r.Auth)
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  TIER\tPROVISIONER\tWARM\tMAX\tSHAPE")
	for _, t := range r.Tiers {
		shape := fmt.Sprintf("%d vCPU, %d MiB, %s", t.VCPU, t.MemMiB, t.Golden)
		if t.Provisioner == config.ProvisionerEC2 {
			shape = strings.Join(t.InstanceTypes, ",") + " (" + t.Market + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\n", t.Name, t.Provisioner, t.Min, t.Max, shape)
	}
	tw.Flush()
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}
