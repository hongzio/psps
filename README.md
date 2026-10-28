# gero

Runs Claude Code and Codex agents inside Apple `container` isolations, and
shows what they do in its own format. See [.docs/PRD.md](.docs/PRD.md) and
[.docs/FEASIBILITY.md](.docs/FEASIBILITY.md).

## Build

```sh
mise install
mise run build        # cross-compiles the linux shim, embeds it, writes bin/gero
mise run test
```

## Use

```sh
bin/gero                                  # start the daemon (registers a LaunchAgent)
sudo bin/gero pf install                  # once: the network fence helper (see below)
bin/gero iso create dev                   # first run builds the base image
bin/gero iso login dev codex              # or claude; or from the web / TUI (see below)
bin/gero iso upgrade dev                  # after a gero upgrade: new image, same volumes
bin/gero iso verify dev                   # measure the fence, the proxies, the mounts
bin/gero agent new --iso dev --kind codex "fix the flaky test"
bin/gero agent ls
bin/gero agent log <id> -f
bin/gero agent send <id> "also update the changelog"
```

### TUI

```sh
bin/gero tui
```

Agents on the left (status at a glance; archived ones folded at the bottom),
the selected agent's conversation in the middle, the input at the bottom with
the agent's settings behind `ctrl+o`. `ctrl+n` starts an agent, `ctrl+g`
manages isolations (environment, upgrades, `v` to verify one, `l` its accounts
(what Claude and Codex log in with), and with `p` the credential proxies each one serves), `ctrl+r` the
proxies' environments (secrets and
browser logins included), `ctrl+a` answers a pending approval, `f1` lists
every key. Rebind any of them:

```sh
bin/gero tui --keys > ~/.config/gero/keys.toml   # then edit
```

### Web

```sh
bin/gero web          # opens http://127.0.0.1:4376 signed in
```

The same agents, conversations, approvals, isolation settings, accounts
(what Claude and Codex log in with) and credential proxies (the Services page, and each isolation's
services), laid out for a
desktop browser or a phone. The daemon serves it on loopback only, and
every API call needs the token `gero web` signs you in with (`--rotate`
issues a new one and signs every browser out). To use it from a phone,
expose it to your tailnet yourself — `gero web` prints the exact
`tailscale serve` command and the phone link — so Tailscale provides the
HTTPS certificate and decides which devices can reach it.

For development, `GERO_HOME=$PWD/.dev` keeps all state under `.dev/` and runs
the daemon as a plain background process instead of touching launchd.

### Claude & Codex: accounts and logins

An agent CLI in an isolation gets its credential one of two ways.

**An account**, defined once on the Mac and used by any number of
isolations (one per CLI in each). Only credentials that stay valid wherever
they are used at once qualify: a Claude token from `claude setup-token`
(your subscription, valid for a year) or an Anthropic API key. The secret
lives in the credentials file under `[account NAME]`; agents get it as
`CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY` when they start.

```sh
claude setup-token                                  # on any machine with a browser
bin/gero account add claude --kind claude-token     # paste it
bin/gero iso account dev claude claude              # dev's Claude uses it (- goes back to a login inside)
bin/gero account set claude                         # rotate: idle agents restart with the new one
```

A ChatGPT login cannot be shared that way: its refresh token is single-use,
so copies of one login in several places log each other out, and OpenAI
offers Plus/Pro no long-lived token (Codex access tokens are for Business
and Enterprise workspaces). Codex therefore logs in inside each isolation.
A token that used to sit in an isolation's environment was moved into an
account, once, when the daemon first started with accounts.

