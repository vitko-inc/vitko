// Package config reads and writes the user's settings file,
// $VITKO_CONFIG_DIR/config.json (default ~/.config/vitko/config.json).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Key describes one setting.
type Key struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Enum    []string `json:"enum,omitempty"`
}

// Keys are the settings vitko understands.
var Keys = []Key{
	{Name: "output", Summary: "Default output format when --output and VITKO_OUTPUT are not set.", Enum: []string{"text", "json", "ndjson"}},
	{Name: "org", Summary: "Default GitHub organization (login or numeric id) when --org and VITKO_ORG are not set."},
}

// FindKey returns the key named n.
func FindKey(n string) *Key {
	for i := range Keys {
		if Keys[i].Name == n {
			return &Keys[i]
		}
	}
	return nil
}

// Dir is the settings directory.
func Dir(getenv func(string) string) string {
	if d := getenv("VITKO_CONFIG_DIR"); d != "" {
		return d
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "vitko")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".vitko")
	}
	return filepath.Join(home, ".config", "vitko")
}

// Path is the settings file.
func Path(getenv func(string) string) string { return filepath.Join(Dir(getenv), "config.json") }

// Load reads the settings. A missing file is empty settings.
func Load(getenv func(string) string) (map[string]string, error) {
	b, err := os.ReadFile(Path(getenv))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", Path(getenv), err)
	}
	return m, nil
}

// Save writes the settings with mode 0600.
func Save(getenv func(string) string, m map[string]string) error {
	if err := os.MkdirAll(Dir(getenv), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(getenv) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(getenv))
}
