// Package ddproxy is the Datadog proxy, a port of isolate's ddproxy.py: the
// isolation gets telemetry without getting a key, and writes only where the
// operator opened them.
//
// Datadog's read APIs need two credentials at once (DD-API-KEY names the
// org, DD-APPLICATION-KEY a user's permissions) and everything except
// /api/v1/validate requires both. So the pair goes into the path instead of
// the isolation:
//
//	isolation  curl "$DATADOG_HOST/api/v1/monitor" -H "DD-API-KEY: $DD_API_KEY"
//	             |  (the isolation's tunnel)
//	daemon     this proxy: endpoint-family allowlist, per-family write
//	             |  permission, enforced log scope, its own rate budget
//	           https://api.datadoghq.com, with both real keys attached
//
// DD_API_KEY is a per-environment workspace token: SDKs refuse to build a
// client with no key set, the proxy needs the caller to prove it is the
// isolation, and, since Datadog's wire has no slot for an environment, the
// token value is also how the isolation picks dev or prod. The real keys
// live in the credentials file ([datadog <env>]), read per request.
//
// Four boundaries, in the order they are checked:
//
//   - By table, never by verb. The forwardable surface is two explicit
//     lists, the reads and the writes each family can offer. An endpoint on
//     neither does not exist from here; Datadog's own read surface includes
//     POSTed searches, so a verb check would be a hope, not a boundary.
//   - Families. Which endpoint families open is the operator's allowlist,
//     and a refusal names the exact command that would widen it.
//   - Writes are a second decision, per family. Reading monitors never
//     implies muting one, and --allow-write dashboards buys dashboards
//     only. POST /api/v1/monitor/mute_all is on no list: silencing the whole
//     org is wider than anything --allow-write monitors promises.
//   - The log scope is rewritten, never validated. A prefix check dies to
//     one boolean OR; (theirs) AND (scope) cannot be composed away.
//
// The rate budget refuses rather than queues: Datadog's limits are per org
// and shared with humans and CI.
package ddproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const DefaultSite = "datadoghq.com"

var (
	Families = []string{"monitors", "logs", "metrics", "events", "traces", "dashboards"}
	// Writable are the families that can be opened for writing at all.
	// logs and traces are absent: submission goes to the intake hosts, not
	// this API, and pipeline configuration is org-wide plumbing.
	Writable = []string{"monitors", "dashboards", "events", "metrics"}
)

// Denied is a refusal with the HTTP status it is answered with.
type Denied struct {
	Reason string
	Status int
}

func (d *Denied) Error() string { return d.Reason }

func deny(status int, format string, a ...any) *Denied {
	return &Denied{Reason: fmt.Sprintf(format, a...), Status: status}
}

// Scope says how the enforced log query reaches a request: through the
// JSON body, the query parameters, or not at all.
type Scope string

const (
	NoScope     Scope = ""
	BodyScope   Scope = "body"
	ParamsScope Scope = "params"
)

type readEndpoint struct {
	method  string
	pattern *regexp.Regexp
	family  string // "" for validate, which needs no family
	scope   Scope
}

type writeEndpoint struct {
	method  string
	pattern *regexp.Regexp
	family  string
}

func re(p string) *regexp.Regexp { return regexp.MustCompile("^" + p + "$") }

// ReadEndpoints is the read half of the forwardable surface.
var ReadEndpoints = []readEndpoint{
	{"GET", re(`/api/v1/validate`), "", NoScope},
	{"GET", re(`/api/v1/monitor`), "monitors", NoScope},
	{"GET", re(`/api/v1/monitor/\d+`), "monitors", NoScope},
	{"GET", re(`/api/v1/monitor/search`), "monitors", NoScope},
	{"GET", re(`/api/v1/monitor/groups/search`), "monitors", NoScope},
	{"GET", re(`/api/v1/downtime`), "monitors", NoScope},
	{"GET", re(`/api/v1/downtime/\d+`), "monitors", NoScope},
	{"GET", re(`/api/v2/downtime`), "monitors", NoScope},
	{"GET", re(`/api/v2/downtime/[A-Za-z0-9-]+`), "monitors", NoScope},
	{"POST", re(`/api/v2/logs/events/search`), "logs", BodyScope},
	{"POST", re(`/api/v2/logs/analytics/aggregate`), "logs", BodyScope},
	{"GET", re(`/api/v2/logs/events`), "logs", ParamsScope},
	// Pipeline and index definitions are configuration, not log content,
	// so they carry no scope: there is no query of the caller's to wrap.
	{"GET", re(`/api/v1/logs/config/pipelines`), "logs", NoScope},
	{"GET", re(`/api/v1/logs/config/pipelines/[A-Za-z0-9_-]+`), "logs", NoScope},
	{"GET", re(`/api/v1/logs/config/pipeline-order`), "logs", NoScope},
	{"GET", re(`/api/v1/logs/config/indexes`), "logs", NoScope},
	{"GET", re(`/api/v1/logs/config/indexes/[A-Za-z0-9_-]+`), "logs", NoScope},
	{"GET", re(`/api/v1/logs/config/index-order`), "logs", NoScope},
	{"GET", re(`/api/v1/query`), "metrics", NoScope},
	{"GET", re(`/api/v1/metrics`), "metrics", NoScope},
	{"GET", re(`/api/v1/metrics/[^/]+`), "metrics", NoScope},
	{"GET", re(`/api/v2/metrics`), "metrics", NoScope},
	{"POST", re(`/api/v2/query/timeseries`), "metrics", NoScope},
	{"GET", re(`/api/v1/events`), "events", NoScope},
	{"GET", re(`/api/v1/events/\d+`), "events", NoScope},
	{"POST", re(`/api/v2/events/search`), "events", NoScope},
	{"POST", re(`/api/v2/spans/events/search`), "traces", NoScope},
	{"GET", re(`/api/v2/spans/events`), "traces", NoScope},
	{"GET", re(`/api/v1/dashboard`), "dashboards", NoScope},
	{"GET", re(`/api/v1/dashboard/[A-Za-z0-9-]+`), "dashboards", NoScope},
	{"GET", re(`/api/v1/slo`), "dashboards", NoScope},
	{"GET", re(`/api/v1/slo/[^/]+`), "dashboards", NoScope},
}

