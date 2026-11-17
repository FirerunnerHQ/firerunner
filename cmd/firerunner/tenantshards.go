package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// shardsReport is what "firerunner tenant shards" prints: the shards that
// share the tenants list under leases (--tenant-leases), which serves each
// tenant, and the removals not yet cleaned up. Every time is the
// database's.
type shardsReport struct {
	DatabaseTime time.Time `json:"database_time"`
	// Assigner is the shard that assigns the tenants, if any holds that
	// lease now.
	Assigner string          `json:"assigner,omitempty"`
	Shards   []shardsShard   `json:"shards"`
	Tenants  []shardsTenant  `json:"tenants"`
	Removals []shardsRemoval `json:"removals"`
	// Problems are what needs looking at, one line each: none when every
	// tenant is served and every removal is under way.
	Problems []string `json:"problems"`
}

type shardsShard struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	Owner       string `json:"owner"`
	TenantOwner string `json:"tenant_owner"`
	// State is live, draining, stale (no heartbeat for 30s) or
	// list-failed (its last read of the tenants list did not apply).
	State        string  `json:"state"`
	HeartbeatAgo float64 `json:"heartbeat_seconds_ago"`
	Assigner     bool    `json:"assigner,omitempty"`
	// Tenants and Tiers are what is assigned to it.
	Tenants int `json:"tenants"`
	Tiers   int `json:"tiers"`
}

type shardsTenant struct {
	ID      string `json:"id"`
	Account string `json:"account"`
	Tiers   int    `json:"tiers"`
	// AssignedTo is the shard it is assigned to, and LeasedBy the one that
	// holds its account's lease now, if any.
	AssignedTo string `json:"assigned_to,omitempty"`
	LeasedBy   string `json:"leased_by,omitempty"`
	// State is served, moving (assigned to one shard, its lease still
	// another's), waiting (assigned, its lease not taken yet) or unplaced.
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
}

type shardsRemoval struct {
	ID         string    `json:"id"`
	Account    string    `json:"account"`
	RemovedAt  time.Time `json:"removed_at"`
	RemovedAgo float64   `json:"removed_seconds_ago"`
	// LeasedBy is the shard that holds the account's lease, which removes
	// it; with none, the assigner sweeps it once it takes that lease.
	LeasedBy string `json:"leased_by,omitempty"`
}

// shardsOf reads an overview of the job store as shards and tenants.
func shardsOf(o jobstore.Overview) (shardsReport, error) {
	r := shardsReport{DatabaseTime: o.Now, Shards: []shardsShard{}, Tenants: []shardsTenant{}, Removals: []shardsRemoval{}, Problems: []string{}}
	list, err := config.ParseTenants(o.List)
	if err != nil {
		return r, fmt.Errorf("the tenants list does not read: %w", err)
	}
	holder := map[string]jobstore.LeaseRow{}
	for _, l := range o.Leases {
		if l.ExpiresAt.After(o.Now) {
			holder[l.Account] = l
		}
	}
	r.Assigner = holder[jobstore.AssignerLease].Shard
	assigned := map[string]jobstore.AssignmentRow{}
	for _, a := range o.Assignments {
		assigned[a.Account] = a
	}
	shardIndex := map[string]int{}
	live := 0
	for _, s := range o.Shards {
		st := shardsShard{ID: s.ID, Host: s.Host, Owner: s.Owner, TenantOwner: s.TenantOwner,
			HeartbeatAgo: o.Now.Sub(s.HeartbeatAt).Seconds(), Assigner: s.ID == r.Assigner}
		switch {
		case s.Draining:
			st.State = "draining"
		case o.Now.Sub(s.HeartbeatAt) > jobstore.ShardStale:
			st.State = "stale"
		case !s.ListOK:
			st.State = "list-failed"
			r.Problems = append(r.Problems, fmt.Sprintf("shard %s: its last read of the tenants list did not apply; it is given no tenant (see its doctor's tenant-catalog)", s.ID))
		default:
			st.State = "live"
			live++
		}
		shardIndex[s.ID] = len(r.Shards)
		r.Shards = append(r.Shards, st)
	}
	if len(o.Shards) > 0 && r.Assigner == "" {
		r.Problems = append(r.Problems, "no shard holds the assigner's lease: nothing is assigned or moved until one does")
	}
	for _, a := range o.Assignments {
		if i, ok := shardIndex[a.Shard]; ok {
			r.Shards[i].Tenants++
			r.Shards[i].Tiers += a.Tiers
		}
	}

	for _, c := range list {
		account := config.AccountOf(c.URL)
		t := shardsTenant{ID: c.ID, Account: account, Tiers: max(1, len(c.Tiers))}
		a, isAssigned := assigned[account]
		l, isLeased := holder[account]
		t.AssignedTo, t.LeasedBy = a.Shard, l.Shard
		switch {
		case isAssigned && isLeased && l.Shard == a.Shard:
			t.State = "served"
		case isAssigned && isLeased:
			t.State = "moving"
			t.Why = fmt.Sprintf("assigned to %s; %s still holds its lease, for %s at most", a.Shard, l.Shard, l.ExpiresAt.Sub(o.Now).Round(time.Second))
		case isAssigned:
			t.State = "waiting"
			t.Why = fmt.Sprintf("assigned to %s, which has not taken its lease yet", a.Shard)
			if i, ok := shardIndex[a.Shard]; !ok || r.Shards[i].State != "live" {
				t.Why = fmt.Sprintf("assigned to %s, which is not live: the assigner moves it", a.Shard)
			}
		default:
			t.State = "unplaced"
			switch {
			case t.Tiers > tenantMaxTiers:
				t.Why = fmt.Sprintf("it lists %d tiers, over the %d a tenant may have", t.Tiers, tenantMaxTiers)
			case live == 0:
				t.Why = "no shard is live"
			case r.Assigner == "":
				t.Why = "no shard assigns the tenants"
			default:
				t.Why = "not assigned yet, or no live shard has room for its tiers (the assigning shard's status says which)"
			}
			if isLeased {
				t.Why += fmt.Sprintf("; %s still serves it", l.Shard)
			}
		}
		if t.State != "served" {
			r.Problems = append(r.Problems, fmt.Sprintf("tenant %s: %s: %s", t.ID, t.State, t.Why))
		}
		r.Tenants = append(r.Tenants, t)
	}

	for _, rm := range o.Removed {
		x := shardsRemoval{ID: rm.ID, RemovedAt: rm.RemovedAt, RemovedAgo: o.Now.Sub(rm.RemovedAt).Seconds()}
		var c config.Tenant
		if json.Unmarshal(rm.Entry, &c) == nil {
			x.Account = config.AccountOf(c.URL)
		}
		x.LeasedBy = holder[x.Account].Shard
		// A removal under way takes a few minutes: its EC2 sweep waits 2.
		if o.Now.Sub(rm.RemovedAt) > 15*time.Minute {
			r.Problems = append(r.Problems, fmt.Sprintf("removal of tenant %s: not done after %s; the status of shard %s says why", rm.ID,
				o.Now.Sub(rm.RemovedAt).Round(time.Minute), cmp.Or(x.LeasedBy, r.Assigner, "(none)")))
		}
		r.Removals = append(r.Removals, x)
	}
	slices.SortFunc(r.Tenants, func(a, b shardsTenant) int { return cmp.Compare(a.ID, b.ID) })
	return r, nil
}

