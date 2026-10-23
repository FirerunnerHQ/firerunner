package diag

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/config"
)

func TestGithubAPIBase(t *testing.T) {
	cases := map[string]string{
		"https://github.com/solcreek":  "https://api.github.com",
		"https://github.com/org/repo":  "https://api.github.com",
		"https://www.github.com/org":   "https://api.github.com",
		"https://ghe.example.com/org":  "https://ghe.example.com/api/v3",
		"http://ghe.internal/org/repo": "http://ghe.internal/api/v3",
		"":                             "https://api.github.com",
		"://bogus":                     "https://api.github.com",
		"ghe.example.com/org":          "https://api.github.com", // no scheme => no host
	}
	for in, want := range cases {
		if got := githubAPIBase(in); got != want {
			t.Errorf("githubAPIBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func testRSAKeyPEM(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	return string(p), key
}

func TestAppJWT(t *testing.T) {
	pemStr, key := testRSAKeyPEM(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := appJWT("Iv1.client", pemStr, now)
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	// Signature must verify against the public key.
	signing := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sum := sha256.Sum256([]byte(signing))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	// Claims must carry issuer and a sane iat/exp window.
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "Iv1.client" {
		t.Errorf("iss = %q, want Iv1.client", claims.Iss)
	}
	if claims.Iat != now.Add(-60*time.Second).Unix() {
		t.Errorf("iat = %d", claims.Iat)
	}
	if claims.Exp <= now.Unix() || claims.Exp > now.Add(10*time.Minute).Unix() {
		t.Errorf("exp %d outside GitHub's 10-min window", claims.Exp)
	}
}

func TestAuthVerify_PAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" {
			t.Errorf("PAT path = %q, want /rate_limit", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer ghp_valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if c := authVerify1(&config.Config{Token: "ghp_valid"}, srv.URL); c.Level != levelPass {
		t.Errorf("valid PAT: got %s %s", c.Level, c.Detail)
	}
	if c := authVerify1(&config.Config{Token: "ghp_wrong"}, srv.URL); c.Level != levelFail {
		t.Errorf("wrong PAT: want FAIL, got %s", c.Level)
	}
	// Unreachable endpoint => WARN, not FAIL.
	if c := authVerify1(&config.Config{Token: "ghp_valid"}, "http://127.0.0.1:1"); c.Level != levelWarn {
		t.Errorf("offline: want WARN, got %s %s", c.Level, c.Detail)
	}
}

func TestAuthVerify_App(t *testing.T) {
	pemStr, _ := testRSAKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/123" {
			t.Errorf("App path = %q", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.Count(auth, ".") != 2 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{AppClientID: "Iv1.client", AppInstallID: 123, AppPrivateKey: pemStr}
	if c := authVerify1(cfg, srv.URL); c.Level != levelPass {
		t.Errorf("valid App: got %s %s", c.Level, c.Detail)
	}

	bad := &config.Config{AppClientID: "Iv1.client", AppInstallID: 123, AppPrivateKey: "PRIVATE KEY garbage"}
	if c := authVerify1(bad, srv.URL); c.Level != levelFail {
		t.Errorf("bad key: want FAIL, got %s", c.Level)
	}
}

func TestReflinkProbe_BogusDir(t *testing.T) {
	if reflinkProbe("/nonexistent/firerunner-does-not-exist") {
		t.Error("reflinkProbe on missing dir should be false")
	}
}

// authVerify1 is authVerify's first check, auth-verify itself.
func authVerify1(cfg *config.Config, apiBase string) Check { return authVerify(cfg, apiBase)[0] }

func TestAuthVerify_AppPermissions(t *testing.T) {
	pemStr, _ := testRSAKeyPEM(t)
	perms := `{"metadata":"read","organization_self_hosted_runners":"write","packages":"read"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":123,"permissions":%s}`, perms)
	}))
	defer srv.Close()
	cfg := &config.Config{URL: "https://github.com/acme", AppClientID: "Iv1.client", AppInstallID: 123, AppPrivateKey: pemStr}
	checks := authVerify(cfg, srv.URL)
	if len(checks) != 2 || checks[1].Name != "app-permissions" {
		t.Fatalf("checks = %+v, want auth-verify then app-permissions", checks)
	}
	if c := checks[1]; c.Level != levelWarn || !strings.Contains(c.Detail, "packages: read (lets the key pull private packages") {
		t.Fatalf("extra packages: %s %s", c.Level, c.Detail)
	}
	perms = `{"metadata":"read","organization_self_hosted_runners":"write"}`
	if c := authVerify(cfg, srv.URL)[1]; c.Level != levelPass {
		t.Fatalf("least privilege: %s %s", c.Level, c.Detail)
	}
	// A PAT has no installation to inspect.
	if n := len(authVerify(&config.Config{Token: "ghp_x"}, srv.URL)); n != 1 {
		t.Fatalf("PAT returned %d checks", n)
	}
}

func TestAppPermissions(t *testing.T) {
	for _, c := range []struct {
		name    string
		granted map[string]string
		repo    bool
		level   string
		detail  string
	}{
		{"org, minimal", map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"}, false, levelPass, "least privilege"},
		{"repo, minimal", map[string]string{"metadata": "read", "administration": "write"}, true, levelPass, "administration (write)"},
		{"org, runners read only", map[string]string{"metadata": "read", "organization_self_hosted_runners": "read"}, false, levelFail, "needs write"},
		{"repo given org permission", map[string]string{"organization_self_hosted_runners": "write"}, true, levelFail, "administration"},
		{"org, extras sorted", map[string]string{"organization_self_hosted_runners": "write", "packages": "read", "contents": "read", "zz_new": "read"}, false, levelWarn,
			"contents: read (lets the key read (or write) repository source); packages: read (lets the key pull private packages and container images, which often hold source or dependencies); zz_new: read (lets the key access GitHub beyond managing runners)"},
	} {
		got := appPermissions(c.granted, c.repo)
		if got.Level != c.level || !strings.Contains(got.Detail, c.detail) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, got.Level, got.Detail, c.level, c.detail)
		}
	}
	if !repoScoped("https://github.com/acme/app") || repoScoped("https://github.com/acme") || repoScoped("https://github.com/acme/") {
		t.Error("repoScoped")
	}
}
