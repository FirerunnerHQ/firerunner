// Package migrate moves a repository's GitHub Actions workflows onto
// firerunner tiers by rewriting their jobs' runs-on labels (#146).
//
// It edits in place: each label it maps is replaced where it stands in the
// file, with its quoting, and every other byte (comments, layout, the rest of
// the YAML) is left as it was. A job it cannot map safely is left alone and
// reported, with why: an expression (runs-on: ${{ matrix.os }}), a runner
// group, a label with no mapping, a reusable workflow call.
package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Map maps a runs-on label (ubuntu-latest) to the firerunner tier label that
// replaces it.
type Map map[string]string

// labelRE is what a target label may be: it is written into YAML as is.
var labelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidTarget reports why label cannot be written as a migration target, or
// nil: it must be a plain label YAML reads as that string.
func ValidTarget(label string) error {
	if !labelRE.MatchString(label) || !plainString(label) {
		return fmt.Errorf("%q is not a plain label", label)
	}
	return nil
}

// Validate refuses a map whose targets could not be written as a plain
// label, or that would map a label twice (a -> b, b -> c): a second run
// must change nothing.
func (m Map) Validate() error {
	if len(m) == 0 {
		return errors.New("migrate: no label mapping (--map FROM=TO)")
	}
	seen := map[string]string{}
	for from, to := range m {
		if from == "" || !labelRE.MatchString(to) || !plainString(to) {
			return fmt.Errorf("migrate: bad mapping %q=%q: a target must be a plain label", from, to)
		}
		if other, ok := seen[FoldLabel(from)]; ok {
			return fmt.Errorf("migrate: labels %q and %q differ only in case; GitHub treats them as one", from, other)
		}
		seen[FoldLabel(from)] = from
	}
	for from, to := range m {
		if next, ok := m.lookup(to); ok && !foldEqual(next, to) {
			return fmt.Errorf("migrate: %q maps to %q, which maps on to %q; map %q to %q directly", from, to, next, from, next)
		}
	}
	return nil
}

// plainString reports whether YAML reads label, unquoted, as that string:
// not true, null, 1.5 or another typed scalar.
func plainString(label string) bool {
	var n yaml.Node
	return yaml.Unmarshal([]byte(label), &n) == nil && len(n.Content) == 1 &&
		n.Content[0].Tag == "!!str" && n.Content[0].Value == label
}

// lookup finds label's target. GitHub matches labels without regard to
// case, so the map does too.
func (m Map) lookup(label string) (string, bool) {
	if to, ok := m[label]; ok {
		return to, true
	}
	for from, to := range m {
		if foldEqual(from, label) {
			return to, true
		}
	}
	return "", false
}

// foldEqual compares labels as GitHub does, folding ASCII case only:
// strings.EqualFold would also equate the long s (U+017F) with s.
func foldEqual(a, b string) bool { return FoldLabel(a) == FoldLabel(b) }