**A login inside the isolation.** `gero iso login dev` shows both CLIs as
an agent there finds them (account and environment applied). A login is run
by the daemon inside the isolation, with no terminal, so the web page (a
phone's included), the TUI and the CLI all drive the same one:

- **Claude**: `claude auth login` prints an address; sign in there and the
  page ends on a code, which you paste back. A wrong code ends the login;
  start it again.
- **Codex**: `codex login --device-auth` prints an address and a one-time
  code to enter there, and notices by itself.

The daemon passes the address and the codes along and keeps them only in
memory for the login's 15 minutes; the credential lands in the isolation as
before. A login (or `gero iso logout`) restarts the isolation's idle agents
of that kind, since Codex reads its login only when it starts.

## How it holds together

- **gero-shim** is each isolation container's main process. It owns the agent
  processes and keeps a durable, sequenced log of everything they read and
  write, on a volume. It never parses agent output.
- **The daemon** reaches the shim through `container exec -i` (no IP route to
  the host), replays an agent's log through a **driver** that understands its
  protocol (`codex app-server` JSON-RPC, `claude -p` stream-json), and stores
  the resulting events in SQLite.
- Daemon restarts lose nothing: agents keep running under the shim and the
  daemon catches up from the log. A reboot or container stop ends the agent
  process; the turn in flight shows as `interrupted`, and the next message
  starts a process that resumes the same session.

## Network fence

Each isolation gets its own container network, and a small root helper
(`dev.gero.pfd`, installed by `sudo gero pf install`) keeps the host's packet
filter fencing those networks off the Mac and its LAN — IPv4 and IPv6 —
while internet egress stays open. The rules live in the `com.apple/gero`
anchor that the stock `/etc/pf.conf` already evaluates, so no system file is
edited. The helper recomputes them every few seconds from the Mac's current
addresses and bridges, so Wi-Fi changes, new containers and reboots need
nothing from you.

The daemon trusts the fence only after measuring it: it listens on every
address the Mac holds and tries each one from inside the isolation. Until
that measurement passes, agents in the isolation take no new turns; if it
finds a hole, they are stopped. A new container's bridge is fenced for IPv6
only once the helper sees it, so the daemon asks the helper again until the
rules pf actually holds cover that bridge, and only then measures; a probe
that could not even leave the isolation (its link-local address is still
tentative for the first seconds after boot) counts as not measured, not as a
pass or a leak. `gero iso create NAME --fence off` opts an
isolation out. `gero pf status -v` shows the loaded rules;
`sudo gero pf uninstall` removes everything the helper installed.

## Credential proxies

An agent can **use** a credential without being able to **read** it: the
credential stays on the Mac, inside a proxy that injects it on the way out,
within an allowlist. These are ports of
[isolate](https://github.com/hongzio/isolate)'s proxies, decision and wire
tests included. Inside the isolation they are fixed loopback addresses:

```
127.0.0.1:3020   git        url.insteadOf sends github.com URLs here
127.0.0.1:3021   AWS        AWS_CONTAINER_CREDENTIALS_FULL_URI points here
127.0.0.1:3022   PostgreSQL PGHOST/PGPORT/PGUSER/PGPASSFILE point here
127.0.0.1:3023   Datadog    DATADOG_HOST points here (DD_API_KEY holds a workspace token)
127.0.0.1:3024   Slack      SLACK_API_URL points here
127.0.0.1:3025   ClickHouse CLICKHOUSE_URL (HTTP interface) points here
127.0.0.1:3026   GitHub API GITHUB_API_URL points here
127.0.0.1:3027   Grafana    GRAFANA_URL points here
127.0.0.1:3028   Notion     NOTION_MCP_URL; every agent gets it as an MCP server
```

The proxies run inside the daemon; nothing listens on the Mac. Each isolation
has one tunnel — SSH over its `container exec` channel, from `gero-shim
tunnel` — and a connection on it reaches that isolation's proxies only, so the
tunnel is who is asking. Edits take effect on the next request.

An **environment** is defined once and exposed per isolation:

```sh
gero env add git work --allow-repo owner/repo [--read-only]
gero env check git work                  # run its credential, ask GitHub who it is
gero iso expose dev git work             # dev now routes owner/repo with that credential
gero iso services dev
gero iso unexpose dev git
gero env help git                        # every flag
```

git with no env (`gero iso expose dev git`) forwards public repositories with
no credential and refuses the rest. What a service puts in the isolation — its
variables in `/etc/gero/env.sh` (and in every agent's environment), git's URL
rewrite in `/etc/gero/gitconfig`, a skill under `~/.claude/skills/gero-*` and
`~/.codex/skills/gero-*` explaining that a refusal is a decision — is written
by `gero-shim sync` and removed when the service is turned off.

Secrets for services that keep one live in `~/.config/gero/credentials`, an
INI of `[service env]` sections, 0600, read per request:
`gero env secret SERVICE NAME` prompts for them (or reads one line per field
from stdin). git uses the env's credential command instead (`gh auth token`).

AWS is a credential broker rather than a proxy: it assumes an IAM role with
the Mac's AWS CLI (`aws sts assume-role --profile ...`) under a session policy
built from the env's allowlist, and hands the isolation the short-lived
result through the standard container-credentials variables, so every SDK
and session-manager-plugin work with no ~/.aws. AWS enforces the allowlist.
Narrowing it applies to the next issue at once; credentials already issued
stay valid until they expire (at most an hour), as STS cannot revoke them.

```sh
gero env add aws dev --role-arn arn:aws:iam::123456789012:role/agent --aws-profile dev \
  --allow-instance i-0123456789abcdef0=session --allow-bucket transfer-bucket=read
gero env check aws dev                   # assume the role now, ask STS who it is
gero iso expose dev aws dev
```

PostgreSQL is terminated on both sides, the way pgbouncer does it: the
isolation signs in with its workspace token (from PGPASSFILE), the env name
as the user and a database on the allowlist; the proxy then opens its own
connection with the env's role and stored password (SCRAM-SHA-256, MD5 or
cleartext, TLS per `--sslmode`) and becomes a byte pump. What the role may
do inside those databases is Postgres's grants, deliberately not a SQL
filter. ClickHouse is fronted over its HTTP interface only: the query
endpoint and /ping, the real login attached, and for a read-only env (the
default) `readonly=2` pinned so the server itself refuses writes.

```sh
gero env add pg prod --host db.internal --user app_ro --sslmode verify-full --allow-db app
gero env secret pg prod                  # the role's password
gero env add clickhouse main --host ch.internal
gero env secret clickhouse main          # user and password
```

Datadog, Grafana and Slack are HTTP proxies over an allowlist of endpoint
families, reads by default and each write family a separate choice
(`--allow-api`, `--allow-write`; a write whose reads are not open is dropped
with a warning naming the flags that keep it). Datadog can pin a log-search
scope, Grafana bounds queries to named data sources and saves to named
folders, Slack can deny channels. For these, as for ClickHouse and Notion,
the isolation's token is per env and its value picks the env.

Notion is its hosted MCP server, behind a fixed tool table: a tool the
table does not model does not exist from the isolation, and writes are
families an env opens by name. There is no integration to create; gero logs
in as you:

```sh
gero env add notion work                 # content and search; --allow-api pages ... for more
gero env secret notion work              # prints a URL; approve it in any browser
gero iso expose dev notion work
```

`gero env secret notion` works from a browser on another device too: the
page Notion sends you to afterwards will not load there; paste its address
back into the terminal. Services that are MCP servers reach agents at launch
— Claude Code through `--mcp-config /etc/gero/mcp.json`, Codex through `-c
mcp_servers.…` — so neither tool's own configuration is touched.

Isolations created before the proxies need `gero iso upgrade NAME` once;
`gero iso ls` says so under PROXIES.

## Verify

```sh
gero iso verify dev          # exit 0 ok / 1 a guarantee is broken / 2 setup only
gero iso verify              # every isolation
gero iso verify dev --json
```

Measures what an isolation can do, now, instead of trusting the
configuration: the fence afresh (every address the Mac holds, from inside),
that no Mac directory is mounted, and for each exposed proxy that the tunnel
carries its ports, that it refuses a request it must refuse — a wrong
workspace token, an unlisted repository — made from inside the isolation as
the agent, with curl or psql, and that each env still answers from the Mac
(`gero env check`). Then that the credentials file is owner-only and the
internet is still reachable. FAIL lines are broken guarantees, WARN lines
setup; a leak found here stops agents like any other measurement. The TUI
has it on `v` in the isolations page, the web on each isolation card.
