// Package cacheserver is a stdlib-only implementation of the GitHub Actions
// cache service (v2) protocol, disk-backed unless SetBlobStore moves completed
// blobs and the index to an external store. It lets firerunner microVMs keep
// their `actions/cache` (and setup-*/pnpm/go-build caches built on it) on the
// host's local disk instead of GitHub's hosted cache, so dependency restore
// runs at LAN speed and without GitHub's per-repo quota.
//
// # Protocol
//
// The current `@actions/cache` toolkit (cache service v2, the only version on
// github.com since 2025-02) talks a small Twirp-over-HTTP JSON RPC to the URL
// in ACTIONS_RESULTS_URL, then streams the archive blob to/from a "signed" URL
// the RPC hands back. This server implements both halves:
//
//	POST /twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry
//	POST /twirp/github.actions.results.api.v1.CacheService/FinalizeCacheEntryUpload
//	POST /twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL
//	PUT  /upload/<id>?sig=...     (Azure block-blob protocol: single-shot, or
//	                               ?comp=block&blockid=.. then ?comp=blocklist)
//	GET  /download/<id>?sig=...   (ranged GET + HEAD, i.e. Azure getProperties)
//
// The archive never passes through the RPC: CreateCacheEntry returns an upload
// URL back at this same server, the runner PUTs the blob there (Azure SDK, so
// small blobs are one PUT and large ones are staged blocks committed by a block
// list), and GetCacheEntryDownloadURL returns a download URL served straight
// off disk with Range support. Signed URLs are built from the request's Host
// header so each microVM reaches the server on its own per-slot gateway IP.
//
// # Artifacts
//
// ACTIONS_RESULTS_URL is shared by a second Twirp service, ArtifactService
// (actions/upload-artifact and download-artifact v4+), so a cache-redirect
// golden diverts artifact RPCs here as well. Artifacts are user-facing
// deliverables that must stay visible in the GitHub UI, API and `gh run
// download`, so instead of storing them the server forwards that one service
// to the real GitHub Results endpoint (SetArtifactUpstream) and relays the
// reply verbatim; the archive itself then flows between the guest and the
// signed blob URL GitHub returns, exactly as it would without any redirect:
//
//	POST /twirp/github.actions.results.api.v1.ArtifactService/<Method>  -> upstream
//
// # Isolation and trust model
//
// Cache authority comes from host-owned job records indexed by the connection's
// source address. The daemon pins each guest interface to that address. Guests
// cannot select their repository, write ref, or readable refs. Missing identities
// cannot read or write any repository.
// Every blob request also checks its scope; reservations belong to one VM/job.
// Legacy entries without a ref are never served. See the README for deployment
// requirements, including local routing without source NAT and directory access.
package cacheserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/firerunnerhq/firerunner/internal/gitcache"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
)

const (
	// twirpBase is the fixed Twirp service route the v2 toolkit POSTs to.
	twirpBase = "/twirp/github.actions.results.api.v1.CacheService/"

	// maxIncompleteAge bounds how long a reserved-but-unfinalized entry lingers
	// before the janitor reclaims it (a crashed or abandoned upload).
	maxIncompleteAge = 30 * time.Minute
)

// Entry is one cache record. Reserved on CreateCacheEntry, its blob is streamed
// to /upload/<ID>, and it becomes restorable once FinalizeCacheEntryUpload marks
// it Complete.
type Entry struct {
	ioMu       sync.Mutex // serializes upload and finalize; never copied or persisted
	Ref        string     `json:"ref"`
	Generation string     `json:"generation"`
	JobID      string     `json:"job_id"`
	RunID      int64      `json:"run_id"`
	ID         uint64     `json:"id"`
	Sig        string     `json:"sig"`
	RepoID     string     `json:"repo_id"`
	Key        string     `json:"key"` // lower case, as keys match without regard to it
	// ClientKey is the key as the job wrote it, which a hit must return: setup-node
	// compares it exactly, and saves again when it differs.
	ClientKey string `json:"client_key,omitempty"`
	Version   string `json:"version"`
	Size      int64  `json:"size"`
	Complete  bool   `json:"complete"`
	CreatedAt int64  `json:"created_at"`
	UsedAt    int64  `json:"used_at"`
}

// Server is a disk-backed GitHub Actions cache (v2) server. It is safe for
// concurrent use by many microVMs.
type Server struct {
	dir string
	log *slog.Logger
	mux *http.ServeMux

	mu                sync.Mutex
	entries           map[uint64]*Entry
	nextID            uint64
	maxSize           int64            // 0 = unlimited; total completed-blob bytes to keep
	maxEntry          int64            // 0 = unlimited; hard cap on a single entry's blob
	maxEntries        int              // 0 = unlimited; entries, completed and reserved
	maxRepoEntries    int              // 0 = unlimited; the same, for one repository
	staged            map[uint64]int64 // bytes written so far for a not-yet-finalized entry
	identityMu        sync.Mutex
	identityDenied    map[string]uint64 // by reason, under identityMu
	identityFailures  map[string]bool
	identityWait      time.Duration
	identityWaits     atomic.Uint64
	identityWaitNanos atomic.Uint64
	lookup            func(string) (jobidentity.Identity, error)

	store  BlobStore        // nil keeps blobs on local disk
	stored map[uint64]int64 // with a store: entries whose whole blob reached it, by size
	index  indexSync

	// With a store, local copies of completed blobs serve restores at disk speed;
	// the store stays the durable tier. localCap 0 keeps no copies.
	localCap  int64
	local     map[uint64]int64 // local copies by size
	filling   map[uint64]bool  // copies being fetched from the store
	fillBytes int64            // their total size

	// counters (under mu) for observability via /stats and /metrics.
	hits      uint64
	misses    uint64
	saves     uint64
	evictions uint64

	// artifactProxy forwards ArtifactService RPCs to GitHub (nil = disabled).
	// It and its counters are atomic so the proxy path never waits on the
	// index lock, which cache hit/finalize paths hold across index writes.
	artifactProxy   atomic.Pointer[httputil.ReverseProxy]
	artifactTimeout time.Duration
	bodyIdle        atomic.Int64 // a time.Duration; see SetBodyIdle
	artifactRPCs    atomic.Uint64
	artifactErrors  atomic.Uint64
}

// Stats is a point-in-time snapshot of the cache store, served by /stats and
// /metrics and consumed by `firerunner status`.
type Stats struct {
	IdentityDenied         uint64            `json:"identity_denied"`
	IdentityDeniedByReason map[string]uint64 `json:"identity_denied_by_reason"`
	IdentityWaits          uint64            `json:"identity_waits"`        // requests that waited on a pending lease
	IdentityWaitSeconds    float64           `json:"identity_wait_seconds"` // total time those requests waited
	Entries                int               `json:"entries"`               // completed entries currently stored
	Bytes                  int64             `json:"bytes"`                 // total size of completed blobs
	MaxBytes               int64             `json:"max_bytes"`             // configured cap (0 = unlimited)
	Hits                   uint64            `json:"hits"`                  // download-URL lookups that matched
	Misses                 uint64            `json:"misses"`                // download-URL lookups that did not
	Saves                  uint64            `json:"saves"`                 // entries finalized
	Evictions              uint64            `json:"evictions"`             // entries removed by the size or entry caps
	ArtifactRPCs           uint64            `json:"artifact_rpcs"`         // ArtifactService RPCs forwarded upstream
	ArtifactErrors         uint64            `json:"artifact_errors"`       // artifact RPCs refused, failed, or answered 5xx
}

