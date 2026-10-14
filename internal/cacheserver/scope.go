package cacheserver

import (
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
