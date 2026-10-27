package chproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

const skillTemplate = `---
name: gero-clickhouse
description: How ClickHouse access works inside this gero isolation. Use when a
  ClickHouse query needs running, when a query or connection is refused, or
  when deciding whether ClickHouse is reachable from here.
---

# ClickHouse in this isolation

There is no ClickHouse password here, and there is not meant to be one.
CLICKHOUSE_PASSWORD holds a workspace token that opens a proxy outside the
isolation; the proxy holds the real login and attaches it on the way out.
CLICKHOUSE_URL points at that proxy (plain HTTP on loopback — TLS to the real
server is the proxy's job).

Plain curl works with no setup, and FORMAT clauses work as usual:

    curl -sS -H "X-ClickHouse-Key: $CLICKHOUSE_PASSWORD" \
      "$CLICKHOUSE_URL/" --data-binary \
      "SELECT count() FROM system.tables FORMAT PrettyCompact"

HTTP drivers work the same way: clickhouse-connect and dbt's clickhouse profile
read CLICKHOUSE_HOST/CLICKHOUSE_PORT/CLICKHOUSE_USER/CLICKHOUSE_PASSWORD-shaped
settings — point them at the values already in the environment and set
secure/TLS to false (the proxy is loopback). ` + "`clickhouse-client`" + ` (the CLI) speaks
only the native TCP protocol and cannot use this proxy; use curl or an HTTP
driver instead — the SQL is identical.

Environments:

%[1]s

The default environment is ` + "`%[2]s`" + `. Each environment has its own
CLICKHOUSE_PASSWORD_<ENV> variable holding that env's workspace token;
switching envs is swapping the password value for one command:

    CLICKHOUSE_PASSWORD=$CLICKHOUSE_PASSWORD_PROD curl -sS \
      -H "X-ClickHouse-Key: $CLICKHOUSE_PASSWORD" "$CLICKHOUSE_URL/?query=..."

## When a request is refused

- A "readonly" error from the server (Cannot execute query in readonly mode /
  Cannot modify 'readonly' setting) — this environment is read-only: the proxy
  pins readonly=2 on every request and the server enforces it. INSERT, DDL and
  ` + "`SETTINGS readonly=0`" + ` are all refused, and this cannot be widened from here. If
  a write is genuinely needed, ask the operator to run
  ` + "`gero env edit clickhouse <env> --read-write`" + `.
- A refusal naming a path — only the query endpoint (/) and /ping are
  forwarded. /play and every other handler do not exist from here.
- A 429 with Retry-After is the proxy's own rate budget protecting the shared
  cluster. Wait it out; do not tighten the loop.

There is no other route to this ClickHouse server from the isolation: the proxy
is the only thing the token in CLICKHOUSE_PASSWORD opens.
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
		mode := "read-only"
		if !e.readOnly() {
			mode = "read-write"
		}
		lines = append(lines, fmt.Sprintf("- `%s`%s — %s:%d, %s (CLICKHOUSE_PASSWORD_%s)", n, def, e.Host, e.port(), mode, proxy.EnvSuffix(n)))
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
