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
		"group":      "runner group",
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
	if _, err := Rewrite([]byte("jobs: [\n"), testMap); err == nil {
		t.Error("an unparseable workflow is not an error")
	}
	for _, m := range []Map{nil, {"ubuntu-latest": ""}, {"ubuntu-latest": "a b"}, {"ubuntu-latest": "x\ny"}, {"ubuntu-latest": "'q'"}, {"": "fr"}} {
		if _, err := Rewrite([]byte("jobs: {}\n"), m); err == nil {
			t.Errorf("map %q accepted", m)
		}
	}
}