// snapshot collects current stats. The caller must NOT hold s.mu.
func (s *Server) snapshot() Stats {
	st := Stats{
		IdentityDeniedByReason: make(map[string]uint64, len(denyReasons)),
		IdentityWaits:          s.identityWaits.Load(),
		IdentityWaitSeconds:    time.Duration(s.identityWaitNanos.Load()).Seconds(),
		ArtifactRPCs:           s.artifactRPCs.Load(), ArtifactErrors: s.artifactErrors.Load(),
	}
	s.identityMu.Lock()
	for _, reason := range denyReasons {
		st.IdentityDeniedByReason[reason] = s.identityDenied[reason]
		st.IdentityDenied += s.identityDenied[reason]
	}
	s.identityMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.MaxBytes, st.Hits, st.Misses, st.Saves, st.Evictions = s.maxSize, s.hits, s.misses, s.saves, s.evictions
	for _, e := range s.entries {
		if e.Complete {
			st.Entries++
			st.Bytes += e.Size
		}
	}
	return st
}

// SetMaxSize caps the total size of completed cache blobs kept on disk. When a
// newly finalized entry pushes the total over the cap, the least-recently-used
// completed entries are evicted until it fits (the just-finalized entry is never
// the victim). A non-positive value means unlimited. Call before serving.
func (s *Server) SetMaxSize(n int64) {
	s.mu.Lock()
	s.maxSize = n
	s.mu.Unlock()
}

// SetMaxEntrySize caps the size of any single cache entry's blob. Uploads that
// exceed it are refused (413) mid-stream rather than after the fact, so one job
// cannot fill the host disk with a single unbounded upload. A non-positive value
// means unlimited. Call before serving.
func (s *Server) SetMaxEntrySize(n int64) {
	s.mu.Lock()
	s.maxEntry = n
	s.mu.Unlock()
}

// entryCap returns the effective per-entry byte cap (0 = unlimited).
func (s *Server) entryCap() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxEntry
}

// New opens (creating if needed) a cache store rooted at dir and returns a
// ready-to-serve Server. The on-disk index is loaded so caches survive restarts.
func New(dir string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create cache dir %q: %w", d, err)
		}
	}
	// MkdirAll leaves an already-existing dir's mode untouched, so tighten the
	// root explicitly: it holds index.json (every entry's blob token) and must
	// not be group/world readable even if it was pre-created by the operator.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod cache dir %q: %w", dir, err)
	}
	s := &Server{
		dir:             dir,
		log:             log.With("module", "cacheserver"),
		entries:         make(map[uint64]*Entry),
		staged:          make(map[uint64]int64),
		nextID:          1,
		artifactTimeout: defaultArtifactRPCTimeout,
		identityDenied:  make(map[string]uint64),
		identityWait:    DefaultIdentityWait,
	}
	s.bodyIdle.Store(int64(DefaultBodyIdle))
	if err := s.load(); err != nil {
		return nil, err
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST "+twirpBase+"CreateCacheEntry", s.handleCreate)
	s.mux.HandleFunc("POST "+twirpBase+"FinalizeCacheEntryUpload", s.handleFinalize)
	s.mux.HandleFunc("POST "+twirpBase+"GetCacheEntryDownloadURL", s.handleGetDownloadURL)
	// Any other method, or any of these by GET, is a Twirp bad_route.
	s.mux.HandleFunc(twirpBase+"{method}", twirpBadRoute)
	// So is anything else under /twirp/: another service, or the
	// ArtifactService by a method other than POST.
	s.mux.HandleFunc("/twirp/", twirpBadRoute)
	// A blob by a method its endpoint does not serve, as Azure answers it.
	s.mux.HandleFunc("/upload/{id}", unsupportedVerb)
	s.mux.HandleFunc("/download/{id}", unsupportedVerb)
	s.mux.HandleFunc("POST "+artifactTwirpBase+"{method}", s.handleArtifactProxy)
	s.mux.HandleFunc("PUT /upload/{id}", s.handleUpload)
	s.mux.HandleFunc("GET /download/{id}", s.handleDownload)
	s.mux.HandleFunc("HEAD /download/{id}", s.handleDownload)
	s.mux.HandleFunc("GET /stats", s.handleStats)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	return s, nil
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Also bind at handler entry for in-process callers. Do this before reading
	// the request body, which an old VM could deliberately delay across reuse.
	if r.Context().Value(connectionGenerationKey{}) == nil {
		r = r.WithContext(s.bindGeneration(r.Context(), r.RemoteAddr))
	}
	// The artifact proxy sets its own deadline for a whole RPC.
	if idle := s.bodyIdle.Load(); idle > 0 && r.Body != nil && r.Body != http.NoBody && !strings.HasPrefix(r.URL.Path, artifactTwirpBase) {
		r.Body = &idleBody{ReadCloser: r.Body, rc: http.NewResponseController(w), idle: time.Duration(idle)}
	}
	s.mux.ServeHTTP(w, r)
}

// DefaultBodyIdle is SetBodyIdle's default for the cache-server command.
const DefaultBodyIdle = 60 * time.Second

// SetBodyIdle bounds how long a request body may send nothing. A guest can
// otherwise trickle a body for as long as it likes, holding a connection, a
// goroutine and, for an upload, a reservation, since a large archive leaves
// no overall deadline that would be safe. Each read pushes the connection's
// read deadline idle into the future, so an upload that keeps sending is
// never cut, however long it takes. 0 turns the bound off. Call before
// serving.
func (s *Server) SetBodyIdle(idle time.Duration) { s.bodyIdle.Store(int64(max(idle, 0))) }

// idleBody extends the connection's read deadline before each read of the
// request body (see SetBodyIdle). The deadline errors are the reader's: a
// stalled body fails its upload like a dropped connection.
type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	done bool // the body has returned an error, such as io.EOF
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.done {
		// At EOF net/http clears the deadline and starts reading the
		// connection in the background to notice a client that leaves.
		// A deadline set again here would expire in that read and cancel
		// the request while the handler still works (TestBodyIdleEndsAtEOF).
		return b.ReadCloser.Read(p)
	}
	// A connection that cannot take a deadline keeps the server's own
	// timeouts, as it did before.
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	n, err := b.ReadCloser.Read(p)
	b.done = err != nil
	return n, err
}

// StartJanitor runs a background loop that reclaims abandoned (reserved but
// never finalized) entries until the returned stop function is called.
func (s *Server) StartJanitor() (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		s.gc()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.gc()
			}
		}
	}()
	return func() { close(done) }
}

// --- Twirp message shapes (proto3 JSON, useProtoFieldName: true; int64 as string) ---

type cacheMetadata struct {
	RepositoryID string `json:"repository_id"`
}

type createReq struct {
	Metadata *cacheMetadata `json:"metadata"`
	Key      string         `json:"key"`
	Version  string         `json:"version"`
}
type createResp struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url"`
	Message         string `json:"message,omitempty"`
}

type finalizeReq struct {
	Metadata  *cacheMetadata `json:"metadata"`
	Key       string         `json:"key"`
	SizeBytes string         `json:"size_bytes"`
	Version   string         `json:"version"`
}
type finalizeResp struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entry_id"`
	Message string `json:"message,omitempty"`
}

