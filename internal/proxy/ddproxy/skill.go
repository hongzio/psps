package ddproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// The skill is the only Datadog document the agent reads before deciding
// what it may do, so a stale sentence here costs more than one anywhere
// else: the per-env list carries the writes, and the refusal section names
// the flags that widen them.

const skillTemplate = `---
name: gero-datadog
description: How Datadog access works inside this gero isolation. Use when a
  Datadog API call fails or is refused, when alert triage needs a monitor
  definition, log search or metric history, or when deciding whether Datadog is
  reachable from here.
---

# Datadog in this isolation

There are no Datadog keys here, and there are not meant to be any. DD_API_KEY
holds a workspace token that opens a proxy outside the isolation; the proxy
holds the real API and application key pair and attaches both on the way out.
DATADOG_HOST points at that proxy.

Plain curl works with no setup:

    curl -sS -H "DD-API-KEY: $DD_API_KEY" "$DATADOG_HOST/api/v1/validate"

The modern datadog-api-client (Python/Go) reads DD_API_KEY and DD_APP_KEY from
the environment but insists on building its own https URL, so point it at the
proxy explicitly — one line:

    configuration = Configuration(host=os.environ["DATADOG_HOST"])

The legacy datadogpy package honours DATADOG_HOST natively. Anything else that
can take a base URL works the same way: base URL from DATADOG_HOST, keys
already in the environment.

Environments, the endpoint families each opens, and which of those it may
write. Reading a family never implies writing it — an environment that reads
monitors still cannot mute one unless it is listed as writing them:

%[1]s

The default environment is ` + "`%[2]s`" + `. Each environment has its own
DD_API_KEY_<ENV> variable holding that env's workspace token; switching envs is
swapping the key value for one command:

    DD_API_KEY=$DD_API_KEY_PROD curl -sS -H "DD-API-KEY: $DD_API_KEY" ...

## When a request is refused

- A refusal naming an endpoint family — that family is outside the operator's
  allowlist. That is a decision, not a fault to retry or route around. If
  access is genuinely needed, ask the operator to run
  ` + "`gero env edit datadog <env> --allow-api <family>`" + `.
- A refusal naming a write — the family is readable here but not writable. The
  list above is the whole truth about which families this environment writes;
  no way of reshaping the request gets past it. Ask the operator for
  ` + "`gero env edit datadog <env> --allow-write <family>`" + `, and say what you
  intend to change and why — opening a write is a decision they make per
  family, not a formality.
- Some writes exist in Datadog and nowhere here, no matter what is open:
  ` + "`POST /api/v1/monitor/mute_all`" + ` silences the whole org and is refused even where
  monitor writes are on, and logs and traces have no write surface at all. A
  404 on those is the answer, not a bug to work around.
- Where a write *is* open, it reaches the real Datadog org. A dashboard edited
  here is edited for everyone looking at it; a muted monitor stops paging a
  human. Read the current state before changing it, and prefer the narrowest
  change that does the job.
- Log searches are automatically narrowed to the operator's scope%[3]s; a
  wider query cannot escape it, so scope your searches inside it. Log pipeline
  and index definitions (` + "`/api/v1/logs/config/...`" + `) are readable with the logs
  family and are not narrowed; they are configuration, not log content.
- A 429 with Retry-After is the proxy's own rate budget protecting the
  org-wide Datadog limit. Wait it out; do not tighten the loop.

There is no other route to Datadog's authenticated APIs from this isolation:
the proxy is the only thing the token in DD_API_KEY opens.
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
		def := ""
		if n == v.Exposure.Default {
			def = " (default)"
		}
		families := strings.Join(e.Families, ", ")
		if families == "" {
			families = "(none)"
		}
		w := WritesFor(e)
		sort.Strings(w)
		writes := strings.Join(w, ", ")
		if writes == "" {
			writes = "none (read-only)"
		}
		lines = append(lines, fmt.Sprintf("- `%s`%s — api.%s, families: %s; writes: %s", n, def, e.site(), families, writes))
	}
	list := strings.Join(lines, "\n")
	if list == "" {
		list = "(none exposed yet)"
	}
	def := v.Exposure.Default
	if def == "" {
		def = "(none)"
	}
	scope := ""
	if e, ok := envs[v.Exposure.Default]; ok && e.LogsQuery != "" {
		scope = " (`" + e.LogsQuery + "`)"
	}
	return fmt.Sprintf(skillTemplate, list, def, scope)
}
