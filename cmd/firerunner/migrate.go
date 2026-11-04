package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/migrate"
)

const migrateUsage = `usage:
  firerunner migrate --repo OWNER/NAME --map FROM=TO [--map FROM=TO ...]
                     [--catalog FILE] [--branch NAME] [--dry-run] [--json]

Move a repository's GitHub Actions jobs onto firerunner: rewrite the runs-on
labels in .github/workflows at the head of its default branch, commit them to
a branch (default firerunner/migrate) and open a pull request, or update the
one already open. Only the labels change; every other byte of each workflow
is kept. Jobs it cannot map safely (expressions, runner groups, self-hosted,
unmapped labels, reusable workflow calls) are left alone and listed, with why.

It runs as you, not as the runner App, which stays read-only: the token comes
from GH_TOKEN, GITHUB_TOKEN, or "gh auth token", and needs the repo and
workflow scopes (gh auth refresh -s workflow), or for a fine-grained token,
Contents, Pull requests and Workflows: write.

flags:
  --repo OWNER/NAME   the repository to migrate
  --map FROM=TO       replace runs-on label FROM with TO (repeatable)
  --catalog FILE      refuse a TO that is no tier name or label in this
                      tier catalog, so no job waits for a runner that never comes
  --branch NAME       the pull request's branch (default firerunner/migrate)
  --dry-run           change nothing: print the diff (with --json, the report)
  --json              print the report as JSON
  --api-url URL       the GitHub API (default https://api.github.com)
exit status: 0 done (or nothing to change), 1 failed, 2 usage error
`

// mapFlag collects repeated --map FROM=TO.
type mapFlag migrate.Map

func (m mapFlag) String() string { return "" }

func (m mapFlag) Set(v string) error {
	from, to, ok := strings.Cut(v, "=")
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if !ok || from == "" || to == "" {
		return fmt.Errorf("want FROM=TO, not %q", v)
	}
	if prev, dup := m[from]; dup && prev != to {
		return fmt.Errorf("%s is mapped to both %s and %s", from, prev, to)
	}
	m[from] = to
	return nil
}

// migrateCmd runs "firerunner migrate" and returns the exit status.
func migrateCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	m := mapFlag{}
	repo := fs.String("repo", "", "")
	catalog := fs.String("catalog", "", "")
	branch := fs.String("branch", migrate.DefaultBranch, "")
	dryRun := fs.Bool("dry-run", false, "")
	asJSON := fs.Bool("json", false, "")
	apiURL := fs.String("api-url", "https://api.github.com", "")
	fs.Var(m, "map", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, migrateUsage)
			return 0
		}
		return migrateUsageError(stderr, err.Error())
	}
	switch {
	case fs.NArg() > 0:
		return migrateUsageError(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	case *repo == "":
		return migrateUsageError(stderr, "--repo is required")
	case len(m) == 0:
		return migrateUsageError(stderr, "at least one --map FROM=TO is required")
	}
	if err := migrate.Map(m).Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *catalog != "" {
		if err := checkTargets(*catalog, migrate.Map(m)); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	fail := func(err error) int {
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		}
		fmt.Fprintln(stderr, "migrate:", err)
		return 1
	}
	host, err := apiHost(*apiURL)
	if err != nil {
		return migrateUsageError(stderr, err.Error())
	}
	token, err := userToken(host)
	if err != nil {
		return fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	gh := &migrate.GitHub{Base: *apiURL, Token: token}
	rep, err := gh.Migrate(ctx, migrate.Options{Repo: *repo, Map: migrate.Map(m), Branch: *branch, DryRun: *dryRun})
	if err != nil {
		if migrate.NeedsWorkflowScope(err) {
			err = fmt.Errorf("%w\nthe token may not change workflows: run \"gh auth refresh -s workflow\", or give a fine-grained token Workflows: write", err)
		}
		return fail(err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return 0
	}
	printMigrate(stdout, rep)
	return 0
}

func migrateUsageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "%s\nrun \"firerunner migrate -h\" for usage\n", msg)
	return 2
}

// checkTargets refuses a target that no tier of the catalog answers to.
func checkTargets(path string, m migrate.Map) error {
	// Only the names matter here, so the slot budget is no constraint.
	tiers, err := config.CheckCatalog(path, 1<<16)
	if err != nil {
		return fmt.Errorf("--catalog: %w", err)
	}
	known := map[string]bool{}
	for _, t := range tiers {
		known[strings.ToLower(t.Name)] = true
		for _, l := range t.Labels {
			known[strings.ToLower(l)] = true
		}
	}
	for from, to := range m {
		if !known[strings.ToLower(to)] {
			return fmt.Errorf("--map %s=%s: no tier in %s is named or labelled %q", from, to, path, to)
		}
	}
	return nil
}

// apiHost is the GitHub host whose API apiURL is: github.com for
// api.github.com, else the URL's host (GHES serves its API at /api/v3).
// The token goes there, so it must be https, or http on loopback (tests).
func apiHost(apiURL string) (string, error) {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("--api-url %q is not a URL", apiURL)
	}
	loopback := u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("--api-url %q must use https: the token is sent to it", apiURL)
	}
	if strings.EqualFold(u.Host, "api.github.com") {
		return "github.com", nil
	}
	return u.Host, nil
}

// userToken is the person's own GitHub token: GH_TOKEN, GITHUB_TOKEN, or
// the gh CLI's for host.
func userToken(host string) (string, error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, nil
		}
	}
	out, err := exec.Command("gh", "auth", "token", "--hostname", host).Output()
	if t := strings.TrimSpace(string(out)); err == nil && t != "" {
		return t, nil
	}
	return "", fmt.Errorf(`no GitHub token for %s: set GH_TOKEN, or log in with "gh auth login --hostname %s"`, host, host)
}

func printMigrate(w io.Writer, rep *migrate.Report) {
	if rep.DryRun {
		for _, f := range rep.Files {
			fmt.Fprint(w, f.Diff)
		}
	}
	fmt.Fprintf(w, "%s at %s (%s): %s, %s to change\n", rep.Repo, rep.DefaultBranch, short(rep.Base), migrate.Plural(len(rep.Files), "workflow file"), migrate.Plural(rep.Changed(), "label"))
	for _, f := range rep.Files {
		for _, c := range f.Changes {
			fmt.Fprintf(w, "  change  %s job %s: %s -> %s\n", f.Path, c.Job, c.From, c.To)
		}
		for _, s := range f.Skips {
			fmt.Fprintf(w, "  skip    %s:%d job %s: %s\n", f.Path, s.Line, s.Job, s.Reason)
		}
		if f.Error != "" {
			fmt.Fprintf(w, "  skip    %s: %s\n", f.Path, f.Error)
		}
	}
	switch {
	case rep.DryRun:
		fmt.Fprintln(w, "dry run: nothing pushed")
		if rep.PR == "open" {
			fmt.Fprintf(w, "a pull request from an earlier run is open: %s\n", rep.PRURL)
		}
	case rep.PR == "stale":
		fmt.Fprintf(w, "nothing to change, but a pull request from an earlier run is still open; close it: %s\n", rep.PRURL)
	case rep.Changed() == 0:
		fmt.Fprintln(w, "nothing to change")
	default:
		fmt.Fprintf(w, "pull request %s: %s\n", rep.PR, rep.PRURL)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
