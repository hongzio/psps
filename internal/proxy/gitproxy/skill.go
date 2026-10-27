package gitproxy

import (
	"fmt"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// The skill is the only thing the agent reads before deciding what it may
// do, so it says why a 403 happens and that it is a decision, not a fault to
// route around.

const skillTemplate = `---
name: gero-git
description: How git and GitHub work inside this gero isolation. Use when a git
  clone, fetch or push fails with 403, when deciding whether a repository is
  reachable, or when GitHub API work is needed instead of git.
---

# git in this isolation

There is no GitHub credential here, and there is not meant to be one. Git works
because url.insteadOf rewrites https://github.com/... to a proxy outside the
isolation, which injects the credential on the way out. Use normal GitHub URLs;
the rewrite is automatic.

**Public repositories clone normally.** The proxy forwards them with no
credential attached, exactly as an anonymous clone would, so no allowlist entry
is needed to fetch a dependency, an editor plugin or anyone's open source. They
are read-only that way, because pushing needs a credential.%[1]s

The allowlist governs the credential, so what it decides is which *private*
repositories open, and which repositories accept a push.

## When a clone or push is refused

The proxy answers with the reason, and git prints it after ` + "`remote:`" + `.

- ` + "`is not in any exposed git environment, and it is not public`" + ` — a private
  repository this isolation may not reach. You cannot add one; ask the operator
  to run ` + "`gero env edit git <env> --allow-repo owner/repo`" + `.
- ` + "`is public ... but pushing needs one`" + ` — you may read this repository but not
  write to it. Same request to the operator if writing is genuinely needed.
- an error naming a read-only environment — push is disabled for the env that
  repository belongs to. Commit locally and report that push was refused; do
  not try to work around it.
- a refusal of a push that touches .github/workflows/ comes from GitHub, not the
  proxy: the credential lacks the workflow scope. Report it to the operator.

Credentialed from here (public repositories are read-only regardless):

%[2]s

## Do not try to route around it

Rewriting the remote to a direct github.com URL, or using a different host
spelling, gains nothing on a private repository: there is no credential in this
isolation for git to send, so it only turns a clear error into a confusing one.
On a public one it is merely redundant — the proxy was already fetching it
anonymously.

%[3]s`

const apiSection = `## GitHub API work (pull requests, reviews, comments)

There is no API token here either. GITHUB_API_URL points at a proxy that
injects the credential on the way out, scoped to the same repositories as git
plus a per-environment set of write families. ` + "`gh`" + ` cannot use this proxy (it
insists on TLS to a custom API host); plain curl works with no setup and needs
no Authorization header:

    # after pushing a branch, open the PR:
    curl -sS -X POST "$GITHUB_API_URL/repos/owner/repo/pulls" \
      -d '{"title": "...", "head": "branch", "base": "main", "body": "..."}'

    curl -sS "$GITHUB_API_URL/repos/owner/repo/pulls?state=open"
    curl -sS -X POST "$GITHUB_API_URL/repos/owner/repo/issues/7/comments" \
      -d '{"body": "..."}'

Reads (GET) are open for every repository an exposed environment lists.
Writes are matched verb by verb against the env's families (each line above
says which); a refusal names the family and the operator command that would
open it — a decision, not a fault to retry around. Only
/repos/{owner}/{repo}/... plus /user and /rate_limit exist here; /graphql does
not. For a *public* repository outside the allowlist, query
https://api.github.com directly — anonymous reads work over the isolation's
own egress.
`

const apiOffSection = `## GitHub API work

The API side of this proxy is off for this isolation. Pushing still works where
the environments above allow it; opening the pull request is the operator's
step. Report the pushed branch rather than looking for another route to the
API.
`

func skillBody(v proxy.View, envs map[string]Env, s Settings) string {
	// Name repositories only while the list is short: fifty names would sit
	// in every session's context to say what one failed clone says better.
	var lines []string
	for _, name := range v.Exposure.Envs {
		env, ok := envs[name]
		if !ok {
			continue
		}
		var summary string
		switch {
		case len(env.Repos) == 0:
			summary = "(no repositories)"
		case len(env.Repos) <= 10:
			summary = strings.Join(env.Repos, ", ")
		default:
			summary = fmt.Sprintf("%d repositories; try the clone and read the refusal if there is one", len(env.Repos))
		}
		mode := "read-write"
		if env.ReadOnly {
			mode = "read-only"
		}
		// With the API side off, a families list here would promise writes
		// the section below takes back.
		apiNote := ""
		if s.API {
			writes := "none"
			if w := WritesFor(env); len(w) > 0 && !env.ReadOnly {
				writes = strings.Join(w, ", ")
			}
			apiNote = "; api writes: " + writes
		}
		lines = append(lines, fmt.Sprintf("- `%s` (%s%s) — %s", name, mode, apiNote, summary))
	}
	envList := strings.Join(lines, "\n")
	if envList == "" {
		envList = "(none — public repositories still clone, but every private one is refused until the operator exposes an env listing it)"
	}
	publicNote := ""
	if !s.PublicReads {
		publicNote = "\n\n**Except here:** this isolation also refuses public repositories through the proxy, so only the repositories listed below clone."
	}
	section := apiOffSection
	if s.API {
		section = apiSection
	}
	return fmt.Sprintf(skillTemplate, publicNote, envList, section)
}
