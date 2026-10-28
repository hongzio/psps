// Package gitproxy gives an isolation git and the GitHub API without giving
// it a token. It is a port of isolate's gitproxy.py and ghapi.py.
//
// The GitHub REST API cannot clone, push or pull, so real git means a
// credential — unless the credential goes into the path instead of the
// isolation:
//
//	isolation  git clone https://github.com/owner/repo
//	             | url.insteadOf rewrites it to http://127.0.0.1:3020/owner/repo
//	             | (the tunnel)
//	daemon     this proxy — repository allowlist, optional read-only
//	             | Authorization: Basic x-access-token:<gh auth token>
//	           https://github.com/owner/repo.git
//
// gh's token carries the `repo` scope, every repository the user can touch,
// and GitHub offers no way to narrow it. The path names /{owner}/{repo}, so
// the allowlist can do what the token cannot. It governs the credential,
// not the bytes: a public repository is forwarded with no Authorization at
// all, read-only by construction, because the isolation could fetch it over
// its own egress anyway and denying it would only teach the operator to
// widen the allowlist.
package gitproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/proxy"
)

// Git's smart HTTP surface and nothing else. The dumb protocol would expose
// repository internals over paths the allowlist does not model.
var servicePaths = map[string]bool{"info/refs": true, "git-upload-pack": true, "git-receive-pack": true}

// Segment is an owner or repository name as GitHub accepts it: no slashes,
// no traversal.
var Segment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseTarget returns (owner, repo, service) for a smart-HTTP path. It is
// strict on purpose: the allowlist is only as good as this function's
// agreement with GitHub about which repository a path names, so encodings
// are refused rather than decoded and hoped about.
func ParseTarget(rawPath string) (owner, repo, service string, err error) {
	if strings.ContainsAny(rawPath, "%\\\x00") {
		return "", "", "", proxy.Deny("encoded characters are not accepted in a repository path")
	}
	var parts []string
	for _, p := range strings.Split(rawPath, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 3 {
		return "", "", "", proxy.Deny("not a git smart-HTTP path")
	}
	owner, repo = parts[0], parts[1]
	if !Segment.MatchString(owner) || !Segment.MatchString(repo) || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", "", "", proxy.Deny("bad owner or repository name")
	}
	service = strings.Join(parts[2:], "/")
	if !servicePaths[service] {
		return "", "", "", proxy.Deny("only git smart-HTTP endpoints are proxied")
	}
	return owner, strings.TrimSuffix(repo, ".git"), service, nil
}

// RequestedService is which git service a request is really for. info/refs
// announces the operation in its query, so a push is visible one request
// before git-receive-pack arrives, and read-only fails at discovery rather
// than after the client has built a pack.
func RequestedService(rawQuery, service string) string {
	if service != "info/refs" {
		return service
	}
	for _, part := range strings.Split(rawQuery, "&") {
		if v, ok := strings.CutPrefix(part, "service="); ok {
			return v
		}
	}
	return service
}

func IsWrite(service string) bool { return service == "git-receive-pack" }

// What the policy decided about the credential, which is the only thing it
// is for.
const (
	Authenticated = "authenticated"
	Anonymous     = "anonymous"
)

// Checker says whether a repository is public.
type Checker interface {
	Check(owner, repo string) bool
}

// Policy decides whether a request gets a credential, and whose. An env
// bundles repositories with the command that yields their credential, so
// the repository in the path routes: the env listing it supplies the token,
// and read-only is that env's property. A repository two exposed envs both
// list is refused rather than resolved by precedence.
type Policy struct {
	public   Checker
	readOnly map[string]bool
	route    map[string]string
	overlaps map[string][]string
}

func NewPolicy(envs map[string]Env, public Checker) *Policy {
	p := &Policy{public: public, readOnly: map[string]bool{}, route: map[string]string{}, overlaps: map[string][]string{}}
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		env := envs[name]
		p.readOnly[name] = env.ReadOnly
		for _, r := range env.Repos {
			key := strings.TrimSuffix(strings.ToLower(r), ".git")
			if prev, ok := p.route[key]; ok && prev != name {
				if len(p.overlaps[key]) == 0 {
					p.overlaps[key] = []string{prev}
				}
				p.overlaps[key] = append(p.overlaps[key], name)
			}
			p.route[key] = name
		}
	}
	for key := range p.overlaps {
		delete(p.route, key)
	}
	return p
}

