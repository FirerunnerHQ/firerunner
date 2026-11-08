package gitproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/firerunnerhq/firerunner/internal/gitcache"
)

// entryVersion keeps these entries apart from actions/cache's. An actions/cache
// client that chooses the same key and version reaches no more than this
// proxy does: its own repository and refs, as cache-server decides, which
// also saves an entry only for the job's own repository.
const entryVersion = gitcache.EntryVersion

const twirpBase = "/twirp/github.actions.results.api.v1.CacheService/"

// CacheServer is a Cache that speaks the actions/cache protocol to cache-server,
// through the instance's loopback proxy, which identifies the job by this VM's
// address. A git entry is then an ordinary entry, and scope checks, eviction,
// size limits and storage all apply to it unchanged.
type CacheServer struct {
	URL    string // such as http://127.0.0.1
	Client *http.Client
}

func (c *CacheServer) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c *CacheServer) rpc(ctx context.Context, method string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+twirpBase+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", method, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Open implements Cache.
func (c *CacheServer) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	var got struct {
		OK  bool   `json:"ok"`
		URL string `json:"signed_download_url"`
	}
	in := map[string]any{"key": key, "version": entryVersion, "restore_keys": []string{}}
	if err := c.rpc(ctx, "GetCacheEntryDownloadURL", in, &got); err != nil {
		return nil, err
	}
	if !got.OK || got.URL == "" {
		return nil, ErrMiss
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, got.URL, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("download: status %d", res.StatusCode)
	}
	return res.Body, nil
}

// Save implements Cache. An entry that exists already, or that another job is
// creating, is not an error.
func (c *CacheServer) Save(ctx context.Context, key, path string, size int64) error {
	var created struct {
		OK  bool   `json:"ok"`
		URL string `json:"signed_upload_url"`
	}
	in := map[string]any{"key": key, "version": entryVersion}
	if err := c.rpc(ctx, "CreateCacheEntry", in, &created); err != nil {
		return err
	}
	if !created.OK {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, created.URL, f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	res, err := c.client().Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return fmt.Errorf("upload: status %d", res.StatusCode)
	}
	var finished struct {
		OK bool `json:"ok"`
	}
	in["size_bytes"] = strconv.FormatInt(size, 10)
	if err := c.rpc(ctx, "FinalizeCacheEntryUpload", in, &finished); err != nil {
		return err
	}
	if !finished.OK {
		return errors.New("finalize refused")
	}
	return nil
}