type getReq struct {
	Metadata    *cacheMetadata `json:"metadata"`
	Key         string         `json:"key"`
	RestoreKeys []string       `json:"restore_keys"`
	Version     string         `json:"version"`
}
type getResp struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url"`
	MatchedKey        string `json:"matched_key"`
	Message           string `json:"message,omitempty"`
}

// handleCreate reserves a new entry and returns an upload URL, or ok:false when
// an entry with the same repo+ref+key+version already exists — completed (immutable) or a
// still-live reservation (another job is creating it). Keeping reservations
// unique per repo+ref+key+version is what makes the finalize step unambiguous, matching
// GitHub's "another job may be creating this cache".
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !decodeTwirp(w, r, &req) {
		return
	}
	if req.Key == "" || req.Version == "" {
		twirpError(w, "invalid_argument", "key and version are required")
		return
	}
	if msg := checkKeys(req.Key, nil, req.Version); msg != "" {
		twirpError(w, "invalid_argument", msg)
		return
	}
	identity := s.identity(r)
	repo := identity.Repository
	key := strings.ToLower(req.Key)
	if identity.WriteRef == "" {
		writeJSON(w, http.StatusOK, createResp{OK: false, Message: "job has no cache write scope"})
		return
	}
	if req.Version == gitcache.EntryVersion && !ownGitEntry(key, repo) {
		// A job that fetched another private repository would otherwise
		// store its objects under this repository's scope, where a fork PR
		// reading the default branch's caches could restore them.
		writeJSON(w, http.StatusOK, createResp{OK: false, Message: "git cache entries are for the job's own repository"})
		return
	}

	s.mu.Lock()
	if e := s.findExact(repo, identity.WriteRef, key, req.Version); e != nil {
		if e.Complete {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, createResp{OK: false, Message: "cache entry already exists"})
			return
		}
		// A reservation for this key+version already exists. Handing out a second
		// one is the confused-deputy bug: the finalize RPC carries no entry id
		// (the v2 toolkit correlates by key+version), so with two live
		// reservations findReserved picks the newest and one caller's Finalize
		// would publish the other caller's bytes. Keep reservations unique — the
		// first reserver owns the key until it finalizes or the reservation goes
		// stale, exactly like GitHub's "another job may be creating this cache".
		if e.UsedAt >= time.Now().Add(-maxIncompleteAge).Unix() {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, createResp{OK: false, Message: "cache entry is already being created"})
			return
		}
		// The existing reservation is stale (its upload never completed); reclaim
		// it so a fresh reservation can take the key over.
		s.removeBlob(e.ID)
		_ = os.RemoveAll(s.tmpDir(e.ID))
		delete(s.entries, e.ID)
		delete(s.staged, e.ID)
	}
	if msg := s.roomForEntry(identity); msg != "" {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, createResp{OK: false, Message: msg})
		return
	}
	e := &Entry{
		ID:         s.nextID,
		Sig:        newToken(),
		RepoID:     repo,
		Ref:        identity.WriteRef,
		Generation: identity.Generation,
		JobID:      identity.JobID,
		RunID:      identity.RunID,
		Key:        key,
		ClientKey:  req.Key,
		Version:    req.Version,
		Size:       -1,
		CreatedAt:  time.Now().Unix(),
		UsedAt:     time.Now().Unix(),
	}
	s.nextID++
	s.entries[e.ID] = e
	if err := s.save(); err != nil {
		delete(s.entries, e.ID)
		s.mu.Unlock()
		s.log.Error("cache index save failed", "err", err)
		twirpError(w, "internal", "cache index unavailable")
		return
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, createResp{
		OK:              true,
		SignedUploadURL: s.blobURL(r, "upload", e.ID, e.Sig),
	})
}

// handleFinalize marks a reserved entry complete once its blob is on disk.
func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	var req finalizeReq
	if !decodeTwirp(w, r, &req) {
		return
	}
	identity := s.identity(r)
	repo := identity.Repository
	key := strings.ToLower(req.Key)

	s.mu.Lock()
	e := s.findReserved(repo, identity.WriteRef, key, req.Version)
	s.mu.Unlock()
	if e == nil || !owns(identity, e) {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: "no owned cache reservation"})
		return
	}
	e.ioMu.Lock()
	defer e.ioMu.Unlock()
	// The upload may have held the lock while the job exited. Recheck authority.
	identity = s.identity(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[e.ID] != e || e.Complete || !owns(identity, e) {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: "cache reservation expired"})
		return
	}
	size, uploaded := s.stored[e.ID]
	if s.store == nil {
		if fi, err := os.Stat(s.blobPath(e.ID)); err == nil {
			size, uploaded = fi.Size(), true
		}
	}
	if !uploaded {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: "cache blob was not uploaded"})
		return
	}
	if want, err := strconv.ParseInt(req.SizeBytes, 10, 64); err == nil && want >= 0 && want != size {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: fmt.Sprintf("size mismatch: got %d, want %d", size, want)})
		return
	}
	e.Size = size
	e.Complete = true
	e.UsedAt = time.Now().Unix()
	delete(s.staged, e.ID) // it now counts as a completed blob, not in-flight
	delete(s.stored, e.ID)
	if s.store != nil {
		// Only now: an upload never finalized must not displace a copy.
		s.keepLocal(e.ID, size)
	}
	s.saves++
	s.evictLRU(e.ID)
	if err := s.save(); err != nil {
		s.log.Error("cache index save failed", "err", err)
		twirpError(w, "internal", "cache index unavailable")
		return
	}
	s.log.Info("cache entry finalized", "id", e.ID, "key", e.Key, "size", e.Size)
	writeJSON(w, http.StatusOK, finalizeResp{OK: true, EntryID: strconv.FormatUint(e.ID, 10)})
}

// handleGetDownloadURL resolves the best matching entry across the caller's
// read refs in order (own, PR base, default; see match) and returns a download
// URL, or ok:false on a miss.
func (s *Server) handleGetDownloadURL(w http.ResponseWriter, r *http.Request) {
	var req getReq
	if !decodeTwirp(w, r, &req) {
		return
	}
	// Each restore key costs two scans of every entry under the lock.
	if msg := checkKeys(req.Key, req.RestoreKeys, req.Version); msg != "" {
		twirpError(w, "invalid_argument", msg)
		return
	}
	identity := s.identity(r)
	repo := identity.Repository

	s.mu.Lock()
	e := s.match(repo, identity.ReadRefs, strings.ToLower(req.Key), req.RestoreKeys, req.Version)
	if e != nil {
		e.UsedAt = time.Now().Unix()
		s.hits++
		_ = s.save()
	} else {
		s.misses++
	}
	s.mu.Unlock()

	if e == nil {
		writeJSON(w, http.StatusOK, getResp{OK: false})
		return
	}
	writeJSON(w, http.StatusOK, getResp{
		OK:                true,
		SignedDownloadURL: s.blobURL(r, "download", e.ID, e.Sig),
		MatchedKey:        e.matchedKey(req.Key),
	})
}

// matchedKey is the key to report for a hit on asked. An entry from before
// ClientKey has only the lower-case one, so an exact hit echoes the job's own:
// entries are immutable, and it would otherwise warn until its key changed.
func (e *Entry) matchedKey(asked string) string {
	switch {
	case e.ClientKey != "":
		return e.ClientKey
	case e.Key == strings.ToLower(asked):
		return asked
	}
	return e.Key
}

