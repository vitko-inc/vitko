// Package switcher rewrites GitHub Actions workflows so that jobs run on Vitko
// runners. It edits `runs-on` values in place, so comments, quoting and
// formatting survive, and it skips (with a reason) anything it can't change
// safely.
package switcher

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skip reasons (an open enum).
const (
	ReasonExpression       = "expression"
	ReasonNotLinuxX64      = "not_linux_x64"
	ReasonOtherUbuntu      = "other_ubuntu_version"
	ReasonRunnerGroup      = "runner_group"
	ReasonSelfHosted       = "self_hosted"
	ReasonLabelSet         = "label_set"
	ReasonUnknownLabel     = "unknown_label"
	ReasonReusableWorkflow = "reusable_workflow"
	ReasonYAMLAlias        = "yaml_alias"
	ReasonUnparseable      = "unparseable"
)

// Change is one runs-on value that is (or would be) rewritten.
type Change struct {
	File string `json:"file"`
	Job  string `json:"job"`
	Line int    `json:"line"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Skipped is a job left as it is, with the reason.
type Skipped struct {
	File   string `json:"file"`
	Job    string `json:"job"`
	Line   int    `json:"line"`
	RunsOn string `json:"runs_on"`
	Reason string `json:"reason"`
	Hint   string `json:"hint"`
}

// Already is a job that already runs on Vitko.
type Already struct {
	File   string `json:"file"`
	Job    string `json:"job"`
	RunsOn string `json:"runs_on"`
}

// Options select what to switch.
type Options struct {
	// Root is a repository directory or a single workflow file.
	Root string
	// Workflows limits the run to these workflow files (base name or path).
	Workflows []string
	// Jobs limits the run to these job ids.
	Jobs []string
	// Label is the runs-on label to switch to.
	Label string
}

// Plan is the outcome of planning: changes, skips and the new file contents.
type Plan struct {
	Changes  []Change
	Skipped  []Skipped
	Already  []Already
	Files    []string          // workflow files read, relative to Root's directory
	NewFiles map[string][]byte // relative path -> new content, only changed files
	Diff     string
	base     string
}

// LabelRe validates a runs-on label given with --label.
var LabelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ErrNoWorkflows is returned when there is nothing to read.
type ErrNoWorkflows struct{ Dir string }

func (e ErrNoWorkflows) Error() string { return "no workflow files in " + e.Dir }

// Discover returns workflow files under root (sorted) and the directory the
// output paths are relative to.
func Discover(root string) (base string, files []string, err error) {
	st, err := os.Stat(root)
	if err != nil {
		return "", nil, err
	}
	if !st.IsDir() {
		return filepath.Dir(root), []string{filepath.Base(root)}, nil
	}
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, ErrNoWorkflows{Dir: dir}
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && (strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml")) {
			files = append(files, filepath.ToSlash(filepath.Join(".github", "workflows", n)))
		}
	}
	if len(files) == 0 {
		return "", nil, ErrNoWorkflows{Dir: dir}
	}
	sort.Strings(files)
	return root, files, nil
}

// Make plans the switch without writing anything.
func Make(o Options) (*Plan, error) {
	base, files, err := Discover(o.Root)
	if err != nil {
		return nil, err
	}
	p := &Plan{Changes: []Change{}, Skipped: []Skipped{}, Already: []Already{}, NewFiles: map[string][]byte{}, base: base}
	for _, rel := range files {
		if !matchWorkflow(rel, o.Workflows) {
			continue
		}
		p.Files = append(p.Files, rel)
		src, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		out, err := planFile(p, rel, src, o)
		if err != nil {
			p.Skipped = append(p.Skipped, Skipped{File: rel, Job: "", Reason: ReasonUnparseable,
				Hint: "The file isn't valid YAML, so it was left alone: " + firstLine(err.Error())})
			continue
		}
		if !bytes.Equal(out, src) {
			p.NewFiles[rel] = out
			p.Diff += unifiedDiff(rel, src, out)
		}
	}
	return p, nil
}

// Apply writes the planned files.
func (p *Plan) Apply() error {
	for _, rel := range sortedKeys(p.NewFiles) {
		path := filepath.Join(p.base, filepath.FromSlash(rel))
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, p.NewFiles[rel], st.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// ChangedFiles lists files with changes, sorted.
func (p *Plan) ChangedFiles() []string { return sortedKeys(p.NewFiles) }

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func matchWorkflow(rel string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		f = filepath.ToSlash(strings.TrimPrefix(f, "./"))
		if rel == f || filepath.Base(rel) == f || strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)) == f {
			return true
		}
	}
	return false
}

func matchJob(job string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f == job {
			return true
		}
	}
	return false
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// edit is one in-place replacement on a line.
type edit struct {
	line, col int // 1-based, col in runes
	old, new  string
}

func planFile(p *Plan, rel string, src []byte, o Options) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return src, nil
	}
	root := doc.Content[0]
	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return src, nil
	}
	var edits []edit
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		id, job := jobs.Content[i].Value, jobs.Content[i+1]
		if !matchJob(id, o.Jobs) || job.Kind != yaml.MappingNode {
			continue
		}
		if uses := mapValue(job, "uses"); uses != nil {
			if !strings.HasPrefix(uses.Value, "./") {
				p.Skipped = append(p.Skipped, Skipped{File: rel, Job: id, Line: uses.Line, RunsOn: "", Reason: ReasonReusableWorkflow,
					Hint: "This job calls a workflow from another repository (" + uses.Value + "). Switch that workflow in its own repository."})
			}
			continue
		}
		ro := mapValue(job, "runs-on")
		if ro == nil {
			continue
		}
		skip := func(reason, runsOn, hint string) {
			p.Skipped = append(p.Skipped, Skipped{File: rel, Job: id, Line: ro.Line, RunsOn: runsOn, Reason: reason, Hint: hint})
		}
		switch ro.Kind {
		case yaml.AliasNode:
			skip(ReasonYAMLAlias, "*"+ro.Value, "runs-on comes from a YAML anchor. Change the anchor by hand if every job that uses it should move.")
		case yaml.MappingNode:
			skip(ReasonRunnerGroup, flowString(ro), "The job picks a runner group. Change it by hand if the group should move.")
		case yaml.SequenceNode:
			var labels []string
			for _, it := range ro.Content {
				labels = append(labels, it.Value)
			}
			if len(ro.Content) == 1 && ro.Content[0].Kind == yaml.ScalarNode {
				if e, ok := decide(p, rel, id, ro.Content[0], o.Label, src); ok {
					edits = append(edits, e)
				}
				continue
			}
			if containsFold(labels, "self-hosted") {
				skip(ReasonSelfHosted, "["+strings.Join(labels, ", ")+"]", "The job runs on your own self-hosted runners.")
				continue
			}
			skip(ReasonLabelSet, "["+strings.Join(labels, ", ")+"]", "The job asks for several labels. Replace them with "+o.Label+" by hand if it should move.")
		case yaml.ScalarNode:
			if e, ok := decide(p, rel, id, ro, o.Label, src); ok {
				edits = append(edits, e)
			}
		}
	}
	return applyEdits(src, edits)
}

var matrixRe = regexp.MustCompile(`^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$`)

// decide classifies one scalar runs-on value and returns an edit when it
// should change.
func decide(p *Plan, rel, job string, n *yaml.Node, label string, src []byte) (edit, bool) {
	v := strings.TrimSpace(n.Value)
	skip := func(reason, hint string) {
		p.Skipped = append(p.Skipped, Skipped{File: rel, Job: job, Line: n.Line, RunsOn: v, Reason: reason, Hint: hint})
	}
	if strings.Contains(v, "${{") {
		if m := matrixRe.FindStringSubmatch(v); m != nil {
			skip(ReasonExpression, fmt.Sprintf("runs-on comes from the matrix (matrix.%s). Change its Linux x64 values to %s by hand, and check any conditions that compare matrix.%s.", m[1], label, m[1]))
		} else {
			skip(ReasonExpression, "runs-on is an expression. Change it by hand if the job should move.")
		}
		return edit{}, false
	}
	kind := classify(v)
	if v == label {
		kind = "already"
	}
	switch kind {
	case "switch":
		old, ok := rawScalar(n)
		if !ok {
			skip(ReasonUnparseable, "The value's formatting isn't supported. Change it by hand.")
			return edit{}, false
		}
		newRaw := requote(n.Style, label)
		if !lineHas(src, n.Line, n.Column, old) {
			skip(ReasonUnparseable, "The value's formatting isn't supported. Change it by hand.")
			return edit{}, false
		}
		p.Changes = append(p.Changes, Change{File: rel, Job: job, Line: n.Line, From: v, To: label})
		return edit{line: n.Line, col: n.Column, old: old, new: newRaw}, true
	case "already":
		p.Already = append(p.Already, Already{File: rel, Job: job, RunsOn: v})
	case ReasonOtherUbuntu:
		skip(kind, "Vitko runners use Ubuntu 24.04. Check that the job works on 24.04, then change it by hand.")
	case ReasonNotLinuxX64:
		skip(kind, "Only Linux x64 jobs can run on Vitko. This one stays on GitHub.")
	case ReasonSelfHosted:
		skip(kind, "The job runs on your own self-hosted runners.")
	default:
		skip(ReasonUnknownLabel, "A custom or larger runner label. Change it by hand if the job should move.")
	}
	return edit{}, false
}

var olderUbuntu = regexp.MustCompile(`^ubuntu-(1[0-9]|2[0-3])\.[0-9]+$`)

func classify(label string) string {
	l := strings.ToLower(label)
	switch {
	case l == "ubuntu-latest" || l == "ubuntu-24.04":
		return "switch"
	case strings.HasPrefix(l, "vitko-"):
		return "already"
	case olderUbuntu.MatchString(l):
		return ReasonOtherUbuntu
	case strings.HasPrefix(l, "ubuntu-") && strings.HasSuffix(l, "-arm"):
		return ReasonNotLinuxX64
	case strings.HasPrefix(l, "windows-") || strings.HasPrefix(l, "macos-"):
		return ReasonNotLinuxX64
	case l == "self-hosted":
		return ReasonSelfHosted
	}
	return ReasonUnknownLabel
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func flowString(n *yaml.Node) string {
	var parts []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		parts = append(parts, n.Content[i].Value+": "+n.Content[i+1].Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// rawScalar is the scalar as written in the file.
func rawScalar(n *yaml.Node) (string, bool) {
	switch n.Style {
	case 0, yaml.TaggedStyle:
		return n.Value, true
	case yaml.SingleQuotedStyle:
		return "'" + strings.ReplaceAll(n.Value, "'", "''") + "'", true
	case yaml.DoubleQuotedStyle:
		return `"` + n.Value + `"`, true
	}
	return "", false
}

func requote(style yaml.Style, v string) string {
	switch style {
	case yaml.SingleQuotedStyle:
		return "'" + v + "'"
	case yaml.DoubleQuotedStyle:
		return `"` + v + `"`
	}
	return v
}

// splitLines splits keeping each line's terminator.
func splitLines(b []byte) []string {
	s := string(b)
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

func lineHas(src []byte, line, col int, old string) bool {
	lines := splitLines(src)
	if line < 1 || line > len(lines) {
		return false
	}
	r := []rune(lines[line-1])
	if col < 1 || col-1+len([]rune(old)) > len(r) {
		return false
	}
	return string(r[col-1:col-1+len([]rune(old))]) == old
}

func applyEdits(src []byte, edits []edit) ([]byte, error) {
	if len(edits) == 0 {
		return src, nil
	}
	lines := splitLines(src)
	// Right to left, so earlier columns on the same line stay valid.
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].line != edits[j].line {
			return edits[i].line < edits[j].line
		}
		return edits[i].col > edits[j].col
	})
	for _, e := range edits {
		r := []rune(lines[e.line-1])
		start, end := e.col-1, e.col-1+len([]rune(e.old))
		lines[e.line-1] = string(r[:start]) + e.new + string(r[end:])
	}
	out := []byte(strings.Join(lines, ""))
	// Safety net: the result must still parse.
	var chk yaml.Node
	if err := yaml.Unmarshal(out, &chk); err != nil {
		return nil, fmt.Errorf("rewrite produced invalid YAML: %v", err)
	}
	return out, nil
}
