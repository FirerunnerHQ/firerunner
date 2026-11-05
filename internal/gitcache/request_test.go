package gitcache

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	oidA = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	oidB = "b1b3f9723831141a31a1a7252a213e216ea76e56"
)

// request builds a body the way git does: capabilities, a delimiter, arguments
// and a flush.
func request(caps, args []string) []byte {
	var b strings.Builder
	line := func(s string) { fmt.Fprintf(&b, "%04x%s\n", len(s)+5, s) }
	for _, l := range caps {
		line(l)
	}
	b.WriteString("0001")
	for _, l := range args {
		line(l)
	}
	b.WriteString("0000")
	return []byte(b.String())
}

var fetchCaps = []string{"command=fetch", "agent=git/2.56.0-Linux", "object-format=sha1"}

// From GIT_TRACE_PACKET: both `git fetch --depth=1` and the arguments
// actions/checkout passes send exactly this.
var shallowArgs = []string{"thin-pack", "no-progress", "ofs-delta", "deepen 1", "want " + oidA, "done"}

func mustParse(t *testing.T, caps, args []string) Fetch {
	t.Helper()
	f, err := Parse(request(caps, args))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParse_ShallowFetchFromRealGit(t *testing.T) {
	f := mustParse(t, fetchCaps, shallowArgs)
	if f.Deepen != 1 || !f.ThinPack || !f.OfsDelta || f.IncludeTag || f.Filter != "" ||
		f.ObjectFormat != "sha1" || len(f.Wants) != 1 || f.Wants[0] != oidA {
		t.Fatalf("Fetch = %+v", f)
	}
}

func TestParse_BloblessFetch(t *testing.T) {
	f := mustParse(t, fetchCaps, []string{"thin-pack", "ofs-delta", "filter blob:none", "want " + oidA, "done"})
	if f.Filter != "blob:none" || f.Deepen != 0 {
		t.Fatalf("Fetch = %+v", f)
	}
}

func TestParse_FixedFilters(t *testing.T) {
	for _, filter := range []string{"blob:none", "tree:0", "tree:3", "blob:limit=100", "blob:limit=1m", "blob:limit=2K"} {
		if f := mustParse(t, fetchCaps, []string{"filter " + filter, "want " + oidA, "done"}); f.Filter != filter {
			t.Errorf("Filter = %q, want %q", f.Filter, filter)
		}
	}
}

func TestParse_SHA256(t *testing.T) {
	oid := strings.Repeat("ab", 32)
	f := mustParse(t, []string{"command=fetch", "object-format=sha256"}, []string{"want " + oid, "done"})
	if f.ObjectFormat != "sha256" {
		t.Fatalf("Fetch = %+v", f)
	}
	if _, err := Parse(request([]string{"command=fetch", "object-format=sha256"}, []string{"want " + oidA, "done"})); err == nil {
		t.Fatal("a sha1 want passed as sha256")
	}
}

func TestParse_Refuses(t *testing.T) {
	cases := map[string][]byte{
		// The client has objects, so the answer depends on them.
		"have":     request(fetchCaps, []string{"want " + oidA, "have " + oidB, "done"}),
		"shallow":  request(fetchCaps, []string{"want " + oidA, "shallow " + oidB, "done"}),
		"not done": request(fetchCaps, []string{"want " + oidA}),
		// The answer moves with the ref.
		"want-ref": request(fetchCaps, []string{"want-ref refs/heads/main", "done"}),
		// By date or by exclusion, the answer can change.
		"deepen-since":    request(fetchCaps, []string{"want " + oidA, "deepen-since 1700000000", "done"}),
		"deepen-not":      request(fetchCaps, []string{"want " + oidA, "deepen-not refs/heads/x", "done"}),
		"deepen-relative": request(fetchCaps, []string{"want " + oidA, "deepen 1", "deepen-relative", "done"}),
		"packfile-uris":   request(fetchCaps, []string{"want " + oidA, "packfile-uris https", "done"}),
		"sideband-all":    request(fetchCaps, []string{"want " + oidA, "sideband-all", "done"}),
		"unknown arg":     request(fetchCaps, []string{"want " + oidA, "from-the-future", "done"}),
		"unknown cap":     request(append(fetchCaps[:3:3], "server-option=x"), []string{"want " + oidA, "done"}),
		"ls-refs":         request([]string{"command=ls-refs", "object-format=sha1"}, []string{"peel", "symrefs"}),
		"two commands":    request(append(fetchCaps[:3:3], "command=ls-refs"), []string{"want " + oidA, "done"}),
		"no command":      request([]string{"object-format=sha1"}, []string{"want " + oidA, "done"}),
		"no wants":        request(fetchCaps, []string{"done"}),
		"short want":      request(fetchCaps, []string{"want abc123", "done"}),
		"upper want":      request(fetchCaps, []string{"want " + strings.ToUpper(oidA), "done"}),
		"bad deepen":      request(fetchCaps, []string{"want " + oidA, "deepen 0", "done"}),
		"two deepens":     request(fetchCaps, []string{"want " + oidA, "deepen 1", "deepen 2", "done"}),
		"two filters":     request(fetchCaps, []string{"want " + oidA, "filter blob:none", "filter tree:0", "done"}),
		"bad format":      request([]string{"command=fetch", "object-format=md5"}, []string{"want " + oidA, "done"}),
		"sparse filter":   request(fetchCaps, []string{"want " + oidA, "filter sparse:oid=HEAD:patterns", "done"}),
		"combined filter": request(fetchCaps, []string{"want " + oidA, "filter combine:blob:none+sparse:oid=main:p", "done"}),
		"unknown filter":  request(fetchCaps, []string{"want " + oidA, "filter object:type=blob", "done"}),
		"bad tree filter": request(fetchCaps, []string{"want " + oidA, "filter tree:x", "done"}),
		"bad blob limit":  request(fetchCaps, []string{"want " + oidA, "filter blob:limit=HEAD:x", "done"}),
		"empty filter":    request(fetchCaps, []string{"want " + oidA, "filter", "done"}),
		"empty":           nil,
		"not pkt-line":    []byte("zzzz"),
		"truncated":       []byte("0014command=fe"),
		"no delimiter":    []byte("0012command=fetch\n0000"),
		"two delims":      append([]byte("00010001"), request(fetchCaps, nil)...),
		"after flush":     append(request(fetchCaps, []string{"want " + oidA, "done"}), "0000"...),
		"no flush":        request(fetchCaps, []string{"want " + oidA, "done"})[:len(request(fetchCaps, []string{"want " + oidA, "done"}))-4],
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if f, err := Parse(body); !errors.Is(err, ErrNotCacheable) {
				t.Fatalf("Parse = %+v, %v; want ErrNotCacheable", f, err)
			}
		})
	}
}

