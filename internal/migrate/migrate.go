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

// Validate refuses a map whose targets could not be written as a plain label.
func (m Map) Validate() error {
	if len(m) == 0 {
		return errors.New("migrate: no label mapping (--map FROM=TO)")
	}
	for from, to := range m {
		if from == "" || !labelRE.MatchString(to) {
			return fmt.Errorf("migrate: bad mapping %q=%q: a target must be a plain label", from, to)
		}
	}
	return nil
}

// target reports whether label is one the map writes.
func (m Map) target(label string) bool {
	for _, to := range m {
		if to == label {
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

// edit replaces src[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// Rewrite maps the runs-on labels of one workflow file's jobs.
func Rewrite(src []byte, m Map) (Result, error) {
	if err := m.Validate(); err != nil {
		return Result{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return Result{}, fmt.Errorf("parse workflow: %w", err)
	}
	res := Result{Out: src}
	if len(doc.Content) == 0 {
		return res, nil
	}
	jobs := value(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return res, nil
	}
	lines := lineStarts(src)
	var edits []edit
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		name, job := jobs.Content[i].Value, jobs.Content[i+1]
		if job.Kind != yaml.MappingNode {
			continue
		}
		runsOn := value(job, "runs-on")
		if runsOn == nil {
			if value(job, "uses") != nil {
				res.Skips = append(res.Skips, Skip{name, job.Line, "calls a reusable workflow: migrate the workflow it calls"})
			}
			continue
		}
		es, changes, skip := mapRunsOn(src, lines, name, runsOn, m)
		if skip != "" {
			res.Skips = append(res.Skips, Skip{name, runsOn.Line, skip})
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
	var check yaml.Node
	if err := yaml.Unmarshal(out, &check); err != nil {
		return Result{}, fmt.Errorf("rewritten workflow does not parse: %w", err)
	}
	res.Out = out
	return res, nil
}

// mapRunsOn plans the edits for one job's runs-on, or says why it is left
// alone. All of a job's labels must map, or none is changed: a runner needs
// every label in the list.
func mapRunsOn(src []byte, lines []int, job string, n *yaml.Node, m Map) ([]edit, []Change, string) {
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
		return nil, nil, "runs-on names a runner group: set its labels by hand"
	default:
		return nil, nil, "runs-on is not a label or a list of labels"
	}
	var edits []edit
	var changes []Change
	for _, s := range scalars {
		label := s.Value
		switch {
		case strings.Contains(label, "${{"):
			return nil, nil, fmt.Sprintf("runs-on is an expression (%s): change the values it takes by hand", label)
		case label == "self-hosted":
			return nil, nil, "already runs on self-hosted runners"
		}
		to, ok := m[label]
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
		edits = append(edits, edit{start, end, to})
		changes = append(changes, Change{job, s.Line, label, to})
	}
	return edits, changes, ""
}

// span finds a scalar's text in src from its position: the quoted string's
// inside for a quoted one, the plain token otherwise. The text found must be
// the scalar's value, or the edit is refused.
func span(src []byte, lines []int, s *yaml.Node) (int, int, error) {
	if s.Line < 1 || s.Line > len(lines) {
		return 0, 0, errors.New("label position out of range")
	}
	lineEnd := len(src)
	if s.Line < len(lines) {
		lineEnd = lines[s.Line] - 1
	}
	line := src[lines[s.Line-1]:lineEnd]
	// yaml.v3 columns count characters, not bytes.
	col, chars := 0, s.Column-1
	for ; chars > 0 && col < len(line); chars-- {
		_, size := utf8.DecodeRune(line[col:])
		col += size
	}
	if chars != 0 || col >= len(line) {
		return 0, 0, errors.New("label position out of range")
	}
	start := lines[s.Line-1] + col
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

// lineStarts returns the byte offset where each line starts.
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}
