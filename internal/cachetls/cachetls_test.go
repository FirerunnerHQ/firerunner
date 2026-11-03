package cachetls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pool(t *testing.T, ca *CA) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA certificate does not parse")
	}
	return p
}

func TestLoadOrCreateKeepsTheKeyPrivateAndReloads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	ca, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{caKeyFile: 0o600, caCertFile: 0o644} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v mode %v, want %v", name, err, fi.Mode().Perm(), want)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", fi.Mode().Perm())
	}
	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.CertPEM()) != string(ca.CertPEM()) {
		t.Fatal("a restart made a new CA instead of loading the one on disk")
	}
}

func TestLoadOrCreateRefusesAMismatchAndReplacesHalfACA(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if _, err := LoadOrCreate(other); err != nil {
		t.Fatal(err)
	}
	// Another CA's key with this certificate: tampering, not a crash.
	key, _ := os.ReadFile(filepath.Join(other, caKeyFile))
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), "do not belong together") {
		t.Fatalf("mismatched key: %v", err)
	}
	// A crash between the two writes leaves one file: start over.
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("certificate without key: %v, want a new CA", err)
	}
	if string(ca.CertPEM()) == string(first.CertPEM()) {
		t.Fatal("kept a certificate whose key is gone")
	}
}

func TestLoadOrCreateGuardsTheDirectoryAndKey(t *testing.T) {
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o777); err != nil { // past the umask
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(loose); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o700 {
		t.Fatalf("a loose CA dir stayed %v, want 0700", fi.Mode().Perm())
	}

	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, caKeyFile)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), "must have mode 0600") {
		t.Fatalf("readable key: %v, want refused", err)
	}
	planted := filepath.Join(t.TempDir(), "planted.key")
	key, _ := os.ReadFile(keyPath)
	if err := os.WriteFile(planted, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(planted, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked key: %v, want refused", err)
	}
}

func TestLoadOrCreateReplacesACANearExpiry(t *testing.T) {
	dir := t.TempDir()
	old, keyDER, err := create(time.Now().Add(-caLifetime + 10*24*time.Hour)) // 10 days left
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, caKeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, caCertFile), old.CertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca.CertPEM()) == string(old.CertPEM()) || ca.cert.NotAfter.Before(time.Now().Add(caLifetime-time.Hour)) {
		t.Fatal("a CA 10 days from expiry was kept")
	}
}

func TestLeafRefusesToOutliveTheCA(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca.now = func() time.Time { return ca.cert.NotAfter.Add(-time.Hour) }
	if _, _, err := ca.Leaf(2 * time.Hour); err == nil {
		t.Fatal("issued a leaf that outlives its CA")
	}
	if _, _, err := ca.Leaf(30 * time.Minute); err != nil {
		t.Fatalf("a leaf within the CA's life: %v", err)
	}
}

func TestLeafVerifiesForTheResultsHostOnlyAndExpires(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Leaf(2 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("leaf certificate and key do not pair: %v", err)
	}
	b, _ := pem.Decode(certPEM)
	leaf, _ := x509.ParseCertificate(b.Bytes)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: ResultsHost, Roots: pool(t, ca)}); err != nil {
		t.Fatalf("leaf does not verify for %s: %v", ResultsHost, err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "api.github.com", Roots: pool(t, ca)}); err == nil {
		t.Fatal("leaf verifies for another name")
	}
	if life := leaf.NotAfter.Sub(time.Now()); life > 2*time.Hour || life < 2*time.Hour-time.Minute {
		t.Fatalf("leaf lives %v, want the 2h asked for", life)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: ResultsHost, Roots: pool(t, ca), CurrentTime: time.Now().Add(3 * time.Hour)}); err == nil {
		t.Fatal("leaf still verifies after its lifetime")
	}
}

// TestCAIsConstrainedToTheResultsHost: even a certificate the CA's own key
// signs for another name does not verify, so the key could impersonate
// nothing else.
func TestCAIsConstrainedToTheResultsHost(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "github.com"}, DNSNames: []string{"github.com"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	forged, _ := x509.ParseCertificate(der)
	if _, err := forged.Verify(x509.VerifyOptions{DNSName: "github.com", Roots: pool(t, ca)}); err == nil {
		t.Fatal("a certificate for github.com signed by the cache CA verifies")
	}
}

// TestLeafServesTLSAsTheResultsHost: a client trusting only the CA reaches a
// server presenting a leaf, under the results host's name, as the runner and
// node would through the proxy.
func TestLeafServesTLSAsTheResultsHost(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Leaf(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool(t, ca), ServerName: ResultsHost}}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("TLS as %s: %v", ResultsHost, err)
	}
	resp.Body.Close()
}

func TestBundleCarriesAWorkingLeafAndTheUpstream(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ca.Bundle(time.Hour, "10.0.0.5:8099")
	if err != nil {
		t.Fatal(err)
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if b.Upstream != "10.0.0.5:8099" || b.CA != string(ca.CertPEM()) {
		t.Fatalf("bundle upstream %q, CA match %v", b.Upstream, b.CA == string(ca.CertPEM()))
	}
	if _, err := tls.X509KeyPair([]byte(b.Cert), []byte(b.Key)); err != nil {
		t.Fatalf("bundle's leaf and key do not pair: %v", err)
	}
	if _, err := ca.Bundle(time.Hour, ""); err == nil {
		t.Fatal("a bundle without an upstream")
	}
}
