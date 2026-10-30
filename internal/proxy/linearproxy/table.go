// Package linearproxy is the Linear proxy, built the way notionproxy is:
// the isolation speaks MCP to Linear, and writes only by permission.
//
// There is no integration and no API key. gero logs in to Linear's hosted
// MCP server the way any MCP client does (dynamic client registration, a
// browser login, PKCE) and keeps the OAuth tokens in the credentials file:
//
//	isolation  an MCP client, Authorization: Bearer $LINEAR_TOKEN,
//	             http://127.0.0.1:3030/mcp   (the isolation's tunnel)
//	daemon     this proxy: a fixed tool table, its own rate budget, the
//	             |  real access token attached, refreshed here
//	           https://mcp.linear.app/mcp
//
// LINEAR_TOKEN is a per-environment workspace token: since MCP has no slot
// for an environment, the token value is also how the isolation picks one
// workspace or another. The real tokens live in [linear <env>] and are
// rewritten here when the access token runs out.
//
// The boundary is a tool table. MCP is JSON-RPC over POST, so a verb check
// means nothing; what an agent can do is the tool it names in tools/call.
// Tools are grouped into families an environment allows one by one, and a
// tool the table does not name does not exist from here, not in tools/list
// and not callable, so a tool Linear ships tomorrow stays shut until
// someone models it. initialize answers with the tools capability only, so
// resources and prompts are never offered.
//
// The other boundary is wider than an API key's: an MCP login acts as the
// Linear user and sees what that user sees. The family table is the whole
// boundary on verbs, the user's own access the whole boundary on scope.
package linearproxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Families are the tools each one means. Linear has renamed its tools
// (create_issue and update_issue became save_issue), so a family names the
// old and the new spellings; whichever Linear offers is the one that
// answers. Reads are open by default; each write is its own choice.
var Families = map[string][]string{
	"issues": {
		"list_issues", "get_issue", "list_my_issues", "get_issue_git_branch_name",
		"list_issue_statuses", "get_issue_status", "list_issue_labels", "list_comments",
		"get_attachment", "extract_images",
	},
	"projects": {
		"list_projects", "get_project", "list_project_labels", "list_milestones", "get_milestone",
		"list_initiatives", "get_initiative", "list_project_updates", "get_project_update",
	},
	"documents": {"list_documents", "get_document"},
	"teams":     {"list_teams", "get_team", "list_cycles"},
	"users":     {"list_users", "get_user"},
	// Creating an issue and changing one are one tool in today's Linear, so
	// they are one choice here.
	"issue-edits": {"save_issue", "create_issue", "update_issue", "create_attachment"},
	"comments":    {"save_comment", "create_comment"},
	"labels":      {"create_issue_label"},
	"project-edits": {
		"save_project", "create_project", "update_project", "save_milestone",
		"save_initiative", "save_project_update", "save_project_label",
	},
	"document-edits": {"save_document", "create_document", "update_document"},
	"deletes":        {"delete_comment", "delete_attachment"},
}

var (
	ReadFamilies  = []string{"issues", "projects", "documents", "teams", "users"}
	WriteFamilies = []string{"issue-edits", "comments", "labels", "project-edits", "document-edits", "deletes"}
	// DefaultFamilies is what an env allows before anyone chooses: every
	// read, no write.
	DefaultFamilies = ReadFamilies
)

// Probe tools answer for every env, outside every family, so an agent's
// first sanity check always has an answer. Linear's documentation search
// reads nothing of the workspace's.
const Probe = "probe"

var ProbeTools = []string{"search_documentation"}

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

// Env is one Linear environment: which families it opens. Families nil
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
		return "", deny(403, "linear: %q is not a modeled tool; tools this proxy does not know do not exist from here (modeled families: %s)",
			tool, strings.Join(familyNames(), ", "))
	}
	if family == Probe || allows(e, family) {
		return family, nil
	}
	kind := "read"
	if isWrite(family) {
		kind = "write"
	}
	return "", deny(403, "linear: %s is in the '%s' %s family, which environment %q does not allow; the operator can open it with `gero env edit linear %s --allow-api %s`",
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
		return Action{}, deny(400, "linear: not a JSON-RPC 2.0 message; batches are not accepted either")
	}
	m, present := msg["method"]
	if !present {
		// The client answering a request the server sent it.
		_, r := msg["result"]
		_, er := msg["error"]
		if r || er {
			return Action{Forward: true}, nil
		}
		return Action{}, deny(400, "linear: a message with neither a method nor a result")
	}
	method, ok := m.(string)
	if !ok {
		return Action{}, deny(400, "linear: method must be a string")
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
			return Action{}, deny(400, "linear: tools/call without a tool name")
		}
		if _, err := Allowed(e, envName, name); err != nil {
			return Action{}, err
		}
		return Action{Forward: true}, nil
	}
	return Action{}, deny(404, "linear: %s is not served here; this proxy serves tools only — initialize, tools/list, tools/call", method)
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
