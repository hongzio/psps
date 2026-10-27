package notionproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

const skillTemplate = `---
name: gero-notion
description: How Notion access works inside this gero isolation. Use when a
  Notion page, database or search needs reading (or, where the environment
  allows it, writing), or when a Notion MCP tool call is refused.
---

# Notion in this isolation

Notion arrives as MCP tools: the ` + "`notion`" + ` server (and ` + "`notion-<env>`" + ` for any
other environment) is handed to every agent gero starts, pointing at a proxy
outside the isolation. There is no Notion credential here, and there is not
meant to be one — the proxy holds a Notion login on the Mac and attaches it on
the way out. NOTION_TOKEN is only a workspace token that opens the proxy;
NOTION_MCP_URL is its address, for any other MCP client (a ` + "`claude`" + ` you start
by hand: ` + "`claude --mcp-config /etc/gero/mcp.json`" + `).

The login acts as a Notion user, so the tools see what that user sees — there
is no sharing screen narrowing it. What narrows it is each environment's
*boundary* — a set of tool families, decided by the operator:

- ` + "`content`" + ` — notion-fetch, data source queries, page listings, backlinks,
  comments, attachments
- ` + "`search`" + ` — notion-search
- ` + "`users`" + ` — workspace members, teams, groups
- ` + "`meetings`" + ` — meeting notes
- ` + "`calendar`" + ` — calendars, events, contacts
- ` + "`connectors`" + ` — search across Slack, Drive, GitHub connected to Notion
- ` + "`memory`" + ` — memories saved by connected agents
- ` + "`skills`" + ` — download and list Notion Skills
- ` + "`agents`" + ` — find Custom Agents and read their sessions
- ` + "`comments`" + ` (write) — create, edit, resolve, react to comments
- ` + "`pages`" + ` (write) — create, update, move, duplicate pages; folders; attachments
- ` + "`databases`" + ` (write) — create databases; edit data sources and views
- ` + "`trash`" + ` (write) — delete and restore pages and databases
- ` + "`events`" + ` (write) — create, update, cancel calendar events
- ` + "`sessions`" + ` (write) — start and steer Custom Agent sessions
- ` + "`skill-edits`" + ` (write) — upload, enable, convert Skills

notion-get-self and notion-get-tool-access always answer, as the wiring probe.
A tool outside the allowed families does not appear in the tool list at all.

Environments and their boundaries:

%[1]s

The default environment is ` + "`%[2]s`" + `, served as ` + "`notion`" + `.

## When a call is refused

- A tool result naming a family — outside this env's boundary. That is the
  operator's decision, not a fault to retry or route around; the refusal names
  the exact ` + "`gero env edit notion ...`" + ` command that would open it, so report that
  instead of looking for another path.
- A tool result saying a tool is not modeled — tools the proxy does not know do
  not exist from here, for any environment.
- A 429 with Retry-After is the proxy's own rate budget protecting the login's
  Notion rate limit. Wait it out; do not tighten the loop.
- An error saying the login expired — the operator has to log in again on the
  Mac (` + "`gero env secret notion <env>`" + `); nothing in the isolation can fix it.

There is no other route to Notion's authenticated APIs from this isolation: the
proxy is the only thing the token in NOTION_TOKEN opens.
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
