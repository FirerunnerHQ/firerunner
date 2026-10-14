package jobidentity

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

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v4"
)

// GitHub resolves only authenticated REST data. Tokens never enter job records.
// It has its own HTTP client, independent of the scaleset client's global lock.
type GitHub struct {
	base      string
	client    *http.Client
	pat       string
	app       scaleset.GitHubAppAuth
	mu        sync.Mutex
	tokens    map[string]installationToken
	resolveMu sync.Mutex
	runs      map[string]runResult
	pending   map[string]chan struct{}
}

type installationToken struct {
	value  string
	expiry time.Time
}
type runResult struct {
	identity Identity
	expiry   time.Time
}

func NewGitHub(rawURL, pat string, app scaleset.GitHubAppAuth) (*GitHub, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("GitHub URL must use https")
	}
	base := u.Scheme + "://" + u.Host + "/api/v3"
	if u.Host == "github.com" || u.Host == "www.github.com" {
		base = "https://api.github.com"
	}
	return &GitHub{base: base, pat: pat, app: app, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (g *GitHub) auth(ctx context.Context, repo string) (string, error) {
	if !ValidRepository(repo) {
		return "", errors.New("invalid token repository")
	}
	if g.pat != "" {
		return g.pat, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.tokens[repo]; t.value != "" && time.Until(t.expiry) > time.Minute {
		return t.value, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(g.app.PrivateKey))
	if err != nil {
		return "", err
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: g.app.ClientID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))})
	signed, err := t.SignedString(key)
	if err != nil {
		return "", err
	}
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body, _ := json.Marshal(map[string]any{
		"repositories": []string{strings.SplitN(repo, "/", 2)[1]},
		"permissions":  map[string]string{"actions": "read", "pull_requests": "read", "metadata": "read", "contents": "read"},
	})
	if err := g.request(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", g.app.InstallationID), signed, bytes.NewReader(body), &result); err != nil {
		return "", err
	}
	if result.Token == "" || !result.ExpiresAt.After(now.Add(time.Minute)) {
		return "", errors.New("invalid installation token response")
	}
	if g.tokens == nil {
		g.tokens = make(map[string]installationToken)
	}
	g.tokens[repo] = installationToken{result.Token, result.ExpiresAt}
	return result.Token, nil
}

