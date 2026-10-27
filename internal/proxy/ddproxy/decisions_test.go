package ddproxy

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
)

// Ported from isolate's test_ddproxy.py: the boundaries the isolation
// cannot cross, pinned before the wire exists. Which endpoints are modeled
// at all (default-deny), which family they belong to, how the enforced log
// scope is welded onto a query (rewritten, never merely validated), which
// env a presented workspace token names, and how the credentials file must
// look before it is trusted.

func newStore(t *testing.T) *creds.Store {
	t.Helper()
	return &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
}

func asDenied(t *testing.T, err error) *Denied {
	t.Helper()
	d, ok := err.(*Denied)
	if !ok {
		t.Fatalf("%v (%T) is not a refusal", err, err)
	}
	return d
}

// --- keys ---

func TestTheStoreYieldsBothKeys(t *testing.T) {
	c := newStore(t)
	c.Write("datadog", "prod", map[string]string{"api_key": "aaa", "app_key": "bbb"})
	api, app, err := readKeys(c, "prod")
	if err != nil || api != "aaa" || app != "bbb" {
		t.Fatalf("%q %q %v", api, app, err)
	}
}

func TestAMissingStoreNamesTheStanzaAsA503(t *testing.T) {
	_, _, err := readKeys(newStore(t), "prod")
	d := asDenied(t, err)
	if d.Status != 503 || !strings.Contains(d.Reason, "[datadog prod]") || !strings.Contains(d.Reason, "api_key") {
		t.Fatalf("%+v", d)
	}
}

func TestAMissingAppKeySaysWhichKeyIsMissing(t *testing.T) {
	c := newStore(t)
	c.Write("datadog", "prod", map[string]string{"api_key": "aaa"})
	_, _, err := readKeys(c, "prod")
	if d := asDenied(t, err); !strings.Contains(d.Reason, "app_key") {
		t.Fatalf("%+v", d)
	}
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
	envs := []string{"dev", "prod"}
	for _, bad := range []string{"nope", ""} {
		if got := ResolveEnv(toks, envs, bad); got != "" {
			t.Errorf("%q resolved to %q", bad, got)
		}
	}
	if got := ResolveEnv(nil, nil, "tok-dev"); got != "" {
		t.Fatalf("no envs resolved to %q", got)
	}
	// gero: an exposed env whose token was never minted must not match an
	// empty presented one.
	if got := ResolveEnv(map[string]string{}, []string{"dev"}, ""); got != "" {
		t.Fatalf("an unminted token matched: %q", got)
	}
}

// --- routing ---

func route(t *testing.T, method, path string) Match {
	t.Helper()
	m, err := Route(method, path)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return m
}

func TestValidateIsFamilyFree(t *testing.T) {
	if m := route(t, "GET", "/api/v1/validate"); m != (Match{}) {
		t.Fatalf("%+v", m)
	}
}

func TestMonitorReadsAreTheMonitorsFamily(t *testing.T) {
	for _, p := range []string{"/api/v1/monitor", "/api/v1/monitor/1234", "/api/v1/monitor/search", "/api/v1/downtime"} {
		if m := route(t, "GET", p); m.Family != "monitors" || m.Write {
			t.Errorf("%s: %+v", p, m)
		}
	}
}

func TestAQueryStringDoesNotChangeTheRoute(t *testing.T) {
	if m := route(t, "GET", "/api/v1/monitor?page_size=1"); m.Family != "monitors" {
		t.Fatalf("%+v", m)
	}
}

func TestLogSearchIsScopedThroughTheBody(t *testing.T) {
	if m := route(t, "POST", "/api/v2/logs/events/search"); m != (Match{Family: "logs", Scope: BodyScope}) {
		t.Fatalf("%+v", m)
	}
}

func TestTheGetVariantIsScopedThroughTheParams(t *testing.T) {
	if m := route(t, "GET", "/api/v2/logs/events"); m != (Match{Family: "logs", Scope: ParamsScope}) {
		t.Fatalf("%+v", m)
	}
}

