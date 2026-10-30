package linearproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

const skillTemplate = `---
name: gero-linear
description: How Linear access works inside this gero isolation. Use when a
  Linear issue, project, document or team needs reading (or, where the
  environment allows it, an issue created or changed, or a comment posted),
  or when a Linear MCP tool call is refused.
---

# Linear in this isolation

Linear arrives as MCP tools: the ` + "`linear`" + ` server (and ` + "`linear-<env>`" + ` for any
other environment) is handed to every agent gero starts, pointing at a proxy
outside the isolation. There is no Linear credential here, and there is not
meant to be one — the proxy holds a Linear login on the Mac and attaches it on
the way out. LINEAR_TOKEN is only a workspace token that opens the proxy;
LINEAR_MCP_URL is its address, for any other MCP client (a ` + "`claude`" + ` you start
by hand: ` + "`claude --mcp-config /etc/gero/mcp.json`" + `).

The login acts as a Linear user, so the tools see what that user sees. What
narrows it is each environment's *boundary* — a set of tool families, decided
by the operator:

- ` + "`issues`" + ` — list and read issues, their statuses, labels, comments and attachments
- ` + "`projects`" + ` — projects, milestones, initiatives, project updates
- ` + "`documents`" + ` — Linear documents
- ` + "`teams`" + ` — teams and their cycles
- ` + "`users`" + ` — workspace members
- ` + "`issue-edits`" + ` (write) — create and change issues (save_issue), attach files
- ` + "`comments`" + ` (write) — post and edit comments on issues
- ` + "`labels`" + ` (write) — create issue labels
- ` + "`project-edits`" + ` (write) — create and change projects, milestones, initiatives, updates
- ` + "`document-edits`" + ` (write) — create and change documents
- ` + "`deletes`" + ` (write) — delete comments and attachments

search_documentation (Linear's own help) always answers, as the wiring probe.
A tool outside the allowed families does not appear in the tool list at all.

Environments and their boundaries:

%[1]s

The default environment is ` + "`%[2]s`" + `, served as ` + "`linear`" + `.

## When a call is refused

- A tool result naming a family — outside this env's boundary. That is the
  operator's decision, not a fault to retry or route around; the refusal names
  the exact ` + "`gero env edit linear ...`" + ` command that would open it, so report that
  instead of looking for another path.
- A tool result saying a tool is not modeled — tools the proxy does not know do
  not exist from here, for any environment.
- A 429 with Retry-After is the proxy's own rate budget protecting the login's
  Linear rate limit. Wait it out; do not tighten the loop.
- An error saying the login expired — the operator has to log in again on the
  Mac (` + "`gero env secret linear <env>`" + `); nothing in the isolation can fix it.

There is no other route to Linear's authenticated APIs from this isolation: the
proxy is the only thing the token in LINEAR_TOKEN opens.
`

func skillBody(v proxy.View, envs map[string]Env) string {
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		fams := FamiliesFor(envs[n])
		sort.Strings(fams)
		var bits []string
		for _, f := range fams {
			if isWrite(f) {
				f += " (write)"
			}
			bits = append(bits, f)
		}
		b := strings.Join(bits, ", ")
		if b == "" {
			b = "probe tools only"
		}
		lines = append(lines, fmt.Sprintf("- `%s` (server `%s`) — %s", n, serverName(n, v.Exposure.Default), b))
	}
	list := strings.Join(lines, "\n")
	if list == "" {
		list = "(none exposed yet)"
	}
	def := v.Exposure.Default
	if def == "" {
		def = "(none)"
	}
	return fmt.Sprintf(skillTemplate, list, def)
}
