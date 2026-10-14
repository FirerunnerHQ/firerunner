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

// SetScope enables host-authenticated, ref-scoped caching. Call before serving.
// The default ref is an explicit operator policy for requests without a job
// record, never a claim from the guest. Empty disables that read-only fallback.
// A server without SetScope has no cache read/write authority.
func (s *Server) SetScope(repo, defaultRef string, lookup func(string) (jobidentity.Identity, error)) error {
	if !jobidentity.ValidRepository(repo) {
		return fmt.Errorf("--repository must be owner/name")
	}
	if defaultRef != "" && (!strings.HasPrefix(defaultRef, "refs/heads/") || !jobidentity.ValidRef(defaultRef)) {
		return fmt.Errorf("--default-ref must be refs/heads/<branch> or empty")
	}
	if lookup == nil {
		return fmt.Errorf("job identity reader is required")
	}
	s.repo, s.defaultRef, s.lookup = strings.ToLower(repo), defaultRef, lookup
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
	var fallback jobidentity.Identity
	if s.repo == "" || s.lookup == nil {
		return fallback
	}
	fallback.Repository = s.repo
	if s.defaultRef != "" {
		fallback.ReadRefs = []string{s.defaultRef}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return jobidentity.Identity{}
	}
	i, err := s.lookup(host)
	if err != nil || i.Validate(time.Now()) != nil {
		return fallback
	}
	if generation, bound := r.Context().Value(connectionGenerationKey{}).(string); bound && generation != i.Generation {
		return jobidentity.Identity{}
	}
	if !strings.EqualFold(i.Repository, s.repo) {
		return jobidentity.Identity{}
	}
	i.Repository = s.repo
	return i
}

func owns(i jobidentity.Identity, e *Entry) bool {
	return i.WriteRef != "" && e.Ref == i.WriteRef && e.RepoID == i.Repository &&
		e.Generation == i.Generation && e.JobID == i.JobID && e.RunID == i.RunID
}
