// Package api talks to the Vitko Runners API. Credentials, in order:
// VITKO_TOKEN (an API token), then, inside a GitHub Actions job that may
// request an ID token, GitHub's OIDC token exchanged for a short-lived one,
// then the stored sign-in from `vitko login`, refreshed when it expires.
// There is no --token flag: flags leak into process lists and shell history.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vitko-inc/vitko/internal/cli"
	"github.com/vitko-inc/vitko/internal/version"
)

// DefaultRunnersURL is the Vitko Runners API.
const DefaultRunnersURL = "https://api.runners.vitko.inc"

// Client calls one product API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Getenv  func(string) string
	Store   CredentialFile
	Now     func() time.Time

	// resolved credential for this process
	bearer string
	source string
}

// Origin is the API origin credentials are stored under.
func (c *Client) Origin() string { return strings.TrimRight(c.BaseURL, "/") }

// Request is one API call.
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Body    any
	Headers map[string]string
	// Anonymous calls send no credential (sign-in routes, pricing).
	Anonymous bool
}

type apiErrorBody struct {
	Schema string    `json:"schema"`
	Error  cli.Error `json:"error"`
}

// Do sends the request and returns the response document, raw.
func (c *Client) Do(ctx context.Context, req Request) (json.RawMessage, error) {
	if !req.Anonymous {
		if err := c.authenticate(ctx); err != nil {
			return nil, err
		}
	}
	status, body, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	// A stored sign-in whose access token was refused: refresh once, retry.
	if status == http.StatusUnauthorized && !req.Anonymous && c.source == "login" {
		if err := c.refresh(ctx, true); err == nil {
			if status, body, err = c.send(ctx, req); err != nil {
				return nil, err
			}
		}
	}
	if status >= 200 && status < 300 {
		if len(bytes.TrimSpace(body)) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return json.RawMessage(body), nil
	}
	return nil, decodeError(status, body)
}

func decodeError(status int, body []byte) error {
	var e apiErrorBody
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		return cli.APIError(status, e.Error)
	}
	return cli.APIError(status, cli.Error{Code: "", Message: fmt.Sprintf("The Vitko API answered HTTP %d.", status), Hint: "Try again in a minute."})
}