// render writes the report for people.
func (r shardsReport) render(w io.Writer) {
	fmt.Fprintf(w, "shards (database time %s):\n", r.DatabaseTime.UTC().Format(time.RFC3339))
	if len(r.Shards) == 0 {
		fmt.Fprintln(w, "  none: no daemon serves this list under --tenant-leases")
	}
	for _, s := range r.Shards {
		role := ""
		if s.Assigner {
			role = ", assigning"
		}
		fmt.Fprintf(w, "  %s  %s%s  host %s  owner %s  beat %s ago  %d tenant(s), %d tier(s)\n", s.ID, s.State, role, s.Host, s.Owner,
			humanDur(time.Duration(s.HeartbeatAgo*float64(time.Second))), s.Tenants, s.Tiers)
	}
	fmt.Fprintln(w, "tenants:")
	if len(r.Tenants) == 0 {
		fmt.Fprintln(w, "  none listed")
	}
	for _, t := range r.Tenants {
		switch t.State {
		case "served":
			fmt.Fprintf(w, "  %s  %s  %d tier(s)  served by %s\n", t.ID, t.Account, t.Tiers, t.LeasedBy)
		default:
			fmt.Fprintf(w, "  %s  %s  %d tier(s)  %s: %s\n", t.ID, t.Account, t.Tiers, t.State, t.Why)
		}
	}
	if len(r.Removals) > 0 {
		fmt.Fprintln(w, "removals not yet cleaned up:")
		for _, x := range r.Removals {
			by := "the assigning shard sweeps it"
			if x.LeasedBy != "" {
				by = "being removed under " + x.LeasedBy + "'s lease"
			}
			fmt.Fprintf(w, "  %s  %s  removed %s ago  %s\n", x.ID, x.Account, humanDur(time.Duration(x.RemovedAgo*float64(time.Second))), by)
		}
	}
	if len(r.Problems) == 0 {
		fmt.Fprintln(w, "no problem: every tenant is served")
		return
	}
	fmt.Fprintln(w, "problems:")
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  %s\n", p)
	}
}

// humanDur is d to the second, or the minute past an hour.
func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

// shardsStore is what "tenant shards" reads; a *jobstore.TenantStore.
type shardsStore interface {
	Overview(ctx context.Context) (jobstore.Overview, error)
	Close()
}

// openShards connects to the job store the daemon's settings name.
func openShards(cfg *config.Config) (shardsStore, error) {
	dsn, err := jobStoreDSN(cfg)
	if err != nil {
		return nil, err
	}
	if dsn == "" {
		return nil, fmt.Errorf("no job store: set FR_JOB_EVENTS_DSN or FR_JOB_EVENTS_DSN_FILE")
	}
	return jobstore.OpenTenants(dsn)
}

// tenantShardsCmd runs "firerunner tenant shards [--json]". args follow the
// action; the env file has been read.
func tenantShardsCmd(args []string, stdout, stderr io.Writer, open func(*config.Config) (shardsStore, error)) int {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json", "-json", "--json=true":
			asJSON = true
		case "-h", "--help", "-help":
			fmt.Fprint(stdout, tenantUsage)
			return 0
		default:
			fmt.Fprintf(stderr, "firerunner tenant shards: unknown argument %q\n%s", a, tenantUsage)
			return 2
		}
	}
	cfg, err := config.Parse(nil)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant shards: %v\n%s", err, tenantUsage)
		return 2
	}
	store, err := open(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant shards: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o, err := store.Overview(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant shards: %v\n", err)
		return 1
	}
	r, err := shardsOf(o)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner tenant shards: %v\n", err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintf(stderr, "firerunner tenant shards: %v\n", err)
			return 1
		}
		return 0
	}
	r.render(stdout)
	return 0
}
