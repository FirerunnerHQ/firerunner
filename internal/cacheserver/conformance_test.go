package cacheserver

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// spec is the Twirp v7 specification's error codes and their HTTP statuses
// (https://twitchtv.github.io/twirp/docs/spec_v7.html#error-codes), written
// out here rather than read from the table under test.
var spec = map[string]int{
	"canceled": 408, "unknown": 500, "invalid_argument": 400, "malformed": 400,
	"deadline_exceeded": 408, "not_found": 404, "bad_route": 404, "already_exists": 409,
	"permission_denied": 403, "unauthenticated": 401, "resource_exhausted": 429,
	"failed_precondition": 412, "aborted": 409, "out_of_range": 400, "unimplemented": 501,
	"internal": 500, "unavailable": 503, "dataloss": 500,
}

// Every Twirp error code is answered at the HTTP status the Twirp
// specification gives it, and an unknown code falls back to internal.
func TestTwirpErrorStatusFollowsTheSpec(t *testing.T) {
	if len(twirpStatus) != len(spec) {
		t.Errorf("%d codes, the spec has %d", len(twirpStatus), len(spec))
	}
	for code, status := range spec {
		w := newRecorder()
		twirpError(w, code, "m")
		var body map[string]string
		if err := json.Unmarshal(w.body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q: %v", code, w.body.String(), err)
		}
		if w.status != status || body["code"] != code || body["msg"] != "m" {
			t.Errorf("%s: %d %v, want %d", code, w.status, body, status)
		}
	}
	w := newRecorder()
	twirpError(w, "no_such_code", "m")
	if w.status != http.StatusInternalServerError || !strings.Contains(w.body.String(), `"internal"`) {
		t.Errorf("unknown code: %d %s, want 500 internal", w.status, w.body.String())
	}
}

// A request the CacheService does not serve is a Twirp bad_route (404, JSON),
// as a Twirp server answers: an unknown method, a GET, or a body not sent as
// JSON.
func TestCacheServiceBadRoutes(t *testing.T) {
	_, ts, _ := budgetServer(t, 0)
	for _, tc := range []struct {
		name, method, path, contentType string
	}{
		{"unknown method", http.MethodPost, twirpBase + "DeleteCacheEntry", "application/json"},
		{"GET", http.MethodGet, twirpBase + "CreateCacheEntry", ""},
		{"protobuf", http.MethodPost, twirpBase + "CreateCacheEntry", "application/protobuf"},
		{"no content type", http.MethodPost, twirpBase + "GetCacheEntryDownloadURL", ""},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(`{"key":"k","version":"v"}`))
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), `"code":"bad_route"`) ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %s %q, want 404 bad_route JSON", tc.name, resp.StatusCode, resp.Header.Get("Content-Type"), raw)
		}
	}
	// A charset parameter is still JSON.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+twirpBase+"GetCacheEntryDownloadURL", strings.NewReader(`{"key":"k","version":"v"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("JSON with a charset: %d, want 200", resp.StatusCode)
	}
}

