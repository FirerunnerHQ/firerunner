// Package cachetls issues the TLS identity an EC2 runner's cache proxy
// presents for GitHub's results host, so actions/cache reaches firerunner's
// cache-server without a patched runner.
//
// One CA per deployment lives in the control VM's work dir; its key never
// leaves it. The CA is name-constrained to the results host, so even its key
// could sign for no other name (for clients that enforce constraints, as Go,
// OpenSSL and Node do). Each instance gets its own leaf for that one name,
// valid for the instance's lifetime, in its one-time handoff: a job that reads
// its leaf key holds a certificate for one host that expires with the VM.
//
// A tier with a git cache gets a second CA, for github.com, with its own leaf.
// It stays apart because it is trusted differently: the results host's CA goes
// into the instance's system store, which would make every tool there accept a
// leaf for github.com. The git CA is trusted by git alone, through sslCAInfo.
package cachetls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ResultsHost is the name the toolkit's cache and artifact clients reach:
// ACTIONS_RESULTS_URL's host.
const ResultsHost = "results-receiver.actions.githubusercontent.com"

// GitHost is the name git reaches, and the git proxy answers as.
const GitHost = "github.com"

// File names in the CA's directory.
const (
	caCertFile = "cache-ca.pem"
	caKeyFile  = "cache-ca.key"
)

// caLifetime bounds the CA; a replaced control VM makes a new one anyway.
// One within caRenew of expiry is replaced at start: nothing but the bundles
// trusts it, and live instances keep the copy they booted with.
const (
	caLifetime = 5 * 365 * 24 * time.Hour
	caRenew    = 30 * 24 * time.Hour
)

// CA signs leaf certificates for one host.
type CA struct {
	host    string
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
	now     func() time.Time
}

// LoadOrCreate reads the CA from dir, or creates one there: its key mode
// 0600, in a directory only its owner may enter. The directory must belong to
// this process's user (a looser mode is tightened), so no other local user can
// plant a CA of their own. A lone certificate or key, as a crash between the
// two writes leaves, or a CA near its expiry, is replaced; a certificate and
// key that do not belong together are refused.
func LoadOrCreate(dir string) (*CA, error) { return LoadOrCreateFor(dir, ResultsHost) }

// LoadOrCreateFor is LoadOrCreate for a CA constrained to host. A CA in dir
// constrained to anything else is refused, as it would sign leaves for the
// wrong name.
func LoadOrCreateFor(dir, host string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := ownDir(dir); err != nil {
		return nil, err
	}
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := readKey(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		ca, err := parse(certPEM, keyPEM, host)
		if err != nil {
			return nil, err
		}
		if time.Now().Add(caRenew).Before(ca.cert.NotAfter) {
			return ca, nil
		}
	case certErr != nil && !errors.Is(certErr, os.ErrNotExist):
		return nil, certErr
	case keyErr != nil && !errors.Is(keyErr, os.ErrNotExist):
		return nil, keyErr
	}
	ca, keyDER, err := create(time.Now(), host)
	if err != nil {
		return nil, err
	}
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, ca.certPEM, 0o644); err != nil {
		return nil, err
	}
	return ca, nil
}

// ownDir refuses a CA directory another user owns, or that is a symlink, and
// tightens one of ours to 0700.
func ownDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("cachetls: %s is not a directory", dir)
	}
	if uid, ok := owner(fi); ok && uid != os.Getuid() {
		return fmt.Errorf("cachetls: %s belongs to uid %d, not this process's %d: another user could plant a CA there", dir, uid, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// readKey reads the CA key, which must be a regular file only its owner may
// read.
func readKey(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("cachetls: %s is not a regular file (%v): the key must not be a link", path, fi.Mode())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("cachetls: %s has mode %v; the key must have mode 0600", path, fi.Mode().Perm())
	}
	return os.ReadFile(path)
}

func create(now time.Time, host string) (*CA, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "firerunner cache CA", Organization: []string{"firerunner"}},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caLifetime),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{host: host, cert: cert, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: key, now: time.Now}, keyDER, nil
}

func parse(certPEM, keyPEM []byte, host string) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("cachetls: CA certificate or key is not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cachetls: CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cachetls: CA key: %w", err)
	}
	if !cert.IsCA || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("cachetls: CA certificate and key do not belong together")
	}
	if len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != host {
		return nil, fmt.Errorf("cachetls: CA is constrained to %v, not %s", cert.PermittedDNSDomains, host)
	}
	return &CA{host: host, cert: cert, certPEM: certPEM, key: key, now: time.Now}, nil
}

// CertPEM is the CA certificate instances add to their trust.
func (c *CA) CertPEM() []byte { return c.certPEM }

// Leaf issues a certificate and key for the CA's host, valid for ttl from now
// (and never past the CA).
func (c *CA) Leaf(ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := c.now()
	notAfter := now.Add(ttl)
	if notAfter.After(c.cert.NotAfter) {
		// The leaf would outlive the CA and stop verifying mid-job.
		return nil, nil, fmt.Errorf("cachetls: the CA expires at %s, before a leaf for %s would; restart to replace it", c.cert.NotAfter.Format(time.RFC3339), ttl)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: c.host},
		DNSNames:     []string{c.host},
		NotBefore:    now.Add(-5 * time.Minute), // the instance's clock may lag
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// Bundle is what an instance's cache proxy needs, handed to it once with its
// config (jitserver.CachePath): the CA to trust, its own leaf, where
// cache-server listens and, for a tier with a git cache, the git proxy's
// identity.
type Bundle struct {
	CA       string       `json:"ca"`
	Cert     string       `json:"cert"`
	Key      string       `json:"key"`
	Upstream string       `json:"upstream"` // host:port of cache-server
	Git      *GitIdentity `json:"git,omitempty"`
}

// GitIdentity is the git proxy's certificate for GitHost, and the CA git is
// told to trust for it.
type GitIdentity struct {
	CA   string `json:"ca"`
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Bundle issues a leaf for one instance, valid for ttl, and packs it with the
// CA and cache-server's address as JSON. With git set, it adds a leaf from that
// CA for the instance's git proxy.
func (c *CA) Bundle(ttl time.Duration, upstream string, git *CA) ([]byte, error) {
	if upstream == "" {
		return nil, errors.New("cachetls: a bundle needs the cache-server address")
	}
	cert, key, err := c.Leaf(ttl)
	if err != nil {
		return nil, err
	}
	b := Bundle{CA: string(c.certPEM), Cert: string(cert), Key: string(key), Upstream: upstream}
	if git != nil {
		gitCert, gitKey, err := git.Leaf(ttl)
		if err != nil {
			return nil, err
		}
		b.Git = &GitIdentity{CA: string(git.certPEM), Cert: string(gitCert), Key: string(gitKey)}
	}
	return json.Marshal(b)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return n
}

// writeFile writes atomically with perm, so a crash leaves no half-written
// key that LoadOrCreate would then refuse.
func writeFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cachetls-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // a power loss must not leave it empty
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
