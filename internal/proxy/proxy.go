// Package proxy is what every credential proxy has in common: how a service
// describes its environments and per-isolation switches, what it puts in the
// isolation, and how the daemon hands it the connections an isolation makes.
//
// A proxy holds a credential the isolation never sees and injects it on the
// way out, within an allowlist. Each one is a port of the isolate proxy of
// the same name, decision tests and wire tests included; the differences are
// structural only. The proxies run inside the daemon, so there are no host
// ports: a connection arrives through one isolation's tunnel, and that is
// who is asking. Configuration is read per connection or per request, so an
// edit takes effect without restarting anything, while what must outlive a
// request (caches, rate budgets) lives in the long-lived Instance.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

// Denied is a refusal whose Reason names the decision and the fix. Proxies
// return it to the isolation as a protocol-appropriate error: an agent that
// cannot see why it was refused will try to route around the refusal
// instead of asking the operator.
type Denied struct{ Reason string }

func (d *Denied) Error() string { return d.Reason }

func Deny(format string, a ...any) *Denied { return &Denied{Reason: fmt.Sprintf(format, a...)} }

// View is one isolation's current configuration of one service.
type View struct {
	// Isolation is the isolation's name as it is now, for what is said to
	// people.
	Isolation string
	// Enabled is whether the service is exposed to the isolation at all.
	Enabled  bool
	Exposure store.Exposure
	// Envs holds the exposed envs' definitions, by name, in no order; the
	// exposure's Envs gives the order.
	Envs map[string]json.RawMessage
	// Missing names exposed envs the registry no longer has.
	Missing []string
	// Tokens are the isolation's workspace tokens for this service, by env
	// ("" for a service with one token per isolation).
	Tokens map[string]string
}

// Decode unmarshals the env's definition into v.
func (v View) Decode(env string, into any) error {
	raw, ok := v.Envs[env]
	if !ok {
		return Deny("environment %q is not exposed to this isolation", env)
	}
	return json.Unmarshal(raw, into)
}

// Setting decodes the exposure's settings into into (a struct with
// defaults already set).
func (v View) Settings(into any) {
	if len(v.Exposure.Settings) > 0 {
		json.Unmarshal(v.Exposure.Settings, into)
	}
}

// Host is what an Instance gets from the daemon.
type Host struct {
	// Name is the isolation's name as it is now: an isolation can be
	// renamed while its instances serve it.
	Name func() string
	// View reads the current configuration. Call it per connection or per
	// request, never once at start.
	View  func() (View, error)
	Creds *creds.Store
	Log   *log.Logger
	// Mail is the daemon's own answer to the mail service, which is not a
	// proxy: it holds the agents' inboxes. The other services ignore it.
	Mail http.Handler
}

func (h Host) Logf(service, format string, a ...any) {
	h.Log.Printf("[%s:%s] %s", service, h.Name(), fmt.Sprintf(format, a...))
}

// Instance serves one service for one isolation, for the daemon's life.
// Serve is called for each of the service's ports every time the isolation
// opens a tunnel, and returns when the listener closes.
type Instance interface {
	Serve(port int, l net.Listener) error
}

type FieldKind string

const (
	String FieldKind = "string"
	Int    FieldKind = "int"
	Bool   FieldKind = "bool"
	List   FieldKind = "list"
)

// Field is one setting of an env definition or an exposure, as the CLI,
// the TUI and the web show it.
type Field struct {
	Key  string    `json:"key"`
	Kind FieldKind `json:"kind"`
	// Flag is the CLI flag; for a Bool, Neg is the flag that sets false.
	Flag string `json:"flag"`
	Neg  string `json:"neg,omitempty"`
	// Clear is the flag that empties a List.
	Clear   string   `json:"clear,omitempty"`
	Metavar string   `json:"metavar,omitempty"`
	Help    string   `json:"help"`
	Choices []string `json:"choices,omitempty"`
	// Lookup is a List whose choices the service finds itself (a
	// ChoiceLister) once the env's credential is known: the repositories a
	// token opens. Forms ask for the credential first.
	Lookup bool `json:"lookup,omitempty"`
	// Isolations is a List of isolations: typed and shown by name, kept by
	// ID, so a rename keeps what it grants and a name used again later
	// grants nothing.
	Isolations bool `json:"isolations,omitempty"`
}

// Patch changes an env definition or an exposure's settings the way the CLI
// spells it: a non-empty list replaces the stored one, Clear empties a list,
// anything unnamed survives.
type Patch struct {
	Set   map[string]any `json:"set,omitempty"`
	Clear []string       `json:"clear,omitempty"`
}