// WriteEndpoints is the write half. Nothing here forwards unless the env
// names its family in both families and writes. Absent on purpose: POST
// /api/v1/monitor/mute_all.
var WriteEndpoints = []writeEndpoint{
	{"POST", re(`/api/v1/monitor`), "monitors"},
	{"PUT", re(`/api/v1/monitor/\d+`), "monitors"},
	{"DELETE", re(`/api/v1/monitor/\d+`), "monitors"},
	{"POST", re(`/api/v1/monitor/\d+/mute`), "monitors"},
	{"POST", re(`/api/v1/monitor/\d+/unmute`), "monitors"},
	{"POST", re(`/api/v1/downtime`), "monitors"},
	{"PUT", re(`/api/v1/downtime/\d+`), "monitors"},
	{"DELETE", re(`/api/v1/downtime/\d+`), "monitors"},
	{"POST", re(`/api/v2/downtime`), "monitors"},
	{"PATCH", re(`/api/v2/downtime/[A-Za-z0-9-]+`), "monitors"},
	{"DELETE", re(`/api/v2/downtime/[A-Za-z0-9-]+`), "monitors"},
	{"POST", re(`/api/v1/dashboard`), "dashboards"},
	{"PUT", re(`/api/v1/dashboard/[A-Za-z0-9-]+`), "dashboards"},
	{"DELETE", re(`/api/v1/dashboard/[A-Za-z0-9-]+`), "dashboards"},
	{"POST", re(`/api/v1/slo`), "dashboards"},
	{"PUT", re(`/api/v1/slo/[^/]+`), "dashboards"},
	{"DELETE", re(`/api/v1/slo/[^/]+`), "dashboards"},
	{"POST", re(`/api/v1/events`), "events"},
	{"POST", re(`/api/v1/series`), "metrics"},
	{"POST", re(`/api/v1/distribution_points`), "metrics"},
	{"PUT", re(`/api/v1/metrics/[^/]+`), "metrics"},
}

// familyPrefixes are only for phrasing refusals: a request that missed the
// table still deserves to hear which family it was reaching for.
var familyPrefixes = []struct{ prefix, family string }{
	{"/api/v1/monitor", "monitors"},
	{"/api/v1/downtime", "monitors"},
	{"/api/v2/downtime", "monitors"},
	{"/api/v1/logs", "logs"},
	{"/api/v2/logs", "logs"},
	{"/api/v1/metrics", "metrics"},
	{"/api/v1/query", "metrics"},
	{"/api/v1/series", "metrics"},
	{"/api/v1/distribution_points", "metrics"},
	{"/api/v2/metrics", "metrics"},
	{"/api/v2/query", "metrics"},
	{"/api/v1/events", "events"},
	{"/api/v2/events", "events"},
	{"/api/v2/spans", "traces"},
	{"/api/v1/dashboard", "dashboards"},
	{"/api/v1/slo", "dashboards"},
}

