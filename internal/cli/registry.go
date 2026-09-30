// Package cli is the command framework: one registry of commands drives
// argument parsing, the output contract, text help and `vitko help --json`.
// The command list itself lives in internal/commands.
package cli

import (
	"io"
	"strings"
)

// FlagType is the JSON-facing type of a flag value.
type FlagType string

const (
	TypeString  FlagType = "string"
	TypeBool    FlagType = "boolean"
	TypeInt     FlagType = "integer"
	TypeNumber  FlagType = "number"
	TypeStrings FlagType = "string[]" // repeatable; each value may also be comma-separated
)

// Flag declares one --flag.
type Flag struct {
	Name    string   `json:"name"`
	Type    FlagType `json:"type"`
	Enum    []string `json:"enum,omitempty"`
	Default any      `json:"default,omitempty"`
	Env     string   `json:"env,omitempty"`
	Summary string   `json:"summary"`
	// Placeholder is the value name shown in text help (for example FILE).
	Placeholder string `json:"-"`
}

// Arg declares one positional argument.
type Arg struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
}

// Command is one leaf command. Groups (for example `vitko runners`) are
// implied by the paths of their commands.
type Command struct {
	Path        []string
	Summary     string
	Description string
	Args        []Arg
	Flags       []Flag
	// Mutates is true when the command changes something (files, remote state).
	Mutates bool
	// Idempotent is true when repeating the command with the same input is safe
	// and leads to the same result.
	Idempotent bool
	// Network is true when the command may use the network.
	Network bool
	// Scopes the command needs on a Vitko credential (none in this release).
	Scopes []string
	// Output is the schema id of the document on stdout.
	Output string
	// Errors lists the error codes the command may return, besides the
	// codes every command may return (usage, internal).
	Errors   []string
	Examples []string
	// Hidden commands work but are not listed (aliases).
	Hidden bool
	// Plugin commands hand every argument to an external program.
	Plugin string
	// Run does the work and returns the output document.
	Run func(*Ctx) (any, error)
	// Text renders the document for a terminal. Nil means JSON.
	Text func(*Ctx, io.Writer, any) error
}

// Name is the path joined by spaces.
func (c *Command) Name() string { return strings.Join(c.Path, " ") }

// Group describes a path prefix (for text help).
type Group struct {
	Path    []string
	Summary string
}

// GlobalFlags apply to every command.
var GlobalFlags = []Flag{
	{Name: "output", Type: TypeString, Enum: []string{"text", "json", "ndjson"}, Env: "VITKO_OUTPUT", Placeholder: "MODE",
		Summary: "Output format. Default: text on a terminal, json otherwise."},
	{Name: "json", Type: TypeBool, Summary: "Same as --output json."},
	{Name: "fields", Type: TypeStrings, Placeholder: "LIST",
		Summary: "Keep only these fields in JSON output (comma-separated, dotted paths such as months.month). Implies --output json."},
	{Name: "help", Type: TypeBool, Summary: "Show help for the command. With --json, as JSON."},
}

// CommonErrors may be returned by any command.
var CommonErrors = []string{"usage", "internal"}
