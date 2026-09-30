package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var apiNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// fakeAPI is a small Vitko Runners API: enough to drive every command.
type fakeAPI struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	polls    int
	refresh  int
	bearers  map[string]string // bearer -> actor
	orgs     []map[string]any
	requests []string
	lastBody map[string]any
	lastHdr  http.Header
	lastQ    string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, bearers: map[string]string{"vitko_pat_env": "token:tok_env", "vitko_at_ci": "oidc:acme/web@refs/heads/main"},
		orgs: []map[string]any{{"id": 42, "login": "acme", "role": "admin", "scopes": []string{"runners:read"}}}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) fail(w http.ResponseWriter, status int, code, message string) {
	f.reply(w, status, map[string]any{"schema": "vitko.error/v1", "error": map[string]any{"code": code, "message": message, "retryable": false, "hint": "a hint", "request_id": "req_1",
		"fix": map[string]string{"url": "https://app.example"}}})
}

func (f *fakeAPI) tokens(w http.ResponseWriter, access, refresh string) {
	f.reply(w, 200, map[string]any{"schema": "vitko.auth-token/v1", "token_type": "Bearer", "access_token": access, "expires_in": 3600, "expires_at": apiNow.Add(time.Hour),
		"refresh_token": refresh, "refresh_expires_at": apiNow.Add(720 * time.Hour), "kind": "user", "user": map[string]any{"id": 7, "login": "alice"},
		"orgs": []any{map[string]any{"id": 42, "login": "acme", "role": "admin", "scopes": []string{"runners:read"}}}, "signed_in_at": apiNow})
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.lastBody, f.lastHdr, f.lastQ = map[string]any{}, r.Header.Clone(), r.URL.RawQuery
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &f.lastBody)
	switch r.URL.Path {
	case "/actions-token":
		if r.Header.Get("Authorization") != "Bearer req-token" || r.URL.Query().Get("audience") != f.server.URL {
			w.WriteHeader(403)
			return
		}
		f.reply(w, 200, map[string]string{"value": "gh-id-token"})
		return
	case "/v1/auth/device":
		f.reply(w, 200, map[string]any{"schema": "vitko.device-authorization/v1", "device_code": "vitko_dc_1", "user_code": "BCDF-GHJK", "verification_uri": "https://app.example/device",
			"verification_uri_complete": "https://app.example/device/BCDF-GHJK", "expires_in": 600, "expires_at": apiNow.Add(10 * time.Minute), "interval": 5})
		return
	case "/v1/auth/device/token":
		f.polls++
		switch f.polls {
		case 1:
			f.fail(w, 400, "authorization_pending", "waiting")
		case 2:
			f.fail(w, 400, "slow_down", "slower")
		default:
			f.bearers["vitko_at_1"] = "github:alice#7"
			f.tokens(w, "vitko_at_1", "vitko_rt_1")
		}
		return
	case "/v1/auth/refresh":
		f.refresh++
		if f.lastBody["refresh_token"] != "vitko_rt_1" {
			f.fail(w, 401, "token_expired", "ended")
			return
		}
		f.bearers["vitko_at_2"] = "github:alice#7"
		f.tokens(w, "vitko_at_2", "vitko_rt_1")
		return
	case "/v1/auth/logout":
		f.reply(w, 200, map[string]any{"schema": "vitko.logout/v1", "signed_out": true})
		return
	case "/v1/auth/oidc/github":
		if f.lastBody["id_token"] != "gh-id-token" {
			f.fail(w, 401, "token_invalid", "bad id token")
			return
		}
		f.reply(w, 200, map[string]any{"schema": "vitko.auth-token/v1", "token_type": "Bearer", "access_token": "vitko_at_ci", "expires_in": 900, "expires_at": apiNow.Add(15 * time.Minute), "kind": "oidc"})
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	actor, ok := f.bearers[bearer]
	if !ok {
		f.fail(w, 401, "token_invalid", "The token isn't valid.")
		return
	}
	switch {
	case r.URL.Path == "/v1/me":
		f.reply(w, 200, map[string]any{"schema": "vitko.whoami/v1", "kind": "user", "actor": actor, "orgs": f.orgs, "repositories": []string{}})
	case r.URL.Path == "/v1/orgs":
		f.reply(w, 200, map[string]any{"schema": "vitko.orgs/v1", "data": f.orgs})
	case r.URL.Path == "/v1/orgs/42/tokens" && r.Method == "GET":
		f.reply(w, 200, map[string]any{"schema": "vitko.tokens/v1", "data": []any{map[string]any{"id": "tok_1", "name": "ci", "hint": "ab12", "scopes": []string{"runners:read"}, "repositories": []string{}, "expires_at": apiNow.Add(time.Hour), "revoked_at": nil, "active": true}}})
	case r.URL.Path == "/v1/orgs/42/tokens" && r.Method == "POST":
		f.reply(w, 201, map[string]any{"schema": "vitko.token/v1", "token": "vitko_pat_new", "data": map[string]any{"id": "tok_2", "name": f.lastBody["name"], "scopes": f.lastBody["scopes"], "expires_at": apiNow.Add(time.Hour)}})
	case r.URL.Path == "/v1/orgs/42/tokens/tok_2" && r.Method == "DELETE":
		f.reply(w, 200, map[string]any{"schema": "vitko.token/v1", "data": map[string]any{"id": "tok_2", "name": "x", "scopes": []string{}, "expires_at": apiNow, "revoked_at": apiNow}})
	case r.URL.Path == "/v1/orgs/42/jobs":
		page := map[string]any{"schema": "vitko.runners.jobs/v1", "data": []any{map[string]any{"id": "j1", "status": "completed", "repository": "acme/web"}}, "next_cursor": "c2", "since": "7d", "time_zone": "UTC"}
		if r.URL.Query().Get("cursor") == "c2" {
			page["data"], page["next_cursor"] = []any{map[string]any{"id": "j2", "status": "running", "repository": "acme/api"}}, nil
		}
		f.reply(w, 200, page)
	case r.URL.Path == "/v1/orgs/42/jobs/j1":
		f.reply(w, 200, map[string]any{"schema": "vitko.runners.job/v1", "data": map[string]any{"id": "j1", "status": "completed", "repository": "acme/web"}})
	case r.URL.Path == "/v1/orgs/42/jobs/nope":
		f.fail(w, 404, "not_found", "Not found, or not visible to you.")
	case r.URL.Path == "/v1/orgs/42/usage":
		f.reply(w, 200, map[string]any{"schema": "vitko.runners.usage/v1", "month": "2026-10", "data": map[string]any{"jobs": 3, "billed_seconds": 90, "gross_micros": 3000, "paid_micros": 0, "github_list_micros": 9000}})
	case r.URL.Path == "/v1/orgs/42/limits" && r.Method == "GET":
		f.reply(w, 200, map[string]any{"schema": "vitko.runners.limits/v1", "data": map[string]any{"revision": 3, "spend_limit_micros": 100000000, "spend_ceiling_micros": 300000000, "new_jobs": "start",
			"can_raise_to": map[string]any{"spend_limit_micros": 300000000, "concurrency_cap": 8}}})
	case r.URL.Path == "/v1/orgs/42/limits" && r.Method == "PATCH":
		if v, _ := f.lastBody["spend_limit_micros"].(float64); v > 300000000 {
			f.fail(w, 422, "limit_above_ceiling", "The spend limit you asked for is above your organization's ceiling ($300.00).")
			return
		}
		if r.URL.Query().Get("dry_run") == "true" {
			f.reply(w, 200, map[string]any{"schema": "vitko.plan/v1", "dry_run": true, "would_succeed": true, "changes": []any{}, "blocked_by": nil, "revision": 3})
			return
		}
		f.reply(w, 200, map[string]any{"schema": "vitko.runners.limits/v1", "data": map[string]any{"revision": 4, "changed": true}})
	case r.URL.Path == "/v1/orgs/42/limits/ceiling":
		if r.Method == "PUT" && strings.HasPrefix(actor, "token:") {
			f.fail(w, 403, "human_session_required", "Only an organization admin, signed in, can do this.")
			return
		}
		f.reply(w, 200, map[string]any{"schema": "vitko.runners.ceiling/v1", "data": map[string]any{"revision": 5, "spend_ceiling_micros": 500000000, "concurrency_ceiling": nil, "currency": "USD"}})
	default:
		f.fail(w, 404, "not_found", "no route")
	}
}

