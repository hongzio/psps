package grafanaproxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Ported from isolate's test_grafanaproxy.py: the decisions, pinned before
// the wire. Which endpoints are modeled at all (default-deny — Grafana's
// read surface includes a POSTed query), which family they belong to, how a
// query is authorized (every entry names an allowlisted UID, and the body
// that goes upstream is the one that was checked), how a save is bounded by
// folder, and which env a presented token names.

func mustDeny(t *testing.T, err error, status int, want string) {
	t.Helper()
	d, ok := err.(*Denied)
	if !ok {
		t.Fatalf("got %v, want a refusal", err)
	}
	if status != 0 && d.Status != status {
		t.Errorf("status %d, want %d (%s)", d.Status, status, d.Reason)
	}
	if !strings.Contains(d.Reason, want) {
		t.Errorf("%q does not mention %q", d.Reason, want)
	}
}

// --- the credential ---

func TestTheStoreYieldsTheToken(t *testing.T) {
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("grafana", "prod", map[string]string{"token": "glsa_secret"})
	if tok, err := readToken(cs, "prod"); err != nil || tok != "glsa_secret" {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestAMissingStoreNamesTheStanzaAsA503(t *testing.T) {
	_, err := readToken(&creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}, "prod")
	mustDeny(t, err, 503, "[grafana prod]")
	mustDeny(t, err, 503, "token")
}

// --- which env a token names ---

func TestTokenValuePicksTheEnv(t *testing.T) {
	toks := map[string]string{"dev": "tok-dev", "prod": "tok-prod"}
	envs := []string{"dev", "prod"}
	if ResolveEnv(toks, envs, "tok-dev") != "dev" || ResolveEnv(toks, envs, "tok-prod") != "prod" {
		t.Fatal("a token did not name its env")
	}
}

func TestAWrongOrEmptyTokenResolvesToNone(t *testing.T) {
	toks := map[string]string{"dev": "tok-dev", "prod": "tok-prod"}
	for _, bad := range []string{"nope", ""} {
		if got := ResolveEnv(toks, []string{"dev", "prod"}, bad); got != "" {
			t.Errorf("%q resolved to %q", bad, got)
		}
	}
	if got := ResolveEnv(nil, nil, "tok-dev"); got != "" {
		t.Fatalf("no envs resolved to %q", got)
	}
}

func TestAnEnvWithNoTokenMintedNeverMatchesAnEmptyOne(t *testing.T) {
	if got := ResolveEnv(map[string]string{}, []string{"dev"}, ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// --- the upstream address ---

func TestACloudURLParsesToHTTPSDefaultPort(t *testing.T) {
	if got, err := UpstreamParts("https://stack.grafana.net"); err != nil || got != (Upstream{"https", "stack.grafana.net", ""}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestASelfHostedURLKeepsPortAndSubpath(t *testing.T) {
	if got, err := UpstreamParts("http://10.0.0.5:3000/grafana/"); err != nil || got != (Upstream{"http", "10.0.0.5:3000", "/grafana"}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAURLWithoutASchemeOrHostIsRefused(t *testing.T) {
	for _, u := range []string{"stack.grafana.net", "https://", "", "ftp://host", "https://host?x=1", "https://host#f", "https://user:pw@host"} {
		if _, err := UpstreamParts(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

// --- routing ---

func route(t *testing.T, method, path string) (string, string) {
	t.Helper()
	f, k, err := Route(method, path)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return f, k
}

func TestHealthIsFamilyFree(t *testing.T) {
	if f, k := route(t, "GET", "/api/health"); f != "" || k != "" {
		t.Fatalf("%q %q", f, k)
	}
}

func TestReadsBelongToTheirFamily(t *testing.T) {
	for family, paths := range map[string][]string{
		"dashboards":  {"/api/search", "/api/dashboards/uid/abc-123"},
		"folders":     {"/api/folders", "/api/folders/abc123"},
		"datasources": {"/api/datasources", "/api/datasources/uid/abc123"},
		"alerts": {"/api/v1/provisioning/alert-rules", "/api/v1/provisioning/alert-rules/abc123",
			"/api/v1/provisioning/contact-points", "/api/prometheus/grafana/api/v1/rules"},
		"annotations": {"/api/annotations"},
	} {
		for _, p := range paths {
			if f, _ := route(t, "GET", p); f != family {
				t.Errorf("%s is %q, want %q", p, f, family)
			}
		}
	}
}

func TestADataSourceQueryIsTheQueryFamilyWithBodyCheck(t *testing.T) {
	if f, k := route(t, "POST", "/api/ds/query"); f != "query" || k != "query" {
		t.Fatalf("%q %q", f, k)
	}
}

func TestAQueryStringDoesNotChangeTheRoute(t *testing.T) {
	// RawTarget splits the query off before routing; a trailing slash is
	// the same endpoint.
	if f, _ := route(t, "GET", "/api/search/"); f != "dashboards" {
		t.Fatalf("%q", f)
	}
}

func TestADashboardSaveIsTheOneWriteOnTheTable(t *testing.T) {
	f, k := route(t, "POST", "/api/dashboards/db")
	if f != "dashboards" || k != "save" || !WriteKinds[k] {
		t.Fatalf("%q %q", f, k)
	}
}

func TestAWriteOnAModeledFamilyIsNamedAWrite(t *testing.T) {
	for _, c := range [][2]string{
		{"DELETE", "/api/dashboards/uid/abc"}, {"POST", "/api/dashboards/uid/abc/restore"},
		{"POST", "/api/dashboards/import"}, {"DELETE", "/api/datasources/uid/abc"},
		{"POST", "/api/folders"}, {"PUT", "/api/v1/provisioning/alert-rules/abc"},
		{"POST", "/api/annotations"}, {"DELETE", "/api/folders/abc"},
	} {
		_, _, err := Route(c[0], c[1])
		mustDeny(t, err, 403, "read-only")
	}
}

func TestAnUnmodeledReadIsRefusedNotForwarded(t *testing.T) {
	for _, p := range []string{"/api/orgs", "/api/users", "/api/admin/settings", "/api/serviceaccounts", "//api/search"} {
		_, _, err := Route("GET", p)
		mustDeny(t, err, 404, "not a modeled read endpoint")
	}
}

func TestATraversalPathIsRefused(t *testing.T) {
	for _, p := range []string{"/api/search/../admin/settings", "/api/search/%2e%2e/admin", "/api/./search"} {
		_, _, err := Route("GET", p)
		mustDeny(t, err, 400, "")
	}
}

// --- dashboard saves ---

var folders = []string{"sandbox"}

func save(t *testing.T, body map[string]any, lookup func(string) (string, bool, error)) ([]byte, error) {
	t.Helper()
	raw, _ := json.Marshal(body)
	if lookup == nil {
		lookup = func(string) (string, bool, error) { return "", false, nil }
	}
	return AuthorizeSave(raw, folders, lookup)
}

func dash(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestAFreshDashboardIntoAnAllowedFolderPasses(t *testing.T) {
	out, err := save(t, dash("dashboard", dash("title", "t"), "folderUid", "sandbox"), nil)
	var sent map[string]any
	json.Unmarshal(out, &sent)
	if err != nil || sent["folderUid"] != "sandbox" {
		t.Fatalf("%s %v", out, err)
	}
}

func TestAFolderOffTheAllowlistNamesTheOperatorCommand(t *testing.T) {
	_, err := save(t, dash("dashboard", dash("title", "t"), "folderUid", "prod"), nil)
	mustDeny(t, err, 403, "gero env edit grafana <env> --allow-folder prod")
}

func TestTheGeneralFolderIsNeverWritable(t *testing.T) {
	for _, body := range []map[string]any{
		dash("dashboard", dash("title", "t")),
		dash("dashboard", dash("title", "t"), "folderUid", ""),
	} {
		_, err := save(t, body, nil)
		mustDeny(t, err, 400, "General")
	}
}

func TestLegacyAndNestedFolderSpellingsAreRefused(t *testing.T) {
	for _, body := range []map[string]any{
		dash("dashboard", dash("title", "t"), "folderUid", "sandbox", "folderId", 7),
		dash("dashboard", dash("title", "t", "folderUid", "prod"), "folderUid", "sandbox"),
		dash("dashboard", dash("title", "t", "folderId", 3), "folderUid", "sandbox"),
	} {
		if _, err := save(t, body, nil); err == nil {
			t.Errorf("%v accepted", body)
		}
	}
}

func TestOverwritingADashboardThatLivesElsewhereIsAMove(t *testing.T) {
	// The check the body alone cannot make: a save naming an existing UID
	// overwrites it wherever it is, so landing inside the allowlist is not
	// enough.
	for _, current := range []string{"prod-folder", ""} {
		_, err := save(t, dash("dashboard", dash("uid", "abc", "title", "t"), "folderUid", "sandbox"),
			func(string) (string, bool, error) { return current, true, nil })
		mustDeny(t, err, 403, "already exists")
		if current == "" {
			mustDeny(t, err, 403, "General")
		}
	}
}

func TestUpdatingADashboardAlreadyInTheFolderPasses(t *testing.T) {
	var asked []string
	out, err := save(t, dash("dashboard", dash("uid", "abc", "id", 42, "title", "t", "version", 3), "folderUid", "sandbox", "overwrite", false),
		func(uid string) (string, bool, error) { asked = append(asked, uid); return "sandbox", true, nil })
	if err != nil || len(asked) != 1 || asked[0] != "abc" {
		t.Fatalf("%v %v", asked, err)
	}
	var sent struct{ Dashboard map[string]any }
	json.Unmarshal(out, &sent)
	// The id Grafana's own GET hands back is dropped: the forwarded body
	// names the dashboard one way only, the way that was checked.
	if _, ok := sent.Dashboard["id"]; ok || sent.Dashboard["uid"] != "abc" {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(string(out), `"version":3`) {
		t.Fatalf("the version was not kept exactly: %s", out)
	}
}

func TestANumericIDWithoutAUIDIsRefused(t *testing.T) {
	_, err := save(t, dash("dashboard", dash("id", 42, "title", "t"), "folderUid", "sandbox"), nil)
	mustDeny(t, err, 400, "not numeric id")
}

func TestAUIDOutsideGrafanasAlphabetIsRefused(t *testing.T) {
	for _, uid := range []any{"../x", "a/b", 5, ""} {
		_, err := save(t, dash("dashboard", dash("uid", uid, "title", "t"), "folderUid", "sandbox"), nil)
		mustDeny(t, err, 400, "Grafana UID")
	}
}

func TestAnEmptyFolderAllowlistNamesTheOperatorCommand(t *testing.T) {
	_, err := AuthorizeSave([]byte(`{"dashboard": {}}`), nil, nil)
	mustDeny(t, err, 403, "--allow-folder")
}

func TestAShapelessSaveIsRefused(t *testing.T) {
	for _, raw := range []string{"", "not json", "[]", `{"dashboard": "x"}`, `{"dashboard": {}} {"x": 1}`} {
		if _, err := AuthorizeSave([]byte(raw), folders, nil); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

// --- queries ---

var allowedUIDs = []string{"uid-prom", "uid-loki"}

func queryBody(queries ...any) []byte {
	if queries == nil {
		queries = []any{}
	}
	b, _ := json.Marshal(map[string]any{"queries": queries, "from": "now-1h", "to": "now"})
	return b
}

func TestAnAllowlistedUIDPassesAndTheCheckedBodyIsReturned(t *testing.T) {
	out, err := AuthorizeQueryBody(queryBody(dash("refId", "A", "datasource", dash("uid", "uid-prom"), "expr", "up")), allowedUIDs)
	var sent struct {
		Queries []struct{ Datasource struct{ UID string } }
	}
	json.Unmarshal(out, &sent)
	if err != nil || sent.Queries[0].Datasource.UID != "uid-prom" {
		t.Fatalf("%s %v", out, err)
	}
}

func TestEveryEntryMustNameAnAllowlistedUID(t *testing.T) {
	_, err := AuthorizeQueryBody(queryBody(
		dash("refId", "A", "datasource", dash("uid", "uid-prom")),
		dash("refId", "B", "datasource", dash("uid", "uid-other"))), allowedUIDs)
	mustDeny(t, err, 403, "uid-other")
	mustDeny(t, err, 403, "--allow-datasource")
}

func TestALegacyNameOrNumericIDCannotBypassTheAllowlist(t *testing.T) {
	for _, entry := range []any{
		dash("refId", "A", "datasource", "Prometheus"),
		dash("refId", "A", "datasourceId", 7),
		dash("refId", "A"),
	} {
		if _, err := AuthorizeQueryBody(queryBody(entry), allowedUIDs); err == nil {
			t.Errorf("%v accepted", entry)
		}
	}
}

func TestAShapelessQueryIsRefused(t *testing.T) {
	for _, raw := range []string{"", "[]", "not json", `{"queries": {}}`, `{"queries": ["x"]}`} {
		if _, err := AuthorizeQueryBody([]byte(raw), allowedUIDs); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestADuplicateKeyCannotSmuggleASecondDataSource(t *testing.T) {
	// Whatever reading the check made is the only one that leaves.
	raw := []byte(`{"queries": [{"refId": "A", "datasource": {"uid": "uid-secret", "uid": "uid-prom"}}]}`)
	out, err := AuthorizeQueryBody(raw, allowedUIDs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "uid-secret") {
		t.Fatalf("the unchecked spelling went out: %s", out)
	}
}

func TestAnEmptyQueryListIsRefusedNotForwarded(t *testing.T) {
	_, err := AuthorizeQueryBody(queryBody(), allowedUIDs)
	mustDeny(t, err, 400, "empty")
}

func TestAnEmptyAllowlistNamesTheOperatorCommand(t *testing.T) {
	_, err := AuthorizeQueryBody(queryBody(dash("refId", "A", "datasource", dash("uid", "uid-prom"))), nil)
	mustDeny(t, err, 403, "--allow-datasource")
}

// --- env definitions ---

func TestNormalizeDropsAWriteWhoseReadIsShutAndRefusesWhatCannotBeOpened(t *testing.T) {
	// A write never opens a read nobody named: it is dropped, and the
	// warning names the flags that keep both.
	env := map[string]any{"base_url": "https://g.example.net", "writes": []string{"dashboards"},
		"families": []string{"query"}, "datasources": []string{"uid-prom", "uid-prom"}, "folders": []string{"agent-sbx"}}
	warn, err := Service{}.Normalize("prod", env)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env["families"].([]string), ","); got != "query" {
		t.Fatalf("families %q", got)
	}
	if got := env["writes"].([]string); len(got) != 0 {
		t.Fatalf("writes %v", got)
	}
	if !strings.Contains(strings.Join(warn, "\n"), "--allow-api dashboards --allow-write dashboards") {
		t.Fatalf("warnings %v", warn)
	}
	env = map[string]any{"base_url": "https://g.example.net", "writes": []string{"dashboards"},
		"families": []string{"query", "dashboards"}, "datasources": []string{"uid-prom", "uid-prom"}, "folders": []string{"agent-sbx"}}
	if warn, err = (Service{}).Normalize("prod", env); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env["writes"].([]string), ","); got != "dashboards" {
		t.Fatalf("writes %q", got)
	}
	if got := strings.Join(env["datasources"].([]string), ","); got != "uid-prom" {
		t.Fatalf("datasources %q", got)
	}
	if !strings.Contains(strings.Join(warn, "\n"), "opens dashboard saves") || !strings.Contains(strings.Join(warn, "\n"), "Viewer") {
		t.Fatalf("warnings %v", warn)
	}
	for want, bad := range map[string]map[string]any{
		"--base-url is required": {},
		"not a usable Grafana":   {"base_url": "g.example.net"},
		"not an endpoint family": {"base_url": "https://g", "families": []string{"users"}},
		"has no write surface":   {"base_url": "https://g", "writes": []string{"folders"}},
		"not a data source UID":  {"base_url": "https://g", "datasources": []string{"a/b"}},
		"not a folder UID":       {"base_url": "https://g", "folders": []string{"../x"}},
	} {
		if _, err := (Service{}).Normalize("prod", bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestNormalizeWarnsAboutAnAllowlistThatAnswersNothing(t *testing.T) {
	warn, _ := Service{}.Normalize("prod", map[string]any{"base_url": "https://g", "families": []string{"query"}})
	all := strings.Join(warn, "\n")
	if !strings.Contains(all, "no --allow-datasource") {
		t.Fatalf("%v", warn)
	}
	warn, _ = Service{}.Normalize("prod", map[string]any{"base_url": "https://g"})
	if !strings.Contains(strings.Join(warn, "\n"), "only /api/health") {
		t.Fatalf("%v", warn)
	}
}

// --- the skill (isolate's test_grafana_skill.py) ---

func skillView() proxy.View {
	envs := map[string]Env{
		"prod": {BaseURL: "https://g.example.net", Families: []string{"dashboards", "query"},
			Datasources: []string{"uid-prom"}, Writes: []string{"dashboards"}, Folders: []string{"agent-sbx"}},
		"dev": {BaseURL: "https://dev.example.net", Families: []string{"dashboards"}},
	}
	v := proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{},
		Tokens: map[string]string{"prod": "tp", "dev": "td"}}
	v.Exposure = store.Exposure{Envs: []string{"prod", "dev"}, Default: "prod"}
	for n, e := range envs {
		v.Envs[n], _ = json.Marshal(e)
	}
	return v
}

func skillText(t *testing.T) string {
	t.Helper()
	ws, err := Service{}.Workspace(skillView())
	if err != nil {
		t.Fatal(err)
	}
	return ws.Files[0].Content
}

func TestAnEnvWithSavesNamesItsFolders(t *testing.T) {
	want := "`prod` (default) — https://g.example.net, families: dashboards, query; query data sources: uid-prom; saves dashboards into folders: agent-sbx"
	if !strings.Contains(skillText(t), want) {
		t.Fatalf("missing %q", want)
	}
}

func TestAnEnvWithNoWritesReadsAsReadOnly(t *testing.T) {
	want := "`dev` — https://dev.example.net, families: dashboards; saves: none (read-only)"
	if !strings.Contains(skillText(t), want) {
		t.Fatalf("missing %q", want)
	}
}

func TestTheSkillNoLongerSaysEveryWriteIsRefused(t *testing.T) {
	text := skillText(t)
	if strings.Contains(text, "every write (saving a dashboard") || !strings.Contains(text, "--allow-write dashboards --allow-folder") {
		t.Fatal(text)
	}
	if strings.Contains(text, "%!") || strings.Contains(text, "isolate ") {
		t.Fatalf("a formatting slip or an isolate command:\n%s", text)
	}
}

func TestTheOverwriteRuleIsSpelledOut(t *testing.T) {
	// An agent that does not know about the lookup would copy a production
	// dashboard with its own UID and read the refusal as a bug.
	text := skillText(t)
	if !strings.Contains(text, "already lives in an allowlisted folder") || !strings.Contains(text, "new UID") {
		t.Fatal(text)
	}
}

func TestTheWorkspaceGetsATokenPerEnv(t *testing.T) {
	ws, _ := Service{}.Workspace(skillView())
	env := strings.Join(ws.Env, "\n")
	for _, want := range []string{"GRAFANA_URL=http://127.0.0.1:3027", "GRAFANA_TOKEN=tp", "GRAFANA_SERVICE_ACCOUNT_TOKEN=tp", "GRAFANA_TOKEN_PROD=tp", "GRAFANA_TOKEN_DEV=td"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	if len(ws.Files) != 2 || !strings.HasSuffix(ws.Files[0].Path, "gero-grafana/SKILL.md") {
		t.Fatalf("%+v", ws.Files)
	}
}
