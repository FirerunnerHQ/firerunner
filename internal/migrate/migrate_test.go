package migrate

import (
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var testMap = Map{"ubuntu-latest": "fr-small", "ubuntu-24.04": "fr-large", "é": "fr-e"}

func TestRewriteGolden(t *testing.T) {
	src, err := os.ReadFile("testdata/ci.in.yml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Rewrite(src, testMap)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("testdata/ci.out.yml", res.Out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/ci.out.yml")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Out) != string(want) {
		t.Errorf("output differs from testdata/ci.out.yml:\n%s", res.Out)
	}

	wantChanges := []Change{
		{"plain", 10, "ubuntu-latest", "fr-small"},
		{"double", 14, "ubuntu-latest", "fr-small"},
		{"single", 17, "ubuntu-24.04", "fr-large"},
		{"flow", 20, "ubuntu-latest", "fr-small"},
		{"flow", 20, "ubuntu-24.04", "fr-large"},
		{"block", 24, "ubuntu-latest", "fr-small"},
		{"block", 25, "ubuntu-24.04", "fr-large"},
	}
	if !reflect.DeepEqual(res.Changes, wantChanges) {
		t.Errorf("changes:\n got %+v\nwant %+v", res.Changes, wantChanges)
	}
	skipped := map[string]string{}
	for _, s := range res.Skips {
		skipped[s.Job] = s.Reason
	}
	for job, reason := range map[string]string{
		"matrix":     "expression",
		"group":      "group/labels object",
		"selfhosted": "already runs on self-hosted",
		"mac":        `no mapping for label "macos-latest"`,
		"mixed":      `no mapping for label "gpu"`,
		"reusable":   "reusable workflow",
	} {
		if !strings.Contains(skipped[job], reason) {
			t.Errorf("job %s: skip reason %q, want one naming %q", job, skipped[job], reason)
		}
	}
	if len(skipped) != 6 {
		t.Errorf("skips %+v, want exactly 6", res.Skips)
	}
	if _, ok := skipped["already"]; ok {
		t.Error("a job already on a target label is reported as skipped")
	}

	// A second run is a no-op.
	again, err := Rewrite(res.Out, testMap)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Out) != string(res.Out) || len(again.Changes) != 0 {
		t.Errorf("second run changed %+v", again.Changes)
	}
}