func apiHarness(t *testing.T) (*harness, *fakeAPI) {
	f := newFakeAPI(t)
	h := newHarness(t)
	h.http = f.server.Client()
	h.env["VITKO_RUNNERS_API_URL"] = f.server.URL
	return h, f
}

func TestLoginStoresTheSignInAndCommandsUseAndRefreshIt(t *testing.T) {
	validate := validator(t)
	h, f := apiHarness(t)
	h.terminal = true
	r := h.run("login", "--output", "json")
	if r.code != 0 {
		t.Fatalf("login: %d %s", r.code, r.stderr)
	}
	if err := validate("vitko.login/v1", []byte(r.stdout)); err != nil || !strings.Contains(r.stdout, `"actor": "alice"`) {
		t.Errorf("login output: %v\n%s", err, r.stdout)
	}
	pending := strings.SplitN(r.stderr, "\n", 2)[0]
	if err := validate("vitko.login-pending/v1", []byte(pending)); err != nil || !strings.Contains(pending, "BCDF-GHJK") {
		t.Errorf("pending line: %v %s", err, pending)
	}
	if len(h.opened) != 1 || h.opened[0] != "https://app.example/device/BCDF-GHJK" {
		t.Errorf("browser: %v", h.opened)
	}
	h.terminal = false
	path := filepath.Join(h.env["VITKO_CONFIG_DIR"], "credentials.json")
	st, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("credential file: %v %v", st, err)
	}
	r = h.run("whoami")
	if r.code != 0 || validate("vitko.whoami/v1", []byte(r.stdout)) != nil {
		t.Fatalf("whoami: %d %s %s", r.code, r.stdout, r.stderr)
	}
	// Two hours later the access token has expired: it is refreshed once.
	h.now = apiNow.Add(2 * time.Hour)
	if r := h.run("whoami"); r.code != 0 || f.refresh != 1 {
		t.Fatalf("refresh: %d refreshes=%d %s", r.code, f.refresh, r.stderr)
	}
	stored, _ := os.ReadFile(path)
	if !strings.Contains(string(stored), "vitko_at_2") {
		t.Errorf("refreshed token not stored")
	}
	r = h.run("logout")
	if r.code != 0 || validate("vitko.logout/v1", []byte(r.stdout)) != nil || !strings.Contains(r.stdout, `"server_session_ended": true`) {
		t.Fatalf("logout: %d %s", r.code, r.stdout)
	}
	if r := h.run("whoami"); r.code != 3 || !strings.Contains(r.stderr, `"code": "signed_out"`) || !strings.Contains(r.stderr, `"command": "vitko login"`) {
		t.Fatalf("after logout: %d %s", r.code, r.stderr)
	}
}

