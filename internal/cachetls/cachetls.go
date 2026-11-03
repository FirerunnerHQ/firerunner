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

// File names in the CA's directory.
const (
	caCertFile = "cache-ca.pem"
	caKeyFile  = "cache-ca.key"
)

// caLifetime bounds the CA; a replaced control VM makes a new one anyway.
const caLifetime = 5 * 365 * 24 * time.Hour

// CA signs leaf certificates for ResultsHost.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
	now     func() time.Time
}

// LoadOrCreate reads the CA from dir, or creates one there: its key mode
// 0600, in a directory only its owner may enter.
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return parse(certPEM, keyPEM)
	case !errors.Is(certErr, os.ErrNotExist) && certErr != nil:
		return nil, certErr
	case !errors.Is(keyErr, os.ErrNotExist) && keyErr != nil:
		return nil, keyErr
	case certErr == nil || keyErr == nil:
		return nil, fmt.Errorf("cachetls: %s holds only half a CA (certificate or key); remove both to make a new one", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ca, keyDER, err := create(time.Now())
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

func create(now time.Time) (*CA, []byte, error) {
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
		PermittedDNSDomains:         []string{ResultsHost},
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
	return &CA{cert: cert, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: key, now: time.Now}, keyDER, nil
}

func parse(certPEM, keyPEM []byte) (*CA, error) {
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
	return &CA{cert: cert, certPEM: certPEM, key: key, now: time.Now}, nil
}

// CertPEM is the CA certificate instances add to their trust.
func (c *CA) CertPEM() []byte { return c.certPEM }

// Leaf issues a certificate and key for ResultsHost, valid for ttl from now
// (and never past the CA).
func (c *CA) Leaf(ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := c.now()
	notAfter := now.Add(ttl)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: ResultsHost},
		DNSNames:     []string{ResultsHost},
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
// config (jitserver.CachePath): the CA to trust, its own leaf, and where
// cache-server listens.
type Bundle struct {
	CA       string `json:"ca"`
	Cert     string `json:"cert"`
	Key      string `json:"key"`
	Upstream string `json:"upstream"` // host:port of cache-server
}

// Bundle issues a leaf for one instance, valid for ttl, and packs it with the
// CA and cache-server's address as JSON.
func (c *CA) Bundle(ttl time.Duration, upstream string) ([]byte, error) {
	if upstream == "" {
		return nil, errors.New("cachetls: a bundle needs the cache-server address")
	}
	cert, key, err := c.Leaf(ttl)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Bundle{CA: string(c.certPEM), Cert: string(cert), Key: string(key), Upstream: upstream})
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
