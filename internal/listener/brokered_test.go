package listener

import (
	"context"
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
	"github.com/hashicorp/go-retryablehttp"
)

// fakeGitHub answers what the scale set client asks to reach the Actions
// service, and records the Authorization of each registration-token request.
type fakeGitHub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{}
	exp := time.Now().Add(time.Hour).Unix()
	admin := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".sig"
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
			w.WriteHeader(http.StatusNotFound)
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

// client is a scale set client on the brokered path, trusting f's certificate.
func (f *fakeGitHub) client(t *testing.T, tokens func(context.Context) (string, error)) *scaleset.Client {
	t.Helper()
	rc := retryablehttp.NewClient()
	rc.HTTPClient.Transport = f.srv.Client().Transport
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: f.srv.URL + "/acme", PersonalAccessToken: brokered,
	}, scaleset.WithRetryableHTTPClint(withToken(rc, tokens, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The real library, given the placeholder, sends GitHub the token Tokens
// returns, and never the placeholder. If an upgrade of actions/scaleset stops
// calling retryablehttp's hook with the request it sends, this fails.
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
}

// Without a token, GitHub gets no Authorization: never the placeholder.
func TestBrokeredTokenFailureSendsNone(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t, func(context.Context) (string, error) { return "", errors.New("KMS down") })
	if _, err := c.GetRunnerGroupByName(context.Background(), "default"); err == nil {
		t.Fatal("no error without a token")
	}
	for _, a := range f.auths() {
		if a != "" {
			t.Fatalf("GitHub saw %q", a)
		}
	}
}
