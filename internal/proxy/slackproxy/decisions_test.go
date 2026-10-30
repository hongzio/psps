package slackproxy

import (
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/creds"
)

// Ported from isolate's test_slackproxy.py: every boundary the isolation
// cannot cross, pinned before the wire. Which Web API methods are modeled at
// all (default-deny: Slack does not split reads from writes by verb, so the
// table is the only thing "read-only" can mean), which env a presented token
// names, and where the channel deny list bites.

func mustDeny(t *testing.T, d *Denied, want ...string) *Denied {
	t.Helper()
	if d == nil {
		t.Fatal("accepted")
	}
	for _, w := range want {
		if !strings.Contains(d.Reason, w) {
			t.Fatalf("%q does not mention %q", d.Reason, w)
		}
	}
	return d
}

func families(f ...string) *[]string { return &f }

// --- the credential ---

func TestTheStoreYieldsItsToken(t *testing.T) {
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("slack", "prod", map[string]string{"token": "xoxp-real"})
	if got, err := cs.Read("slack", "prod"); err != nil || got["token"] != "xoxp-real" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestAMissingSectionNamesTheFix(t *testing.T) {
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	_, err := cs.Read("slack", "prod")
	if err == nil || !strings.Contains(err.Error(), "[slack prod]") {
		t.Fatalf("%v", err)
	}
}

// --- which env a token names ---

func TestEachTokenNamesItsEnv(t *testing.T) {
	toks := map[string]string{"dev": "tok-dev", "prod": "tok-prod"}
	envs := []string{"dev", "prod"}
	if got := ResolveEnv(toks, envs, "tok-prod"); got != "prod" {
		t.Fatalf("%q", got)
	}
	for _, bad := range []string{"nope", ""} {
		if got := ResolveEnv(toks, envs, bad); got != "" {
			t.Errorf("%q resolved to %q", bad, got)
		}
	}
	if got := ResolveEnv(nil, nil, "tok-dev"); got != "" {
		t.Fatalf("%q", got)
	}
	// An env with no token minted never matches an empty one.
	if got := ResolveEnv(map[string]string{}, []string{"dev"}, ""); got != "" {
		t.Fatalf("%q", got)
	}
}

// --- routing ---

func TestTheModeledMethodsRouteByGetAndPost(t *testing.T) {
	for _, verb := range []string{"GET", "POST"} {
		for _, m := range []string{"auth.test", "slackLists.items.list", "slackLists.items.info"} {
			if got, d := Route(verb, "/api/"+m); d != nil || got != m {
				t.Errorf("%s %s = %q, %v", verb, m, got, d)
			}
		}
	}
}

func TestATrailingSlashDoesNotChangeTheRoute(t *testing.T) {
	// The query string is split off before Route (proxy.RawTarget); a
	// trailing slash is the path-side equivalent.
	if got, d := Route("GET", "/api/slackLists.items.list/"); d != nil || got != "slackLists.items.list" {
		t.Fatalf("%q %v", got, d)
	}
}

func TestAnUnmodeledMethodIsRefusedAndNamesTheSurface(t *testing.T) {
	_, d := Route("POST", "/api/chat.postMessage")
	mustDeny(t, d, "chat.postMessage", "slackLists.items.list")
}

func TestAListsWriteIsRefusedLikeAnyOtherMethod(t *testing.T) {
	for _, m := range []string{"slackLists.items.create", "slackLists.items.delete", "slackLists.items.update"} {
		if _, d := Route("POST", "/api/"+m); d == nil {
			t.Errorf("%s accepted", m)
		}
	}
}

func TestAVerbOtherThanGetOrPostIsRefused(t *testing.T) {
	for _, verb := range []string{"PUT", "PATCH", "DELETE"} {
		if _, d := Route(verb, "/api/auth.test"); d == nil {
			t.Errorf("%s accepted", verb)
		}
	}
}

func TestAPathOutsideTheAPIPrefixIsRefused(t *testing.T) {
	for _, p := range []string{"/auth.test", "/files/T1/F1/download", "/"} {
		if _, d := Route("GET", p); d == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestATraversalPathIsRefused(t *testing.T) {
	for _, p := range []string{"/api/../admin", "/api/%2e%2e/admin", "/api/./auth.test"} {
		if _, d := Route("GET", p); d == nil || d.Status != 400 {
			t.Errorf("%s: %v", p, d)
		}
	}
}

// --- the family boundary ---

func TestMissingFamiliesMeanTheOriginalListsSurface(t *testing.T) {
	if got := FamiliesFor(Env{}); !reflect.DeepEqual(got, []string{"lists"}) {
		t.Fatalf("%v", got)
	}
	if got := FamiliesFor(Env{Families: families()}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestAuthTestAnswersWhateverTheBoundarySays(t *testing.T) {
	if d := Allowed(Env{Families: families()}, "dev", "auth.test"); d != nil {
		t.Fatal(d)
	}
}

func TestEachFamilyOpensExactlyItsMethods(t *testing.T) {
	e := Env{Families: families("messages")}
	if d := Allowed(e, "dev", "conversations.history"); d != nil {
		t.Fatal(d)
	}
	// --allow-api replaces the list, so the fix keeps what the env has.
	mustDeny(t, Allowed(e, "dev", "slackLists.items.list"), `"lists"`, "gero env edit slack dev --allow-api lists --allow-api messages")
}

func TestTheUsersFamilyIsItsOwnChoice(t *testing.T) {
	mustDeny(t, Allowed(Env{Families: families("messages")}, "dev", "users.list"), `"users"`, "--allow-api messages --allow-api users")
	e := Env{Families: families("users")}
	for _, m := range []string{"users.list", "users.info", "users.lookupByEmail", "users.profile.get"} {
		if got, d := Route("GET", "/api/"+m); d != nil || got != m {
			t.Errorf("%s = %q, %v", m, got, d)
		}
		if d := Allowed(e, "dev", m); d != nil {
			t.Errorf("%s: %v", m, d)
		}
	}
	if _, d := Route("POST", "/api/users.profile.set"); d == nil {
		t.Fatal("a profile write routed")
	}
	if ReadsChannels([]string{"users"}) {
		t.Fatal("users is channel clamped")
	}
}

func TestAClosedMessagesFamilyNamesTheFix(t *testing.T) {
	mustDeny(t, Allowed(Env{}, "prod", "conversations.replies"), "--allow-api messages")
}

func TestSearchOpensOnlyWhenNamed(t *testing.T) {
	if d := Allowed(Env{Families: families("search")}, "dev", "search.messages"); d != nil {
		t.Fatal(d)
	}
	mustDeny(t, Allowed(Env{}, "prod", "search.messages"), "--allow-api search")
}

// --- the channel clamp: default-open, the deny list carves exceptions ---

var clampEnv = Env{Families: families("messages"), DenyChannels: []string{"C0666"}}

func TestAnUndeniedChannelPassesWhereverItIsNamed(t *testing.T) {
	for _, c := range []struct {
		q, body, ctype string
	}{
		{"channel=C0123", "", ""},
		{"", "channel=C0123", "application/x-www-form-urlencoded"},
		{"", `{"channel": "C0123"}`, "application/json"},
		{"", `{"channel": "C0123"}`, "application/json; charset=utf-8"},
	} {
		if d := GuardChannel(clampEnv, "dev", "conversations.history", c.q, []byte(c.body), c.ctype); d != nil {
			t.Errorf("%+v: %v", c, d)
		}
	}
}

func TestADeniedChannelIsRefusedAndSaysWhoseDecision(t *testing.T) {
	mustDeny(t, GuardChannel(clampEnv, "dev", "conversations.history", "channel=C0666", nil, ""), "C0666", "--deny-channel")
}

func TestADeniedChannelCannotRideShotgunInTheBody(t *testing.T) {
	// ?channel=innocent with a body naming the denied one: every occurrence
	// is checked, because Slack's precedence is its own.
	for _, c := range []struct{ body, ctype string }{
		{"channel=C0666", "application/x-www-form-urlencoded"},
		{"channel=C0666", ""},
		{"chan%6Eel=C0666", "application/x-www-form-urlencoded"},
		{`{"channel": "C0666"}`, "application/json"},
	} {
		if d := GuardChannel(clampEnv, "dev", "conversations.history", "channel=C0123", []byte(c.body), c.ctype); d == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestAnEmptyDenyListForwardsWithoutParsing(t *testing.T) {
	e := Env{Families: families("messages"), DenyChannels: []string{}}
	if d := GuardChannel(e, "dev", "conversations.history", "", nil, ""); d != nil {
		t.Fatal(d)
	}
	if d := GuardChannel(e, "dev", "conversations.history", "channel=C0666", []byte("{not json"), "application/json"); d != nil {
		t.Fatal(d)
	}
}

func TestListsMethodsAreNeverChannelClamped(t *testing.T) {
	if d := GuardChannel(clampEnv, "dev", "slackLists.items.list", "channel=C0666", nil, ""); d != nil {
		t.Fatal(d)
	}
}

func TestAnUnparseableJSONBodyIsRefusedNotWavedThrough(t *testing.T) {
	if d := GuardChannel(clampEnv, "dev", "conversations.history", "channel=C0123", []byte("{not json"), "application/json"); d == nil {
		t.Fatal("accepted")
	}
}

func TestABodyTheClampCannotReadIsRefused(t *testing.T) {
	// Beyond isolate: a multipart or text body might carry a channel Slack
	// reads; one the clamp cannot parse is refused while the deny list is
	// live, and a non-string JSON channel likewise.
	for _, c := range []struct{ body, ctype string }{
		{"--x\r\nContent-Disposition: form-data; name=\"channel\"\r\n\r\nC0666\r\n--x--", "multipart/form-data; boundary=x"},
		{"channel=C0666", "text/plain"},
		{`{"channel": ["C0666"]}`, "application/json"},
		{"channel=%zz", "application/x-www-form-urlencoded"},
	} {
		if d := GuardChannel(clampEnv, "dev", "conversations.history", "", []byte(c.body), c.ctype); d == nil || d.Status != 400 {
			t.Errorf("%+v: %v", c, d)
		}
	}
}

// --- files ---

func TestAFilePathRoutesToTheDownload(t *testing.T) {
	if got, d := Route("GET", "/files/F0123ABC"); d != nil || got != Download {
		t.Fatalf("%q %v", got, d)
	}
	if id, d := FileIDOf("/files/F0123ABC"); d != nil || id != "F0123ABC" {
		t.Fatalf("%q %v", id, d)
	}
}

func TestADownloadIsGetOnly(t *testing.T) {
	if _, d := Route("POST", "/files/F0123ABC"); d == nil {
		t.Fatal("accepted")
	}
}

func TestOnlyAFileIDNamesADownload(t *testing.T) {
	for _, p := range []string{"/files/", "/files/C0123", "/files/f0123", "/files/F0123/extra", "/files/F01-23"} {
		if _, d := Route("GET", p); d == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestTheSyntheticDownloadIsNotAWebAPIMethod(t *testing.T) {
	d := mustDeny(t, func() *Denied { _, d := Route("GET", "/api/"+Download); return d }())
	_, surface, _ := strings.Cut(d.Reason, "exactly:")
	if strings.Contains(surface, Download) || !strings.Contains(surface, "files.info") {
		t.Fatalf("surface = %q", surface)
	}
}

func TestFilesOpensOnlyWhenNamed(t *testing.T) {
	e := Env{Families: families("files")}
	for _, m := range []string{"files.info", Download} {
		if d := Allowed(e, "dev", m); d != nil {
			t.Fatal(d)
		}
		mustDeny(t, Allowed(Env{Families: families("messages")}, "dev", m), "--allow-api files")
	}
}

var filesEnv = Env{Families: families("files"), DenyChannels: []string{"C0666"}}

func TestAFileSharedOnlyInOpenChannelsPasses(t *testing.T) {
	f := map[string]any{"id": "F1", "channels": []any{"C0123"}, "shares": map[string]any{"public": map[string]any{"C0123": []any{map[string]any{}}}}}
	if d := GuardFile(filesEnv, "dev", f); d != nil {
		t.Fatal(d)
	}
}

func TestAFileSharedAnywhereDeniedIsRefused(t *testing.T) {
	share := map[string]any{"C0666": []any{map[string]any{}}}
	for _, f := range []map[string]any{
		{"id": "F1", "channels": []any{"C0123", "C0666"}},
		{"id": "F1", "groups": []any{"C0666"}},
		{"id": "F1", "ims": []any{"C0666"}},
		{"id": "F1", "shares": map[string]any{"public": share}},
		{"id": "F1", "shares": map[string]any{"private": share}},
	} {
		mustDeny(t, GuardFile(filesEnv, "dev", f), "C0666", "--deny-channel")
	}
}

func TestAnUnsharedFilePassesBecauseTheListCarvesExceptions(t *testing.T) {
	if d := GuardFile(filesEnv, "dev", map[string]any{"id": "F1"}); d != nil {
		t.Fatal(d)
	}
}

func TestTheTokenNeverGoesToAnotherHost(t *testing.T) {
	base, _ := url.Parse("https://files.slack.com")
	ok := map[string]any{"id": "F1", "url_private_download": "https://files.slack.com/files-pri/T1-F1/download/a.png?t=1"}
	if p, d := DownloadTarget(ok, base); d != nil || p != "/files-pri/T1-F1/download/a.png?t=1" {
		t.Fatalf("%q %v", p, d)
	}
	for _, u := range []string{
		"https://evil.example/x.png",
		"http://files.slack.com/x.png",
		"https://files.slack.com.evil.example/x.png",
		"https://user@files.slack.com/x.png",
		"https://files.slack.com:8443/x.png",
		"",
	} {
		if _, d := DownloadTarget(map[string]any{"id": "F1", "url_private": u}, base); d == nil || d.Status != 502 {
			t.Errorf("%q: %v", u, d)
		}
	}
}

// --- search: the deny list rides into the query ---

type fakeNames map[string]string

func (f fakeNames) Get(_, _, id string) (string, *Denied) {
	if n, ok := f[id]; ok {
		return n, nil
	}
	return "", deny(502, "slack: denied channel %s did not resolve", id)
}

var (
	searchEnv   = Env{Families: families("search"), DenyChannels: []string{"C0666", "C0777"}}
	searchNames = fakeNames{"C0666": "secrets", "C0777": "board"}
)

func queryValue(t *testing.T, encoded string) string {
	t.Helper()
	v, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return v.Get("query")
}

func TestTheDenyListRidesIntoTheQueryAsExclusions(t *testing.T) {
	q, _, d := GuardSearch(searchEnv, "dev", "query=deploy+failure", nil, "", "xoxp-real", searchNames)
	if d != nil {
		t.Fatal(d)
	}
	// Exclusions ride in deny-list (id) order: C0666 -> secrets first.
	if got := queryValue(t, q); got != "deploy failure -in:#secrets -in:#board" {
		t.Fatalf("%q", got)
	}
}

func TestAFormBodyQueryIsRewrittenToo(t *testing.T) {
	_, body, d := GuardSearch(searchEnv, "dev", "", []byte("query=deploy"), "application/x-www-form-urlencoded", "xoxp-real", searchNames)
	if d != nil {
		t.Fatal(d)
	}
	if got := queryValue(t, string(body)); !strings.Contains(got, "-in:#secrets") || !strings.Contains(got, "-in:#board") {
		t.Fatalf("%q", got)
	}
}

func TestACallersOwnInQualifierCannotBeatTheExclusion(t *testing.T) {
	// Qualifiers are ANDed: in:#secrets AND -in:#secrets matches nothing,
	// so the contradiction fails closed.
	q, _, _ := GuardSearch(searchEnv, "dev", "query=x+in:%23secrets", nil, "", "xoxp-real", searchNames)
	if got := queryValue(t, q); !strings.Contains(got, "in:#secrets") || !strings.Contains(got, "-in:#secrets") {
		t.Fatalf("%q", got)
	}
}

func TestAnEncodedQueryKeyIsRewrittenToo(t *testing.T) {
	q, _, _ := GuardSearch(searchEnv, "dev", "qu%65ry=x", nil, "", "xoxp-real", searchNames)
	if v, _ := url.ParseQuery(q); !strings.Contains(v.Get("query"), "-in:#secrets") {
		t.Fatalf("%q", q)
	}
}

func TestAnEmptyDenyListChangesNothing(t *testing.T) {
	e := Env{Families: families("search"), DenyChannels: []string{}}
	q, body, d := GuardSearch(e, "dev", "query=x", []byte("{anything}"), "application/json", "tok", searchNames)
	if d != nil || q != "query=x" || string(body) != "{anything}" {
		t.Fatalf("%q %q %v", q, body, d)
	}
}

func TestAJSONBodyIsRefusedWhenTheDenyListIsLive(t *testing.T) {
	_, _, d := GuardSearch(searchEnv, "dev", "query=x", []byte(`{"query": "x"}`), "application/json", "xoxp-real", searchNames)
	if d == nil || d.Status != 400 {
		t.Fatalf("%v", d)
	}
}

func TestAnUnresolvableDeniedChannelFailsClosed(t *testing.T) {
	e := Env{Families: families("search"), DenyChannels: []string{"C0000"}}
	if _, _, d := GuardSearch(e, "dev", "query=x", nil, "", "tok", searchNames); d == nil {
		t.Fatal("accepted")
	}
}

// --- the legacy token parameter ---

func TestATokenParamIsStripped(t *testing.T) {
	for in, want := range map[string]string{
		"token=X&list_id=F1":   "list_id=F1",
		"list_id=F1&tok%65n=X": "list_id=F1",
		"list_id=F1&limit=5":   "list_id=F1&limit=5",
	} {
		if got, d := StripCredentials(in); d != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, d)
		}
	}
}

// --- env definitions ---

func TestNormalizeMakesTheBoundaryExplicit(t *testing.T) {
	env := map[string]any{}
	warn, err := Service{}.Normalize("dev", env)
	if err != nil || len(warn) != 0 {
		t.Fatalf("%v %v", warn, err)
	}
	if !reflect.DeepEqual(env["families"], []string{"lists"}) || !reflect.DeepEqual(env["deny_channels"], []string{}) {
		t.Fatalf("%v", env)
	}
	// An explicit empty list stays empty: auth.test only.
	env = map[string]any{"families": []string{}}
	Service{}.Normalize("dev", env)
	if !reflect.DeepEqual(env["families"], []string{}) {
		t.Fatalf("%v", env)
	}
}

func TestNormalizeRefusesWhatCannotWork(t *testing.T) {
	for want, env := range map[string]map[string]any{
		"not a slack method family": {"families": []string{"chat"}},
		"not names: general":        {"deny_channels": []string{"general"}},
	} {
		if _, err := (Service{}).Normalize("dev", env); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestOpeningChannelReadsWithoutADenyListIsWarned(t *testing.T) {
	warn, err := Service{}.Normalize("dev", map[string]any{"families": []string{"messages", "lists"}})
	if err != nil || len(warn) != 1 || !strings.Contains(warn[0], "gero env edit slack dev --deny-channel") {
		t.Fatalf("%v %v", warn, err)
	}
	warn, _ = Service{}.Normalize("dev", map[string]any{"families": []string{"messages"}, "deny_channels": []string{"C0666"}})
	if len(warn) != 0 {
		t.Fatalf("%v", warn)
	}
}
