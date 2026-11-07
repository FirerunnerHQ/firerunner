// Package ghauth signs the JWTs a GitHub App authenticates with. The key may be
// in this process, or in AWS KMS, where it never leaves the service: a
// compromised process can then sign only while it runs, and every signature is
// in CloudTrail (#167).
package ghauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// Signer makes an RSASSA-PKCS1-v1_5 signature over a SHA-256 digest: RS256.
type Signer interface {
	SignSHA256(ctx context.Context, digest []byte) ([]byte, error)
}

// KeySigner signs with a key held in this process.
type KeySigner struct{ Key *rsa.PrivateKey }

func (s KeySigner) SignSHA256(_ context.Context, digest []byte) ([]byte, error) {
	return rsa.SignPKCS1v15(nil, s.Key, crypto.SHA256, digest)
}

// KMSAPI is the one call KMSSigner makes; *kms.Client satisfies it.
type KMSAPI interface {
	Sign(ctx context.Context, in *kms.SignInput, opts ...func(*kms.Options)) (*kms.SignOutput, error)
}

// KMSSigner signs with an RSA key in AWS KMS, by key ID, ARN or alias.
type KMSSigner struct {
	API   KMSAPI
	KeyID string
}

func (s KMSSigner) SignSHA256(ctx context.Context, digest []byte) ([]byte, error) {
	out, err := s.API.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.KeyID),
		Message:          digest,
		MessageType:      types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecRsassaPkcs1V15Sha256,
	})
	if err != nil {
		return nil, fmt.Errorf("ghauth: KMS sign with %s: %w", s.KeyID, err)
	}
	if len(out.Signature) == 0 {
		return nil, errors.New("ghauth: KMS returned no signature")
	}
	return out.Signature, nil
}

// JWT lifetimes. GitHub refuses an App JWT valid for more than ten minutes, and
// one issued in the future; iat is set back a minute for clock skew.
const (
	backdate = time.Minute
	lifetime = 9 * time.Minute
	// renewBefore is how long before expiry a cached JWT is replaced, so a
	// request never carries one that ends in flight.
	renewBefore = time.Minute
)

// JWT is a GitHub App's JWT, signed once and reused until it nears expiry: with
// KMS, each signature is a network call and costs one.
type JWT struct {
	issuer string
	signer Signer
	now    func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewJWT returns the JWTs of the App whose client ID (or App ID) is issuer.
func NewJWT(issuer string, s Signer) *JWT {
	return &JWT{issuer: issuer, signer: s, now: time.Now}
}

// WithClock makes j read the time from now, for tests and for checks that
// state the time they sign at.
func (j *JWT) WithClock(now func() time.Time) *JWT {
	j.now = now
	return j
}

// Token returns a JWT valid for at least renewBefore.
func (j *JWT) Token(ctx context.Context) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	if j.token != "" && j.expiry.Sub(now) > renewBefore {
		return j.token, nil
	}
	exp := now.Add(lifetime)
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-backdate).Unix(), "exp": exp.Unix(), "iss": j.issuer})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := j.signer.SignSHA256(ctx, digest[:])
	if err != nil {
		return "", err
	}
	j.token, j.expiry = unsigned+"."+enc(sig), exp
	return j.token, nil
}

// App is a GitHub App installation: what to ask for tokens with.
type App struct {
	InstallationID int64
	JWT            *JWT
}

// Valid reports whether a is an App at all, rather than a PAT install's zero.
func (a App) Valid() bool { return a.JWT != nil && a.InstallationID > 0 }
