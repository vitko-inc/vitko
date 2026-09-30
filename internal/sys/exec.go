// Package sys runs external programs (git, gh) on the user's behalf.
package sys

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Exec runs a program in dir and returns its stdout. Tests replace it.
type Exec func(dir, name string, args ...string) (string, error)

// ExecError carries a failed program's stderr.
type ExecError struct {
	Cmd    string
	Stderr string
	Code   int
}

func (e *ExecError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Cmd + ": " + msg
}

// RealExec runs programs for real.
func RealExec(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	c.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1")
	if err := c.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return out.String(), &ExecError{Cmd: name + " " + firstArgs(args), Stderr: errb.String(), Code: code}
	}
	return out.String(), nil
}

func firstArgs(a []string) string {
	if len(a) > 2 {
		a = a[:2]
	}
	return strings.Join(a, " ")
}
