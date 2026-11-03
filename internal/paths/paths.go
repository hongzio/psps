// Package paths says where psps keeps things on the host.
package paths

import (
	"os"
	"path/filepath"

	"github.com/hongzio/psps/internal/creds"
)

type Paths struct {
	Config string // ~/.config/psps: user-edited settings, keybindings
	Data   string // ~/.local/share/psps: the database
	State  string // ~/.local/state/psps: socket, lock, logs
}

// Default follows the XDG layout. PSPS_HOME puts everything under one
// directory instead, for development and tests.
func Default() Paths {
	if h := os.Getenv("PSPS_HOME"); h != "" {
		return Paths{Config: filepath.Join(h, "config"), Data: filepath.Join(h, "data"), State: filepath.Join(h, "state")}
	}
	home, _ := os.UserHomeDir()
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return filepath.Join(home, def)
	}
	return Paths{
		Config: filepath.Join(env("XDG_CONFIG_HOME", ".config"), "psps"),
		Data:   filepath.Join(env("XDG_DATA_HOME", ".local/share"), "psps"),
		State:  filepath.Join(env("XDG_STATE_HOME", ".local/state"), "psps"),
	}
}

func (p Paths) Ensure() error {
	for _, d := range []string{p.Config, p.Data, p.State} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (p Paths) Socket() string { return filepath.Join(p.State, "psps.sock") }
func (p Paths) Lock() string   { return filepath.Join(p.State, "daemon.lock") }
func (p Paths) Log() string    { return filepath.Join(p.State, "daemon.log") }
func (p Paths) DB() string     { return filepath.Join(p.Data, "psps.db") }

// Credentials is the one file the credential proxies read their secrets
// from: an INI of [<service> <env>] sections, 0600.
func (p Paths) Credentials() string { return filepath.Join(p.Config, "credentials") }

// CredentialsKey seals the credentials file's values. It lives in the state
// directory, apart from the file, so a commit or copy of ~/.config carries
// ciphertext alone.
func (p Paths) CredentialsKey() string { return filepath.Join(p.State, "credentials.key") }

// CredStore is the credentials file with its key.
func (p Paths) CredStore() *creds.Store {
	return &creds.Store{Path: p.Credentials(), KeyPath: p.CredentialsKey()}
}

// Skills holds the operator's own skills, one folder each, which the skill
// service puts in isolations.
func (p Paths) Skills() string { return filepath.Join(p.Config, "skills") }

// Settings is the daemon's own settings file, read when it starts.
func (p Paths) Settings() string { return filepath.Join(p.Config, "config.toml") }

// WebToken signs browsers in to the web UI. Anyone holding it controls the
// agents, so it lives with the socket, readable by the user only.
func (p Paths) WebToken() string { return filepath.Join(p.State, "web-token") }
