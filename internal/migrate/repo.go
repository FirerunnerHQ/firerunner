package migrate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// DefaultBranch is the branch migrate keeps its pull request on.
const DefaultBranch = "firerunner/migrate"

// commitMarker and treeTrailer end every commit migrate makes; the trailer
// names the commit's own tree. A branch head whose message lacks them, or
// whose tree is not the one named (an amend, a rebase, a fix pushed on top),
// holds someone else's work, and migrate refuses to overwrite it.
const (
	commitMarker = "Generated-by: firerunner migrate"
	treeTrailer  = "Migrate-Tree: "
)

const workflowsDir = ".github/workflows"

var repoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// validBranch applies git's ref name rules (git check-ref-format), and
// refuses a name that could reach another ref through the URL path.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasPrefix(b, "-") ||
		strings.HasPrefix(b, "refs/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") || b == "@" {
		return false
	}
	for _, seg := range strings.Split(b, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	for _, c := range b {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return false
		}
	}
	return true
}

// Options is one repository's migration.
type Options struct {
	Repo   string // OWNER/NAME
	Map    Map
	Branch string // DefaultBranch when empty
	DryRun bool
}

// Report is what a migration did, or would do.
type Report struct {
	OK            bool         `json:"ok"` // always true: a failure is {"ok": false, "error": …}
	Repo          string       `json:"repo"`
	DefaultBranch string       `json:"default_branch"`
	Base          string       `json:"base"` // the default branch's commit the rewrite started from
	Map           Map          `json:"map"`
	DryRun        bool         `json:"dry_run"`
	Files         []FileReport `json:"files"`
	Branch        string       `json:"branch,omitempty"`
	// PR is "created", "updated", "unchanged" (the open PR already holds
	// this rewrite), "open" (a dry run, and a PR from an earlier run is
	// open), "stale" (nothing to change, but a PR from an earlier run is
	// still open: close it) or "none".
	PR    string `json:"pr"`
	PRURL string `json:"pr_url,omitempty"`
}

// FileReport is one workflow file's result. Error is set when the file could
// not be read as a workflow; it is then left alone.
type FileReport struct {
	Path    string   `json:"path"`
	Changes []Change `json:"changes,omitempty"`
	Skips   []Skip   `json:"skips,omitempty"`
	Error   string   `json:"error,omitempty"`
	Diff    string   `json:"diff,omitempty"`

	mode string
	out  []byte
}

// Changed counts the labels the migration replaces.
func (r *Report) Changed() int {
	n := 0
	for _, f := range r.Files {
		n += len(f.Changes)
	}
	return n
}

// Migrate rewrites a repository's workflows at the head of its default
// branch and, unless DryRun, commits them to Branch and opens or updates a
// pull request from it. The branch is rebuilt from the default branch on
// every run, so a rerun updates the PR to the current workflows.
func (g *GitHub) Migrate(ctx context.Context, o Options) (*Report, error) {
	if !repoRE.MatchString(o.Repo) {
		return nil, fmt.Errorf("repository %q is not OWNER/NAME", o.Repo)
	}
	if err := o.Map.Validate(); err != nil {
		return nil, err
	}
	branch := o.Branch
	if branch == "" {
		branch = DefaultBranch
	}
	if !validBranch(branch) {
		return nil, fmt.Errorf("branch %q is not a valid branch name", branch)
	}
	var repo struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
	}
	if err := g.do(ctx, "GET", "/repos/"+o.Repo, nil, &repo); err != nil {
		return nil, err
	}
	// A renamed or transferred repository answers at its old name through
	// redirects; work at its current one, so the PR lookup by head owner
	// finds the PR.
	if !repoRE.MatchString(repo.FullName) {
		return nil, fmt.Errorf("GitHub names the repository %q", repo.FullName)
	}
	rp := "/repos/" + repo.FullName
	if repo.Archived {
		return nil, fmt.Errorf("%s is archived", o.Repo)
	}
	if branch == repo.DefaultBranch {
		return nil, fmt.Errorf("branch %q is the default branch", branch)
	}
	var head struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := g.do(ctx, "GET", rp+"/commits/"+url.PathEscape(repo.DefaultBranch), nil, &head); err != nil {
		return nil, err
	}
	rep := &Report{OK: true, Repo: repo.FullName, DefaultBranch: repo.DefaultBranch, Base: head.SHA, Map: o.Map, DryRun: o.DryRun, PR: "none", Files: []FileReport{}}

	files, err := g.workflows(ctx, rp, head.SHA)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		var blob struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		fr := FileReport{Path: f.path, mode: f.mode}
		if err := g.do(ctx, "GET", rp+"/git/blobs/"+f.sha, nil, &blob); err != nil {
			return nil, err
		}
		src, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil || blob.Encoding != "base64" {
			return nil, fmt.Errorf("%s: unexpected blob encoding %q", f.path, blob.Encoding)
		}
		res, err := Rewrite(src, o.Map)
		if err != nil {
			fr.Error = err.Error()
		} else {
			fr.Changes, fr.Skips = res.Changes, res.Skips
			if len(res.Changes) > 0 {
				fr.out = res.Out
				fr.Diff = UnifiedDiff(f.path, src, res.Out)
			}
		}
		rep.Files = append(rep.Files, fr)
	}
	if o.DryRun || rep.Changed() == 0 {
		pr, err := g.openPR(ctx, rp, rep.Repo, branch, rep.DefaultBranch)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			rep.Branch, rep.PRURL, rep.PR = branch, pr.HTMLURL, "open"
			if !o.DryRun {
				rep.PR = "stale"
			}
		}
		return rep, nil
	}
	rep.Branch = branch
	return rep, g.publish(ctx, rp, rep, head.SHA, head.Commit.Tree.SHA)
}

