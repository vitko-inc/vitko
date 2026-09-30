package switcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const label = "vitko-ubuntu-24.04"

// copyTree copies testdata/repo into a temp dir so tests can write to it.
func copyTree(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", "repo")
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func skipFor(p *Plan, file, job string) *Skipped {
	for i := range p.Skipped {
		if strings.HasSuffix(p.Skipped[i].File, file) && p.Skipped[i].Job == job {
			return &p.Skipped[i]
		}
	}
	return nil
}

func TestPlanChangesAndSkips(t *testing.T) {
	root := copyTree(t)
	p, err := Make(Options{Root: root, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ file, job string }
	got := map[key]Change{}
	for _, c := range p.Changes {
		got[key{filepath.Base(c.File), c.Job}] = c
	}
	for _, want := range []key{
		{"ci.yml", "test"}, {"ci.yml", "lint"}, {"ci.yml", "build"}, {"ci.yml", "flow"},
		{"called.yml", "inner"}, {"crlf.yml", "crlf"},
	} {
		c, ok := got[want]
		if !ok {
			t.Errorf("missing change %v", want)
			continue
		}
		if c.To != label {
			t.Errorf("%v: to = %q", want, c.To)
		}
	}
	if len(p.Changes) != 6 {
		t.Errorf("changes = %d, want 6: %+v", len(p.Changes), p.Changes)
	}
	reasons := map[key]string{
		{"ci.yml", "mac"}:         ReasonNotLinuxX64,
		{"ci.yml", "arm"}:         ReasonNotLinuxX64,
		{"ci.yml", "old"}:         ReasonOtherUbuntu,
		{"ci.yml", "big"}:         ReasonUnknownLabel,
		{"matrix.yml", "test"}:    ReasonExpression,
		{"matrix.yml", "choose"}:  ReasonExpression,
		{"reuse.yaml", "remote"}:  ReasonReusableWorkflow,
		{"reuse.yaml", "group"}:   ReasonRunnerGroup,
		{"reuse.yaml", "mine"}:    ReasonSelfHosted,
		{"reuse.yaml", "multi"}:   ReasonLabelSet,
		{"alias.yml", "anchored"}: ReasonYAMLAlias,
	}
	for k, want := range reasons {
		s := skipFor(p, k.file, k.job)
		if s == nil {
			t.Errorf("%v: not skipped", k)
			continue
		}
		if s.Reason != want {
			t.Errorf("%v: reason = %q, want %q", k, s.Reason, want)
		}
		if s.Hint == "" {
			t.Errorf("%v: no hint", k)
		}
	}
	if len(p.Skipped) != len(reasons) {
		t.Errorf("skipped = %d, want %d: %+v", len(p.Skipped), len(reasons), p.Skipped)
	}
	if s := skipFor(p, "reuse.yaml", "local"); s != nil {
		t.Errorf("a local reusable workflow should not be reported (its own file is switched): %+v", s)
	}
	if s := skipFor(p, "matrix.yml", "test"); s != nil && !strings.Contains(s.Hint, "matrix.os") {
		t.Errorf("matrix hint should name the matrix key: %q", s.Hint)
	}
	if len(p.Already) != 1 || p.Already[0].Job != "done" {
		t.Errorf("already = %+v", p.Already)
	}
	// Planning writes nothing.
	if strings.Contains(read(t, root, ".github/workflows/ci.yml"), label+"   #") {
		t.Error("Make wrote to disk")
	}
}

func TestApplyPreservesFormatting(t *testing.T) {
	root := copyTree(t)
	p, err := Make(Options{Root: root, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	ci := read(t, root, ".github/workflows/ci.yml")
	for _, want := range []string{
		"# CI for the example project.\n",
		"    runs-on: vitko-ubuntu-24.04   # keep this comment\n",
		"    runs-on: \"vitko-ubuntu-24.04\"\n",
		"    runs-on: 'vitko-ubuntu-24.04'\n",
		"    runs-on: [vitko-ubuntu-24.04]\n",
		"    runs-on: macos-latest\n",
		"    runs-on: ubuntu-22.04\n",
		"    needs: [test, lint]\n",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml lacks %q:\n%s", want, ci)
		}
	}
	orig, _ := os.ReadFile("testdata/repo/.github/workflows/ci.yml")
	if strings.Count(ci, "\n") != strings.Count(string(orig), "\n") {
		t.Error("line count changed")
	}
	crlf := read(t, root, ".github/workflows/crlf.yml")
	if !strings.Contains(crlf, "    runs-on: vitko-ubuntu-24.04\r\n") || strings.Count(crlf, "\r\n") != 7 {
		t.Errorf("CRLF not preserved: %q", crlf)
	}
	called := read(t, root, ".github/workflows/called.yml")
	if !strings.Contains(called, "defaults: &defaults\n  runs-on: ubuntu-latest\n") {
		t.Errorf("a runs-on outside jobs must not change:\n%s", called)
	}
	matrix := read(t, root, ".github/workflows/matrix.yml")
	orig, _ = os.ReadFile("testdata/repo/.github/workflows/matrix.yml")
	if matrix != string(orig) {
		t.Error("matrix.yml should be unchanged")
	}
}

func TestIdempotent(t *testing.T) {
	root := copyTree(t)
	p, _ := Make(Options{Root: root, Label: label})
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	p2, err := Make(Options{Root: root, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Changes) != 0 || len(p2.NewFiles) != 0 || p2.Diff != "" {
		t.Errorf("second run changes: %+v", p2.Changes)
	}
	if len(p2.Already) != 7 {
		t.Errorf("already = %d, want 7", len(p2.Already))
	}
}

func TestCustomLabelIsIdempotent(t *testing.T) {
	root := copyTree(t)
	p, _ := Make(Options{Root: root, Label: "my-runners"})
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	p2, _ := Make(Options{Root: root, Label: "my-runners"})
	if len(p2.Changes) != 0 {
		t.Errorf("second run changes: %+v", p2.Changes)
	}
}

func TestFilters(t *testing.T) {
	root := copyTree(t)
	p, err := Make(Options{Root: root, Label: label, Workflows: []string{"ci.yml"}, Jobs: []string{"test", "lint"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 2 || len(p.Skipped) != 0 {
		t.Errorf("changes %+v skipped %+v", p.Changes, p.Skipped)
	}
	p, _ = Make(Options{Root: root, Label: label, Workflows: []string{"reuse"}})
	if len(p.Files) != 1 || p.Files[0] != ".github/workflows/reuse.yaml" {
		t.Errorf("files = %v", p.Files)
	}
}

func TestSingleFile(t *testing.T) {
	root := copyTree(t)
	p, err := Make(Options{Root: filepath.Join(root, ".github/workflows/crlf.yml"), Label: label})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].File != "crlf.yml" {
		t.Errorf("%+v", p.Changes)
	}
}

func TestDiff(t *testing.T) {
	root := copyTree(t)
	p, _ := Make(Options{Root: root, Label: label, Workflows: []string{"ci.yml"}, Jobs: []string{"test"}})
	want := "--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -4,7 +4,7 @@\n \n jobs:\n   test:\n-    runs-on: ubuntu-latest   # keep this comment\n+    runs-on: vitko-ubuntu-24.04   # keep this comment\n     steps:\n       - uses: actions/checkout@v4\n       - run: make test\n"
	if p.Diff != want {
		t.Errorf("diff:\n%s\nwant:\n%s", p.Diff, want)
	}
}

func TestInvalidYAMLIsSkipped(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	_ = os.MkdirAll(wf, 0o755)
	_ = os.WriteFile(filepath.Join(wf, "bad.yml"), []byte("jobs:\n  x: [unclosed\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wf, "good.yml"), []byte("jobs:\n  x:\n    runs-on: ubuntu-latest\n"), 0o644)
	p, err := Make(Options{Root: dir, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || len(p.Skipped) != 1 || p.Skipped[0].Reason != ReasonUnparseable {
		t.Errorf("changes %+v skipped %+v", p.Changes, p.Skipped)
	}
}

func TestNoWorkflows(t *testing.T) {
	_, err := Make(Options{Root: t.TempDir(), Label: label})
	if _, ok := err.(ErrNoWorkflows); !ok {
		t.Errorf("err = %v", err)
	}
}

func TestClassify(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu-latest": "switch", "ubuntu-24.04": "switch", "Ubuntu-Latest": "switch",
		"ubuntu-22.04": ReasonOtherUbuntu, "ubuntu-20.04": ReasonOtherUbuntu,
		"ubuntu-24.04-arm": ReasonNotLinuxX64, "ubuntu-22.04-arm": ReasonNotLinuxX64,
		"windows-latest": ReasonNotLinuxX64, "macos-15": ReasonNotLinuxX64,
		"self-hosted": ReasonSelfHosted, "vitko-ubuntu-24.04": "already",
		"ubuntu-latest-4-cores": ReasonUnknownLabel, "gpu-box": ReasonUnknownLabel,
	} {
		if got := classify(in); got != want {
			t.Errorf("classify(%q) = %q, want %q", in, got, want)
		}
	}
}