func TestKey_IgnoresWhatDoesNotChangeTheObjects(t *testing.T) {
	base := mustParse(t, fetchCaps, shallowArgs).Key("o/r")
	same := map[string]Fetch{
		"agent":    mustParse(t, []string{"command=fetch", "agent=git/2.43.0", "object-format=sha1"}, shallowArgs),
		"session":  mustParse(t, append(fetchCaps[:3:3], "session-id=abc"), shallowArgs),
		"progress": mustParse(t, fetchCaps, []string{"thin-pack", "ofs-delta", "deepen 1", "want " + oidA, "done"}),
		"order":    mustParse(t, fetchCaps, []string{"want " + oidA, "ofs-delta", "deepen 1", "thin-pack", "done"}),
		"repeat":   mustParse(t, fetchCaps, append(shallowArgs[:5:5], "want "+oidA, "done")),
	}
	ab := mustParse(t, fetchCaps, []string{"want " + oidA, "want " + oidB, "done"}).Key("o/r")
	ba := mustParse(t, fetchCaps, []string{"want " + oidB, "want " + oidA, "done"}).Key("o/r")
	if ab != ba {
		t.Error("the order of wants changed the key")
	}
	for name, f := range same {
		if got := f.Key("o/r"); got != base {
			t.Errorf("%s changed the key", name)
		}
	}
	if !strings.HasPrefix(base, "git/") || len(base) != 68 {
		t.Fatalf("key = %q", base)
	}
}

