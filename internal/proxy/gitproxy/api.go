package gitproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// The GitHub API side. The pack protocol ends at the push; opening the pull
// request is a REST call that needs the credential the isolation does not
// have. Same answer: the same token, read the same way, injected here.
//
// Reads are broad within the repository allowlist — the agent already has
// the working tree, so hiding the issue list protects nothing. Writes are
// narrow: only the families the env names, matched verb by verb against
// the routes each family means. A public repository's reads are not
// modelled: the isolation's own egress reaches api.github.com anonymously.

// MaxBody bounds a JSON write. GitHub caps issue and PR bodies at 65536
// characters, so this is headroom, not a limit anyone meets honestly.
const MaxBody = 1 << 20

const number = `[0-9]+`

type route struct {
	verb string
	re   *regexp.Regexp
}

// WriteFamilies are the whole write surface: each the set of (verb,
// repo-relative route) pairs it means, anchored so a family never grows by
// prefix accident.
var WriteFamilies = map[string][]route{
	"pulls": {
		{"POST", re(`pulls`)},
		{"PATCH", re(`pulls/` + number)},
		{"POST", re(`pulls/` + number + `/requested_reviewers`)},
	},
	"reviews": {
		{"POST", re(`pulls/` + number + `/reviews`)},
	},
	"comments": {
		{"POST", re(`issues/` + number + `/comments`)},
		{"POST", re(`pulls/` + number + `/comments`)},
		{"POST", re(`pulls/` + number + `/comments/` + number + `/replies`)},
		{"PATCH", re(`issues/comments/` + number)},
		{"PATCH", re(`pulls/comments/` + number)},
	},
	"issues": {
		{"POST", re(`issues`)},
		{"PATCH", re(`issues/` + number)},
		{"POST", re(`issues/` + number + `/labels`)},
	},
	"merge": {
		{"PUT", re(`pulls/` + number + `/merge`)},
	},
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(`\A` + s + `\z`) }

// DefaultWrites is what an env allows before anyone chooses: the PR
// workflow. Merge stays off because landing a PR is the one write that
// changes what everyone else pulls.
var DefaultWrites = []string{"comments", "pulls", "reviews"}