func TestLoginWithToken(t *testing.T) {
	h, f := apiHarness(t)
	h.stdin = "vitko_pat_env\n"
	if r := h.run("login", "--with-token"); r.code != 0 {
		t.Fatalf("login --with-token: %d %s", r.code, r.stderr)
	}
	f.requests = nil
	if r := h.run("orgs", "list"); r.code != 0 || !strings.Contains(r.stdout, "acme") {
		t.Fatalf("orgs: %d %s", r.code, r.stderr)
	}
	h.stdin = "not-a-token\n"
	if r := h.run("login", "--with-token"); r.code != 2 {
		t.Fatalf("bad token: %d", r.code)
	}
}

func TestCredentialOrderEnvThenActionsThenLogin(t *testing.T) {
	h, f := apiHarness(t)
	h.env["VITKO_TOKEN"] = "vitko_pat_env"
	if r := h.run("whoami"); r.code != 0 || !strings.Contains(r.stdout, "token:tok_env") {
		t.Fatalf("VITKO_TOKEN: %d %s %s", r.code, r.stdout, r.stderr)
	}
	delete(h.env, "VITKO_TOKEN")
	h.env["ACTIONS_ID_TOKEN_REQUEST_URL"] = f.server.URL + "/actions-token?x=1"
	h.env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = "req-token"
	if r := h.run("whoami"); r.code != 0 || !strings.Contains(r.stdout, "oidc:acme/web") {
		t.Fatalf("GitHub Actions: %d %s %s", r.code, r.stdout, r.stderr)
	}
	h.env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = "wrong"
	if r := h.run("whoami"); r.code != 4 || !strings.Contains(r.stderr, "id-token: write") {
		t.Fatalf("no ID token: %d %s", r.code, r.stderr)
	}
	delete(h.env, "ACTIONS_ID_TOKEN_REQUEST_URL")
	delete(h.env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if r := h.run("whoami"); r.code != 3 {
		t.Fatalf("nothing: %d %s", r.code, r.stderr)
	}
	h.env["VITKO_TOKEN"] = "vitko_pat_revoked"
	if r := h.run("whoami"); r.code != 3 || !strings.Contains(r.stderr, "token_invalid") {
		t.Fatalf("revoked token: %d %s", r.code, r.stderr)
	}
}

func TestOrgCommandsAgainstTheAPI(t *testing.T) {
	validate := validator(t)
	h, f := apiHarness(t)
	h.env["VITKO_TOKEN"] = "vitko_pat_env"
	check := func(args ...string) result {
		t.Helper()
		r := h.run(args...)
		if r.code != 0 {
			t.Fatalf("%v: %d %s", args, r.code, r.stderr)
		}
		var d struct {
			Schema string `json:"schema"`
		}
		_ = json.Unmarshal([]byte(r.stdout), &d)
		if err := validate(d.Schema, []byte(r.stdout)); err != nil {
			t.Errorf("%v: %s doesn't match its schema: %v\n%s", args, d.Schema, err, r.stdout)
		}
		return r
	}
	check("tokens", "list")
	r := check("tokens", "create", "--name", "deploys", "--scopes", "runners:read,runners:limits:write", "--repos", "acme/web")
	if !strings.Contains(r.stdout, "vitko_pat_new") || f.lastBody["name"] != "deploys" || len(f.lastBody["scopes"].([]any)) != 2 || f.lastBody["expires_in_days"] != float64(90) {
		t.Errorf("create: %s %v", r.stdout, f.lastBody)
	}
	check("tokens", "create", "--name", "x", "--scopes", "runners:read", "--dry-run")
	check("tokens", "revoke", "tok_2")
	if r := check("runners", "jobs", "list", "--repo", "acme/web", "--since", "24h"); !strings.Contains(f.lastQ, "repo=acme%2Fweb") || !strings.Contains(f.lastQ, "since=24h") || !strings.Contains(r.stdout, `"next_cursor": "c2"`) {
		t.Errorf("jobs query: %s %s", f.lastQ, r.stdout)
	}
	if r := check("runners", "jobs", "list", "--all"); !strings.Contains(r.stdout, "j2") || !strings.Contains(r.stdout, `"next_cursor": null`) {
		t.Errorf("--all: %s", r.stdout)
	}
	check("runners", "jobs", "show", "j1")
	if r := h.run("runners", "jobs", "show", "nope"); r.code != 5 {
		t.Errorf("missing job: %d", r.code)
	}
	check("runners", "usage", "--month", "2026-10")
	check("runners", "limits", "show")
	check("runners", "limits", "set", "--spend-usd", "250", "--concurrency", "4")
	if f.lastBody["spend_limit_micros"] != float64(250000000) || f.lastBody["concurrency_cap"] != float64(4) {
		t.Errorf("limits body: %v", f.lastBody)
	}
	check("runners", "limits", "set", "--no-spend-limit", "--if-revision", "3", "--dry-run")
	if v, ok := f.lastBody["spend_limit_micros"]; !ok || v != nil || f.lastHdr.Get("If-Match") != `"3"` || f.lastQ != "dry_run=true" || len(f.lastBody) != 1 {
		t.Errorf("no-limit body: %v %v %s", f.lastBody, f.lastHdr.Get("If-Match"), f.lastQ)
	}
	r = h.run("runners", "limits", "set", "--spend-usd", "500")
	if r.code != 7 || !strings.Contains(r.stderr, `"code": "limit_above_ceiling"`) || !strings.Contains(r.stderr, `"hint": "a hint"`) || !strings.Contains(r.stderr, "req_1") {
		t.Errorf("above the ceiling: %d %s", r.code, r.stderr)
	}
	if r := h.run("runners", "limits", "set"); r.code != 2 {
		t.Errorf("nothing to set: %d", r.code)
	}
	if r := h.run("runners", "limits", "set", "--spend-usd", "1", "--spend-micros", "2"); r.code != 2 {
		t.Errorf("two spend flags: %d", r.code)
	}
	if r := h.run("runners", "limits", "ceiling", "set", "--spend-usd", "500"); r.code != 4 || !strings.Contains(r.stderr, "human_session_required") {
		t.Errorf("token sets the ceiling: %d %s", r.code, r.stderr)
	}
	check("runners", "limits", "ceiling", "show")
	// Organization by login, and several organizations need --org.
	if r := h.run("runners", "limits", "show", "--org", "ACME"); r.code != 0 {
		t.Errorf("--org by login: %d %s", r.code, r.stderr)
	}
	if r := h.run("runners", "limits", "show", "--org", "other"); r.code != 5 {
		t.Errorf("unknown org: %d %s", r.code, r.stderr)
	}
	f.orgs = append(f.orgs, map[string]any{"id": 43, "login": "beta", "role": "member", "scopes": []string{}})
	if r := h.run("runners", "limits", "show"); r.code != 2 || !strings.Contains(r.stderr, "acme, beta") {
		t.Errorf("several orgs: %d %s", r.code, r.stderr)
	}
	h.env["VITKO_ORG"] = "acme"
	if r := h.run("runners", "limits", "show"); r.code != 0 {
		t.Errorf("VITKO_ORG: %d %s", r.code, r.stderr)
	}
}

func TestUnreachableAPIIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.env["VITKO_RUNNERS_API_URL"] = "http://127.0.0.1:1"
	h.env["VITKO_TOKEN"] = "vitko_pat_x"
	if r := h.run("whoami"); r.code != 8 || !strings.Contains(r.stderr, `"code": "unavailable"`) {
		t.Fatalf("unreachable: %d %s", r.code, r.stderr)
	}
}
