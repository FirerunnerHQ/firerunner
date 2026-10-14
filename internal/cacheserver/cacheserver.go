// Package cacheserver is a stdlib-only, disk-backed implementation of the
// GitHub Actions cache service (v2) protocol. It lets firerunner microVMs keep
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
// cannot write and may read only the explicitly configured default branch.
// Every blob request also checks its scope; reservations belong to one VM/job.
// Legacy entries without a ref are never served. See the README for deployment
// requirements, including local routing without source NAT and directory access.
package cacheserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/solcreek/firerunner/internal/jobidentity"
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
	Key        string     `json:"key"`
	Version    string     `json:"version"`
	Size       int64      `json:"size"`
	Complete   bool       `json:"complete"`
	CreatedAt  int64      `json:"created_at"`
	UsedAt     int64      `json:"used_at"`
}

// Server is a disk-backed GitHub Actions cache (v2) server. It is safe for
// concurrent use by many microVMs.
type Server struct {
	dir string
	log *slog.Logger
	mux *http.ServeMux

	mu         sync.Mutex
	entries    map[uint64]*Entry
	nextID     uint64
	maxSize    int64            // 0 = unlimited; total completed-blob bytes to keep
	maxEntry   int64            // 0 = unlimited; hard cap on a single entry's blob
	staged     map[uint64]int64 // bytes written so far for a not-yet-finalized entry
	repo       string
	defaultRef string
	lookup     func(string) (jobidentity.Identity, error)

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
	artifactRPCs    atomic.Uint64
	artifactErrors  atomic.Uint64
}

// Stats is a point-in-time snapshot of the cache store, served by /stats and
// /metrics and consumed by `firerunner status`.
type Stats struct {
	Entries        int    `json:"entries"`         // completed entries currently stored
	Bytes          int64  `json:"bytes"`           // total size of completed blobs
	MaxBytes       int64  `json:"max_bytes"`       // configured cap (0 = unlimited)
	Hits           uint64 `json:"hits"`            // download-URL lookups that matched
	Misses         uint64 `json:"misses"`          // download-URL lookups that did not
	Saves          uint64 `json:"saves"`           // entries finalized
	Evictions      uint64 `json:"evictions"`       // entries removed by the size cap
	ArtifactRPCs   uint64 `json:"artifact_rpcs"`   // ArtifactService RPCs forwarded upstream
	ArtifactErrors uint64 `json:"artifact_errors"` // artifact RPCs refused, failed, or answered 5xx
}

// snapshot collects current stats. The caller must NOT hold s.mu.
func (s *Server) snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		MaxBytes: s.maxSize, Hits: s.hits, Misses: s.misses, Saves: s.saves, Evictions: s.evictions,
		ArtifactRPCs: s.artifactRPCs.Load(), ArtifactErrors: s.artifactErrors.Load(),
	}
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
	}
	if err := s.load(); err != nil {
		return nil, err
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST "+twirpBase+"CreateCacheEntry", s.handleCreate)
	s.mux.HandleFunc("POST "+twirpBase+"FinalizeCacheEntryUpload", s.handleFinalize)
	s.mux.HandleFunc("POST "+twirpBase+"GetCacheEntryDownloadURL", s.handleGetDownloadURL)
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
	s.mux.ServeHTTP(w, r)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	if req.Key == "" || req.Version == "" {
		twirpError(w, http.StatusBadRequest, "invalid_argument", "key and version are required")
		return
	}
	identity := s.identity(r)
	repo := identity.Repository
	key := strings.ToLower(req.Key)
	if identity.WriteRef == "" {
		writeJSON(w, http.StatusOK, createResp{OK: false, Message: "job has no cache write scope"})
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
		_ = os.Remove(s.blobPath(e.ID))
		_ = os.RemoveAll(s.tmpDir(e.ID))
		delete(s.entries, e.ID)
		delete(s.staged, e.ID)
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
		twirpError(w, http.StatusInternalServerError, "internal", err.Error())
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
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
	fi, err := os.Stat(s.blobPath(e.ID))
	if err != nil {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: "cache blob was not uploaded"})
		return
	}
	if want, err := strconv.ParseInt(req.SizeBytes, 10, 64); err == nil && want >= 0 && want != fi.Size() {
		writeJSON(w, http.StatusOK, finalizeResp{OK: false, Message: fmt.Sprintf("size mismatch: got %d, want %d", fi.Size(), want)})
		return
	}
	e.Size = fi.Size()
	e.Complete = true
	e.UsedAt = time.Now().Unix()
	delete(s.staged, e.ID) // it now counts as a completed blob, not in-flight
	s.saves++
	s.evictLRU(e.ID)
	if err := s.save(); err != nil {
		twirpError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.log.Info("cache entry finalized", "id", e.ID, "key", e.Key, "size", e.Size)
	writeJSON(w, http.StatusOK, finalizeResp{OK: true, EntryID: strconv.FormatUint(e.ID, 10)})
}