// handleUpload accepts the archive blob. It speaks the subset of the Azure
// block-blob API the cache toolkit's Azure SDK emits: a single-shot PUT for
// blobs up to 128 MiB, or staged blocks (?comp=block&blockid=..) committed by a
// block list (?comp=blocklist) for larger ones.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	e, ok := s.authEntry(w, r)
	if !ok {
		return
	}
	e.ioMu.Lock()
	defer e.ioMu.Unlock()
	if !owns(s.identity(r), e) {
		azureError(w, http.StatusForbidden, azNotPermitted, "this job may not access this blob")
		return
	}
	// Entries are immutable once finalized: the blob token is handed out for both
	// download and upload, so without this check any client that can read a
	// completed entry could also overwrite or truncate it. Writes are only
	// accepted while the entry is still reserved.
	s.mu.Lock()
	complete := e.Complete
	s.mu.Unlock()
	if complete {
		azureError(w, http.StatusConflict, azImmutable, "cache entry already finalized")
		return
	}
	switch r.URL.Query().Get("comp") {
	case "block":
		s.uploadBlock(w, r, e)
	case "blocklist":
		s.commitBlockList(w, r, e)
	case "":
		s.uploadSingle(w, r, e)
	default:
		azureError(w, http.StatusBadRequest, azBadQuery, "comp must be block or blocklist")
	}
}