func TestLogPipelinesAndIndexesAreUnscopedLogsReads(t *testing.T) {
	// Configuration, not log content: there is no query to wrap.
	for _, p := range []string{"/api/v1/logs/config/pipelines", "/api/v1/logs/config/pipelines/aBc-12_x",
		"/api/v1/logs/config/pipeline-order", "/api/v1/logs/config/indexes",
		"/api/v1/logs/config/indexes/main", "/api/v1/logs/config/index-order"} {
		if m := route(t, "GET", p); m != (Match{Family: "logs"}) {
			t.Errorf("%s: %+v", p, m)
		}
	}
}

func TestMetricHistoryAndMetadataAreMetrics(t *testing.T) {
	for _, c := range [][2]string{{"GET", "/api/v1/query"}, {"GET", "/api/v1/metrics/system.cpu.user"}, {"POST", "/api/v2/query/timeseries"}} {
		if m := route(t, c[0], c[1]); m.Family != "metrics" {
			t.Errorf("%s: %+v", c[1], m)
		}
	}
}

func TestSpansAreTracesAndDashboardsCoverSLOs(t *testing.T) {
	if route(t, "POST", "/api/v2/spans/events/search").Family != "traces" || route(t, "GET", "/api/v1/slo").Family != "dashboards" {
		t.Fatal("misrouted")
	}
}

func TestAModeledWriteRoutesAsAWriteOfItsFamily(t *testing.T) {
	// Routing names the write; whether it is allowed is the env's business,
	// decided one layer up against that env's write list.
	for _, c := range [][3]string{
		{"POST", "/api/v1/monitor", "monitors"},
		{"PUT", "/api/v1/monitor/1234", "monitors"},
		{"DELETE", "/api/v1/monitor/1234", "monitors"},
		{"POST", "/api/v1/monitor/1234/mute", "monitors"},
		{"POST", "/api/v1/monitor/1234/unmute", "monitors"},
		{"POST", "/api/v1/downtime", "monitors"},
		{"PUT", "/api/v1/downtime/99", "monitors"},
		{"DELETE", "/api/v1/downtime/99", "monitors"},
		{"POST", "/api/v2/downtime", "monitors"},
		{"PATCH", "/api/v2/downtime/abc-def", "monitors"},
		{"DELETE", "/api/v2/downtime/abc-def", "monitors"},
		{"POST", "/api/v1/dashboard", "dashboards"},
		{"PUT", "/api/v1/dashboard/abc-def", "dashboards"},
		{"DELETE", "/api/v1/dashboard/abc-def", "dashboards"},
		{"POST", "/api/v1/slo", "dashboards"},
		{"PUT", "/api/v1/slo/abc", "dashboards"},
		{"DELETE", "/api/v1/slo/abc", "dashboards"},
		{"POST", "/api/v1/events", "events"},
		{"POST", "/api/v1/series", "metrics"},
		{"POST", "/api/v1/distribution_points", "metrics"},
		{"PUT", "/api/v1/metrics/system.cpu.user", "metrics"},
	} {
		if m := route(t, c[0], c[1]); m.Family != c[2] || !m.Write {
			t.Errorf("%s %s: %+v", c[0], c[1], m)
		}
	}
}

func TestMuteAllStaysUnmodeledEvenWhereMonitorWritesExist(t *testing.T) {
	// Silencing the whole org is wider than anything --allow-write monitors
	// promises, so it is off the table rather than behind it.
	_, err := Route("POST", "/api/v1/monitor/mute_all")
	if d := asDenied(t, err); !strings.Contains(d.Reason, "not a modeled write") {
		t.Fatalf("%+v", d)
	}
}

func TestAFamilyWithNoWriteSurfaceSaysSo(t *testing.T) {
	for _, c := range [][2]string{{"POST", "/api/v2/logs/config/pipelines"}, {"POST", "/api/v1/logs/config/pipelines"},
		{"PUT", "/api/v1/logs/config/indexes/main"}, {"DELETE", "/api/v2/spans/events/1"}} {
		_, err := Route(c[0], c[1])
		if d := asDenied(t, err); !strings.Contains(d.Reason, "no write surface") {
			t.Errorf("%s: %+v", c[1], d)
		}
	}
}