func TestRewriteEdges(t *testing.T) {
	for _, tc := range []struct {
		name, in, out string
		skip          string // a skip reason fragment, when the job is left alone
	}{
		{
			name: "no trailing newline",
			in:   "jobs:\n  a:\n    runs-on: ubuntu-latest",
			out:  "jobs:\n  a:\n    runs-on: fr-small",
		},
		{
			name: "CRLF line endings",
			in:   "jobs:\r\n  a:\r\n    runs-on: ubuntu-latest\r\n    steps: []\r\n",
			out:  "jobs:\r\n  a:\r\n    runs-on: fr-small\r\n    steps: []\r\n",
		},
		{
			name: "hash inside a plain label is not a comment",
			in:   "jobs:\n  a:\n    runs-on: ubuntu-latest#x\n",
			skip: `no mapping for label "ubuntu-latest#x"`,
		},
		{
			name: "flow mapping job",
			in:   "jobs:\n  a: {runs-on: ubuntu-latest, steps: []}\n",
			out:  "jobs:\n  a: {runs-on: fr-small, steps: []}\n",
		},
		{
			name: "anchored label",
			in:   "jobs:\n  a:\n    runs-on: &os ubuntu-latest\n",
			skip: "anchor",
		},
		{
			name: "anchored list item",
			in:   "jobs:\n  a:\n    runs-on: [&os ubuntu-latest]\n",
			skip: "other than labels",
		},
		{
			name: "self-hosted with mapped labels",
			in:   "jobs:\n  a:\n    runs-on: [self-hosted, ubuntu-latest]\n",
			skip: "already runs on self-hosted",
		},
		{
			name: "aliased label",
			in:   "x: &os ubuntu-latest\njobs:\n  a:\n    runs-on: *os\n",
			skip: "alias",
		},
		{
			name: "non-ASCII before the label",
			in:   "jobs:\n  a:\n    runs-on: [\"é\", ubuntu-latest] # ü\n",
			out:  "jobs:\n  a:\n    runs-on: [\"fr-e\", fr-small] # ü\n",
		},
		{
			name: "escaped quoted label is refused, not misplaced",
			in:   "jobs:\n  a:\n    runs-on: \"ubuntu\\x2dlatest\"\n",
			skip: "could not place",
		},
		{
			name: "block scalar",
			in:   "jobs:\n  a:\n    runs-on: >-\n      ubuntu-latest\n",
			skip: "block scalar",
		},
		{
			name: "expression inside a list",
			in:   "jobs:\n  a:\n    runs-on: [ubuntu-latest, \"${{ inputs.size }}\"]\n",
			skip: "expression",
		},
		{
			// yaml counts a lone CR, NEL, LS and PS as line breaks: a
			// split on \n alone would edit the line below runs-on.
			name: "lone CR in a comment",
			in:   "jobs:\n  a:  # x\r\n    runs-on: ubuntu-latest\n    foo-bar: ubuntu-latest\n",
			out:  "jobs:\n  a:  # x\r\n    runs-on: fr-small\n    foo-bar: ubuntu-latest\n",
		},
		{
			name: "CR inside a quoted string",
			in:   "jobs:\n  a:\n    name: \"x\ry\"\n    runs-on: ubuntu-latest\n    foo-bar: ubuntu-latest\n",
			out:  "jobs:\n  a:\n    name: \"x\ry\"\n    runs-on: fr-small\n    foo-bar: ubuntu-latest\n",
		},
		{
			name: "NEL, LS and PS",
			in:   "jobs:\n  a:\n    name: 'x\u0085y\u2028z\u2029w'\n    runs-on: ubuntu-latest\n    foo-bar: ubuntu-latest\n",
			out:  "jobs:\n  a:\n    name: 'x\u0085y\u2028z\u2029w'\n    runs-on: fr-small\n    foo-bar: ubuntu-latest\n",
		},
		{
			name: "BOM with a flow mapping on line 1",
			in:   "\ufeff{jobs: {a: {runs-on: ubuntu-latest}}}\n",
			out:  "\ufeff{jobs: {a: {runs-on: fr-small}}}\n",
		},
		{
			name: "labels match without regard to case",
			in:   "jobs:\n  a:\n    runs-on: Ubuntu-Latest\n",
			out:  "jobs:\n  a:\n    runs-on: fr-small\n",
		},
		{
			name: "self-hosted in another case",
			in:   "jobs:\n  a:\n    runs-on: [Self-Hosted, ubuntu-latest]\n",
			skip: "already runs on self-hosted",
		},
		{
			name: "tagged label",
			in:   "jobs:\n  a:\n    runs-on: !!str ubuntu-latest\n",
			skip: "YAML tag",
		},
		{
			name: "labels-only object",
			in:   "jobs:\n  a:\n    runs-on: {labels: [ubuntu-latest]}\n",
			skip: "group/labels object",
		},
		{
			name: "empty runs-on",
			in:   "jobs:\n  a:\n    runs-on:\n",
			skip: "runs-on is empty",
		},
		{
			name: "empty list",
			in:   "jobs:\n  a:\n    runs-on: []\n",
			skip: "runs-on is empty",
		},
		{
			name: "runs-on twice",
			in:   "jobs:\n  a:\n    runs-on: ubuntu-latest\n    runs-on: macos-latest\n",
			skip: "more than once",
		},
		{
			name: "runs-on twice after another duplicate key",
			in:   "jobs:\n  a:\n    steps: []\n    steps: []\n    runs-on: ubuntu-latest\n    runs-on: macos-latest\n",
			skip: "more than once",
		},
		{
			name: "label YAML reads as a number",
			in:   "jobs:\n  a:\n    runs-on: 123\n",
			skip: "not a string",
		},
		{
			name: "empty quoted runs-on",
			in:   "jobs:\n  a:\n    runs-on: \"\"\n",
			skip: "runs-on is empty",
		},
		{
			name: "jobs is an alias",
			in:   "x: &j {a: {runs-on: ubuntu-latest}}\njobs: *j\n",
			skip: "jobs is a YAML alias",
		},
		{
			name: "merge key",
			in:   "d: &d {runs-on: ubuntu-latest}\njobs:\n  a:\n    <<: *d\n",
			skip: "YAML merge",
		},
		{
			name: "aliased job",
			in:   "jobs:\n  a: &j\n    runs-on: ubuntu-latest\n  b: *j\n",
			skip: "anchor",
		},
		{
			name: "no jobs",
			in:   "name: x\non: push\n",
			out:  "name: x\non: push\n",
		},
		{
			name: "empty file",
			in:   "",
			out:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Rewrite([]byte(tc.in), testMap)
			if err != nil {
				t.Fatal(err)
			}
			if tc.out != "" || tc.in == "" {
				if string(res.Out) != tc.out {
					t.Errorf("got %q, want %q", res.Out, tc.out)
				}
				if len(res.Skips) != 0 {
					t.Errorf("unexpected skips %+v", res.Skips)
				}
				return
			}
			// Left alone: the input comes back unchanged, and says why.
			if string(res.Out) != tc.in {
				t.Errorf("a refused job was changed: %q", res.Out)
			}
			if len(res.Skips) == 0 || !strings.Contains(res.Skips[0].Reason, tc.skip) {
				t.Errorf("skips %+v, want one naming %q", res.Skips, tc.skip)
			}
		})
	}
}

