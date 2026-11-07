package listener

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// fakeGitHub answers what the scale set client asks to reach the Actions
// service. It records the Authorization of each registration-token request and
// counts the Actions service calls, which it refuses with 403.
type fakeGitHub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	seen    []string
	service atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{}
	exp := time.Now().Add(time.Hour).Unix()
	admin := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".sig"
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/orgs/acme/actions/runners/registration-token":
			f.mu.Lock()
			f.seen = append(f.seen, r.Header.Get("Authorization"))
			f.mu.Unlock()
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"reg","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/api/v3/actions/runner-registration":
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, f.srv.URL+"/svc", admin)
		default:
			f.service.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// client is a scale set client built as newClient builds one for brokered
// tokens, trusting f's certificate, with quick retries.
func (f *fakeGitHub) client(t *testing.T, tokens func(context.Context) (string, error)) *scaleset.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: f.srv.URL + "/acme", PersonalAccessToken: brokered,
	}, append(brokeredOptions(tokens, nil), scaleset.WithRootCAs(pool),
		scaleset.WithRetryMax(2), scaleset.WithRetryWaitMax(time.Millisecond))...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The real library, given the placeholder, sends GitHub the token Tokens
// returns, and never the placeholder. If an upgrade of actions/scaleset stops
// asking its transport's proxy function about each request, this fails.
func TestBrokeredTokenReachesGitHub(t *testing.T) {
	f := newFakeGitHub(t)
	var n atomic.Int32
	c := f.client(t, func(context.Context) (string, error) { return fmt.Sprintf("ghs_%d", n.Add(1)), nil })
	// Any Actions service call first fetches a registration token.
	_, _ = c.GetRunnerGroupByName(context.Background(), "default")
	got := f.auths()
	if len(got) == 0 {
		t.Fatal("the library asked GitHub for no registration token")
	}
	if got[0] != "Bearer ghs_1" {
		t.Fatalf("GitHub saw %q, want the brokered token", got[0])
	}
	// The library's own retrying client, not one shared and changed by it: a
	// refusal from the Actions service is not retried as a 5xx would be.
	if n := f.service.Load(); n != 1 {
		t.Fatalf("the Actions service was asked %d times for one refused call", n)
	}
}

// Without a token, nothing reaches GitHub, the library tries again, and its
// error is not one that stops the tier.
func TestBrokeredTokenFailureIsTransient(t *testing.T) {
	f := newFakeGitHub(t)
	var asks atomic.Int32
	c := f.client(t, func(context.Context) (string, error) {
		asks.Add(1)
		return "", errors.New("ThrottlingException")
	})
	_, err := c.GetRunnerGroupByName(context.Background(), "default")
	if err == nil {
		t.Fatal("no error without a token")
	}
	if isFatal(err) {
		t.Fatalf("a missing token stops the tier: %v", err)
	}
	if got := f.auths(); len(got) != 0 {
		t.Fatalf("GitHub was asked for a registration token %d times, with %q", len(got), got)
	}
	if asks.Load() < 2 {
		t.Fatalf("asked for a token %d times: the library's retries should ask again", asks.Load())
	}
}
