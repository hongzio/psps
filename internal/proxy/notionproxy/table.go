// Package notionproxy is the Notion proxy, a port of isolate's
// notionproxy.py: the isolation speaks MCP to Notion, and writes only by
// permission.
//
// There is no integration. gero logs in to Notion's hosted MCP server the
// way any MCP client does (dynamic client registration, a browser login,
// PKCE) and keeps the OAuth tokens in the credentials file. Nobody needs
// the right to create a workspace integration, which is the point:
//
//	isolation  an MCP client, Authorization: Bearer $NOTION_TOKEN,
//	             http://127.0.0.1:3028/mcp   (the isolation's tunnel)
//	daemon     this proxy: a fixed tool table, its own rate budget, the
//	             |  real access token attached, refreshed here
//	           https://mcp.notion.com/mcp
//
// NOTION_TOKEN is a per-environment workspace token: since MCP has no slot
// for an environment, the token value is also how the isolation picks dev
// or prod. The real tokens live in [notion <env>] and are rewritten here
// when the access token runs out.
//
// The boundary is a tool table. MCP is JSON-RPC over POST, so a verb check
// means nothing; what an agent can do is the tool it names in tools/call.
// Tools are grouped into families an environment allows one by one, and a
// tool the table does not name does not exist from here, not in tools/list
// and not callable, so a tool Notion ships tomorrow stays shut until
// someone models it. initialize answers with the tools capability only, so
// resources and prompts are never offered.
//
// The other boundary is wider than an integration's: an MCP login acts as
// the Notion user and sees what that user sees. The family table is the
// whole boundary on verbs, the user's own access the whole boundary on
// scope.
package notionproxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Families are the tools each one means, grouped by what opening it costs.
// The reads past content and search reach things a page lookup never does,
// so each is its own choice; the writes are never open unless an env names
// them.
var Families = map[string][]string{
	"content": {
		"notion-fetch", "notion-query-data-sources", "notion-query-multiple-data-sources",
		"notion-list-private-pages", "notion-list-shared-pages", "notion-list-favorite-pages",
		"notion-list-recent-pages", "notion-list-page-backlinks", "notion-get-comments",
		"notion-get-file-download-urls", "notion-download-attachment", "notion-get-async-task",
	},
	// notion-ai-search is gone from tools/list but still callable by name;
	// it searches what notion-search does.
	"search":   {"notion-search", "notion-ai-search"},
	"users":    {"notion-get-users", "notion-get-teams", "notion-get-groups"},
	"meetings": {"notion-query-meeting-notes"},
	"calendar": {
		"notion-calendar-list-calendars", "notion-calendar-list-events", "notion-calendar-list-coworkers-events",
		"notion-calendar-list-calendar-resources", "notion-calendar-list-contacts",
	},
	// Slack, Google Drive, GitHub, read through Notion, around whatever
	// boundary gero draws for those services directly.
	"connectors": {"notion-connector-search"},
	"memory":     {"notion-memory-search"},
	"skills":     {"notion-download-skill", "notion-get-enabled-skills"},
	"agents": {
		"notion-search-agents", "notion-query-sessions", "notion-search-sessions", "notion-get-session-status",
		"notion-wait-session", "notion-list-session-events", "notion-read-session-event",
	},
	"comments": {
		"notion-create-comment", "notion-update-comment", "notion-delete-comment",
		"notion-set-discussion-resolved", "notion-set-comment-reaction",
	},
	"pages": {
		"notion-create-pages", "notion-update-page", "notion-move-pages", "notion-duplicate-page",
		"notion-create-folder", "notion-update-folder", "notion-create-file-upload", "notion-create-attachment",
	},
	"databases": {"notion-create-database", "notion-update-data-source", "notion-create-view", "notion-update-view"},
	"trash":     {"notion-delete-pages", "notion-delete-databases", "notion-restore-pages"},
	"events":    {"notion-calendar-create-events", "notion-calendar-update-events", "notion-calendar-cancel-events"},
	// Starting or steering a Notion custom agent hands work to something
	// that acts with the user's whole access, outside this table.
	"sessions":    {"notion-spawn-session", "notion-stop-session", "notion-send-message-to-session"},
	"skill-edits": {"notion-upload-skill", "notion-update-enabled-skills", "notion-convert-page-to-skill"},
}

var (
	ReadFamilies  = []string{"content", "search", "users", "meetings", "calendar", "connectors", "memory", "skills", "agents"}
	WriteFamilies = []string{"comments", "pages", "databases", "trash", "events", "sessions", "skill-edits"}
	// DefaultFamilies is what an env allows before anyone chooses: page reads
	// and search.
	DefaultFamilies = []string{"content", "search"}
)

// Probe tools answer for every env, outside every family, so `gero env
// check` and an agent's first sanity check always have an answer.
const Probe = "probe"

var ProbeTools = []string{"notion-get-self", "notion-get-tool-access"}

var toolFamily = func() map[string]string {
	m := map[string]string{}
	for f, tools := range Families {
		for _, t := range tools {
			m[t] = f
		}
	}
	for _, t := range ProbeTools {
		m[t] = Probe
	}
	return m
}()

// ToolFamily is a tool's family, Probe for the probes, "" for a tool the
// table does not model.
func ToolFamily(name string) string { return toolFamily[name] }

func isWrite(f string) bool {
	for _, w := range WriteFamilies {
		if w == f {
			return true
		}
	}
	return false
}