func familyOf(path string) string {
	for _, p := range familyPrefixes {
		if path == p.prefix || strings.HasPrefix(path, p.prefix+"/") {
			return p.family
		}
	}
	return ""
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Match is what Route found: the family ("" for validate), how the log
// scope applies, and whether the endpoint is a write.
type Match struct {
	Family string
	Scope  Scope
	Write  bool
}

// Route is the modeled endpoint a request names, or a refusal. Whether a
// write is permitted is the env's business, settled one layer up, so this
// stays a pure statement about the API surface. Percent-encoding and dot
// segments are refused outright rather than normalised: the table is only
// as good as its agreement with Datadog about which endpoint a path names.
func Route(method, rawPath string) (Match, error) {
	path, _, _ := strings.Cut(rawPath, "?")
	if strings.ContainsAny(path, "%\\\x00") {
		return Match{}, deny(400, "datadog: encoded characters are not accepted in an API path")
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return Match{}, deny(400, "datadog: dot segments are not accepted in an API path")
		}
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	// Reads first: Datadog answers two of its biggest reads to POST, so a
	// POSTed search must match as a read before the write table sees it.
	for _, e := range ReadEndpoints {
		if method == e.method && e.pattern.MatchString(path) {
			return Match{Family: e.family, Scope: e.scope}, nil
		}
	}
	for _, e := range WriteEndpoints {
		if method == e.method && e.pattern.MatchString(path) {
			return Match{Family: e.family, Write: true}, nil
		}
	}
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		family := familyOf(path)
		if contains(Writable, family) {
			return Match{}, deny(404, "datadog: %s %s is not a modeled write endpoint; the %q family's writes are a fixed list and this is not on it", method, path, family)
		}
		if family != "" {
			return Match{}, deny(403, "datadog: %s %s is a write, and the %q family has no write surface here", method, path, family)
		}
		return Match{}, deny(404, "datadog: %s %s is not a modeled endpoint; this proxy forwards a fixed surface", method, path)
	}
	return Match{}, deny(404, "datadog: %s %s is not a modeled read endpoint; this proxy forwards a fixed surface", method, path)
}

// ScopedQuery welds the operator's scope onto the caller's query with an
// AND. Rewritten, never validated: `service:project OR service:other`
// passes any prefix check; parenthesised under an AND it cannot escape. An
// empty or wildcard query becomes the scope itself.
func ScopedQuery(query, scope string) string {
	if scope == "" {
		return query
	}
	query = strings.TrimSpace(query)
	if query == "" || query == "*" {
		return scope
	}
	return "(" + query + ") AND (" + scope + ")"
}

// WrapLogsBody is the POSTed search body with its filter.query rewritten.
// Numbers are kept as written; key order is not significant to Datadog.
func WrapLogsBody(raw []byte, scope string) ([]byte, error) {
	var body any = map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil || dec.More() {
			body = nil
		}
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return nil, deny(400, "datadog: the log search body is not a JSON object, so the enforced query scope cannot be applied")
	}
	filter, present := obj["filter"]
	if !present || filter == nil {
		filter = map[string]any{}
	}
	f, ok := filter.(map[string]any)
	if !ok {
		return nil, deny(400, "datadog: the log search filter is not a JSON object, so the enforced query scope cannot be applied")
	}
	var q string
	switch v := f["query"].(type) {
	case nil:
	case string:
		q = v
	default:
		return nil, deny(400, "datadog: the log search filter.query is not a string, so the enforced query scope cannot be applied")
	}
	f["query"] = ScopedQuery(q, scope)
	obj["filter"] = f
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, deny(400, "datadog: the log search body could not be re-encoded: %v", err)
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// pair is one query parameter as sent, with its decoded name and value.
type pair struct{ raw, key, value string }

// parseQuery splits a query string without re-encoding it. Names are
// decoded for comparison, as Datadog reads them; a malformed escape is
// refused rather than guessed at.
func parseQuery(q string) ([]pair, error) {
	var out []pair
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			return nil, deny(400, "datadog: query parameter %q is not valid URL encoding", k)
		}
		out = append(out, pair{part, key, value})
	}
	return out, nil
}

// WrapLogsParams is the GET variant: filter[query] rewritten in the query
// string, every other parameter forwarded byte for byte.
func WrapLogsParams(q, scope string) (string, error) {
	pairs, err := parseQuery(q)
	if err != nil {
		return "", err
	}
	var kept []string
	existing := ""
	for _, p := range pairs {
		if p.key == "filter[query]" {
			existing = p.value
			continue
		}
		kept = append(kept, p.raw)
	}
	kept = append(kept, url.QueryEscape("filter[query]")+"="+url.QueryEscape(ScopedQuery(existing, scope)))
	return strings.Join(kept, "&"), nil
}

// StripCredentials drops the api_key and application_key parameters
// datadogpy sends, returning what is left and the api_key presented.
func StripCredentials(q string) (string, string, error) {
	pairs, err := parseQuery(q)
	if err != nil {
		return "", "", err
	}
	var kept []string
	presented := ""
	for _, p := range pairs {
		switch p.key {
		case "api_key":
			presented = p.value
		case "application_key":
		default:
			kept = append(kept, p.raw)
		}
	}
	return strings.Join(kept, "&"), presented, nil
}