// FoldLabel is the key two runs-on labels share when GitHub treats them as
// one: ASCII case folded, nothing else.
func FoldLabel(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// target reports whether label is one the map writes.
func (m Map) target(label string) bool {
	for _, to := range m {
		if foldEqual(to, label) {
			return true
		}
	}
	return false
}

// Change is one label replaced.
type Change struct {
	Job  string `json:"job"`
	Line int    `json:"line"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Skip is one job left alone, and why.
type Skip struct {
	Job    string `json:"job"`
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// Result is one workflow file's rewrite. Out equals the input when nothing
// changed.
type Result struct {
	Out     []byte   `json:"-"`
	Changes []Change `json:"changes,omitempty"`
	Skips   []Skip   `json:"skips,omitempty"`
}

// edit replaces src[start:end] with text, the new value of node.
type edit struct {
	start, end int
	text       string
	node       *yaml.Node
}

// Rewrite maps the runs-on labels of one workflow file's jobs.
func Rewrite(src []byte, m Map) (Result, error) {
	if err := m.Validate(); err != nil {
		return Result{}, err
	}
	doc, err := parseOne(src)
	if err != nil {
		return Result{}, fmt.Errorf("parse workflow: %w", err)
	}
	res := Result{Out: src}
	if doc == nil {
		return res, nil
	}
	jobs := value(doc, "jobs")
	if jobs != nil && jobs.Kind == yaml.AliasNode {
		res.Skips = append(res.Skips, Skip{"", jobs.Line, "jobs is a YAML alias: change it by hand"})
		return res, nil
	}
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return res, nil
	}
	if k := duplicateKey(jobs); k != "" {
		return Result{}, fmt.Errorf("job %q appears more than once", k)
	}
	lines := lineSpans(src)
	var edits []edit
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		name, job := jobs.Content[i].Value, jobs.Content[i+1]
		skip := func(line int, reason string) { res.Skips = append(res.Skips, Skip{name, line, reason}) }
		switch {
		case job.Kind == yaml.AliasNode:
			skip(job.Line, "the job is a YAML alias of another: change it by hand")
			continue
		case job.Kind != yaml.MappingNode:
			continue
		case job.Anchor != "":
			skip(job.Line, "the job carries a YAML anchor: change it by hand")
			continue
		case value(job, "<<") != nil:
			skip(job.Line, "the job takes keys from a YAML merge (<<): change it by hand")
			continue
		case keyCount(job, "runs-on") > 1:
			skip(job.Line, "runs-on appears more than once")
			continue
		}
		runsOn := value(job, "runs-on")
		if runsOn == nil {
			if value(job, "uses") != nil {
				skip(job.Line, "calls a reusable workflow: migrate the workflow it calls")
			}
			continue
		}
		es, changes, reason := mapRunsOn(src, lines, name, runsOn, m)
		if reason != "" {
			skip(runsOn.Line, reason)
			continue
		}
		edits = append(edits, es...)
		res.Changes = append(res.Changes, changes...)
	}
	if len(edits) == 0 {
		return res, nil
	}
	out, err := apply(src, edits)
	if err != nil {
		return Result{}, err
	}
	if err := verify(doc, out, edits); err != nil {
		return Result{}, fmt.Errorf("rewrite refused: %w", err)
	}
	res.Out = out
	return res, nil
}

// parseOne parses a single-document file; nil when it is empty.
func parseOne(src []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err == io.EOF {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var more yaml.Node
	if err := dec.Decode(&more); err != io.EOF {
		return nil, errors.New("the file holds more than one YAML document")
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return doc.Content[0], nil
}

// verify proves the rewrite did what was planned and nothing else: out
// parses to the same tree as before, every scalar the same but the edited
// ones, which now hold their new labels with their old tags.
func verify(before *yaml.Node, out []byte, edits []edit) error {
	after, err := parseOne(out)
	if err != nil {
		return fmt.Errorf("the rewritten workflow does not parse: %w", err)
	}
	want := make(map[*yaml.Node]string, len(edits))
	for _, e := range edits {
		want[e.node] = e.text
	}
	seen := 0
	var walk func(a, b *yaml.Node) error
	walk = func(a, b *yaml.Node) error {
		if b == nil || a.Kind != b.Kind || len(a.Content) != len(b.Content) || a.Anchor != b.Anchor || a.Tag != b.Tag {
			return fmt.Errorf("the rewrite changed the structure near line %d", a.Line)
		}
		if to, ok := want[a]; ok {
			if b.Value != to {
				return fmt.Errorf("line %d holds %q, not %q", a.Line, b.Value, to)
			}
			seen++
		} else if a.Value != b.Value {
			return fmt.Errorf("the rewrite changed %q on line %d", a.Value, a.Line)
		}
		for i := range a.Content {
			if err := walk(a.Content[i], b.Content[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(before, after); err != nil {
		return err
	}
	if seen != len(want) {
		return errors.New("not every planned edit landed")
	}
	return nil
}

// mapRunsOn plans the edits for one job's runs-on, or says why it is left
// alone. All of a job's labels must map, or none is changed: a runner needs
// every label in the list.
func mapRunsOn(src []byte, lines []lineSpan, job string, n *yaml.Node, m Map) ([]edit, []Change, string) {
	var scalars []*yaml.Node
	switch {
	case n.Kind == yaml.AliasNode:
		return nil, nil, "runs-on is a YAML alias: change the label it refers to by hand"
	case n.Anchor != "":
		return nil, nil, "runs-on carries a YAML anchor: change it by hand"
	}
	switch n.Kind {
	case yaml.ScalarNode:
		scalars = []*yaml.Node{n}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode || c.Anchor != "" {
				return nil, nil, "runs-on holds something other than labels"
			}
			scalars = append(scalars, c)
		}
	case yaml.MappingNode:
		return nil, nil, "runs-on is a group/labels object: set it by hand"
	default:
		return nil, nil, "runs-on is not a label or a list of labels"
	}
	if len(scalars) == 0 || (n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")) {
		return nil, nil, "runs-on is empty"
	}
	var edits []edit
	var changes []Change
	for _, s := range scalars {
		label := s.Value
		switch {
		case strings.Contains(label, "${{"):
			return nil, nil, fmt.Sprintf("runs-on is an expression (%s): change the values it takes by hand", label)
		case foldEqual(label, "self-hosted"):
			return nil, nil, "already runs on self-hosted runners"
		case s.Style&yaml.TaggedStyle != 0:
			return nil, nil, "a label carries a YAML tag: change it by hand"
		case s.Tag != "!!str":
			return nil, nil, fmt.Sprintf("label %s is not a string to YAML (%s): quote it, or change it by hand", label, s.Tag)
		}
		to, ok := m.lookup(label)
		if !ok && m.target(label) {
			// Already migrated: a second run changes nothing.
			to, ok = label, true
		}
		if !ok {
			return nil, nil, fmt.Sprintf("no mapping for label %q", label)
		}
		if to == label {
			continue
		}
		start, end, err := span(src, lines, s)
		if err != nil {
			return nil, nil, err.Error()
		}
		edits = append(edits, edit{start, end, to, s})
		changes = append(changes, Change{job, s.Line, label, to})
	}
	return edits, changes, ""
}

// span finds a scalar's text in src from its position: the quoted string's
// inside for a quoted one, the plain token otherwise. The text found must be
// the scalar's value, or the edit is refused.
func span(src []byte, lines []lineSpan, s *yaml.Node) (int, int, error) {
	if s.Line < 1 || s.Line > len(lines) {
		return 0, 0, errors.New("label position out of range")
	}
	l := lines[s.Line-1]
	line := src[l.start:l.end]
	// yaml.v3 columns count characters, not bytes, and not a leading BOM.
	col, chars := 0, s.Column-1
	if s.Line == 1 && bytes.HasPrefix(line, []byte("\ufeff")) {
		col = len("\ufeff")
	}
	for ; chars > 0 && col < len(line); chars-- {
		_, size := utf8.DecodeRune(line[col:])
		col += size
	}
	if chars != 0 || col >= len(line) {
		return 0, 0, errors.New("label position out of range")
	}
	start := l.start + col
	rest := line[col:]
	var inner []byte
	switch s.Style {
	case yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle:
		q := rest[0]
		closeAt := bytes.IndexByte(rest[1:], q)
		if closeAt < 0 {
			return 0, 0, errors.New("label spans lines")
		}
		start++
		inner = rest[1 : 1+closeAt]
	case 0: // plain
		end := len(rest)
		for i, c := range rest {
			if c == ',' || c == ']' || c == '}' || c == '\r' || (c == '#' && i > 0 && (rest[i-1] == ' ' || rest[i-1] == '\t')) {
				end = i
				break
			}
		}
		inner = bytes.TrimRight(rest[:end], " \t")
	default:
		return 0, 0, errors.New("label is a block scalar")
	}
	if string(inner) != s.Value {
		return 0, 0, fmt.Errorf("could not place label %q in the file", s.Value)
	}
	return start, start + len(inner), nil
}

// apply makes non-overlapping edits, back to front.
func apply(src []byte, edits []edit) ([]byte, error) {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), src...)
	last := len(out) + 1
	for _, e := range edits {
		if e.end > last {
			return nil, errors.New("overlapping edits")
		}
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
		last = e.start
	}
	return out, nil
}

// value returns the value of key in mapping node n, or nil.
func value(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// lineSpan is one line's bytes, without its break.
type lineSpan struct{ start, end int }

// lineSpans splits src into lines as YAML counts them: a break is \r\n, \r,
// \n, NEL, LS or PS. Counting differently would put a node's line on the
// wrong bytes.
func lineSpans(src []byte) []lineSpan {
	var lines []lineSpan
	start := 0
	for i := 0; i < len(src); {
		n := 0
		switch {
		case src[i] == '\r' && i+1 < len(src) && src[i+1] == '\n':
			n = 2
		case src[i] == '\r' || src[i] == '\n':
			n = 1
		case bytes.HasPrefix(src[i:], []byte("\u0085")), bytes.HasPrefix(src[i:], []byte("\u2028")), bytes.HasPrefix(src[i:], []byte("\u2029")):
			_, n = utf8.DecodeRune(src[i:])
		}
		if n == 0 {
			i++
			continue
		}
		lines = append(lines, lineSpan{start, i})
		i += n
		start = i
	}
	return append(lines, lineSpan{start, len(src)})
}

// keyCount counts key in mapping node n.
func keyCount(n *yaml.Node, key string) int {
	c := 0
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			c++
		}
	}
	return c
}

// duplicateKey returns a key that mapping node n holds more than once.
func duplicateKey(n *yaml.Node) string {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if seen[k] {
			return k
		}
		seen[k] = true
	}
	return ""
}