func (c *Client) send(ctx context.Context, req Request) (int, []byte, error) {
	u := c.Origin() + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	var reader io.Reader
	if req.Body != nil {
		raw, err := json.Marshal(req.Body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, req.Method, u, reader)
	if err != nil {
		return 0, nil, cli.Errf("usage", "The API URL %q isn't valid: %v", c.BaseURL, err)
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "vitko/"+version.Version)
	if req.Body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if !req.Anonymous && c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for k, v := range req.Headers {
		r.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		e := cli.Errf("unavailable", "The Vitko API at %s couldn't be reached: %v", c.Origin(), shortErr(err))
		e.Hint = "Check your network. VITKO_RUNNERS_API_URL changes the API address."
		return 0, nil, e
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, cli.Errf("unavailable", "Reading the API's answer failed: %v", err)
	}
	return resp.StatusCode, body, nil
}

func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// Source says where the credential came from: env, github-actions, login.
func (c *Client) Source() string { return c.source }

// CredentialSource reports, without calling the API, which credential would
// be used.
func (c *Client) CredentialSource() (string, error) {
	if strings.TrimSpace(c.Getenv("VITKO_TOKEN")) != "" {
		return "env", nil
	}
	if c.inActions() {
		return "github-actions", nil
	}
	cred, ok, err := c.Store.Get(c.Origin())
	if err != nil {
		return "", err
	}
	if !ok {
		return "none", nil
	}
	return "login:" + cred.Kind, nil
}

func (c *Client) inActions() bool {
	return c.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "" && c.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") != ""
}

func signedOut() error {
	e := cli.Errf("signed_out", "You're not signed in to Vitko.")
	e.Hint = "Run `vitko login`, or set VITKO_TOKEN to an API token."
	e.Fix = &cli.Fix{Command: "vitko login"}
	return e
}

func (c *Client) authenticate(ctx context.Context) error {
	if c.bearer != "" {
		return nil
	}
	if t := strings.TrimSpace(c.Getenv("VITKO_TOKEN")); t != "" {
		c.bearer, c.source = t, "env"
		return nil
	}
	if c.inActions() {
		token, err := c.actionsExchange(ctx)
		if err != nil {
			return err
		}
		c.bearer, c.source = token, "github-actions"
		return nil
	}
	cred, ok, err := c.Store.Get(c.Origin())
	if err != nil {
		return cli.Errf("invalid_input", "%v", err)
	}
	if !ok {
		return signedOut()
	}
	c.source = "login"
	if cred.Kind == "token" {
		c.bearer = cred.APIToken
		return nil
	}
	if cred.AccessToken != "" && c.Now().Add(time.Minute).Before(cred.AccessExpiresAt) {
		c.bearer = cred.AccessToken
		return nil
	}
	return c.refresh(ctx, false)
}

// TokenDocument is vitko.auth-token/v1 as the API returns it.
type TokenDocument struct {
	AccessToken      string    `json:"access_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	SignedInAt       time.Time `json:"signed_in_at"`
	User             struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c *Client) refresh(ctx context.Context, force bool) error {
	cred, ok, err := c.Store.Get(c.Origin())
	if err != nil || !ok || cred.Kind != "user" || cred.RefreshToken == "" {
		return signedOut()
	}
	if !force && cred.AccessToken != "" && c.Now().Add(time.Minute).Before(cred.AccessExpiresAt) {
		c.bearer = cred.AccessToken
		return nil
	}
	status, body, err := c.send(ctx, Request{Method: http.MethodPost, Path: "/v1/auth/refresh", Body: map[string]string{"refresh_token": cred.RefreshToken}, Anonymous: true})
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		_ = c.Store.Put(c.Origin(), nil)
		e := cli.Errf("token_expired", "Your sign-in has ended.")
		e.Hint = "Sign in again."
		e.Fix = &cli.Fix{Command: "vitko login"}
		return e
	}
	if status != http.StatusOK {
		return decodeError(status, body)
	}
	var doc TokenDocument
	if err := json.Unmarshal(body, &doc); err != nil || doc.AccessToken == "" {
		return cli.Errf("unavailable", "The API's sign-in answer couldn't be read.")
	}
	cred.AccessToken, cred.AccessExpiresAt = doc.AccessToken, doc.ExpiresAt
	cred.RefreshToken, cred.RefreshExpiresAt = doc.RefreshToken, doc.RefreshExpiresAt
	if err := c.Store.Put(c.Origin(), &cred); err != nil {
		return cli.Errf("internal", "Couldn't save the refreshed sign-in: %v", err)
	}
	c.bearer = cred.AccessToken
	return nil
}

// actionsExchange asks GitHub Actions for an ID token for this API and
// exchanges it for a short-lived Vitko token (kept in memory only).
func (c *Client) actionsExchange(ctx context.Context) (string, error) {
	audience := c.Getenv("VITKO_OIDC_AUDIENCE")
	if audience == "" {
		audience = c.Origin()
	}
	u, err := url.Parse(c.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"))
	if err != nil {
		return "", cli.Errf("usage", "ACTIONS_ID_TOKEN_REQUEST_URL isn't a URL.")
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	r.Header.Set("Authorization", "Bearer "+c.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"))
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return "", cli.Errf("unavailable", "GitHub Actions didn't issue an ID token: %v", shortErr(err))
	}
	defer resp.Body.Close()
	var idToken struct {
		Value string `json:"value"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&idToken) != nil || idToken.Value == "" {
		e := cli.Errf("forbidden", "GitHub Actions didn't issue an ID token (HTTP %d).", resp.StatusCode)
		e.Hint = "Add `permissions: id-token: write` to the workflow or job."
		return "", e
	}
	status, body, err := c.send(ctx, Request{Method: http.MethodPost, Path: "/v1/auth/oidc/github", Body: map[string]string{"id_token": idToken.Value}, Anonymous: true})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", decodeError(status, body)
	}
	var doc TokenDocument
	if err := json.Unmarshal(body, &doc); err != nil || doc.AccessToken == "" {
		return "", cli.Errf("unavailable", "The API's sign-in answer couldn't be read.")
	}
	return doc.AccessToken, nil
}
