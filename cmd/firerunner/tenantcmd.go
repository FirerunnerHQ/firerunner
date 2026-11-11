package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	awsgd "github.com/aws/aws-sdk-go-v2/service/guardduty"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/guardduty"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

const tenantUsage = `usage:
  firerunner tenant suspend ID --reason TEXT [--state SPEC] [--env-file FILE|none] [--json]
      hold every launch of the tenant and end every runner it has, busy or
      not: its running jobs fail
  firerunner tenant resume ID [--state SPEC] [--env-file FILE|none] [--json]
      lift a suspension; a flag for review stays until cleared
  firerunner tenant clear-flag ID [--state SPEC] [--env-file FILE|none] [--json]
      clear the tenant's flag for review, once reviewed
  firerunner tenant list [--state SPEC] [--env-file FILE|none] [--json]
      list the suspended tenants, and those flagged for review
  firerunner tenant findings [--owner OWNER] [--since DURATION]
      [--min-severity N] [--env-file FILE|none] [--json]
      list the GuardDuty findings on OWNER's EC2 instances in this account
      and the tenant each names; writes nothing

ID is a tenant's id in the --tenants catalog, or _install for the install's
own tiers. SPEC is where the daemon reads tenant states, as its
--tenant-state: ssm:<path> (one SSM parameter per suspended tenant; uses this
shell's AWS credentials and region, FR_AWS_REGION or AWS_REGION) or an
absolute file path. Default FR_TENANT_STATE; with no FR_* variable set, the
daemon's settings file is read for it, as status and doctor do (--env-file
FILE reads another, --env-file none none). Flags are kept beside it, under
its name with -flags added.

A running daemon reads the states every 15 seconds: within that, a suspended
tenant's launches are held and its machines asked to terminate. The
suspension holds across restarts until resumed.

With --guardduty, a daemon reads GuardDuty's findings every 2 minutes and
flags the tenant whose machine one names, by its firerunner:owner tag; status
and doctor show the flag. A flag never suspends: review it, then suspend the
tenant or clear the flag. findings defaults to --owner FR_OWNER, --since 24h
and --min-severity FR_GUARDDUTY_MIN_SEVERITY or 4.
exit status: 0 done, 1 failed, 2 usage error
`

