package slackproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// Config cannot express why a refusal happened, or that it is final; the
// skill does.

const skillTemplate = `---
name: gero-slack
description: How Slack access works inside this gero isolation. Use when a Slack
  List (slackLists.items.*), a channel message or a file attached to one needs
  reading, or when a call to the local Slack proxy is refused.
---

# Slack in this isolation

There is no Slack token here, and there is not meant to be one. SLACK_TOKEN
holds a workspace token that opens a read-only proxy outside the isolation; the
proxy holds the real token and attaches it on the way out. SLACK_API_URL points
at that proxy. Plain curl works with no setup:

    curl -sS -H "Authorization: Bearer $SLACK_TOKEN" \
      "$SLACK_API_URL/slackLists.items.list?list_id=<F...>"

    curl -sS -H "Authorization: Bearer $SLACK_TOKEN" \
      "$SLACK_API_URL/conversations.history?channel=<C...>&latest=<ts>&inclusive=true&limit=1"

What answers is each environment's *boundary* — a set of method families,
decided by the operator:

- ` + "`lists`" + ` — slackLists.items.info, slackLists.items.list
- ` + "`messages`" + ` — conversations.history, conversations.replies,
  conversations.info, for every channel except the env's deny list; a message
  permalink's /archives/<C...>/p<ts> names the channel and the timestamp
  (insert a dot: p1726000000123456 -> 1726000000.123456)
- ` + "`search`" + ` — search.messages across the workspace; denied channels are
  excluded by qualifiers the proxy appends to every query itself
- ` + "`files`" + ` — files.info, plus the file's bytes from
  ` + "`${SLACK_API_URL%%/api}/files/<F...>`" + ` — the id from a message's ` + "`files[].id`" + `. A
  message's ` + "`url_private`" + ` does not open from here (it answers with a login
  page); this path is the way to it, and a file shared in any denied channel is
  refused:

      curl -sS -H "Authorization: Bearer $SLACK_TOKEN" \
        "${SLACK_API_URL%%/api}/files/<F...>" -o <name>

auth.test always answers, as the wiring probe.

Environments and their boundaries:

%[1]s

The default is ` + "`%[2]s`" + `; each env has its own SLACK_TOKEN_<ENV> variable, so
another env for one command is a key swap:

    SLACK_TOKEN=$SLACK_TOKEN_PROD curl -sS -H "Authorization: Bearer $SLACK_TOKEN" ...

## When a request is refused

- A refusal naming a family or a channel — outside this env's boundary. That is
  the operator's decision, not a fault to retry or route around; the refusal
  names the exact ` + "`gero env edit slack ...`" + ` command that would open it, so report
  that instead of looking for another path.
- A refusal naming a method — writes and everything else in the Web API do not
  exist from here, for any environment.
- A 429 with Retry-After is the proxy's own rate budget protecting Slack's
  shared per-method limits. Wait it out; do not tighten the loop.

There is no other route to slack.com's authenticated APIs from this isolation:
the proxy is the only thing the token in SLACK_TOKEN opens.
`

func skillBody(v proxy.View, envs map[string]Env) string {
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		e := envs[n]
		families := FamiliesFor(e)
		sort.Strings(families)
		bits := strings.Join(families, ", ")
		if bits == "" {
			bits = "auth.test only"
		}
		if ReadsChannels(families) {
			denied := append([]string(nil), e.DenyChannels...)
			sort.Strings(denied)
			d := strings.Join(denied, ", ")
			if d == "" {
				d = "(none)"
			}
			bits += " — denied channels: " + d
		}
		def := ""
		if n == v.Exposure.Default {
			def = " (default)"
		}
		lines = append(lines, fmt.Sprintf("- `%s`%s — %s (SLACK_TOKEN_%s)", n, def, bits, proxy.EnvSuffix(n)))
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
