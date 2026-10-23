package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// testKeys generates every key shape the table needs. Nothing here is
// committed key material: each run makes its own.
type testKeys struct {
	pkcs1, pkcs8, cert, ecSEC1, ecPKCS8 []byte
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p8, err := x509.MarshalPKCS8PrivateKey(rk)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rk.PublicKey, rk)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(ek)
	if err != nil {
		t.Fatal(err)
	}
	ep8, err := x509.MarshalPKCS8PrivateKey(ek)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(typ string, b []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}) }
	return testKeys{
		pkcs1:   enc("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rk)),
		pkcs8:   enc("PRIVATE KEY", p8),
		cert:    enc("CERTIFICATE", cert),
		ecSEC1:  enc("EC PRIVATE KEY", sec1),
		ecPKCS8: enc("PRIVATE KEY", ep8),
	}
}

// pemBody returns the base64 lines of a PEM, the part that is key material.
func pemBody(b []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "-----") {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestResolvePrivateKey_File reads each key shape from a file, as
// FR_APP_PRIVATE_KEY=/path does in production. Every rejection names the
// flag and the file and describes the problem; none contains key bytes.
func TestResolvePrivateKey_File(t *testing.T) {
	k := newTestKeys(t)
	for _, tc := range []struct {
		name    string
		content []byte // nil: no file at all
		wantErr string // "" means accepted
	}{
		{"empty", []byte{}, "file is empty"},
		{"whitespace", []byte(" \n\t\r\n"), "file holds only whitespace"},
		{"garbage", []byte("this is not a key\n"), "no PEM block"},
		{"certificate", k.cert, `not an RSA private key (PEM block "CERTIFICATE"`},
		{"EC SEC1", k.ecSEC1, "not an RSA private key (an EC private key)"},
		{"EC PKCS#8", k.ecPKCS8, "not an RSA private key (a PKCS#8 EC key)"},
		{"RSA PKCS#1", k.pkcs1, ""},
		{"RSA PKCS#8", k.pkcs8, ""},
		{"RSA PKCS#1 CRLF", []byte(strings.ReplaceAll(string(k.pkcs1), "\n", "\r\n")), ""},
		{"RSA PKCS#1 after text", append([]byte("Bag Attributes\n"), k.pkcs1...), ""},
		{"missing", nil, "file does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app-key.pem")
			if tc.content != nil {
				if err := os.WriteFile(path, tc.content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := &Config{AppClientID: "Iv1.abc", AppInstallID: 1, AppPrivateKey: path}
			got, err := c.ResolvePrivateKey()
			if tc.wantErr == "" {
				if err != nil || got != string(tc.content) {
					t.Fatalf("want the key accepted, got err %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error %q, got none", tc.wantErr)
			}
			msg := err.Error()
			want := "--app-private-key " + path + ": " + tc.wantErr
			if !strings.HasPrefix(msg, want) {
				t.Fatalf("error = %q, want prefix %q", msg, want)
			}
			for _, l := range pemBody(tc.content) {
				if strings.Contains(msg, l) {
					t.Fatalf("error contains key bytes: %q", msg)
				}
			}
		})
	}
}

// TestParseAppPrivateKey_MatchesScaleSet checks that ParseAppPrivateKey
// accepts exactly what the scale set client's JWT signer accepts, so a key
// that works is never refused and one that would fail is never let through.
func TestParseAppPrivateKey_MatchesScaleSet(t *testing.T) {
	k := newTestKeys(t)
	for name, in := range map[string][]byte{
		"empty": {}, "whitespace": []byte(" \n"), "garbage": []byte("nope"),
		"certificate": k.cert, "EC SEC1": k.ecSEC1, "EC PKCS#8": k.ecPKCS8,
		"RSA PKCS#1": k.pkcs1, "RSA PKCS#8": k.pkcs8,
		"RSA PKCS#1 CRLF":       []byte(strings.ReplaceAll(string(k.pkcs1), "\n", "\r\n")),
		"RSA PKCS#1 after text": append([]byte("Bag Attributes\n"), k.pkcs1...),
		"RSA PKCS#8 as RSA PRIVATE KEY": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
			Bytes: func() []byte { b, _ := pem.Decode(k.pkcs8); return b.Bytes }()}),
		"truncated": k.pkcs1[:len(k.pkcs1)/2],
	} {
		_, ours := ParseAppPrivateKey(in)
		_, theirs := jwt.ParseRSAPrivateKeyFromPEM(in)
		if (ours == nil) != (theirs == nil) {
			t.Errorf("%s: ParseAppPrivateKey err %v, scale set's parser err %v", name, ours, theirs)
		}
	}
}

func TestResolvePrivateKey_Inline(t *testing.T) {
	k := newTestKeys(t)
	c := &Config{AppClientID: "Iv1.abc", AppPrivateKey: string(k.pkcs8)}
	if got, err := c.ResolvePrivateKey(); err != nil || got != string(k.pkcs8) {
		t.Fatalf("inline PKCS#8: err %v", err)
	}
	c.AppPrivateKey = string(k.ecPKCS8)
	_, err := c.ResolvePrivateKey()
	if err == nil || !strings.HasPrefix(err.Error(), "--app-private-key (inline PEM): not an RSA private key") {
		t.Fatalf("inline EC key: err %v", err)
	}
	for _, l := range pemBody(k.ecPKCS8) {
		if strings.Contains(err.Error(), l) {
			t.Fatalf("error contains key bytes: %v", err)
		}
	}
}

func TestResolvePrivateKey_Unset(t *testing.T) {
	// No App, or an App beside a token: nothing to resolve.
	for _, c := range []*Config{{}, {Token: "t"}, {Token: "t", AppClientID: "Iv1.abc"}} {
		if got, err := c.ResolvePrivateKey(); err != nil || got != "" {
			t.Fatalf("%+v: got %q err %v", c, got, err)
		}
	}
	// An App with no key and no token would reach the scale set client with
	// no credentials at all.
	_, err := (&Config{AppClientID: "Iv1.abc", AppInstallID: 1}).ResolvePrivateKey()
	if err == nil || !strings.Contains(err.Error(), "--app-private-key is required") {
		t.Fatalf("App without key: err %v", err)
	}
}

func TestFromFlags_AppNeedsInstallationID(t *testing.T) {
	_, err := FromFlags([]string{"--url", "u", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0",
		"--app-client-id", "Iv1.abc"})
	if err == nil || !strings.Contains(err.Error(), "--app-installation-id is required") {
		t.Fatalf("err %v", err)
	}
}
