package gitproxy

import (
	"errors"
	"strings"
	"testing"
)

// Ported from isolate's tests/test_ghapi.py: which paths are modelled at
// all, which env a repository routes to, and how a verb and route land in
// a write family. The families are the whole write surface.

func apiStatus(t *testing.T, err error) (int, string) {
	t.Helper()
	var d *APIDenied
	if !errors.As(err, &d) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return d.Status, d.Reason
}

func TestARepoPathYieldsOwnerRepoAndTail(t *testing.T) {
	for path, want := range map[string][3]string{
		"/repos/axis-to/api/pulls": {"axis-to", "api", "pulls"},
		"/repos/axis-to/api":       {"axis-to", "api", ""},
		"/user":                    {"", "", "user"},
		"/rate_limit":              {"", "", "rate_limit"},
	} {
		o, r, tail, err := ParseAPITarget(path)
		if err != nil || [3]string{o, r, tail} != want {
			t.Errorf("%s: %s %s %s %v", path, o, r, tail, err)
		}
	}
}

func TestEverythingElseIsNotModeled(t *testing.T) {
	for _, p := range []string{"/graphql", "/search/issues", "/orgs/axis-to", "/orgs/axis-to/members", "/notifications"} {
		_, _, _, err := ParseAPITarget(p)
		if code, _ := apiStatus(t, err); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestAPITraversalAndEncodingAreRefused(t *testing.T) {
	for _, p := range []string{"/repos/../user", "/repos/a/%2e%2e/pulls", "/repos/a\\b/c"} {
		if _, _, _, err := ParseAPITarget(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

var resolveEnvs = map[string]Env{"work": {Repos: []string{"axis-to/api", "axis-to/web"}}, "oss": {Repos: []string{"someone/tool"}}}

func TestTheListingEnvIsTheRoute(t *testing.T) {
	if env, _ := ResolveRepo(resolveEnvs, "axis-to", "api"); env != "work" {
		t.Fatal(env)
	}
	if env, _ := ResolveRepo(resolveEnvs, "Axis-To", "API"); env != "work" {
		t.Fatal("case-sensitive match", env)
	}
	if env, _ := ResolveRepo(resolveEnvs, "someone", "tool"); env != "oss" {
		t.Fatal(env)
	}
}

func TestAnUnlistedRepoNamesTheFixAndThePublicEscape(t *testing.T) {
	_, err := ResolveRepo(resolveEnvs, "axis-to", "secret")
	code, r := apiStatus(t, err)
	if code != 404 || !strings.Contains(r, "--allow-repo axis-to/secret") || !strings.Contains(r, "api.github.com") {
		t.Fatal(code, r)
	}
}

func TestAnOverlapIsRefusedNotResolved(t *testing.T) {
	_, err := ResolveRepo(map[string]Env{"a": {Repos: []string{"o/r"}}, "b": {Repos: []string{"o/r"}}}, "o", "r")
	if _, r := apiStatus(t, err); !strings.Contains(r, "a, b") {
		t.Fatal(r)
	}
}

func TestWritesForMissingMeansTheDefaultAndEmptyMeansNone(t *testing.T) {
	if got := strings.Join(WritesFor(Env{}), ","); got != "comments,pulls,reviews" {
		t.Fatal(got)
	}
	empty := []string{}
	if got := WritesFor(Env{APIWrites: &empty}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestReadsAreOpenWhateverTheFamiliesSay(t *testing.T) {
	none := []string{}
	for _, tail := range []string{"pulls", "issues/3", "commits", "contents/README.md", "actions/runs"} {
		if err := Allowed(Env{APIWrites: &none}, "work", "GET", tail); err != nil {
			t.Error(tail, err)
		}
	}
}

func TestTheDefaultFamiliesCoverThePRWorkflow(t *testing.T) {
	for _, c := range [][2]string{
		{"POST", "pulls"}, {"PATCH", "pulls/12"}, {"POST", "pulls/12/requested_reviewers"},
		{"POST", "pulls/12/reviews"}, {"POST", "issues/12/comments"}, {"PATCH", "issues/comments/9"},
	} {
		if err := Allowed(Env{}, "work", c[0], c[1]); err != nil {
			t.Error(c, err)
		}
	}
}

func TestADisabledFamilyNamesItselfAndTheFix(t *testing.T) {
	_, r := apiStatus(t, Allowed(Env{}, "work", "PUT", "pulls/12/merge"))
	if !strings.Contains(r, `"merge"`) || !strings.Contains(r, "psps env edit git work --api-write merge") {
		t.Fatal(r)
	}
	merge := []string{"merge"}
	if err := Allowed(Env{APIWrites: &merge}, "work", "PUT", "pulls/12/merge"); err != nil {
		t.Fatal(err)
	}
}

func TestAnUnmodeledWriteIsRefusedWithTheCatalog(t *testing.T) {
	for _, c := range [][2]string{{"DELETE", "issues/comments/9"}, {"POST", "hooks"}, {"PUT", "contents/README.md"}, {"PATCH", ""}} {
		_, r := apiStatus(t, Allowed(Env{}, "work", c[0], c[1]))
		if !strings.Contains(r, "not a modeled write") {
			t.Error(c, r)
		}
	}
}

func TestARoutePrefixDoesNotSmuggleAWrite(t *testing.T) {
	if Allowed(Env{}, "work", "POST", "pulls/12/merge/confirm") == nil {
		t.Fatal("a longer route matched a family by prefix")
	}
}

func TestAReadOnlyEnvRefusesEveryWrite(t *testing.T) {
	pulls := []string{"pulls"}
	_, r := apiStatus(t, Allowed(Env{ReadOnly: true, APIWrites: &pulls}, "work", "POST", "pulls"))
	if !strings.Contains(r, "read-only") {
		t.Fatal(r)
	}
}

func TestGuardQuery(t *testing.T) {
	if got := GuardQuery("state=open&access_token=x"); got != "state=open" {
		t.Fatal(got)
	}
	if got := GuardQuery("state=open&per_page=5"); got != "state=open&per_page=5" {
		t.Fatal(got)
	}
	if got := GuardQuery("ACCESS%5FTOKEN=x&q=a+b"); got != "q=a+b" {
		t.Fatal(got)
	}
}

func TestGlobalReadsRouteToTheFirstEnv(t *testing.T) {
	if env, _ := DefaultEnvName(map[string]Env{"work": {}, "aux": {}}); env != "aux" {
		t.Fatal(env)
	}
	_, err := DefaultEnvName(nil)
	if code, r := apiStatus(t, err); code != 503 || !strings.Contains(r, "psps iso expose") {
		t.Fatal(code, r)
	}
}

func TestParseScopes(t *testing.T) {
	got := ParseScopes("gist, read:org, repo, workflow", true)
	if len(got) != 4 || !got["workflow"] {
		t.Fatal(got)
	}
	if got := ParseScopes("  ", true); got == nil || len(got) != 0 {
		t.Fatal("an empty header is a token with no scopes")
	}
	if ParseScopes("", false) != nil {
		t.Fatal("a missing header means scopes do not apply")
	}
}
