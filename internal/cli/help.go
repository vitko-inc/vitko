package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vitko-inc/vitko/internal/version"
)

// HelpDoc is vitko.help/v1: the command tree, generated from the registry.
type HelpDoc struct {
	Schema      string           `json:"schema"`
	CLI         HelpCLI          `json:"cli"`
	Scope       string           `json:"scope"`
	Output      HelpOutput       `json:"output"`
	ExitCodes   any              `json:"exit_codes"`
	ErrorCodes  []map[string]any `json:"error_codes"`
	GlobalFlags []Flag           `json:"global_flags"`
	Commands    []CommandDoc     `json:"commands"`

	groups []Group
	path   []string
}

// HelpCLI names the program.
type HelpCLI struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// HelpOutput states the output contract.
type HelpOutput struct {
	Modes                []string `json:"modes"`
	DefaultOnTerminal    string   `json:"default_on_terminal"`
	DefaultNotOnTerminal string   `json:"default_not_on_terminal"`
	Env                  string   `json:"env"`
	ErrorsOn             string   `json:"errors_on"`
	ErrorSchema          string   `json:"error_schema"`
	Interactive          bool     `json:"interactive"`
	SchemaIDPattern      string   `json:"schema_id_pattern"`
	Notes                []string `json:"notes"`
}

// CommandDoc describes one command.
type CommandDoc struct {
	Path         string   `json:"path"`
	Summary      string   `json:"summary"`
	Description  string   `json:"description,omitempty"`
	Args         []Arg    `json:"args"`
	Flags        []Flag   `json:"flags"`
	Mutates      bool     `json:"mutates"`
	Idempotent   bool     `json:"idempotent"`
	DryRun       bool     `json:"dry_run"`
	Network      bool     `json:"network"`
	Scopes       []string `json:"scopes"`
	OutputSchema string   `json:"output_schema,omitempty"`
	Errors       []string `json:"errors"`
	Examples     []string `json:"examples"`
	Plugin       bool     `json:"plugin"`
}

func helpCommand(a *App) *Command {
	if c := a.find([]string{"help"}); c != nil {
		return c
	}
	return &Command{Path: []string{"help"}, Output: "vitko.help/v1", Run: func(*Ctx) (any, error) { return nil, nil }}
}

// HelpFor runs `vitko help [<command>...]`.
func HelpFor(ctx *Ctx) (any, error) {
	a := ctx.App
	path := ctx.Args
	if len(path) > 0 && a.find(path) == nil && !a.isGroup(path) {
		return nil, Errf("unknown_command", "%q is not a vitko command.", strings.Join(path, " ")).
			WithHint("Run `vitko help` to list commands.")
	}
	return a.helpDoc(path), nil
}

func (a *App) helpDoc(path []string) *HelpDoc {
	d := &HelpDoc{
		Schema: "vitko.help/v1",
		CLI:    HelpCLI{Name: "vitko", Version: version.Version},
		Scope:  strings.Join(path, " "),
		Output: HelpOutput{
			Modes:                []string{"text", "json", "ndjson"},
			DefaultOnTerminal:    "text",
			DefaultNotOnTerminal: "json",
			Env:                  "VITKO_OUTPUT",
			ErrorsOn:             "stderr",
			ErrorSchema:          "vitko.error/v1",
			Interactive:          false,
			SchemaIDPattern:      "vitko[.<product>].<type>/v<N>",
			Notes: []string{
				"stdout carries only the result document; diagnostics and errors go to stderr.",
				"Every JSON document has a \"schema\" field. New fields may appear without a version change; a breaking change bumps the version.",
				"Enums are open: handle values you don't know.",
				"Money is in integer micros of USD (1 USD = 1,000,000 micros), with a \"currency\" field.",
				"Unknown values are null, with a *_unavailable_reason field, never 0.",
				"vitko never prompts. Missing input is a usage error (exit code 2).",
			},
		},
		ExitCodes:   ExitCodes,
		ErrorCodes:  ErrorCodeList(),
		GlobalFlags: GlobalFlags,
		Commands:    []CommandDoc{},
		groups:      a.Groups,
		path:        path,
	}
	for _, c := range a.sortedCommands(path, false) {
		d.Commands = append(d.Commands, docFor(c))
	}
	for _, p := range a.pluginsOnPath() {
		d.Commands = append(d.Commands, CommandDoc{
			Path: strings.Join(p, " "), Summary: "Helper program found on your PATH (vitko-" + strings.Join(p, "-") + ").",
			Args: []Arg{}, Flags: []Flag{}, Scopes: []string{}, Errors: []string{}, Examples: []string{}, Plugin: true,
		})
	}
	return d
}

