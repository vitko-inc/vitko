// Package commands is the one list of vitko commands. The CLI, its help,
// `vitko help --json` and the JSON Schemas are all driven from it.
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/vitko-inc/vitko/internal/cli"
	"github.com/vitko-inc/vitko/internal/config"
	"github.com/vitko-inc/vitko/internal/runners/estimate"
	"github.com/vitko-inc/vitko/internal/runners/pricing"
	"github.com/vitko-inc/vitko/internal/runners/switcher"
	"github.com/vitko-inc/vitko/internal/sys"
	"github.com/vitko-inc/vitko/internal/version"
	"github.com/vitko-inc/vitko/schemas"
)

// Env is what the commands need from the outside world. Tests replace parts.
type Env struct {
	Stdin            io.Reader
	Stdout, Stderr   io.Writer
	Getenv           func(string) string
	StdoutIsTerminal bool
	LookPath         func(string) (string, error)
	Exec             sys.Exec
	Now              func() time.Time
	// HTTP calls the Vitko API (nil: a default client).
	HTTP *http.Client
	// Sleep waits between sign-in polls.
	Sleep func(time.Duration)
	// OpenBrowser opens a link for the person at the terminal (best effort).
	OpenBrowser func(string) error
}

// DefaultEnv is the real process environment.
func DefaultEnv() Env {
	st, err := os.Stdout.Stat()
	tty := err == nil && st.Mode()&os.ModeCharDevice != 0
	return Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, StdoutIsTerminal: tty,
		LookPath: exec.LookPath, Exec: sys.RealExec, Now: time.Now,
		HTTP: &http.Client{Timeout: 60 * time.Second}, Sleep: time.Sleep, OpenBrowser: openBrowser,
	}
}

// New builds the CLI.
func New(env Env) *cli.App {
	a := &cli.App{
		Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr,
		Getenv: env.Getenv, StdoutIsTerminal: env.StdoutIsTerminal, LookPath: env.LookPath,
		DefaultOutput: func() string {
			m, err := config.Load(env.Getenv)
			if err != nil {
				return ""
			}
			return m["output"]
		},
		Groups: []cli.Group{
			{Path: []string{"runners"}, Summary: "Vitko Runners: GitHub Actions runners at a third of GitHub's price."},
			{Path: []string{"runners", "repos"}, Summary: "Work with your repositories' workflows."},
			{Path: []string{"schema"}, Summary: "The JSON Schemas of vitko's output."},
			{Path: []string{"config"}, Summary: "Your vitko settings."},
			{Path: []string{"orgs"}, Summary: "The organizations you can use."},
			{Path: []string{"tokens"}, Summary: "Organization API tokens for agents and scripts."},
			{Path: []string{"runners", "jobs"}, Summary: "Jobs that ran on Vitko runners."},
			{Path: []string{"runners", "limits"}, Summary: "Spend and concurrency limits, and their ceilings."},
			{Path: []string{"runners", "limits", "ceiling"}, Summary: "The ceilings API tokens and CI jobs can't set limits above."},
		},
	}
	a.Commands = List(env)
	return a
}

