package gitproxy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/proxy"
)

// Ported from isolate's tests/test_gitproxy.py. The repository allowlist is
// the only thing narrowing a token with the `repo` scope, so this parser
// deciding a different repository than GitHub would is not a bug in a URL
// helper — it is the whole restriction failing open.

func denied(t *testing.T, err error) string {
	t.Helper()
	var d *proxy.Denied
	if !errors.As(err, &d) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return d.Reason
}

func TestAcceptsTheThreeSmartHTTPEndpoints(t *testing.T) {
	for path, want := range map[string]string{
		"/octocat/Hello-World.git/info/refs":        "info/refs",
		"/octocat/Hello-World.git/git-upload-pack":  "git-upload-pack",
		"/octocat/Hello-World.git/git-receive-pack": "git-receive-pack",
	} {
		owner, repo, svc, err := ParseTarget(path)
		if err != nil || owner != "octocat" || repo != "Hello-World" || svc != want {
			t.Errorf("%s: %s %s %s %v", path, owner, repo, svc, err)
		}
	}
}

func TestGitSuffixIsOptional(t *testing.T) {
	if owner, repo, _, err := ParseTarget("/octocat/Hello-World/info/refs"); err != nil || owner != "octocat" || repo != "Hello-World" {
		t.Fatal(owner, repo, err)
	}
}