// tenantReport is what the tenant subcommand prints with --json.
type tenantReport struct {
	Store string `json:"store,omitempty"`
	// Flags is where flags are kept; list's and clear-flag's.
	Flags string `json:"flags,omitempty"`
	// ID and State are set by suspend ("suspended") and resume ("active");
	// ID alone by clear-flag.
	ID     string    `json:"id,omitempty"`
	State  string    `json:"state,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	// Suspended and Flagged are list's.
	Suspended []tenantSuspension `json:"suspended,omitempty"`
	Flagged   []tenantFlag       `json:"flagged,omitempty"`
	// Findings are findings'.
	Findings []tenantFinding `json:"findings,omitempty"`
}

type tenantSuspension struct {
	ID     string    `json:"id"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

type tenantFlag struct {
	ID string `json:"id"`
	tenantstate.Flag
}

// tenantFinding is a GuardDuty finding, and the tenant it names, if any.
type tenantFinding struct {
	Tenant    string    `json:"tenant,omitempty"`
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Severity  float64   `json:"severity"`
	UpdatedAt time.Time `json:"updated_at"`
	Count     int       `json:"count"`
	Instance  string    `json:"instance"`
	Owner     string    `json:"owner"`
	Tier      string    `json:"tier,omitempty"`
	Runner    string    `json:"runner,omitempty"`
}

// tenantOpeners open what the tenant subcommand reads and writes.
type tenantOpeners struct {
	states func(spec string) (tenantstate.Store, error)
	flags  func(spec string) (tenantstate.FlagStore, error)
	// guardDuty returns the client and this account's id, whose findings
	// alone count ("" for every account's).
	guardDuty func() (guardduty.API, string, error)
}

// tenantArgs are the subcommand's parsed arguments.
type tenantArgs struct {
	action, spec, reason, owner string
	since                       time.Duration
	minSeverity                 float64
	ids                         []string
	asJSON                      bool
	set                         map[string]bool
}

// tenantCmd runs "firerunner tenant ..." and returns the exit status.
func tenantCmd(args []string, stdout, stderr io.Writer, open tenantOpeners) (code int) {
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n%s", append(a, tenantUsage)...)
		return 2
	}
	if len(args) == 0 {
		return usageErr("firerunner tenant: name an action")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, tenantUsage)
		return 0
	}
	switch args[0] {
	case "suspend", "resume", "clear-flag", "list", "findings":
	default:
		return usageErr("firerunner tenant: unknown action %q", args[0])
	}
	// On the daemon's host, its settings, the state store included, are in
	// its EnvironmentFile, as for status and doctor.
	rest, err := diagEnv("tenant", args[1:], stderr)
	if err != nil {
		return usageErr("firerunner tenant: %v", err)
	}
	a, help, err := parseTenantArgs(append([]string{args[0]}, rest...))
	if help {
		fmt.Fprint(stdout, tenantUsage)
		return 0
	}
	if err != nil {
		return usageErr("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if a.action == "findings" {
		return tenantFindings(ctx, a, stdout, stderr, open.guardDuty)
	}

	r := tenantReport{}
	fail := func(where fmt.Stringer, err error) int {
		fmt.Fprintf(stderr, "firerunner tenant %s: %s: %v\n", a.action, where, err)
		if a.action != "list" && accessDenied(err) {
			// The control VM's role reads tenant state and writes flags,
			// but changing either is an operator's act: say where to run it.
			fmt.Fprintf(stderr, "These AWS credentials may not change tenant state; a control VM's role is not meant to. Run it where you hold operator credentials:\n  firerunner tenant %s %s --state %s\n", a.action, a.ids[0], a.spec)
		}
		return 1
	}
	var store tenantstate.Store
	var flags tenantstate.FlagStore
	var flagsErr error
	if a.action != "clear-flag" {
		if store, err = open.states(a.spec); err != nil {
			fmt.Fprintf(stderr, "firerunner tenant: %v\n", err)
			return 1
		}
		r.Store = store.String()
	}
	if a.action == "clear-flag" || a.action == "list" {
		if flags, err = open.flags(a.spec); err != nil {
			fmt.Fprintf(stderr, "firerunner tenant: %v\n", err)
			return 1
		}
		r.Flags = flags.String()
	}
	switch a.action {
	case "suspend":
		rec := tenantstate.Record{Reason: strings.TrimSpace(a.reason), Since: time.Now().UTC().Truncate(time.Second)}
		if err := store.Put(ctx, a.ids[0], rec); err != nil {
			return fail(store, err)
		}
		r.ID, r.State, r.Reason, r.Since = a.ids[0], "suspended", rec.Reason, rec.Since
	case "resume":
		if err := store.Delete(ctx, a.ids[0]); err != nil {
			return fail(store, err)
		}
		r.ID, r.State = a.ids[0], "active"
	case "clear-flag":
		fl, _, err := flags.LoadFlags(ctx)
		if err != nil {
			return fail(flags, err)
		}
		if _, ok := fl[a.ids[0]]; ok {
			if err := flags.DeleteFlag(ctx, a.ids[0]); err != nil {
				return fail(flags, err)
			}
			r.State = "cleared"
		} else {
			r.State = "not flagged"
		}
		r.ID = a.ids[0]
	case "list":
		st, err := store.Load(ctx)
		if err != nil {
			return fail(store, err)
		}
		for _, id := range st.IDs() {
			r.Suspended = append(r.Suspended, tenantSuspension{ID: id, Reason: st[id].Reason, Since: st[id].Since})
		}
		// The suspensions are printed even when the flags cannot be
		// read, as by credentials from before flags were kept.
		fl, _, err := flags.LoadFlags(ctx)
		if err != nil {
			flagsErr = err
		}
		for _, id := range fl.IDs() {
			r.Flagged = append(r.Flagged, tenantFlag{ID: id, Flag: fl[id]})
		}
	}
	defer func() {
		if flagsErr != nil {
			code = fail(flags, flagsErr)
		}
	}()
	if a.asJSON {
		writeJSON(stdout, r)
		return 0
	}
	switch a.action {
	case "suspend":
		fmt.Fprintf(stdout, "suspended %s in %s: %s\nA running daemon holds its launches and ends its runners within %s.\n", r.ID, r.Store, r.Reason, tenantstate.DefaultInterval)
	case "resume":
		fmt.Fprintf(stdout, "resumed %s in %s\nA running daemon launches for its waiting jobs within %s.\n", r.ID, r.Store, tenantstate.DefaultInterval)
	case "clear-flag":
		if r.State == "not flagged" {
			fmt.Fprintf(stdout, "%s is not flagged in %s\n", r.ID, r.Flags)
			break
		}
		fmt.Fprintf(stdout, "cleared the flag on %s in %s\nA running daemon's status drops it within %s; a new finding, or an update of one, flags it again.\n", r.ID, r.Flags, guardduty.DefaultInterval)
	case "list":
		if len(r.Suspended) == 0 {
			fmt.Fprintf(stdout, "no tenant suspended in %s\n", r.Store)
		}
		for _, s := range r.Suspended {
			fmt.Fprintf(stdout, "%s\tsuspended since %s: %s\n", s.ID, s.Since.Format(time.RFC3339), s.Reason)
		}
		for _, f := range r.Flagged {
			fmt.Fprintf(stdout, "%s\tflagged for review: %s, severity %g, %d finding(s) or update(s) from %s to %s, latest %s on %s\n",
				f.ID, f.Type, f.Severity, f.Count, f.First.Format(time.RFC3339), f.Last.Format(time.RFC3339), f.Finding, f.Instance)
		}
	}
	return 0
}

