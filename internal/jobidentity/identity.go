// Package jobidentity binds host-observed GitHub jobs to ephemeral VM addresses.
// Nothing in this package accepts identity claims from a guest.
package jobidentity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const DefaultDir = "/run/firerunner/jobs"

// Job comes from the authenticated scale-set control plane, not a runner token.
type Job struct {
	WorkflowRef string // Workflow definition, deliberately not used as triggering authority.
	Runner      string
	Repository  string
	Event       string
	RunID       int64
	JobID       string
}

// Identity is published by the daemon. Generation changes on EVERY slot reuse.
// ExpiresAt is renewed while the daemon owns the VM, bounding crash leftovers.
type Identity struct {
	IP         string   `json:"ip"`
	Runner     string   `json:"runner"`
	Generation string   `json:"generation"`
	Repository string   `json:"repository"`
	Event      string   `json:"event"`
	WriteRef   string   `json:"write_ref"`
	ReadRefs   []string `json:"read_refs"`
	Fork       bool     `json:"fork"`
	RunID      int64    `json:"run_id"`
	JobID      string   `json:"job_id"`
	ExpiresAt  int64    `json:"expires_at"`
}

func ValidRef(ref string) bool {
	if !(strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") || strings.HasPrefix(ref, "refs/pull/")) {
		return false
	}
	return len(ref) <= 1024 && !strings.HasSuffix(ref, "/") && !strings.ContainsAny(ref, "\x00\r\n \\~^:?*[") && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.Contains(ref, "@{")
}

func ValidRepository(repo string) bool {
	p := strings.Split(repo, "/")
	if len(p) != 2 {
		return false
	}
	for _, s := range p {
		if s == "" || s == "." || s == ".." {
			return false
		}
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
	}
	return true
}

// Validate checks a resolved record. Pending records deliberately fail this.
func (i Identity) Validate(now time.Time) error {
	if !ValidRepository(i.Repository) || i.Runner == "" || i.Generation == "" || i.JobID == "" || i.RunID <= 0 || i.ExpiresAt <= now.Unix() {
		return errors.New("missing or expired job identity")
	}
	if i.WriteRef != "" && !ValidRef(i.WriteRef) {
		return errors.New("invalid write ref")
	}
	if len(i.ReadRefs) == 0 || len(i.ReadRefs) > 3 {
		return errors.New("invalid read refs")
	}
	for _, ref := range i.ReadRefs {
		if !ValidRef(ref) {
			return errors.New("invalid read ref")
		}
	}
	if i.WriteRef != "" && i.ReadRefs[0] != i.WriteRef {
		return errors.New("write scope is not own scope")
	}
	return nil
}

func (i Identity) CanRead(ref string) bool {
	if ref == "" {
		return false
	}
	for _, allowed := range i.ReadRefs {
		if ref == allowed {
			return true
		}
	}
	return false
}

// Reader always rereads the atomic file: revocation must not wait for a cache TTL.
// The directory is host-private and must never be mounted into a guest.
type Reader struct {
	Dir string
	// Nil expects the current UID. Separate cache users must pin the daemon UID.
	OwnerUID *int
}

func (s Reader) trusted(st os.FileInfo) bool {
	uid := os.Getuid()
	if s.OwnerUID != nil {
		uid = *s.OwnerUID
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && st.Mode().Perm()&0027 == 0
}

// Check fails startup on an unreadable, writable-by-readers or wrongly owned directory.
func (s Reader) Check() error {
	st, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || !s.trusted(st) {
		return errors.New("jobs directory must be daemon-owned and at most 0750")
	}
	if err := syscall.Access(s.Dir, 4|1); err != nil {
		return fmt.Errorf("jobs directory requires read/search access: %w", err)
	}
	f, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return nil
	}
	return err
}

func (s Reader) Lookup(ip string) (Identity, error) {
	var i Identity
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Unmap().Is4() {
		return i, errors.New("invalid guest address")
	}
	ip = a.Unmap().String()
	dir, err := os.Lstat(s.Dir)
	if err != nil {
		return i, err
	}
	if !dir.IsDir() || !s.trusted(dir) {
		return i, errors.New("jobs directory is not private")
	}
	path := filepath.Join(s.Dir, ip+".json")
	file, err := os.Lstat(path)
	if err != nil {
		return i, err
	}
	if !file.Mode().IsRegular() || !s.trusted(file) {
		return i, errors.New("job record is not a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return i, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return i, err
	}
	if !os.SameFile(file, opened) || !s.trusted(opened) {
		return i, errors.New("job record changed while opening")
	}
	if err := json.NewDecoder(io.LimitReader(f, 65537)).Decode(&i); err != nil {
		return Identity{}, err
	}
	if i.IP != ip {
		return Identity{}, fmt.Errorf("identity address mismatch")
	}
	if err := i.Validate(time.Now()); err != nil {
		return i, err // preserve the trusted generation of a pending lease
	}
	return i, nil
}