func (g *GitHub) request(ctx context.Context, method, path, token string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && !(method == http.MethodPost && resp.StatusCode == http.StatusCreated) {
		return fmt.Errorf("GitHub %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
}
func (g *GitHub) get(ctx context.Context, path string, out any) error {
	parts := strings.SplitN(strings.TrimPrefix(path, "/repos/"), "/", 3)
	if len(parts) < 2 {
		return errors.New("invalid repository API path")
	}
	token, err := g.auth(ctx, strings.ToLower(parts[0]+"/"+parts[1]))
	if err != nil {
		return err
	}
	return g.request(ctx, http.MethodGet, path, token, nil, out)
}

type repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}
type pull struct {
	Number int `json:"number"`
	Head   struct {
		Ref  string     `json:"ref"`
		SHA  string     `json:"sha"`
		Repo repository `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string     `json:"ref"`
		Repo repository `json:"repo"`
	} `json:"base"`
}
type workflowRun struct {
	ID             int64      `json:"id"`
	Event          string     `json:"event"`
	HeadBranch     string     `json:"head_branch"`
	HeadSHA        string     `json:"head_sha"`
	Repository     repository `json:"repository"`
	HeadRepository repository `json:"head_repository"`
	PullRequests   []pull     `json:"pull_requests"`
}

func refs(own, base, def string) []string {
	var out []string
	for _, ref := range []string{own, base, def} {
		if ref == "" {
			continue
		}
		seen := false
		for _, r := range out {
			if r == ref {
				seen = true
			}
		}
		if !seen {
			out = append(out, ref)
		}
	}
	return out
}

// Resolve coalesces concurrent matrix jobs and memoizes successful run policies.
// Job IDs and VM generations are never reused from the memoized policy.
func (g *GitHub) Resolve(ctx context.Context, j Job) (Identity, error) {
	if !ValidRepository(j.Repository) || j.RunID <= 0 || j.JobID == "" {
		return Identity{}, errors.New("invalid job metadata")
	}
	key := fmt.Sprintf("%s/%d/%s", strings.ToLower(j.Repository), j.RunID, j.Event)
	for {
		if err := ctx.Err(); err != nil {
			return Identity{}, err
		}
		g.resolveMu.Lock()
		if cached, ok := g.runs[key]; ok && time.Now().Before(cached.expiry) {
			g.resolveMu.Unlock()
			i := cached.identity
			i.JobID = j.JobID
			i.ReadRefs = append([]string(nil), i.ReadRefs...)
			return i, nil
		}
		if done := g.pending[key]; done != nil {
			g.resolveMu.Unlock()
			select {
			case <-ctx.Done():
				return Identity{}, ctx.Err()
			case <-done:
				continue
			}
		}
		if g.pending == nil {
			g.pending = make(map[string]chan struct{})
		}
		done := make(chan struct{})
		g.pending[key] = done
		g.resolveMu.Unlock()
		i, err := g.resolve(ctx, j)
		g.resolveMu.Lock()
		if err == nil {
			if g.runs == nil {
				g.runs = make(map[string]runResult)
			}
			// Bound memory even when the daemon handles many short jobs.
			if len(g.runs) >= 1024 {
				for k := range g.runs {
					delete(g.runs, k)
					break
				}
			}
			saved := i
			saved.JobID = ""
			saved.ReadRefs = append([]string(nil), i.ReadRefs...)
			g.runs[key] = runResult{saved, time.Now().Add(2 * time.Minute)}
		}
		delete(g.pending, key)
		close(done)
		g.resolveMu.Unlock()
		return i, err
	}
}

// Workflow-file refs may identify a reusable workflow, even in the same repo.
// Resolve the triggering run instead; never elevate it using JobWorkflowRef.
func (g *GitHub) resolve(ctx context.Context, j Job) (Identity, error) {
	var i Identity
	if !ValidRepository(j.Repository) || j.RunID <= 0 || j.JobID == "" {
		return i, errors.New("invalid job metadata")
	}
	root := "/repos/" + j.Repository
	var run workflowRun
	if err := g.get(ctx, fmt.Sprintf("%s/actions/runs/%d", root, j.RunID), &run); err != nil {
		return i, err
	}
	if run.ID != j.RunID || run.Event != j.Event || !strings.EqualFold(run.Repository.FullName, j.Repository) || run.HeadSHA == "" {
		return i, errors.New("workflow run does not match job")
	}
	var repo repository
	if err := g.get(ctx, root, &repo); err != nil {
		return i, err
	}
	def := "refs/heads/" + repo.DefaultBranch
	if !strings.EqualFold(repo.FullName, j.Repository) || !ValidRef(def) {
		return i, errors.New("repository default branch unavailable")
	}
	i = Identity{Repository: strings.ToLower(repo.FullName), Event: run.Event, RunID: j.RunID, JobID: j.JobID, ReadRefs: []string{def}, Fork: !strings.EqualFold(run.HeadRepository.FullName, repo.FullName)}
	switch run.Event {
	case "pull_request":
		candidates := run.PullRequests
		if len(candidates) == 0 {
			// This also handles fork runs whose pull_requests field is absent. A
			// unique matching PR is required; branch-name guessing is not sufficient.
			if err := g.get(ctx, root+"/commits/"+url.PathEscape(run.HeadSHA)+"/pulls?per_page=100", &candidates); err != nil {
				return Identity{}, err
			}
		}
		if len(candidates) == 0 || len(candidates) > 8 {
			return Identity{}, errors.New("PR association missing or ambiguous")
		}
		var matched []pull
		seen := map[int]bool{}
		for _, candidate := range candidates {
			if candidate.Number <= 0 || seen[candidate.Number] {
				continue
			}
			seen[candidate.Number] = true
			var p pull
			if err := g.get(ctx, fmt.Sprintf("%s/pulls/%d", root, candidate.Number), &p); err != nil {
				return Identity{}, err
			}
			if p.Number == candidate.Number && p.Head.Ref == run.HeadBranch && strings.EqualFold(p.Head.Repo.FullName, run.HeadRepository.FullName) && strings.EqualFold(p.Base.Repo.FullName, repo.FullName) {
				matched = append(matched, p)
			}
		}
		if len(matched) != 1 {
			return Identity{}, errors.New("PR association missing or ambiguous")
		}
		p := matched[0]
		base := "refs/heads/" + p.Base.Ref
		if !ValidRef(base) {
			return Identity{}, errors.New("invalid PR base")
		}
		i.WriteRef = fmt.Sprintf("refs/pull/%d/merge", p.Number)
		i.ReadRefs = refs(i.WriteRef, base, def)
	case "push", "workflow_dispatch", "schedule", "repository_dispatch":
		if i.Fork {
			return Identity{}, errors.New("branch run has foreign head repository")
		}
		own := "refs/heads/" + run.HeadBranch
		if !ValidRef(own) {
			return Identity{}, errors.New("invalid branch ref")
		}
		// matching-refs returns [] for an absent tag (unlike ref's ambiguous 404).
		var tags []struct {
			Ref string `json:"ref"`
		}
		if err := g.get(ctx, root+"/git/matching-refs/tags/"+escapeRefPath(run.HeadBranch), &tags); err != nil {
			return Identity{}, err
		}
		for _, tag := range tags {
			if tag.Ref == "refs/tags/"+run.HeadBranch {
				return Identity{}, errors.New("tag run or ambiguous branch/tag name; cache writes disabled")
			}
		}
		var branch struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := g.get(ctx, root+"/git/ref/heads/"+escapeRefPath(run.HeadBranch), &branch); err != nil {
			return Identity{}, err
		}
		if branch.Ref != own {
			return Identity{}, errors.New("branch ref unavailable; cache writes disabled")
		}
		i.WriteRef = own
		i.ReadRefs = refs(own, "", def)
	default:
		// pull_request_target, workflow_run, issue_comment, unknown events: read
		// default only. Never assume the workflow-file ref is the triggering ref.
	}
	return i, nil
}

// Preserve slashes in branch names; escape each path segment separately.
func escapeRefPath(ref string) string {
	parts := strings.Split(ref, "/")
	for n := range parts {
		parts[n] = url.PathEscape(parts[n])
	}
	return strings.Join(parts, "/")
}
