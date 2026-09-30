package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/vitko-inc/vitko/internal/jsonx"
	"github.com/vitko-inc/vitko/schemas"
)

// App is the CLI: the registry plus its environment. Tests build one with
// buffers in place of the real streams.
type App struct {
	Commands []*Command
	Groups   []Group

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// StdoutIsTerminal decides the default output mode.
	StdoutIsTerminal bool
	// DefaultOutput is the configured output mode ("" when not configured).
	DefaultOutput func() string
	LookPath      func(string) (string, error)
}

// Ctx is what a command's Run and Text see.
type Ctx struct {
	App     *App
	Cmd     *Command
	Args    []string
	Mode    string
	values  map[string]any
	explict map[string]bool
}

// String returns a string flag.
func (c *Ctx) String(name string) string { s, _ := c.values[name].(string); return s }

// Bool returns a boolean flag.
func (c *Ctx) Bool(name string) bool { b, _ := c.values[name].(bool); return b }

// Int returns an integer flag.
func (c *Ctx) Int(name string) int { i, _ := c.values[name].(int); return i }

// Number returns a number flag.
func (c *Ctx) Number(name string) float64 { f, _ := c.values[name].(float64); return f }

// Strings returns a repeatable flag's values.
func (c *Ctx) Strings(name string) []string { s, _ := c.values[name].([]string); return s }

// IsSet reports whether the flag was given on the command line or by its env var.
func (c *Ctx) IsSet(name string) bool { return c.explict[name] }

// Stderr is where diagnostics go.
func (c *Ctx) Stderr() io.Writer { return c.App.Stderr }

// Run parses args (without the program name), runs the command and returns
// the process exit code.
func (a *App) Run(args []string) int {
	cmd, group, rest, pre, err := a.resolve(args)
	mode := a.modeFromArgs(append(append([]string{}, pre...), rest...))
	if err != nil {
		return a.fail(mode, err)
	}
	if cmd != nil && cmd.Plugin != "" {
		return a.runPlugin(mode, cmd, rest)
	}
	if cmd == nil {
		// A group: `vitko runners` prints its help.
		ctx, perr := a.parse(helpCommand(a), append(pre, rest...))
		if perr != nil {
			return a.fail(mode, perr)
		}
		return a.emit(ctx, a.helpDoc(group))
	}
	ctx, perr := a.parse(cmd, append(pre, rest...))
	if perr != nil {
		return a.fail(mode, perr)
	}
	if ctx.Bool("help") {
		ctx.Cmd = helpCommand(a)
		return a.emit(ctx, a.helpDoc(cmd.Path))
	}
	doc, runErr := cmd.Run(ctx)
	var pending ChangesPending
	if runErr != nil && errors.As(runErr, &pending) {
		if code := a.emit(ctx, doc); code != ExitOK {
			return code
		}
		return ExitChangesPending
	}
	if runErr != nil {
		return a.fail(ctx.Mode, runErr)
	}
	return a.emit(ctx, doc)
}

// isGroup reports whether path is a proper prefix of some command.
func (a *App) isGroup(path []string) bool {
	for _, c := range a.Commands {
		if len(c.Path) > len(path) && hasPrefix(c.Path, path) {
			return true
		}
	}
	return false
}

func (a *App) find(path []string) *Command {
	for _, c := range a.Commands {
		if equal(c.Path, path) {
			return c
		}
	}
	return nil
}

// resolve finds the command. It returns the command (nil for a bare group),
// the group path, the tokens after the command path, and flag tokens seen
// before the path was complete.
func (a *App) resolve(args []string) (*Command, []string, []string, []string, error) {
	var path, pre []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			return nil, path, args[i:], pre, nil
		}
		if strings.HasPrefix(tok, "-") && tok != "-" {
			pre = append(pre, tok)
			// A global flag's value may follow as its own token.
			if name, _, hasEq := strings.Cut(strings.TrimLeft(tok, "-"), "="); !hasEq {
				if f := findFlag(GlobalFlags, name); f != nil && f.Type != TypeBool && i+1 < len(args) {
					i++
					pre = append(pre, args[i])
				}
			}
			continue
		}
		next := append(append([]string{}, path...), tok)
		if c := a.find(next); c != nil {
			return c, next, args[i+1:], pre, nil
		}
		if a.isGroup(next) {
			path = next
			continue
		}
		if len(path) == 1 && path[0] != "help" {
			if bin := "vitko-" + path[0] + "-" + tok; a.lookPath(bin) != "" {
				return &Command{Path: next, Plugin: bin}, next, args[i+1:], pre, nil
			}
		}
		e := Errf("unknown_command", "%q is not a vitko command.", strings.Join(next, " "))
		hint := "Run `vitko help` to list commands."
		if len(path) > 0 {
			hint = fmt.Sprintf("Run `vitko %s --help` to list its commands.", strings.Join(path, " "))
		}
		return nil, nil, nil, pre, e.WithHint(hint)
	}
	if len(path) == 0 {
		return nil, nil, nil, pre, nil
	}
	return nil, path, nil, pre, nil
}

