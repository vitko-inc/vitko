package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/vitko-inc/vitko/internal/cli"
	"github.com/vitko-inc/vitko/internal/sys"
	"github.com/vitko-inc/vitko/schemas"
)

var update = flag.Bool("update", false, "rewrite golden files")

const golden = "../../testdata/golden"

type result struct {
	code           int
	stdout, stderr string
}

type harness struct {
	env      map[string]string
	terminal bool
	lookPath func(string) (string, error)
	exec     sys.Exec
}

func newHarness(t *testing.T) *harness {
	return &harness{
		env:      map[string]string{"VITKO_CONFIG_DIR": t.TempDir()},
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		exec:     func(string, string, ...string) (string, error) { return "", errors.New("no exec in tests") },
	}
}

func (h *harness) run(args ...string) result {
	var out, errb bytes.Buffer
	a := New(Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv:           func(k string) string { return h.env[k] },
		StdoutIsTerminal: h.terminal, LookPath: h.lookPath, Exec: h.exec,
		Now: func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) },
	})
	code := a.Run(args)
	return result{code, out.String(), errb.String()}
}

// validator compiles every embedded schema.
func validator(t *testing.T) func(id string, doc []byte) error {
	t.Helper()
	c := jsonschema.NewCompiler()
	for _, id := range schemas.IDs() {
		b, _ := schemas.Get(id)
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if err := c.AddResource(schemas.BaseURL+id, v); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	cache := map[string]*jsonschema.Schema{}
	return func(id string, doc []byte) error {
		s, ok := cache[id]
		if !ok {
			var err error
			if s, err = c.Compile(schemas.BaseURL + id); err != nil {
				return err
			}
			cache[id] = s
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
		if err != nil {
			return err
		}
		return s.Validate(v)
	}
}

func schemaOf(t *testing.T, doc string) string {
	t.Helper()
	var d struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, doc)
	}
	return d.Schema
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(golden, name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/commands -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run `go test ./internal/commands -update` if the change is intended.\n--- got ---\n%s", name, got)
	}
}

func TestGoldenOutputsMatchAndValidate(t *testing.T) {
	validate := validator(t)
	h := newHarness(t)
	cases := []struct {
		name string
		args []string
	}{
		{"help.json", []string{"help"}},
		{"help-runners-estimate.json", []string{"runners", "estimate", "--help", "--json"}},
		{"pricing.json", []string{"runners", "pricing"}},
		{"schema-list.json", []string{"schema", "list"}},
		{"estimate-synthetic.json", []string{"runners", "estimate", "../runners/estimate/testdata/synthetic-usage-report.csv", "--by-repo"}},
		{"switch-dry-run.json", []string{"runners", "repos", "switch", "../runners/switcher/testdata/repo", "--dry-run"}},
		{"config-list.json", []string{"config", "list"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := h.run(c.args...)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			id := schemaOf(t, r.stdout)
			if err := validate(id, []byte(r.stdout)); err != nil {
				t.Errorf("output does not match %s: %v", id, err)
			}
			out := r.stdout
			if c.name == "config-list.json" {
				out = strings.ReplaceAll(out, h.env["VITKO_CONFIG_DIR"], "$VITKO_CONFIG_DIR")
			}
			checkGolden(t, c.name, out)
		})
	}
}

func TestHelpText(t *testing.T) {
	h := newHarness(t)
	h.terminal = true
	r := h.run("help")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	checkGolden(t, "help.txt", r.stdout)
}

func TestErrorsValidateAndMapToExitCodes(t *testing.T) {
	validate := validator(t)
	h := newHarness(t)
	for _, c := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"nope"}, 2, "unknown_command"},
		{[]string{"runners", "nope"}, 2, "unknown_command"},
		{[]string{"runners", "pricing", "--nope"}, 2, "usage"},
		{[]string{"runners", "pricing", "--fields", "nope"}, 2, "usage"},
		{[]string{"runners", "pricing", "--output", "yaml"}, 2, "usage"},
		{[]string{"runners", "estimate"}, 2, "input_required"},
		{[]string{"runners", "estimate", "missing.csv"}, 5, "file_not_found"},
		{[]string{"runners", "estimate", "--per-second-factor", "0", "x"}, 2, "usage"},
		{[]string{"runners", "estimate", "--from-github", "acme"}, 2, "tool_missing"},
		{[]string{"runners", "repos", "switch", "does/not/exist"}, 5, "file_not_found"},
		{[]string{"runners", "repos", "switch", "."}, 5, "file_not_found"},
		{[]string{"runners", "repos", "switch", "--pr"}, 2, "tool_missing"},
		{[]string{"runners", "split-tests"}, 2, "unknown_command"},
		{[]string{"schema", "show", "vitko.nope/v1"}, 5, "schema_not_found"},
		{[]string{"schema", "show"}, 2, "input_required"},
		{[]string{"config", "set", "nope", "x"}, 2, "usage"},
		{[]string{"config", "set", "output", "yaml"}, 2, "usage"},
		{[]string{"version", "extra"}, 2, "usage"},
	} {
		r := h.run(c.args...)
		if r.code != c.code {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, r.code, c.code, r.stderr)
		}
		if r.stdout != "" {
			t.Errorf("%v: stdout must be empty on error, got %q", c.args, r.stdout)
		}
		if err := validate("vitko.error/v1", []byte(r.stderr)); err != nil {
			t.Errorf("%v: stderr is not vitko.error/v1: %v\n%s", c.args, err, r.stderr)
		}
		var d struct {
			Error cli.Error `json:"error"`
		}
		_ = json.Unmarshal([]byte(r.stderr), &d)
		if d.Error.Code != c.err {
			t.Errorf("%v: code %q, want %q", c.args, d.Error.Code, c.err)
		}
		if d.Error.Hint == "" && d.Error.Fix == nil && c.err != "usage" {
			t.Errorf("%v: no hint or fix", c.args)
		}
	}
}

