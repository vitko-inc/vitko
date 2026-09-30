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
	ExitLimit          = 7
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
	{ExitConflict, "Conflict with the current state, for example a branch that already exists or a stale revision."},
	{ExitLimit, "A spend or concurrency limit, or its ceiling, stopped it."},
	{ExitUnavailable, "Temporarily unavailable or rate limited. Safe to retry."},
	{ExitChangesPending, "--check: changes would be made."},
}

// Codes 9 and 11-99 are reserved for future use.

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
	// From the Vitko API (the API may add codes; the exit code then follows
	// the HTTP status).
	"signed_out":             {ExitSignedOut, "Not signed in to Vitko."},
	"token_expired":          {ExitSignedOut, "The sign-in or token has expired."},
	"token_invalid":          {ExitSignedOut, "The token isn't valid (revoked, expired or mistyped)."},
	"forbidden":              {ExitForbidden, "Not allowed."},
	"scope_missing":          {ExitForbidden, "The credential lacks the scope this needs."},
	"human_session_required": {ExitForbidden, "Only an organization admin, signed in, can do this."},
	"fresh_sign_in_required": {ExitForbidden, "This needs a sign-in from the last 12 hours."},
	"not_found":              {ExitNotFound, "Not found, or not visible to you."},
	"revision_conflict":      {ExitConflict, "The resource changed since the revision given."},
	"limit_above_ceiling":    {ExitLimit, "The value is above the organization's ceiling."},
	"ceiling_not_set":        {ExitLimit, "No ceiling is set, so tokens can only lower the limit."},
	"access_denied":          {ExitForbidden, "The sign-in was denied in the browser."},
	"expired_token":          {ExitSignedOut, "The sign-in code expired before it was approved."},
	"unavailable":            {ExitUnavailable, "The Vitko API is unavailable. Safe to retry."},
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
	// exit overrides the catalog's exit code (API errors with new codes).
	exit              int
	Code              string `json:"code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds *int   `json:"retry_after_seconds,omitempty"`
	Hint              string `json:"hint,omitempty"`
	Fix               *Fix   `json:"fix,omitempty"`
	Docs              string `json:"docs,omitempty"`
	Details           any    `json:"details,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ExitCode is the process exit code for this error.
func (e *Error) ExitCode() int {
	if e.exit != 0 {
		return e.exit
	}
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

// APIError is an error the Vitko API returned. Its exit code comes from the
// catalog when the code is known, else from the HTTP status.
func APIError(status int, e Error) *Error {
	out := e
	if _, known := errorCodes[out.Code]; !known || out.Code == "" {
		out.exit = ExitForStatus(status)
		if out.Code == "" {
			out.Code = "unavailable"
		}
	}
	return &out
}

// ExitForStatus maps an HTTP status to an exit code.
func ExitForStatus(status int) int {
	switch {
	case status == 400:
		return ExitUsage
	case status == 401:
		return ExitSignedOut
	case status == 403:
		return ExitForbidden
	case status == 404:
		return ExitNotFound
	case status == 409 || status == 412:
		return ExitConflict
	case status == 422:
		return ExitLimit
	case status == 429 || status == 502 || status == 503 || status == 504:
		return ExitUnavailable
	}
	return ExitInternal
}

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
