package cacheserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/firerunnerhq/firerunner/internal/cacheserver/s3store"
)

// TestS3StoreAgainstARealBucket runs the store's paths against real S3: the
// semantics the in-memory store only imitates (a missing index, multipart
// uploads read back by range, a missing blob as fs.ErrNotExist). It is skipped
// unless FR_S3_TEST_BUCKET names a scratch bucket the default AWS chain may
// read, write, list and delete in. FR_S3_TEST_REGION is optional, as for
// cache-server --s3-region; FR_S3_TEST_MIB sets the large blob's size (40). It
// works under a fresh prefix and deletes what it wrote.
//
//	FR_S3_TEST_BUCKET=… go test -run S3StoreAgainst -v ./internal/cacheserver/
func TestS3StoreAgainstARealBucket(t *testing.T) {
	bucket := os.Getenv("FR_S3_TEST_BUCKET")
	if bucket == "" {
		t.Skip("FR_S3_TEST_BUCKET not set")
	}
	region := os.Getenv("FR_S3_TEST_REGION")
	ctx := context.Background()
	prefix := fmt.Sprintf("firerunner-test/%d/", time.Now().UnixNano())
	t.Cleanup(func() { deletePrefix(t, bucket, region, prefix) })

	store, err := s3store.New(ctx, bucket, prefix, region, nil)
	if err != nil {
		t.Fatalf("s3store.New: %v", err)
	}
	if data, err := store.LoadIndex(ctx); err != nil || data != nil {
		t.Fatalf("LoadIndex on an empty prefix = %q, %v; want nil, nil (s3:ListBucket missing?)", data, err)
	}

	// Over the uploader's 32 MiB part size, so the blob is a multipart object.
	mib := 40
	if v := os.Getenv("FR_S3_TEST_MIB"); v != "" {
		if mib, err = strconv.Atoi(v); err != nil || mib < 33 {
			t.Fatalf("FR_S3_TEST_MIB=%q: want a whole number over 32", v)
		}
	}
	payload := make([]byte, mib<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	first, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetBlobStore(store); err != nil {
		t.Fatalf("SetBlobStore: %v", err)
	}
	ts := httptest.NewServer(first)
	t.Cleanup(ts.Close)
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"})
	start := time.Now()
	putBlob(t, create["signed_upload_url"].(string), payload)
	t.Logf("%d MiB upload through the store: %v", mib, time.Since(start).Round(time.Millisecond))
	if fin := twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1", SizeBytes: fmt.Sprint(len(payload))}); fin["ok"] != true {
		t.Fatalf("finalize: %v", fin)
	}
	saveEntry(t, ts.URL, "owner/pinned", "lost", "v1", 1024) // id 2, lost below
	first.StartIndexSync()()                                 // push the index now

	// A fresh server: index from the bucket, every byte from S3 (no local copies).
	fresh, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.SetBlobStore(store); err != nil {
		t.Fatalf("fresh SetBlobStore: %v", err)
	}
	fresh.SetLocalCacheSize(0)
	ts2 := httptest.NewServer(fresh)
	t.Cleanup(ts2.Close)
	url := downloadURL(t, ts2.URL, "k")
	n := int64(len(payload))
	for _, r := range []struct {
		header     string
		start, end int64 // inclusive
		code       int
	}{
		{"", 0, n - 1, http.StatusOK},
		{"bytes=0-1023", 0, 1023, http.StatusPartialContent},
		{fmt.Sprintf("bytes=%d-%d", 32<<20-512, 32<<20+511), 32<<20 - 512, 32<<20 + 511, http.StatusPartialContent}, // across the part boundary
		{"bytes=-4096", n - 4096, n - 1, http.StatusPartialContent},
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if r.header != "" {
			req.Header.Set("Range", r.header)
		}
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %q: %v", r.header, err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != r.code || !bytes.Equal(got, payload[r.start:r.end+1]) {
			t.Fatalf("GET %q = %d, %d bytes, %v; want %d with bytes %d-%d", r.header, resp.StatusCode, len(got), err, r.code, r.start, r.end)
		}
		t.Logf("GET %q from S3: %d bytes in %v", r.header, len(got), time.Since(start).Round(time.Millisecond))
	}

	// Local copies: the restore from S3 fills one, the next reads it.
	fresh.SetLocalCacheSize(2 * n)
	if code, _ := getBody(t, url); code != http.StatusOK {
		t.Fatalf("GET to fill a local copy = %d", code)
	}
	waitUntil(t, "a local copy filled from S3", func() bool {
		fresh.mu.Lock()
		defer fresh.mu.Unlock()
		_, ok := fresh.local[1]
		return ok
	})
	start = time.Now()
	if code, body := getBody(t, url); code != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("GET from the local copy = %d, %d bytes", code, len(body))
	}
	t.Logf("GET from the local copy: %d bytes in %v", n, time.Since(start).Round(time.Millisecond))

	// A blob the bucket lost: not found, and the entry goes.
	if err := store.Delete(ctx, 2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, 2, 0, 16); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get of a deleted blob = %v, want fs.ErrNotExist", err)
	}
	if code, _ := getBody(t, downloadURL(t, ts2.URL, "lost")); code != http.StatusNotFound {
		t.Fatalf("GET of a blob the bucket lost = %d, want 404", code)
	}
	if get := twirp(t, ts2.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("owner/pinned"), Key: "lost", Version: "v1"}); get["ok"] == true {
		t.Fatalf("lost entry still found: %v", get)
	}
}

// deletePrefix removes every object under prefix.
func deletePrefix(t *testing.T, bucket, region, prefix string) {
	t.Helper()
	ctx := context.Background()
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithEC2IMDSRegion()}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	client := s3.NewFromConfig(cfg)
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Errorf("cleanup: list %s: %v", prefix, err)
			return
		}
		for _, o := range page.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key}); err != nil {
				t.Errorf("cleanup: delete %s: %v", aws.ToString(o.Key), err)
			}
		}
	}
}