// List returns every command.
func List(env Env) []*cli.Command {
	cmds := []*cli.Command{
		{
			Path:        []string{"help"},
			Summary:     "Show help for vitko or a command.",
			Description: "Lists commands. With --json (or when output is not a terminal), prints the whole command tree as one document: every command's arguments, flags, output schema and error codes, plus the exit codes.",
			Args:        []cli.Arg{{Name: "command", Summary: "Show help for this command only, for example `runners estimate`.", Variadic: true}},
			Idempotent:  true, Output: "vitko.help/v1", Errors: []string{"unknown_command"},
			Examples: []string{"vitko help", "vitko help --json", "vitko help runners estimate --json"},
			Run:      cli.HelpFor, Text: cli.HelpText,
		},
		{
			Path: []string{"version"}, Summary: "Show the version of vitko.",
			Idempotent: true, Output: "vitko.version/v1",
			Run: func(*cli.Ctx) (any, error) { return versionDoc(), nil },
			Text: func(_ *cli.Ctx, w io.Writer, v any) error {
				d := v.(map[string]any)
				_, err := fmt.Fprintf(w, "vitko %s (%s/%s)\n", d["version"], d["os"], d["arch"])
				return err
			},
		},
		{
			Path: []string{"schema", "list"}, Summary: "List the JSON Schemas of vitko's output.",
			Idempotent: true, Output: "vitko.schema-list/v1",
			Run: func(*cli.Ctx) (any, error) {
				items := []map[string]any{}
				for _, id := range schemas.IDs() {
					items = append(items, map[string]any{"id": id, "title": schemas.Title(id), "url": schemas.BaseURL + id})
				}
				return map[string]any{"schema": "vitko.schema-list/v1", "schemas": items}, nil
			},
			Text: func(_ *cli.Ctx, w io.Writer, v any) error {
				for _, it := range v.(map[string]any)["schemas"].([]map[string]any) {
					fmt.Fprintf(w, "%-30s %s\n", it["id"], it["title"])
				}
				return nil
			},
		},
		{
			Path: []string{"schema", "show"}, Summary: "Print a JSON Schema (2020-12).",
			Description: "Prints the schema document itself, as JSON in every output mode.",
			Args:        []cli.Arg{{Name: "id", Summary: "Schema id, for example vitko.runners.estimate/v1.", Required: true}},
			Idempotent:  true, Errors: []string{"schema_not_found"},
			Examples: []string{"vitko schema show vitko.runners.estimate/v1"},
			Run: func(c *cli.Ctx) (any, error) {
				b, ok := schemas.Get(c.Args[0])
				if !ok {
					return nil, cli.Errf("schema_not_found", "There is no schema %q.", c.Args[0]).WithFix("vitko schema list")
				}
				return json.RawMessage(b), nil
			},
		},
		configList(env), configGet(env), configSet(env),
		doctorCmd(env),
		{
			Path: []string{"runners", "pricing"}, Summary: "Show the price list.",
			Description: "The public list price, how time is billed, the free minutes and the dated GitHub price it is compared with.",
			Idempotent:  true, Output: "vitko.runners.pricing/v1",
			Examples: []string{"vitko runners pricing", "vitko runners pricing --fields price_micros_per_slot_minute,free_minutes_per_month"},
			Run:      func(*cli.Ctx) (any, error) { return pricing.Current(), nil },
			Text:     pricingText,
		},
		estimateCmd(env),
		switchCmd(env),
	}
	return append(cmds, APICommands(env)...)
}