func (a *App) lookPath(bin string) string {
	if a.LookPath == nil {
		return ""
	}
	p, err := a.LookPath(bin)
	if err != nil {
		return ""
	}
	return p
}

// runPlugin hands all arguments to an external program with the same
// output contract.
func (a *App) runPlugin(mode string, cmd *Command, args []string) int {
	path := a.lookPath(cmd.Plugin)
	if path == "" {
		e := Errf("plugin_not_installed", "`vitko %s` needs the %s helper, which isn't installed here.", cmd.Name(), cmd.Plugin)
		e.Hint = "This helper comes preinstalled on Vitko runners. Run the command inside a job on a Vitko runner."
		return a.fail(mode, e)
	}
	c := exec.Command(path, args...)
	c.Stdin, c.Stdout, c.Stderr = a.Stdin, a.Stdout, a.Stderr
	c.Env = append(os.Environ(), "VITKO_OUTPUT="+mode)
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return a.fail(mode, Errf("internal", "Couldn't run %s: %v", cmd.Plugin, err))
	}
	return ExitOK
}

// modeFromArgs finds the output mode before full parsing, so that usage
// errors are printed in the caller's format.
func (a *App) modeFromArgs(args []string) string {
	mode := ""
	for i, t := range args {
		if t == "--" {
			break
		}
		switch {
		case t == "--json":
			mode = "json"
		case strings.HasPrefix(t, "--output="):
			mode = strings.TrimPrefix(t, "--output=")
		case t == "--output" && i+1 < len(args):
			mode = args[i+1]
		case t == "--fields" || strings.HasPrefix(t, "--fields="):
			if mode == "" {
				mode = "json"
			}
		}
	}
	if validMode(mode) {
		return mode
	}
	return a.defaultMode()
}

func validMode(m string) bool { return m == "text" || m == "json" || m == "ndjson" }

func (a *App) defaultMode() string {
	if m := a.Getenv("VITKO_OUTPUT"); validMode(m) {
		return m
	}
	if a.DefaultOutput != nil {
		if m := a.DefaultOutput(); validMode(m) {
			return m
		}
	}
	if a.StdoutIsTerminal {
		return "text"
	}
	return "json"
}

func findFlag(flags []Flag, name string) *Flag {
	for i := range flags {
		if flags[i].Name == name {
			return &flags[i]
		}
	}
	return nil
}

// parse reads flags and positional args for cmd.
func (a *App) parse(cmd *Command, toks []string) (*Ctx, error) {
	ctx := &Ctx{App: a, Cmd: cmd, values: map[string]any{}, explict: map[string]bool{}}
	all := append(append([]Flag{}, cmd.Flags...), GlobalFlags...)
	for _, f := range all {
		if f.Default != nil {
			ctx.values[f.Name] = f.Default
		}
		if f.Env != "" {
			if v := a.Getenv(f.Env); v != "" {
				if err := setFlag(ctx, &f, v, true); err != nil {
					return nil, err.(*Error).WithHint(fmt.Sprintf("Check the %s environment variable.", f.Env))
				}
				ctx.explict[f.Name] = true
			}
		}
	}
	fromFlags := map[string]bool{}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "--" {
			ctx.Args = append(ctx.Args, toks[i+1:]...)
			break
		}
		if !strings.HasPrefix(t, "-") || t == "-" {
			ctx.Args = append(ctx.Args, t)
			continue
		}
		if t == "-h" {
			t = "--help"
		}
		if !strings.HasPrefix(t, "--") {
			return nil, Errf("usage", "Unknown flag %q. Flags are spelled with two dashes.", t).
				WithHint(fmt.Sprintf("Run `vitko %s --help` for its flags.", cmd.Name()))
		}
		name, val, hasVal := strings.Cut(t[2:], "=")
		f := findFlag(all, name)
		if f == nil {
			return nil, Errf("usage", "Unknown flag --%s for `vitko %s`.", name, cmd.Name()).
				WithHint(fmt.Sprintf("Run `vitko %s --help` for its flags.", cmd.Name()))
		}
		if !hasVal && f.Type != TypeBool {
			if i+1 >= len(toks) {
				return nil, Errf("input_required", "--%s needs a value.", name)
			}
			i++
			val = toks[i]
		}
		if !hasVal && f.Type == TypeBool {
			val = "true"
		}
		if f.Type == TypeStrings && !fromFlags[name] {
			delete(ctx.values, name) // flags replace env/default values
		}
		fromFlags[name] = true
		if err := setFlag(ctx, f, val, false); err != nil {
			return nil, err
		}
		ctx.explict[name] = true
	}
	// Output mode.
	mode := ctx.String("output")
	if ctx.Bool("json") {
		mode = "json"
	}
	if mode == "" && len(ctx.Strings("fields")) > 0 {
		mode = "json"
	}
	if mode == "" {
		mode = a.defaultMode()
	}
	ctx.Mode = mode
	if ctx.Bool("help") {
		return ctx, nil
	}
	if err := checkArgs(cmd, ctx.Args); err != nil {
		return nil, err
	}
	if fields := ctx.Strings("fields"); len(fields) > 0 {
		for _, p := range fields {
			if !schemas.HasField(cmd.Output, p) {
				return nil, Errf("usage", "--fields: %q is not a field of %s.", p, cmd.Output).
					WithHint(fmt.Sprintf("Run `vitko schema show %s` to see the fields.", cmd.Output))
			}
		}
	}
	return ctx, nil
}