// handleGetDownloadURL resolves the best matching entry (exact key first, then
// restore-key prefixes in order, newest wins) and returns a download URL, or
// ok:false on a miss.
func (s *Server) handleGetDownloadURL(w http.ResponseWriter, r *http.Request) {
	var req getReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
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
		MatchedKey:        e.Key,
	})
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
		http.Error(w, "forbidden", http.StatusForbidden)
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
		http.Error(w, "cache entry already finalized", http.StatusConflict)
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
		http.Error(w, "unsupported comp", http.StatusBadRequest)
	}
}

func (s *Server) uploadSingle(w http.ResponseWriter, r *http.Request, e *Entry) {
	body := r.Body
	if cap := s.entryCap(); cap > 0 {
		body = http.MaxBytesReader(w, r.Body, cap)
	}
	n, err := writeFile(s.blobPath(e.ID), body)
	if err != nil {
		_ = os.Remove(s.blobPath(e.ID)) // don't leave an oversized/partial blob squatting disk
		s.clearStaged(e.ID)
		code, msg := uploadStatus(err)
		http.Error(w, msg, code)
		return
	}
	s.setStaged(e.ID, n)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) uploadBlock(w http.ResponseWriter, r *http.Request, e *Entry) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		http.Error(w, "missing blockid", http.StatusBadRequest)
		return
	}
	dir := s.tmpDir(e.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	n, err := writeFile(blockPath, body)
	if err != nil {
		_ = os.Remove(blockPath)
		code, msg := uploadStatus(err)
		http.Error(w, msg, code)
		return
	}
	s.addStaged(e.ID, n)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) commitBlockList(w http.ResponseWriter, r *http.Request, e *Entry) {
	var list struct {
		Latest      []string `xml:"Latest"`
		Uncommitted []string `xml:"Uncommitted"`
		Committed   []string `xml:"Committed"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := xml.Unmarshal(body, &list); err != nil {
		http.Error(w, "bad block list: "+err.Error(), http.StatusBadRequest)
		return
	}
	ids := list.Latest
	ids = append(ids, list.Uncommitted...)
	ids = append(ids, list.Committed...)
	if len(ids) == 0 {
		http.Error(w, "empty block list", http.StatusBadRequest)
		return
	}

	// Assemble into a temp file and rename over the blob only once it is fully
	// built and within the per-entry cap. Streaming straight into the live blob
	// would truncate a previously finalized entry the instant a commit arrives,
	// even if a staged block is missing or the total exceeds the cap.
	dir := s.tmpDir(e.ID)
	tmp := s.blobPath(e.ID) + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cap := s.entryCap()
	var total int64
	for _, id := range ids {
		blockPath := filepath.Join(dir, hex.EncodeToString([]byte(id)))
		f, err := os.Open(blockPath)
		if err != nil {
			out.Close()
			_ = os.Remove(tmp)
			http.Error(w, "missing staged block: "+err.Error(), http.StatusBadRequest)
			return
		}
		n, err := io.Copy(out, f)
		f.Close()
		total += n
		if err != nil {
			out.Close()
			_ = os.Remove(tmp)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if cap > 0 && total > cap {
			out.Close()
			_ = os.Remove(tmp)
			_ = os.RemoveAll(dir)
			s.clearStaged(e.ID)
			http.Error(w, "cache entry exceeds max entry size", http.StatusRequestEntityTooLarge)
			return
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, s.blobPath(e.ID)); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.RemoveAll(dir)
	s.setStaged(e.ID, total)
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
	f, err := os.Open(s.blobPath(e.ID))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A restore issues one getProperties (HEAD) then many ranged GETs; log the
	// HEAD and any un-ranged GET at INFO so a restore shows up once, and leave
	// the per-range chatter out of the default log.
	if r.Method == http.MethodHead || r.Header.Get("Range") == "" {
		s.log.Info("cache entry served", "id", e.ID, "key", e.Key, "size", e.Size, "method", r.Method)
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
	fmt.Fprintf(w, "# HELP firerunner_cache_evictions_total Entries removed by the size cap.\n")
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
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, false
	}
	s.mu.Lock()
	e := s.entries[id]
	s.mu.Unlock()
	if e == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("sig")), []byte(e.Sig)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	identity := s.identity(r)
	s.mu.Lock()
	complete := e.Complete
	s.mu.Unlock()
	if r.Method == http.MethodPut {
		if !owns(identity, e) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return nil, false
		}
	} else if !complete || e.RepoID != identity.Repository || !identity.CanRead(e.Ref) {
		http.Error(w, "forbidden", http.StatusForbidden)
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

func (s *Server) completeExact(repo, ref, key, version string) *Entry {
	var best *Entry
	for _, e := range s.entries {
		if e.Complete && ref != "" && e.Ref == ref && e.RepoID == repo && e.Version == version && e.Key == key {
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
		if e.Complete && ref != "" && e.Ref == ref && e.RepoID == repo && e.Version == version && strings.HasPrefix(e.Key, prefix) {
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
	var total int64
	for _, e := range s.entries {
		if e.Complete {
			total += e.Size
		}
	}
	for _, n := range s.staged {
		total += n
	}
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
		_ = os.Remove(s.blobPath(victim.ID))
		_ = os.RemoveAll(s.tmpDir(victim.ID))
		delete(s.entries, victim.ID)
		delete(s.staged, victim.ID)
		s.evictions++
		s.log.Info("evicted cache entry (size cap)", "id", victim.ID, "key", victim.Key, "size", victim.Size)
	}
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
			_ = os.Remove(s.blobPath(id))
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

// uploadStatus maps an upload write error to an HTTP status: an over-cap body
// (http.MaxBytesError) is a 413, anything else a 500.
func uploadStatus(err error) (int, string) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge, "cache entry exceeds max entry size"
	}
	return http.StatusInternalServerError, err.Error()
}

// staged-byte accounting bounds the disk a not-yet-finalized entry occupies.
func (s *Server) setStaged(id uint64, n int64) {
	s.mu.Lock()
	s.staged[id] = n
	s.mu.Unlock()
}
func (s *Server) addStaged(id uint64, n int64) {
	s.mu.Lock()
	s.staged[id] += n
	s.mu.Unlock()
}
func (s *Server) stagedBytes(id uint64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staged[id]
}
func (s *Server) clearStaged(id uint64) {
	s.mu.Lock()
	delete(s.staged, id)
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

// twirpError writes a Twirp-shaped error envelope ({code,msg}) with an HTTP
// status. The toolkit only surfaces these for genuinely bad requests; cache
// misses and reservation conflicts are ok:false 200s handled inline.
func twirpError(w http.ResponseWriter, code int, twirpCode, msg string) {
	writeJSON(w, code, map[string]string{"code": twirpCode, "msg": msg})
}