func TestKey_DependsOnWhatChangesTheObjects(t *testing.T) {
	base := mustParse(t, fetchCaps, shallowArgs)
	wants := func(oids ...string) []string {
		a := []string{"thin-pack", "ofs-delta", "deepen 1"}
		for _, o := range oids {
			a = append(a, "want "+o)
		}
		return append(a, "done")
	}
	different := map[string]string{
		"repository": base.Key("o/other"),
		"owner":      base.Key("other/r"),
		"commit":     mustParse(t, fetchCaps, wants(oidB)).Key("o/r"),
		"more wants": mustParse(t, fetchCaps, wants(oidA, oidB)).Key("o/r"),
		"depth":      mustParse(t, fetchCaps, []string{"thin-pack", "ofs-delta", "deepen 2", "want " + oidA, "done"}).Key("o/r"),
		"filter":     mustParse(t, fetchCaps, append(shallowArgs[:5:5], "filter blob:none", "done")).Key("o/r"),
		"tags":       mustParse(t, fetchCaps, append(shallowArgs[:5:5], "include-tag", "done")).Key("o/r"),
		"no thin":    mustParse(t, fetchCaps, []string{"ofs-delta", "deepen 1", "want " + oidA, "done"}).Key("o/r"),
		"no ofs":     mustParse(t, fetchCaps, []string{"thin-pack", "deepen 1", "want " + oidA, "done"}).Key("o/r"),
	}
	seen := map[string]string{base.Key("o/r"): "base"}
	for name, k := range different {
		if prev, dup := seen[k]; dup {
			t.Errorf("%s has the same key as %s", name, prev)
		}
		seen[k] = name
	}
}

// One field must not be able to run into the next.
func TestKey_FieldsDoNotRunTogether(t *testing.T) {
	cases := map[string][2]string{
		"repository into format": {
			Fetch{ObjectFormat: "sha1"}.Key("o/r"),
			Fetch{ObjectFormat: "ha1"}.Key("o/rs"),
		},
		"one want into the next": {
			Fetch{Wants: []string{"ab", "c"}}.Key("o/r"),
			Fetch{Wants: []string{"a", "bc"}}.Key("o/r"),
		},
	}
	for name, k := range cases {
		if k[0] == k[1] {
			t.Errorf("%s: same key", name)
		}
	}
}

func TestRepository(t *testing.T) {
	good := map[string]string{
		"/PostHog/posthog.git/git-upload-pack": "posthog/posthog",
		"/PostHog/posthog/git-upload-pack":     "posthog/posthog",
		"/a-b/c_d.e/git-upload-pack":           "a-b/c_d.e",
	}
	for in, want := range good {
		if got, ok := Repository(in); !ok || got != want {
			t.Errorf("Repository(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{
		"/o/r.git/info/refs", "/o/r.git/git-receive-pack", "/o/git-upload-pack", "/git-upload-pack",
		"/o/r/x/git-upload-pack", "/../r/git-upload-pack", "/o/../git-upload-pack", "/o%2fr/git-upload-pack", "",
	} {
		if got, ok := Repository(in); ok {
			t.Errorf("Repository(%q) = %q", in, got)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(request(fetchCaps, shallowArgs))
	f.Add(request([]string{"command=ls-refs"}, []string{"peel"}))
	f.Add([]byte("0000"))
	f.Add([]byte("0001"))
	f.Add([]byte("0005a0000"))
	f.Fuzz(func(t *testing.T, body []byte) {
		g, err := Parse(body)
		if err != nil {
			if !errors.Is(err, ErrNotCacheable) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		if len(g.Wants) == 0 {
			t.Fatal("a cacheable fetch with no wants")
		}
		_ = g.Key("o/r")
	})
}
