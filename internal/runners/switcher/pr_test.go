package switcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitko-inc/vitko/internal/sys"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := sys.RealExec(dir, "git", args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// fakeGH answers gh calls and passes git through to the real git.
type fakeGH struct {
	body    string
	created []string
}

func (f *fakeGH) exec(dir, name string, args ...string) (string, error) {
	if name != "gh" {
		return sys.RealExec(dir, name, args...)
	}
	switch strings.Join(args[:2], " ") {
	case "auth status":
		return "", nil
	case "repo view":
		return "main\n", nil
	case "pr create":
		f.created = args
		for i, a := range args {
			if a == "--body-file" {
				b, _ := os.ReadFile(args[i+1])
				f.body = string(b)
			}
		}
		return "https://github.com/example/app/pull/7\n", nil
	}
	return "", errors.New("unexpected gh call")
}

func setupRepo(t *testing.T) (origin, clone string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_CONFIG_GLOBAL": filepath.Join(t.TempDir(), "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	base := t.TempDir()
	origin = filepath.Join(base, "origin.git")
	clone = filepath.Join(base, "clone")
	git(t, base, "init", "--quiet", "--bare", "-b", "main", origin)
	git(t, base, "clone", "--quiet", origin, clone)
	wf := filepath.Join(clone, ".github", "workflows")
	_ = os.MkdirAll(wf, 0o755)
	_ = os.WriteFile(filepath.Join(wf, "ci.yml"), []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n  mac:\n    runs-on: macos-latest\n    steps:\n      - run: true\n"), 0o644)
	git(t, clone, "add", ".")
	git(t, clone, "commit", "--quiet", "-m", "init")
	git(t, clone, "push", "--quiet", "origin", "HEAD:main")
	return origin, clone
}

func TestOpenPR(t *testing.T) {
	origin, clone := setupRepo(t)
	gh := &fakeGH{}
	res, err := OpenPR(PROptions{Options: Options{Root: clone, Label: label}, Branch: "vitko/switch-runners"}, gh.exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://github.com/example/app/pull/7" || res.Base != "main" {
		t.Errorf("res = %+v", res)
	}
	pushed := git(t, origin, "show", "vitko/switch-runners:.github/workflows/ci.yml")
	if !strings.Contains(pushed, "runs-on: vitko-ubuntu-24.04") || !strings.Contains(pushed, "runs-on: macos-latest") {
		t.Errorf("pushed file:\n%s", pushed)
	}
	msg := git(t, origin, "log", "-1", "--format=%B", "vitko/switch-runners")
	if !strings.HasPrefix(msg, "Run CI jobs on Vitko Runners") {
		t.Errorf("commit message: %q", msg)
	}
	local, _ := os.ReadFile(filepath.Join(clone, ".github/workflows/ci.yml"))
	if strings.Contains(string(local), "vitko") {
		t.Error("the caller's working copy was modified")
	}
	if head := strings.TrimSpace(git(t, clone, "rev-parse", "--abbrev-ref", "HEAD")); head != "main" {
		t.Errorf("caller's branch changed to %s", head)
	}
	if wts := git(t, clone, "worktree", "list"); strings.Count(wts, "\n") != 1 {
		t.Errorf("temporary worktree left behind:\n%s", wts)
	}
	joined := strings.Join(gh.created, " ")
	if !strings.Contains(joined, "--base main") || !strings.Contains(joined, "--head vitko/switch-runners") {
		t.Errorf("gh pr create args: %v", gh.created)
	}
	for _, want := range []string{"`test`", "To undo", "macos-latest"} {
		if !strings.Contains(gh.body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, gh.body)
		}
	}

	// A second run finds the branch and refuses.
	_, err = OpenPR(PROptions{Options: Options{Root: clone, Label: label}, Branch: "vitko/switch-runners"}, gh.exec)
	var pe *PRError
	if !errors.As(err, &pe) || pe.Step != StepBranch {
		t.Errorf("second run: %v", err)
	}
}

func TestOpenPRDryRunPushesNothing(t *testing.T) {
	origin, clone := setupRepo(t)
	gh := &fakeGH{}
	res, err := OpenPR(PROptions{Options: Options{Root: clone, Label: label}, Branch: "vitko/switch-runners", DryRun: true}, gh.exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "" || len(res.Plan.Changes) != 1 || !strings.Contains(res.Plan.Diff, "+    runs-on: vitko-ubuntu-24.04") {
		t.Errorf("res = %+v", res)
	}
	if out := git(t, origin, "branch", "--list"); strings.Contains(out, "vitko/") {
		t.Errorf("dry run pushed a branch: %s", out)
	}
	if gh.created != nil {
		t.Error("dry run opened a pull request")
	}
}

func TestOpenPRNothingToDo(t *testing.T) {
	_, clone := setupRepo(t)
	gh := &fakeGH{}
	if _, err := OpenPR(PROptions{Options: Options{Root: clone, Label: label, Jobs: []string{"mac"}}, Branch: "b"}, gh.exec); err != nil {
		t.Fatal(err)
	}
	if gh.created != nil {
		t.Error("opened a pull request with no changes")
	}
}

func TestOpenPRNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	gh := &fakeGH{}
	_, err := OpenPR(PROptions{Options: Options{Root: t.TempDir(), Label: label}, Branch: "b"}, gh.exec)
	var pe *PRError
	if !errors.As(err, &pe) || pe.Step != StepRepo {
		t.Errorf("err = %v", err)
	}
}
