package switcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vitko-inc/vitko/internal/sys"
)

// PROptions configure a pull request.
type PROptions struct {
	Options
	Base   string
	Branch string
	DryRun bool
}

// PRStep names where a pull request attempt failed, so the caller can map
// it to an error code.
type PRStep string

const (
	StepRepo   PRStep = "repo"
	StepAuth   PRStep = "auth"
	StepBase   PRStep = "base"
	StepBranch PRStep = "branch"
	StepCommit PRStep = "commit"
	StepPush   PRStep = "push"
	StepCreate PRStep = "create"
)

// PRError is a failure at a step of the pull request flow.
type PRError struct {
	Step PRStep
	Err  error
}

func (e *PRError) Error() string { return string(e.Step) + ": " + e.Err.Error() }
func (e *PRError) Unwrap() error { return e.Err }

// PRResult is the outcome of OpenPR.
type PRResult struct {
	Plan   *Plan
	URL    string
	Base   string
	Branch string
}

// OpenPR plans the switch on a clean copy of the default branch (a temporary
// git worktree, so the caller's checkout is never touched), then commits,
// pushes and opens a pull request with the caller's own git and gh
// credentials. With DryRun it stops after planning.
func OpenPR(o PROptions, run sys.Exec) (*PRResult, error) {
	fail := func(step PRStep, err error) (*PRResult, error) { return nil, &PRError{Step: step, Err: err} }
	top, err := run(o.Root, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return fail(StepRepo, err)
	}
	top = strings.TrimSpace(top)
	if _, err := run(top, "gh", "auth", "status"); err != nil {
		return fail(StepAuth, err)
	}
	base := o.Base
	if base == "" {
		out, err := run(top, "gh", "repo", "view", "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
		if err != nil {
			return fail(StepBase, err)
		}
		base = strings.TrimSpace(out)
	}
	if _, err := run(top, "git", "fetch", "--quiet", "origin", base); err != nil {
		return fail(StepBase, err)
	}
	if !o.DryRun {
		if out, err := run(top, "git", "ls-remote", "--heads", "origin", o.Branch); err != nil {
			return fail(StepPush, err)
		} else if strings.TrimSpace(out) != "" {
			return fail(StepBranch, fmt.Errorf("branch %s already exists on origin", o.Branch))
		}
		if _, err := run(top, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+o.Branch); err == nil {
			return fail(StepBranch, fmt.Errorf("branch %s already exists locally", o.Branch))
		}
	}
	tmp, err := os.MkdirTemp("", "vitko-switch-")
	if err != nil {
		return fail(StepRepo, err)
	}
	wt := filepath.Join(tmp, "repo")
	defer func() {
		_, _ = run(top, "git", "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(tmp)
	}()
	if _, err := run(top, "git", "worktree", "add", "--quiet", "--detach", wt, "origin/"+base); err != nil {
		return fail(StepRepo, err)
	}
	po := o.Options
	po.Root = wt
	plan, err := Make(po)
	if err != nil {
		return nil, err
	}
	res := &PRResult{Plan: plan, Base: base, Branch: o.Branch}
	if o.DryRun || len(plan.NewFiles) == 0 {
		return res, nil
	}
	if err := plan.Apply(); err != nil {
		return fail(StepCommit, err)
	}
	if _, err := run(wt, "git", "switch", "--quiet", "-c", o.Branch); err != nil {
		return fail(StepBranch, err)
	}
	args := append([]string{"add", "--"}, plan.ChangedFiles()...)
	if _, err := run(wt, "git", args...); err != nil {
		return fail(StepCommit, err)
	}
	if _, err := run(wt, "git", "commit", "--quiet", "-m", commitMessage(plan, o.Label)); err != nil {
		return fail(StepCommit, err)
	}
	if _, err := run(wt, "git", "push", "--quiet", "-u", "origin", o.Branch); err != nil {
		return fail(StepPush, err)
	}
	bodyFile := filepath.Join(tmp, "body.md")
	if err := os.WriteFile(bodyFile, []byte(PRBody(plan, o.Label)), 0o600); err != nil {
		return fail(StepCreate, err)
	}
	out, err := run(wt, "gh", "pr", "create", "--base", base, "--head", o.Branch,
		"--title", "Run CI jobs on Vitko Runners", "--body-file", bodyFile)
	if err != nil {
		return fail(StepCreate, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	res.URL = strings.TrimSpace(lines[len(lines)-1])
	return res, nil
}

func commitMessage(p *Plan, label string) string {
	return fmt.Sprintf("Run CI jobs on Vitko Runners\n\nChange runs-on to %s for %d job(s).\nTo undo, revert this commit.\n", label, len(p.Changes))
}

// PRBody explains the change and how to undo it.
func PRBody(p *Plan, label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This changes `runs-on` to `%s` for %d job(s), so they run on Vitko Runners. Nothing else in the workflows changes.\n\n", label, len(p.Changes))
	b.WriteString("| File | Job | Line | Before | After |\n|---|---|---|---|---|\n")
	for _, c := range p.Changes {
		fmt.Fprintf(&b, "| `%s` | `%s` | %d | `%s` | `%s` |\n", c.File, c.Job, c.Line, c.From, c.To)
	}
	if len(p.Skipped) > 0 {
		b.WriteString("\n**Left as they are:**\n\n")
		for _, s := range p.Skipped {
			ro := s.RunsOn
			if ro == "" {
				ro = "-"
			}
			fmt.Fprintf(&b, "- `%s` job `%s` (`%s`): %s\n", s.File, s.Job, ro, s.Hint)
		}
	}
	b.WriteString("\n**Before merging:** make sure Vitko Runners is set up for this repository (https://runners.vitko.inc). Until it is, these jobs wait for a runner.\n")
	b.WriteString("\n**To undo:** revert this pull request, or set `runs-on` back to the values in the Before column.\n")
	b.WriteString("\nOpened with `vitko runners repos switch --pr`.\n")
	return b.String()
}
