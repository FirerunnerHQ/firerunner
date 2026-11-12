package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

const entitlementUsage = `usage:
  firerunner entitlement set ID --account STATE --budget STATE
      [--plan NAME] [--reason TEXT] [--valid-for DURATION]
      [--store SPEC] [--env-file FILE|none] [--json]
      write what tenant ID may run, as billing would
  firerunner entitlement clear ID [--store SPEC] [--env-file FILE|none] [--json]
      remove its record: the tenant is then held for want of one
  firerunner entitlement list [--store SPEC] [--env-file FILE|none] [--json]
      list every record, and what it allows now

Billing writes these records; this command is for operators and tests. ID is
a tenant's id in the --tenants catalog (the install is never held). --account
is trial, active, past_due or suspended; --budget is ok, warning or capped;
--valid-for defaults to 24h. A daemon reads the records every 30 seconds:

  - no record, a suspended account, or a capped budget: the tenant's launches
    are held and its jobs wait at GitHub with the reason; running jobs go on;
  - a record past its valid-until: held, unless its account is active and
    its budget not capped (then it goes on launching, and doctor warns).

SPEC is the daemon's --entitlements: ssm:<path> (one SSM parameter per
tenant; this shell's AWS credentials and region, FR_AWS_REGION or
AWS_REGION) or an absolute file path. Default FR_ENTITLEMENTS; with no FR_*
variable set, the daemon's settings file is read for it, as status and
doctor do (--env-file FILE reads another, --env-file none none).
exit status: 0 done, 1 failed, 2 usage error
`

// entitlementReport is what the entitlement subcommand prints with --json.
type entitlementReport struct {
	Store   string              `json:"store"`
	ID      string              `json:"id,omitempty"`
	Cleared bool                `json:"cleared,omitempty"`
	Record  *entitlement.Record `json:"record,omitempty"`
	// Decision is what the record set or listed allows now.
	Decision *entitlementDecision `json:"decision,omitempty"`
	Records  []entitlementEntry   `json:"records,omitempty"`
}

type entitlementEntry struct {
	ID       string              `json:"id"`
	Record   entitlement.Record  `json:"record"`
	Invalid  string              `json:"invalid,omitempty"`
	Decision entitlementDecision `json:"decision"`
}

type entitlementDecision struct {
	Held     bool   `json:"held"`
	FailOpen bool   `json:"fail_open,omitempty"`
	Warning  bool   `json:"warning,omitempty"`
	Why      string `json:"why,omitempty"`
}

func decisionOf(d entitlement.Decision) entitlementDecision {
	return entitlementDecision{Held: d.Hold, FailOpen: d.FailOpen, Warning: d.Warning, Why: d.Why}
}

