package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
)

// Action is something a key can do. Each belongs to the context it is
// listed under in defaultBindings; a key may be reused across contexts.
type Action string

type binding struct {
	action  Action
	keys    []string
	context string
	help    string
}

// defaultBindings is the whole keyboard surface. Everything is reachable
// from the keyboard, and every entry can be rebound in keys.toml.
var defaultBindings = []binding{
	{"quit", []string{"ctrl+c"}, "global", "quit"},
	{"help", []string{"f1", "ctrl+_"}, "global", "show keys"},
	{"focus_next", []string{"tab"}, "global", "next pane / field"},
	{"focus_prev", []string{"shift+tab"}, "global", "previous pane / field"},
	{"new_agent", []string{"ctrl+n"}, "global", "new agent"},
	{"isolations", []string{"ctrl+g"}, "global", "isolations"},
	{"services", []string{"ctrl+r"}, "global", "services: the credential proxies' environments"},
	{"agent_settings", []string{"ctrl+o"}, "global", "agent settings (model, effort, approvals)"},
	{"approvals", []string{"ctrl+a"}, "global", "answer the pending approval"},
	{"interrupt", []string{"ctrl+x"}, "global", "interrupt the running turn"},

	{"up", []string{"up", "k"}, "list", "move up"},
	{"down", []string{"down", "j"}, "list", "move down"},
	{"open", []string{"enter"}, "list", "open"},
	{"archive", []string{"x"}, "sidebar", "archive the selected agent"},
	{"toggle_archived", []string{"z"}, "sidebar", "show / hide archived agents"},

	{"scroll_up", []string{"up", "k"}, "conversation", "scroll up"},
	{"scroll_down", []string{"down", "j"}, "conversation", "scroll down"},
	{"page_up", []string{"pgup", "ctrl+u"}, "conversation", "page up"},
	{"page_down", []string{"pgdown", "ctrl+d"}, "conversation", "page down"},
	{"top", []string{"g", "home"}, "conversation", "top"},
	{"bottom", []string{"G", "end"}, "conversation", "bottom"},

	{"send", []string{"enter"}, "input", "send"},
	{"newline", []string{"alt+enter", "ctrl+j"}, "input", "new line"},
	{"back", []string{"esc"}, "input", "leave the input / close"},

	{"submit", []string{"ctrl+s"}, "form", "submit"},
	{"prev_option", []string{"left"}, "form", "previous choice"},
	{"next_option", []string{"right"}, "form", "next choice"},

	{"accept", []string{"y"}, "approval", "accept"},
	{"decline", []string{"n"}, "approval", "decline (the agent carries on)"},
	{"cancel_turn", []string{"c"}, "approval", "decline and stop the turn"},

	{"iso_new", []string{"n"}, "isolations", "new isolation"},
	{"iso_env", []string{"e"}, "isolations", "set an environment variable"},
	{"iso_login", []string{"l"}, "isolations", "accounts: what Claude and Codex log in with there"},
	{"iso_settings", []string{"s"}, "isolations", "settings: rename, CPUs and memory"},
	{"iso_upgrade", []string{"u"}, "isolations", "upgrade to the current image"},
	{"iso_delete", []string{"d"}, "isolations", "delete"},
	{"iso_services", []string{"p"}, "isolations", "what the isolation's credential proxies serve"},
	{"iso_verify", []string{"v"}, "isolations", "measure the isolation's guarantees now (gero iso verify)"},

	{"cli_login", []string{"l"}, "clis", "log the CLI in inside the isolation (from any browser)"},
	{"cli_logout", []string{"o"}, "clis", "log the CLI out inside the isolation"},
	{"cli_account", []string{"a"}, "clis", "choose the account the CLI uses, or a login inside"},
	{"acct_new", []string{"n"}, "clis", "new account (a Claude token, an API key)"},
	{"acct_secret", []string{"s"}, "clis", "replace the selected account's secret"},
	{"acct_delete", []string{"d"}, "clis", "remove the selected account"},

	{"svc_new", []string{"n"}, "services", "new environment for the service under the cursor"},
	{"svc_edit", []string{"e"}, "services", "edit (an env; in an isolation, a service)"},
	{"svc_secret", []string{"s"}, "services", "store the secret, or log in through the browser"},
	{"svc_check", []string{"c"}, "services", "check the env from the Mac, now"},
	{"svc_delete", []string{"d"}, "services", "remove the env"},
	{"svc_off", []string{"x"}, "services", "turn a service off for the isolation"},
	{"copy", []string{"y"}, "services", "copy the login address to the clipboard (also on accounts)"},
}

type Keymap struct {
	keys map[Action][]string
}

func defaultKeymap() Keymap {
	k := Keymap{keys: map[Action][]string{}}
	for _, b := range defaultBindings {
		k.keys[b.action] = append([]string(nil), b.keys...)
	}
	return k
}

// Is reports whether msg is bound to a.
func (k Keymap) Is(msg tea.KeyMsg, a Action) bool {
	s := msg.String()
	for _, key := range k.keys[a] {
		if key == s {
			return true
		}
	}
	return false
}

// Label is the first key bound to a, for hints.
func (k Keymap) Label(a Action) string {
	if ks := k.keys[a]; len(ks) > 0 {
		return ks[0]
	}
	return "unbound"
}

// LoadKeymap reads overrides from path. A missing file means defaults; an
// unknown action or a malformed value is reported but does not stop the TUI.
func LoadKeymap(path string) (Keymap, []string) {
	k := defaultKeymap()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return k, nil
		}
		return k, []string{err.Error()}
	}
	var file struct {
		Keys map[string]any `toml:"keys"`
	}
	if _, err := toml.Decode(string(b), &file); err != nil {
		return k, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var warnings []string
	for name, v := range file.Keys {
		a := Action(name)
		if _, ok := k.keys[a]; !ok {
			warnings = append(warnings, fmt.Sprintf("%s: unknown action %q", path, name))
			continue
		}
		switch v := v.(type) {
		case string:
			k.keys[a] = []string{v}
		case []any:
			var ks []string
			for _, x := range v {
				if s, ok := x.(string); ok {
					ks = append(ks, s)
				}
			}
			k.keys[a] = ks
		default:
			warnings = append(warnings, fmt.Sprintf("%s: %s must be a key or a list of keys", path, name))
		}
	}
	sort.Strings(warnings)
	return k, warnings
}

// DefaultKeysTOML is a complete keys.toml with every default, for
// `gero tui --keys > ~/.config/gero/keys.toml`.
func DefaultKeysTOML() string {
	var b strings.Builder
	b.WriteString("# gero TUI key bindings. Each action takes a key or a list of keys.\n")
	b.WriteString("# Key names: \"ctrl+n\", \"alt+enter\", \"shift+tab\", \"pgup\", \"f1\", \"a\".\n\n[keys]\n")
	ctx := ""
	for _, bd := range defaultBindings {
		if bd.context != ctx {
			ctx = bd.context
			fmt.Fprintf(&b, "\n# %s\n", ctx)
		}
		quoted := make([]string, len(bd.keys))
		for i, k := range bd.keys {
			quoted[i] = fmt.Sprintf("%q", k)
		}
		fmt.Fprintf(&b, "%-16s = [%s]  # %s\n", bd.action, strings.Join(quoted, ", "), bd.help)
	}
	return b.String()
}
