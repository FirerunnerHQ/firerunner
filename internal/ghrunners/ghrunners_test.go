package ghrunners

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// tokenExpiry is when the fake server's installation tokens expire.
var tokenExpiry = func() time.Time { return time.Now().Add(time.Hour) }

func testKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// server serves n runners under scope, 100 per page, and installation
// tokens; it records the token permissions asked for.
func server(t *testing.T, scope string, n int, perms *string, tokens *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			b, _ := io.ReadAll(r.Body)
			*perms = string(b)
			*tokens++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"inst","expires_at":%q}`, tokenExpiry().Format(time.RFC3339))
		case r.URL.Path == scope+"/actions/runners":
			if r.Header.Get("Authorization") != "Bearer inst" && r.Header.Get("Authorization") != "Bearer ghp_x" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			var rs []Runner
			for i := (page - 1) * 100; i < min(page*100, n); i++ {
				rs = append(rs, Runner{ID: int64(i), Name: fmt.Sprintf("tier-%04d", i), Status: "offline"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runners": rs}) // no total_count: pages end short
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func client(t *testing.T, srv *httptest.Server, rawURL, pat string) *Client {
	t.Helper()
	c, err := New(rawURL, pat, scaleset.GitHubAppAuth{ClientID: "Iv1.x", InstallationID: 42, PrivateKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	c.base = srv.URL
	return c
}

func TestOrgRunnersPaginatedWithANarrowToken(t *testing.T) {
	var perms string
	var tokens int
	srv := server(t, "/orgs/acme", 250, &perms, &tokens)
	defer srv.Close()
	c := client(t, srv, "https://github.com/acme", "")
	rs, err := c.Runners(t.Context())
	if err != nil || len(rs) != 250 || rs[249].Name != "tier-0249" {
		t.Fatalf("runners = %d, %v", len(rs), err)
	}
	if perms != `{"permissions":{"organization_self_hosted_runners":"read"}}` {
		t.Fatalf("token permissions = %s, want runners read only", perms)
	}
	if _, err := c.Runners(t.Context()); err != nil || tokens != 1 {
		t.Fatalf("second list: err %v, %d token requests, want the token reused", err, tokens)
	}
}

func TestRepoScopeAsksForAdministrationRead(t *testing.T) {
	var perms string
	var tokens int
	srv := server(t, "/repos/acme/app", 3, &perms, &tokens)
	defer srv.Close()
	rs, err := client(t, srv, "https://github.com/acme/app", "").Runners(t.Context())
	if err != nil || len(rs) != 3 || !strings.Contains(perms, `"administration":"read"`) {
		t.Fatalf("runners %d err %v perms %s", len(rs), err, perms)
	}
}

func TestPAT(t *testing.T) {
	var perms string
	var tokens int
	srv := server(t, "/orgs/acme", 1, &perms, &tokens)
	defer srv.Close()
	if rs, err := client(t, srv, "https://github.com/acme", "ghp_x").Runners(t.Context()); err != nil || len(rs) != 1 || tokens != 0 {
		t.Fatalf("runners %d err %v tokens %d", len(rs), err, tokens)
	}
}

func TestErrors(t *testing.T) {
	var perms string
	var tokens int
	srv := server(t, "/orgs/other", 1, &perms, &tokens)
	defer srv.Close()
	if _, err := client(t, srv, "https://github.com/acme", "ghp_x").Runners(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v, want the HTTP status", err)
	}
	for _, bad := range []string{"http://github.com/acme", "https://github.com/", "https://github.com/a/b/c"} {
		if _, err := New(bad, "x", scaleset.GitHubAppAuth{}); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
}

func TestAPIBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme":      "https://api.github.com",
		"https://www.github.com/acme":  "https://api.github.com",
		"https://acme.ghe.com/org":     "https://api.acme.ghe.com",
		"https://ghes.example.com/org": "https://ghes.example.com/api/v3",
	} {
		u, _ := url.Parse(in)
		if got := APIBase(u); got != want {
			t.Errorf("APIBase(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestListingCachedThenRefreshed(t *testing.T) {
	var perms string
	var tokens int
	lists := 0
	inner := server(t, "/orgs/acme", 5, &perms, &tokens)
	defer inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runners") {
			lists++
		}
		resp, err := http.DefaultClient.Do(mustReq(t, r, inner.URL))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer srv.Close()
	c := client(t, srv, "https://github.com/acme", "")
	now := time.Now()
	c.now = func() time.Time { return now }
	tokenExpiry = func() time.Time { return now.Add(time.Hour) }
	defer func() { tokenExpiry = func() time.Time { return time.Now().Add(time.Hour) } }()
	for range 3 { // three tiers asking within a minute
		if _, err := c.Runners(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if lists != 1 {
		t.Fatalf("listed %d times within CacheFor, want 1", lists)
	}
	now = now.Add(2 * time.Minute)
	if _, err := c.Runners(t.Context()); err != nil || lists != 2 {
		t.Fatalf("after CacheFor: lists %d err %v", lists, err)
	}
	// The token (valid an hour) is reused; past its expiry a new one is
	// minted.
	if tokens != 1 {
		t.Fatalf("minted %d tokens, want 1", tokens)
	}
	now = now.Add(2 * time.Hour)
	if _, err := c.Runners(t.Context()); err != nil || tokens != 2 {
		t.Fatalf("after expiry: tokens %d err %v", tokens, err)
	}
}

func mustReq(t *testing.T, r *http.Request, base string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(r.Method, base+r.URL.RequestURI(), r.Body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = r.Header.Clone()
	return req
}

func TestErrorsSayWhy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer limited":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1761300000")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
		}
	}))
	defer srv.Close()
	_, err := client(t, srv, "https://github.com/acme", "limited").Runners(t.Context())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403: API rate limit exceeded (rate limit spent until 1761300000)") {
		t.Fatalf("rate limit: %v", err)
	}
	c := client(t, srv, "https://github.com/acme", "")
	c.token, c.expiry = "revoked", time.Now().Add(time.Hour)
	if _, err := c.Runners(t.Context()); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("401: %v", err)
	}
	if c.token != "" {
		t.Fatal("a 401 kept the cached token")
	}
}

func TestTokenWithoutExpiryRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"token":"t"}`)
	}))
	defer srv.Close()
	if _, err := client(t, srv, "https://github.com/acme", "").Runners(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid installation token") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnterpriseURLRejected(t *testing.T) {
	if _, err := New("https://github.com/enterprises/acme", "x", scaleset.GitHubAppAuth{}); err == nil || !strings.Contains(err.Error(), "enterprise") {
		t.Fatalf("err = %v", err)
	}
}
