package migrate

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// DefaultBranch is the branch migrate keeps its pull request on.
const DefaultBranch = "firerunner/migrate"

// commitMarker ends every commit migrate makes: a branch whose head lacks it
// holds someone else's work, and migrate refuses to overwrite it.
const commitMarker = "Generated-by: firerunner migrate"

const workflowsDir = ".github/workflows"

var repoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// Options is one repository's migration.
type Options struct {
	Repo   string // OWNER/NAME
	Map    Map
	Branch string // DefaultBranch when empty
	DryRun bool
}

// Report is what a migration did, or would do.
type Report struct {
	Repo          string       `json:"repo"`
	DefaultBranch string       `json:"default_branch"`
	Base          string       `json:"base"` // the default branch's commit the rewrite started from
	Map           Map          `json:"map"`
	DryRun        bool         `json:"dry_run"`
	Files         []FileReport `json:"files"`
	Branch        string       `json:"branch,omitempty"`
	// PR is "created", "updated", "unchanged" (an open PR already holds this
	// rewrite) or "none" (nothing to change, or a dry run).
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
	rp := "/repos/" + o.Repo
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
	}
	if err := g.do(ctx, "GET", rp, nil, &repo); err != nil {
		return nil, err
	}
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
	rep := &Report{Repo: o.Repo, DefaultBranch: repo.DefaultBranch, Base: head.SHA, Map: o.Map, DryRun: o.DryRun, PR: "none", Files: []FileReport{}}

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
		return rep, nil
	}
	rep.Branch = branch
	return rep, g.publish(ctx, rp, rep, head.SHA, head.Commit.Tree.SHA)
}

type workflowFile struct{ path, mode, sha string }

// workflows lists the workflow files GitHub reads: the regular .yml and
// .yaml files directly in .github/workflows.
func (g *GitHub) workflows(ctx context.Context, rp, commit string) ([]workflowFile, error) {
	var dir []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	err := g.do(ctx, "GET", rp+"/contents/.github?ref="+commit, nil, &dir)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
	update := true
	if exists {
		var cur struct {
			Message string `json:"message"`
			Tree    struct {
				SHA string `json:"sha"`
			} `json:"tree"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := g.do(ctx, "GET", rp+"/git/commits/"+ref.Object.SHA, nil, &cur); err != nil {
			return err
		}
		if !strings.Contains(cur.Message, commitMarker) {
			return fmt.Errorf("branch %s holds commits migrate did not make; delete it, or pass another --branch", rep.Branch)
		}
		update = cur.Tree.SHA != tree.SHA || len(cur.Parents) != 1 || cur.Parents[0].SHA != base
	}
	if update {
		var commit struct {
			SHA string `json:"sha"`
		}
		if err := g.do(ctx, "POST", rp+"/git/commits", map[string]any{"message": commitMessage(rep), "tree": tree.SHA, "parents": []string{base}}, &commit); err != nil {
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

	owner := strings.SplitN(rep.Repo, "/", 2)[0]
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	q := url.Values{"head": {owner + ":" + rep.Branch}, "base": {rep.DefaultBranch}, "state": {"open"}}
	if err := g.do(ctx, "GET", rp+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return err
	}
	body := prBody(rep)
	if len(prs) == 0 {
		var pr struct {
			HTMLURL string `json:"html_url"`
		}
		in := map[string]string{"title": "Run CI on firerunner", "head": rep.Branch, "base": rep.DefaultBranch, "body": body}
		if err := g.do(ctx, "POST", rp+"/pulls", in, &pr); err != nil {
			return err
		}
		rep.PR, rep.PRURL = "created", pr.HTMLURL
		return nil
	}
	rep.PRURL = prs[0].HTMLURL
	if !update && prs[0].Body == body {
		rep.PR = "unchanged"
		return nil
	}
	if err := g.do(ctx, "PATCH", fmt.Sprintf("%s/pulls/%d", rp, prs[0].Number), map[string]string{"body": body}, nil); err != nil {
		return err
	}
	rep.PR = "updated"
	return nil
}

// escapeRef escapes each segment of a branch name for a URL path.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func commitMessage(rep *Report) string {
	var b strings.Builder
	b.WriteString("ci: run jobs on firerunner runners\n\n")
	for _, from := range rep.Map.keys() {
		fmt.Fprintf(&b, "runs-on %s -> %s\n", from, rep.Map[from])
	}
	b.WriteString("\n" + commitMarker + "\n")
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
