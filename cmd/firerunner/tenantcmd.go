package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

const tenantUsage = `usage:
  firerunner tenant suspend ID --reason TEXT [--state SPEC] [--json]
      hold every launch of the tenant and end every runner it has, busy or
      not: its running jobs fail
  firerunner tenant resume ID [--state SPEC] [--json]
      lift a suspension
  firerunner tenant list [--state SPEC] [--json]
      list the suspended tenants

ID is a tenant's id in the --tenants catalog, or _install for the install's
own tiers. SPEC is where the daemon reads tenant states, as its
--tenant-state: ssm:<path> (one SSM parameter per suspended tenant; uses this
shell's AWS credentials and region, FR_AWS_REGION or AWS_REGION) or an
absolute file path. Default FR_TENANT_STATE.

A running daemon reads the states every 15 seconds: within that, a suspended
tenant's launches are held and its machines asked to terminate. The
suspension holds across restarts until resumed.
exit status: 0 done, 1 failed, 2 usage error
`

// tenantReport is what the tenant subcommand prints with --json.
type tenantReport struct {
	Store string `json:"store"`
	// ID and State are set by suspend ("suspended") and resume ("active").
	ID     string    `json:"id,omitempty"`
	State  string    `json:"state,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	// Suspended is list's.
	Suspended []tenantSuspension `json:"suspended,omitempty"`
}

type tenantSuspension struct {
	ID     string    `json:"id"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// tenantCmd runs "firerunner tenant ..." and returns the exit status. open
// returns the store a spec names.
func tenantCmd(args []string, stdout, stderr io.Writer, open func(spec string) (tenantstate.Store, error)) int {
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
	action := args[0]
	switch action {
	case "suspend", "resume", "list":
	default:
		return usageErr("firerunner tenant: unknown action %q", action)
	}
	spec, reason, asJSON, reasonSet := os.Getenv("FR_TENANT_STATE"), "", false, false
	var ids []string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "-h", "--help":
			fmt.Fprint(stdout, tenantUsage)
			return 0
		case "--json", "-json":
			if hasVal && val != "true" {
				return usageErr("--json takes no value")
			}
			asJSON = true
			continue
		case "--state", "-state", "--reason", "-reason":
			if !hasVal {
				if i+1 >= len(rest) {
					return usageErr("%s needs a value", name)
				}
				i++
				val = rest[i]
			}
			if strings.HasSuffix(name, "state") {
				spec = val
			} else {
				reason, reasonSet = val, true
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			return usageErr("firerunner tenant %s: unknown flag %q", action, a)
		}
		ids = append(ids, a)
	}
	switch {
	case action == "list" && len(ids) > 0:
		return usageErr("firerunner tenant list takes no id")
	case action != "list" && len(ids) != 1:
		return usageErr("firerunner tenant %s takes one tenant id", action)
	case action != "list" && !tenantstate.ValidID(ids[0]):
		return usageErr("%q is not a tenant id: lowercase letters, digits and hyphens, or %s for the install's own tiers", ids[0], tenantstate.Install)
	case action == "suspend" && strings.TrimSpace(reason) == "":
		return usageErr("firerunner tenant suspend: --reason is required; it is recorded, and shown in firerunner status")
	case action != "suspend" && reasonSet:
		return usageErr("--reason is for suspend")
	case spec == "":
		return usageErr("no tenant state store: pass --state, or set FR_TENANT_STATE as the daemon's")
	}
	if err := tenantstate.CheckSpec(spec); err != nil {
		return usageErr("--state %v", err)
	}
	store, err := open(spec)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := tenantReport{Store: store.String()}
	switch action {
	case "suspend":
		rec := tenantstate.Record{Reason: strings.TrimSpace(reason), Since: time.Now().UTC().Truncate(time.Second)}
		err = store.Put(ctx, ids[0], rec)
		r.ID, r.State, r.Reason, r.Since = ids[0], "suspended", rec.Reason, rec.Since
	case "resume":
		err = store.Delete(ctx, ids[0])
		r.ID, r.State = ids[0], "active"
	case "list":
		var st tenantstate.States
		st, err = store.Load(ctx)
		for _, id := range st.IDs() {
			r.Suspended = append(r.Suspended, tenantSuspension{ID: id, Reason: st[id].Reason, Since: st[id].Since})
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant %s: %s: %v\n", action, store, err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return 0
	}
	switch action {
	case "suspend":
		fmt.Fprintf(stdout, "suspended %s in %s: %s\nA running daemon holds its launches and ends its runners within %s.\n", r.ID, r.Store, r.Reason, tenantstate.DefaultInterval)
	case "resume":
		fmt.Fprintf(stdout, "resumed %s in %s\nA running daemon launches for its waiting jobs within %s.\n", r.ID, r.Store, tenantstate.DefaultInterval)
	case "list":
		if len(r.Suspended) == 0 {
			fmt.Fprintf(stdout, "no tenant suspended in %s\n", r.Store)
		}
		for _, s := range r.Suspended {
			fmt.Fprintf(stdout, "%s\tsuspended since %s: %s\n", s.ID, s.Since.Format(time.RFC3339), s.Reason)
		}
	}
	return 0
}

// openTenantState opens the store spec names, with an SSM client from this
// shell's AWS configuration for an SSM path.
func openTenantState(spec string) (tenantstate.Store, error) {
	return tenantstate.Open(spec, func() (tenantstate.SSMAPI, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		awsCfg, err := loadAWSConfig(ctx, &config.Config{AWSRegion: os.Getenv("FR_AWS_REGION")})
		if err != nil {
			return nil, errors.New("tenant state: " + err.Error())
		}
		return awsssm.NewFromConfig(awsCfg), nil
	})
}
