package tenantstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func TestFlagsSpec(t *testing.T) {
	for in, want := range map[string]string{
		"ssm:/fr/control/tenant-state":    "ssm:/fr/control/tenant-state-flags",
		"/var/lib/fr/tenant-state.json":   "/var/lib/fr/tenant-state-flags.json",
		"/var/lib/fr/states":              "/var/lib/fr/states-flags",
		"/var/lib/fr.d/tenant-state.json": "/var/lib/fr.d/tenant-state-flags.json",
	} {
		if got := FlagsSpec(in); got != want {
			t.Errorf("FlagsSpec(%q) = %q, want %q", in, got, want)
		}
	}
}

// testFlagStore runs the same checks on either store: flags and the cursor
// round-trip, a clear removes one flag only, and clearing what is not there
// is no error.
func testFlagStore(t *testing.T, s FlagStore) {
	t.Helper()
	ctx := context.Background()
	fl, cur, err := s.LoadFlags(ctx)
	if err != nil || len(fl) != 0 || !cur.Since.IsZero() {
		t.Fatalf("an empty store: %v, %+v, %v", fl, cur, err)
	}
	at := time.Date(2026, 11, 11, 3, 7, 23, 0, time.UTC)
	f := Flag{Type: "CryptoCurrency:EC2/BitcoinTool.B!DNS", Tier: "small", Instance: "i-0aaa", Finding: "f1", Severity: 8, Count: 1, First: at, Last: at}
	if err := s.PutFlag(ctx, "acme", f); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFlag(ctx, Install, f); err != nil {
		t.Fatal(err)
	}
	c := Cursor{Since: at.Add(-time.Hour), Seen: map[string]time.Time{"f1": at}}
	if err := s.PutCursor(ctx, c); err != nil {
		t.Fatal(err)
	}
	fl, cur, err = s.LoadFlags(ctx)
	if err != nil || len(fl) != 2 || fl["acme"] != f || !cur.Since.Equal(c.Since) || !cur.Seen["f1"].Equal(at) {
		t.Fatalf("reloaded %v, %+v, %v", fl, cur, err)
	}
	if _, ok := fl[cursorName]; ok {
		t.Fatal("the cursor reads as a flag")
	}
	if err := s.DeleteFlag(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFlag(ctx, "acme"); err != nil {
		t.Fatalf("clearing a flag already cleared: %v", err)
	}
	fl, cur, _ = s.LoadFlags(ctx)
	if len(fl) != 1 || fl.IDs()[0] != Install || cur.Since.IsZero() {
		t.Fatalf("after a clear: %v, %+v", fl, cur)
	}
}

func TestFileFlagStore(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFlags(filepath.Join(dir, "tenant-state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.String() != filepath.Join(dir, "tenant-state-flags.json") {
		t.Fatalf("String = %q", s)
	}
	testFlagStore(t, s)
	if _, err := os.Stat(filepath.Join(dir, "tenant-state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state file was written: %v", err)
	}
	if err := os.WriteFile(s.String(), []byte(" \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadFlags(context.Background()); err == nil {
		t.Fatal("an empty file read as no flags")
	}
}

func TestSSMFlagStore(t *testing.T) {
	api := &fakeSSM{params: map[string]types.Parameter{
		"/fr/control/tenant-state/x": {Name: aws.String("/fr/control/tenant-state/x"), Value: aws.String(`{"reason":"r"}`)},
	}}
	s, err := OpenFlags("ssm:/fr/control/tenant-state", func() (SSMAPI, error) { return api, nil })
	if err != nil {
		t.Fatal(err)
	}
	if s.String() != "ssm:/fr/control/tenant-state-flags" {
		t.Fatalf("String = %q", s)
	}
	testFlagStore(t, s)
	if _, ok := api.params["/fr/control/tenant-state/x"]; !ok {
		t.Fatal("a suspension was touched")
	}
	// The flags never land under the states' path, where they would suspend.
	for n := range api.params {
		if n != "/fr/control/tenant-state/x" && strings.HasPrefix(n, "/fr/control/tenant-state/") {
			t.Fatalf("%s is under the states' path", n)
		}
	}
	// A flag written by hand that does not parse still flags.
	api.params["/fr/control/tenant-state-flags/b"] = types.Parameter{Name: aws.String("/fr/control/tenant-state-flags/b"), Value: aws.String("look at b")}
	fl, _, err := s.LoadFlags(context.Background())
	if f, ok := fl["b"]; err != nil || !ok || !strings.Contains(f.Type, "does not parse") {
		t.Fatalf("an unparsable flag: %+v, %v, %v", f, ok, err)
	}
	api.err = errors.New("ThrottlingException")
	if _, _, err := s.LoadFlags(context.Background()); err == nil {
		t.Fatal("a failing read: no error")
	}
}
