package grafanaproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// The skill is the agent's operational truth, and the only Grafana document
// it reads before deciding what it can do: each env line carries its save
// folders, because "every write is refused" stopped being true once an
// operator can open saves.

const skillTemplate = `---
name: gero-grafana
description: How Grafana access works inside this gero isolation. Use when a
  Grafana API call fails or is refused, when triage needs a dashboard, an alert
  rule or a data source query, or when deciding whether Grafana is reachable
  from here.
---

# Grafana in this isolation

There is no Grafana service account token here, and there is not meant to be
one. GRAFANA_TOKEN holds a workspace token that opens a proxy outside the
isolation; the proxy holds the real token and attaches it on the way out.
GRAFANA_URL points at that proxy.

Plain curl works with no setup:

    curl -sS -H "Authorization: Bearer $GRAFANA_TOKEN" \
      "$GRAFANA_URL/api/search?query=&limit=20"

Anything that takes a base URL works the same way — base URL from GRAFANA_URL,
token already in the environment (GRAFANA_SERVICE_ACCOUNT_TOKEN carries the
same value for tools that read that name).

The read surface is a fixed table: dashboard search and fetch, folders, data
source metadata, alert rules and firing instances, annotations, and
` + "`POST /api/ds/query`" + ` for running queries through a data source. Which of those
families answer is per environment, below.

There is one write: saving a dashboard, ` + "`POST /api/dashboards/db`" + ` (create, or
overwrite by UID). It answers only in an environment listed below as writing
dashboards, and only into the folders listed with it:

    curl -sS -H "Authorization: Bearer $GRAFANA_TOKEN" \
      -H "Content-Type: application/json" "$GRAFANA_URL/api/dashboards/db" \
      -d '{"folderUid": "<allowlisted uid>", "overwrite": false,
           "dashboard": {"title": "...", "panels": []}}'

- Name the folder by top-level ` + "`folderUid`" + `. ` + "`folderId`" + `, a folder inside the
  dashboard object, or no folder (General) are refused.
- To edit an existing dashboard, GET it, change it, and save it back with its
  ` + "`uid`" + ` and the ` + "`version`" + ` you read. A save naming a UID is refused unless that
  dashboard already lives in an allowlisted folder — the proxy asks Grafana
  where it is first — so copying a production dashboard means saving it under a
  new UID (or none, with its numeric ` + "`id`" + ` removed), not reusing its own.
- Deleting a dashboard, restoring a version, and every other write (folders,
  data sources, alert rules, annotations) exist nowhere here. A refusal on
  those is the answer, not a bug to route around.

Environments, their endpoint families, and what they may save:

%[1]s

The default environment is ` + "`%[2]s`" + `. Each environment has its own
GRAFANA_TOKEN_<ENV> variable holding that env's workspace token; switching envs
is swapping the token value for one command:

    GRAFANA_TOKEN=$GRAFANA_TOKEN_PROD curl -sS \
      -H "Authorization: Bearer $GRAFANA_TOKEN" "$GRAFANA_URL/api/search"

## When a request is refused

- A refusal naming an endpoint family — that family is outside the operator's
  allowlist. That is a decision, not a fault to retry or route around. If
  access is genuinely needed, ask the operator to run
  ` + "`gero env edit grafana <env> --allow-api <family>`" + `.
- A refusal naming a data source UID — ` + "`/api/ds/query`" + ` only runs against data
  sources the operator allowlisted, and every query entry must name one by UID
  (` + "`datasource: {\"uid\": ...}`" + `; the legacy name and numeric-id forms are
  refused). Ask the operator to run
  ` + "`gero env edit grafana <env> --allow-datasource <uid>`" + `.
- A refusal naming a write to the "dashboards" family — this environment reads
  dashboards but does not save them. Ask the operator for
  ` + "`gero env edit grafana <env> --allow-write dashboards --allow-folder <uid>`" + `,
  and say what you intend to build and why.
- A refusal naming a folder — the save lands in, or would overwrite a dashboard
  in, a folder outside the allowlist. Save into an allowlisted folder under a
  fresh UID instead, or ask the operator for
  ` + "`gero env edit grafana <env> --allow-folder <uid>`" + `.
- A refusal naming read-only — that write (deleting a dashboard, editing a data
  source, silencing an alert) is refused for every environment. This cannot be
  widened from here.
- Where a save *is* open, it reaches the real Grafana. A dashboard overwritten
  here is overwritten for everyone looking at it. Read the current state before
  changing it, and keep ` + "`overwrite`" + ` false unless replacing is the point.
- A 429 with Retry-After is the proxy's own rate budget protecting the shared
  instance. Wait it out; do not tighten the loop.

There is no other route to Grafana's authenticated APIs from this isolation:
the proxy is the only thing the token in GRAFANA_TOKEN opens.
`

func skillBody(v proxy.View, envs map[string]Env) string {
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	orNone := func(l []string, none string) string {
		if len(l) == 0 {
			return none
		}
		return strings.Join(l, ", ")
	}
	var lines []string
	for _, n := range names {
		e := envs[n]
		def := ""
		if n == v.Exposure.Default {
			def = " (default)"
		}
		base := e.BaseURL
		if base == "" {
			base = "?"
		}
		line := fmt.Sprintf("- `%s`%s — %s, families: %s", n, def, base, orNone(e.Families, "(none)"))
		if contains(e.Families, "query") {
			line += "; query data sources: " + orNone(e.Datasources, "(none yet)")
		}
		if len(e.Writes) > 0 {
			line += "; saves dashboards into folders: " + orNone(e.Folders, "(none yet)")
		} else {
			line += "; saves: none (read-only)"
		}
		lines = append(lines, line+fmt.Sprintf(" (GRAFANA_TOKEN_%s)", proxy.EnvSuffix(n)))
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