func TestOutputModeDefaults(t *testing.T) {
	h := newHarness(t)
	if r := h.run("version"); !strings.HasPrefix(r.stdout, "{") {
		t.Errorf("not a terminal: want JSON, got %q", r.stdout)
	}
	h.terminal = true
	if r := h.run("version"); !strings.HasPrefix(r.stdout, "vitko ") {
		t.Errorf("terminal: want text, got %q", r.stdout)
	}
	if r := h.run("version", "--json"); !strings.HasPrefix(r.stdout, "{") {
		t.Errorf("--json: got %q", r.stdout)
	}
	if r := h.run("version", "--output", "ndjson"); strings.Count(r.stdout, "\n") != 1 || !strings.HasPrefix(r.stdout, "{\"schema\"") {
		t.Errorf("ndjson: got %q", r.stdout)
	}
	h.env["VITKO_OUTPUT"] = "json"
	if r := h.run("version"); !strings.HasPrefix(r.stdout, "{") {
		t.Errorf("VITKO_OUTPUT=json: got %q", r.stdout)
	}
	delete(h.env, "VITKO_OUTPUT")
	if r := h.run("config", "set", "output", "json"); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if r := h.run("version"); !strings.HasPrefix(r.stdout, "{") {
		t.Errorf("config output=json: got %q", r.stdout)
	}
	// Errors follow the mode too.
	h.run("config", "set", "output", "")
	if r := h.run("nope"); !strings.HasPrefix(r.stderr, "vitko: ") {
		t.Errorf("text error: %q", r.stderr)
	}
}

func TestFields(t *testing.T) {
	h := newHarness(t)
	r := h.run("runners", "pricing", "--fields", "price_micros_per_slot_minute,comparison.github_micros_per_minute")
	want := "{\n  \"schema\": \"vitko.runners.pricing/v1\",\n  \"price_micros_per_slot_minute\": 2000,\n  \"comparison\": {\n    \"github_micros_per_minute\": 6000\n  }\n}\n"
	if r.stdout != want {
		t.Errorf("got\n%s", r.stdout)
	}
}

