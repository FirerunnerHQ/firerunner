package ghauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// verify checks tok as GitHub would: RS256 over header.claims by pub.
func verify(t *testing.T, tok string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var header, claims map[string]any
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	c, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(c, &claims) != nil || header["alg"] != "RS256" {
		t.Fatalf("header %s, claims %s", h, c)
	}
	return claims
}

type countingSigner struct {
	Signer
	n int
}

func (c *countingSigner) SignSHA256(ctx context.Context, d []byte) ([]byte, error) {
	c.n++
	return c.Signer.SignSHA256(ctx, d)
}

func TestJWTIsSignedAndReused(t *testing.T) {
	k := key(t)
	s := &countingSigner{Signer: KeySigner{Key: k}}
	j := NewJWT("Iv1.app", s)
	now := time.Unix(1_000_000, 0)
	j.now = func() time.Time { return now }

	tok, err := j.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	claims := verify(t, tok, &k.PublicKey)
	if claims["iss"] != "Iv1.app" || claims["iat"] != float64(now.Add(-time.Minute).Unix()) || claims["exp"] != float64(now.Add(9*time.Minute).Unix()) {
		t.Fatalf("claims = %v", claims)
	}
	// GitHub refuses a JWT valid for more than ten minutes.
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat > 600 {
		t.Fatalf("valid for %v s", exp-iat)
	}

	now = now.Add(7*time.Minute + 59*time.Second)
	if again, _ := j.Token(context.Background()); again != tok || s.n != 1 {
		t.Fatalf("not reused with a minute left: %d signatures", s.n)
	}
	now = now.Add(2 * time.Second)
	if again, _ := j.Token(context.Background()); again == tok || s.n != 2 {
		t.Fatalf("reused within a minute of expiry: %d signatures", s.n)
	}
}

func TestJWTFailsWithItsSigner(t *testing.T) {
	j := NewJWT("Iv1.app", failingSigner{})
	if _, err := j.Token(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if _, err := j.Token(context.Background()); err == nil {
		t.Fatal("a failed signature was cached")
	}
}

type failingSigner struct{}

func (failingSigner) SignSHA256(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("denied")
}

// fakeKMS signs as KMS does for RSASSA_PKCS1_V1_5_SHA_256 over a digest.
type fakeKMS struct {
	key *rsa.PrivateKey
	in  *kms.SignInput
	err error
}

func (f *fakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	if in.MessageType != types.MessageTypeDigest || in.SigningAlgorithm != types.SigningAlgorithmSpecRsassaPkcs1V15Sha256 || len(in.Message) != sha256.Size {
		return nil, errors.New("fake KMS: not an RS256 digest")
	}
	sig, err := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, in.Message)
	return &kms.SignOutput{Signature: sig}, err
}

func TestKMSSignerMakesRS256(t *testing.T) {
	k := key(t)
	f := &fakeKMS{key: k}
	tok, err := NewJWT("Iv1.app", KMSSigner{API: f, KeyID: "alias/github-app"}).Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verify(t, tok, &k.PublicKey)
	if *f.in.KeyId != "alias/github-app" {
		t.Fatalf("key = %s", *f.in.KeyId)
	}
	f.err = errors.New("AccessDeniedException")
	if _, err := (KMSSigner{API: f, KeyID: "k"}).SignSHA256(context.Background(), make([]byte, 32)); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v", err)
	}
}

func TestAppValid(t *testing.T) {
	if (App{}).Valid() || (App{InstallationID: 1}).Valid() || (App{JWT: NewJWT("x", nil)}).Valid() {
		t.Fatal("an incomplete App is valid")
	}
	if !(App{InstallationID: 1, JWT: NewJWT("x", nil)}).Valid() {
		t.Fatal("an App is not valid")
	}
}

// A JWT GitHub refused is signed again, and only that one is dropped.
func TestRefusedJWTIsSignedAgain(t *testing.T) {
	s := &countingSigner{Signer: KeySigner{Key: key(t)}}
	j := NewJWT("Iv1.app", s)
	first, _ := j.Token(context.Background())
	j.Refused("some other token")
	if again, _ := j.Token(context.Background()); again != first {
		t.Fatal("another token's refusal dropped this one")
	}
	j.Refused(first)
	if _, err := j.Token(context.Background()); err != nil || s.n != 2 {
		t.Fatalf("not signed again after a refusal: %d signatures, %v", s.n, err)
	}
}

// A clock that moves past the JWT's exp gets a new one. The case this guards in
// production, a wall clock that steps while Go's monotonic reading does not,
// cannot be built in a test: wall() strips the monotonic reading, and only
// review keeps it there.
func TestJWTExpiresByTheClock(t *testing.T) {
	s := &countingSigner{Signer: KeySigner{Key: key(t)}}
	start := time.Now()
	now := start
	j := NewJWT("Iv1.app", s).WithClock(func() time.Time { return now })
	if _, err := j.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Jump the wall clock past expiry while the monotonic reading stays put.
	now = start.Round(0).Add(time.Hour)
	if _, err := j.Token(context.Background()); err != nil || s.n != 2 {
		t.Fatalf("a JWT past its wall-clock expiry was reused: %d signatures", s.n)
	}
}

// blockingSigner holds every signature until released.
type blockingSigner struct {
	release chan struct{}
	calls   chan struct{}
	Signer
}

func (b *blockingSigner) SignSHA256(ctx context.Context, d []byte) ([]byte, error) {
	b.calls <- struct{}{}
	<-b.release
	return b.Signer.SignSHA256(ctx, d)
}

// Callers waiting on a slow signer share one signature, and each can give up on
// its own deadline without failing the others.
func TestJWTWaitersShareOneSignature(t *testing.T) {
	b := &blockingSigner{release: make(chan struct{}), calls: make(chan struct{}, 4), Signer: KeySigner{Key: key(t)}}
	j := NewJWT("Iv1.app", b)
	impatient, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 3)
	go func() { _, err := j.Token(impatient); errs <- err }()
	<-b.calls
	for range 2 {
		go func() { _, err := j.Token(context.Background()); errs <- err }()
	}
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("the impatient caller got %v, want its own cancellation", err)
	}
	close(b.release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a patient caller failed: %v", err)
		}
	}
	if n := len(b.calls); n != 0 {
		t.Fatalf("%d more signatures, want the one shared", n)
	}
}
