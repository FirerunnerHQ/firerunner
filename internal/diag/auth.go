package diag

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
)

// githubAPIBase derives the REST API base URL from the configured org/repo URL,
// so doctor probes the host firerunner actually talks to. github.com maps to
// api.github.com; a GitHub Enterprise Server host serves its API under
// /api/v3 on the same host. An unparseable/empty URL falls back to github.com.
func githubAPIBase(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "https://api.github.com"
	}
	switch strings.ToLower(u.Host) {
	case "github.com", "www.github.com":
		return "https://api.github.com"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host + "/api/v3"
}

// authVerify makes a single read-only authenticated request to confirm the
// configured credentials are actually accepted by GitHub — the presence checks
// in authCheck only prove the values are set and parseable. It never mutates
// anything (a PAT hits /rate_limit; a GitHub App hits GET /app/installations/ID
// with a short-lived JWT, and, for a cache on selected repositories, lists them
// with a read-only token). It is best-effort: a transport error WARNs (the host
// may simply be offline), an explicit 401/403 FAILs, anything else WARNs.
//
// For a GitHub App it also returns the app-permissions check: what the
// installation was granted against the least-privilege set (see
// appPermissions).
func authVerify(cfg *config.Config, apiBase string) []Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var req *http.Request
	if cfg.Token != "" {
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/rate_limit", nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	} else {
		key, err := cfg.ResolvePrivateKey()
		if err != nil {
			return []Check{fail("auth-verify", "GitHub App private key: %v", err)}
		}
		jwt, err := appJWT(cfg.AppClientID, key, time.Now())
		if err != nil {
			return []Check{fail("auth-verify", "cannot sign GitHub App JWT: %v", err)}
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/app/installations/%d", apiBase, cfg.AppInstallID), nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []Check{warn("auth-verify", "could not verify credentials (offline?): %v", err)}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		checks := []Check{pass("auth-verify", "credentials accepted by %s", apiBase)}
		if cfg.Token == "" {
			var inst struct {
				Permissions map[string]string `json:"permissions"`
				Selection   string            `json:"repository_selection"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&inst); err != nil {
				checks = append(checks, warn("app-permissions", "could not read the installation's permissions: %v", err))
			} else {
				cache := cfg.UsesEC2Cache() || cfg.Firecracker.CachePort != 0 || cfg.Firecracker.CacheURL != ""
				checks = append(checks, appPermissions(inst.Permissions, repoScoped(cfg.URL), cache))
				if cache && inst.Selection == "selected" {
					checks = append(checks, appRepositories(ctx, cfg, apiBase, req.Header.Get("Authorization")))
				}
			}
		}
		return checks
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return []Check{fail("auth-verify", "credentials rejected (HTTP %d)", resp.StatusCode)}
	default:
		return []Check{warn("auth-verify", "unexpected HTTP %d verifying credentials", resp.StatusCode)}
	}
}

// maxListedRepositories bounds what a check names, and what it reads.
const maxListedRepositories = 100

// appRepositories checks a cache against an App installed on selected
// repositories. Resolving a job's identity needs a token for its repository,
// which GitHub refuses for one outside the installation. Its jobs then run with
// no cache, and their first step waits out the lookup, with nothing to see but
// a log line on the control VM. A runner for one repository can be checked; for
// an organization, only the operator knows which repositories it serves.
//
// auth is the App's JWT. It asks for a token with metadata read alone, which is
// all that listing repositories takes.
func appRepositories(ctx context.Context, cfg *config.Config, apiBase, auth string) Check {
	body := strings.NewReader(`{"permissions":{"metadata":"read"}}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, cfg.AppInstallID), body)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	var token struct {
		Token string `json:"token"`
	}
	if err := appJSON(req, &token); err != nil {
		return warn("app-repositories", "could not read the installation's repositories: %v", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/installation/repositories?per_page=%d", apiBase, maxListedRepositories), nil)
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	var list struct {
		Total        int `json:"total_count"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := appJSON(req, &list); err != nil {
		return warn("app-repositories", "could not read the installation's repositories: %v", err)
	}
	names := make([]string, 0, len(list.Repositories))
	for _, r := range list.Repositories {
		names = append(names, r.FullName)
	}
	listed := strings.Join(names, ", ")
	if listed == "" {
		listed = "none"
	}
	if list.Total > len(names) {
		listed = fmt.Sprintf("%s and %d more", listed, list.Total-len(names))
	}
	if repoScoped(cfg.URL) {
		want := repoName(cfg.URL)
		for _, name := range names {
			if strings.EqualFold(name, want) {
				return pass("app-repositories", "the App is installed on %s", want)
			}
		}
		if list.Total > len(names) {
			return warn("app-repositories", "could not find %s among the first %d of the App's %d repositories", want, len(names), list.Total)
		}
		return fail("app-repositories", "the App is not installed on %s, so its jobs get no cache; it is on: %s", want, listed)
	}
	return warn("app-repositories", "the App is installed on selected repositories only (%s); jobs in any other get no cache, and their first step waits for a lookup that fails. Add the repositories you serve, or choose all", listed)
}

// appJSON sends req and decodes a 2xx answer into out.
func appJSON(req *http.Request, out any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(out)
}

// repoName returns the owner/name of a repository URL.
func repoName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
}

// repoScoped reports whether FR_URL names a single repository
// (https://github.com/ORG/REPO) rather than an organization.
func repoScoped(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return len(strings.Split(strings.Trim(u.Path, "/"), "/")) >= 2
}

// permissionRisk says what an App permission beyond firerunner's needs would
// let a holder of its key do; others get a generic line.
var permissionRisk = map[string]string{
	"packages":                    "pull private packages and container images, which often hold source or dependencies",
	"contents":                    "read (or write) repository source",
	"actions":                     "read workflow runs and logs, and their artifacts",
	"secrets":                     "manage Actions secrets",
	"organization_secrets":        "manage organization Actions secrets",
	"workflows":                   "change workflow files",
	"pull_requests":               "read (or write) pull requests",
	"issues":                      "read (or write) issues",
	"members":                     "read (or change) organization membership",
	"organization_administration": "change organization settings",
	"administration":              "change repository settings",
	"environments":                "read (or change) deployment environments and their secrets",
}

// cacheReads are the read permissions a cache's job identity resolution uses
// (internal/jobidentity): the run, its jobs and the refs it may touch.
var cacheReads = []string{"actions", "contents", "pull_requests"}

// appPermissions compares an installation's granted permissions with what
// firerunner uses: metadata (read, every App has it) and self-hosted runners
// (write), granted as organization_self_hosted_runners for an org, or as
// administration for a single repository; with a cache, also cacheReads.
// Anything less FAILs (the scale-set calls will be refused, or every job's
// cache denied); anything more WARNs, naming what it allows.
func appPermissions(granted map[string]string, repo, cache bool) Check {
	need := "organization_self_hosted_runners"
	if repo {
		need = "administration"
	}
	if granted[need] != "write" {
		return fail("app-permissions", "the installation has %s: %q; firerunner needs write to manage runners", need, granted[need])
	}
	used := map[string]bool{need: true, "metadata": true}
	if cache {
		var missing []string
		for _, p := range cacheReads {
			if granted[p] == "" {
				missing = append(missing, p)
			}
			if granted[p] == "read" {
				used[p] = true
			}
		}
		if len(missing) > 0 {
			return fail("app-permissions", "the cache resolves each job's identity with read on actions, contents and pull_requests; missing %s, so every job's cache is denied. Grant them in the App's settings (Repository permissions), then accept them on the installation", strings.Join(missing, ", "))
		}
	}
	var extra []string
	for name, level := range granted {
		if used[name] {
			continue
		}
		risk := permissionRisk[name]
		if risk == "" {
			risk = "access GitHub beyond managing runners"
		}
		extra = append(extra, fmt.Sprintf("%s: %s (lets the key %s)", name, level, risk))
	}
	if len(extra) == 0 {
		if cache {
			return pass("app-permissions", "least privilege: metadata (read), %s (write), and actions, contents, pull_requests (read) for the cache", need)
		}
		return pass("app-permissions", "least privilege: metadata (read), %s (write)", need)
	}
	sort.Strings(extra)
	return warn("app-permissions", "the App grants more than firerunner uses; remove from the App's settings: %s", strings.Join(extra, "; "))
}

// appJWT builds the short-lived RS256 JWT GitHub requires to authenticate as a
// GitHub App. The issuer is the App's client ID (GitHub also accepts the App
// ID). Kept stdlib-only: PKCS#1/PKCS#8 PEM parsing + rsa.SignPKCS1v15.
func appJWT(issuer, pemKey string, now time.Time) (string, error) {
	key, err := config.ParseAppPrivateKey([]byte(pemKey))
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(), // GitHub caps App JWTs at 10 min
		"iss": issuer,
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + enc(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + enc(sig), nil
}
