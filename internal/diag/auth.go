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
// with a short-lived JWT). It is best-effort: a transport error WARNs (the host
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
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&inst); err != nil {
				checks = append(checks, warn("app-permissions", "could not read the installation's permissions: %v", err))
			} else {
				checks = append(checks, appPermissions(inst.Permissions, repoScoped(cfg.URL)))
			}
		}
		return checks
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return []Check{fail("auth-verify", "credentials rejected (HTTP %d)", resp.StatusCode)}
	default:
		return []Check{warn("auth-verify", "unexpected HTTP %d verifying credentials", resp.StatusCode)}
	}
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

// appPermissions compares an installation's granted permissions with what
// firerunner uses: metadata (read, every App has it) and self-hosted runners
// (write), granted as organization_self_hosted_runners for an org, or as
// administration for a single repository. Anything less FAILs (the scale-set
// calls will be refused); anything more WARNs, naming what it allows.
func appPermissions(granted map[string]string, repo bool) Check {
	need := "organization_self_hosted_runners"
	if repo {
		need = "administration"
	}
	if granted[need] != "write" {
		return fail("app-permissions", "the installation has %s: %q; firerunner needs write to manage runners", need, granted[need])
	}
	var extra []string
	for name, level := range granted {
		if name == need || name == "metadata" {
			continue
		}
		risk := permissionRisk[name]
		if risk == "" {
			risk = "access GitHub beyond managing runners"
		}
		extra = append(extra, fmt.Sprintf("%s: %s (lets the key %s)", name, level, risk))
	}
	if len(extra) == 0 {
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