// entitlementCmd runs "firerunner entitlement ..." and returns the exit
// status. open opens the store a spec names.
func entitlementCmd(args []string, stdout, stderr io.Writer, open func(spec string) (entitlement.Store, error)) int {
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n%s", append(a, entitlementUsage)...)
		return 2
	}
	if len(args) == 0 {
		return usageErr("firerunner entitlement: name an action")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, entitlementUsage)
		return 0
	}
	switch args[0] {
	case "set", "clear", "list":
	default:
		return usageErr("firerunner entitlement: unknown action %q", args[0])
	}
	action := args[0]
	rest, err := diagEnv("entitlement", args[1:], stderr)
	if err != nil {
		return usageErr("firerunner entitlement: %v", err)
	}
	spec, asJSON := os.Getenv("FR_ENTITLEMENTS"), false
	r := entitlement.Record{}
	validFor := 24 * time.Hour
	var ids []string
	set := map[string]bool{}
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		name, val, hasVal := strings.Cut(arg, "=")
		name = "--" + strings.TrimLeft(name, "-")
		switch name {
		case "--h", "--help":
			fmt.Fprint(stdout, entitlementUsage)
			return 0
		case "--json":
			if hasVal && val != "true" {
				return usageErr("--json takes no value")
			}
			asJSON = true
			continue
		case "--store", "--account", "--budget", "--plan", "--reason", "--valid-for":
			if !hasVal {
				if i+1 >= len(rest) {
					return usageErr("%s needs a value", name)
				}
				i++
				val = rest[i]
			}
			set[name] = true
			switch name {
			case "--store":
				spec = val
			case "--account":
				r.Account = entitlement.Account(val)
			case "--budget":
				r.Budget = entitlement.Budget(val)
			case "--plan":
				r.Plan = val
			case "--reason":
				r.Reason = val
			case "--valid-for":
				if validFor, err = time.ParseDuration(val); err != nil || validFor <= 0 {
					return usageErr("--valid-for %q: give a duration such as 24h", val)
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return usageErr("firerunner entitlement %s: unknown flag %q", action, arg)
		}
		ids = append(ids, arg)
	}
	for _, f := range []string{"--account", "--budget", "--plan", "--reason", "--valid-for"} {
		if set[f] && action != "set" {
			return usageErr("%s is for set", f)
		}
	}
	switch {
	case action == "list" && len(ids) > 0:
		return usageErr("firerunner entitlement list takes no id")
	case action != "list" && len(ids) != 1:
		return usageErr("firerunner entitlement %s takes one tenant id", action)
	case action != "list" && (ids[0] == tenantstate.Install || !tenantstate.ValidID(ids[0])):
		return usageErr("%q is not a tenant id: lowercase letters, digits and hyphens (the install is never held)", ids[0])
	case spec == "":
		return usageErr("no entitlement store: pass --store, or set FR_ENTITLEMENTS as the daemon's")
	}
	if err := entitlement.CheckSpec(spec); err != nil {
		return usageErr("--store %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if action == "set" {
		r.ValidUntil = now.Add(validFor)
		if err := r.Validate(); err != nil {
			return usageErr("firerunner entitlement set: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := open(spec)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner entitlement: %v\n", err)
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "firerunner entitlement %s: %s: %v\n", action, store, err)
		if accessDenied(err) {
			fmt.Fprintln(stderr, "These AWS credentials may not change entitlements; a control VM's role only reads them. Run it where billing's or an operator's credentials are.")
		}
		return 1
	}
	rep := entitlementReport{Store: store.String()}
	switch action {
	case "set":
		if err := store.Put(ctx, ids[0], r); err != nil {
			return fail(err)
		}
		d := decisionOf(entitlement.Decide(r, true, now))
		rep.ID, rep.Record, rep.Decision = ids[0], &r, &d
	case "clear":
		if err := store.Delete(ctx, ids[0]); err != nil {
			return fail(err)
		}
		rep.ID, rep.Cleared = ids[0], true
	case "list":
		rs, err := store.Load(ctx)
		if err != nil {
			return fail(err)
		}
		for _, id := range rs.IDs() {
			rec := rs[id]
			rep.Records = append(rep.Records, entitlementEntry{ID: id, Record: rec, Invalid: rec.Invalid, Decision: decisionOf(entitlement.Decide(rec, true, now))})
		}
	}
	if asJSON {
		writeJSON(stdout, rep)
		return 0
	}
	switch action {
	case "set":
		state := "may launch"
		if rep.Decision.Held {
			state = "is held: " + rep.Decision.Why
		}
		fmt.Fprintf(stdout, "set %s in %s: account %s, budget %s, valid until %s; it %s\nA running daemon applies it within %s.\n",
			rep.ID, rep.Store, r.Account, r.Budget, r.ValidUntil.Format(time.RFC3339), state, entitlement.DefaultInterval)
	case "clear":
		fmt.Fprintf(stdout, "cleared %s's record in %s; with none, a running daemon holds its launches within %s.\n", rep.ID, rep.Store, entitlement.DefaultInterval)
	case "list":
		if len(rep.Records) == 0 {
			fmt.Fprintf(stdout, "no entitlement record in %s: a daemon reading it holds every tenant of its catalog\n", rep.Store)
		}
		for _, e := range rep.Records {
			if e.Invalid != "" {
				fmt.Fprintf(stdout, "%s\tunreadable (%s): held\n", e.ID, e.Invalid)
				continue
			}
			state := "launches"
			switch {
			case e.Decision.Held:
				state = "HELD: " + e.Decision.Why
			case e.Decision.FailOpen:
				state = "EXPIRED, launching on it"
			case e.Decision.Warning:
				state = "launches, near its spend cap"
			}
			fmt.Fprintf(stdout, "%s\taccount %s\tbudget %s\tvalid until %s\t%s\n", e.ID, e.Record.Account, e.Record.Budget, e.Record.ValidUntil.Format(time.RFC3339), state)
		}
	}
	return 0
}

// openEntitlements opens a store, with an SSM client from this shell's AWS
// configuration where the spec names SSM.
func openEntitlements(spec string) (entitlement.Store, error) {
	return entitlement.Open(spec, cliSSM)
}