type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

// openPR finds the open pull request from branch into base, if any.
func (g *GitHub) openPR(ctx context.Context, rp, repo, branch, base string) (*pullRequest, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var prs []pullRequest
	q := url.Values{"head": {owner + ":" + branch}, "base": {base}, "state": {"open"}}
	if err := g.do(ctx, "GET", rp+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

type workflowFile struct{ path, mode, sha string }

// workflows lists the workflow files GitHub reads: the regular .yml and
// .yaml files directly in .github/workflows.
func (g *GitHub) workflows(ctx context.Context, rp, commit string) ([]workflowFile, error) {
	var raw json.RawMessage
	err := g.do(ctx, "GET", rp+"/contents/.github?ref="+commit, nil, &raw)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dir []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	if json.Unmarshal(raw, &dir) != nil {
		return nil, nil // .github is a file or a symlink: no workflows
	}
	treeSHA := ""
	for _, e := range dir {
		if e.Name == "workflows" && e.Type == "dir" {
			treeSHA = e.SHA
		}
	}
	if treeSHA == "" {
		return nil, nil
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.do(ctx, "GET", rp+"/git/trees/"+treeSHA, nil, &tree); err != nil {
		return nil, err
	}
	var out []workflowFile
	for _, e := range tree.Tree {
		ext := path.Ext(e.Path)
		if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") || (ext != ".yml" && ext != ".yaml") {
			continue // symlinks, submodules, directories, other files
		}
		out = append(out, workflowFile{workflowsDir + "/" + e.Path, e.Mode, e.SHA})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// publish commits the rewritten files on top of base to the branch and
// opens or updates the pull request.
func (g *GitHub) publish(ctx context.Context, rp string, rep *Report, base, baseTree string) error {
	// Whose is the branch? Decided before anything is written.
	refPath := rp + "/git/ref/heads/" + escapeRef(rep.Branch)
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.do(ctx, "GET", refPath, nil, &ref)
	exists := err == nil
	if err != nil && !notFound(err) {
		return err
	}
	var cur struct {
		Message string `json:"message"`
		Tree    struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if exists {
		if err := g.do(ctx, "GET", rp+"/git/commits/"+ref.Object.SHA, nil, &cur); err != nil {
			return err
		}
		if !hasLine(cur.Message, commitMarker) || !hasLine(cur.Message, treeTrailer+cur.Tree.SHA) || len(cur.Parents) != 1 {
			return fmt.Errorf("branch %s holds commits migrate did not make; delete it, or pass another --branch", rep.Branch)
		}
	}

	type entry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var entries []entry
	for _, f := range rep.Files {
		if f.out == nil {
			continue
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := g.do(ctx, "POST", rp+"/git/blobs", map[string]string{"content": base64.StdEncoding.EncodeToString(f.out), "encoding": "base64"}, &blob); err != nil {
			return err
		}
		entries = append(entries, entry{f.Path, f.mode, "blob", blob.SHA})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, "POST", rp+"/git/trees", map[string]any{"base_tree": baseTree, "tree": entries}, &tree); err != nil {
		return err
	}
	update := !exists || cur.Tree.SHA != tree.SHA || cur.Parents[0].SHA != base
	if update {
		var commit struct {
			SHA string `json:"sha"`
		}
		if err := g.do(ctx, "POST", rp+"/git/commits", map[string]any{"message": commitMessage(rep, tree.SHA), "tree": tree.SHA, "parents": []string{base}}, &commit); err != nil {
			return err
		}
		if exists {
			err = g.do(ctx, "PATCH", rp+"/git/refs/heads/"+escapeRef(rep.Branch), map[string]any{"sha": commit.SHA, "force": true}, nil)
		} else {
			err = g.do(ctx, "POST", rp+"/git/refs", map[string]string{"ref": "refs/heads/" + rep.Branch, "sha": commit.SHA}, nil)
		}
		if err != nil {
			return err
		}
	}

	pr, err := g.openPR(ctx, rp, rep.Repo, rep.Branch, rep.DefaultBranch)
	if err != nil {
		return err
	}
	body := prBody(rep)
	if pr == nil {
		var created pullRequest
		in := map[string]string{"title": "Run CI on firerunner", "head": rep.Branch, "base": rep.DefaultBranch, "body": body}
		if err := g.do(ctx, "POST", rp+"/pulls", in, &created); err != nil {
			return err
		}
		rep.PR, rep.PRURL = "created", created.HTMLURL
		return nil
	}
	rep.PRURL = pr.HTMLURL
	if !update && pr.Body == body {
		rep.PR = "unchanged"
		return nil
	}
	if err := g.do(ctx, "PATCH", fmt.Sprintf("%s/pulls/%d", rp, pr.Number), map[string]string{"body": body}, nil); err != nil {
		return err
	}
	rep.PR = "updated"
	return nil
}

// hasLine reports whether msg has a line that is exactly line. GitHub
// stores a commit message without its final newline.
func hasLine(msg, line string) bool {
	for _, l := range strings.Split(msg, "\n") {
		if strings.TrimRight(l, "\r") == line {
			return true
		}
	}
	return false
}

// escapeRef escapes each segment of a branch name for a URL path.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func commitMessage(rep *Report, tree string) string {
	var b strings.Builder
	b.WriteString("ci: run jobs on firerunner runners\n\n")
	for _, from := range rep.Map.keys() {
		fmt.Fprintf(&b, "runs-on %s -> %s\n", from, rep.Map[from])
	}
	b.WriteString("\n" + commitMarker + "\n" + treeTrailer + tree + "\n")
	return b.String()
}

func prBody(rep *Report) string {
	var b strings.Builder
	b.WriteString("This pull request moves jobs onto firerunner runners by changing their `runs-on` labels. Nothing else in the workflows changes.\n\n")
	b.WriteString("| label | becomes |\n|---|---|\n")
	for _, from := range rep.Map.keys() {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", from, rep.Map[from])
	}
	fmt.Fprintf(&b, "\n**Changed** (%s):\n\n", Plural(rep.Changed(), "label"))
	var skips []string
	for _, f := range rep.Files {
		for _, c := range f.Changes {
			fmt.Fprintf(&b, "- `%s` job `%s`: `%s` → `%s`\n", f.Path, c.Job, c.From, c.To)
		}
		for _, s := range f.Skips {
			skips = append(skips, fmt.Sprintf("- `%s` job `%s` (line %d): %s\n", f.Path, s.Job, s.Line, s.Reason))
		}
		if f.Error != "" {
			skips = append(skips, fmt.Sprintf("- `%s`: not read: %s\n", f.Path, f.Error))
		}
	}
	if len(skips) > 0 {
		b.WriteString("\n**Left alone** (change these by hand if they should move too):\n\n")
		b.WriteString(strings.Join(skips, ""))
	}
	b.WriteString("\nTo roll back after merging, revert this pull request.\n\n")
	fmt.Fprintf(&b, "Generated by `firerunner migrate` from `%s` at %s. Running it again rebuilds this branch from the default branch and updates this pull request.\n", rep.DefaultBranch, rep.Base)
	return b.String()
}

func (m Map) keys() []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Plural renders n and noun, adding an s unless n is 1.
func Plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