func versionDoc() map[string]any {
	return map[string]any{
		"schema": "vitko.version/v1", "version": version.Version,
		"commit": nullIfEmpty(version.Commit), "date": nullIfEmpty(version.Date),
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func pricingText(_ *cli.Ctx, w io.Writer, v any) error {
	d := v.(pricing.Doc)
	fmt.Fprintf(w, "%s price list (as of %s)\n\n", d.Product, d.PricesAsOf)
	fmt.Fprintf(w, "  Price        $%.3f per minute per slot (a slot is up to %d vCPU / %d GB, Linux x64, %s)\n",
		float64(d.PriceMicrosPerMin)/1e6, d.Slot.VCPUsMax, d.Slot.MemoryBytes>>30, d.Slot.OS)
	fmt.Fprintf(w, "  Billing      %s\n", d.Billing.Description)
	fmt.Fprintf(w, "  Free         %s minutes a month\n", commas(d.FreeMinutesPerMonth))
	fmt.Fprintf(w, "  runs-on      %s\n", d.Slot.RunsOn)
	fmt.Fprintf(w, "\n  %s GitHub charges $%.3f per minute.\n  Basis: %s\n  Source: %s (as of %s)\n",
		d.Comparison.Ratio, float64(d.Comparison.GithubMicrosPerMinute)/1e6, d.Comparison.Basis, d.Comparison.Source, d.Comparison.PricesAsOf)
	for _, n := range d.Notes {
		fmt.Fprintf(w, "\n%s\n", n)
	}
	fmt.Fprintf(w, "More: %s\n", d.URL)
	return nil
}

func commas(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// ---- config ----

func configValues(env Env) (map[string]any, error) {
	m, err := config.Load(env.Getenv)
	if err != nil {
		return nil, cli.Errf("invalid_input", "%v", err).WithHint("Fix or delete the file, then run the command again.")
	}
	vals := map[string]any{}
	for _, k := range config.Keys {
		v, src := resolveKey(env, m, k.Name)
		vals[k.Name] = map[string]any{"value": v, "source": src}
	}
	return vals, nil
}

func resolveKey(env Env, m map[string]string, key string) (any, string) {
	if key == "output" {
		if e := env.Getenv("VITKO_OUTPUT"); e != "" {
			return e, "env"
		}
	}
	if v, ok := m[key]; ok && v != "" {
		return v, "file"
	}
	return nil, "unset"
}

func configList(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"config", "list"}, Summary: "List your settings.",
		Idempotent: true, Output: "vitko.config/v1", Errors: []string{"invalid_input"},
		Run: func(*cli.Ctx) (any, error) {
			vals, err := configValues(env)
			if err != nil {
				return nil, err
			}
			return map[string]any{"schema": "vitko.config/v1", "path": config.Path(env.Getenv), "values": vals, "keys": config.Keys}, nil
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			d := v.(map[string]any)
			fmt.Fprintf(w, "Settings file: %s\n", d["path"])
			for _, k := range config.Keys {
				e := d["values"].(map[string]any)[k.Name].(map[string]any)
				val := e["value"]
				if val == nil {
					val = "(not set)"
				}
				fmt.Fprintf(w, "  %-10s %v  [%s]\n", k.Name, val, e["source"])
			}
			return nil
		},
	}
}

func checkKey(name string) (*config.Key, error) {
	k := config.FindKey(name)
	if k == nil {
		names := []string{}
		for _, k := range config.Keys {
			names = append(names, k.Name)
		}
		return nil, cli.Errf("usage", "There is no setting %q.", name).WithHint("Settings: " + strings.Join(names, ", ") + ".")
	}
	return k, nil
}

func configGet(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"config", "get"}, Summary: "Show one setting.",
		Args:       []cli.Arg{{Name: "key", Summary: "The setting's name.", Required: true}},
		Idempotent: true, Output: "vitko.config-value/v1", Errors: []string{"invalid_input"},
		Examples: []string{"vitko config get output"},
		Run: func(c *cli.Ctx) (any, error) {
			if _, err := checkKey(c.Args[0]); err != nil {
				return nil, err
			}
			m, err := config.Load(env.Getenv)
			if err != nil {
				return nil, cli.Errf("invalid_input", "%v", err)
			}
			v, src := resolveKey(env, m, c.Args[0])
			return map[string]any{"schema": "vitko.config-value/v1", "key": c.Args[0], "value": v, "source": src}, nil
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			d := v.(map[string]any)
			if d["value"] == nil {
				_, err := fmt.Fprintln(w, "(not set)")
				return err
			}
			_, err := fmt.Fprintln(w, d["value"])
			return err
		},
	}
}

func configSet(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"config", "set"}, Summary: "Change one setting.",
		Description: "Writes the settings file. An empty value removes the setting.",
		Args: []cli.Arg{{Name: "key", Summary: "The setting's name.", Required: true},
			{Name: "value", Summary: "The new value (empty to remove it).", Required: true}},
		Flags:   []cli.Flag{{Name: "dry-run", Type: cli.TypeBool, Summary: "Show what would change without writing anything."}},
		Mutates: true, Idempotent: true, Output: "vitko.config/v1", Errors: []string{"invalid_input"},
		Examples: []string{"vitko config set output json", "vitko config set output ''"},
		Run: func(c *cli.Ctx) (any, error) {
			k, err := checkKey(c.Args[0])
			if err != nil {
				return nil, err
			}
			val := c.Args[1]
			if val != "" && len(k.Enum) > 0 && !contains(k.Enum, val) {
				return nil, cli.Errf("usage", "%q is not a valid %s.", val, k.Name).WithHint("Use one of: " + strings.Join(k.Enum, ", ") + ".")
			}
			m, err := config.Load(env.Getenv)
			if err != nil {
				return nil, cli.Errf("invalid_input", "%v", err)
			}
			var from any
			if old, ok := m[k.Name]; ok {
				from = old
			}
			var to any
			if val != "" {
				to = val
			}
			changed := from != to
			if c.Bool("dry-run") {
				changes := []map[string]any{}
				if changed {
					changes = append(changes, map[string]any{"resource": "config", "field": k.Name, "from": from, "to": to})
				}
				return map[string]any{"schema": "vitko.plan/v1", "dry_run": true, "would_succeed": true, "changes": changes, "blocked_by": nil}, nil
			}
			if changed {
				if val == "" {
					delete(m, k.Name)
				} else {
					m[k.Name] = val
				}
				if err := config.Save(env.Getenv, m); err != nil {
					return nil, cli.Errf("internal", "Couldn't write %s: %v", config.Path(env.Getenv), err)
				}
			}
			vals, err := configValues(env)
			if err != nil {
				return nil, err
			}
			return map[string]any{"schema": "vitko.config/v1", "path": config.Path(env.Getenv), "changed": changed, "values": vals, "keys": config.Keys}, nil
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			d := v.(map[string]any)
			if d["schema"] == "vitko.plan/v1" {
				ch := d["changes"].([]map[string]any)
				if len(ch) == 0 {
					_, err := fmt.Fprintln(w, "No change.")
					return err
				}
				for _, c := range ch {
					fmt.Fprintf(w, "Would change %s: %v -> %v\n", c["field"], orUnset(c["from"]), orUnset(c["to"]))
				}
				return nil
			}
			if d["changed"] == true {
				_, err := fmt.Fprintln(w, "Saved.")
				return err
			}
			_, err := fmt.Fprintln(w, "No change.")
			return err
		},
	}
}

