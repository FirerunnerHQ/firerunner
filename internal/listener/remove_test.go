package listener

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// RemoveByName finds a registered scale set by its name in the runner group
// and deletes it; with none registered, it deletes nothing.
func TestRemoveByName(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	exp := time.Now().Add(time.Hour).Unix()
	admin := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".sig"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/orgs/acme/actions/runners/registration-token":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"reg","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/api/v3/actions/runner-registration":
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, srv.URL+"/svc", admin)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_apis/runtime/runnerscalesets"):
			if r.URL.Query().Get("runnerGroupId") != "1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("name") == "small" {
				fmt.Fprint(w, `{"count":1,"value":[{"id":7,"name":"small"}]}`)
				return
			}
			fmt.Fprint(w, `{"count":0,"value":[]}`)
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	old := clientOptions
	clientOptions = []scaleset.HTTPOption{scaleset.WithHTTPClient(scaleset.NewHTTPClient(scaleset.HTTPClientConfig{RootCAs: pool}))}
	defer func() { clientOptions = old }()

	cfg := Config{URL: srv.URL + "/acme", Token: "ghp_x", Name: "small"}
	if removed, err := RemoveByName(context.Background(), cfg); err != nil || !removed {
		t.Fatalf("removed %v, %v; want the registered one", removed, err)
	}
	cfg.Name = "large"
	if removed, err := RemoveByName(context.Background(), cfg); err != nil || removed {
		t.Fatalf("removed %v, %v; want none: none is registered", removed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/7") {
		t.Fatalf("deleted %v, want scale set 7 only", deleted)
	}
}

// Remove deletes the scale set even when this process did not create it,
// and even when told to keep scale sets on close: its tenant is gone.
func TestRemoveDeletesAScaleSetItFound(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	exp := time.Now().Add(time.Hour).Unix()
	admin := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".sig"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/orgs/acme/actions/runners/registration-token":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"reg","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/api/v3/actions/runner-registration":
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, srv.URL+"/svc", admin)
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	old := clientOptions
	clientOptions = []scaleset.HTTPOption{scaleset.WithHTTPClient(scaleset.NewHTTPClient(scaleset.HTTPClientConfig{RootCAs: pool}))}
	defer func() { clientOptions = old }()

	c, err := newClient(Config{URL: srv.URL + "/acme", Token: "ghp_x"})
	if err != nil {
		t.Fatal(err)
	}
	sess := &fakeSession{}
	s := &ScaleSet{cfg: Config{KeepOnClose: true}, log: testLogger(), session: sess, client: c, scaleSetID: 4, created: false}
	if err := s.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sess.closed.Load() {
		t.Fatal("the message session was not closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/4") {
		t.Fatalf("deleted %v, want scale set 4", deleted)
	}
}
