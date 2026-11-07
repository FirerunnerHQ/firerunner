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
	"runtime"
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

// A JWT GitHub refused is signed again, at most once a minute, and only that
// one is dropped.
func TestRefusedJWTIsSignedAgain(t *testing.T) {
	s := &countingSigner{Signer: KeySigner{Key: key(t)}}
	now := time.Unix(1_000_000, 0)
	j := NewJWT("Iv1.app", s).WithClock(func() time.Time { return now })
	first, _ := j.Token(context.Background())
	now = now.Add(time.Minute)
	j.Refused("some other token")
	if again, _ := j.Token(context.Background()); again != first {
		t.Fatal("another token's refusal dropped this one")
	}
	j.Refused(first)
	second, err := j.Token(context.Background())
	if err != nil || s.n != 2 {
		t.Fatalf("not signed again after a refusal: %d signatures, %v", s.n, err)
	}
	// A refusal that lasts costs a signature a minute, not one per request.
	now = now.Add(59 * time.Second)
	j.Refused(second)
	if again, _ := j.Token(context.Background()); again != second || s.n != 2 {
		t.Fatalf("a JWT refused within a minute of signing was signed again: %d signatures", s.n)
	}
}

// A clock that moves past the JWT's exp gets a new one.
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

// A clock set back, as NTP does to one that ran fast at boot, makes the JWT it
// signed wrong at once: it is signed again, refused or not.
func TestJWTSignedBeforeAClockStepBackIsReplaced(t *testing.T) {
	s := &countingSigner{Signer: KeySigner{Key: key(t)}}
	now := time.Unix(1_000_000, 0).Add(3 * time.Hour)
	j := NewJWT("Iv1.app", s).WithClock(func() time.Time { return now })
	fast, _ := j.Token(context.Background())
	now = now.Add(-3 * time.Hour)
	j.Refused(fast)
	if again, err := j.Token(context.Background()); err != nil || again == fast || s.n != 2 {
		t.Fatalf("kept a JWT from before the clock was set back: %d signatures, %v", s.n, err)
	}

	// Unrefused, too.
	fast, _ = j.Token(context.Background())
	now = now.Add(-time.Hour)
	if again, _ := j.Token(context.Background()); again == fast || s.n != 3 {
		t.Fatalf("served a JWT signed in the future: %d signatures", s.n)
	}
}

// The cache keeps wall-clock times only. With Go's monotonic reading kept, its
// comparisons would ignore a clock step and stand still through a suspend.
func TestJWTKeepsNoMonotonicReading(t *testing.T) {
	j := NewJWT("Iv1.app", KeySigner{Key: key(t)})
	if _, err := j.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j.expiry != j.expiry.Round(0) || j.signedAt != j.signedAt.Round(0) {
		t.Fatalf("expiry %v, signed %v: a monotonic reading", j.expiry, j.signedAt)
	}
}

// A signature that fails while the cached JWT still has its last minute leaves
// that JWT in use, rather than failing every request.
func TestFailedSignatureKeepsAStillValidJWT(t *testing.T) {
	k := key(t)
	fail := false
	s := signerFunc(func(ctx context.Context, d []byte) ([]byte, error) {
		if fail {
			return nil, errors.New("ThrottlingException")
		}
		return KeySigner{Key: k}.SignSHA256(ctx, d)
	})
	now := time.Unix(1_000_000, 0)
	j := NewJWT("Iv1.app", s).WithClock(func() time.Time { return now })
	first, _ := j.Token(context.Background())
	fail = true
	now = now.Add(8*time.Minute + 30*time.Second)
	if got, err := j.Token(context.Background()); err != nil || got != first {
		t.Fatalf("got %v with half a minute left", err)
	}
	now = now.Add(time.Minute)
	if _, err := j.Token(context.Background()); err == nil {
		t.Fatal("an expired JWT was handed out")
	}
}

type signerFunc func(context.Context, []byte) ([]byte, error)

func (f signerFunc) SignSHA256(ctx context.Context, d []byte) ([]byte, error) { return f(ctx, d) }

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
	// All three wait on the one signature before it is let through.
	for {
		j.mu.Lock()
		n := j.inflight.waiters
		j.mu.Unlock()
		if n == 3 {
			break
		}
		runtime.Gosched()
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