// Check returns the decision and the env whose credential to use.
func (p *Policy) Check(owner, repo, service string) (string, string, error) {
	full := strings.ToLower(owner + "/" + repo)
	if names, ok := p.overlaps[full]; ok {
		return "", "", proxy.Deny("%s is listed by environments %s at once; remove it from all but one (`gero env edit git <env> ...`)",
			full, strings.Join(names, ", "))
	}
	if env, ok := p.route[full]; ok {
		if p.readOnly[env] && IsWrite(service) {
			return "", "", proxy.Deny("environment %q is read-only; %s is not permitted", env, service)
		}
		return Authenticated, env, nil
	}
	public := p.public != nil && p.public.Check(owner, repo)
	switch {
	case public && !IsWrite(service):
		return Anonymous, "", nil
	case public:
		return "", "", proxy.Deny("%s is public, so it is readable here without a credential, but pushing needs one: add it to a git environment with `gero env edit git <env> --allow-repo %s`", full, full)
	case len(p.route) == 0:
		return "", "", proxy.Deny("%s is not public and no exposed git environment lists any repository; add one with `gero env add git <name> --allow-repo owner/repo`", full)
	}
	return "", "", proxy.Deny("%s is not in any exposed git environment, and it is not public", full)
}

// PublicRepos asks GitHub, anonymously, which repositories it already serves
// to nobody in particular. A 200 to a request carrying no credential is
// evidence, not assertion.
//
// A wrong answer cannot leak a credential: this picks between forwarding
// with no Authorization header and denying, never between forwarding with
// the token and without. A false "public" earns a 401 from GitHub.
type PublicRepos struct {
	TTL      time.Duration
	Attempts int
	// Head and GetStatus reach the upstream; tests replace them.
	Head      func(path string) (status int, location string, err error)
	GetStatus func(path string) (int, error)
	Upstream  string // host, for judging redirects
	Log       func(string)
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]publicAnswer
}

type publicAnswer struct {
	public bool
	at     time.Time
}

// NewPublicRepos probes base (https://github.com) through rt.
func NewPublicRepos(base *url.URL, rt http.RoundTripper, logf func(string)) *PublicRepos {
	pr := &PublicRepos{TTL: 5 * time.Minute, Attempts: 2, Upstream: base.Host, Log: logf}
	do := func(method, path string) (*http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, base.Scheme+"://"+base.Host+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "*/*")
		req.Header.Set("User-Agent", "gero-gitproxy")
		resp, err := rt.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		// The body is never read: on the git endpoint it can front a ref
		// advertisement worth many megabytes, and only the status is wanted.
		resp.Body.Close()
		return resp, nil
	}
	pr.Head = func(path string) (int, string, error) {
		resp, err := do("HEAD", path)
		if err != nil {
			return 0, "", err
		}
		return resp.StatusCode, resp.Header.Get("Location"), nil
	}
	pr.GetStatus = func(path string) (int, error) {
		resp, err := do("GET", path)
		if err != nil {
			return 0, err
		}
		return resp.StatusCode, nil
	}
	return pr
}

func (pr *PublicRepos) clock() time.Time {
	if pr.now != nil {
		return pr.now()
	}
	return time.Now()
}

// Check answers from a five-minute cache. A transport failure is not
// evidence about visibility: it is retried once, then denied, and neither
// cached nor silent — swallowing one turned a slow probe into a confident,
// wrong, unexplained denial of a repository anyone can clone.
func (pr *PublicRepos) Check(owner, repo string) bool {
	key := strings.ToLower(owner + "/" + repo)
	pr.mu.Lock()
	if hit, ok := pr.cache[key]; ok && pr.clock().Sub(hit.at) < pr.TTL {
		pr.mu.Unlock()
		return hit.public
	}
	pr.mu.Unlock()

	var answer bool
	var failure error
	ok := false
	for range max(pr.Attempts, 1) {
		a, err := pr.probe(owner, repo)
		if err == nil {
			answer, ok = a, true
			break
		}
		failure = err
	}
	if !ok {
		if pr.Log != nil {
			pr.Log(fmt.Sprintf("PUBLIC-PROBE-FAILED %s/%s: %v", owner, repo, failure))
		}
		return false
	}
	pr.mu.Lock()
	if pr.cache == nil {
		pr.cache = map[string]publicAnswer{}
	}
	pr.cache[key] = publicAnswer{answer, pr.clock()}
	pr.mu.Unlock()
	return answer
}

// probe asks the cheap question first and the authoritative one only if it
// says no. The repository page answers torvalds/linux in well under a
// second, where info/refs builds the whole ref advertisement before its
// status line; but a wiki is a real repository at owner/repo.wiki.git with
// no page of its own, so when the page declines, the git endpoint has the
// last word.
func (pr *PublicRepos) probe(owner, repo string) (bool, error) {
	if ok, err := pr.pageSaysPublic(owner, repo, 2); err != nil || ok {
		return ok, err
	}
	status, err := pr.GetStatus(fmt.Sprintf("/%s/%s.git/info/refs?service=git-upload-pack", owner, repo))
	return status == 200, err
}

