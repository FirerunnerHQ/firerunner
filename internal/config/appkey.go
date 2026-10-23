package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolvePrivateKey returns the GitHub App private key as PEM, after checking
// that it parses as an RSA private key. --app-private-key (FR_APP_PRIVATE_KEY)
// holds either the PEM itself or the path to a PEM file.
//
// It returns "" and no error when no key is configured and none is needed (a
// --token is set, or no App is). With an App and no token, a missing,
// unreadable or unparsable key is an error: the scale set client would
// otherwise be handed an unusable key and fail, or panic, only once it signs
// its first JWT. Startup, `config check` and `doctor` all call this, so they
// agree on every key.
//
// No error ever contains key material. A file path is named only when it is
// plainly a path (absolute, or a file that exists): a relative value that
// names no file may be key material pasted without its header.
func (c *Config) ResolvePrivateKey() (string, error) {
	const flagName = "--app-private-key"
	v := c.AppPrivateKey
	if v == "" {
		if c.AppClientID != "" && c.Token == "" {
			return "", errors.New(flagName + " is required with --app-client-id (or set --token)")
		}
		return "", nil
	}
	if strings.Contains(v, "PRIVATE KEY") {
		if _, err := ParseAppPrivateKey([]byte(v)); err != nil {
			return "", fmt.Errorf("%s (inline PEM): %w", flagName, err)
		}
		return v, nil
	}
	if strings.ContainsAny(v, "\n\r") {
		return "", errors.New(flagName + ` is neither PEM with a "PRIVATE KEY" header nor a file path`)
	}
	b, err := os.ReadFile(v)
	if err != nil {
		var why string
		switch {
		case errors.Is(err, os.ErrNotExist):
			why = "file does not exist"
		case errors.Is(err, os.ErrPermission):
			why = "file is not readable (permission denied)"
		default:
			why = "file could not be read"
		}
		return "", fmt.Errorf("%s: %s", keySource(flagName, v), why)
	}
	if _, err := ParseAppPrivateKey(b); err != nil {
		return "", fmt.Errorf("%s: %w", keySource(flagName, v), err)
	}
	return string(b), nil
}

// keySource names the key's file in an error, unless the value might be key
// material rather than a path; see ResolvePrivateKey.
func keySource(flagName, path string) string {
	if filepath.IsAbs(path) {
		return flagName + " " + path
	}
	if _, err := os.Stat(path); err == nil {
		return flagName + " " + path
	}
	return flagName
}

// ParseAppPrivateKey parses a GitHub App private key. It accepts exactly what
// the scale set client's JWT signer accepts (golang-jwt's
// ParseRSAPrivateKeyFromPEM): the first PEM block in the input, holding an
// RSA key in PKCS#1 ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE KEY") form.
// GitHub has issued App keys in both forms. Errors describe the input's shape
// and never include its bytes.
func ParseAppPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("file holds only whitespace")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if _, ecErr := x509.ParseECPrivateKey(block.Bytes); ecErr == nil {
			return nil, errors.New("not an RSA private key (an EC private key)")
		}
		return nil, fmt.Errorf("not an RSA private key (PEM block %s does not parse as PKCS#1 or PKCS#8)", blockType(block.Type))
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key (a PKCS#8 %s key)", keyKind(k))
	}
	return rk, nil
}

// blockType quotes a PEM block's type for an error message, if it looks like
// a type label (as "CERTIFICATE" does) rather than arbitrary content.
func blockType(t string) string {
	if t == "" || len(t) > 40 {
		return "of an unexpected type"
	}
	for _, r := range t {
		if (r < 'A' || r > 'Z') && r != ' ' {
			return "of an unexpected type"
		}
	}
	return fmt.Sprintf("%q", t)
}

func keyKind(k any) string {
	switch k.(type) {
	case *ecdsa.PrivateKey:
		return "EC"
	case ed25519.PrivateKey:
		return "Ed25519"
	default:
		return "non-RSA"
	}
}