// Apply returns cfg with p applied, checked against fields.
func Apply(fields []Field, cfg json.RawMessage, p Patch) (map[string]any, error) {
	out := map[string]any{}
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &out); err != nil {
			return nil, err
		}
	}
	byKey := map[string]Field{}
	for _, f := range fields {
		byKey[f.Key] = f
	}
	for _, k := range p.Clear {
		f, ok := byKey[k]
		if !ok || f.Kind != List {
			return nil, fmt.Errorf("%q is not a list that can be cleared", k)
		}
		out[k] = []string{}
	}
	keys := make([]string, 0, len(p.Set))
	for k := range p.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, ok := byKey[k]
		if !ok {
			return nil, fmt.Errorf("%q is not a setting here", k)
		}
		v, err := coerce(f, p.Set[k])
		if err != nil {
			return nil, err
		}
		if f.Kind == List && len(v.([]string)) == 0 {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func coerce(f Field, v any) (any, error) {
	bad := func() error { return fmt.Errorf("--%s: expected a %s, got %v", f.Flag, f.Kind, v) }
	switch f.Kind {
	case String:
		s, ok := v.(string)
		if !ok {
			return nil, bad()
		}
		s = strings.TrimSpace(s)
		if len(f.Choices) > 0 && !contains(f.Choices, s) {
			return nil, fmt.Errorf("--%s: %q is not one of %s", f.Flag, s, strings.Join(f.Choices, ", "))
		}
		return s, nil
	case Int:
		switch n := v.(type) {
		case float64:
			return int(n), nil
		case int:
			return n, nil
		}
		return nil, bad()
	case Bool:
		b, ok := v.(bool)
		if !ok {
			return nil, bad()
		}
		return b, nil
	case List:
		var items []string
		switch l := v.(type) {
		case []string:
			items = l
		case []any:
			for _, x := range l {
				s, ok := x.(string)
				if !ok {
					return nil, bad()
				}
				items = append(items, s)
			}
		default:
			return nil, bad()
		}
		var out []string
		for _, s := range items {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if len(f.Choices) > 0 && !contains(f.Choices, s) {
				return nil, fmt.Errorf("--%s: %q is not one of %s", f.Flag, s, strings.Join(f.Choices, ", "))
			}
			out = append(out, s)
		}
		if out == nil {
			out = []string{}
		}
		return out, nil
	}
	return nil, bad()
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Workspace is what a service puts in an isolation while it is exposed.
type Workspace struct {
	// Env reaches every agent process and every shell in the isolation.
	Env []string
	// Files are written by `gero-shim sync`.
	Files []shimproto.SyncFile
	// MCP servers are handed to every agent gero starts, at launch: Claude
	// Code through --mcp-config, Codex through -c overrides. Neither tool's
	// own configuration is touched.
	MCP []MCPServer
}

// MCPServer is a streamable-HTTP MCP server inside the isolation. The bearer
// token is named by variable, which Workspace.Env sets, so it never rides on
// a command line.
type MCPServer struct {
	Name     string
	URL      string
	TokenEnv string
}

// Service is one credential proxy.
type Service interface {
	Name() string
	// Ports are the proxy's fixed addresses inside the isolation (git has
	// two: the pack protocol and the REST API).
	Ports() []int
	// Summary says in one line what the proxy is for.
	Summary() string
	// Fields describe an env definition; Settings an exposure's switches.
	Fields() []Field
	Settings() []Field
	// Normalize validates an env definition after a patch, fills in its
	// defaults and canonicalises it; warnings are true but legal things the
	// operator should hear about (an allowlist that routes nothing).
	Normalize(name string, env map[string]any) (warnings []string, err error)
	// NormalizeSettings does the same for an exposure's settings.
	NormalizeSettings(settings map[string]any) error
	// TokenKeys are the workspace tokens an exposure needs: one per env,
	// [""] for one per isolation, or none.
	TokenKeys(x store.Exposure) []string
	// Workspace is what the isolation gets while the service is exposed,
	// and Managed every path it may have written, so turning it off
	// removes them.
	Workspace(v View) (Workspace, error)
	Managed() []string
	// New starts the service's Instance for one isolation.
	New(h Host) Instance
	// Check exercises an env from the host, now, and says what answered.
	Check(ctx context.Context, env string, cfg json.RawMessage, c *creds.Store) (string, error)
	// Refusals are requests the proxy must refuse while exposed as v.
	Refusals(v View) []Refusal
}

// Refusal is a request a proxy must refuse, which `gero iso verify` makes
// from inside the isolation, as the agent, through the tunnel: what the
// boundary does is measured, not read off the code. Want is text only this
// proxy's refusal carries, so a refusal relayed from upstream (GitHub's own
// 404 for a repository that does not exist) does not count.
type Refusal struct {
	// What is tried, for the report: "a wrong workspace token".
	What string
	Port int
	// An HTTP request, unless Postgres is set: then a startup packet as
	// that user, answered with a wrong password.
	Method   string
	Path     string
	Header   map[string]string
	Body     string
	Postgres string
	// Status is the HTTP status the refusal answers with.
	Status int
	Want   string
}

// WrongToken is the token every Refusal presents in place of the workspace
// token.
const WrongToken = "gero-verify-not-the-token"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ValidEnvName is isolate's rule: letters, digits, - and _, up to 32.
func ValidEnvName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%q is not a usable environment name (letters, digits, - and _, up to 32)", name)
	}
	return nil
}

