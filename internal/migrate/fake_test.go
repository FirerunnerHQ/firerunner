package migrate

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub is an in-memory repository behind the REST endpoints migrate
// uses. Trees are flat path -> blob maps; a tree's id hashes its content.
type fakeGitHub struct {
	mu            sync.Mutex
	repo          string
	oldName       string // a name the repository was transferred from: GitHub still answers at it
	defaultBranch string
	blobs         map[string][]byte
	trees         map[string]map[string]fakeEntry
	commits       map[string]fakeCommit
	refs          map[string]string // branch -> commit
	pulls         []*fakePull
	calls         []string // "METHOD path" of every write
	// workflowScope, when false, refuses workflow changes as GitHub does a
	// token without the workflow scope.
	workflowScope bool
}

type fakeEntry struct{ mode, sha string }

type fakeCommit struct {
	tree, message string
	parents       []string
}

type fakePull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Title   string `json:"title"`
	head    string
}

func hash(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newFakeGitHub(t *testing.T, files map[string]string) (*fakeGitHub, *GitHub) {
	f := &fakeGitHub{repo: "acme/app", defaultBranch: "main", blobs: map[string][]byte{}, trees: map[string]map[string]fakeEntry{},
		commits: map[string]fakeCommit{}, refs: map[string]string{}, workflowScope: true}
	f.commitFiles("main", files, "initial")
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &GitHub{Base: srv.URL, Token: "t"}
}

func (f *fakeGitHub) putBlob(b []byte) string {
	sha := hash("blob", string(b))
	f.blobs[sha] = b
	return sha
}

func (f *fakeGitHub) putTree(t map[string]fakeEntry) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{"tree"}
	for _, k := range keys {
		parts = append(parts, k, t[k].mode, t[k].sha)
	}
	sha := hash(parts...)
	f.trees[sha] = t
	return sha
}

// commitFiles commits files (path -> content; "" deletes) onto branch.
func (f *fakeGitHub) commitFiles(branch string, files map[string]string, msg string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree := map[string]fakeEntry{}
	var parents []string
	if head, ok := f.refs[branch]; ok {
		for k, v := range f.trees[f.commits[head].tree] {
			tree[k] = v
		}
		parents = []string{head}
	}
	for p, c := range files {
		if c == "" {
			delete(tree, p)
			continue
		}
		mode := "100644"
		if strings.HasPrefix(c, "LINK:") {
			mode = "120000"
		}
		tree[p] = fakeEntry{mode, f.putBlob([]byte(c))}
	}
	sha := hash("commit", f.putTree(tree), msg, strings.Join(parents, ","))
	f.commits[sha] = fakeCommit{f.putTree(tree), msg, parents}
	f.refs[branch] = sha
	return sha
}

// file returns path's content at branch's head.
func (f *fakeGitHub) file(branch, p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.trees[f.commits[f.refs[branch]].tree][p]
	return string(f.blobs[e.sha])
}