// parseTenantArgs reads the action, its ids and flags, and checks them. help
// is true when they ask for the usage.
func parseTenantArgs(args []string) (a tenantArgs, help bool, err error) {
	a = tenantArgs{action: args[0], spec: os.Getenv("FR_TENANT_STATE"), owner: os.Getenv("FR_OWNER"),
		since: 24 * time.Hour, minSeverity: guardduty.DefaultMinSeverity, set: map[string]bool{}}
	switch a.action {
	case "suspend", "resume", "clear-flag", "list", "findings":
	default:
		return a, false, fmt.Errorf("firerunner tenant: unknown action %q", a.action)
	}
	// Read only for findings: a bad value must never block a suspension.
	if v := os.Getenv("FR_GUARDDUTY_MIN_SEVERITY"); v != "" && a.action == "findings" {
		if a.minSeverity, err = strconv.ParseFloat(v, 64); err != nil {
			return a, false, fmt.Errorf("FR_GUARDDUTY_MIN_SEVERITY=%q is not a number", v)
		}
	}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		name, val, hasVal := strings.Cut(arg, "=")
		name = "--" + strings.TrimLeft(name, "-")
		switch name {
		case "--h", "--help":
			return a, true, nil
		case "--json":
			if hasVal && val != "true" {
				return a, false, errors.New("--json takes no value")
			}
			a.asJSON = true
			continue
		case "--state", "--reason", "--owner", "--since", "--min-severity":
			if !hasVal {
				if i+1 >= len(rest) {
					return a, false, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = rest[i]
			}
			a.set[name] = true
			switch name {
			case "--state":
				a.spec = val
			case "--reason":
				a.reason = val
			case "--owner":
				a.owner = val
			case "--since":
				if a.since, err = time.ParseDuration(val); err != nil || a.since <= 0 {
					return a, false, fmt.Errorf("--since %q: give a duration such as 24h", val)
				}
			case "--min-severity":
				if a.minSeverity, err = strconv.ParseFloat(val, 64); err != nil {
					return a, false, fmt.Errorf("--min-severity %q is not a number", val)
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return a, false, fmt.Errorf("firerunner tenant %s: unknown flag %q", a.action, arg)
		}
		a.ids = append(a.ids, arg)
	}
	takesID := a.action != "list" && a.action != "findings"
	for flag, actions := range map[string]string{
		"--reason": "suspend", "--owner": "findings", "--since": "findings", "--min-severity": "findings",
		"--state": "suspend resume clear-flag list",
	} {
		if a.set[flag] && !strings.Contains(" "+actions+" ", " "+a.action+" ") {
			return a, false, fmt.Errorf("%s is for %s", flag, strings.ReplaceAll(actions, " ", ", "))
		}
	}
	switch {
	case !takesID && len(a.ids) > 0:
		return a, false, fmt.Errorf("firerunner tenant %s takes no id", a.action)
	case takesID && len(a.ids) != 1:
		return a, false, fmt.Errorf("firerunner tenant %s takes one tenant id", a.action)
	case takesID && !tenantstate.ValidID(a.ids[0]):
		return a, false, fmt.Errorf("%q is not a tenant id: lowercase letters, digits and hyphens, or %s for the install's own tiers", a.ids[0], tenantstate.Install)
	case a.action == "suspend" && strings.TrimSpace(a.reason) == "":
		return a, false, errors.New("firerunner tenant suspend: --reason is required; it is recorded, and shown in firerunner status")
	case a.action == "findings" && a.owner == "":
		return a, false, errors.New("firerunner tenant findings: no owner: pass --owner, or set FR_OWNER as the daemon's")
	case a.action == "findings" && (a.minSeverity < 1 || a.minSeverity > 10):
		return a, false, fmt.Errorf("--min-severity %g: GuardDuty's severities run from 1 to 10", a.minSeverity)
	case a.action == "findings":
		return a, false, nil
	case a.spec == "":
		return a, false, errors.New("no tenant state store: pass --state, or set FR_TENANT_STATE as the daemon's")
	}
	if err := tenantstate.CheckSpec(a.spec); err != nil {
		return a, false, fmt.Errorf("--state %w", err)
	}
	return a, false, nil
}

// tenantFindings lists the findings on a.owner's instances in this account
// and the tenant each names, by the owner tag as a daemon maps it, over
// a.since rather than a daemon's cursor, and for any tenant id, in the
// catalog or not. It writes nothing: neither a flag nor the cursor.
func tenantFindings(ctx context.Context, a tenantArgs, stdout, stderr io.Writer, open func() (guardduty.API, string, error)) int {
	api, account, err := open()
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant findings: %v\n", err)
		return 1
	}
	found, detectors, err := guardduty.List(ctx, api, guardduty.Query{Since: time.Now().Add(-a.since), MinSeverity: a.minSeverity, Account: account})
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant findings: %v\n", err)
		return 1
	}
	if detectors == 0 {
		fmt.Fprintln(stderr, "firerunner tenant findings: GuardDuty has no detector in this region")
	}
	tenantOf := guardduty.Owners(a.owner, nil)
	r := tenantReport{}
	for _, f := range found {
		id, ok := tenantOf(f.Owner)
		if !ok {
			continue
		}
		r.Findings = append(r.Findings, tenantFinding{Tenant: id, ID: f.ID, Type: f.Type, Severity: f.Severity, UpdatedAt: f.UpdatedAt,
			Count: f.Count, Instance: f.Instance, Owner: f.Owner, Tier: f.Tier, Runner: f.Runner})
	}
	if a.asJSON {
		writeJSON(stdout, r)
		return 0
	}
	if len(r.Findings) == 0 {
		fmt.Fprintf(stdout, "no finding of severity %g and above on %s's instances in the last %s\n", a.minSeverity, a.owner, a.since)
	}
	for _, f := range r.Findings {
		fmt.Fprintf(stdout, "%s\t%s\tseverity %g\tupdated %s\ttier %s\tinstance %s\tfinding %s\n",
			f.Tenant, f.Type, f.Severity, f.UpdatedAt.Format(time.RFC3339), f.Tier, f.Instance, f.ID)
	}
	return 0
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// openTenant opens what the tenant subcommand uses, with clients from this
// shell's AWS configuration where a spec names SSM, or for findings.
func openTenant() tenantOpeners {
	newSSM := func() (tenantstate.SSMAPI, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		awsCfg, err := loadAWSConfig(ctx, &config.Config{AWSRegion: os.Getenv("FR_AWS_REGION")})
		if err != nil {
			return nil, errors.New("tenant state: " + err.Error())
		}
		return awsssm.NewFromConfig(awsCfg), nil
	}
	return tenantOpeners{
		states: func(spec string) (tenantstate.Store, error) { return tenantstate.Open(spec, newSSM) },
		flags:  func(spec string) (tenantstate.FlagStore, error) { return tenantstate.OpenFlags(spec, newSSM) },
		guardDuty: func() (guardduty.API, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			awsCfg, err := loadAWSConfig(ctx, &config.Config{AWSRegion: os.Getenv("FR_AWS_REGION")})
			if err != nil {
				return nil, "", err
			}
			account, err := callerAccount(awssts.NewFromConfig(awsCfg))(ctx)
			if err != nil {
				return nil, "", err
			}
			return awsgd.NewFromConfig(awsCfg), account, nil
		},
	}
}

// accessDenied reports whether err is AWS refusing the caller's credentials
// the call.
func accessDenied(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
		return true
	}
	return false
}
