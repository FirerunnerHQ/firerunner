package main

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/solcreek/firerunner/internal/config"
)

// The flag package prints every flag's default in -h output and on every
// parse error. A credential that reaches a flag default is therefore printed
// to stderr by a mistyped flag. These tests find the flags and the secret
// environment variables themselves, so a flag or variable added later is
// covered without editing them.

// secretWords mark a flag or environment variable name as a credential.
var secretWords = map[string]bool{
	"TOKEN": true, "KEY": true, "SECRET": true, "PASSWORD": true, "PASSWD": true,
	"PRIVATE": true, "CREDENTIAL": true, "CREDENTIALS": true, "PAT": true,
}

// looksSecret reports whether a name like FR_APP_PRIVATE_KEY or
// app-private-key names a credential.
func looksSecret(name string) bool {
	for _, w := range strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool { return r == '_' || r == '-' }) {
		if secretWords[w] {
			return true
		}
	}
	return false
}

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)

// secretEnvNames returns every string literal in the module's non-test Go
// source that is shaped like an environment variable and names a credential:
// any variable the code could read a secret from.
func secretEnvNames(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("go.mod not found above the test directory")
		}
		root = parent
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && envName.MatchString(s) && looksSecret(s) {
					seen[s] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for s := range seen {
		names = append(names, s)
	}
	return names
}

// plantSecrets sets every secret environment variable to a recognisable
// value for the rest of the test, and returns those values.
func plantSecrets(t *testing.T) []string {
	t.Helper()
	names := secretEnvNames(t)
	// The search must find the credentials we know of, or the test passes
	// without testing anything.
	for _, want := range []string{"FR_TOKEN", "FR_APP_PRIVATE_KEY"} {
		if !strings.Contains(" "+strings.Join(names, " ")+" ", " "+want+" ") {
			t.Fatalf("secret env search found %v, not %s", names, want)
		}
	}
	t.Logf("planted %d secret env vars: %v", len(names), names)
	var values []string
	for _, n := range names {
		v := "planted-secret-" + n
		t.Setenv(n, v)
		values = append(values, v)
	}
	return values
}

// flagSets returns every flag set the binary builds: the daemon's, which
// status, doctor and config check parse too, and cache-server's.
func flagSets() map[string]*flag.FlagSet {
	cache, _ := newCacheFlagSet()
	return map[string]*flag.FlagSet{"firerunner": config.FlagSet(), "cache-server": cache}
}

func leaked(s string, secrets []string) string {
	for _, v := range secrets {
		if strings.Contains(s, v) {
			return v
		}
	}
	return ""
}

func TestFlags_NoSecretIsADefault(t *testing.T) {
	secrets := plantSecrets(t)
	secretFlags := 0
	for set, fs := range flagSets() {
		fs.VisitAll(func(f *flag.Flag) {
			if looksSecret(f.Name) {
				secretFlags++
				if f.DefValue != "" {
					t.Errorf("%s --%s names a credential but has the default %q: the flag package prints it", set, f.Name, f.DefValue)
				}
			}
			if v := leaked(f.DefValue+"\n"+f.Usage, secrets); v != "" {
				t.Errorf("%s --%s carries the secret %q in its default or usage", set, f.Name, v)
			}
		})
	}
	if secretFlags == 0 {
		t.Fatal("no flag looks like a credential; the name check is broken")
	}
}

// captureOutput runs f and returns what it wrote to os.Stdout and os.Stderr.
func captureOutput(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	f()
	os.Stdout, os.Stderr = oldOut, oldErr
	w.Close()
	<-done
	r.Close()
	return buf.String()
}

// badArgs returns argument lists that make fs print its usage: -h, an unknown
// flag, and a value that does not parse for each flag that is not a string.
func badArgs(fs *flag.FlagSet) [][]string {
	args := [][]string{{"-h"}, {"--no-such-flag"}}
	fs.VisitAll(func(f *flag.Flag) {
		if g, ok := f.Value.(flag.Getter); ok {
			if _, isString := g.Get().(string); isString {
				return // any value parses, and the command would run
			}
		}
		args = append(args, []string{"--" + f.Name + "=not-a-value"})
	})
	return args
}

func TestCLI_UsageAndParseErrorsNeverPrintSecrets(t *testing.T) {
	secrets := plantSecrets(t)
	sets := flagSets()
	type entry struct {
		name string
		set  string
		run  func(args []string) string // returns the error text, if any
	}
	errText := func(err error) string {
		if err != nil {
			return err.Error()
		}
		return ""
	}
	entries := []entry{
		// The daemon logs FromFlags' error and exits.
		{"firerunner", "firerunner", func(a []string) string { _, err := config.FromFlags(a); return errText(err) }},
		{"status", "firerunner", func(a []string) string { return errText(diagnose("status", a)) }},
		{"doctor", "firerunner", func(a []string) string { return errText(diagnose("doctor", a)) }},
		{"config check", "firerunner", func(a []string) string {
			var out, errb bytes.Buffer
			configCmd(append([]string{"check"}, a...), &out, &errb)
			return out.String() + errb.String()
		}},
		{"cache-server", "cache-server", func(a []string) string { return errText(cacheServe(a)) }},
	}
	for _, e := range entries {
		for _, args := range badArgs(sets[e.set]) {
			var text string
			out := captureOutput(t, func() { text = e.run(args) })
			if v := leaked(out+"\n"+text, secrets); v != "" {
				t.Errorf("%s %v printed the secret %q:\n%s\n%s", e.name, args, v, out, text)
			}
			// The output was captured, or the check above proves nothing.
			if args[0] == "--no-such-flag" && !strings.Contains(out, "no-such-flag") {
				t.Errorf("%s %v: usage output not captured:\n%s", e.name, args, out)
			}
		}
	}
}
