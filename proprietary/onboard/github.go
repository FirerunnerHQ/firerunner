package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/firerunnerhq/firerunner/internal/ghauth"
)

// GitHub calls GitHub's REST API as the hosted App: as the App itself to
// read an installation, and with an installation's token to read its
// repositories and keep its organization's runner group.
type GitHub struct {
	// Base is the REST root, e.g. https://api.github.com.
	Base string
	JWT  *ghauth.JWT
	HTTP *http.Client
	now  func() time.Time

	mu     sync.Mutex
	tokens map[int64]token
}

type token struct {
	value  string
	expiry time.Time
}

// NewGitHub returns a client for the App jwt signs for, on GitHub's REST
// root base.
func NewGitHub(base string, jwt *ghauth.JWT) *GitHub {
	return &GitHub{Base: strings.TrimSuffix(base, "/"), JWT: jwt, now: time.Now, tokens: map[int64]token{},
		HTTP: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}
}

// StatusError is a response GitHub refused, by its status.
type StatusError struct {
	Method, Path string
	Code         int
	Message      string
	RateLimited  bool
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("GitHub %s %s: HTTP %d", e.Method, e.Path, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RateLimited {
		msg += " (rate limit spent)"
	}
	return msg
}

// statusOf returns err's HTTP status, or 0.
func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// Installation reads an installation as the App: Lookup's source.
func (g *GitHub) Installation(ctx context.Context, id int64) (Observed, error) {
	signed, err := g.JWT.Token(ctx)
	if err != nil {
		return Observed{}, fmt.Errorf("App JWT: %w", err)
	}
	var out struct {
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		SuspendedAt *time.Time `json:"suspended_at"`
	}
	err = g.do(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", id), signed, nil, &out)
	if statusOf(err) == http.StatusNotFound {
		return Observed{}, nil
	}
	if err != nil {
		if statusOf(err) == http.StatusUnauthorized {
			g.JWT.Refused(signed)
		}
		return Observed{}, err
	}
	return Observed{Found: true, Suspended: out.SuspendedAt != nil, AccountID: out.Account.ID, Login: out.Account.Login, Type: out.Account.Type}, nil
}

// token returns a token of the installation's, narrowed to what keeping
// its runner group needs.
func (g *GitHub) token(ctx context.Context, installation int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if t, ok := g.tokens[installation]; ok && t.expiry.Sub(now) > 5*time.Minute {
		return t.value, nil
	}
	signed, err := g.JWT.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("App JWT: %w", err)
	}
	body := `{"permissions":{"organization_self_hosted_runners":"write","metadata":"read"}}`
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installation), signed, strings.NewReader(body), &out); err != nil {
		if statusOf(err) == http.StatusUnauthorized {
			g.JWT.Refused(signed)
		}
		return "", err
	}
	if out.Token == "" || !out.ExpiresAt.After(now.Add(5*time.Minute)) {
		return "", errors.New("GitHub returned no usable installation token")
	}
	g.tokens[installation] = token{out.Token, out.ExpiresAt}
	return out.Token, nil
}

// Repositories lists the repositories the installation may access, and
// whether it may access all of the account's.
func (g *GitHub) Repositories(ctx context.Context, installation int64) ([]Repository, bool, error) {
	tok, err := g.token(ctx, installation)
	if err != nil {
		return nil, false, err
	}
	var all []Repository
	selection := ""
	for page := 1; ; page++ {
		var out struct {
			TotalCount          int          `json:"total_count"`
			RepositorySelection string       `json:"repository_selection"`
			Repositories        []Repository `json:"repositories"`
		}
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), tok, nil, &out); err != nil {
			return nil, false, err
		}
		selection = out.RepositorySelection
		all = append(all, out.Repositories...)
		if len(out.Repositories) < 100 || len(all) >= out.TotalCount {
			return all, selection == "all", nil
		}
	}
}

// RunnerGroups lists the organization's runner groups.
func (g *GitHub) RunnerGroups(ctx context.Context, installation int64, org string) ([]RunnerGroup, error) {
	tok, err := g.token(ctx, installation)
	if err != nil {
		return nil, err
	}
	var all []RunnerGroup
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int           `json:"total_count"`
			RunnerGroups []RunnerGroup `json:"runner_groups"`
		}
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/actions/runner-groups?per_page=100&page=%d", url.PathEscape(org), page), tok, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.RunnerGroups...)
		if len(out.RunnerGroups) < 100 || len(all) >= out.TotalCount {
			return all, nil
		}
	}
}

// CreateRunnerGroup creates the organization's runner group as spec says.
func (g *GitHub) CreateRunnerGroup(ctx context.Context, installation int64, org string, spec GroupSpec) (RunnerGroup, error) {
	tok, err := g.token(ctx, installation)
	if err != nil {
		return RunnerGroup{}, err
	}
	body := map[string]any{"name": spec.Name, "visibility": spec.Visibility, "allows_public_repositories": false}
	if spec.Visibility == "selected" {
		body["selected_repository_ids"] = nonNil(spec.Repositories)
	}
	var out RunnerGroup
	err = g.do(ctx, http.MethodPost, fmt.Sprintf("/orgs/%s/actions/runner-groups", url.PathEscape(org)), tok, jsonBody(body), &out)
	return out, err
}

// UpdateRunnerGroup sets the group's visibility, and keeps public
// repositories out. The repositories of a selected group are
// SetGroupRepositories'.
func (g *GitHub) UpdateRunnerGroup(ctx context.Context, installation int64, org string, id int64, spec GroupSpec) error {
	tok, err := g.token(ctx, installation)
	if err != nil {
		return err
	}
	body := map[string]any{"name": spec.Name, "visibility": spec.Visibility, "allows_public_repositories": false}
	return g.do(ctx, http.MethodPatch, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", url.PathEscape(org), id), tok, jsonBody(body), nil)
}

// SetGroupRepositories makes repositories the selected group's, all of them.
func (g *GitHub) SetGroupRepositories(ctx context.Context, installation int64, org string, id int64, repositories []int64) error {
	tok, err := g.token(ctx, installation)
	if err != nil {
		return err
	}
	body := map[string]any{"selected_repository_ids": nonNil(repositories)}
	return g.do(ctx, http.MethodPut, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/repositories", url.PathEscape(org), id), tok, jsonBody(body), nil)
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func (g *GitHub) do(ctx context.Context, method, path, auth string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, g.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&msg)
		return &StatusError{Method: method, Path: strings.SplitN(path, "?", 2)[0], Code: resp.StatusCode,
			Message: msg.Message, RateLimited: resp.Header.Get("X-RateLimit-Remaining") == "0"}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
