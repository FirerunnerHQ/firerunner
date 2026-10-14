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

func (s *Server) identity(r *http.Request) jobidentity.Identity {
	if s.lookup == nil {
		return jobidentity.Identity{}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return jobidentity.Identity{}
	}
	i, err := s.lookup(host)
	if err == nil {
		err = i.Validate(time.Now())
	}
	if err != nil {
		s.identityDenied.Add(1)
		s.identityMu.Lock()
		if s.identityFailures == nil {
			s.identityFailures = make(map[string]bool)
		}
		if !s.identityFailures[host] && len(s.identityFailures) < 4096 {
			s.identityFailures[host] = true
			s.log.Warn("cache identity unavailable; reads and writes denied", "guest", host, "err", err)
		}
		s.identityMu.Unlock()
		return jobidentity.Identity{}
	}
	s.identityMu.Lock()
	delete(s.identityFailures, host)
	s.identityMu.Unlock()
	if generation, bound := r.Context().Value(connectionGenerationKey{}).(string); bound && generation != i.Generation {
		return jobidentity.Identity{}
	}
	i.Repository = strings.ToLower(i.Repository)
	return i
}

func owns(i jobidentity.Identity, e *Entry) bool {
	return i.WriteRef != "" && e.Ref == i.WriteRef && e.RepoID == i.Repository &&
		e.Generation == i.Generation && e.JobID == i.JobID && e.RunID == i.RunID
}
