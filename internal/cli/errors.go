package cli

import (
	"errors"
	"fmt"
	"sort"

	"github.com/vitko-inc/vitko/internal/jsonx"
)

// Exit codes. They are part of the public contract: a change here is a
// major version.
const (
	ExitOK             = 0
	ExitInternal       = 1
	ExitUsage          = 2
	ExitSignedOut      = 3
	ExitForbidden      = 4
	ExitNotFound       = 5
	ExitConflict       = 6
	ExitUnavailable    = 8
	ExitChangesPending = 10
)

// ExitCodes documents every exit code, in order, for help --json.
var ExitCodes = []struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}{
	{ExitOK, "Success."},
	{ExitInternal, "Unexpected failure. Please report it."},
	{ExitUsage, "Usage: a bad flag or argument, missing input, or a required tool is missing."},
	{ExitSignedOut, "Not signed in, or the sign-in expired."},
	{ExitForbidden, "Signed in but not allowed."},
	{ExitNotFound, "Not found, or not visible to you."},
	{ExitConflict, "Conflict with the current state, for example a branch that already exists."},
	{ExitUnavailable, "Temporarily unavailable or rate limited. Safe to retry."},
	{ExitChangesPending, "--check: changes would be made."},
}

// Codes 7, 9 and 11-99 are reserved for future use.

// errorCodes maps every stable error code to its exit code and meaning.
var errorCodes = map[string]struct {
	Exit    int
	Meaning string
}{
	"internal":             {ExitInternal, "An unexpected failure."},
	"usage":                {ExitUsage, "A flag or argument is wrong."},
	"unknown_command":      {ExitUsage, "No such command."},
	"input_required":       {ExitUsage, "A required argument or flag is missing."},
	"invalid_input":        {ExitUsage, "An input file or value couldn't be read."},
	"tool_missing":         {ExitUsage, "A program this command needs (git or gh) isn't installed."},
	"github_signed_out":    {ExitSignedOut, "The GitHub CLI (gh) isn't signed in."},
	"github_forbidden":     {ExitForbidden, "GitHub refused the request for this account."},
	"file_not_found":       {ExitNotFound, "A file or directory doesn't exist."},
	"github_not_found":     {ExitNotFound, "GitHub has no such organization or repository, or it isn't visible to you."},
	"schema_not_found":     {ExitNotFound, "No schema has that id."},
	"plugin_not_installed": {ExitNotFound, "The helper program for this command isn't installed here."},
	"not_a_git_repository": {ExitNotFound, "The path isn't inside a git repository."},
	"branch_exists":        {ExitConflict, "The branch for the pull request already exists."},
	"github_unavailable":   {ExitUnavailable, "GitHub didn't answer. Safe to retry."},
}

// ErrorCodeList returns the catalog sorted by code, for help --json.
func ErrorCodeList() []map[string]any {
	keys := make([]string, 0, len(errorCodes))
	for k := range errorCodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"code": k, "exit_code": errorCodes[k].Exit, "meaning": errorCodes[k].Meaning})
	}
	return out
}

// Fix says how to resolve an error: a command an agent may run, or a page a
// person must visit.
type Fix struct {
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Error is the one error shape (vitko.error/v1).
type Error struct {
	Code              string         `json:"code"`
	Message           string         `json:"message"`
	Retryable         bool           `json:"retryable"`
	RetryAfterSeconds *int           `json:"retry_after_seconds,omitempty"`
	Hint              string         `json:"hint,omitempty"`
	Fix               *Fix           `json:"fix,omitempty"`
	Docs              string         `json:"docs,omitempty"`
	Details           map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ExitCode is the process exit code for this error.
func (e *Error) ExitCode() int {
	if c, ok := errorCodes[e.Code]; ok {
		return c.Exit
	}
	return ExitInternal
}

// Errf builds an Error with a code and a formatted message.
func Errf(code, format string, a ...any) *Error {
	if _, ok := errorCodes[code]; !ok {
		panic("cli: unregistered error code " + code)
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, a...), Retryable: errorCodes[code].Exit == ExitUnavailable}
}

// WithHint sets the hint and returns e.
func (e *Error) WithHint(h string) *Error { e.Hint = h; return e }

// WithFix sets a fix command and returns e.
func (e *Error) WithFix(cmd string) *Error { e.Fix = &Fix{Command: cmd}; return e }

// WithDetails sets details and returns e.
func (e *Error) WithDetails(d map[string]any) *Error { e.Details = d; return e }

// AsError converts any error into an *Error (unknown errors become "internal").
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: "internal", Message: err.Error(), Hint: "This is unexpected. Please report it at https://github.com/vitko-inc/vitko/issues with the command you ran."}
}

// ErrorDoc wraps an error in its document.
func ErrorDoc(e *Error) jsonx.Object {
	return jsonx.Object{{Key: "schema", Value: "vitko.error/v1"}, {Key: "error", Value: e}}
}

// ChangesPending is returned (with a document) by --check when changes would
// be made. It is not an error document; it only sets the exit code.
type ChangesPending struct{}

func (ChangesPending) Error() string { return "changes would be made" }