func orUnset(v any) any {
	if v == nil {
		return "(not set)"
	}
	return v
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ---- doctor ----

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

func doctorCmd(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"doctor"}, Summary: "Check that vitko and the tools it uses are ready.",
		Description: "Runs local checks and reports each as ok, warn or fail, with a hint for anything that isn't ok.",
		Idempotent:  true, Network: true, Output: "vitko.doctor/v1",
		Run: func(c *cli.Ctx) (any, error) {
			checks := []check{
				{Name: "vitko", Status: "ok", Detail: fmt.Sprintf("vitko %s (%s/%s)", version.Version, runtime.GOOS, runtime.GOARCH)},
			}
			if _, err := config.Load(env.Getenv); err != nil {
				checks = append(checks, check{Name: "config", Status: "fail", Detail: err.Error(), Hint: "Fix or delete the settings file."})
			} else {
				checks = append(checks, check{Name: "config", Status: "ok", Detail: config.Path(env.Getenv)})
			}
			why := "stdout is not a terminal"
			if env.StdoutIsTerminal {
				why = "stdout is a terminal"
			}
			checks = append(checks, check{Name: "output", Status: "ok", Detail: fmt.Sprintf("%s (%s)", c.Mode, why)})
			if _, err := env.LookPath("git"); err != nil {
				checks = append(checks, check{Name: "git", Status: "warn", Detail: "git is not installed", Hint: "`vitko runners repos switch --pr` needs git. Install it from https://git-scm.com."})
			} else {
				out, _ := env.Exec("", "git", "--version")
				checks = append(checks, check{Name: "git", Status: "ok", Detail: strings.TrimSpace(out)})
			}
			if _, err := env.LookPath("gh"); err != nil {
				checks = append(checks, check{Name: "github_cli", Status: "warn", Detail: "the GitHub CLI (gh) is not installed",
					Hint: "`vitko runners repos switch --pr` and `vitko runners estimate --from-github` need it. Install it from https://cli.github.com."})
			} else if login, err := env.Exec("", "gh", "api", "user", "--jq", ".login"); err != nil {
				checks = append(checks, check{Name: "github_cli", Status: "warn", Detail: "gh is installed but can't reach GitHub as a signed-in user", Hint: "Run `gh auth login`, or check your network."})
			} else {
				checks = append(checks, check{Name: "github_cli", Status: "ok", Detail: "gh is signed in as " + strings.TrimSpace(login)})
			}
			ok := true
			for _, ch := range checks {
				if ch.Status == "fail" {
					ok = false
				}
			}
			return map[string]any{"schema": "vitko.doctor/v1", "ok": ok, "checks": checks}, nil
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			d := v.(map[string]any)
			for _, ch := range d["checks"].([]check) {
				fmt.Fprintf(w, "  %-4s  %-11s %s\n", ch.Status, ch.Name, ch.Detail)
				if ch.Hint != "" {
					fmt.Fprintf(w, "        %-11s %s\n", "", ch.Hint)
				}
			}
			return nil
		},
	}
}