// pageSaysPublic follows redirects on the same host only, so a renamed
// repository is not taken for a private one and a redirect elsewhere
// cannot nominate what counts as public.
func (pr *PublicRepos) pageSaysPublic(owner, repo string, hops int) (bool, error) {
	path := "/" + owner + "/" + repo
	for range hops + 1 {
		status, location, err := pr.Head(path)
		if err != nil {
			return false, err
		}
		if status == 200 {
			return true, nil
		}
		if (status != 301 && status != 302 && status != 307 && status != 308) || location == "" {
			return false, nil
		}
		target, err := url.Parse(location)
		if err != nil || (target.Host != "" && target.Host != pr.Upstream) || target.Path == "" {
			return false, nil
		}
		path = target.Path
	}
	return false, nil
}

// TokenSource runs the env's credential command on demand and keeps the
// answer for a minute, so a clone does not spawn a process per request and
// rotation on the host is still picked up.
type TokenSource struct {
	command []string
	ttl     time.Duration
	now     func() time.Time

	mu      sync.Mutex
	value   string
	fetched time.Time
}

func NewTokenSource(command []string, ttl time.Duration) *TokenSource {
	cmd := append([]string(nil), command...)
	if len(cmd) > 0 && !strings.Contains(cmd[0], "/") {
		// launchd's PATH may lack it; the definition pins an absolute path
		// when it is saved, and this catches one that was not.
		if p, err := exec.LookPath(cmd[0]); err == nil {
			cmd[0] = p
		}
	}
	return &TokenSource{command: cmd, ttl: ttl, now: time.Now}
}

func (t *TokenSource) Get() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != "" && t.now().Sub(t.fetched) < t.ttl {
		return t.value, nil
	}
	if len(t.command) == 0 {
		return "", proxy.Deny("this git environment has no credential command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.command[0], t.command[1:]...).Output()
	var ee *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &ee):
		return "", proxy.Deny("credential command %q is not runnable: %v", t.command[0], err)
	case err != nil || strings.TrimSpace(string(out)) == "":
		return "", proxy.Deny("could not obtain a git credential from %s", strings.Join(t.command, " "))
	}
	t.value, t.fetched = strings.TrimSpace(string(out)), t.now()
	return t.value, nil
}

// basic is how GitHub's git endpoint takes a token: as the password, with a
// username that is ignored but must be present. Bearer is refused there.
func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// gitHandler serves the pack protocol for one isolation.
type gitHandler struct {
	inst *instance
}

func (g gitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	in := g.inst
	rawPath, rawQuery := proxy.RawTarget(r)
	deny := func(status int, err error) {
		reason := err.Error()
		proxy.Refuse(w, status, reason)
		in.logf("DENY %s %s: %s", r.Method, rawPath, reason)
	}
	cfg, err := in.config()
	if err != nil {
		deny(503, err)
		return
	}
	owner, repo, service, err := ParseTarget(rawPath)
	if err != nil {
		deny(403, err)
		return
	}
	var public Checker
	if cfg.settings.PublicReads {
		public = in.public
	}
	decision, envName, err := NewPolicy(cfg.envs, public).Check(owner, repo, RequestedService(rawQuery, service))
	if err != nil {
		deny(403, err)
		return
	}
	// Not fetched at all on the anonymous path, so a public clone cannot
	// spend the credential even by accident.
	var token string
	if decision == Authenticated {
		if token, err = in.credential(envName, cfg.envs[envName]); err != nil {
			deny(503, err)
			return
		}
	}

	target := *in.gitUpstream
	target.Path = "/" + owner + "/" + repo + ".git/" + service
	target.RawPath = ""
	target.RawQuery = rawQuery
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		deny(400, err)
		return
	}
	// The client's own Authorization is always dropped, never merely
	// overwritten: on the anonymous path there is nothing to overwrite it
	// with, and the request must still leave bare.
	proxy.CopyHeaders(up.Header, r.Header, "host", "authorization", "content-length", "expect")
	if token != "" {
		up.Header.Set("Authorization", basic(token))
	}
	up.ContentLength = r.ContentLength
	if r.ContentLength == 0 {
		up.Body = nil
	}
	resp, err := in.transport.RoundTrip(up)
	if err != nil {
		in.logf("UPSTREAM-ERROR %s %s: %v", r.Method, target.Path, err)
		proxy.Refuse(w, 502, "upstream unavailable")
		return
	}
	defer resp.Body.Close()
	proxy.Relay(w, resp)
	label := decision
	if envName != "" {
		label += ":" + envName
	}
	in.logf("%s %s/%s %s [%s] -> %d", r.Method, owner, repo, service, label, resp.StatusCode)
}
