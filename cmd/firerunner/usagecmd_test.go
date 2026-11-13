package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

type fakeUsage struct {
	dsn     string
	after   jobstore.Cursor
	limit   int
	entries []jobstore.UsageEntry
	err     error
	closed  bool
}

func (f *fakeUsage) Read(_ context.Context, after jobstore.Cursor, limit int) ([]jobstore.UsageEntry, error) {
	f.after, f.limit = after, limit
	return f.entries, f.err
}

func (f *fakeUsage) Close() { f.closed = true }

func runUsage(t *testing.T, f *fakeUsage, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := usageCmd(args, &out, &errOut, func(dsn string) (usageReader, error) {
		f.dsn = dsn
		return f, nil
	})
	return code, out.String(), errOut.String()
}

// It reads after the cursor given, and prints each event as a line with
// the cursor that reads on from it.
func TestUsageCmdPrintsEventsAfterACursor(t *testing.T) {
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://db")
	f := &fakeUsage{entries: []jobstore.UsageEntry{
		{Cursor: "7.1", Event: json.RawMessage(`{"event_id":"job/1"}`)},
		{Cursor: "7.2", Event: json.RawMessage(`{"event_id":"job/2"}`)},
	}}
	code, out, errOut := runUsage(t, f, "--after", "6.9", "--limit=2")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if f.dsn != "postgres://db" || f.after != (jobstore.Cursor{Xact: 6, Seq: 9}) || f.limit != 2 || !f.closed {
		t.Fatalf("read %+v", f)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var last jobstore.UsageEntry
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &last) != nil || last.Cursor != "7.2" || string(last.Event) != `{"event_id":"job/2"}` {
		t.Fatalf("printed %q", out)
	}
}

// The store's URL may come from a file, as the daemon's does; with none,
// it is a usage error, and nothing is opened.
func TestUsageCmdStoreURL(t *testing.T) {
	file := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(file, []byte("postgres://from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FR_JOB_EVENTS_DSN", "")
	t.Setenv("FR_JOB_EVENTS_DSN_FILE", file)
	f := &fakeUsage{}
	if code, _, errOut := runUsage(t, f); code != 0 || f.dsn != "postgres://from-file" || f.limit != 1000 || f.after != (jobstore.Cursor{}) {
		t.Fatalf("exit %d, read %+v: %s", code, f, errOut)
	}
	t.Setenv("FR_JOB_EVENTS_DSN_FILE", "")
	f = &fakeUsage{}
	if code, _, errOut := runUsage(t, f, "--env-file", "none"); code != 2 || f.dsn != "" || !strings.Contains(errOut, "no job store") {
		t.Fatalf("exit %d, opened %q: %s", code, f.dsn, errOut)
	}
}

func TestUsageCmdRefusesBadArguments(t *testing.T) {
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://db")
	for _, args := range [][]string{{"--after", "x"}, {"--limit", "0"}, {"--limit", "10001"}, {"--after"}, {"--since", "1d"}, {"extra"}} {
		f := &fakeUsage{}
		if code, _, _ := runUsage(t, f, args...); code != 2 || f.dsn != "" {
			t.Errorf("%v: exit %d, opened %q; want a usage error before opening", args, code, f.dsn)
		}
	}
	if code, out, _ := runUsage(t, &fakeUsage{}, "-h"); code != 0 || !strings.Contains(out, "event_id") {
		t.Errorf("-h: exit %d, %q", code, out)
	}
}

// A failed read prints nothing on stdout, so a consumer never takes a
// partial page for the end.
func TestUsageCmdFailedRead(t *testing.T) {
	t.Setenv("FR_JOB_EVENTS_DSN", "postgres://db")
	f := &fakeUsage{err: errors.New("job store: timed out"), entries: []jobstore.UsageEntry{{Cursor: "1.1"}}}
	if code, out, errOut := runUsage(t, f); code != 1 || out != "" || !strings.Contains(errOut, "timed out") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
}
