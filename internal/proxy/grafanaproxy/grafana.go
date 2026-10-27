// Package grafanaproxy is the Grafana proxy, a port of isolate's
// grafanaproxy.py: the isolation gets dashboards and queries without
// getting a service account token, and saves dashboards only into folders
// the operator opened.
//
// The surface covers what an RCA needs — dashboard search, alert rules, and
// /api/ds/query, the endpoint that actually runs a PromQL or LogQL query
// through Grafana:
//
//	isolation  curl -H "Authorization: Bearer $GRAFANA_TOKEN" "$GRAFANA_URL/api/search"
//	             |  (the isolation's tunnel)
//	daemon     this proxy: endpoint-family allowlist, dashboard saves only
//	             |  into allowlisted folders, data source UID allowlist on
//	             |  queries, its own budget
//	           https://<base_url>, with the real token attached
//
// GRAFANA_TOKEN is a per-environment workspace token: the proxy needs the
// caller to prove it is the isolation, and, since a Bearer header has no
// slot for an environment, the token value is also how the isolation picks
// dev or prod. The real service account token lives in the credentials file
// ([grafana <env>]), read per request.
//
// Four boundaries, in the order they are checked:
//
//   - By table, never by verb. The forwardable surface is two explicit
//     lists, the reads and the one write. Verb filtering fails here in both
//     directions: /api/ds/query is a POST that reads.
//   - Families. Which endpoint families open is the operator's allowlist,
//     and a refusal names the exact command that would widen it.
//   - Dashboard saves are a second decision, bounded by folder. POST
//     /api/dashboards/db forwards only where --allow-write dashboards says
//     so, and only when both the folder the save lands in and the folder the
//     dashboard already lives in are allowlisted. The second check is the one
//     that matters: a save naming an existing UID overwrites that dashboard
//     wherever it is. Deletes are on no table.
//   - Queries name an allowlisted data source, and the checked body is the
//     sent body: every /api/ds/query entry must carry an allowlisted UID, and
//     the body is re-serialised after the check so a parsing differential
//     upstream cannot smuggle a second reading past it.
//
// The service account is the last boundary, and it belongs to Grafana:
// issue the token to a Viewer-role account, with Edit on exactly the
// allowlisted folders if saves are open. The proxy narrows; the role
// enforces.
package grafanaproxy

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Families are the endpoint families an env may open.
var Families = []string{"dashboards", "folders", "datasources", "alerts", "annotations", "query"}

// Writable are the families that can be opened for writing at all. Only
// dashboards: folders, data sources and alert rules are org plumbing whose
// blast radius no folder allowlist bounds.
var Writable = []string{"dashboards"}

// Denied is a refusal with the HTTP status it is answered with.
type Denied struct {
	Reason string
	Status int
}

func (d *Denied) Error() string { return d.Reason }

func deny(status int, format string, a ...any) *Denied {
	return &Denied{Reason: fmt.Sprintf(format, a...), Status: status}
}

// ResolveEnv is which exposed env the presented workspace token names, or
// "". Every candidate is compared, so a mismatch costs what a match does,
// and an env with no token minted never matches.
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

// Upstream is an env's base URL, split: a self-hosted Grafana behind a
// subpath (https://host/grafana) keeps working.
type Upstream struct {
	Scheme, Host, Prefix string
}

func (u Upstream) url(pathAndQuery string) string {
	return u.Scheme + "://" + u.Host + u.Prefix + pathAndQuery
}

// UpstreamParts validates a base URL here rather than at the socket, so
// `gero env add` can refuse one the proxy could never dial.
func UpstreamParts(base string) (Upstream, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return Upstream{}, deny(502, "%q is not a usable Grafana base URL; expected http(s)://host[:port][/subpath]", base)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Upstream{}, deny(502, "a Grafana base URL takes no query or fragment: %q", base)
	}
	if u.User != nil {
		// A credential in the URL would be a second copy of the token
		// outside the credentials file.
		return Upstream{}, deny(502, "a Grafana base URL takes no user or password: %q; the token goes in `gero env secret grafana <env>`", base)
	}
	return Upstream{Scheme: u.Scheme, Host: u.Host, Prefix: strings.TrimRight(u.EscapedPath(), "/")}, nil
}

// --- routing ---------------------------------------------------------------

type endpoint struct {
	method  string
	pattern *regexp.Regexp
	family  string // "" for /api/health, which needs no family
	kind    string // "query" and "save" carry their decision in the body
}

func ep(method, pattern, family, kind string) endpoint {
	return endpoint{method, regexp.MustCompile("^" + pattern + "$"), family, kind}
}

