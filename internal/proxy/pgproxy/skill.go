package pgproxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// Config cannot express why a refusal happened, or that it is final; the
// skill does.

const skillTemplate = `---
name: gero-pg
description: How PostgreSQL access works inside this gero isolation. Use when a
  psql or database connection fails, when a query is refused with permission
  denied, or when deciding whether a database is reachable from here.
---

# PostgreSQL in this isolation

There is no database password here, and there is not meant to be one. libpq's
standard variables (PGHOST, PGPORT, PGUSER, PGPASSFILE) point at a proxy
outside the isolation that holds the real credential and completes
authentication with the server itself. ` + "`psql`" + ` (installed), psycopg and every
libpq binding pick this up with no flags. A driver that ignores them (pg8000)
needs host, port and user passed explicitly from the environment.

The user in the connection string names an *environment*, not a database
role:

%[1]s

The default environment (what PGUSER already says) is ` + "`%[2]s`" + `. For another
environment or database:

    psql "postgres://<env>@127.0.0.1:%[3]d/<database>"

Do not change PGHOST, PGUSER or PGPASSFILE, and do not add a password: none of
that can add access, and all of it disconnects the working chain.

## When a connection or query is refused

- An error naming this proxy (unknown environment, database not allowed) — the
  database is outside the operator's allowlist. That is a decision, not a fault
  to retry or route around. If access is genuinely needed, ask the operator to
  run ` + "`gero env edit pg <env> --allow-db <name>`" + `.
- ` + "`permission denied for table ...`" + ` — the environment's role does not carry
  that grant. Postgres itself is enforcing this; only the operator can widen
  the role.
- ` + "`password authentication failed`" + ` for the real server — the stored password
  is stale. Report it; only the operator can update it
  (` + "`gero env secret pg <env>`" + `).

There is no other route to these databases from this isolation: the proxy is
the only thing the token in PGPASSFILE opens.
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
		dbs := strings.Join(e.Databases, ", ")
		if dbs == "" {
			dbs = "(none)"
		}
		lines = append(lines, fmt.Sprintf("- `%s` — databases: %s (role %s)", n, dbs, e.User))
	}
	list := strings.Join(lines, "\n")
	if list == "" {
		list = "(none exposed yet)"
	}
	def := v.Exposure.Default
	if def == "" {
		def = "(none)"
	}
	return fmt.Sprintf(skillTemplate, list, def, Port)
}