func (f *fakeGitHub) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer t" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	p, ok := strings.CutPrefix(r.URL.Path, "/repos/"+f.repo)
	if !ok && f.oldName != "" {
		p, ok = strings.CutPrefix(r.URL.Path, "/repos/"+f.oldName)
		if ok && r.Method != "GET" {
			// GitHub redirects the old name; a client may replay a write
			// as a read. Writes must go to the current name.
			http.Error(w, `{"message":"Moved Permanently"}`, http.StatusMovedPermanently)
			return
		}
	}
	if !ok || (p != "" && !strings.HasPrefix(p, "/")) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	if r.Method != "GET" {
		f.calls = append(f.calls, r.Method+" "+p)
	}
	var in map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(code int, msg string) { reply(code, map[string]string{"message": msg}) }
	switch {
	case r.Method == "GET" && p == "":
		reply(200, map[string]any{"full_name": f.repo, "default_branch": f.defaultBranch})
	case r.Method == "GET" && strings.HasPrefix(p, "/commits/"):
		c, ok := f.refs[strings.TrimPrefix(p, "/commits/")]
		if !ok {
			fail(404, "Not Found")
			return
		}
		reply(200, map[string]any{"sha": c, "commit": map[string]any{"tree": map[string]string{"sha": f.commits[c].tree}}})
	case r.Method == "GET" && p == "/contents/.github":
		tree := f.trees[f.commits[r.URL.Query().Get("ref")].tree]
		if e, ok := tree[".github"]; ok {
			reply(200, map[string]string{"name": ".github", "type": "file", "sha": e.sha})
			return
		}
		sub := map[string]fakeEntry{}
		for k, v := range tree {
			if rest, ok := strings.CutPrefix(k, ".github/workflows/"); ok {
				sub[rest] = v
			}
		}
		if len(sub) == 0 {
			fail(404, "Not Found")
			return
		}
		reply(200, []map[string]string{{"name": "workflows", "type": "dir", "sha": f.putTree(sub)}})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
		var out []map[string]string
		for k, v := range f.trees[strings.TrimPrefix(p, "/git/trees/")] {
			typ := "blob"
			if strings.Contains(k, "/") { // a subdirectory: report it as a tree
				k, typ, v.mode = strings.SplitN(k, "/", 2)[0], "tree", "040000"
			}
			out = append(out, map[string]string{"path": k, "mode": v.mode, "type": typ, "sha": v.sha})
		}
		reply(200, map[string]any{"tree": out})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/blobs/"):
		b, ok := f.blobs[strings.TrimPrefix(p, "/git/blobs/")]
		if !ok {
			fail(404, "Not Found")
			return
		}
		// GitHub wraps base64 at 60 columns.
		enc := base64.StdEncoding.EncodeToString(b)
		var wrapped strings.Builder
		for len(enc) > 60 {
			wrapped.WriteString(enc[:60] + "\n")
			enc = enc[60:]
		}
		wrapped.WriteString(enc)
		reply(200, map[string]string{"content": wrapped.String(), "encoding": "base64"})
	case r.Method == "POST" && p == "/git/blobs":
		b, err := base64.StdEncoding.DecodeString(in["content"].(string))
		if err != nil {
			fail(422, "bad base64")
			return
		}
		reply(201, map[string]string{"sha": f.putBlob(b)})
	case r.Method == "POST" && p == "/git/trees":
		tree := map[string]fakeEntry{}
		for k, v := range f.trees[in["base_tree"].(string)] {
			tree[k] = v
		}
		for _, e := range in["tree"].([]any) {
			m := e.(map[string]any)
			tree[m["path"].(string)] = fakeEntry{m["mode"].(string), m["sha"].(string)}
		}
		reply(201, map[string]string{"sha": f.putTree(tree)})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
		c := f.commits[strings.TrimPrefix(p, "/git/commits/")]
		var ps []map[string]string
		for _, s := range c.parents {
			ps = append(ps, map[string]string{"sha": s})
		}
		reply(200, map[string]any{"message": c.message, "tree": map[string]string{"sha": c.tree}, "parents": ps})
	case r.Method == "POST" && p == "/git/commits":
		var parents []string
		for _, s := range in["parents"].([]any) {
			parents = append(parents, s.(string))
		}
		// GitHub stores the message without its final newline.
		c := fakeCommit{in["tree"].(string), strings.TrimRight(in["message"].(string), "\n"), parents}
		sha := hash("commit", c.tree, c.message, strings.Join(parents, ","))
		f.commits[sha] = c
		reply(201, map[string]string{"sha": sha})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/ref/heads/"):
		c, ok := f.refs[strings.TrimPrefix(p, "/git/ref/heads/")]
		if !ok {
			fail(404, "Not Found")
			return
		}
		reply(200, map[string]any{"object": map[string]string{"sha": c}})
	case (r.Method == "POST" && p == "/git/refs") || (r.Method == "PATCH" && strings.HasPrefix(p, "/git/refs/heads/")):
		branch := strings.TrimPrefix(p, "/git/refs/heads/")
		if r.Method == "POST" {
			branch = strings.TrimPrefix(in["ref"].(string), "refs/heads/")
		}
		sha := in["sha"].(string)
		if !f.workflowScope && f.touchesWorkflows(f.refs[f.defaultBranch], sha) {
			fail(404, "refusing to allow an OAuth App to create or update workflow `.github/workflows/ci.yml` without `workflow` scope")
			return
		}
		if r.Method == "PATCH" && in["force"] != true {
			fail(422, "Update is not a fast forward")
			return
		}
		f.refs[branch] = sha
		reply(200, map[string]any{"object": map[string]string{"sha": sha}})
	case r.Method == "GET" && p == "/pulls":
		q := r.URL.Query()
		out := []*fakePull{}
		for _, pr := range f.pulls {
			if q.Get("head") == strings.Split(f.repo, "/")[0]+":"+pr.head {
				out = append(out, pr)
			}
		}
		reply(200, out)
	case r.Method == "POST" && p == "/pulls":
		pr := &fakePull{Number: len(f.pulls) + 1, Title: in["title"].(string), Body: in["body"].(string), head: in["head"].(string)}
		pr.HTMLURL = fmt.Sprintf("https://github.com/%s/pull/%d", f.repo, pr.Number)
		f.pulls = append(f.pulls, pr)
		reply(201, pr)
	case r.Method == "PATCH" && strings.HasPrefix(p, "/pulls/"):
		for _, pr := range f.pulls {
			if fmt.Sprint(pr.Number) == strings.TrimPrefix(p, "/pulls/") {
				pr.Body = in["body"].(string)
				reply(200, pr)
				return
			}
		}
		fail(404, "Not Found")
	default:
		fail(404, "Not Found: "+r.Method+" "+p)
	}
}

// touchesWorkflows reports whether commit's tree differs from base's under
// .github/workflows.
func (f *fakeGitHub) touchesWorkflows(base, commit string) bool {
	a, b := f.trees[f.commits[base].tree], f.trees[f.commits[commit].tree]
	for _, t := range []map[string]fakeEntry{a, b} {
		for k := range t {
			if path.Dir(k) == ".github/workflows" && a[k] != b[k] {
				return true
			}
		}
	}
	return false
}