func TestRejectsTheDumbProtocol(t *testing.T) {
	for _, p := range []string{"/o/r.git/objects/info/packs", "/o/r.git/HEAD", "/o/r/objects/ab/cdef"} {
		if _, _, _, err := ParseTarget(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestRejectsTraversalAndEncoding(t *testing.T) {
	for _, p := range []string{
		"/octocat/../other/info/refs",
		"/octocat/%2e%2e/other/info/refs",
		"/octocat/He%2Fllo/info/refs",
		"/octocat/Hello\\World/info/refs",
		"/../../etc/info/refs",
	} {
		if _, _, _, err := ParseTarget(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestRejectsAShortPath(t *testing.T) {
	for _, p := range []string{"/", "/owner", "/owner/repo"} {
		if _, _, _, err := ParseTarget(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestInfoRefsAnnouncesAPushBeforeThePackArrives(t *testing.T) {
	if got := RequestedService("service=git-receive-pack", "info/refs"); got != "git-receive-pack" {
		t.Fatal(got)
	}
	if got := RequestedService("service=git-upload-pack", "info/refs"); got != "git-upload-pack" {
		t.Fatal(got)
	}
	if got := RequestedService("", "git-receive-pack"); got != "git-receive-pack" {
		t.Fatal(got)
	}
}

func TestOnlyReceivePackCountsAsAWrite(t *testing.T) {
	if !IsWrite("git-receive-pack") || IsWrite("git-upload-pack") || IsWrite("info/refs") {
		t.Fatal("write classification is wrong")
	}
}

func oneEnv(readOnly bool, repos ...string) map[string]Env {
	return map[string]Env{"main": {Repos: repos, ReadOnly: readOnly}}
}

func check(t *testing.T, p *Policy, owner, repo, svc string) (string, string) {
	t.Helper()
	d, env, err := p.Check(owner, repo, svc)
	if err != nil {
		t.Fatalf("%s/%s %s refused: %v", owner, repo, svc, err)
	}
	return d, env
}

func TestDeniesEverythingWhenNoEnvListsARepo(t *testing.T) {
	_, _, err := NewPolicy(nil, nil).Check("octocat", "Hello-World", "git-upload-pack")
	if r := denied(t, err); !strings.Contains(r, "gero env add git") {
		t.Fatal(r)
	}
}

func TestAListedRepositoryRoutesToItsEnv(t *testing.T) {
	if d, env := check(t, NewPolicy(oneEnv(false, "octocat/Hello-World"), nil), "octocat", "Hello-World", "git-upload-pack"); d != Authenticated || env != "main" {
		t.Fatal(d, env)
	}
}

func TestDeniesAnUnlistedRepository(t *testing.T) {
	if _, _, err := NewPolicy(oneEnv(false, "octocat/Hello-World"), nil).Check("octocat", "Other-Repo", "git-upload-pack"); err == nil {
		t.Fatal("unlisted repository allowed")
	}
}

func TestMatchingIsCaseInsensitiveAndGitSuffixAgnostic(t *testing.T) {
	// GitHub treats these as one repository; disagreeing would let a cased
	// variant slip past the allowlist.
	if d, _ := check(t, NewPolicy(oneEnv(false, "OctoCat/Hello-World.git"), nil), "octocat", "hello-world", "git-upload-pack"); d != Authenticated {
		t.Fatal(d)
	}
}

func TestReadOnlyIsPerEnv(t *testing.T) {
	p := NewPolicy(map[string]Env{"work": {Repos: []string{"a/b"}}, "oss": {Repos: []string{"python/cpython"}, ReadOnly: true}}, nil)
	if _, env := check(t, p, "a", "b", "git-receive-pack"); env != "work" {
		t.Fatal(env)
	}
	_, _, err := p.Check("python", "cpython", "git-receive-pack")
	if r := denied(t, err); !strings.Contains(r, "read-only") {
		t.Fatal(r)
	}
}

func TestAnOverlappingRepoIsRefusedNamingBothEnvs(t *testing.T) {
	p := NewPolicy(map[string]Env{"a": {Repos: []string{"x/y"}}, "b": {Repos: []string{"x/y"}}}, nil)
	_, _, err := p.Check("x", "y", "git-upload-pack")
	if r := denied(t, err); !strings.Contains(r, "a, b") {
		t.Fatal(r)
	}
}

func TestTokenSourceCachesWithinTheTTL(t *testing.T) {
	src := NewTokenSource([]string{"printf", "tok"}, time.Minute)
	clock := time.Unix(0, 0)
	src.now = func() time.Time { return clock }
	if v, err := src.Get(); err != nil || v != "tok" {
		t.Fatal(v, err)
	}
	first := src.fetched
	clock = clock.Add(10 * time.Second)
	src.Get()
	if src.fetched != first {
		t.Fatal("refetched inside the TTL")
	}
	clock = clock.Add(time.Hour)
	src.Get()
	if src.fetched == first {
		t.Fatal("not refetched after the TTL")
	}
}

func TestAFailingCommandIsDeniedNotCrashed(t *testing.T) {
	if _, err := NewTokenSource([]string{"false"}, time.Second).Get(); err == nil {
		t.Fatal("a failing command yielded a token")
	}
	if _, err := NewTokenSource([]string{"/nonexistent/gh"}, time.Second).Get(); !strings.Contains(denied(t, err), "not runnable") {
		t.Fatal(err)
	}
}

type fakePublic struct {
	public map[string]bool
	asked  []string
}

func newFakePublic(repos ...string) *fakePublic {
	f := &fakePublic{public: map[string]bool{}}
	for _, r := range repos {
		f.public[strings.ToLower(r)] = true
	}
	return f
}

func (f *fakePublic) Check(owner, repo string) bool {
	key := strings.ToLower(owner + "/" + repo)
	f.asked = append(f.asked, key)
	return f.public[key]
}

// The allowlist rations a credential; a public repository needs none.

func publicPolicy(readOnly bool, allowed ...string) *Policy {
	var envs map[string]Env
	if len(allowed) > 0 {
		envs = oneEnv(readOnly, allowed...)
	}
	return NewPolicy(envs, newFakePublic("pub/repo"))
}

func TestPublicRepositoryIsForwardedWithoutTheCredential(t *testing.T) {
	if d, env := check(t, publicPolicy(false), "pub", "repo", "git-upload-pack"); d != Anonymous || env != "" {
		t.Fatal(d, env)
	}
}

func TestPublicStaysReadOnlyBecausePushingNeedsACredential(t *testing.T) {
	_, _, err := publicPolicy(false).Check("pub", "repo", "git-receive-pack")
	if r := denied(t, err); !strings.Contains(r, "pushing needs one") {
		t.Fatal(r)
	}
}

func TestAPushIsRefusedAtDiscoveryNotAfterThePack(t *testing.T) {
	_, _, svc, _ := ParseTarget("/pub/repo.git/info/refs")
	if _, _, err := publicPolicy(false).Check("pub", "repo", RequestedService("service=git-receive-pack", svc)); err == nil {
		t.Fatal("push allowed at discovery")
	}
}

func TestAllowlistingAPublicRepositoryStillBuysPush(t *testing.T) {
	if d, env := check(t, publicPolicy(false, "pub/repo"), "pub", "repo", "git-receive-pack"); d != Authenticated || env != "main" {
		t.Fatal(d, env)
	}
}

func TestPrivateRepositoriesAreUnaffected(t *testing.T) {
	_, _, err := publicPolicy(false).Check("priv", "repo", "git-upload-pack")
	if r := denied(t, err); !strings.Contains(r, "not public") {
		t.Fatal(r)
	}
}

func TestNoPublicReadsRestoresTheStrictAllowlist(t *testing.T) {
	strict := NewPolicy(oneEnv(false, "o/r"), nil)
	if d, _ := check(t, strict, "o", "r", "git-upload-pack"); d != Authenticated {
		t.Fatal(d)
	}
	if _, _, err := strict.Check("pub", "repo", "git-upload-pack"); err == nil {
		t.Fatal("a public repository passed the strict allowlist")
	}
}

func TestTheAllowlistIsCheckedBeforeTheNetwork(t *testing.T) {
	fake := newFakePublic()
	NewPolicy(oneEnv(false, "o/r"), fake).Check("o", "r", "git-upload-pack")
	if len(fake.asked) != 0 {
		t.Fatalf("the network was asked: %v", fake.asked)
	}
}

func counting(answer bool, failure error) (*PublicRepos, *int, *[]string) {
	calls := 0
	var logged []string
	clock := time.Unix(0, 0)
	pr := &PublicRepos{TTL: 100 * time.Second, Attempts: 2, Log: func(m string) { logged = append(logged, m) }}
	pr.now = func() time.Time { return clock }
	pr.Head = func(string) (int, string, error) {
		calls++
		if failure != nil {
			return 0, "", failure
		}
		if answer {
			return 200, "", nil
		}
		return 404, "", nil
	}
	pr.GetStatus = func(string) (int, error) { return 401, nil }
	return pr, &calls, &logged
}

func TestOneProbeServesAWholeClone(t *testing.T) {
	pr, calls, _ := counting(true, nil)
	for range 5 {
		if !pr.Check("o", "r") {
			t.Fatal("not public")
		}
	}
	if *calls != 1 {
		t.Fatalf("%d probes", *calls)
	}
}

func TestTheAnswerIsRecheckedAfterTheTTL(t *testing.T) {
	pr, calls, _ := counting(true, nil)
	pr.Check("o", "r")
	base := pr.now()
	pr.now = func() time.Time { return base.Add(101 * time.Second) }
	pr.Check("o", "r")
	if *calls != 2 {
		t.Fatalf("%d probes", *calls)
	}
}

func TestANetworkFailureDeniesAndIsNotCached(t *testing.T) {
	// A timeout is not evidence about visibility.
	pr, calls, _ := counting(false, errors.New("timeout"))
	if pr.Check("o", "r") || pr.Check("o", "r") {
		t.Fatal("a failure read as public")
	}
	if *calls != 4 { // two checks, each retried once
		t.Fatalf("%d probes", *calls)
	}
}

func TestASingleTransientFailureDoesNotDeny(t *testing.T) {
	answers := []error{errors.New("timeout"), nil}
	pr := &PublicRepos{TTL: time.Minute, Attempts: 2}
	pr.Head = func(string) (int, string, error) {
		err := answers[0]
		answers = answers[1:]
		if err != nil {
			return 0, "", err
		}
		return 200, "", nil
	}
	if !pr.Check("o", "r") {
		t.Fatal("one slow probe denied a public repository")
	}
}

func TestAFailedProbeSaysWhy(t *testing.T) {
	pr, _, logged := counting(false, errors.New("timeout"))
	pr.Check("o", "r")
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "o/r") || !strings.Contains((*logged)[0], "timeout") {
		t.Fatalf("logged %v", *logged)
	}
}

// The classifier, with the network replaced by a script of responses.

type scripted struct {
	pages, gits []string
}

func probeWith(git int, responses ...[2]any) (*PublicRepos, *scripted) {
	s := &scripted{}
	pr := &PublicRepos{Upstream: "github.com"}
	pr.Head = func(path string) (int, string, error) {
		s.pages = append(s.pages, path)
		r := responses[min(len(s.pages)-1, len(responses)-1)]
		return r[0].(int), r[1].(string), nil
	}
	pr.GetStatus = func(path string) (int, error) {
		s.gits = append(s.gits, path)
		return git, nil
	}
	return pr, s
}

func TestAPageThatAnswersSettlesItWithoutASecondRequest(t *testing.T) {
	pr, s := probeWith(401, [2]any{200, ""})
	if ok, _ := pr.probe("o", "r"); !ok || len(s.gits) != 0 {
		t.Fatal(ok, s.gits)
	}
}

func TestFourOhFourFallsThroughToTheGitEndpoint(t *testing.T) {
	pr, s := probeWith(401, [2]any{404, ""})
	if ok, _ := pr.probe("o", "r"); ok {
		t.Fatal("public")
	}
	if len(s.gits) != 1 || s.gits[0] != "/o/r.git/info/refs?service=git-upload-pack" {
		t.Fatal(s.gits)
	}
}

func TestAWikiHasNoPageYetIsStillPublic(t *testing.T) {
	pr, _ := probeWith(200, [2]any{404, ""})
	if ok, _ := pr.probe("OXY2DEV", "markview.nvim.wiki"); !ok {
		t.Fatal("wiki denied")
	}
}

func TestARenameIsFollowedRatherThanReadAsPrivate(t *testing.T) {
	pr, s := probeWith(401, [2]any{301, "/new-owner/new-name"}, [2]any{200, ""})
	if ok, _ := pr.probe("old", "name"); !ok {
		t.Fatal("rename read as private")
	}
	if strings.Join(s.pages, " ") != "/old/name /new-owner/new-name" {
		t.Fatal(s.pages)
	}
}

func TestARedirectOffTheUpstreamHostIsNotFollowed(t *testing.T) {
	pr, s := probeWith(401, [2]any{302, "https://evil.example/o/r"}, [2]any{200, ""})
	if ok, _ := pr.probe("o", "r"); ok {
		t.Fatal("a foreign redirect decided public")
	}
	if len(s.pages) != 1 {
		t.Fatal(s.pages)
	}
}

func TestARedirectLoopTerminates(t *testing.T) {
	pr, s := probeWith(401, [2]any{301, "/round/again"})
	if ok, _ := pr.probe("o", "r"); ok || len(s.pages) != 3 {
		t.Fatal(ok, s.pages)
	}
}
