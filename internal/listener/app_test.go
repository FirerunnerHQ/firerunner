package listener

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/ghauth"
)

// The scale set client authenticates as the App with the JWTs ghauth signs, by
// whatever signer: it is given no key. This runs the real library.
func TestClientSignsWithTheSharedJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var verified, registered atomic.Int32
	exp := time.Now().Add(time.Hour).Unix()
	admin := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".sig"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/app/installations/7/access_tokens":
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			if len(parts) == 3 {
				sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
				digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
				if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) == nil {
					verified.Add(1)
				}
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/api/v3/orgs/acme/actions/runners/registration-token":
			if r.Header.Get("Authorization") == "Bearer ghs_x" {
				registered.Add(1)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"reg","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/api/v3/actions/runner-registration":
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, srv.URL+"/svc", admin)
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

	c, err := newClient(Config{URL: srv.URL + "/acme",
		App: ghauth.App{InstallationID: 7, JWT: ghauth.NewJWT("Iv1", ghauth.KeySigner{Key: key})}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.GetRunnerGroupByName(context.Background(), "default")
	if verified.Load() == 0 || registered.Load() == 0 {
		t.Fatalf("JWTs verified %d, registrations with the installation token %d", verified.Load(), registered.Load())
	}
}