func familyNames() []string {
	out := make([]string, 0, len(Families))
	for f := range Families {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Env is one Notion environment: which families it opens. Families nil
// means the default reads (an env nobody shaped chose exactly that); an
// explicit empty list means the probes and nothing else.
type Env struct {
	Families *[]string `json:"families,omitempty"`
}

// FamiliesFor is what an env allows. A stored name the table no longer has
// opens nothing.
func FamiliesFor(e Env) []string {
	if e.Families == nil {
		return append([]string(nil), DefaultFamilies...)
	}
	out := []string{}
	for _, f := range *e.Families {
		if _, ok := Families[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

func allows(e Env, family string) bool {
	for _, f := range FamiliesFor(e) {
		if f == family {
			return true
		}
	}
	return false
}

// Denied is a refusal with the HTTP status it goes out under.
type Denied struct {
	Reason string
	Status int
}

func (d *Denied) Error() string { return d.Reason }

func deny(status int, format string, a ...any) *Denied {
	return &Denied{Reason: fmt.Sprintf(format, a...), Status: status}
}

// Allowed returns the tool's family or a refusal naming the fix.
func Allowed(e Env, envName, tool string) (string, error) {
	family := ToolFamily(tool)
	if family == "" {
		return "", deny(403, "notion: %q is not a modeled tool; tools this proxy does not know do not exist from here (modeled families: %s)",
			tool, strings.Join(familyNames(), ", "))
	}
	if family == Probe || allows(e, family) {
		return family, nil
	}
	kind := "read"
	if isWrite(family) {
		kind = "write"
	}
	return "", deny(403, "notion: %s is in the '%s' %s family, which environment %q does not allow; the operator can open it with `gero env edit notion %s --allow-api %s`",
		tool, family, kind, envName, envName, family)
}

// Visible is whether tools/list shows a tool to this env.
func Visible(e Env, tool string) bool {
	f := ToolFamily(tool)
	return f == Probe || (f != "" && allows(e, f))
}

// Client notifications that carry nothing but protocol state.
var notifications = map[string]bool{
	"notifications/initialized": true, "notifications/cancelled": true,
	"notifications/progress": true, "notifications/roots/list_changed": true,
}

// Action is what to do with one client message.
type Action struct {
	// Forward sends it upstream; otherwise it is accepted and dropped here.
	Forward bool
	// Rewrite names the answer to narrow on the way back: "initialize",
	// "tools/list" or "".
	Rewrite string
}

// Classify decides one client message. A tools/call refusal carries 403 and
// goes back as a tool result, so the agent reads the sentence as the tool's
// answer; everything else is a JSON-RPC error.
func Classify(msg map[string]any, e Env, envName string) (Action, error) {
	if msg == nil || msg["jsonrpc"] != "2.0" {
		return Action{}, deny(400, "notion: not a JSON-RPC 2.0 message; batches are not accepted either")
	}
	m, present := msg["method"]
	if !present {
		// The client answering a request the server sent it.
		_, r := msg["result"]
		_, er := msg["error"]
		if r || er {
			return Action{Forward: true}, nil
		}
		return Action{}, deny(400, "notion: a message with neither a method nor a result")
	}
	method, ok := m.(string)
	if !ok {
		return Action{}, deny(400, "notion: method must be a string")
	}
	switch {
	case notifications[method]:
		return Action{Forward: true}, nil
	case strings.HasPrefix(method, "notifications/"):
		return Action{}, nil
	case method == "initialize", method == "tools/list":
		return Action{Forward: true, Rewrite: method}, nil
	case method == "ping":
		return Action{Forward: true}, nil
	case method == "tools/call":
		params, _ := msg["params"].(map[string]any)
		name, ok := params["name"].(string)
		if !ok {
			return Action{}, deny(400, "notion: tools/call without a tool name")
		}
		if _, err := Allowed(e, envName, name); err != nil {
			return Action{}, err
		}
		return Action{Forward: true}, nil
	}
	return Action{}, deny(404, "notion: %s is not served here; this proxy serves tools only — initialize, tools/list, tools/call", method)
}

// Narrow rewrites an upstream result in place for what this env may see.
func Narrow(kind string, result any, e Env) any {
	r, ok := result.(map[string]any)
	if !ok {
		return result
	}
	switch kind {
	case "initialize":
		caps, _ := r["capabilities"].(map[string]any)
		out := map[string]any{}
		if t, ok := caps["tools"]; ok {
			out["tools"] = t
		}
		r["capabilities"] = out
	case "tools/list":
		tools, _ := r["tools"].([]any)
		kept := []any{}
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				if name, _ := tm["name"].(string); Visible(e, name) {
					kept = append(kept, t)
				}
			}
		}
		r["tools"] = kept
	}
	return r
}

// decode parses one JSON-RPC message, numbers kept exact.
func decode(b []byte) (map[string]any, error) {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("trailing data")
	}
	m, _ := v.(map[string]any)
	if m == nil {
		// A batch or a scalar: Classify refuses it with the reason.
		return map[string]any{}, nil
	}
	return m, nil
}

// encode is the message as it leaves: re-serialized from what was judged,
// never the raw bytes, so a duplicate key read one way here and another way
// upstream cannot carry a tool past the table.
func encode(v any) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}

// sameID compares JSON-RPC ids by their JSON spelling.
func sameID(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	return string(encode(a)) == string(encode(b))
}