func docFor(c *Command) CommandDoc {
	d := CommandDoc{
		Path: c.Name(), Summary: c.Summary, Description: c.Description,
		Args: c.Args, Flags: c.Flags, Mutates: c.Mutates, Idempotent: c.Idempotent,
		Network: c.Network, Scopes: c.Scopes, OutputSchema: c.Output,
		Errors: append(append([]string{}, CommonErrors...), c.Errors...), Examples: c.Examples,
	}
	for _, f := range c.Flags {
		if f.Name == "dry-run" {
			d.DryRun = true
		}
	}
	if d.Args == nil {
		d.Args = []Arg{}
	}
	if d.Flags == nil {
		d.Flags = []Flag{}
	}
	if d.Scopes == nil {
		d.Scopes = []string{}
	}
	if d.Examples == nil {
		d.Examples = []string{}
	}
	return d
}

// pluginsOnPath finds vitko-<group>-<cmd> programs for top-level groups.
func (a *App) pluginsOnPath() [][]string {
	if a.LookPath == nil || a.Getenv == nil {
		return nil
	}
	var out [][]string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(a.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			for _, g := range a.Groups {
				if len(g.Path) != 1 {
					continue
				}
				prefix := "vitko-" + g.Path[0] + "-"
				if !strings.HasPrefix(name, prefix) || seen[name] {
					continue
				}
				sub := strings.TrimPrefix(name, prefix)
				p := append(append([]string{}, g.Path...), sub)
				if a.find(p) != nil || a.isGroup(p) {
					continue
				}
				if _, err := a.LookPath(name); err == nil {
					seen[name] = true
					out = append(out, p)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], " ") < strings.Join(out[j], " ") })
	return out
}

// HelpText renders vitko.help/v1 for a terminal.
func HelpText(_ *Ctx, w io.Writer, v any) error {
	d := v.(*HelpDoc)
	if len(d.Commands) == 1 && d.Commands[0].Path == d.Scope && d.Scope != "" {
		return commandText(w, d.Commands[0])
	}
	title := "vitko: command-line tool for Vitko."
	if d.Scope != "" {
		for _, g := range d.groups {
			if strings.Join(g.Path, " ") == d.Scope {
				title = "vitko " + d.Scope + ": " + g.Summary
			}
		}
	}
	fmt.Fprintf(w, "%s\n\nUsage:\n  vitko %s<command> [flags]\n\nCommands:\n", title, prefixed(d.Scope))
	width := 0
	for _, c := range d.Commands {
		if len(c.Path) > width {
			width = len(c.Path)
		}
	}
	for _, c := range d.Commands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Path, c.Summary)
	}
	fmt.Fprintf(w, "\nGlobal flags:\n")
	flagsText(w, GlobalFlags)
	fmt.Fprintf(w, "\nOutput is text on a terminal and JSON otherwise. `vitko help --json` describes every command, flag, output schema, error code and exit code.\n")
	return nil
}

func prefixed(s string) string {
	if s == "" {
		return ""
	}
	return s + " "
}

func commandText(w io.Writer, c CommandDoc) error {
	usage := "vitko " + c.Path
	for _, a := range c.Args {
		n := "<" + a.Name + ">"
		if a.Variadic {
			n += "..."
		}
		if !a.Required {
			n = "[" + n + "]"
		}
		usage += " " + n
	}
	if len(c.Flags) > 0 {
		usage += " [flags]"
	}
	fmt.Fprintf(w, "%s\n\nUsage:\n  %s\n", c.Summary, usage)
	if c.Description != "" {
		fmt.Fprintf(w, "\n%s\n", wrap(c.Description, 78))
	}
	if len(c.Args) > 0 {
		fmt.Fprintf(w, "\nArguments:\n")
		for _, a := range c.Args {
			fmt.Fprintf(w, "  %-22s %s\n", "<"+a.Name+">", a.Summary)
		}
	}
	if len(c.Flags) > 0 {
		fmt.Fprintf(w, "\nFlags:\n")
		flagsText(w, c.Flags)
	}
	fmt.Fprintf(w, "\nGlobal flags:\n")
	flagsText(w, GlobalFlags)
	if len(c.Examples) > 0 {
		fmt.Fprintf(w, "\nExamples:\n")
		for _, e := range c.Examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
	if c.OutputSchema != "" {
		fmt.Fprintf(w, "\nJSON output: %s (see `vitko schema show %s`).\n", c.OutputSchema, c.OutputSchema)
	}
	return nil
}

func flagsText(w io.Writer, flags []Flag) {
	for _, f := range flags {
		name := "--" + f.Name
		if f.Type != TypeBool {
			ph := f.Placeholder
			if ph == "" {
				ph = strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
			}
			name += " " + ph
		}
		s := f.Summary
		if len(f.Enum) > 0 {
			s += " One of: " + strings.Join(f.Enum, ", ") + "."
		}
		if f.Default != nil && f.Default != false && f.Default != "" {
			s += fmt.Sprintf(" Default: %v.", f.Default)
		}
		if f.Env != "" {
			s += " Env: " + f.Env + "."
		}
		fmt.Fprintf(w, "  %-22s %s\n", name, s)
	}
}

func wrap(s string, width int) string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			if line != "" && len(line)+1+len(word) > width {
				out = append(out, line)
				line = word
				continue
			}
			if line == "" {
				line = word
			} else {
				line += " " + word
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