func (s *Server) uploadSingle(w http.ResponseWriter, r *http.Request, e *Entry) {
	body := r.Body
	if cap := s.entryCap(); cap > 0 {
		body = http.MaxBytesReader(w, r.Body, cap)
	}
	// A single PUT replaces whatever the entry held, as in Azure. Drop staged
	// blocks and an earlier blob first, so the bytes counted are the bytes on
	// disk: finishUpload records only this blob, and left behind, the blocks
	// would sit on disk uncounted.
	_ = os.RemoveAll(s.tmpDir(e.ID))
	_ = os.Remove(s.blobPath(e.ID))
	s.clearStaged(e.ID)
	br := &budgetReader{r: body, s: s, e: e}
	n, err := writeFile(s.blobPath(e.ID), br)
	br.settle(err != nil)
	if err != nil {
		_ = os.Remove(s.blobPath(e.ID)) // don't leave an oversized/partial blob squatting disk
		s.clearStaged(e.ID)
		s.uploadError(w, e, err)
		return
	}
	if err := s.finishUpload(r, e, n); err != nil {
		s.uploadError(w, e, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) uploadBlock(w http.ResponseWriter, r *http.Request, e *Entry) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		azureError(w, http.StatusBadRequest, azMissingQuery, "blockid is required")
		return
	}
	if len(blockID) > maxBlockIDLen {
		azureError(w, http.StatusBadRequest, azBadQuery, "blockid too long")
		return
	}
	dir := s.tmpDir(e.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.blobInternalError(w, e, err)
		return
	}
	body := r.Body
	if cap := s.entryCap(); cap > 0 {
		// Bound the block by the entry's remaining budget so staged blocks can't
		// accumulate past the per-entry cap before the block list is committed.
		remaining := cap - s.stagedBytes(e.ID)
		if remaining < 0 {
			remaining = 0
		}
		body = http.MaxBytesReader(w, r.Body, remaining)
	}
	blockPath := filepath.Join(dir, hex.EncodeToString([]byte(blockID)))
	br := &budgetReader{r: body, s: s, e: e}
	_, err := writeFile(blockPath, br)
	br.settle(err != nil)
	if err != nil {
		_ = os.Remove(blockPath)
		s.uploadError(w, e, err)
		return
	}
	if !s.reserved(e) {
		// Reclaimed while the block streamed in (see finishUpload).
		_ = os.RemoveAll(dir)
		azureError(w, http.StatusNotFound, azBlobNotFound, "cache reservation expired")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) commitBlockList(w http.ResponseWriter, r *http.Request, e *Entry) {
	var list struct {
		Latest      []string `xml:"Latest"`
		Uncommitted []string `xml:"Uncommitted"`
		Committed   []string `xml:"Committed"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlockListBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			azureError(w, http.StatusRequestEntityTooLarge, azTooLarge, "block list too large")
			return
		}
		if clientGone(err) {
			azureError(w, http.StatusBadRequest, "IncompleteBody", "the request body ended early")
			return
		}
		s.blobInternalError(w, e, err)
		return
	}
	if err := xml.Unmarshal(body, &list); err != nil {
		azureError(w, http.StatusBadRequest, azBadXML, "block list is not valid XML")
		return
	}
	ids := list.Latest
	ids = append(ids, list.Uncommitted...)
	ids = append(ids, list.Committed...)
	if len(ids) == 0 {
		azureError(w, http.StatusBadRequest, azBadBlockList, "empty block list")
		return
	}
	if len(ids) > maxBlocks {
		// Each id costs an open and a copy, so a list of one id repeated
		// would otherwise multiply work without adding bytes.
		azureError(w, http.StatusBadRequest, "BlockListTooLong", "a block list names at most 50,000 blocks")
		return
	}

	// Check the whole list before writing or claiming a byte. Assembly claims
	// what it writes, and a claim can evict other entries for good, so a list
	// that will fail must fail first: a block named twice (one staged block
	// copied over and over would claim bytes nobody uploaded), or a block never
	// uploaded. Staged blocks already count against the per-entry cap, so with
	// each named once the assembly cannot outgrow what was uploaded.
	dir := s.tmpDir(e.ID)
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			azureError(w, http.StatusBadRequest, azBadBlockList, "the block list names a block more than once")
			return
		}
		seen[id] = struct{}{}
		fi, err := os.Stat(filepath.Join(dir, hex.EncodeToString([]byte(id))))
		if err != nil || !fi.Mode().IsRegular() {
			azureError(w, http.StatusBadRequest, azBadBlockList, "the block list names a block that was not uploaded")
			return
		}
	}

	// Assemble into a temp file and rename over the blob only once it is fully
	// built and within the per-entry cap. Streaming straight into the live blob
	// would truncate a previously finalized entry the instant a commit arrives,
	// even if a staged block is missing or the total exceeds the cap. Both are
	// checked again while copying: a block can change after the check above.
	tmp := s.blobPath(e.ID) + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		s.blobInternalError(w, e, err)
		return
	}
	cap := s.entryCap()
	// The assembled blob sits beside its blocks until they are removed, so it
	// is claimed too; finishUpload then records only the blob.
	br := &budgetReader{s: s, e: e}
	var total int64
	for _, id := range ids {
		blockPath := filepath.Join(dir, hex.EncodeToString([]byte(id)))
		f, err := os.Open(blockPath)
		if err != nil {
			out.Close()
			_ = os.Remove(tmp)
			br.settle(true)
			azureError(w, http.StatusBadRequest, azBadBlockList, "the block list names a block that was not uploaded")
			return
		}
		br.r = f
		n, err := io.Copy(out, br)
		f.Close()
		total += n
		if err != nil {
			out.Close()
			_ = os.Remove(tmp)
			br.settle(true)
			s.uploadError(w, e, err)
			return
		}
		if cap > 0 && total > cap {
			out.Close()
			_ = os.Remove(tmp)
			_ = os.RemoveAll(dir)
			s.clearStaged(e.ID)
			azureError(w, http.StatusRequestEntityTooLarge, azTooLarge, "cache entry exceeds max entry size")
			return
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		br.settle(true)
		s.blobInternalError(w, e, err)
		return
	}
	if err := os.Rename(tmp, s.blobPath(e.ID)); err != nil {
		_ = os.Remove(tmp)
		br.settle(true)
		s.blobInternalError(w, e, err)
		return
	}
	_ = os.RemoveAll(dir)
	if err := s.finishUpload(r, e, total); err != nil {
		s.uploadError(w, e, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleDownload serves a completed entry's blob straight off disk. ServeContent
// gives Range + HEAD (the Azure getProperties + segmented download the toolkit
// uses) for free.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	e, ok := s.authEntry(w, r)
	if !ok {
		return
	}
	f, err := s.openLocal(e.ID)
	if err != nil {
		if s.store == nil {
			azureError(w, http.StatusNotFound, azBlobNotFound, "the specified blob does not exist")
			return
		}
		if r.Method == http.MethodHead || r.Header.Get("Range") == "" {
			s.log.Info("cache entry served", "id", e.ID, "key", e.Key, "size", e.Size, "method", r.Method, "from", "store")
		}
		s.serveFromStore(w, r, e)
		s.fillLocal(e)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.blobInternalError(w, e, err)
		return
	}
	// A restore issues one getProperties (HEAD) then many ranged GETs; log the
	// HEAD and any un-ranged GET at INFO so a restore shows up once, and leave
	// the per-range chatter out of the default log.
	if r.Method == http.MethodHead || r.Header.Get("Range") == "" {
		s.log.Info("cache entry served", "id", e.ID, "key", e.Key, "size", e.Size, "method", r.Method)
	}
	// Refuse an unsatisfiable range Azure-shaped, as the store path does,
	// before ServeContent would answer it in plain text.
	if rh := r.Header.Get("Range"); rh != "" {
		if _, _, _, ok := parseRange(rh, fi.Size()); !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", fi.Size()))
			azureError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "the range specified is invalid for the current size of the resource")
			return
		}
	}
	http.ServeContent(w, r, strconv.FormatUint(e.ID, 10), fi.ModTime(), f)
}

// handleStats serves a JSON snapshot of the cache store for `firerunner status`.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

// handleMetrics serves the same numbers in Prometheus text exposition format so
// the cache-server can be scraped without any third-party client library.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	st := s.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP firerunner_cache_identity_denied_total Cache operations denied for unavailable identity, by reason.\n# TYPE firerunner_cache_identity_denied_total counter\n")
	for _, reason := range denyReasons {
		fmt.Fprintf(w, "firerunner_cache_identity_denied_total{reason=%q} %d\n", reason, st.IdentityDeniedByReason[reason])
	}
	fmt.Fprintf(w, "# HELP firerunner_cache_identity_waits_total Cache operations that waited for a pending job identity.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_identity_waits_total counter\nfirerunner_cache_identity_waits_total %d\n", st.IdentityWaits)
	fmt.Fprintf(w, "# HELP firerunner_cache_identity_wait_seconds_total Time cache operations spent waiting for a pending job identity.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_identity_wait_seconds_total counter\nfirerunner_cache_identity_wait_seconds_total %g\n", st.IdentityWaitSeconds)
	fmt.Fprintf(w, "# HELP firerunner_cache_entries Completed cache entries stored.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_entries gauge\nfirerunner_cache_entries %d\n", st.Entries)
	fmt.Fprintf(w, "# HELP firerunner_cache_bytes Total size of completed cache blobs.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_bytes gauge\nfirerunner_cache_bytes %d\n", st.Bytes)
	fmt.Fprintf(w, "# HELP firerunner_cache_max_bytes Configured size cap (0 = unlimited).\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_max_bytes gauge\nfirerunner_cache_max_bytes %d\n", st.MaxBytes)
	fmt.Fprintf(w, "# HELP firerunner_cache_hits_total Download lookups that matched an entry.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_hits_total counter\nfirerunner_cache_hits_total %d\n", st.Hits)
	fmt.Fprintf(w, "# HELP firerunner_cache_misses_total Download lookups with no match.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_misses_total counter\nfirerunner_cache_misses_total %d\n", st.Misses)
	fmt.Fprintf(w, "# HELP firerunner_cache_saves_total Entries finalized.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_saves_total counter\nfirerunner_cache_saves_total %d\n", st.Saves)
	fmt.Fprintf(w, "# HELP firerunner_cache_evictions_total Entries removed by the size or entry caps.\n")
	fmt.Fprintf(w, "# TYPE firerunner_cache_evictions_total counter\nfirerunner_cache_evictions_total %d\n", st.Evictions)
	fmt.Fprintf(w, "# HELP firerunner_artifact_rpcs_total ArtifactService RPCs forwarded to the upstream.\n")
	fmt.Fprintf(w, "# TYPE firerunner_artifact_rpcs_total counter\nfirerunner_artifact_rpcs_total %d\n", st.ArtifactRPCs)
	fmt.Fprintf(w, "# HELP firerunner_artifact_errors_total Artifact RPCs refused, failed, or answered 5xx.\n")
	fmt.Fprintf(w, "# TYPE firerunner_artifact_errors_total counter\nfirerunner_artifact_errors_total %d\n", st.ArtifactErrors)
}

// authEntry resolves and authorizes the {id} + ?sig= on a blob request.
func (s *Server) authEntry(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		azureError(w, http.StatusBadRequest, azBadResourceName, "blob id is not a number")
		return nil, false
	}
	s.mu.Lock()
	e := s.entries[id]
	s.mu.Unlock()
	if e == nil {
		azureError(w, http.StatusNotFound, azBlobNotFound, "the specified blob does not exist")
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("sig")), []byte(e.Sig)) != 1 {
		azureError(w, http.StatusForbidden, azAuthFailed, "the blob signature is missing or wrong")
		return nil, false
	}
	identity := s.identity(r)
	s.mu.Lock()
	complete := e.Complete
	s.mu.Unlock()
	if r.Method == http.MethodPut {
		if !owns(identity, e) {
			azureError(w, http.StatusForbidden, azNotPermitted, "this job may not access this blob")
			return nil, false
		}
	} else if !complete || e.RepoID != identity.Repository || !identity.CanRead(e.Ref) {
		azureError(w, http.StatusForbidden, azNotPermitted, "this job may not access this blob")
		return nil, false
	}
	return e, true
}

// --- matching (caller holds s.mu) ---

func (s *Server) findExact(repo, ref, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.RepoID == repo && ref != "" && e.Ref == ref && e.Key == key && e.Version == version {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

func (s *Server) findReserved(repo, ref, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if !e.Complete && e.RepoID == repo && ref != "" && e.Ref == ref && e.Key == key && e.Version == version {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

const (
	// maxKeys and maxKeyLen are @actions/cache's own limits: a key and its
	// restore keys, at most ten in all, each at most 512 characters.
	maxKeys   = 10
	maxKeyLen = 512
	// DefaultMaxEntries and DefaultMaxRepoEntries are the cache-server
	// command's defaults for SetMaxEntries: far more than a busy host or repo
	// keeps, few enough that scanning them all under the lock stays cheap.
	DefaultMaxEntries     = 20000
	DefaultMaxRepoEntries = 2000
	// maxJobReservations is how many unfinished uploads one job may hold.
	// The toolkit saves one cache at a time per step.
	maxJobReservations = 64
)

// ownGitEntry reports whether a git cache key (lower-cased) names the
// repository of scope, the identity's ScopeKey: "<name>#<id>".
func ownGitEntry(key, scope string) bool {
	repo, ok := gitcache.KeyRepository(key)
	name, _, _ := strings.Cut(scope, "#")
	return ok && repo == name
}

// checkKeys returns why a request's keys are refused, or "" when they are
// within the toolkit's limits. Lengths are in characters, never stricter
// than the toolkit, which counts UTF-16 units.
func checkKeys(key string, restoreKeys []string, version string) string {
	if 1+len(restoreKeys) > maxKeys {
		return fmt.Sprintf("at most %d keys, the key and its restore keys", maxKeys)
	}
	for _, k := range append([]string{key, version}, restoreKeys...) {
		if utf8.RuneCountInString(k) > maxKeyLen {
			return fmt.Sprintf("keys and versions are at most %d characters", maxKeyLen)
		}
	}
	return ""
}

// SetMaxEntries caps how many entries, completed and reserved, the cache
// holds in all and for one repository. Byte caps alone let a job finalize any
// number of empty entries, each kept in the index and scanned on every
// lookup. A new reservation past the repository's cap evicts that
// repository's least-recently-used completed entry, so one repository cannot
// flush the others; past the total it evicts the host's. Either is refused
// when only reservations are left. A non-positive value means unlimited.
// Call before serving.
func (s *Server) SetMaxEntries(total, perRepo int) {
	s.mu.Lock()
	s.maxEntries, s.maxRepoEntries = total, perRepo
	s.mu.Unlock()
}

// roomForEntry makes room for one more entry for the job identity holds, and
// returns why it cannot, or "". The caller holds s.mu.
func (s *Server) roomForEntry(identity jobidentity.Identity) string {
	var repo []*Entry
	reserved := 0
	for _, e := range s.entries {
		if e.RepoID == identity.Repository {
			repo = append(repo, e)
			if !e.Complete && e.JobID == identity.JobID && e.RunID == identity.RunID && e.Generation == identity.Generation {
				reserved++
			}
		}
	}
	if reserved >= maxJobReservations {
		return "job has too many unfinished cache uploads"
	}
	evicted := 0
	defer func() {
		if evicted > 0 {
			s.log.Info("evicted cache entries (entry cap)", "count", evicted)
			if err := s.save(); err != nil {
				s.log.Warn("cache index save failed", "err", err)
			}
		}
	}()
	if s.maxRepoEntries > 0 && len(repo) >= s.maxRepoEntries {
		evicted += s.evictOldest(repo, len(repo)-s.maxRepoEntries+1)
		if len(repo)-evicted >= s.maxRepoEntries {
			return "repository has too many cache entries"
		}
	}
	if s.maxEntries > 0 && len(s.entries) >= s.maxEntries {
		all := make([]*Entry, 0, len(s.entries))
		for _, e := range s.entries {
			all = append(all, e)
		}
		evicted += s.evictOldest(all, len(s.entries)-s.maxEntries+1)
		if len(s.entries) >= s.maxEntries {
			return "cache has too many entries"
		}
	}
	return ""
}

// evictOldest removes up to n of entries' completed ones, least recently used
// first, and returns how many it removed. It sorts once, so trimming a large
// index (after --max-entries is lowered) is not quadratic. The caller holds
// s.mu and saves the index.
func (s *Server) evictOldest(entries []*Entry, n int) int {
	done := entries[:0:0]
	for _, e := range entries {
		if e.Complete {
			done = append(done, e)
		}
	}
	sort.Slice(done, func(i, j int) bool { return lessRecentlyUsed(done[i], done[j]) })
	n = min(n, len(done))
	for _, e := range done[:n] {
		s.removeBlob(e.ID)
		_ = os.RemoveAll(s.tmpDir(e.ID))
		delete(s.entries, e.ID)
		delete(s.staged, e.ID)
		s.evictions++
	}
	return n
}

// match exhausts each scope before trying the next. Within a scope an exact
// key beats its prefix; restore keys are tried in order, exact then prefix.
func (s *Server) match(repo string, readRefs []string, key string, restoreKeys []string, version string) *Entry {
	for _, ref := range readRefs {
		if e := s.completeExact(repo, ref, key, version); e != nil {
			return e
		}
		if key != "" {
			if e := s.completePrefix(repo, ref, key, version); e != nil {
				return e
			}
		}
		for _, rk := range restoreKeys {
			rk = strings.ToLower(rk)
			if e := s.completeExact(repo, ref, rk, version); e != nil {
				return e
			}
			if e := s.completePrefix(repo, ref, rk, version); e != nil {
				return e
			}
		}
	}
	return nil
}

// restorable reports whether a lookup by repo may return e. A git entry must
// name repo itself: one saved before git entries did (or naming another
// repository) could hold another private repository's objects.
func restorable(e *Entry, repo string) bool {
	return e.Version != gitcache.EntryVersion || ownGitEntry(e.Key, repo)
}

func (s *Server) completeExact(repo, ref, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.Complete && ref != "" && e.Ref == ref && e.RepoID == repo && e.Version == version && e.Key == key && restorable(e, repo) {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

func (s *Server) completePrefix(repo, ref, prefix, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.Complete && ref != "" && e.Ref == ref && e.RepoID == repo && e.Version == version && strings.HasPrefix(e.Key, prefix) && restorable(e, repo) {
			if newer(best, e) {
				best = e
			}
		}
	}
	return best
}

// newer reports whether e should replace best as the "newest" match: later
// CreatedAt wins, ties broken by higher ID (entries are created monotonically,
// so this is stable even when several land in the same second).
func newer(best, e *Entry) bool {
	if best == nil {
		return true
	}
	if e.CreatedAt != best.CreatedAt {
		return e.CreatedAt > best.CreatedAt
	}
	return e.ID > best.ID
}

// --- storage helpers ---

func (s *Server) blobPath(id uint64) string {
	return filepath.Join(s.dir, "blobs", strconv.FormatUint(id, 10))
}
func (s *Server) tmpDir(id uint64) string {
	return filepath.Join(s.dir, "tmp", strconv.FormatUint(id, 10))
}

func (s *Server) blobURL(r *http.Request, kind string, id uint64, sig string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%d?sig=%s", scheme, r.Host, kind, id, sig)
}

// evictLRU keeps the total size of completed blobs under s.maxSize by deleting
// the least-recently-used completed entries (oldest UsedAt first). keepID is the
// entry that just triggered the check and is never evicted, so a single blob
// larger than the cap is still served rather than deleting the thing just
// stored. In-flight (staged but not-yet-finalized) bytes count toward the total
// so a wave of concurrent uploads cannot blow past the cap before any of them
// finalizes. The caller must hold s.mu; it does not save the index (the caller
// does). A non-positive cap disables eviction.
func (s *Server) evictLRU(keepID uint64) {
	if s.maxSize <= 0 {
		return
	}
	total := s.bytesUsed()
	for total > s.maxSize {
		var victim *Entry
		for _, e := range s.entries {
			if !e.Complete || e.ID == keepID {
				continue
			}
			if victim == nil || lessRecentlyUsed(e, victim) {
				victim = e
			}
		}
		if victim == nil {
			break // only the kept entry (and in-flight uploads) are left
		}
		total -= victim.Size
		s.removeBlob(victim.ID)
		_ = os.RemoveAll(s.tmpDir(victim.ID))
		delete(s.entries, victim.ID)
		delete(s.staged, victim.ID)
		s.evictions++
		s.log.Info("evicted cache entry (size cap)", "id", victim.ID, "key", victim.Key, "size", victim.Size)
	}
}

// bytesUsed is the size of all completed blobs plus every in-flight upload's
// staged bytes. The caller must hold s.mu.
func (s *Server) bytesUsed() int64 {
	var total int64
	for _, e := range s.entries {
		if e.Complete {
			total += e.Size
		}
	}
	for _, n := range s.staged {
		total += n
	}
	return total
}

// claimChunk is the most an upload claims ahead of its reads, so a streaming
// upload takes s.mu once per chunk rather than once per read.
const claimChunk = 8 << 20

// claimStep is how far ahead of its reads an upload claims. A claim can evict
// completed entries for bytes that never arrive, so the step stays a small
// share of maxSize.
func (s *Server) claimStep() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxSize <= 0 {
		return claimChunk
	}
	return min(claimChunk, max(s.maxSize/64, 1))
}

var (
	errCacheFull       = errors.New("cache is full")
	errReservationGone = errors.New("cache reservation expired")
)

// claim counts n more in-flight bytes for e before they reach the disk.
// Eviction only removes completed entries, so without this a job could
// reserve many keys, stream into each and never finalize, filling the disk
// past maxSize for as long as the janitor leaves the reservations. claim
// refuses with errCacheFull when in-flight bytes would exceed maxSize and
// other uploads hold part of them; otherwise it evicts least-recently-used
// completed entries to make room. An entry alone may still exceed maxSize, as
// evictLRU allows, so disk use stays within the larger of maxSize and one
// entry's cap. It refuses with errReservationGone once e is no longer the
// reserved entry.
func (s *Server) claim(e *Entry, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[e.ID] != e {
		return errReservationGone
	}
	s.staged[e.ID] += n
	if s.maxSize <= 0 {
		return nil
	}
	// Refuse before evicting: when in-flight bytes alone are over the cap,
	// evicting completed entries cannot make room, only lose them.
	var inFlight int64
	for _, b := range s.staged {
		inFlight += b
	}
	if inFlight > s.maxSize && inFlight > s.staged[e.ID] {
		if s.staged[e.ID] -= n; s.staged[e.ID] <= 0 {
			delete(s.staged, e.ID)
		}
		return errCacheFull
	}
	evictions := s.evictions
	s.evictLRU(0) // IDs start at 1, so every completed entry is a candidate
	if s.evictions != evictions {
		if err := s.save(); err != nil {
			s.log.Warn("cache index save failed", "err", err)
		}
	}
	return nil
}

// release returns n claimed bytes that never reached the disk, or that a
// failed upload removed.
func (s *Server) release(e *Entry, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[e.ID] != e {
		return
	}
	if s.staged[e.ID] -= n; s.staged[e.ID] <= 0 {
		delete(s.staged, e.ID)
	}
}

// budgetReader claims bytes for e as an upload streams through it, ahead of
// the write, in claimChunk steps. It fails the read, writing nothing more,
// when a claim is refused.
type budgetReader struct {
	r       io.Reader
	s       *Server
	e       *Entry
	claimed int64 // bytes claimed for e so far
	read    int64 // bytes passed on to the writer
	step    int64 // claim-ahead size, from claimStep on the first read
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.step == 0 {
		b.step = b.s.claimStep()
	}
	n, err := b.r.Read(p)
	if need := b.read + int64(n) - b.claimed; need > 0 {
		// Claim ahead, and fall back to the exact need near a full cache.
		cerr := b.s.claim(b.e, max(need, b.step))
		if cerr == errCacheFull && need < b.step {
			cerr = b.s.claim(b.e, need)
		} else if cerr == nil {
			need = max(need, b.step)
		}
		if cerr != nil {
			return 0, cerr
		}
		b.claimed += need
	}
	b.read += int64(n)
	return n, err
}

// settle returns what was claimed beyond the bytes read; failed releases all
// of it, for an upload whose bytes were removed.
func (b *budgetReader) settle(failed bool) {
	extra := b.claimed - b.read
	if failed {
		extra = b.claimed
	}
	if extra > 0 {
		b.s.release(b.e, extra)
	}
	b.claimed -= extra
}

// lessRecentlyUsed reports whether a is a better eviction victim than b: older
// UsedAt loses first, ties broken by lower ID (the older entry).
func lessRecentlyUsed(a, b *Entry) bool {
	if a.UsedAt != b.UsedAt {
		return a.UsedAt < b.UsedAt
	}
	return a.ID < b.ID
}

// gc reclaims reserved-but-unfinalized entries whose blob upload never completed.
func (s *Server) gc() {
	cutoff := time.Now().Add(-maxIncompleteAge).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed int
	for id, e := range s.entries {
		if !e.Complete && e.UsedAt < cutoff {
			s.removeBlob(id)
			_ = os.RemoveAll(s.tmpDir(id))
			delete(s.entries, id)
			delete(s.staged, id)
			removed++
		}
	}
	if removed > 0 {
		_ = s.save()
		s.log.Info("reclaimed abandoned cache entries", "count", removed)
	}
}

// --- index persistence (caller holds s.mu for save; load runs before serving) ---

func (s *Server) indexPath() string { return filepath.Join(s.dir, "index.json") }

func (s *Server) load() error {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cache index: %w", err)
	}
	return s.loadIndex(data)
}

func (s *Server) loadIndex(data []byte) error {
	var entries []*Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("decode cache index: %w", err)
	}
	for _, e := range entries {
		s.entries[e.ID] = e
		if e.ID >= s.nextID {
			s.nextID = e.ID + 1
		}
	}
	return nil
}

func (s *Server) save() error {
	entries := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if s.store != nil {
		s.index.mu.Lock()
		s.index.data, s.index.dirty = data, true
		s.index.mu.Unlock()
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

// --- small helpers ---

func writeFile(path string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
}

// uploadError answers a failed upload as Azure Storage would: an over-cap body
// (http.MaxBytesError) is a 413, a full cache a 507, a reclaimed reservation a
// 404, a store outage a 507 (which the Azure SDK does not retry), anything
// else a 500 that names no host detail.
func (s *Server) uploadError(w http.ResponseWriter, e *Entry, err error) {
	var mbe *http.MaxBytesError
	switch {
	case clientGone(err):
		// The client hung up mid-upload; nobody reads this answer.
		s.log.Warn("cache upload ended by the client", "id", e.ID, "key", e.Key, "err", err)
		azureError(w, http.StatusBadRequest, "IncompleteBody", "the request body ended early")
	case errors.As(err, &mbe):
		azureError(w, http.StatusRequestEntityTooLarge, azTooLarge, "cache entry exceeds max entry size")
	case errors.Is(err, errCacheFull):
		azureError(w, http.StatusInsufficientStorage, azCacheFull, err.Error())
	case errors.Is(err, errReservationGone):
		azureError(w, http.StatusNotFound, azBlobNotFound, err.Error())
	case errors.Is(err, errStoreUnavailable):
		// 507, not a retryable status: see errStoreUnavailable.
		azureError(w, http.StatusInsufficientStorage, azStoreDown, err.Error())
	default:
		s.log.Error("cache upload failed", "id", e.ID, "key", e.Key, "err", err)
		azureError(w, http.StatusInternalServerError, azInternal, "cache upload failed")
	}
}

// clientGone reports whether err means the client went away while sending:
// its connection closed or reset, the request was cancelled, or its body
// stopped sending for longer than SetBodyIdle allows.
func clientGone(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}

// staged-byte accounting bounds the disk a not-yet-finalized entry occupies:
// uploads claim their bytes as they stream (see claim), and finishUpload
// records an assembled blob.
// reserved reports whether e is still the reserved entry for its ID.
func (s *Server) reserved(e *Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[e.ID] == e
}
func (s *Server) stagedBytes(id uint64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staged[id]
}

// clearStaged forgets a failed upload, including an earlier one's blob in
// the store: the failure removed its local file, so finalize must not
// complete the entry with it.
func (s *Server) clearStaged(id uint64) {
	s.mu.Lock()
	delete(s.staged, id)
	delete(s.stored, id)
	s.mu.Unlock()
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// twirpStatus is the HTTP status the Twirp specification gives each error
// code (https://twitchtv.github.io/twirp/docs/spec_v7.html#error-codes).
var twirpStatus = map[string]int{
	"canceled":            http.StatusRequestTimeout,
	"unknown":             http.StatusInternalServerError,
	"invalid_argument":    http.StatusBadRequest,
	"malformed":           http.StatusBadRequest,
	"deadline_exceeded":   http.StatusRequestTimeout,
	"not_found":           http.StatusNotFound,
	"bad_route":           http.StatusNotFound,
	"already_exists":      http.StatusConflict,
	"permission_denied":   http.StatusForbidden,
	"unauthenticated":     http.StatusUnauthorized,
	"resource_exhausted":  http.StatusTooManyRequests,
	"failed_precondition": http.StatusPreconditionFailed,
	"aborted":             http.StatusConflict,
	"out_of_range":        http.StatusBadRequest,
	"unimplemented":       http.StatusNotImplemented,
	"internal":            http.StatusInternalServerError,
	"unavailable":         http.StatusServiceUnavailable,
	"dataloss":            http.StatusInternalServerError,
}

// twirpError writes a Twirp error, at the HTTP status the specification gives
// its code. Misses and reservation conflicts are not errors to the toolkit:
// they are ok:false 200s, written inline.
func twirpError(w http.ResponseWriter, code, msg string) {
	status, ok := twirpStatus[code]
	if !ok {
		code, status = "internal", http.StatusInternalServerError
	}
	twirpErrorAt(w, status, code, msg)
}

// twirpErrorAt writes a Twirp error at a status of the caller's choosing. Only
// the artifact proxy uses it: as a gateway it answers its upstream's failures
// with 502 and 504, which the artifact client retries, and a Twirp client reads
// the code from the body whatever the status.
func twirpErrorAt(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "msg": msg})
}

// azureError writes a Blob service error as Azure Storage does
// (https://learn.microsoft.com/rest/api/storageservices/status-and-error-codes2):
// the code in the x-ms-error-code header, which the Azure SDK in the toolkit
// reads first, and an XML Error body with the code and a message. Codes are
// Azure's own where one fits.
func azureError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-error-code", code)
	w.WriteHeader(status)
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?><Error><Code>`)
	_ = xml.EscapeText(&body, []byte(code))
	body.WriteString("</Code><Message>")
	_ = xml.EscapeText(&body, []byte(msg))
	body.WriteString("</Message></Error>")
	_, _ = io.WriteString(w, body.String())
}

// unsupportedVerb answers a blob request by a method its endpoint does not
// serve (uploads are PUT, downloads GET or HEAD), as Azure Storage does.
func unsupportedVerb(w http.ResponseWriter, r *http.Request) {
	azureError(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "the resource does not support "+r.Method)
}

// blobInternalError logs a blob endpoint's own failure and answers with an
// Azure InternalError that names no path or host detail.
func (s *Server) blobInternalError(w http.ResponseWriter, e *Entry, err error) {
	s.log.Error("cache blob request failed", "id", e.ID, "key", e.Key, "err", err)
	azureError(w, http.StatusInternalServerError, azInternal, "the server encountered an internal error")
}

// Azure Storage error codes the blob endpoints answer with, and two of
// firerunner's own, for a full cache and a store outage, which Azure has no
// codes for. Both are 507, which the Azure SDK does not retry.
const (
	azAuthFailed      = "AuthenticationFailed"            // 403: bad or missing sig
	azNotPermitted    = "AuthorizationPermissionMismatch" // 403: another job's reservation
	azBlobNotFound    = "BlobNotFound"                    // 404
	azImmutable       = "BlobImmutableDueToPolicy"        // 409: a finalized entry
	azBadQuery        = "InvalidQueryParameterValue"      // 400
	azMissingQuery    = "MissingRequiredQueryParameter"   // 400
	azBadBlockList    = "InvalidBlockList"                // 400
	azBadXML          = "InvalidXmlDocument"              // 400
	azTooLarge        = "RequestBodyTooLarge"             // 413
	azInternal        = "InternalError"                   // 500
	azCacheFull       = "CacheFull"                       // 507, firerunner's
	azStoreDown       = "CacheStoreUnavailable"           // 507, firerunner's: never retried
	azBadResourceName = "InvalidResourceName"             // 400
)

// twirpBadRoute answers a request the CacheService does not serve: an unknown
// method, a method other than POST, or a body that is not JSON.
func twirpBadRoute(w http.ResponseWriter, r *http.Request) {
	twirpError(w, "bad_route", fmt.Sprintf("no handler for %s %s", r.Method, r.URL.Path))
}

const (
	// maxTwirpBody bounds a cache RPC's JSON body. The toolkit sends a key,
	// up to ten restore keys and a version, each a few hundred bytes at most.
	maxTwirpBody = 64 << 10
	// maxBlocks is Azure's limit on blocks in one blob, and maxBlockIDLen the
	// longest block id it accepts: 64 bytes before base64, 88 characters after.
	maxBlocks     = 50000
	maxBlockIDLen = 88
	// maxBlockListBody bounds a block list: maxBlocks ids of maxBlockIDLen
	// inside their XML tags.
	maxBlockListBody = 8 << 20
)

// decodeTwirp decodes a cache RPC body into v, answering a 400 or, over
// maxTwirpBody, a 400 invalid_argument itself and reporting false. The bound applies before
// any identity lookup, so a guest cannot make the server buffer an
// arbitrarily large JSON value.
func decodeTwirp(w http.ResponseWriter, r *http.Request, v any) bool {
	// Twirp serves JSON or protobuf by Content-Type; this server speaks the
	// toolkit's JSON only, and answers anything else as a route it lacks.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		twirpBadRoute(w, r)
		return false
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTwirpBody)).Decode(v)
	if err == nil {
		return true
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		twirpError(w, "invalid_argument", fmt.Sprintf("request body over %d bytes", maxTwirpBody))
	} else {
		twirpError(w, "malformed", "request body is not valid JSON: "+err.Error())
	}
	return false
}