func TestAnUnmodeledReadIsRefusedNotForwarded(t *testing.T) {
	_, err := Route("GET", "/api/v2/usage/estimated_cost")
	if d := asDenied(t, err); !strings.Contains(d.Reason, "not a modeled read endpoint") || d.Status != 404 {
		t.Fatalf("%+v", d)
	}
}

func TestATraversalPathIsRefused(t *testing.T) {
	for _, p := range []string{"/api/v1/monitor/../../v2/usage", "/api/v1/monitor/%2e%2e/secret", "/api/v1/./validate"} {
		if _, err := Route("GET", p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

// --- writes ---

func TestAnUnshapedEnvAllowsNoWrites(t *testing.T) {
	// The absence of the key and an explicit empty list mean the same thing.
	for _, raw := range []string{`{}`, `{"families": ["monitors"]}`, `{"writes": null}`} {
		var e Env
		json.Unmarshal([]byte(raw), &e)
		if w := WritesFor(e); len(w) != 0 {
			t.Errorf("%s writes %v", raw, w)
		}
	}
}

func TestTheNamedFamiliesComeBack(t *testing.T) {
	if w := WritesFor(Env{Writes: []string{"dashboards"}}); !reflect.DeepEqual(w, []string{"dashboards"}) {
		t.Fatalf("%v", w)
	}
}

func TestEveryWritableFamilyHasAWriteRoute(t *testing.T) {
	covered := map[string]bool{}
	for _, e := range WriteEndpoints {
		covered[e.family] = true
	}
	var got []string
	for f := range covered {
		got = append(got, f)
	}
	if !reflect.DeepEqual(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(Writable))) {
		t.Fatalf("write routes cover %v, writable is %v", got, Writable)
	}
}

func TestWritableFamiliesAreASubsetOfTheFamilies(t *testing.T) {
	for _, f := range Writable {
		if !slices.Contains(Families, f) {
			t.Errorf("%s is writable but not a family", f)
		}
	}
}

// --- the enforced log scope ---

func TestAQueryIsWrappedNotTrusted(t *testing.T) {
	if got := ScopedQuery("status:error OR service:other", "service:project"); got != "(status:error OR service:other) AND (service:project)" {
		t.Fatalf("%q", got)
	}
}

func TestAnAbsentQueryBecomesTheScopeItself(t *testing.T) {
	for _, q := range []string{"", "*", "  "} {
		if got := ScopedQuery(q, "service:project"); got != "service:project" {
			t.Errorf("%q -> %q", q, got)
		}
	}
}

func TestNoScopeMeansTheQueryPassesUntouched(t *testing.T) {
	if got := ScopedQuery("anything", ""); got != "anything" {
		t.Fatalf("%q", got)
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	return m
}

func TestASearchBodyGetsItsFilterRewritten(t *testing.T) {
	b, err := WrapLogsBody([]byte(`{"filter": {"query": "status:error", "from": "now-15m"}, "page": {"limit": 25}}`), "service:project")
	if err != nil {
		t.Fatal(err)
	}
	f := decode(t, b)["filter"].(map[string]any)
	if f["query"] != "(status:error) AND (service:project)" || f["from"] != "now-15m" {
		t.Fatalf("%s", b)
	}
	// Numbers are kept as written.
	if !strings.Contains(string(b), `"limit":25`) {
		t.Fatalf("%s", b)
	}
}

func TestABodyWithNoFilterStillGainsTheScope(t *testing.T) {
	for _, raw := range []string{`{}`, ``, `{"filter": null}`} {
		b, err := WrapLogsBody([]byte(raw), "service:project")
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if q := decode(t, b)["filter"].(map[string]any)["query"]; q != "service:project" {
			t.Errorf("%q -> %s", raw, b)
		}
	}
}

func TestAnUnparseableBodyIsRefusedNotForwarded(t *testing.T) {
	for _, raw := range []string{`not json`, `[1]`, `{"filter": "x"}`, `{"filter": {"query": 5}}`, `{} {}`} {
		if _, err := WrapLogsBody([]byte(raw), "service:project"); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestTheGetVariantRewritesTheFilterParam(t *testing.T) {
	q, err := WrapLogsParams("filter%5Bquery%5D=status%3Aerror&page%5Blimit%5D=5", "service:project")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := url.ParseQuery(q)
	if !reflect.DeepEqual(v["filter[query]"], []string{"(status:error) AND (service:project)"}) || v.Get("page[limit]") != "5" {
		t.Fatalf("%q", q)
	}
	// The unencoded spelling and repeats are the same parameter to Datadog.
	q, _ = WrapLogsParams("filter[query]=a&filter%5bquery%5d=b", "service:project")
	if v, _ := url.ParseQuery(q); !reflect.DeepEqual(v["filter[query]"], []string{"(b) AND (service:project)"}) {
		t.Fatalf("%q", q)
	}
}

func TestTheGetVariantGainsTheParamWhenAbsent(t *testing.T) {
	q, _ := WrapLogsParams("", "service:project")
	if v, _ := url.ParseQuery(q); v.Get("filter[query]") != "service:project" {
		t.Fatalf("%q", q)
	}
}

// --- the rate budget ---

func TestTheBurstIsHonouredThenRefused(t *testing.T) {
	now := time.Unix(0, 0)
	b := proxy.NewTokenBucket(60, 3, func() time.Time { return now })
	for range 3 {
		if ok, _ := b.Allow(); !ok {
			t.Fatal("the burst was not honoured")
		}
	}
	if ok, retry := b.Allow(); ok || retry <= 0 {
		t.Fatalf("%v %v", ok, retry)
	}
}

func TestTimeRefillsTheBucket(t *testing.T) {
	now := time.Unix(0, 0)
	b := proxy.NewTokenBucket(60, 1, func() time.Time { return now })
	if ok, _ := b.Allow(); !ok {
		t.Fatal("first")
	}
	if ok, _ := b.Allow(); ok {
		t.Fatal("second")
	}
	now = now.Add(time.Second) // 60/min is one per second
	if ok, _ := b.Allow(); !ok {
		t.Fatal("refill")
	}
}

// --- gero: an env as the CLI stores it ---

func normalize(t *testing.T, env map[string]any) ([]string, error) {
	t.Helper()
	return Service{}.Normalize("prod", env)
}

func TestNormalizeFillsTheBoundaryIn(t *testing.T) {
	env := map[string]any{"families": []string{"monitors", "dashboards", "monitors"}}
	warn, err := normalize(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if env["site"] != "datadoghq.com" || !reflect.DeepEqual(env["families"], []string{"dashboards", "monitors"}) ||
		!reflect.DeepEqual(env["writes"], []string{}) || env["logs_query"] != "" || len(warn) != 0 {
		t.Fatalf("%v %v", env, warn)
	}
}

func TestNormalizeRefusesWhatCannotWork(t *testing.T) {
	for want, env := range map[string]map[string]any{
		"not an endpoint family": {"families": []string{"billing"}},
		"no write surface":       {"families": []string{"logs"}, "writes": []string{"logs"}},
		"not a host name":        {"site": "evil.example/x"},
	} {
		if _, err := normalize(t, env); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestAWriteWhoseReadsAreShutIsDroppedWithTheFix(t *testing.T) {
	env := map[string]any{"families": []string{"monitors"}, "writes": []string{"dashboards", "monitors"}}
	warn, err := normalize(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env["writes"], []string{"monitors"}) {
		t.Fatalf("writes = %v", env["writes"])
	}
	if j := strings.Join(warn, "\n"); !strings.Contains(j, "--allow-api dashboards --allow-write dashboards") || !strings.Contains(j, "opens Datadog writes (monitors)") {
		t.Fatalf("%s", j)
	}
}

func TestNormalizeWarnsWhereTheBoundaryIsWide(t *testing.T) {
	warn, _ := normalize(t, map[string]any{"families": []string{"logs"}})
	if j := strings.Join(warn, "\n"); !strings.Contains(j, "log search can read the entire org") {
		t.Fatalf("%s", j)
	}
	warn, _ = normalize(t, map[string]any{})
	if j := strings.Join(warn, "\n"); !strings.Contains(j, "only /api/v1/validate will answer") {
		t.Fatalf("%s", j)
	}
}
