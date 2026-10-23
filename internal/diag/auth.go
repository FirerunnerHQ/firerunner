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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/solcreek/firerunner/internal/config"
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
func authVerify(cfg *config.Config, apiBase string) Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var req *http.Request
	if cfg.Token != "" {
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/rate_limit", nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	} else {
		key, err := cfg.ResolvePrivateKey()
		if err != nil {
			return fail("auth-verify", "GitHub App private key: %v", err)
		}
		jwt, err := appJWT(cfg.AppClientID, key, time.Now())
		if err != nil {
			return fail("auth-verify", "cannot sign GitHub App JWT: %v", err)
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/app/installations/%d", apiBase, cfg.AppInstallID), nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return warn("auth-verify", "could not verify credentials (offline?): %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return pass("auth-verify", "credentials accepted by %s", apiBase)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fail("auth-verify", "credentials rejected (HTTP %d)", resp.StatusCode)
	default:
		return warn("auth-verify", "unexpected HTTP %d verifying credentials", resp.StatusCode)
	}
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
