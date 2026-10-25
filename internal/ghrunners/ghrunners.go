// Package ghrunners lists an organization's or repository's self-hosted
// runners through GitHub's REST API, which the scale-set API cannot do. With
// a GitHub App it asks for an installation token narrowed to reading
// runners.
package ghrunners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v4"
)

// Runner is a self-hosted runner as the REST API lists it.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "online" or "offline"
	Busy   bool   `json:"busy"`
}

// Client lists the runners registered under one org or repository. Every
// tier shares one: a listing is reused for CacheFor, so N tiers cost one
// listing, not N.
type Client struct {
	base  string // API root, e.g. https://api.github.com
	scope string // "/orgs/ORG" or "/repos/OWNER/REPO"
	repo  bool
	pat   string
	app   scaleset.GitHubAppAuth
	http  *http.Client
	now   func() time.Time
	// CacheFor is how long a listing is reused (default 1m).
	CacheFor time.Duration

	mu     sync.Mutex
	token  string
	expiry time.Time

	// listMu serializes listings, so tiers asking at once share one.
	listMu   sync.Mutex
	cached   []Runner
	cachedAt time.Time
}

// New returns a client for the runners under rawURL (https://HOST/ORG or
// https://HOST/OWNER/REPO), authenticated by a PAT, else the App.
func New(rawURL, pat string, app scaleset.GitHubAppAuth) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("ghrunners: the GitHub URL must use https")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	c := &Client{base: APIBase(u), pat: pat, app: app, now: time.Now, CacheFor: time.Minute,
		http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}
	switch {
	case len(parts) >= 1 && parts[0] == "enterprises":
		return nil, fmt.Errorf("ghrunners: %q is an enterprise; only org and repository runners can be listed", rawURL)
	case len(parts) == 1 && parts[0] != "":
		c.scope = "/orgs/" + url.PathEscape(parts[0])
	case len(parts) == 2:
		c.scope, c.repo = "/repos/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1]), true
	default:
		return nil, fmt.Errorf("ghrunners: %q names neither an org nor a repository", rawURL)
	}
	return c, nil
}

// APIBase is the REST root for a GitHub URL: api.github.com (or api.<tenant>
// for GHE.com), else /api/v3 on a GitHub Enterprise Server host.
func APIBase(u *url.URL) string {
	host := strings.ToLower(u.Host)
	_, forceGHES := os.LookupEnv("GITHUB_ACTIONS_FORCE_GHES")
	if !forceGHES && (host == "github.com" || host == "www.github.com" || host == "github.localhost" || strings.HasSuffix(host, ".ghe.com")) {
		return "https://api." + strings.TrimPrefix(host, "www.")
	}
	return "https://" + host + "/api/v3"
}

// Runners lists every runner under the scope, following pagination. A
// listing younger than CacheFor is returned as is.
func (c *Client) Runners(ctx context.Context) ([]Runner, error) {
	c.listMu.Lock()
	defer c.listMu.Unlock()
	if c.cached != nil && c.now().Sub(c.cachedAt) < c.CacheFor {
		return c.cached, nil
	}
	tok, err := c.auth(ctx)
	if err != nil {
		return nil, err
	}
	all := []Runner{}
	for page := 1; ; page++ {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		path := fmt.Sprintf("%s/actions/runners?per_page=100&page=%d", c.scope, page)
		if err := c.do(ctx, http.MethodGet, path, tok, nil, &out); err != nil {
			if errors.Is(err, errUnauthorized) {
				c.dropToken()
			}
			return nil, err
		}
		all = append(all, out.Runners...)
		// A short page is the last. (total_count is not relied on: a
		// listing races runners coming and going.)
		if len(out.Runners) < 100 {
			c.cached, c.cachedAt = all, c.now()
			return all, nil
		}
	}
}

func (c *Client) dropToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
}

// auth returns the PAT, or an installation token that can only read runners.
func (c *Client) auth(ctx context.Context) (string, error) {
	if c.pat != "" {
		return c.pat, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && c.expiry.Sub(now) > time.Minute {
		return c.token, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(c.app.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("ghrunners: App private key: %w", err)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    c.app.ClientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(key)
	if err != nil {
		return "", err
	}
	perm := "organization_self_hosted_runners"
	if c.repo {
		perm = "administration"
	}
	body := strings.NewReader(fmt.Sprintf(`{"permissions":{%q:"read"}}`, perm))
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", c.app.InstallationID), signed, body, &out); err != nil {
		return "", err
	}
	if out.Token == "" || !out.ExpiresAt.After(now.Add(time.Minute)) {
		return "", errors.New("ghrunners: invalid installation token response")
	}
	c.token, c.expiry = out.Token, out.ExpiresAt
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, path, token string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return statusError(method, path, resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

var errUnauthorized = errors.New("unauthorized")

// statusError names the request, GitHub's message, and a spent rate limit,
// which shares 403 with a missing permission.
func statusError(method, path string, resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	msg := fmt.Sprintf("ghrunners: %s %s: HTTP %d", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	if body.Message != "" {
		msg += ": " + body.Message
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		msg += " (rate limit spent until " + resp.Header.Get("X-RateLimit-Reset") + ")"
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: %w", msg, errUnauthorized)
	}
	return errors.New(msg)
}
