package awscreds

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func creds(key string, exp time.Time) aws.Credentials {
	return aws.Credentials{AccessKeyID: key, SecretAccessKey: "secret-" + key, SessionToken: "token-" + key, CanExpire: true, Expires: exp}
}

func TestWriteShapeAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := Write(path, creds("AKIA1", exp)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, want 0640", fi.Mode().Perm())
	}
	var got map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"Version": float64(1), "AccessKeyId": "AKIA1", "SecretAccessKey": "secret-AKIA1", "SessionToken": "token-AKIA1", "Expiration": "2030-01-02T03:04:05Z"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (file %s)", k, got[k], v, data)
		}
	}
}

// A file Write replaces is never seen half-written, and no temporary file is
// left beside it.
func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	exp := time.Now().Add(time.Hour)
	if err := Write(path, creds("AKIAOLD", exp)); err != nil {
		t.Fatal(err)
	}
	// A reader that opened the old file keeps reading the old file.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Write(path, creds("AKIANEW", exp)); err != nil {
		t.Fatal(err)
	}
	old, _ := io.ReadAll(f)
	if !strings.Contains(string(old), "AKIAOLD") {
		t.Fatalf("open reader saw %s: the file was rewritten in place, not replaced", old)
	}
	c, err := FileProvider{Path: path}.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "AKIANEW" {
		t.Fatalf("after replace: %+v, %v", c, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

// Even with a permissive umask, the file is not world-readable.
func TestWriteModeIgnoresExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, creds("AKIA1", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, want 0640", fi.Mode().Perm())
	}
}

func TestWriteRefusesEmpty(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "c"), aws.Credentials{}); err == nil {
		t.Fatal("wrote credentials without a key")
	}
}

func TestProviderReportsExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	exp := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	if err := Write(path, creds("AKIA1", exp)); err != nil {
		t.Fatal(err)
	}
	c, err := FileProvider{Path: path}.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !c.CanExpire || !c.Expires.Equal(exp) {
		t.Fatalf("CanExpire %v Expires %v, want true %v", c.CanExpire, c.Expires, exp)
	}
	if c.AccessKeyID != "AKIA1" || c.SecretAccessKey != "secret-AKIA1" || c.SessionToken != "token-AKIA1" {
		t.Fatalf("got %+v", c)
	}
}

func TestProviderMissingFile(t *testing.T) {
	_, err := FileProvider{Path: filepath.Join(t.TempDir(), "absent")}.Retrieve(context.Background())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err %v, want not exist", err)
	}
}

func TestProviderRejectsExpiredAndMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	exp := time.Now().Add(time.Hour)
	if err := Write(path, creds("AKIA1", exp)); err != nil {
		t.Fatal(err)
	}
	late := FileProvider{Path: path, Now: func() time.Time { return exp }}
	if _, err := late.Retrieve(context.Background()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired credentials: %v", err)
	}
	for name, body := range map[string]string{
		"not json":   "{",
		"no version": `{"AccessKeyId":"a","SecretAccessKey":"b"}`,
		"no secret":  `{"Version":1,"AccessKeyId":"a"}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (FileProvider{Path: path}).Retrieve(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Through the SDK's cache, a new file is read once the old credentials are
// within ExpiryWindow of expiry, and not before.
func TestFileCacheRereadsNearExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	// Inside the window already: every Retrieve reads the file again.
	if err := Write(path, creds("AKIA1", time.Now().Add(ExpiryWindow-time.Minute))); err != nil {
		t.Fatal(err)
	}
	cache := NewFileCache(path)
	c, err := cache.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "AKIA1" {
		t.Fatalf("first: %+v %v", c, err)
	}
	if err := Write(path, creds("AKIA2", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if c, err = cache.Retrieve(context.Background()); err != nil || c.AccessKeyID != "AKIA2" {
		t.Fatalf("after the file changed: %+v %v, want AKIA2", c, err)
	}
	// Far from expiry: the cache holds them, and a rewrite waits.
	if err := Write(path, creds("AKIA3", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if c, _ = cache.Retrieve(context.Background()); c.AccessKeyID != "AKIA2" {
		t.Fatalf("re-read %s while the cached credentials were fresh", c.AccessKeyID)
	}
}

func TestWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	done := make(chan error, 1)
	go func() { done <- Wait(context.Background(), path, time.Millisecond, quiet) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the file existed: %v", err)
	default:
	}
	if err := Write(path, creds("AKIA1", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after the file was written")
	}
}

func TestWaitCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Wait(ctx, filepath.Join(t.TempDir(), "absent"), time.Millisecond, quiet) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop on cancel")
	}
}

// fakeSTS issues numbered sessions, failing the calls listed in fail, and
// reports each call on calls.
type fakeSTS struct {
	mu       sync.Mutex
	n        int
	fail     map[int]bool
	inputs   []*sts.AssumeRoleInput
	calls    chan int
	lifetime time.Duration
}

func newFakeSTS(fail ...int) *fakeSTS {
	f := &fakeSTS{fail: map[int]bool{}, calls: make(chan int, 100), lifetime: time.Hour}
	for _, n := range fail {
		f.fail[n] = true
	}
	return f
}

func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()
	defer func() { f.calls <- n }()
	if f.fail[n] {
		return nil, errors.New("throttled")
	}
	key := "AKIA" + string(rune('A'+n-1))
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: aws.String(key), SecretAccessKey: aws.String("s"), SessionToken: aws.String("t"),
		Expiration: aws.Time(time.Now().Add(f.lifetime)),
	}}, nil
}

func waitCall(t *testing.T, f *fakeSTS, want int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case n := <-f.calls:
			if n >= want {
				return
			}
		case <-deadline:
			t.Fatalf("AssumeRole call %d never came", want)
		}
	}
}

// eventuallyKey waits until the file holds the session with key want.
func eventuallyKey(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if c, err := (FileProvider{Path: path}).Retrieve(context.Background()); err == nil {
			if got = c.AccessKeyID; got == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("file never held %s (last %q)", want, got)
}

func readKey(t *testing.T, path string) string {
	t.Helper()
	c, err := FileProvider{Path: path}.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c.AccessKeyID
}

func TestRefreshWritesAssumedCredentials(t *testing.T) {
	f := newFakeSTS()
	path := filepath.Join(t.TempDir(), "creds.json")
	r := &Refresher{API: f, RoleARN: "arn:aws:iam::123456789012:role/cache", Path: path, Log: quiet}
	exp, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) < 59*time.Minute {
		t.Fatalf("expiry %v", exp)
	}
	if got := readKey(t, path); got != "AKIAA" {
		t.Fatalf("file holds %s", got)
	}
	in := f.inputs[0]
	if aws.ToString(in.RoleArn) != r.RoleARN || aws.ToString(in.RoleSessionName) != DefaultSessionName || aws.ToInt32(in.DurationSeconds) != 3600 {
		t.Fatalf("AssumeRole input %+v", in)
	}
}

func TestRefreshKeepsFileOnError(t *testing.T) {
	f := newFakeSTS(2)
	path := filepath.Join(t.TempDir(), "creds.json")
	r := &Refresher{API: f, RoleARN: "arn:aws:iam::123456789012:role/cache", Path: path, Log: quiet}
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(context.Background()); err == nil {
		t.Fatal("a failed AssumeRole was not an error")
	}
	if got := readKey(t, path); got != "AKIAA" {
		t.Fatalf("file holds %s after a failed refresh", got)
	}
}

// Run replaces the file every RefreshEvery, retries a failure after a backoff
// shorter than that, and stops when its context does.
func TestRunRefreshesRetriesAndStops(t *testing.T) {
	f := newFakeSTS(2, 3)
	path := filepath.Join(t.TempDir(), "creds.json")
	r := &Refresher{API: f, RoleARN: "arn:aws:iam::123456789012:role/cache", Path: path, Log: quiet,
		RefreshEvery: 20 * time.Millisecond, MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { r.Run(ctx); close(stopped) }()

	eventuallyKey(t, path, "AKIAA")
	eventuallyKey(t, path, "AKIAD") // calls 2 and 3 fail, 4 succeeds
	eventuallyKey(t, path, "AKIAE") // the next, on schedule
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	// No call after Run returned.
	f.mu.Lock()
	n := f.n
	f.mu.Unlock()
	select {
	case m := <-f.calls:
		if m > n {
			t.Fatalf("call %d after Run returned", m)
		}
	default:
	}
}

// After failures, the wait backs off but never past MaxBackoff, and a success
// resets it.
func TestRunBackoffSchedule(t *testing.T) {
	f := newFakeSTS(1, 2, 3, 4)
	refreshed := make(chan struct{}, 10)
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "cache credentials refreshed") {
			refreshed <- struct{}{}
		}
		return len(p), nil
	}), nil))
	r := &Refresher{API: f, RoleARN: "arn:aws:iam::123456789012:role/cache", Path: filepath.Join(t.TempDir(), "c"), Log: log,
		RefreshEvery: time.Hour, MinBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { r.Run(ctx); close(stopped) }()
	// Five calls in well under the hour RefreshEvery: the failures were
	// retried on the backoff, not the schedule.
	waitCall(t, f, 5)
	select {
	case n := <-f.calls:
		t.Fatalf("call %d before RefreshEvery after a success", n)
	default:
	}
	// Cancelled mid-wait (the success is logged just before it), it stops
	// now, not after the hour.
	select {
	case <-refreshed:
	case <-time.After(10 * time.Second):
		t.Fatal("the success was never logged")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop on cancel while waiting")
	}
}