// ---- runners estimate ----

var monthRe = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])$`)

func estimateCmd(env Env) *cli.Command {
	return &cli.Command{
		Path:    []string{"runners", "estimate"},
		Summary: "Estimate what your GitHub Actions usage would cost on Vitko Runners.",
		Description: "Reads GitHub's usage report and prices the Linux x64 minutes at the Vitko list price, next to what GitHub charged. " +
			"It runs on your machine: the report is never uploaded.\n" +
			"Get the report from GitHub: Settings > Billing and licensing > Usage > Get usage report (the summarized report is enough). " +
			"Or pass --from-github to fetch it with the GitHub CLI (needs an organization owner or billing manager).\n" +
			"Each month gets its own free minutes. The result is an upper bound, because GitHub rounds each job up to whole minutes and Vitko bills per second.",
		Args: []cli.Arg{{Name: "file", Summary: "GitHub usage report: the CSV, or the JSON from the billing usage API. Several files are fine.", Variadic: true}},
		Flags: []cli.Flag{
			{Name: "from-github", Type: cli.TypeString, Placeholder: "ORG", Summary: "Fetch the usage of this GitHub organization with the GitHub CLI (gh) instead of reading files."},
			{Name: "month", Type: cli.TypeStrings, Placeholder: "YYYY-MM", Summary: "With --from-github: the month(s) to fetch. Default: last month."},
			{Name: "by-repo", Type: cli.TypeBool, Summary: "Also list Linux x64 minutes per repository."},
			{Name: "per-second-factor", Type: cli.TypeNumber, Default: 1.0, Placeholder: "N",
				Summary: "Illustrative ratio of per-second time to GitHub's rounded minutes, in (0, 1]. 1 is the upper bound."},
		},
		Idempotent: true, Network: true, Output: "vitko.runners.estimate/v1",
		Errors: []string{"input_required", "invalid_input", "file_not_found", "tool_missing", "github_signed_out", "github_forbidden", "github_not_found", "github_unavailable"},
		Examples: []string{
			"vitko runners estimate usage-report.csv",
			"vitko runners estimate usage-2026-08.csv usage-2026-09.csv --by-repo",
			"vitko runners estimate --from-github my-org --month 2026-08 --month 2026-09",
		},
		Run: func(c *cli.Ctx) (any, error) {
			f := c.Number("per-second-factor")
			if !(f > 0 && f <= 1) {
				return nil, cli.Errf("usage", "--per-second-factor must be more than 0 and at most 1.")
			}
			org := c.String("from-github")
			if len(c.Args) == 0 && org == "" {
				return nil, cli.Errf("input_required", "Give a GitHub usage report file, or --from-github <org>.").
					WithHint("Download the report from GitHub: Settings > Billing and licensing > Usage > Get usage report.")
			}
			if len(c.Strings("month")) > 0 && org == "" {
				return nil, cli.Errf("usage", "--month only works with --from-github.")
			}
			var files [][]estimate.Row
			for _, p := range c.Args {
				rows, err := estimate.Load(p)
				if errors.Is(err, fs.ErrNotExist) {
					return nil, cli.Errf("file_not_found", "%s doesn't exist.", p).WithHint("Check the path of the usage report.")
				}
				if err != nil {
					return nil, cli.Errf("invalid_input", "Couldn't read %s: %v", p, err).
						WithHint("Pass GitHub's usage report CSV, or the JSON from the billing usage API.")
				}
				files = append(files, rows)
			}
			if org != "" {
				months := c.Strings("month")
				if len(months) == 0 {
					now := env.Now().UTC()
					months = []string{time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")}
				}
				for _, m := range months {
					rows, err := fetchUsage(env, org, m)
					if err != nil {
						return nil, err
					}
					files = append(files, rows)
				}
			}
			return estimate.Estimate(files, estimate.Options{PerSecondFactor: f, ByRepo: c.Bool("by-repo")}), nil
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			estimate.WriteText(w, v.(estimate.Result))
			return nil
		},
	}
}

var orgRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func fetchUsage(env Env, org, month string) ([]estimate.Row, error) {
	if !orgRe.MatchString(org) {
		return nil, cli.Errf("usage", "%q is not a GitHub organization name.", org)
	}
	m := monthRe.FindStringSubmatch(month)
	if m == nil {
		return nil, cli.Errf("usage", "--month %q is not in the form YYYY-MM.", month)
	}
	if _, err := env.LookPath("gh"); err != nil {
		return nil, ghMissing()
	}
	path := fmt.Sprintf("/organizations/%s/settings/billing/usage?year=%s&month=%s", org, m[1], strings.TrimPrefix(m[2], "0"))
	out, err := env.Exec("", "gh", "api", path)
	if err != nil {
		return nil, ghError(err, "Fetching usage needs an organization owner or billing manager. You can also download the usage report and pass the file.")
	}
	rows, perr := estimate.Parse([]byte(out))
	if perr != nil {
		return nil, cli.Errf("invalid_input", "GitHub's answer for %s couldn't be read: %v", month, perr)
	}
	return rows, nil
}

func ghMissing() *cli.Error {
	e := cli.Errf("tool_missing", "This needs the GitHub CLI (gh), which isn't installed.")
	e.Hint = "Install it, then run `gh auth login`."
	e.Fix = &cli.Fix{URL: "https://cli.github.com"}
	return e
}

// ghError maps a failed git or gh call to an error code.
func ghError(err error, forbiddenHint string) *cli.Error {
	msg := err.Error()
	var ee *sys.ExecError
	if errors.As(err, &ee) {
		msg = strings.TrimSpace(ee.Stderr)
	}
	low := strings.ToLower(msg)
	short := firstLine(msg)
	switch {
	case strings.Contains(low, "gh auth login") || strings.Contains(low, "not logged in") || strings.Contains(low, "authentication") || strings.Contains(low, "http 401"):
		return cli.Errf("github_signed_out", "The GitHub CLI isn't signed in: %s", short).WithFix("gh auth login")
	case strings.Contains(low, "http 403") || strings.Contains(low, "permission") || strings.Contains(low, "denied") || strings.Contains(low, "must have admin"):
		return cli.Errf("github_forbidden", "GitHub refused: %s", short).WithHint(forbiddenHint)
	case strings.Contains(low, "http 404") || strings.Contains(low, "not found") || strings.Contains(low, "could not resolve"):
		return cli.Errf("github_not_found", "GitHub says not found: %s", short).WithHint("Check the name, and that your GitHub account can see it.")
	case strings.Contains(low, "http 5") || strings.Contains(low, "timeout") || strings.Contains(low, "connection") || strings.Contains(low, "could not connect"):
		return cli.Errf("github_unavailable", "GitHub didn't answer: %s", short)
	}
	return cli.Errf("internal", "%s", short)
}

func firstLine(s string) string { l, _, _ := strings.Cut(strings.TrimSpace(s), "\n"); return l }

// ---- runners repos switch ----

func switchCmd(env Env) *cli.Command {
	return &cli.Command{
		Path:    []string{"runners", "repos", "switch"},
		Summary: "Switch a repository's GitHub Actions jobs to Vitko runners.",
		Description: "Changes `runs-on: ubuntu-latest` and `runs-on: ubuntu-24.04` to `" + pricing.Label + "` in .github/workflows. " +
			"Only that value changes; comments and formatting stay as they are. " +
			"Jobs it can't change safely (expressions and matrices, other operating systems, self-hosted runners, runner groups, custom labels, workflows from other repositories) are listed with the reason, and left alone.\n" +
			"Running it again changes nothing. --dry-run shows the diff without writing. --check exits with code 10 if anything would change. " +
			"--pr makes the change on a fresh copy of the default branch and opens a pull request with your own git and GitHub CLI (gh) sign-in; your working copy isn't touched.",
		Args: []cli.Arg{{Name: "path", Summary: "Repository directory, or one workflow file. Default: the current directory."}},
		Flags: []cli.Flag{
			{Name: "workflow", Type: cli.TypeStrings, Placeholder: "FILE", Summary: "Only these workflow files (name or path)."},
			{Name: "job", Type: cli.TypeStrings, Placeholder: "ID", Summary: "Only these job ids."},
			{Name: "label", Type: cli.TypeString, Default: pricing.Label, Placeholder: "LABEL", Summary: "The runs-on label to switch to."},
			{Name: "dry-run", Type: cli.TypeBool, Summary: "Show the diff without writing anything."},
			{Name: "check", Type: cli.TypeBool, Summary: "Write nothing; exit with code 10 if anything would change."},
			{Name: "pr", Type: cli.TypeBool, Summary: "Open a pull request instead of editing the working copy."},
			{Name: "base", Type: cli.TypeString, Placeholder: "BRANCH", Summary: "With --pr: the branch to change and target. Default: the repository's default branch."},
			{Name: "branch", Type: cli.TypeString, Default: "vitko/switch-runners", Placeholder: "BRANCH", Summary: "With --pr: the name of the new branch."},
		},
		Mutates: true, Idempotent: true, Network: true, Output: "vitko.runners.switch/v1",
		Errors: []string{"file_not_found", "not_a_git_repository", "tool_missing", "github_signed_out", "github_forbidden",
			"github_not_found", "github_unavailable", "branch_exists", "input_required"},
		Examples: []string{
			"vitko runners repos switch --dry-run",
			"vitko runners repos switch",
			"vitko runners repos switch --check",
			"vitko runners repos switch --pr",
			"vitko runners repos switch path/to/repo --workflow ci.yml --job test",
		},
		Run:  func(c *cli.Ctx) (any, error) { return runSwitch(env, c) },
		Text: switchText,
	}
}

// SwitchDoc is vitko.runners.switch/v1.
type SwitchDoc struct {
	Schema          string             `json:"schema"`
	DryRun          bool               `json:"dry_run"`
	Check           bool               `json:"check"`
	Changed         bool               `json:"changed"`
	Label           string             `json:"label"`
	Files           []string           `json:"files"`
	Changes         []switcher.Change  `json:"changes"`
	Skipped         []switcher.Skipped `json:"skipped"`
	AlreadySwitched []switcher.Already `json:"already_switched"`
	Diff            string             `json:"diff"`
	PullRequest     *string            `json:"pull_request"`
	Branch          *string            `json:"branch"`
	Base            *string            `json:"base"`
}

func runSwitch(env Env, c *cli.Ctx) (any, error) {
	root := "."
	if len(c.Args) > 0 {
		root = c.Args[0]
	}
	label := c.String("label")
	if !switcher.LabelRe.MatchString(label) {
		return nil, cli.Errf("usage", "--label %q is not a valid runner label.", label)
	}
	if c.Bool("pr") && c.Bool("check") {
		return nil, cli.Errf("usage", "--check and --pr can't be used together.")
	}
	opts := switcher.Options{Root: root, Workflows: c.Strings("workflow"), Jobs: c.Strings("job"), Label: label}
	doc := &SwitchDoc{Schema: "vitko.runners.switch/v1", DryRun: c.Bool("dry-run"), Check: c.Bool("check"), Label: label}
	if _, err := os.Stat(root); err != nil {
		return nil, cli.Errf("file_not_found", "%s doesn't exist.", root).WithHint("Pass a repository directory or a workflow file.")
	}
	var plan *switcher.Plan
	if c.Bool("pr") {
		for _, tool := range []string{"git", "gh"} {
			if _, err := env.LookPath(tool); err != nil {
				if tool == "gh" {
					return nil, ghMissing()
				}
				e := cli.Errf("tool_missing", "--pr needs git, which isn't installed.")
				e.Fix = &cli.Fix{URL: "https://git-scm.com"}
				return nil, e
			}
		}
		res, err := switcher.OpenPR(switcher.PROptions{Options: opts, Base: c.String("base"), Branch: c.String("branch"), DryRun: doc.DryRun}, env.Exec)
		if err != nil {
			return nil, prError(err, c.String("branch"))
		}
		plan = res.Plan
		doc.Base, doc.Branch = &res.Base, &res.Branch
		if res.URL != "" {
			doc.PullRequest = &res.URL
			doc.Changed = true
		}
	} else {
		p, err := switcher.Make(opts)
		if err != nil {
			return nil, switchError(err, root)
		}
		plan = p
		if !doc.DryRun && !doc.Check && len(p.NewFiles) > 0 {
			if err := p.Apply(); err != nil {
				return nil, cli.Errf("internal", "Couldn't write the workflow files: %v", err)
			}
			doc.Changed = true
		}
	}
	doc.Files = plan.Files
	if doc.Files == nil {
		doc.Files = []string{}
	}
	doc.Changes, doc.Skipped, doc.AlreadySwitched, doc.Diff = plan.Changes, plan.Skipped, plan.Already, plan.Diff
	if doc.Check && len(plan.Changes) > 0 {
		return doc, cli.ChangesPending{}
	}
	return doc, nil
}

func switchError(err error, root string) error {
	var nw switcher.ErrNoWorkflows
	if errors.As(err, &nw) {
		return cli.Errf("file_not_found", "No workflow files in %s.", nw.Dir).
			WithHint("Run it in a repository's root directory, or pass the repository's path or a workflow file.")
	}
	if errors.Is(err, fs.ErrNotExist) {
		return cli.Errf("file_not_found", "%s doesn't exist.", root)
	}
	return cli.Errf("internal", "%v", err)
}

func prError(err error, branch string) error {
	var pe *switcher.PRError
	if !errors.As(err, &pe) {
		return switchError(err, ".")
	}
	var ee *sys.ExecError
	stderr := ""
	if errors.As(pe.Err, &ee) {
		stderr = strings.ToLower(ee.Stderr)
	}
	switch pe.Step {
	case switcher.StepRepo:
		if strings.Contains(stderr, "not a git repository") {
			return cli.Errf("not_a_git_repository", "--pr needs a git repository, and this path isn't in one.").
				WithHint("Run it inside a clone of the repository, or leave out --pr to edit the files only.")
		}
	case switcher.StepAuth:
		return cli.Errf("github_signed_out", "The GitHub CLI (gh) isn't signed in.").WithFix("gh auth login")
	case switcher.StepBranch:
		next := branch + "-2"
		return cli.Errf("branch_exists", "The branch %s already exists.", branch).
			WithHint("An earlier run may have opened a pull request already. Otherwise pick another branch name.").
			WithFix("vitko runners repos switch --pr --branch " + next)
	case switcher.StepCommit:
		if strings.Contains(stderr, "tell me who you are") || strings.Contains(stderr, "user.email") {
			return cli.Errf("input_required", "git doesn't know your name and email, so it can't commit.").
				WithHint("Set them with `git config --global user.name` and `git config --global user.email`.")
		}
	case switcher.StepPush:
		return ghError(pe.Err, "You need permission to push branches to this repository.")
	}
	return ghError(pe.Err, "Your GitHub account isn't allowed to do this in this repository.")
}

func switchText(_ *cli.Ctx, w io.Writer, v any) error {
	d := v.(*SwitchDoc)
	if d.Diff != "" && (d.DryRun || d.Check || d.PullRequest == nil) {
		fmt.Fprint(w, d.Diff)
		fmt.Fprintln(w)
	}
	n := len(d.Changes)
	switch {
	case n == 0 && len(d.AlreadySwitched) > 0:
		fmt.Fprintf(w, "Nothing to change: %d job(s) already use Vitko runners.\n", len(d.AlreadySwitched))
	case n == 0:
		fmt.Fprintf(w, "Nothing to change.\n")
	case d.DryRun || d.Check:
		fmt.Fprintf(w, "Would switch %d job(s) to %s. Nothing was written.\n", n, d.Label)
	case d.PullRequest != nil:
		fmt.Fprintf(w, "Opened a pull request that switches %d job(s) to %s:\n  %s\n", n, d.Label, *d.PullRequest)
	default:
		fmt.Fprintf(w, "Switched %d job(s) to %s. Review with `git diff`, then commit.\n", n, d.Label)
	}
	if len(d.Skipped) > 0 {
		fmt.Fprintf(w, "\nLeft as they are (%d):\n", len(d.Skipped))
		for _, s := range d.Skipped {
			job := s.Job
			if job == "" {
				job = "(file)"
			}
			fmt.Fprintf(w, "  %s:%d  job %s  %s\n    %s\n", s.File, s.Line, job, s.RunsOn, s.Hint)
		}
	}
	return nil
}

// openBrowser opens url with the platform's opener, without waiting.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	return exec.Command(path, url).Start()
}
