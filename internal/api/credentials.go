package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Credential is a stored sign-in for one API origin.
type Credential struct {
	// Kind is "user" (a device login) or "token" (an API token).
	Kind             string    `json:"kind"`
	AccessToken      string    `json:"access_token,omitempty"`
	AccessExpiresAt  time.Time `json:"access_expires_at,omitempty"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	APIToken         string    `json:"api_token,omitempty"`
	Login            string    `json:"login,omitempty"`
	SignedInAt       time.Time `json:"signed_in_at,omitempty"`
}

// CredentialFile stores credentials, keyed by API origin, in a file only
// the user can read (mode 0600).
type CredentialFile struct{ Path string }

// CredentialsPath is <config dir>/credentials.json.
func CredentialsPath(configDir string) string { return filepath.Join(configDir, "credentials.json") }

func (f CredentialFile) load() (map[string]Credential, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Credential{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Credential{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.New(f.Path + " is not valid JSON; delete it and sign in again")
	}
	return m, nil
}

// Get returns the credential for origin.
func (f CredentialFile) Get(origin string) (Credential, bool, error) {
	m, err := f.load()
	if err != nil {
		return Credential{}, false, err
	}
	c, ok := m[origin]
	return c, ok, nil
}

// Put stores (or, with nil, removes) the credential for origin.
func (f CredentialFile) Put(origin string, c *Credential) error {
	m, err := f.load()
	if err != nil {
		m = map[string]Credential{}
	}
	if c == nil {
		delete(m, origin)
	} else {
		m[origin] = *c
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}
