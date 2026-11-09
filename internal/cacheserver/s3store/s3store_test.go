package s3store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/firerunnerhq/firerunner/internal/awscreds"
)

// fakeS3 answers every request with an empty index and records the access
// key each was signed with.
func fakeS3(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := ""
		if _, rest, ok := strings.Cut(auth, "Credential="); ok {
			key, _, _ = strings.Cut(rest, "/")
		}
		mu.Lock()
		keys = append(keys, key)
		mu.Unlock()
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	// No ambient credentials or profile may stand in for the provider.
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), keys...)
	}
}

func write(t *testing.T, path, key string) {
	t.Helper()
	err := awscreds.Write(path, aws.Credentials{AccessKeyID: key, SecretAccessKey: "s", SessionToken: "t", CanExpire: true, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
}

// A store given a provider signs with it, not the default chain.
func TestNewSignsWithTheGivenProvider(t *testing.T) {
	_, keys := fakeS3(t)
	path := filepath.Join(t.TempDir(), "creds.json")
	write(t, path, "AKIAFILE1")
	store, err := New(context.Background(), "bucket", "p/", "us-west-2", awscreds.NewFileCache(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := keys(); len(got) != 1 || got[0] != "AKIAFILE1" {
		t.Fatalf("signed with %v, want AKIAFILE1", got)
	}
}

// Without the file there is no fallback to another source: the request
// fails, and the store is not built on someone else's credentials.
func TestNewWithMissingFileFailsRequests(t *testing.T) {
	_, keys := fakeS3(t)
	path := filepath.Join(t.TempDir(), "absent.json")
	store, err := New(context.Background(), "bucket", "p/", "us-west-2", awscreds.NewFileCache(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadIndex(context.Background()); err == nil {
		t.Fatal("a request succeeded without credentials")
	}
	if got := keys(); len(got) != 0 {
		t.Fatalf("sent %d requests without credentials (%v)", len(got), got)
	}
	// Once the file appears, the next request uses it.
	write(t, path, "AKIAFILE2")
	if _, err := store.LoadIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := keys(); len(got) != 1 || got[0] != "AKIAFILE2" {
		t.Fatalf("signed with %v, want AKIAFILE2", got)
	}
}