func TestRewriteErrors(t *testing.T) {
	for name, in := range map[string]string{
		"unparseable":   "jobs: [\n",
		"two documents": "jobs:\n  a:\n    runs-on: ubuntu-latest\n---\njobs: {}\n",
		"duplicate job": "jobs:\n  a:\n    runs-on: ubuntu-latest\n  a:\n    runs-on: ubuntu-latest\n",
	} {
		if _, err := Rewrite([]byte(in), testMap); err == nil {
			t.Errorf("%s: not an error", name)
		}
	}
	for _, m := range []Map{
		nil, {"ubuntu-latest": ""}, {"ubuntu-latest": "a b"}, {"ubuntu-latest": "x\ny"}, {"ubuntu-latest": "'q'"}, {"": "fr"},
		// YAML would read these as a bool, null or a number, not a label.
		{"ubuntu-latest": "true"}, {"ubuntu-latest": "null"}, {"ubuntu-latest": "123"}, {"ubuntu-latest": "1.5"}, {"ubuntu-latest": "0x10"},
		// A chain would make a second run change what the first wrote.
		{"a": "b", "b": "c"},
		// GitHub would see one label mapped two ways.
		{"Ubuntu-Latest": "x", "ubuntu-latest": "y"},
	} {
		if _, err := Rewrite([]byte("jobs: {}\n"), m); err == nil {
			t.Errorf("map %q accepted", m)
		}
	}
}

// verify is the backstop behind span: an edit that lands on other bytes than
// its node's, or writes other text, is refused.
func TestVerifyRefusesMisplacedEdits(t *testing.T) {
	src := []byte("jobs:\n  a:\n    runs-on: ubuntu-latest\n    foo-bar: ubuntu-latest\n")
	doc, err := parseOne(src)
	if err != nil {
		t.Fatal(err)
	}
	runsOn := value(value(value(doc, "jobs"), "a"), "runs-on")
	right := strings.Index(string(src), "ubuntu-latest")
	wrong := strings.LastIndex(string(src), "ubuntu-latest")
	for name, e := range map[string]edit{
		"other bytes": {wrong, wrong + len("ubuntu-latest"), "fr-small", runsOn},
		"other text":  {right, right + len("ubuntu-latest"), "fr-large", runsOn},
	} {
		out, err := apply(src, []edit{e})
		if err != nil {
			t.Fatal(err)
		}
		if name == "other text" {
			e.text = "fr-small" // planned fr-small, wrote fr-large
		}
		if err := verify(doc, out, []edit{e}); err == nil {
			t.Errorf("%s: verify passed %q", name, out)
		}
	}
	good := edit{right, right + len("ubuntu-latest"), "fr-small", runsOn}
	out, _ := apply(src, []edit{good})
	if err := verify(doc, out, []edit{good}); err != nil {
		t.Errorf("a correct edit refused: %v", err)
	}
	// The planned edit and a stray one: the stray change is refused.
	stray, _ := apply(src, []edit{good, {wrong, wrong + len("ubuntu-latest"), "fr-small", nil}})
	if err := verify(doc, stray, []edit{good}); err == nil {
		t.Errorf("verify passed a stray change %q", stray)
	}
}

// A label YAML reads as a number skips its job; the others still migrate.
func TestRewriteNumericLabelSkipsOnlyItsJob(t *testing.T) {
	res, err := Rewrite([]byte("jobs:\n  a:\n    runs-on: 123\n  b:\n    runs-on: ubuntu-latest\n"), Map{"123": "fr-x", "ubuntu-latest": "fr-small"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Out) != "jobs:\n  a:\n    runs-on: 123\n  b:\n    runs-on: fr-small\n" || len(res.Skips) != 1 {
		t.Errorf("out %q, skips %+v", res.Out, res.Skips)
	}
}

// Only ASCII case folds: the long s (U+017F) is not s.
func TestMapFoldsASCIIOnly(t *testing.T) {
	if _, ok := (Map{"s": "x"}).lookup("\u017f"); ok {
		t.Error("the long s matched s")
	}
	if to, ok := (Map{"ubuntu-latest": "x"}).lookup("UBUNTU-LATEST"); !ok || to != "x" {
		t.Error("ASCII case does not fold")
	}
}
