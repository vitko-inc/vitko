package commands

// Commands that call the Vitko Runners API: sign-in, organizations, API
// tokens, jobs, usage, and limits with their ceilings.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vitko-inc/vitko/internal/api"
	"github.com/vitko-inc/vitko/internal/cli"
	"github.com/vitko-inc/vitko/internal/config"
)

func newClient(env Env) *api.Client {
	base := strings.TrimSpace(env.Getenv("VITKO_RUNNERS_API_URL"))
	if base == "" {
		base = api.DefaultRunnersURL
	}
	httpClient := env.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &api.Client{BaseURL: base, HTTP: httpClient, Getenv: env.Getenv, Store: api.CredentialFile{Path: api.CredentialsPath(config.Dir(env.Getenv))}, Now: env.Now}
}

var orgFlag = cli.Flag{Name: "org", Type: cli.TypeString, Env: "VITKO_ORG", Placeholder: "ORG",
	Summary: "The GitHub organization (login or numeric id). Default: the `org` setting, else your only organization."}

var apiErrors = []string{"signed_out", "token_expired", "token_invalid", "forbidden", "scope_missing", "not_found", "unavailable"}

func withErrors(extra ...string) []string { return append(append([]string{}, apiErrors...), extra...) }

// raw decodes a document into a generic map for text rendering.
func raw(v any) map[string]any {
	m := map[string]any{}
	if b, ok := v.(json.RawMessage); ok {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// list joins a JSON list of strings for text output.
func joinList(v any) string {
	items, _ := v.([]any)
	parts := make([]string, 0, len(items))
	for _, x := range items {
		parts = append(parts, str(x))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func usdMicros(v any) string {
	f, ok := v.(float64)
	if !ok {
		return "no limit"
	}
	return fmt.Sprintf("$%.2f", f/1e6)
}

// resolveOrg turns --org (login or id), the setting, or the caller's only
// organization into a numeric id.
func resolveOrg(ctx context.Context, env Env, c *api.Client, flag string) (string, error) {
	if flag == "" {
		if m, err := config.Load(env.Getenv); err == nil {
			flag = m["org"]
		}
	}
	if flag != "" {
		if _, err := strconv.ParseInt(flag, 10, 64); err == nil {
			return flag, nil
		}
	}
	doc, err := c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs"})
	if err != nil {
		return "", err
	}
	var orgs struct {
		Data []struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"data"`
	}
	if err := json.Unmarshal(doc, &orgs); err != nil {
		return "", cli.Errf("unavailable", "The organization list couldn't be read.")
	}
	var logins []string
	for _, o := range orgs.Data {
		logins = append(logins, o.Login)
		if flag != "" && strings.EqualFold(o.Login, flag) {
			return strconv.FormatInt(o.ID, 10), nil
		}
	}
	if flag != "" {
		e := cli.Errf("not_found", "You can't use the organization %q, or it doesn't exist.", flag)
		e.Hint = "Your organizations: " + strings.Join(logins, ", ") + "."
		return "", e
	}
	switch len(orgs.Data) {
	case 1:
		return strconv.FormatInt(orgs.Data[0].ID, 10), nil
	case 0:
		e := cli.Errf("not_found", "Your credential doesn't reach any organization that uses Vitko Runners.")
		e.Hint = "Install the Vitko Runners app on your organization, or sign in as one of its members."
		return "", e
	}
	e := cli.Errf("input_required", "You belong to several organizations; say which one with --org.")
	e.Hint = "Your organizations: " + strings.Join(logins, ", ") + ". Or run `vitko config set org <login>`."
	return "", e
}

func orgCommand(env Env, path []string, summary, description string, flags []cli.Flag, args []cli.Arg, output string, errs []string,
	run func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error)) *cli.Command {
	return &cli.Command{
		Path: path, Summary: summary, Description: description, Args: args,
		Flags: append([]cli.Flag{orgFlag}, flags...), Network: true, Idempotent: true, Output: output, Errors: withErrors(errs...),
		Run: func(cx *cli.Ctx) (any, error) {
			ctx := context.Background()
			c := newClient(env)
			org, err := resolveOrg(ctx, env, c, cx.String("org"))
			if err != nil {
				return nil, err
			}
			return run(ctx, c, org, cx)
		},
	}
}

// APICommands are the commands that call the API.
func APICommands(env Env) []*cli.Command {
	cmds := []*cli.Command{loginCmd(env), logoutCmd(env), whoamiCmd(env), orgsCmd(env)}
	cmds = append(cmds, tokenCmds(env)...)
	cmds = append(cmds, jobCmds(env)...)
	cmds = append(cmds, usageCmd(env))
	cmds = append(cmds, limitCmds(env)...)
	return cmds
}

// ---- sign-in ------------------------------------------------------------------

func loginCmd(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"login"}, Summary: "Sign in to Vitko.",
		Description: "Prints a link and a code; open the link, check the code and approve. The sign-in is kept in <config dir>/credentials.json (readable only by you) and renewed automatically. " +
			"With --with-token, reads an API token from standard input instead. In GitHub Actions no sign-in is needed: vitko exchanges the job's own ID token.",
		Flags: []cli.Flag{
			{Name: "with-token", Type: cli.TypeBool, Summary: "Read an API token (vitko_pat_…) from standard input and store it."},
			{Name: "no-browser", Type: cli.TypeBool, Summary: "Don't try to open the link in a browser."},
		},
		Network: true, Mutates: true, NoDryRun: true, Output: "vitko.login/v1", Errors: withErrors("access_denied", "expired_token", "input_required"),
		Examples: []string{"vitko login", "printf %s \"$TOKEN\" | vitko login --with-token"},
		Run:      func(cx *cli.Ctx) (any, error) { return runLogin(env, cx) },
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			d := v.(map[string]any)
			fmt.Fprintf(w, "Signed in as %s.\n", str(d["actor"]))
			for _, o := range d["orgs"].([]map[string]any) {
				fmt.Fprintf(w, "  %s (%s)\n", str(o["login"]), str(o["role"]))
			}
			return nil
		},
	}
}

func runLogin(env Env, cx *cli.Ctx) (any, error) {
	ctx := context.Background()
	c := newClient(env)
	store := c.Store
	if cx.Bool("with-token") {
		line, err := bufio.NewReader(env.Stdin).ReadString('\n')
		token := strings.TrimSpace(line)
		if (err != nil && err != io.EOF) || !strings.HasPrefix(token, "vitko_pat_") {
			return nil, cli.Errf("input_required", "--with-token reads an API token (vitko_pat_…) from standard input.")
		}
		probe := *c
		probe.Getenv = func(k string) string {
			if k == "VITKO_TOKEN" {
				return token
			}
			return ""
		}
		me, err := probe.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/me"})
		if err != nil {
			return nil, err
		}
		if err := store.Put(c.Origin(), &api.Credential{Kind: "token", APIToken: token, SignedInAt: env.Now().UTC()}); err != nil {
			return nil, cli.Errf("internal", "Couldn't save the token: %v", err)
		}
		return loginDoc(c, me, "token"), nil
	}
	start, err := c.Do(ctx, api.Request{Method: http.MethodPost, Path: "/v1/auth/device", Body: map[string]string{"client": "vitko-cli"}, Anonymous: true})
	if err != nil {
		return nil, err
	}
	var device struct {
		DeviceCode string    `json:"device_code"`
		UserCode   string    `json:"user_code"`
		URI        string    `json:"verification_uri"`
		URIFull    string    `json:"verification_uri_complete"`
		ExpiresAt  time.Time `json:"expires_at"`
		Interval   int       `json:"interval"`
	}
	if err := json.Unmarshal(start, &device); err != nil || device.DeviceCode == "" {
		return nil, cli.Errf("unavailable", "The sign-in couldn't be started.")
	}
	if cx.Mode == "text" {
		fmt.Fprintf(cx.Stderr(), "Open %s\nand check that it shows the code %s, then approve.\nWaiting…\n", device.URIFull, device.UserCode)
	} else {
		pending, _ := json.Marshal(map[string]any{"schema": "vitko.login-pending/v1", "verification_uri": device.URI, "verification_uri_complete": device.URIFull, "user_code": device.UserCode, "expires_at": device.ExpiresAt})
		fmt.Fprintln(cx.Stderr(), string(pending))
	}
	if env.StdoutIsTerminal && !cx.Bool("no-browser") && env.OpenBrowser != nil {
		_ = env.OpenBrowser(device.URIFull)
	}
	interval := time.Duration(max(device.Interval, 1)) * time.Second
	for {
		if !env.Now().Before(device.ExpiresAt) {
			return nil, cli.Errf("expired_token", "The sign-in code expired before it was approved.").WithFix("vitko login")
		}
		env.Sleep(interval)
		doc, err := c.Do(ctx, api.Request{Method: http.MethodPost, Path: "/v1/auth/device/token", Body: map[string]string{"device_code": device.DeviceCode}, Anonymous: true})
		if err != nil {
			ce := cli.AsError(err)
			switch ce.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			}
			return nil, err
		}
		var tokens api.TokenDocument
		if err := json.Unmarshal(doc, &tokens); err != nil || tokens.AccessToken == "" {
			return nil, cli.Errf("unavailable", "The sign-in answer couldn't be read.")
		}
		if err := store.Put(c.Origin(), &api.Credential{Kind: "user", AccessToken: tokens.AccessToken, AccessExpiresAt: tokens.ExpiresAt, RefreshToken: tokens.RefreshToken,
			RefreshExpiresAt: tokens.RefreshExpiresAt, Login: tokens.User.Login, SignedInAt: tokens.SignedInAt}); err != nil {
			return nil, cli.Errf("internal", "Couldn't save the sign-in: %v", err)
		}
		return loginDoc(c, doc, "user"), nil
	}
}

func loginDoc(c *api.Client, doc json.RawMessage, kind string) map[string]any {
	var d struct {
		Actor string `json:"actor"`
		User  *struct {
			Login string `json:"login"`
		} `json:"user"`
		Orgs []map[string]any `json:"orgs"`
	}
	_ = json.Unmarshal(doc, &d)
	actor := d.Actor
	if actor == "" && d.User != nil {
		actor = d.User.Login
	}
	orgs := []map[string]any{}
	for _, o := range d.Orgs {
		orgs = append(orgs, map[string]any{"id": o["id"], "login": o["login"], "role": o["role"], "scopes": o["scopes"]})
	}
	return map[string]any{"schema": "vitko.login/v1", "kind": kind, "actor": actor, "api_url": c.Origin(), "credential_file": c.Store.Path, "orgs": orgs}
}

func logoutCmd(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"logout"}, Summary: "Sign out: end the stored sign-in and delete it.",
		Network: true, Mutates: true, NoDryRun: true, Idempotent: true, Output: "vitko.logout/v1",
		Run: func(cx *cli.Ctx) (any, error) {
			c := newClient(env)
			cred, ok, err := c.Store.Get(c.Origin())
			if err != nil {
				return nil, cli.Errf("invalid_input", "%v", err)
			}
			ended := false
			if ok && cred.Kind == "user" && cred.RefreshToken != "" {
				if _, err := c.Do(context.Background(), api.Request{Method: http.MethodPost, Path: "/v1/auth/logout", Body: map[string]string{"refresh_token": cred.RefreshToken}, Anonymous: true}); err == nil {
					ended = true
				}
			}
			if ok {
				if err := c.Store.Put(c.Origin(), nil); err != nil {
					return nil, cli.Errf("internal", "Couldn't delete the sign-in: %v", err)
				}
			}
			return map[string]any{"schema": "vitko.logout/v1", "signed_out": ok, "server_session_ended": ended, "api_url": c.Origin()}, nil
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			if v.(map[string]any)["signed_out"] == true {
				_, err := fmt.Fprintln(w, "Signed out.")
				return err
			}
			_, err := fmt.Fprintln(w, "You weren't signed in.")
			return err
		},
	}
}

func whoamiCmd(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"whoami"}, Summary: "Show who you're signed in as, and what you can do.",
		Network: true, Idempotent: true, Output: "vitko.whoami/v1", Errors: withErrors(),
		Run: func(*cli.Ctx) (any, error) {
			return newClient(env).Do(context.Background(), api.Request{Method: http.MethodGet, Path: "/v1/me"})
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			d := raw(v)
			fmt.Fprintf(w, "%s (%s)\n", str(d["actor"]), str(d["kind"]))
			orgs, _ := d["orgs"].([]any)
			for _, o := range orgs {
				om := o.(map[string]any)
				fmt.Fprintf(w, "  %-24s %s\n", str(om["login"]), joinList(om["scopes"]))
			}
			return nil
		},
	}
}

func orgsCmd(env Env) *cli.Command {
	return &cli.Command{
		Path: []string{"orgs", "list"}, Summary: "List the organizations you can use.",
		Network: true, Idempotent: true, Output: "vitko.orgs/v1", Errors: withErrors(),
		Run: func(*cli.Ctx) (any, error) {
			return newClient(env).Do(context.Background(), api.Request{Method: http.MethodGet, Path: "/v1/orgs"})
		},
		Text: func(_ *cli.Ctx, w io.Writer, v any) error {
			data, _ := raw(v)["data"].([]any)
			for _, o := range data {
				om := o.(map[string]any)
				fmt.Fprintf(w, "%-12s %-24s %s\n", str(om["id"]), str(om["login"]), str(om["role"]))
			}
			return nil
		},
	}
}

// ---- API tokens -------------------------------------------------------------------

func tokenCmds(env Env) []*cli.Command {
	list := orgCommand(env, []string{"tokens", "list"}, "List the organization's API tokens.", "", nil, nil, "vitko.tokens/v1", nil,
		func(ctx context.Context, c *api.Client, org string, _ *cli.Ctx) (any, error) {
			return c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/tokens"})
		})
	list.Text = func(_ *cli.Ctx, w io.Writer, v any) error {
		data, _ := raw(v)["data"].([]any)
		for _, t := range data {
			tm := t.(map[string]any)
			state := "active"
			if tm["active"] != true {
				state = "inactive"
			}
			fmt.Fprintf(w, "%-30s %-20s %-9s …%s  %s\n", str(tm["id"]), str(tm["name"]), state, str(tm["hint"]), joinList(tm["scopes"]))
		}
		return nil
	}
	create := orgCommand(env, []string{"tokens", "create"}, "Create an API token. It is shown once.",
		"Needs an organization admin who signed in within the last 12 hours. Store the token as a secret and pass it as VITKO_TOKEN.",
		[]cli.Flag{
			{Name: "name", Type: cli.TypeString, Placeholder: "NAME", Summary: "What the token is for."},
			{Name: "scopes", Type: cli.TypeStrings, Placeholder: "LIST", Summary: "Scopes: runners:read, runners:jobs:write, runners:limits:write, runners:config:write, billing:read."},
			{Name: "repos", Type: cli.TypeStrings, Placeholder: "LIST", Summary: "Limit the token to these repositories' jobs (owner/name)."},
			{Name: "expires-in-days", Type: cli.TypeInt, Default: 90, Placeholder: "DAYS", Summary: "Days until the token stops working (1 to 365)."},
			{Name: "dry-run", Type: cli.TypeBool, Summary: "Show what would be created without creating it."},
		}, nil, "vitko.token/v1", []string{"human_session_required", "fresh_sign_in_required", "input_required"},
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			if cx.String("name") == "" || len(cx.Strings("scopes")) == 0 {
				return nil, cli.Errf("input_required", "Give the token a --name and its --scopes.")
			}
			body := map[string]any{"name": cx.String("name"), "scopes": cx.Strings("scopes"), "repositories": nonNilList(cx.Strings("repos")), "expires_in_days": cx.Int("expires-in-days")}
			if cx.Bool("dry-run") {
				return map[string]any{"schema": "vitko.plan/v1", "dry_run": true, "would_succeed": nil, "would_succeed_unavailable_reason": "Token creation is checked when it happens.",
					"changes": []map[string]any{{"resource": "orgs/" + org + "/tokens", "field": "token", "from": nil, "to": body}}, "blocked_by": nil}, nil
			}
			return c.Do(ctx, api.Request{Method: http.MethodPost, Path: "/v1/orgs/" + org + "/tokens", Body: body})
		})
	create.Mutates, create.Idempotent = true, false
	create.Text = func(_ *cli.Ctx, w io.Writer, v any) error {
		d := raw(v)
		if d["schema"] == "vitko.plan/v1" || len(d) == 0 {
			b, _ := json.MarshalIndent(v, "", "  ")
			_, err := fmt.Fprintln(w, string(b))
			return err
		}
		fmt.Fprintf(w, "Created token %s. This is the only time it is shown:\n\n  %s\n\n", str(raw(v)["data"].(map[string]any)["id"]), str(d["token"]))
		return nil
	}
	revoke := orgCommand(env, []string{"tokens", "revoke"}, "Revoke an API token.", "Needs an organization admin who signed in within the last 12 hours.",
		[]cli.Flag{{Name: "dry-run", Type: cli.TypeBool, Summary: "Show what would be revoked without revoking it."}},
		[]cli.Arg{{Name: "id", Summary: "The token's id (tok_…).", Required: true}}, "vitko.token/v1", []string{"human_session_required", "fresh_sign_in_required"},
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			if cx.Bool("dry-run") {
				return map[string]any{"schema": "vitko.plan/v1", "dry_run": true, "would_succeed": nil, "would_succeed_unavailable_reason": "Revocation is checked when it happens.",
					"changes": []map[string]any{{"resource": "orgs/" + org + "/tokens/" + cx.Args[0], "field": "revoked", "from": false, "to": true}}, "blocked_by": nil}, nil
			}
			return c.Do(ctx, api.Request{Method: http.MethodDelete, Path: "/v1/orgs/" + org + "/tokens/" + url.PathEscape(cx.Args[0])})
		})
	revoke.Mutates = true
	return []*cli.Command{list, create, revoke}
}

func nonNilList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---- jobs, usage ------------------------------------------------------------------

func jobCmds(env Env) []*cli.Command {
	list := orgCommand(env, []string{"runners", "jobs", "list"}, "List jobs, newest first.", "",
		[]cli.Flag{
			{Name: "repo", Type: cli.TypeString, Placeholder: "OWNER/NAME", Summary: "Only this repository's jobs."},
			{Name: "status", Type: cli.TypeString, Placeholder: "STATUS", Summary: "Only jobs with this status (for example running, completed)."},
			{Name: "since", Type: cli.TypeString, Enum: []string{"24h", "7d", "30d"}, Default: "7d", Summary: "How far back."},
			{Name: "limit", Type: cli.TypeInt, Default: 100, Placeholder: "N", Summary: "Jobs per page (1 to 1000)."},
			{Name: "cursor", Type: cli.TypeString, Placeholder: "CURSOR", Summary: "Continue from a previous page's next_cursor."},
			{Name: "all", Type: cli.TypeBool, Summary: "Fetch every page."},
		}, nil, "vitko.runners.jobs/v1", nil,
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			q := url.Values{"since": {cx.String("since")}, "limit": {strconv.Itoa(cx.Int("limit"))}}
			for _, k := range []string{"repo", "status", "cursor"} {
				if v := cx.String(k); v != "" {
					q.Set(k, v)
				}
			}
			doc, err := c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/jobs", Query: q})
			if err != nil || !cx.Bool("all") {
				return doc, err
			}
			var page map[string]any
			_ = json.Unmarshal(doc, &page)
			all, _ := page["data"].([]any)
			for i := 0; i < 100 && page["next_cursor"] != nil; i++ {
				q.Set("cursor", str(page["next_cursor"]))
				next, err := c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/jobs", Query: q})
				if err != nil {
					return nil, err
				}
				page = map[string]any{}
				_ = json.Unmarshal(next, &page)
				more, _ := page["data"].([]any)
				all = append(all, more...)
			}
			page["data"], page["next_cursor"] = all, nil
			return page, nil
		})
	list.Text = func(_ *cli.Ctx, w io.Writer, v any) error {
		d := raw(v)
		if len(d) == 0 {
			d, _ = v.(map[string]any)
		}
		data, _ := d["data"].([]any)
		for _, j := range data {
			jm := j.(map[string]any)
			fmt.Fprintf(w, "%-24s %-10s %-30s %s\n", str(jm["id"]), str(jm["status"]), str(jm["repository"]), str(jm["name"]))
		}
		if d["next_cursor"] != nil {
			fmt.Fprintf(w, "More: --cursor %s\n", str(d["next_cursor"]))
		}
		return nil
	}
	show := orgCommand(env, []string{"runners", "jobs", "show"}, "Show one job.", "", nil,
		[]cli.Arg{{Name: "job", Summary: "The job's id.", Required: true}}, "vitko.runners.job/v1", nil,
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			return c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/jobs/" + url.PathEscape(cx.Args[0])})
		})
	show.Text = func(_ *cli.Ctx, w io.Writer, v any) error {
		data, _ := raw(v)["data"].(map[string]any)
		for _, k := range []string{"id", "status", "repository", "workflow", "name", "run_url", "queued_at", "started_at", "finished_at", "billed_seconds", "cost_micros", "github_list_micros"} {
			fmt.Fprintf(w, "%-20s %s\n", k, str(data[k]))
		}
		return nil
	}
	return []*cli.Command{list, show}
}

func usageCmd(env Env) *cli.Command {
	cmd := orgCommand(env, []string{"runners", "usage"}, "Show a month's minutes and cost, next to GitHub's list price.", "",
		[]cli.Flag{{Name: "month", Type: cli.TypeString, Placeholder: "YYYY-MM", Summary: "The month (UTC). Default: this month."}}, nil, "vitko.runners.usage/v1", []string{"usage"},
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			q := url.Values{}
			if m := cx.String("month"); m != "" {
				if !monthRe.MatchString(m) {
					return nil, cli.Errf("usage", "--month %q is not in the form YYYY-MM.", m)
				}
				q.Set("month", m)
			}
			return c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/usage", Query: q})
		})
	cmd.Text = func(_ *cli.Ctx, w io.Writer, v any) error {
		d := raw(v)
		data, _ := d["data"].(map[string]any)
		fmt.Fprintf(w, "Month %s (UTC)\n", str(d["month"]))
		fmt.Fprintf(w, "  Jobs            %s\n  Billed seconds  %s\n  Gross           %s\n  Free minutes    %s\n  Paid            %s\n  GitHub list     %s\n",
			str(data["jobs"]), str(data["billed_seconds"]), usdMicros(data["gross_micros"]), usdMicros(data["free_micros"]), usdMicros(data["paid_micros"]), usdMicros(data["github_list_micros"]))
		return nil
	}
	return cmd
}

// ---- limits and ceilings -------------------------------------------------------------

func moneyFlags(prefix string) []cli.Flag {
	return []cli.Flag{
		{Name: prefix + "-usd", Type: cli.TypeNumber, Placeholder: "USD", Summary: "The monthly amount in US dollars, after free minutes."},
		{Name: prefix + "-micros", Type: cli.TypeInt, Placeholder: "MICROS", Summary: "The same in micros of USD (1 USD = 1,000,000)."},
	}
}

// moneyField reads --X-usd / --X-micros / --no-X into a JSON value.
func moneyField(cx *cli.Ctx, prefix, none string, body map[string]any, field string) error {
	set := 0
	if cx.IsSet(prefix + "-usd") {
		v := cx.Number(prefix + "-usd")
		if v < 0 || v > 1_000_000 {
			return cli.Errf("usage", "--%s-usd must be between 0 and 1000000.", prefix)
		}
		body[field] = int64(math.Round(v * 1e6))
		set++
	}
	if cx.IsSet(prefix + "-micros") {
		body[field] = cx.Int(prefix + "-micros")
		set++
	}
	if cx.Bool(none) {
		body[field] = nil
		set++
	}
	if set > 1 {
		return cli.Errf("usage", "Use only one of --%s-usd, --%s-micros and --%s.", prefix, prefix, none)
	}
	return nil
}

func limitCmds(env Env) []*cli.Command {
	limitsText := func(_ *cli.Ctx, w io.Writer, v any) error {
		d := raw(v)
		if d["schema"] == "vitko.plan/v1" {
			fmt.Fprintf(w, "Dry run: would succeed: %s\n", str(d["would_succeed"]))
			changes, _ := d["changes"].([]any)
			for _, c := range changes {
				cm := c.(map[string]any)
				fmt.Fprintf(w, "  %s: %s -> %s\n", str(cm["field"]), str(cm["from"]), str(cm["to"]))
			}
			if b, ok := d["blocked_by"].(map[string]any); ok {
				fmt.Fprintf(w, "Blocked: %s\n", str(b["message"]))
			}
			return nil
		}
		data, _ := d["data"].(map[string]any)
		fmt.Fprintf(w, "Spend limit        %s (ceiling %s)\n", usdMicros(data["spend_limit_micros"]), usdMicros(data["spend_ceiling_micros"]))
		fmt.Fprintf(w, "Jobs at once       %s (ceiling %s, default %s)\n", str(data["concurrency_cap"]), str(data["concurrency_ceiling"]), str(data["default_concurrency"]))
		fmt.Fprintf(w, "New jobs           %s\n", str(data["new_jobs"]))
		if r := str(data["waiting_reason"]); r != "-" {
			fmt.Fprintf(w, "Waiting because    %s\n", r)
		}
		if can, ok := data["can_raise_to"].(map[string]any); ok {
			fmt.Fprintf(w, "You can set up to  %s, %s jobs at once\n", usdMicros(can["spend_limit_micros"]), str(can["concurrency_cap"]))
		}
		fmt.Fprintf(w, "Revision           %s\n", str(data["revision"]))
		return nil
	}
	show := orgCommand(env, []string{"runners", "limits", "show"}, "Show spend and concurrency limits, their ceilings, and what you may set.", "", nil, nil, "vitko.runners.limits/v1", nil,
		func(ctx context.Context, c *api.Client, org string, _ *cli.Ctx) (any, error) {
			return c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/limits"})
		})
	show.Text = limitsText
	set := orgCommand(env, []string{"runners", "limits", "set"}, "Set the spend limit or the number of jobs at once.",
		"States the target value; repeating it changes nothing. API tokens and CI jobs may lower a limit at any time and raise it only up to the ceiling an organization admin set; with no ceiling, only lower it. At the spend limit new jobs wait and running jobs finish.",
		append(append(moneyFlags("spend"),
			cli.Flag{Name: "no-spend-limit", Type: cli.TypeBool, Summary: "Remove the spend limit."},
			cli.Flag{Name: "concurrency", Type: cli.TypeInt, Placeholder: "N", Summary: "The most jobs that may run at once."},
			cli.Flag{Name: "default-concurrency", Type: cli.TypeBool, Summary: "Go back to the default number of jobs at once."},
			cli.Flag{Name: "if-revision", Type: cli.TypeInt, Placeholder: "N", Summary: "Only change the limits if they are still at this revision."},
		), cli.Flag{Name: "dry-run", Type: cli.TypeBool, Summary: "Check the change with the API without making it."}),
		nil, "vitko.runners.limits/v1", []string{"usage", "input_required", "limit_above_ceiling", "ceiling_not_set", "revision_conflict"},
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			body := map[string]any{}
			if err := moneyField(cx, "spend", "no-spend-limit", body, "spend_limit_micros"); err != nil {
				return nil, err
			}
			if cx.IsSet("concurrency") && cx.Bool("default-concurrency") {
				return nil, cli.Errf("usage", "Use only one of --concurrency and --default-concurrency.")
			}
			if cx.IsSet("concurrency") {
				body["concurrency_cap"] = cx.Int("concurrency")
			}
			if cx.Bool("default-concurrency") {
				body["concurrency_cap"] = nil
			}
			if len(body) == 0 {
				return nil, cli.Errf("input_required", "Say what to change: --spend-usd, --spend-micros, --no-spend-limit, --concurrency or --default-concurrency.")
			}
			req := api.Request{Method: http.MethodPatch, Path: "/v1/orgs/" + org + "/limits", Body: body, Headers: map[string]string{}}
			if cx.Bool("dry-run") {
				req.Query = url.Values{"dry_run": {"true"}}
			}
			if cx.IsSet("if-revision") {
				req.Headers["If-Match"] = strconv.Quote(strconv.Itoa(cx.Int("if-revision")))
			}
			return c.Do(ctx, req)
		})
	set.Mutates, set.Text = true, limitsText
	ceilingText := func(_ *cli.Ctx, w io.Writer, v any) error {
		d := raw(v)
		if d["schema"] == "vitko.plan/v1" {
			return limitsText(nil, w, v)
		}
		data, _ := d["data"].(map[string]any)
		fmt.Fprintf(w, "Spend ceiling          %s\nJobs-at-once ceiling   %s\nRevision               %s\n", usdMicros(data["spend_ceiling_micros"]), str(data["concurrency_ceiling"]), str(data["revision"]))
		return nil
	}
	ceilingShow := orgCommand(env, []string{"runners", "limits", "ceiling", "show"}, "Show the ceilings tokens can't set limits above.", "", nil, nil, "vitko.runners.ceiling/v1", nil,
		func(ctx context.Context, c *api.Client, org string, _ *cli.Ctx) (any, error) {
			return c.Do(ctx, api.Request{Method: http.MethodGet, Path: "/v1/orgs/" + org + "/limits/ceiling"})
		})
	ceilingShow.Text = ceilingText
	ceilingSet := orgCommand(env, []string{"runners", "limits", "ceiling", "set"}, "Set the ceilings (an organization admin, signed in within 12 hours).",
		"API tokens and CI jobs can set limits at or below the ceilings. Only an organization admin who signed in recently can change them; tokens can't.",
		append(append(moneyFlags("spend"),
			cli.Flag{Name: "no-spend-ceiling", Type: cli.TypeBool, Summary: "Remove the spend ceiling (tokens can then only lower the spend limit)."},
			cli.Flag{Name: "concurrency", Type: cli.TypeInt, Placeholder: "N", Summary: "The most jobs at once a token may set."},
			cli.Flag{Name: "no-concurrency-ceiling", Type: cli.TypeBool, Summary: "Remove the jobs-at-once ceiling."},
			cli.Flag{Name: "if-revision", Type: cli.TypeInt, Placeholder: "N", Summary: "Only change them if the limits are still at this revision."},
		), cli.Flag{Name: "dry-run", Type: cli.TypeBool, Summary: "Check the change with the API without making it."}),
		nil, "vitko.runners.ceiling/v1", []string{"usage", "input_required", "human_session_required", "fresh_sign_in_required", "revision_conflict"},
		func(ctx context.Context, c *api.Client, org string, cx *cli.Ctx) (any, error) {
			body := map[string]any{}
			if err := moneyField(cx, "spend", "no-spend-ceiling", body, "spend_ceiling_micros"); err != nil {
				return nil, err
			}
			if cx.IsSet("concurrency") {
				body["concurrency_ceiling"] = cx.Int("concurrency")
			}
			if cx.Bool("no-concurrency-ceiling") {
				body["concurrency_ceiling"] = nil
			}
			if len(body) == 0 {
				return nil, cli.Errf("input_required", "Say what to change: --spend-usd, --spend-micros, --no-spend-ceiling, --concurrency or --no-concurrency-ceiling.")
			}
			req := api.Request{Method: http.MethodPut, Path: "/v1/orgs/" + org + "/limits/ceiling", Body: body, Headers: map[string]string{}}
			if cx.Bool("dry-run") {
				req.Query = url.Values{"dry_run": {"true"}}
			}
			if cx.IsSet("if-revision") {
				req.Headers["If-Match"] = strconv.Quote(strconv.Itoa(cx.Int("if-revision")))
			}
			return c.Do(ctx, req)
		})
	ceilingSet.Mutates, ceilingSet.Text = true, ceilingText
	return []*cli.Command{show, set, ceilingShow, ceilingSet}
}
