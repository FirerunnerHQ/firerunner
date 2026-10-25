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

// Client lists the runners registered under one org or repository.
type Client struct {
	base  string // API root, e.g. https://api.github.com
	scope string // "/orgs/ORG" or "/repos/OWNER/REPO"
	repo  bool
	pat   string
	app   scaleset.GitHubAppAuth
	http  *http.Client
	now   func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New returns a client for the runners under rawURL (https://HOST/ORG or
// https://HOST/OWNER/REPO), authenticated by a PAT, else the App.
func New(rawURL, pat string, app scaleset.GitHubAppAuth) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("ghrunners: the GitHub URL must use https")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	c := &Client{base: APIBase(u), pat: pat, app: app, now: time.Now,
		http: &http.Client{Timeout: 15 * time.Second}}
	switch {
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
	if !forceGHES && (host == "github.com" || host == "www.github.com" || strings.HasSuffix(host, ".ghe.com")) {
		return "https://api." + strings.TrimPrefix(host, "www.")
	}
	return "https://" + host + "/api/v3"
}

// Runners lists every runner under the scope, following pagination.
func (c *Client) Runners(ctx context.Context) ([]Runner, error) {
	tok, err := c.auth(ctx)
	if err != nil {
		return nil, err
	}
	var all []Runner
	for page := 1; ; page++ {
		var out struct {
			TotalCount int      `json:"total_count"`
			Runners    []Runner `json:"runners"`
		}
		path := fmt.Sprintf("%s/actions/runners?per_page=100&page=%d", c.scope, page)
		if err := c.do(ctx, http.MethodGet, path, tok, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Runners...)
		if len(out.Runners) < 100 || len(all) >= out.TotalCount {
			return all, nil
		}
	}
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
	if out.Token == "" {
		return "", errors.New("ghrunners: empty installation token")
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
		return fmt.Errorf("ghrunners: %s %s: HTTP %d", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