// readEndpoints is the read half of the forwardable surface. An endpoint on
// neither table does not exist as far as the isolation is concerned:
// default-deny makes the boundary a property of the table rather than a
// hope about HTTP verbs, since Grafana's read surface includes a POSTed
// query.
var readEndpoints = []endpoint{
	ep("GET", `/api/health`, "", ""),
	ep("GET", `/api/search`, "dashboards", ""),
	ep("GET", `/api/dashboards/uid/[A-Za-z0-9_-]+`, "dashboards", ""),
	ep("GET", `/api/folders`, "folders", ""),
	ep("GET", `/api/folders/[A-Za-z0-9_-]+`, "folders", ""),
	ep("GET", `/api/datasources`, "datasources", ""),
	ep("GET", `/api/datasources/uid/[A-Za-z0-9_-]+`, "datasources", ""),
	ep("GET", `/api/v1/provisioning/alert-rules`, "alerts", ""),
	ep("GET", `/api/v1/provisioning/alert-rules/[A-Za-z0-9_-]+`, "alerts", ""),
	ep("GET", `/api/v1/provisioning/contact-points`, "alerts", ""),
	ep("GET", `/api/prometheus/grafana/api/v1/rules`, "alerts", ""),
	ep("GET", `/api/annotations`, "annotations", ""),
	ep("POST", `/api/ds/query`, "query", "query"),
}

// writeEndpoints is the write half: one endpoint. Saving is create-or-update
// by UID, so it is the whole of "edit a dashboard". Absent on purpose:
// deletes, version restores, permission edits, and the
// /apis/dashboard.grafana.app/... resource API — a second spelling of the
// same save would be a second allowlist to keep.
var writeEndpoints = []endpoint{
	ep("POST", `/api/dashboards/db`, "dashboards", "save"),
}

// WriteKinds are the kinds whose endpoint changes state upstream.
var WriteKinds = map[string]bool{"save": true}

// Route is the (family, kind) a modeled endpoint names. Whether a write is
// permitted is the env's business, one layer up. Percent-encoding and dot
// segments are refused rather than normalised: the table is only as good
// as its agreement with Grafana about which endpoint a path names.
func Route(method, rawPath string) (family, kind string, err error) {
	if strings.ContainsAny(rawPath, "%\\\x00") {
		return "", "", deny(400, "grafana: encoded characters are not accepted in an API path")
	}
	for _, seg := range strings.Split(rawPath, "/") {
		if seg == "." || seg == ".." {
			return "", "", deny(400, "grafana: dot segments are not accepted in an API path")
		}
	}
	path := strings.TrimRight(rawPath, "/")
	if path == "" {
		path = "/"
	}
	for _, table := range [][]endpoint{readEndpoints, writeEndpoints} {
		for _, e := range table {
			if method == e.method && e.pattern.MatchString(path) {
				return e.family, e.kind, nil
			}
		}
	}
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		return "", "", deny(403, "grafana: %s %s is a write this proxy never forwards; Grafana is read-only here except for saving a dashboard (POST /api/dashboards/db) into an allowlisted folder, and only where the operator opened it", method, path)
	}
	return "", "", deny(404, "grafana: %s %s is not a modeled read endpoint; this proxy forwards a fixed read-only surface", method, path)
}

// --- bodies ------------------------------------------------------------------

// UID is Grafana's own UID alphabet, enforced at `gero env add` so the
// allowlist only holds values the route table's uid patterns can express.
var UID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// decodeObject parses a JSON body keeping numbers exact, so re-encoding
// does not round a dashboard version or id.
func decodeObject(raw []byte) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// AuthorizeQueryBody authorizes a /api/ds/query body entry by entry and
// returns it re-encoded: the bytes that go upstream are the serialisation
// of exactly the object that was checked. The legacy string and numeric-id
// addressing forms are refused, because an allowlist with three spellings
// is one.
func AuthorizeQueryBody(raw []byte, allowed []string) ([]byte, error) {
	if len(allowed) == 0 {
		return nil, deny(403, "grafana: no data sources are allowlisted for queries in this environment. Ask the operator to run:\n  gero env edit grafana <env> --allow-datasource <uid>")
	}
	body, ok := decodeObject(raw)
	queries, isList := body["queries"].([]any)
	if !ok || !isList {
		return nil, deny(400, "grafana: a query body must be a JSON object with a queries list")
	}
	if len(queries) == 0 {
		return nil, deny(400, "grafana: the queries list is empty; nothing to authorize, nothing to forward")
	}
	for _, q := range queries {
		entry, ok := q.(map[string]any)
		if !ok {
			return nil, deny(400, "grafana: each query must be a JSON object")
		}
		if _, ok := entry["datasourceId"]; ok {
			return nil, deny(400, "grafana: numeric datasourceId addressing is refused; name the data source by UID (datasource: {uid: ...})")
		}
		src, _ := entry["datasource"].(map[string]any)
		uid, isStr := src["uid"].(string)
		if src == nil || !isStr {
			return nil, deny(400, "grafana: each query must name its data source by UID (datasource: {uid: ...}); the legacy name form is refused")
		}
		if !contains(allowed, uid) {
			return nil, deny(403, "grafana: data source %q is not in the query allowlist for this environment. Ask the operator to run:\n  gero env edit grafana <env> --allow-datasource %s", uid, uid)
		}
	}
	return encode(body), nil
}