func setFlag(ctx *Ctx, f *Flag, val string, fromEnv bool) error {
	bad := func(want string) error {
		return Errf("usage", "--%s: %q is not %s.", f.Name, val, want)
	}
	switch f.Type {
	case TypeBool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return bad("true or false")
		}
		ctx.values[f.Name] = b
	case TypeInt:
		n, err := strconv.Atoi(val)
		if err != nil {
			return bad("a whole number")
		}
		ctx.values[f.Name] = n
	case TypeNumber:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return bad("a number")
		}
		ctx.values[f.Name] = n
	case TypeStrings:
		cur, _ := ctx.values[f.Name].([]string)
		for _, p := range strings.Split(val, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cur = append(cur, p)
			}
		}
		ctx.values[f.Name] = cur
	default:
		if len(f.Enum) > 0 && !contains(f.Enum, val) {
			return bad("one of " + strings.Join(f.Enum, ", "))
		}
		ctx.values[f.Name] = val
	}
	return nil
}

func checkArgs(cmd *Command, args []string) error {
	required, max, variadic := 0, len(cmd.Args), false
	for _, a := range cmd.Args {
		if a.Required {
			required++
		}
		if a.Variadic {
			variadic = true
		}
	}
	if len(args) < required {
		missing := cmd.Args[len(args)].Name
		return Errf("input_required", "`vitko %s` needs <%s>.", cmd.Name(), missing).
			WithHint(fmt.Sprintf("Run `vitko %s --help` for usage.", cmd.Name()))
	}
	if !variadic && len(args) > max {
		return Errf("usage", "`vitko %s` got an unexpected argument %q.", cmd.Name(), args[max]).
			WithHint(fmt.Sprintf("Run `vitko %s --help` for usage.", cmd.Name()))
	}
	return nil
}

// emit writes a document to stdout in the chosen mode.
func (a *App) emit(ctx *Ctx, doc any) int {
	if ctx.Mode == "text" && ctx.Cmd.Text != nil && len(ctx.Strings("fields")) == 0 {
		if err := ctx.Cmd.Text(ctx, a.Stdout, doc); err != nil {
			return a.fail("text", err)
		}
		return ExitOK
	}
	v, err := jsonx.FromValue(doc)
	if err != nil {
		return a.fail(ctx.Mode, Errf("internal", "Couldn't encode the output: %v", err))
	}
	v = jsonx.SchemaFirst(v)
	if fields := ctx.Strings("fields"); len(fields) > 0 {
		v = jsonx.Trim(v, fields)
	}
	if err := jsonx.Encode(a.Stdout, v, ctx.Mode != "ndjson"); err != nil {
		return ExitInternal
	}
	return ExitOK
}

// fail writes an error to stderr and returns its exit code.
func (a *App) fail(mode string, err error) int {
	e := AsError(err)
	if mode == "json" || mode == "ndjson" {
		_ = jsonx.Encode(a.Stderr, ErrorDoc(e), mode == "json")
		return e.ExitCode()
	}
	fmt.Fprintf(a.Stderr, "vitko: %s\n", e.Message)
	if e.Hint != "" {
		fmt.Fprintf(a.Stderr, "  hint: %s\n", e.Hint)
	}
	if e.Fix != nil && e.Fix.Command != "" {
		fmt.Fprintf(a.Stderr, "  fix:  %s\n", e.Fix.Command)
	}
	if e.Fix != nil && e.Fix.URL != "" {
		fmt.Fprintf(a.Stderr, "  see:  %s\n", e.Fix.URL)
	}
	return e.ExitCode()
}

func hasPrefix(p, prefix []string) bool {
	if len(prefix) > len(p) {
		return false
	}
	for i := range prefix {
		if p[i] != prefix[i] {
			return false
		}
	}
	return true
}

func equal(a, b []string) bool { return len(a) == len(b) && hasPrefix(a, b) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sortedCommands returns visible commands under prefix, sorted by path.
func (a *App) sortedCommands(prefix []string, includeHidden bool) []*Command {
	var out []*Command
	for _, c := range a.Commands {
		if (c.Hidden && !includeHidden) || !hasPrefix(c.Path, prefix) {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
