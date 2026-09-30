package switcher

import (
	"fmt"
	"strings"
)

const diffContext = 3

// unifiedDiff renders a unified diff for edits that replace lines one for
// one (the line count never changes).
func unifiedDiff(path string, a, b []byte) string {
	al, bl := splitLines(a), splitLines(b)
	if len(al) != len(bl) {
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n(line count changed; diff not shown)\n", path, path)
	}
	var changed []int
	for i := range al {
		if al[i] != bl[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	for i := 0; i < len(changed); {
		start := max(0, changed[i]-diffContext)
		end := min(len(al), changed[i]+diffContext+1)
		j := i + 1
		for j < len(changed) && changed[j]-diffContext <= end {
			end = min(len(al), changed[j]+diffContext+1)
			j++
		}
		n := end - start
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", start+1, n, start+1, n)
		for k := start; k < end; k++ {
			if al[k] == bl[k] {
				sb.WriteString(" " + ensureNL(al[k]))
				continue
			}
			sb.WriteString("-" + ensureNL(al[k]))
			sb.WriteString("+" + ensureNL(bl[k]))
		}
		i = j
	}
	return sb.String()
}

func ensureNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n\\ No newline at end of file\n"
}