// Blob endpoints fail as Azure Storage does: the code in x-ms-error-code, and
// an XML Error body carrying the same code.
func TestBlobErrorsAreAzureShaped(t *testing.T) {
	s, ts, _ := budgetServer(t, 0)
	s.SetMaxEntrySize(10)
	url := reserve(t, ts.URL, "k")
	for _, tc := range []struct {
		name, url string
		body      []byte
		status    int
		code      string
	}{
		{"wrong signature", strings.Replace(url, "sig=", "sig=x", 1), []byte("x"), http.StatusForbidden, "AuthenticationFailed"},
		{"over the entry cap", url, bytes.Repeat([]byte("x"), 11), http.StatusRequestEntityTooLarge, "RequestBodyTooLarge"},
		{"unknown comp", url + "&comp=page", []byte("x"), http.StatusBadRequest, "InvalidQueryParameterValue"},
		{"block without an id", url + "&comp=block", []byte("x"), http.StatusBadRequest, "MissingRequiredQueryParameter"},
		{"unknown blob", ts.URL + "/upload/999?sig=x", []byte("x"), http.StatusNotFound, "BlobNotFound"},
		{"bad block list", url + "&comp=blocklist", []byte("<not xml"), http.StatusBadRequest, "InvalidXmlDocument"},
	} {
		req, _ := http.NewRequest(http.MethodPut, tc.url, bytes.NewReader(tc.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if err := xml.Unmarshal(raw, &e); err != nil {
			t.Errorf("%s: body %q is not an Azure error: %v", tc.name, raw, err)
			continue
		}
		if resp.StatusCode != tc.status || resp.Header.Get("x-ms-error-code") != tc.code || e.Code != tc.code || e.Message == "" {
			t.Errorf("%s: %d %q %+v, want %d %s", tc.name, resp.StatusCode, resp.Header.Get("x-ms-error-code"), e, tc.status, tc.code)
		}
	}
}

// azureErr does one blob request and returns its status, its x-ms-error-code
// and the code in its XML body ("" for an empty body, as HEAD has).
func azureErr(t *testing.T, method, url string, body io.Reader, header map[string]string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var e struct {
		Code string `xml:"Code"`
	}
	if len(raw) > 0 {
		if err := xml.Unmarshal(raw, &e); err != nil {
			t.Errorf("%s %s: body %q is not an Azure error: %v", method, url, raw, err)
		}
	}
	return resp.StatusCode, resp.Header.Get("x-ms-error-code"), e.Code
}

// Downloads, a finalized entry, a block list naming a missing block and a
// method an endpoint does not serve fail Azure-shaped too; HEAD carries the
// code in its header only.
func TestMoreBlobErrorsAreAzureShaped(t *testing.T) {
	_, ts, _ := budgetServer(t, 0)
	saveEntry(t, ts.URL, "owner/pinned", "done", "v1", 8)
	get := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Key: "done", Version: "v1"})
	down := get["signed_download_url"].(string)
	up := strings.Replace(down, "/download/", "/upload/", 1)
	fresh := reserve(t, ts.URL, "fresh")
	for _, tc := range []struct {
		name, method, url string
		body              string
		header            map[string]string
		status            int
		code              string
		bodyCode          bool
	}{
		{"download, wrong signature", http.MethodGet, strings.Replace(down, "sig=", "sig=x", 1), "", nil, 403, "AuthenticationFailed", true},
		{"HEAD, wrong signature", http.MethodHead, strings.Replace(down, "sig=", "sig=x", 1), "", nil, 403, "AuthenticationFailed", false},
		{"download, range past the end", http.MethodGet, down, "", map[string]string{"Range": "bytes=100-200"}, 416, "InvalidRange", true},
		{"upload to a finalized entry", http.MethodPut, up, "x", nil, 409, "BlobImmutableDueToPolicy", true},
		{"block list naming a missing block", http.MethodPut, fresh + "&comp=blocklist", "<BlockList><Latest>nope</Latest></BlockList>", nil, 400, "InvalidBlockList", true},
		{"POST to a blob", http.MethodPost, fresh, "x", nil, 405, "UnsupportedHttpVerb", true},
		{"DELETE a blob", http.MethodDelete, down, "", nil, 405, "UnsupportedHttpVerb", true},
	} {
		status, header, body := azureErr(t, tc.method, tc.url, strings.NewReader(tc.body), tc.header)
		wantBody := ""
		if tc.bodyCode {
			wantBody = tc.code
		}
		if status != tc.status || header != tc.code || body != wantBody {
			t.Errorf("%s: %d %q %q, want %d %q %q", tc.name, status, header, body, tc.status, tc.code, wantBody)
		}
	}
}

// A store outage and a full cache are 507s with firerunner's own codes, which
// the Azure SDK does not retry; a store-backed download refuses a bad range
// as the local path does.
func TestStoreBlobErrorsAreAzureShaped(t *testing.T) {
	store := newMemStore()
	_, ts := newStoreServer(t, store)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 8)
	url := downloadURL(t, ts.URL, "k")
	if status, header, _ := azureErr(t, http.MethodGet, url, nil, map[string]string{"Range": "bytes=50-"}); status != 416 || header != "InvalidRange" {
		t.Errorf("store download past the end: %d %q, want 416 InvalidRange", status, header)
	}
	store.failGet = true
	if status, header, body := azureErr(t, http.MethodGet, url, nil, nil); status != 507 || header != azStoreDown || body != azStoreDown {
		t.Errorf("store down: %d %q %q, want 507 %s", status, header, body, azStoreDown)
	}

	_, full, _ := budgetServer(t, 4)
	a := reserve(t, full.URL, "a")
	putBlob(t, a, []byte("xxxx"))
	b := reserve(t, full.URL, "b")
	if status, header, body := azureErr(t, http.MethodPut, b, strings.NewReader("yyyy"), nil); status != 507 || header != azCacheFull || body != azCacheFull {
		t.Errorf("cache full: %d %q %q, want 507 %s", status, header, body, azCacheFull)
	}
}

// An internal failure is logged in full and answered without the paths that
// name the host's directories.
func TestInternalErrorsNameNoHostPath(t *testing.T) {
	s, ts, dir := budgetServer(t, 0)
	url := reserve(t, ts.URL, "k")
	// Staged blocks go under tmp/<id>; a file in its place makes that fail.
	if err := os.WriteFile(s.tmpDir(1), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, url+"&comp=block&blockid=b0", strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || resp.Header.Get("x-ms-error-code") != azInternal {
		t.Fatalf("%d %q, want 500 InternalError", resp.StatusCode, resp.Header.Get("x-ms-error-code"))
	}
	if strings.Contains(string(raw), dir) || strings.Contains(string(raw), "/tmp") {
		t.Fatalf("the error names a host path: %s", raw)
	}
}

// recorder is a minimal ResponseWriter for the error helpers.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder                    { return &recorder{header: http.Header{}} }
func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