func folderRefusal(uid, what string) *Denied {
	return deny(403, "grafana: %s folder %q, which is not in the folder allowlist for this environment. Ask the operator to run:\n  gero env edit grafana <env> --allow-folder %s", what, uid, uid)
}

func isZero(v any) bool {
	switch n := v.(type) {
	case nil:
		return true
	case json.Number:
		f, err := n.Float64()
		return err == nil && f == 0
	}
	return false
}

// AuthorizeDashboardBody authorizes a /api/dashboards/db body and returns
// it re-encoded, with the UID of the dashboard it would overwrite ("" for
// a fresh one).
//
// The folder must be named by UID and allowlisted; the General folder (no
// folderUid) is never on the list, since an entry cannot spell it. The
// legacy numeric forms are refused: folderId is a second way to name the
// folder, and a dashboard.id without a UID a second way to name the
// dashboard that the upstream lookup could not check. An id alongside a
// UID is dropped, because that is the shape Grafana's own GET hands back.
func AuthorizeDashboardBody(raw []byte, allowed []string) ([]byte, string, error) {
	if len(allowed) == 0 {
		return nil, "", deny(403, "grafana: no folders are allowlisted for dashboard saves in this environment. Ask the operator to run:\n  gero env edit grafana <env> --allow-folder <uid>")
	}
	body, ok := decodeObject(raw)
	dash, isObj := body["dashboard"].(map[string]any)
	if !ok || !isObj {
		return nil, "", deny(400, "grafana: a dashboard save must be a JSON object with a dashboard object")
	}
	if _, ok := body["folderId"]; ok {
		return nil, "", deny(400, "grafana: numeric folderId addressing is refused; name the folder by UID (folderUid)")
	}
	for _, k := range []string{"folderUid", "folderId"} {
		if _, ok := dash[k]; ok {
			return nil, "", deny(400, "grafana: the folder is named once, by the top-level folderUid; drop dashboard.%s", k)
		}
	}
	folder, _ := body["folderUid"].(string)
	if folder == "" {
		return nil, "", deny(400, "grafana: a dashboard save must name its folder by UID (folderUid); the General folder is never writable here")
	}
	if !contains(allowed, folder) {
		return nil, "", folderRefusal(folder, "this save lands in")
	}
	uid := ""
	if v, present := dash["uid"]; present && v != nil {
		s, isStr := v.(string)
		if !isStr || !UID.MatchString(s) {
			return nil, "", deny(400, "grafana: dashboard.uid must be a Grafana UID (letters, digits, - and _)")
		}
		uid = s
	}
	if uid == "" && !isZero(dash["id"]) {
		return nil, "", deny(400, "grafana: a dashboard save addresses an existing dashboard by uid, not numeric id")
	}
	delete(dash, "id")
	return encode(body), uid, nil
}

// AuthorizeSave is the whole save decision: the body, then where its UID
// lives now. lookup returns the folder the dashboard is in ("" for
// General), or found=false for none. A save that would overwrite a
// dashboard outside the allowlist is refused even when it lands inside it:
// that is a move out of a folder the operator never opened.
func AuthorizeSave(raw []byte, allowed []string, lookup func(uid string) (folder string, found bool, err error)) ([]byte, error) {
	body, uid, err := AuthorizeDashboardBody(raw, allowed)
	if err != nil {
		return nil, err
	}
	if uid != "" {
		current, found, err := lookup(uid)
		if err != nil {
			return nil, err
		}
		if found && !contains(allowed, current) {
			if current == "" {
				current = "General"
			}
			return nil, folderRefusal(current, fmt.Sprintf("dashboard %s already exists, and lives in", uid))
		}
	}
	return body, nil
}
