package main

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

const (
	// shardMaxTiers is the most tiers the assigner places on one shard:
	// what a move after a crash re-registers, at about 1.4s a tier, and
	// what one bad release reaches. Memory allows far more (#166).
	shardMaxTiers = 500
	// tenantMaxTiers is the most tiers a tenant may list to be placed:
	// its move after a crash, about 50s plus 1.4s a tier, stays within the
	// minutes GitHub keeps its queued jobs assigned.
	tenantMaxTiers = 40
)

// tenantPlace is one tenant of the list as the assigner places it.
type tenantPlace struct {
	id, account string
	tiers       int
}

// placesOf is the list as the assigner places it: a tenant counts at least
// one tier.
func placesOf(list []config.Tenant) []tenantPlace {
	places := make([]tenantPlace, 0, len(list))
	for _, c := range list {
		places = append(places, tenantPlace{id: c.ID, account: config.AccountOf(c.URL), tiers: max(1, len(c.Tiers))})
	}
	return places
}

// place assigns each tenant's account to a shard, from the assignments now:
//
//   - one assigned to a live shard stays there, whatever the load: a
//     tenant moves only for a reason;
//   - one unassigned, or on a shard draining or gone, goes to the live
//     shard with the fewest tiers that has room for it (shardMaxTiers), the
//     first by id on a tie;
//   - one listing more than tenantMaxTiers tiers is not placed, nor is one
//     no shard has room for; unplaced says why, by tenant id.
//
// An account no tenant lists is unassigned. Tenants are placed in account
// order, so that the result depends on nothing but its inputs.
func place(tenants []tenantPlace, shards []jobstore.ShardState, current map[string]jobstore.Assignment) (next map[string]jobstore.Assignment, unplaced map[string]string) {
	next, unplaced = map[string]jobstore.Assignment{}, map[string]string{}
	load := map[string]int{}
	var live []string
	for _, s := range shards {
		if s.Live {
			live = append(live, s.ID)
			load[s.ID] = 0
		}
	}
	slices.Sort(live)
	tenants = slices.SortedFunc(slices.Values(tenants), func(a, b tenantPlace) int { return cmp.Compare(a.account, b.account) })
	var waiting []tenantPlace
	for _, t := range tenants {
		if t.tiers > tenantMaxTiers {
			unplaced[t.id] = fmt.Sprintf("it lists %d tiers, over the %d a tenant may have", t.tiers, tenantMaxTiers)
			continue
		}
		if a, ok := current[t.account]; ok {
			if _, isLive := load[a.Shard]; isLive {
				next[t.account] = jobstore.Assignment{Shard: a.Shard, Tiers: t.tiers}
				load[a.Shard] += t.tiers
				continue
			}
		}
		waiting = append(waiting, t)
	}
	for _, t := range waiting {
		best := ""
		for _, id := range live {
			if load[id]+t.tiers > shardMaxTiers {
				continue
			}
			if best == "" || load[id] < load[best] {
				best = id
			}
		}
		if best == "" {
			why := "no shard is live"
			if len(live) > 0 {
				why = fmt.Sprintf("no live shard has room for its %d tiers (%d a shard)", t.tiers, shardMaxTiers)
			}
			unplaced[t.id] = why
			continue
		}
		next[t.account] = jobstore.Assignment{Shard: best, Tiers: t.tiers}
		load[best] += t.tiers
	}
	return next, unplaced
}
