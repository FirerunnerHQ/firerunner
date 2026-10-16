package cacheserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/solcreek/firerunner/internal/jobidentity"
)

// SetScope enables multi-repository caching using only host-authenticated records.
// Missing, pending, expired and invalid records have no read or write authority.
func (s *Server) SetScope(lookup func(string) (jobidentity.Identity, error)) error {
	if lookup == nil {
		return fmt.Errorf("job identity reader is required")
	}
	s.lookup = lookup
	return nil
}

// ConnectionContext must be installed on http.Server.ConnContext. A TCP
// connection belongs to the VM generation that opened it, including while that
// VM is waiting for JobStarted. Queued requests/keep-alives from a dead VM must
// never acquire the next occupant's authority after its IP is reused.
type connectionGenerationKey struct{}

func (s *Server) ConnectionContext(ctx context.Context, conn net.Conn) context.Context {
	return s.bindGeneration(ctx, conn.RemoteAddr().String())
}
func (s *Server) bindGeneration(ctx context.Context, remote string) context.Context {
	generation := ""
	host, _, err := net.SplitHostPort(remote)
	if err == nil && s.lookup != nil {
		i, _ := s.lookup(host) // a valid pending lease has a generation but no job yet
		generation = i.Generation
	}
	return context.WithValue(ctx, connectionGenerationKey{}, generation)
}

// DefaultIdentityWait bounds how long a request from a pending lease waits for
// its job rights. JobStarted reaches the daemon tens of seconds after the guest
// has dequeued the job, and a workflow's first cache restore usually runs
// sooner; denying it then would turn most restores into misses.
const DefaultIdentityWait = 60 * time.Second

const identityPoll = 250 * time.Millisecond

// Denial reasons exported by /stats and /metrics, in exposition order.
const (
	denyMissing    = "missing"
	denyPending    = "pending"
	denyUnresolved = "unresolved"
	denyExpired    = "expired"
	denyInvalid    = "invalid"
	denyGeneration = "generation"
)

var denyReasons = []string{denyMissing, denyPending, denyUnresolved, denyExpired, denyInvalid, denyGeneration}

// SetIdentityWait sets how long a pending lease may wait for job rights
// (0 denies at once). Call before serving.
func (s *Server) SetIdentityWait(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.identityWait = d
}

func (s *Server) identity(r *http.Request) jobidentity.Identity {
	if s.lookup == nil {
		return jobidentity.Identity{}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return jobidentity.Identity{}
	}
	start := time.Now()
	i, reason, waited := s.awaitIdentity(r.Context(), host, start.Add(s.identityWait))
	if waited {
		s.identityWaits.Add(1)
		s.identityWaitNanos.Add(uint64(time.Since(start)))
	}
	if reason != "" {
		s.denyIdentity(host, reason)
		return jobidentity.Identity{}
	}
	s.identityMu.Lock()
	delete(s.identityFailures, host)
	s.identityMu.Unlock()
	i.Repository = strings.ToLower(i.Repository)
	return i
}

// awaitIdentity polls a pending lease until it resolves, turns terminal, changes
// generation, the deadline passes or the client goes away. Every exit except a
// valid record of the connection's own generation is a denial.
func (s *Server) awaitIdentity(ctx context.Context, host string, deadline time.Time) (jobidentity.Identity, string, bool) {
	bound, isBound := ctx.Value(connectionGenerationKey{}).(string)
	waited := false
	for {
		i, err := s.lookup(host)
		now := time.Now()
		if err == nil {
			err = i.Validate(now)
		}
		switch {
		case isBound && i.Generation != "" && i.Generation != bound:
			return jobidentity.Identity{}, denyGeneration, waited
		case err == nil:
			return i, "", waited
		case !i.Pending(now):
			return jobidentity.Identity{}, denialReason(i, now), waited
		case !now.Before(deadline):
			return jobidentity.Identity{}, denyPending, waited
		}
		waited = true
		t := time.NewTimer(min(identityPoll, deadline.Sub(now)))
		select {
		case <-ctx.Done():
			t.Stop()
			return jobidentity.Identity{}, denyPending, waited
		case <-t.C:
		}
	}
}

func denialReason(i jobidentity.Identity, now time.Time) string {
	switch {
	case i.Generation == "":
		return denyMissing
	case i.Unresolved:
		return denyUnresolved
	case i.ExpiresAt <= now.Unix():
		return denyExpired
	}
	return denyInvalid
}

func (s *Server) denyIdentity(host, reason string) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	s.identityDenied[reason]++
	if s.identityFailures == nil {
		s.identityFailures = make(map[string]bool)
	}
	if !s.identityFailures[host] && len(s.identityFailures) < 4096 {
		s.identityFailures[host] = true
		s.log.Warn("cache identity unavailable; reads and writes denied", "guest", host, "reason", reason)
	}
}

func owns(i jobidentity.Identity, e *Entry) bool {
	return i.WriteRef != "" && e.Ref == i.WriteRef && e.RepoID == i.Repository &&
		e.Generation == i.Generation && e.JobID == i.JobID && e.RunID == i.RunID
}