func familyNames() []string {
	out := make([]string, 0, len(WriteFamilies))
	for f := range WriteFamilies {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// globalReads carry no repository: who the token is, and how much budget
// remains.
var globalReads = map[string]bool{"user": true, "rate_limit": true}

// APIDenied carries the HTTP status a refusal should have.
type APIDenied struct {
	Reason string
	Status int
}

func (d *APIDenied) Error() string { return d.Reason }

func apiDeny(status int, format string, a ...any) *APIDenied {
	return &APIDenied{Reason: fmt.Sprintf(format, a...), Status: status}
}

// WritesFor is the families an env allows: nil means the default set, an
// explicit empty list means reads only.
func WritesFor(env Env) []string {
	if env.APIWrites == nil {
		return append([]string(nil), DefaultWrites...)
	}
	return append([]string(nil), *env.APIWrites...)
}

// ParseAPITarget returns (owner, repo, tail) for a /repos path, ("", "",
// name) for a global read, or a refusal.
func ParseAPITarget(rawPath string) (owner, repo, tail string, err error) {
	if strings.ContainsAny(rawPath, "%\\\x00") {
		return "", "", "", apiDeny(403, "encoded characters are not accepted in an API path")
	}
	var parts []string
	for _, p := range strings.Split(rawPath, "/") {
		if p == "." || p == ".." {
			return "", "", "", apiDeny(403, "dot segments are not accepted in an API path")
		}
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) > 0 && parts[0] == "repos" {
		if len(parts) < 3 {
			return "", "", "", apiDeny(403, "a /repos path names an owner and a repository")
		}
		if !Segment.MatchString(parts[1]) || !Segment.MatchString(parts[2]) {
			return "", "", "", apiDeny(403, "bad owner or repository name")
		}
		return parts[1], parts[2], strings.Join(parts[3:], "/"), nil
	}
	if len(parts) == 1 && globalReads[parts[0]] {
		return "", "", parts[0], nil
	}
	return "", "", "", apiDeny(404, "only /repos/{owner}/{repo}/... plus /user and /rate_limit are proxied; /graphql and the rest of the API are not modeled")
}

// ResolveRepo is the env whose allowlist carries owner/repo.
func ResolveRepo(envs map[string]Env, owner, repo string) (string, error) {
	full := strings.ToLower(owner + "/" + repo)
	var holders []string
	for name, env := range envs {
		for _, r := range env.Repos {
			if strings.TrimSuffix(strings.ToLower(r), ".git") == full {
				holders = append(holders, name)
				break
			}
		}
	}
	sort.Strings(holders)
	if len(holders) > 1 {
		return "", apiDeny(403, "%s is listed by environments %s at once; remove it from all but one (`gero env edit git <env> ...`)",
			full, strings.Join(holders, ", "))
	}
	if len(holders) == 0 {
		return "", apiDeny(404, "%s is not in any exposed git environment; ask the operator to run `gero env edit git <env> --allow-repo %s`. A public repository's reads need no proxy: api.github.com answers the isolation's own egress anonymously.", full, full)
	}
	return holders[0], nil
}

// Allowed refuses unless this verb and route are modelled and permitted.
func Allowed(env Env, envName, method, tail string) error {
	switch method {
	case "GET", "HEAD":
		return nil
	case "POST", "PATCH", "PUT", "DELETE":
	default:
		return apiDeny(403, "verb %s is not accepted here", method)
	}
	if env.ReadOnly {
		return apiDeny(403, "environment %q is read-only; API writes are refused along with push. Report it rather than working around it.", envName)
	}
	writes := WritesFor(env)
	for _, family := range familyNames() {
		for _, rt := range WriteFamilies[family] {
			if rt.verb == method && rt.re.MatchString(tail) {
				for _, w := range writes {
					if w == family {
						return nil
					}
				}
				return apiDeny(403, "%s %s is the %q write family, which environment %q does not allow; the operator can open it with `gero env edit git %s --api-write %s`",
					method, tail, family, envName, envName, family)
			}
		}
	}
	return apiDeny(403, "%s %s is not a modeled write (modeled families: %s); reads are open, other writes are not proxied",
		method, tail, strings.Join(familyNames(), ", "))
}

// GuardQuery is the caller's query minus any credential slot, with every
// other pair kept byte for byte. ?access_token= is long dead at GitHub;
// dropping it keeps this proxy from ever forwarding something shaped like
// a secret.
func GuardQuery(rawQuery string) string {
	var kept []string
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		k, _, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(k); err == nil && strings.EqualFold(key, "access_token") {
			continue
		} else if err != nil && strings.EqualFold(k, "access_token") {
			continue
		}
		kept = append(kept, pair)
	}
	return strings.Join(kept, "&")
}

// DefaultEnvName answers a global read, which has no repository to route
// by: the first exposed env, sorted, speaks for the account.
func DefaultEnvName(envs map[string]Env) (string, error) {
	if len(envs) == 0 {
		return "", apiDeny(503, "no git environment is exposed to this isolation; `gero iso expose <isolation> git <env>` first")
	}
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names[0], nil
}

type apiHandler struct {
	inst *instance
}

func (a apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	in := a.inst
	rawPath, rawQuery := proxy.RawTarget(r)
	refuse := func(err error) {
		status, reason := 403, err.Error()
		var ad *APIDenied
		if errors.As(err, &ad) {
			status = ad.Status
		}
		if status == 429 {
			w.Header().Set("Retry-After", "2")
		}
		proxy.Refuse(w, status, "gero github api proxy: "+reason)
		in.logf("DENY %s %s: %s", r.Method, rawPath, reason)
	}
	cfg, err := in.config()
	if err != nil {
		refuse(apiDeny(503, "%s", err))
		return
	}
	if !cfg.settings.API {
		refuse(apiDeny(503, "the GitHub API side is off for this isolation; opening the pull request is the operator's step"))
		return
	}
	owner, repo, tail, err := ParseAPITarget(rawPath)
	if err != nil {
		refuse(err)
		return
	}
	var envName string
	if owner == "" {
		if r.Method != "GET" && r.Method != "HEAD" {
			refuse(apiDeny(403, "/%s only answers reads", tail))
			return
		}
		envName, err = DefaultEnvName(cfg.envs)
	} else {
		envName, err = ResolveRepo(cfg.envs, owner, repo)
	}
	if err != nil {
		refuse(err)
		return
	}
	env := cfg.envs[envName]
	if err := Allowed(env, envName, r.Method, tail); err != nil {
		refuse(err)
		return
	}
	if ok, wait := in.buckets.Get(envName).Allow(); !ok {
		refuse(apiDeny(429, "rate budget for environment %q is spent; retry in %.0fs", envName, wait.Seconds()+0.5))
		return
	}
	token, err := in.credential(envName, env)
	if err != nil {
		refuse(apiDeny(503, "%s", err))
		return
	}
	var body []byte
	if r.ContentLength != 0 {
		// JSON writes are small by GitHub's own caps; a body that will not
		// say its length, or says a huge one, is refused rather than
		// buffered on faith.
		if r.ContentLength < 0 {
			refuse(apiDeny(411, "send a Content-Length body; chunked uploads are not modeled here"))
			return
		}
		if r.ContentLength > MaxBody {
			refuse(apiDeny(413, "the request body exceeds %d bytes", MaxBody))
			return
		}
		if body, err = io.ReadAll(io.LimitReader(r.Body, MaxBody+1)); err != nil {
			refuse(apiDeny(400, "could not read the request body: %v", err))
			return
		}
	}

	target := *in.apiUpstream
	if owner == "" {
		target.Path = "/" + tail
	} else {
		target.Path = "/repos/" + owner + "/" + repo
		if tail != "" {
			target.Path += "/" + tail
		}
	}
	target.RawPath = ""
	target.RawQuery = GuardQuery(rawQuery)
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		refuse(apiDeny(400, "%v", err))
		return
	}
	proxy.CopyHeaders(up.Header, r.Header, "host", "authorization", "content-length", "expect")
	up.Header.Set("Authorization", "Bearer "+token)
	if up.Header.Get("User-Agent") == "" {
		up.Header.Set("User-Agent", "gero-ghapi")
	}
	if up.Header.Get("Accept") == "" {
		up.Header.Set("Accept", "application/vnd.github+json")
	}
	up.ContentLength = int64(len(body))
	if len(body) == 0 {
		up.Body = nil
	}
	resp, err := in.transport.RoundTrip(up)
	if err != nil {
		in.logf("UPSTREAM-ERROR %s %s: %v", r.Method, target.Path, err)
		proxy.Refuse(w, 502, "gero github api proxy: upstream unavailable")
		return
	}
	defer resp.Body.Close()
	proxy.Relay(w, resp)
	in.logf("%s %s [%s] -> %d", r.Method, target.Path, envName, resp.StatusCode)
}

// ParseScopes reads X-OAuth-Scopes. Absent and empty are different facts:
// GitHub sends the header only for credentials scopes apply to, so a
// fine-grained token has none, while a classic token may have an empty one.
func ParseScopes(header string, present bool) map[string]bool {
	if !present {
		return nil
	}
	out := map[string]bool{}
	for _, s := range strings.Split(header, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}