func TestConfig(t *testing.T) {
	h := newHarness(t)
	r := h.run("config", "set", "output", "json", "--dry-run")
	if !strings.Contains(r.stdout, "\"vitko.plan/v1\"") || !strings.Contains(r.stdout, "\"to\": \"json\"") {
		t.Errorf("dry run: %s", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(h.env["VITKO_CONFIG_DIR"], "config.json")); err == nil {
		t.Error("dry run wrote the file")
	}
	r = h.run("config", "set", "output", "json")
	if !strings.Contains(r.stdout, "\"changed\": true") {
		t.Errorf("set: %s", r.stdout)
	}
	r = h.run("config", "set", "output", "json")
	if !strings.Contains(r.stdout, "\"changed\": false") {
		t.Errorf("set again: %s", r.stdout)
	}
	r = h.run("config", "get", "output")
	if !strings.Contains(r.stdout, "\"source\": \"file\"") {
		t.Errorf("get: %s", r.stdout)
	}
	st, _ := os.Stat(filepath.Join(h.env["VITKO_CONFIG_DIR"], "config.json"))
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	_ = filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	return dst
}

func TestSwitchCheckAndApply(t *testing.T) {
	h := newHarness(t)
	dir := copyDir(t, "../runners/switcher/testdata/repo")
	r := h.run("runners", "repos", "switch", dir, "--check")
	if r.code != 10 || !strings.Contains(r.stdout, "\"changes\": [") {
		t.Errorf("check: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = h.run("runners", "repos", "switch", dir)
	if r.code != 0 || !strings.Contains(r.stdout, "\"changed\": true") {
		t.Errorf("apply: exit %d\n%s", r.code, r.stdout)
	}
	r = h.run("runners", "repos", "switch", dir, "--check")
	if r.code != 0 || !strings.Contains(r.stdout, "\"changed\": false") {
		t.Errorf("check after apply: exit %d\n%s", r.code, r.stdout)
	}
	h.terminal = true
	r = h.run("runners", "repos", "switch", dir)
	if !strings.Contains(r.stdout, "already use Vitko runners") {
		t.Errorf("text: %s", r.stdout)
	}
}

func TestEstimateFromGitHub(t *testing.T) {
	h := newHarness(t)
	h.lookPath = func(n string) (string, error) { return "/usr/bin/" + n, nil }
	var calls []string
	h.exec = func(dir, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return `{"usageItems":[{"date":"2026-09-03T00:00:00Z","product":"actions","sku":"Actions Linux","quantity":3000,"unitType":"Minutes","pricePerUnit":0.006,"grossAmount":18,"netAmount":18,"repositoryName":"api"}]}`, nil
	}
	r := h.run("runners", "estimate", "--from-github", "acme")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	if len(calls) != 1 || calls[0] != "gh api /organizations/acme/settings/billing/usage?year=2026&month=9" {
		t.Errorf("calls = %v", calls)
	}
	if !strings.Contains(r.stdout, "\"vitko_micros\": 2000000") {
		t.Errorf("%s", r.stdout)
	}
	h.exec = func(string, string, ...string) (string, error) {
		return "", &sys.ExecError{Cmd: "gh api", Stderr: "gh: Not Found (HTTP 404)", Code: 1}
	}
	if r := h.run("runners", "estimate", "--from-github", "acme", "--month", "2026-08"); r.code != 5 {
		t.Errorf("404: exit %d %s", r.code, r.stderr)
	}
	h.exec = func(string, string, ...string) (string, error) {
		return "", &sys.ExecError{Cmd: "gh api", Stderr: "gh: Must have admin rights to Repository. (HTTP 403)", Code: 1}
	}
	if r := h.run("runners", "estimate", "--from-github", "acme"); r.code != 4 {
		t.Errorf("403: exit %d %s", r.code, r.stderr)
	}
	if r := h.run("runners", "estimate", "--from-github", "acme", "--month", "2026-13"); r.code != 2 {
		t.Errorf("bad month: exit %d", r.code)
	}
}

func TestDoctor(t *testing.T) {
	validate := validator(t)
	h := newHarness(t)
	r := h.run("doctor")
	if r.code != 0 || validate("vitko.doctor/v1", []byte(r.stdout)) != nil {
		t.Fatalf("exit %d %s %s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "\"status\": \"warn\"") {
		t.Errorf("missing tools should warn: %s", r.stdout)
	}
	os.WriteFile(filepath.Join(h.env["VITKO_CONFIG_DIR"], "config.json"), []byte("{"), 0o600)
	r = h.run("doctor")
	if !strings.Contains(r.stdout, "\"ok\": false") {
		t.Errorf("bad config should fail: %s", r.stdout)
	}
}

func TestPluginDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"hello $VITKO_OUTPUT $*\"\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "vitko-runners-hello"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := newHarness(t)
	h.lookPath = exec.LookPath
	h.env["PATH"] = os.Getenv("PATH")
	r := h.run("runners", "hello", "--x", "y")
	if r.code != 3 || r.stdout != "hello json --x y\n" {
		t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	r = h.run("help", "runners")
	if !strings.Contains(r.stdout, "\"path\": \"runners hello\"") || !strings.Contains(r.stdout, "\"plugin\": true") {
		t.Errorf("plugin not listed in help: %s", r.stdout)
	}
}

func TestRegistryIsConsistent(t *testing.T) {
	codes := map[string]bool{}
	for _, c := range cli.ErrorCodeList() {
		codes[c["code"].(string)] = true
	}
	ids := map[string]bool{}
	for _, id := range schemas.IDs() {
		ids[id] = true
		b, _ := schemas.Get(id)
		var s struct {
			ID    string `json:"$id"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(b, &s); err != nil || s.ID != schemas.BaseURL+id || s.Title == "" {
			t.Errorf("%s: bad $id or title (%v)", id, err)
		}
	}
	seen := map[string]bool{}
	for _, c := range List(Env{}) {
		if seen[c.Name()] {
			t.Errorf("duplicate command %s", c.Name())
		}
		seen[c.Name()] = true
		if c.Summary == "" || c.Run == nil {
			t.Errorf("%s: missing summary or run", c.Name())
		}
		if c.Output != "" && !ids[c.Output] {
			t.Errorf("%s: output schema %s is not in schemas/", c.Name(), c.Output)
		}
		for _, e := range c.Errors {
			if !codes[e] {
				t.Errorf("%s: error code %s is not in the catalog", c.Name(), e)
			}
		}
		for _, f := range c.Flags {
			if f.Name == "dry-run" && !c.Mutates {
				t.Errorf("%s: --dry-run on a command that doesn't mutate", c.Name())
			}
		}
		if c.Mutates && !hasFlag(c, "dry-run") {
			t.Errorf("%s: mutating command without --dry-run", c.Name())
		}
	}
}

func hasFlag(c *cli.Command, name string) bool {
	for _, f := range c.Flags {
		if f.Name == name {
			return true
		}
	}
	return false
}
