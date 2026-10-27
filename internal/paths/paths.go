// Package paths says where gero keeps things on the host.
package paths

import (
	"os"
	"path/filepath"
)

type Paths struct {
	Config string // ~/.config/gero: user-edited settings, keybindings
	Data   string // ~/.local/share/gero: the database
	State  string // ~/.local/state/gero: socket, lock, logs
}

// Default follows the XDG layout. GERO_HOME puts everything under one
// directory instead, for development and tests.
func Default() Paths {
	if h := os.Getenv("GERO_HOME"); h != "" {
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
		Config: filepath.Join(env("XDG_CONFIG_HOME", ".config"), "gero"),
		Data:   filepath.Join(env("XDG_DATA_HOME", ".local/share"), "gero"),
		State:  filepath.Join(env("XDG_STATE_HOME", ".local/state"), "gero"),
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

func (p Paths) Socket() string { return filepath.Join(p.State, "gero.sock") }
func (p Paths) Lock() string   { return filepath.Join(p.State, "daemon.lock") }
func (p Paths) Log() string    { return filepath.Join(p.State, "daemon.log") }
func (p Paths) DB() string     { return filepath.Join(p.Data, "gero.db") }

// Credentials is the one file the credential proxies read their secrets
// from: an INI of [<service> <env>] sections, 0600.
func (p Paths) Credentials() string { return filepath.Join(p.Config, "credentials") }

// WebToken signs browsers in to the web UI. Anyone holding it controls the
// agents, so it lives with the socket, readable by the user only.
func (p Paths) WebToken() string { return filepath.Join(p.State, "web-token") }
