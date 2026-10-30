// Package slackproxy is the read-only Slack proxy, a port of isolate's
// slackproxy.py: the isolation reads Slack Lists, channel messages and their
// files without holding a Slack token.
//
//	isolation  curl -H "Authorization: Bearer $SLACK_TOKEN" "$SLACK_API_URL/slackLists.items.list"
//	             |  (the isolation's tunnel)
//	daemon     this proxy: a fixed method table, its own rate budget, the
//	             |  real token attached
//	           https://slack.com/api/...
//
// SLACK_TOKEN is a per-environment workspace token: the proxy needs the
// caller to prove it is the isolation, SDKs want a token-shaped value
// either way, and, since Slack's wire has no slot for an environment, the
// token's value is also how the isolation picks dev or prod. The real token
// lives in the credentials file ([slack <env>]), read per request.
//
// "Read-only" can only mean a method table here. Slack's Web API does not
// split reads from writes by HTTP verb (everything answers to POST), so the
// forwardable surface is an explicit list of method names, grouped into
// families an environment opens one by one. Slack offers no channel-scoped
// token, so channel reads get a per-env deny list instead: messages refuses
// a denied channel wherever the request names it, search gets the denial
// pushed into the query as exclusion qualifiers, and files are clamped on
// where files.info says they are shared.
//
// The rate budget refuses rather than queues: Slack's limits are
// per-method per-workspace and shared with every other integration.
package slackproxy

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Download is not a Web API method: the isolation asks for GET /files/<F...>
// and the proxy turns the id into bytes itself.
const Download = "files.download"

var FileID = regexp.MustCompile(`^F[A-Z0-9]+$`)

// ChannelID is what --deny-channel takes: ids, not names, because the
// messages clamp compares ids on the wire and a name would deny nothing.
var ChannelID = regexp.MustCompile(`^[CGD][A-Z0-9]{4,}$`)

// Families is the forwardable surface, grouped by what opening it costs.
var Families = map[string][]string{
	"files":    {"files.info", Download},
	"lists":    {"slackLists.items.info", "slackLists.items.list"},
	"messages": {"conversations.history", "conversations.replies", "conversations.info"},
	"search":   {"search.messages"},
	// Who the workspace's members are: what turns a message's U... id into
	// a name, and an email into a U... id. It reads no channel.
	"users": {"users.list", "users.info", "users.lookupByEmail", "users.profile.get"},
}

// RequiredScopes is what each family needs from the stored token, checked
// by `gero env check`. search.messages additionally answers only to a user
// token (xoxp-...), which the same check surfaces as a missing scope;
// users.lookupByEmail and the email in a profile also need users:read.email.
var RequiredScopes = map[string]string{
	"files": "files:read", "lists": "lists:read", "messages": "channels:history", "search": "search:read",
	"users": "users:read",
}

var DefaultFamilies = []string{"lists"}

// ChannelFamilies are the families the deny list clamps: everything that
// reads what a channel holds, as opposed to a List.
var ChannelFamilies = []string{"files", "messages", "search"}

