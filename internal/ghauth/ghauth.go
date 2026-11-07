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
	// refusalWait is how young a refused JWT is kept all the same. A refusal
	// that lasts, from a clock still wrong or a key since rotated, then costs
	// a signature a minute instead of one per request.
	refusalWait = time.Minute
)

// signTimeout bounds one signature: a KMS that does not answer must not hold
// every tenant's token requests.
const signTimeout = 10 * time.Second

// JWT is a GitHub App's JWT, signed once and reused until it nears expiry: with
// KMS, each signature is a network call and costs one. Callers asking while a
// signature is in flight wait for it, each for as long as its own context.
type JWT struct {
	issuer string
	signer Signer
	now    func() time.Time

	mu       sync.Mutex
	token    string
	signedAt time.Time
	expiry   time.Time
	inflight *signing
}

type signing struct {
	done    chan struct{}
	waiters int // callers waiting on it, for tests
	token   string
	err     error
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

// wall is the time on the clock GitHub checks exp and iat against. A
// monotonic reading would ignore a clock step, and stand still while the
// machine is suspended.
func (j *JWT) wall() time.Time { return j.now().Round(0) }

// Token returns a JWT valid for at least renewBefore.
func (j *JWT) Token(ctx context.Context) (string, error) {
	j.mu.Lock()
	if j.token != "" && j.expiry.Sub(j.wall()) > renewBefore {
		defer j.mu.Unlock()
		return j.token, nil
	}
	c := j.inflight
	if c == nil {
		c = &signing{done: make(chan struct{})}
		j.inflight = c
		go j.sign(c)
	}
	c.waiters++
	j.mu.Unlock()
	select {
	case <-c.done:
		return c.token, c.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// sign makes a new JWT for whoever waits on c. It runs on its own deadline, not
// a caller's, as every caller shares its result.
func (j *JWT) sign(c *signing) {
	ctx, cancel := context.WithTimeout(context.Background(), signTimeout)
	defer cancel()
	now := j.wall()
	exp := now.Add(lifetime)
	token, err := j.signAt(ctx, now, exp)
	j.mu.Lock()
	defer j.mu.Unlock()
	if err == nil {
		j.token, j.signedAt, j.expiry = token, now, exp
	}
	c.token, c.err = token, err
	j.inflight = nil
	close(c.done)
}

func (j *JWT) signAt(ctx context.Context, now, exp time.Time) (string, error) {
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
	return unsigned + "." + enc(sig), nil
}

// Refused drops token if it is the cached JWT and at least refusalWait old, so
// a later caller signs anew. Call it when GitHub answers 401 to it: the clock
// may have been wrong when it was signed.
func (j *JWT) Refused(token string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.token == token && j.wall().Sub(j.signedAt) >= refusalWait {
		j.token = ""
	}
}

// App is a GitHub App installation: what to ask for tokens with.
type App struct {
	InstallationID int64
	JWT            *JWT
}

// Valid reports whether a is an App at all, rather than a PAT install's zero.
func (a App) Valid() bool { return a.JWT != nil && a.InstallationID > 0 }
