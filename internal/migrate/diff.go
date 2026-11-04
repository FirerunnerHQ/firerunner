package migrate

import (
	"fmt"
	"strings"
)

// diffContext is the unchanged lines shown around each change.
const diffContext = 3

// UnifiedDiff renders a rewrite as a unified diff. Rewrite replaces text
// within lines and never adds or removes one, so the two sides align line
// for line; were they ever not to, the whole file is shown as one hunk.
func UnifiedDiff(path string, old, new []byte) string {
	a, b := splitLines(string(old)), splitLines(string(new))
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	if len(a) != len(b) {
		fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(a), len(b))
		for _, l := range a {
			writeLine(&out, '-', l)
		}
		for _, l := range b {
			writeLine(&out, '+', l)
		}
		return out.String()
	}
	var changed []int
	for i := range a {
		if a[i] != b[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	for i := 0; i < len(changed); {
		// A hunk runs from the first change's context to the last change
		// whose context touches the next one's.
		j := i
		for j+1 < len(changed) && changed[j+1]-changed[j] <= 2*diffContext {
			j++
		}
		start := max(changed[i]-diffContext, 0)
		end := min(changed[j]+diffContext+1, len(a))
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", start+1, end-start, start+1, end-start)
		for k := start; k < end; k++ {
			if a[k] == b[k] {
				writeLine(&out, ' ', a[k])
				continue
			}
			writeLine(&out, '-', a[k])
			writeLine(&out, '+', b[k])
		}
		i = j + 1
	}
	return out.String()
}

// splitLines splits text into lines, each keeping its newline; a last line
// without one is marked as git marks it when written.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(s, "\n")[:strings.Count(s, "\n")+boolInt(!strings.HasSuffix(s, "\n"))]
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func writeLine(out *strings.Builder, mark byte, line string) {
	out.WriteByte(mark)
	out.WriteString(strings.TrimSuffix(line, "\n"))
	out.WriteByte('\n')
	if !strings.HasSuffix(line, "\n") {
		out.WriteString("\\ No newline at end of file\n")
	}
}