func familyNames() []string {
	out := make([]string, 0, len(Families))
	for f := range Families {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Methods is every modeled method name, auth.test (the wiring probe, outside
// every family) first; APIMethods is what answers under /api/.
var Methods, APIMethods = func() ([]string, []string) {
	m := []string{"auth.test"}
	for _, f := range familyNames() {
		m = append(m, Families[f]...)
	}
	var api []string
	for _, x := range m {
		if x != Download {
			api = append(api, x)
		}
	}
	return m, api
}()

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ReadsChannels is whether any of these families is one the deny list
// clamps.
func ReadsChannels(families []string) bool {
	for _, f := range families {
		if contains(ChannelFamilies, f) {
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

// Env is one Slack environment. Families nil means `lists`, the surface
// envs written before families existed chose; an explicit empty list means
// auth.test and nothing else.
type Env struct {
	Families     *[]string `json:"families,omitempty"`
	DenyChannels []string  `json:"deny_channels"`
}

func FamiliesFor(e Env) []string {
	if e.Families == nil {
		return append([]string(nil), DefaultFamilies...)
	}
	return append([]string(nil), (*e.Families)...)
}

// ResolveEnv is which exposed env the presented token names. Every
// candidate is compared, so a mismatch costs what a match does, and an env
// with no token minted never matches.
func ResolveEnv(tokens map[string]string, envs []string, presented string) string {
	found := ""
	for _, name := range envs {
		want := tokens[name]
		if presented != "" && want != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1 {
			found = name
		}
	}
	return found
}

// Route is the modeled method a request names. Percent-encoding and dot
// segments are refused outright rather than normalised: the table is only
// as good as its agreement with Slack about which method a path names.
func Route(verb, rawPath string) (string, *Denied) {
	if strings.ContainsAny(rawPath, "%\\\x00") {
		return "", deny(400, "slack: encoded characters are not accepted in an API path")
	}
	for _, seg := range strings.Split(rawPath, "/") {
		if seg == "." || seg == ".." {
			return "", deny(400, "slack: dot segments are not accepted in an API path")
		}
	}
	if verb != "GET" && verb != "POST" {
		return "", deny(403, "slack: the Web API answers GET and POST only; %s is refused", verb)
	}
	path := strings.TrimRight(rawPath, "/")
	if strings.HasPrefix(path, "/files/") {
		if verb != "GET" {
			return "", deny(403, "slack: a file download is GET only")
		}
		if _, d := FileIDOf(path); d != nil {
			return "", d
		}
		return Download, nil
	}
	if !strings.HasPrefix(path, "/api/") {
		if path == "" {
			path = "/"
		}
		return "", deny(404, "slack: %s is not a Web API path; this proxy forwards /api/<method> only", path)
	}
	method := strings.TrimPrefix(path, "/api/")
	if contains(APIMethods, method) {
		return method, nil
	}
	return "", deny(404, "slack: %s is not a modeled read method; this proxy forwards exactly: %s", method, strings.Join(APIMethods, ", "))
}

// FileIDOf is the F... id a /files/<id> path names.
func FileIDOf(path string) (string, *Denied) {
	id := strings.TrimRight(strings.TrimPrefix(path, "/files/"), "/")
	if !FileID.MatchString(id) {
		return "", deny(404, "slack: %s is not a file path; downloads are /files/<file id>, the F... id from a message's `files`", path)
	}
	return id, nil
}

// Allowed refuses a method outside the env's families. The suggested
// command names every family the env keeps, because --allow-api replaces
// the list rather than adding to it.
func Allowed(e Env, envName, method string) *Denied {
	if method == "auth.test" {
		return nil
	}
	have := FamiliesFor(e)
	for _, f := range familyNames() {
		if !contains(Families[f], method) {
			continue
		}
		if contains(have, f) {
			return nil
		}
		want := append(have, f)
		sort.Strings(want)
		flags := make([]string, len(want))
		for i, w := range want {
			flags[i] = "--allow-api " + w
		}
		return deny(404, "slack: %s is the %q method family, which environment %q does not allow; the operator can open it with `gero env edit slack %s %s`",
			method, f, envName, envName, strings.Join(flags, " "))
	}
	return deny(404, "slack: %s is not a modeled read method", method)
}

// pair is one parameter as sent, with its decoded name and value.
type pair struct{ raw, key, value string }

func parseForm(q string) ([]pair, error) {
	var out []pair
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err := url.QueryUnescape(k)
		if err != nil {
			return nil, err
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			return nil, err
		}
		out = append(out, pair{part, key, value})
	}
	return out, nil
}

func mediaType(ctype string) string {
	t, _, _ := strings.Cut(ctype, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// ChannelsOf is every channel the request names, wherever Slack would read
// it. Slack accepts parameters in the query string and in the body (form or
// JSON), and its precedence is its own, so a clamp that checked one slot
// would let ?channel=allowed ride shotgun for a body naming another. A body
// in any other shape cannot be checked, so it is refused.
func ChannelsOf(query string, body []byte, ctype string) ([]string, *Denied) {
	var out []string
	collect := func(encoded string, where string) *Denied {
		pairs, err := parseForm(encoded)
		if err != nil {
			return deny(400, "slack: the %s is not valid URL encoding, so the channel it names cannot be checked", where)
		}
		for _, p := range pairs {
			if v := strings.TrimSpace(p.value); p.key == "channel" && v != "" {
				out = append(out, v)
			}
		}
		return nil
	}
	if d := collect(query, "query string"); d != nil {
		return nil, d
	}
	if len(body) == 0 {
		return out, nil
	}
	switch mediaType(ctype) {
	case "", "application/x-www-form-urlencoded":
		if d := collect(string(body), "form body"); d != nil {
			return nil, d
		}
	case "application/json":
		var data any
		if json.Unmarshal(body, &data) != nil {
			return nil, deny(400, "slack: the JSON body does not parse, so the channel it names cannot be checked")
		}
		if m, ok := data.(map[string]any); ok {
			switch c := m["channel"].(type) {
			case nil:
			case string:
				if v := strings.TrimSpace(c); v != "" {
					out = append(out, v)
				}
			default:
				return nil, deny(400, "slack: the JSON body names its channel as a %T, so it cannot be checked; send the channel id as a string", c)
			}
		}
	default:
		return nil, deny(400, "slack: send channel reads as a form, JSON or URL parameters; the deny list cannot be checked against a %s body", mediaType(ctype))
	}
	return out, nil
}

// GuardChannel is the deny-list clamp on the messages family. Default-open
// on purpose: the operator chose a token that sees these channels, so the
// deny list carves exceptions rather than granting. A request naming no
// channel is forwarded, because every method in the family requires one and
// Slack refuses the parameterless call itself.
func GuardChannel(e Env, envName, method, query string, body []byte, ctype string) *Denied {
	if !contains(Families["messages"], method) || len(e.DenyChannels) == 0 {
		return nil
	}
	channels, d := ChannelsOf(query, body, ctype)
	if d != nil {
		return d
	}
	for _, c := range channels {
		if contains(e.DenyChannels, c) {
			return deny(403, "slack: channel %s is denied in environment %q; that is the operator's decision (`gero env edit slack %s --deny-channel ...` is where it is drawn)", c, envName, envName)
		}
	}
	return nil
}

// FileChannels is every conversation files.info says the file is shared in.
func FileChannels(file map[string]any) []string {
	places := map[string]bool{}
	for _, k := range []string{"channels", "groups", "ims"} {
		if l, ok := file[k].([]any); ok {
			for _, x := range l {
				if s, ok := x.(string); ok {
					places[s] = true
				}
			}
		}
	}
	if shares, ok := file["shares"].(map[string]any); ok {
		for _, kind := range []string{"public", "private"} {
			if m, ok := shares[kind].(map[string]any); ok {
				for c := range m {
					places[c] = true
				}
			}
		}
	}
	out := make([]string, 0, len(places))
	for c := range places {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// GuardFile is the deny-list clamp on the files family. A file shared in an
// open channel and a denied one is refused: the denied share is enough for
// the bytes to be the denied channel's.
func GuardFile(e Env, envName string, file map[string]any) *Denied {
	for _, c := range FileChannels(file) {
		if contains(e.DenyChannels, c) {
			id, _ := file["id"].(string)
			if id == "" {
				id = "?"
			}
			return deny(403, "slack: file %s is shared in channel %s, which is denied in environment %q; that is the operator's decision (`gero env edit slack %s --deny-channel ...` is where it is drawn)", id, c, envName, envName)
		}
	}
	return nil
}

// InfoAnswer is the `file` object of a files.info answer.
func InfoAnswer(raw []byte) (map[string]any, *Denied) {
	var answer map[string]any
	if json.Unmarshal(raw, &answer) != nil {
		return nil, deny(502, "slack: files.info did not answer JSON, so where the file is shared cannot be checked")
	}
	if ok, _ := answer["ok"].(bool); !ok {
		e, _ := answer["error"].(string)
		if e == "" {
			e = "not ok"
		}
		return nil, deny(404, "slack: files.info answered %q", e)
	}
	file, ok := answer["file"].(map[string]any)
	if !ok {
		return nil, deny(502, "slack: files.info answered without a file")
	}
	return file, nil
}

// DownloadTarget is the path on the files host to fetch. Matched on the
// parsed URL as a whole (scheme, no userinfo, host and port exactly), so no
// lookalike carries the token anywhere else.
func DownloadTarget(file map[string]any, filesBase *url.URL) (string, *Denied) {
	raw, _ := file["url_private_download"].(string)
	if raw == "" {
		raw, _ = file["url_private"].(string)
	}
	id, _ := file["id"].(string)
	if id == "" {
		id = "?"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != filesBase.Scheme || u.User != nil || u.Host != filesBase.Host || u.Opaque != "" {
		return "", deny(502, "slack: file %s lives at %q, not on %s://%s/; the token is not sent anywhere else", id, raw, filesBase.Scheme, filesBase.Host)
	}
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path, nil
}

// StripCredentials drops the legacy `token` parameter Slack still accepts:
// the only token the isolation holds is its workspace token, and even that
// does not belong on Slack's wire. What is kept goes on byte for byte.
func StripCredentials(query string) (string, *Denied) {
	pairs, err := parseForm(query)
	if err != nil {
		return "", deny(400, "slack: the query string is not valid URL encoding")
	}
	var kept []string
	for _, p := range pairs {
		if p.key != "token" {
			kept = append(kept, p.raw)
		}
	}
	return strings.Join(kept, "&"), nil
}

// Names resolves a channel id to its name with the env's own token.
type Names interface {
	Get(env, token, channelID string) (string, *Denied)
}

// GuardSearch pushes the deny list into every `query`. Slack search crosses
// the whole workspace in one call, which would make the messages clamp
// decorative unless the denial rides inside the query. Qualifiers are ANDed,
// so appending -in:#name excludes the channel even against a caller's own
// in: naming it: the two contradict and match nothing, which fails closed.
func GuardSearch(e Env, envName, query string, body []byte, ctype, token string, names Names) (string, []byte, *Denied) {
	if len(e.DenyChannels) == 0 {
		return query, body, nil
	}
	if len(body) > 0 {
		if t := mediaType(ctype); t != "" && t != "application/x-www-form-urlencoded" {
			return "", nil, deny(400, "slack: send the search query as a form or URL parameter; the deny-list qualifiers cannot be added to a %s body here", t)
		}
	}
	denied := append([]string(nil), e.DenyChannels...)
	sort.Strings(denied)
	var tail []string
	for _, c := range denied {
		name, d := names.Get(envName, token, c)
		if d != nil {
			return "", nil, d
		}
		tail = append(tail, "-in:#"+name)
	}
	exclusions := strings.Join(tail, " ")
	rewrite := func(encoded string) (string, *Denied) {
		pairs, err := parseForm(encoded)
		if err != nil {
			return "", deny(400, "slack: the search parameters are not valid URL encoding")
		}
		out := make([]string, len(pairs))
		for i, p := range pairs {
			out[i] = p.raw
			if p.key == "query" {
				k, _, _ := strings.Cut(p.raw, "=")
				out[i] = k + "=" + url.QueryEscape(strings.TrimSpace(p.value+" "+exclusions))
			}
		}
		return strings.Join(out, "&"), nil
	}
	q, d := rewrite(query)
	if d != nil {
		return "", nil, d
	}
	if len(body) > 0 {
		b, d := rewrite(string(body))
		if d != nil {
			return "", nil, d
		}
		body = []byte(b)
	}
	return q, body, nil
}
