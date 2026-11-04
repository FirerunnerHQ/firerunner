package migrate

import (
	"context"
	"strings"
	"testing"
)

const ciYML = `name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest # fast
    steps:
      - run: make test
  mac:
    runs-on: macos-latest
    steps:
      - run: make test
`

var repoMap = Map{"ubuntu-latest": "fr-small"}

func repoFiles() map[string]string {
	return map[string]string{
		"README.md":                            "hi\n",
		".github/workflows/ci.yml":             ciYML,
		".github/workflows/lint.yaml":          "jobs:\n  lint:\n    runs-on: [ubuntu-latest]\n",
		".github/workflows/notes.md":           "runs-on: ubuntu-latest\n",
		".github/workflows/link.yml":           "LINK:ci.yml",
		".github/workflows/sub/inner.yml":      "jobs:\n  x:\n    runs-on: ubuntu-latest\n",
		".github/workflows/broken.yml":         "jobs: [\n",
		".github/workflows/nothing-to-map.yml": "jobs:\n  x:\n    runs-on: windows-latest\n",
	}
}

func TestMigrateOpensPR(t *testing.T) {
	f, gh := newFakeGitHub(t, repoFiles())
	ctx := context.Background()
	rep, err := gh.Migrate(ctx, Options{Repo: "acme/app", Map: repoMap})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PR != "created" || rep.PRURL == "" || rep.Branch != DefaultBranch || rep.Changed() != 2 {
		t.Fatalf("report %+v", rep)
	}
	var paths []string
	for _, fr := range rep.Files {
		paths = append(paths, fr.Path)
	}
	// Only regular .yml/.yaml files directly in .github/workflows.
	if got := strings.Join(paths, " "); got != ".github/workflows/broken.yml .github/workflows/ci.yml .github/workflows/lint.yaml .github/workflows/nothing-to-map.yml" {
		t.Errorf("files %s", got)
	}
	if rep.Files[0].Error == "" {
		t.Error("an unparseable workflow is not reported")
	}

	want := strings.Replace(ciYML, "runs-on: ubuntu-latest # fast", "runs-on: fr-small # fast", 1)
	if got := f.file(DefaultBranch, ".github/workflows/ci.yml"); got != want {
		t.Errorf("ci.yml on the branch:\n%s", got)
	}
	if got := f.file(DefaultBranch, ".github/workflows/lint.yaml"); got != "jobs:\n  lint:\n    runs-on: [fr-small]\n" {
		t.Errorf("lint.yaml on the branch: %q", got)
	}
	for _, p := range []string{".github/workflows/sub/inner.yml", ".github/workflows/notes.md", "README.md"} {
		if f.file(DefaultBranch, p) != f.file("main", p) {
			t.Errorf("%s changed", p)
		}
	}
	if f.file("main", ".github/workflows/ci.yml") != ciYML {
		t.Error("the default branch changed")
	}
	head := f.commits[f.refs[DefaultBranch]]
	if len(head.parents) != 1 || head.parents[0] != f.refs["main"] || !strings.Contains(head.message, commitMarker) {
		t.Errorf("branch head %+v", head)
	}
	body := f.pulls[0].Body
	for _, s := range []string{"`ubuntu-latest` | `fr-small`", "job `test`", `no mapping for label "macos-latest"`, "broken.yml", "revert this pull request"} {
		if !strings.Contains(body, s) {
			t.Errorf("PR body lacks %q:\n%s", s, body)
		}
	}

	// A rerun with nothing new changes nothing.
	before := len(f.writes())
	rep, err = gh.Migrate(ctx, Options{Repo: "acme/app", Map: repoMap})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PR != "unchanged" || rep.PRURL != f.pulls[0].HTMLURL {
		t.Errorf("rerun: %+v", rep)
	}
	for _, c := range f.writes()[before:] {
		if strings.HasPrefix(c, "PATCH") || c == "POST /git/commits" || c == "POST /pulls" {
			t.Errorf("rerun wrote %s", c)
		}
	}

	// After the default branch moves, a rerun rebuilds the branch on it and
	// updates the same PR.
	f.commitFiles("main", map[string]string{".github/workflows/new.yml": "jobs:\n  n:\n    runs-on: ubuntu-latest\n"}, "add new")
	rep, err = gh.Migrate(ctx, Options{Repo: "acme/app", Map: repoMap})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PR != "updated" || len(f.pulls) != 1 || rep.Changed() != 3 {
		t.Errorf("after main moved: %+v, %d PRs", rep, len(f.pulls))
	}
	if f.commits[f.refs[DefaultBranch]].parents[0] != f.refs["main"] {
		t.Error("the branch is not rebuilt on the new default branch head")
	}
	if got := f.file(DefaultBranch, ".github/workflows/new.yml"); !strings.Contains(got, "fr-small") {
		t.Errorf("new.yml on the branch: %q", got)
	}
	if !strings.Contains(f.pulls[0].Body, "new.yml") {
		t.Error("the PR body is not updated")
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	f, gh := newFakeGitHub(t, repoFiles())
	rep, err := gh.Migrate(context.Background(), Options{Repo: "acme/app", Map: repoMap, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("a dry run wrote %v", w)
	}
	if rep.PR != "none" || rep.Branch != "" || rep.Changed() != 2 {
		t.Errorf("report %+v", rep)
	}
	var diff string
	for _, fr := range rep.Files {
		diff += fr.Diff
	}
	if !strings.Contains(diff, "-    runs-on: ubuntu-latest # fast\n+    runs-on: fr-small # fast\n") {
		t.Errorf("diff:\n%s", diff)
	}
}

func TestMigrateNothingToChange(t *testing.T) {
	f, gh := newFakeGitHub(t, map[string]string{".github/workflows/w.yml": "jobs:\n  x:\n    runs-on: windows-latest\n"})
	rep, err := gh.Migrate(context.Background(), Options{Repo: "acme/app", Map: repoMap})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PR != "none" || len(f.writes()) != 0 || len(rep.Files[0].Skips) != 1 {
		t.Errorf("report %+v, writes %v", rep, f.writes())
	}

	// No workflows at all is not an error either.
	_, gh = newFakeGitHub(t, map[string]string{"README.md": "x\n"})
	rep, err = gh.Migrate(context.Background(), Options{Repo: "acme/app", Map: repoMap})
	if err != nil || len(rep.Files) != 0 {
		t.Errorf("no workflows: %+v, %v", rep, err)
	}
}

func TestMigrateRefusesForeignBranch(t *testing.T) {
	f, gh := newFakeGitHub(t, repoFiles())
	f.refs[DefaultBranch] = f.refs["main"]
	f.commitFiles(DefaultBranch, map[string]string{"x": "someone's work\n"}, "wip")
	foreign := f.refs[DefaultBranch]
	_, err := gh.Migrate(context.Background(), Options{Repo: "acme/app", Map: repoMap})
	if err == nil || !strings.Contains(err.Error(), "did not make") {
		t.Fatalf("err = %v", err)
	}
	if f.refs[DefaultBranch] != foreign {
		t.Error("someone else's branch was overwritten")
	}
	// Another branch works.
	rep, err := gh.Migrate(context.Background(), Options{Repo: "acme/app", Map: repoMap, Branch: "ci/firerunner"})
	if err != nil || rep.PR != "created" {
		t.Errorf("--branch: %+v, %v", rep, err)
	}
}

func TestMigrateWorkflowScope(t *testing.T) {
	f, gh := newFakeGitHub(t, repoFiles())
	f.workflowScope = false
	_, err := gh.Migrate(context.Background(), Options{Repo: "acme/app", Map: repoMap})
	if !NeedsWorkflowScope(err) {
		t.Fatalf("err = %v, want a workflow scope refusal", err)
	}
}

func TestMigrateRefusesBadInput(t *testing.T) {
	_, gh := newFakeGitHub(t, repoFiles())
	ctx := context.Background()
	for _, o := range []Options{
		{Repo: "acme", Map: repoMap},
		{Repo: "acme/app/x", Map: repoMap},
		{Repo: "../x/y", Map: repoMap},
		{Repo: "acme/app"},
		{Repo: "acme/app", Map: repoMap, Branch: "main"},
	} {
		if _, err := gh.Migrate(ctx, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	// Even if the default branch's head looked like migrate's own commit,
	// migrate never rewrites the default branch.
	f, gh2 := newFakeGitHub(t, repoFiles())
	main := f.commitFiles("main", map[string]string{"x": "y\n"}, "x\n\n"+commitMarker+"\n")
	if _, err := gh2.Migrate(ctx, Options{Repo: "acme/app", Map: repoMap, Branch: "main"}); err == nil || f.refs["main"] != main {
		t.Errorf("the default branch was taken as the PR branch: %v", err)
	}
	gh.Token = "wrong"
	if _, err := gh.Migrate(ctx, Options{Repo: "acme/app", Map: repoMap}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad token: %v", err)
	}
}

func TestUnifiedDiff(t *testing.T) {
	var a, b []string
	for i := 1; i <= 20; i++ {
		a = append(a, "l"+string(rune('a'+i)))
		b = append(b, "l"+string(rune('a'+i)))
	}
	b[1], b[4], b[15] = "X", "Y", "Z"
	got := UnifiedDiff("f.yml", []byte(strings.Join(a, "\n")+"\n"), []byte(strings.Join(b, "\n")+"\n"))
	want := `--- a/f.yml
+++ b/f.yml
@@ -1,8 +1,8 @@
 lb
-lc
+X
 ld
 le
-lf
+Y
 lg
 lh
 li
@@ -13,7 +13,7 @@
 ln
 lo
 lp
-lq
+Z
 lr
 ls
 lt
`
	if got != want {
		t.Errorf("diff:\n%s\nwant:\n%s", got, want)
	}
	if UnifiedDiff("f", []byte("a\n"), []byte("a\n")) != "" {
		t.Error("no change renders a diff")
	}
}