// EnvSuffix turns an env name into what follows DD_API_KEY_: prod -> PROD,
// us-east -> US_EAST.
func EnvSuffix(name string) string {
	return strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(name, "_"))
}

// Registry is every service gero has, by name.
type Registry struct {
	order []Service
	by    map[string]Service
}

func NewRegistry(svcs ...Service) *Registry {
	r := &Registry{by: map[string]Service{}}
	for _, s := range svcs {
		r.order = append(r.order, s)
		r.by[s.Name()] = s
	}
	return r
}

func (r *Registry) All() []Service { return r.order }

func (r *Registry) Get(name string) (Service, error) {
	if s, ok := r.by[name]; ok {
		return s, nil
	}
	names := make([]string, 0, len(r.order))
	for _, s := range r.order {
		names = append(names, s.Name())
	}
	return nil, fmt.Errorf("unknown service %q (have: %s)", name, strings.Join(names, ", "))
}

// SkillFiles are where an agent finds a skill: Claude Code's and Codex's
// user skill directories, both in the agent's home.
func SkillFiles(name, body string) []shimproto.SyncFile {
	var out []shimproto.SyncFile
	for _, dir := range SkillDirs(name) {
		out = append(out, shimproto.SyncFile{Path: dir + "/SKILL.md", Mode: 0o644, Agent: true, Content: body})
	}
	return out
}

func SkillDirs(name string) []string {
	return []string{
		"/home/agent/.claude/skills/gero-" + name,
		"/home/agent/.codex/skills/gero-" + name,
	}
}

// Strings reads a list setting from a patched definition, whatever JSON
// decoding made of it.
func Strings(m map[string]any, key string) []string {
	switch l := m[key].(type) {
	case []string:
		return l
	case []any:
		var out []string
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func BoolOr(m map[string]any, key string, def bool) bool {
	if b, ok := m[key].(bool); ok {
		return b
	}
	return def
}

func StringOr(m map[string]any, key, def string) string {
	if s, ok := m[key].(string); ok && s != "" {
		return s
	}
	return def
}

func IntOr(m map[string]any, key string, def int) int {
	switch n := m[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

// SortedSet is the list deduplicated and sorted, never nil.
func SortedSet(l []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range l {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Login is a browser login in progress. URL is what the operator opens;
// the browser lands on Redirect afterwards, and whatever address it lands
// on is what FinishLogin is handed. State never leaves the daemon.
type Login struct {
	URL      string
	Redirect string
	Expires  time.Time
	State    any
}

// Choice is one value a Lookup field can take, as the service found it.
type Choice struct {
	Value string `json:"value"`
	// Note is what to show beside it ("private", "archived").
	Note string `json:"note,omitempty"`
}

// ChoiceLister finds a Lookup field's choices from the env as a form has it
// so far and its secret, typed in that form or stored. With no credential
// to look with, it refuses with a Denied that says to give one first.
type ChoiceLister interface {
	Choices(ctx context.Context, field string, env map[string]any, secret map[string]string) ([]Choice, error)
}

// SecretOptional is a service whose env can take its credential from
// somewhere other than its secret (git's token command); an env that does
// is not missing its secret.
type SecretOptional interface {
	CredentialElsewhere(env json.RawMessage) bool
}

// LoginService is a service whose credential is a browser login rather
// than a secret typed in (Notion's OAuth). The daemon runs the exchange, so
// the CLI, the TUI and the web all log in the same way, including from a
// browser on another machine: the operator pastes back the address the
// browser landed on.
type LoginService interface {
	StartLogin(ctx context.Context, env, redirect string) (Login, error)
	FinishLogin(ctx context.Context, c *creds.Store, env string, l Login, pasted string) error
}
