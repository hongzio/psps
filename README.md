# gero

Runs Claude Code and Codex agents inside Apple `container` isolations, and
shows what they do in its own format. See [.docs/PRD.md](.docs/PRD.md) and
[.docs/FEASIBILITY.md](.docs/FEASIBILITY.md).

## Install

On an Apple silicon Mac:

```sh
brew trust hongzio/tap   # Homebrew 7 loads no third-party tap it does not trust
brew tap hongzio/tap
brew install gero        # the release's binary; brings Apple's container
```

`brew upgrade gero` is all an upgrade takes: the next gero command moves the
daemon onto the new version, its agents carrying on. Run `sudo gero pf install`
again after one, since the fence helper is a copy of gero. Before
`brew uninstall gero`, run `gero daemon uninstall` and `sudo gero pf uninstall`.

A release is `sh scripts/release.sh 1.2.3`: it tags main, builds gero, attaches
it to the GitHub release and points the tap's formula (from
[packaging/gero.rb](packaging/gero.rb)) at it.

## Build

```sh
mise install
mise run build        # cross-compiles the linux shim, embeds it, writes bin/gero
mise run test
```

A build of your own says it is version `dev`; the formula stamps the release's.

## Use

```sh
bin/gero                                  # start the daemon (registers a LaunchAgent)
sudo bin/gero pf install                  # once: the network fence helper (see below)
bin/gero iso create dev                   # first run builds the base image
bin/gero iso login dev codex              # or claude; or from the web / TUI (see below)
bin/gero iso upgrade dev                  # after a gero upgrade: new image, same volumes
bin/gero iso set dev --memory 8G --cpus 6  # resize: a new container, same volumes
bin/gero iso rename dev work              # agents keep running
bin/gero iso verify dev                   # measure the fence, the proxies, the mounts
bin/gero agent new --iso dev --kind codex "fix the flaky test"
bin/gero agent new --iso dev --kind claude  # no prompt: it waits for its first message
bin/gero agent new --iso dev --kind codex --id login "fix the login bug"
bin/gero agent ls
bin/gero agent log <id> -f
bin/gero agent send <id> "also update the changelog"
bin/gero agent inbox <id> [--rm N|--clear] # mail other agents sent it (see Mail)
bin/gero agent rename <id> login bug      # only the name changes; a turn carries on
bin/gero agent id <id> login              # an ID of your own (--clear gives it back)
bin/gero agent archive <id>               # stops it; its history is kept
bin/gero agent rm <id> [--purge]          # an archived one: gone from gero; --purge also
                                          # removes its workspace and session transcript
```

### TUI

```sh
bin/gero tui
```

Agents on the left (status at a glance; archived ones folded at the bottom,
`r` renames one, `i` gives it an ID of your own, `d` deletes one),
the selected agent's conversation in the middle, the input at the bottom with
the agent's settings behind `ctrl+o`. `ctrl+n` makes an agent (its first message, typed in the input, starts it and names it), `ctrl+g`
manages isolations (environment, `s` to rename or resize one, upgrades, `v` to verify one, `l` its accounts
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

### Approvals

Each agent runs in one of four modes (`--approvals`, or its settings in the
TUI and on the web), changed at any time:

- **never** (the default): unattended. The isolation, its network fence and
  the credential proxies are the boundary.
- **auto**: unattended, with a reviewer model in your place. Claude runs in
  its auto mode: a classifier checks each action before it runs and refuses
  what goes beyond the request, targets unknown infrastructure or follows
  instructions planted in content it read; what it keeps refusing comes to
  you as an approval. Codex runs its own Auto: a workspace-write sandbox
  whose escapes (the network, the proxies included, and writes outside the
  workspace) go to its auto-review agent; the conversation shows each
  review, a refusal in red. Both cost a little latency per reviewed action,
  and the reviews count toward the CLI's usage.
- **ask**: every tool use waits for you (`ctrl+a` in the TUI, the
  conversation on the web).
- **default**: the CLI's own configuration in the isolation decides:
  Claude's `~/.claude/settings.json` (`permissions.defaultMode`; manual,
  asking for anything not allowed, when it says nothing), Codex's
  `~/.codex/config.toml` (`approval_policy`, `sandbox_mode`,
  `approvals_reviewer`; Codex's own defaults, a read-only sandbox that asks
  for writes and the network, where it says nothing). Whatever that asks
  comes to you as an approval. Edit those files with `gero iso shell`; a
  running agent picks the change up when its mode is set again or it
  restarts.

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
- An isolation is an ID to gero (`7f3a9c1b`); its name is what people call
  it. Everything that belongs to it names it by ID: its agents, exposures,
  tokens and accounts in the store, and its container, network and volumes
  (`gero-7f3a9c1b`, `-net`, `-home`, `-state`; `gero iso ls` shows the
  container). So `gero iso rename` changes the name and nothing else, and a
  new isolation never picks up an old one's volumes. Isolations from before
  IDs kept their name as their ID. `container` cannot resize a VM, so
  `gero iso set` replaces the container on the same volumes, as an upgrade
  does: running agents end, sessions and workspaces stay, and a size the new
  container will not start with is put back.
- An agent likewise has a key gero gives it (`77b17c2c`), which its
  workspace, log and history are named after, and an ID it goes by: yours
  (`--id`, `gero agent id`, the TUI's `i`, the web's Name & ID), or else an
  adjective-noun gero picks (`brave-otter`, one of 250,000). The ID names it wherever the key
  does and is shown in its place; no other agent may have it, as theirs or
  as their key, archived ones included, which is checked each time one is
  given. Taking it away (`--clear`, or empty) frees it, and the agent goes
  by its key. The key stays random: a deleted agent's workspace stays
  unless purged, and an ID that came back would find it.

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
127.0.0.1:3029   mail       GERO_MAIL_URL, each agent's own (see Mail)
```

The proxies run inside the daemon; nothing listens on the Mac. Each isolation
has one tunnel — SSH over its `container exec` channel, from `gero-shim
tunnel` — and a connection on it reaches that isolation's proxies only, so the
tunnel is who is asking. Edits take effect on the next request.

An **environment** is defined once and exposed per isolation:

```sh
gero env add git work --allow-repo owner/repo [--read-only]
gero env secret git work                 # its GitHub token (or: --token-command gh)
gero env choices git work repos          # the repositories that token opens
gero env check git work                  # take its credential, ask GitHub who it is
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
from stdin). git's is a GitHub token; an env may instead name a
gh-compatible command (`--token-command gh`, which runs `gh auth token`), and
a stored token wins over it. The web and TUI env forms for git ask for the
token first and then list the repositories it opens to tick; until there is a
token (or a command) to list with, the repositories are typed, and the form
says why.

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
gero env choices aws dev role_arn --aws-profile dev   # what that profile's login lists
```

The web and TUI forms pick all of it from what the Mac's AWS CLI lists: the
profile (`aws configure list-profiles`), then with it the role, the instances
and the buckets, looked up again when the profile changes. Roles are the
account's that an AWS principal may assume, each with whom it trusts; a role
in another account, which `iam list-roles` cannot see, is typed. Instances
are swept from every enabled region, with their Name tag; a region the
account is kept out of is left out. A ticked instance or bucket narrows
itself in place (session or command; read, write or delete), which lands in
the session policy as `NAME=CAP`. Whatever the profile may not list
(`iam:ListRoles`, `s3:ListAllMyBuckets` refused) is typed instead, with AWS's
refusal shown; the TUI filters a long list with `/` and narrows the ticked
entry under the cursor with 1, 2, 3.

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

## Mail

Agents write to each other through gero. Every agent has an inbox; a mail
waits there until its agent is idle, then arrives as a turn of its own, one
mail at a time, oldest first. An agent that is in error or was interrupted
keeps its mail until your next message, which always goes before mail.

```sh
gero iso expose dev mail                  # dev's agents write to each other
gero iso expose scratch mail --to dev     # ...and scratch's to dev's too (one way)
gero agent inbox <id>                     # what waits; --rm N / --clear take it out unread
gero agent mail <id>                      # its history: what it was sent, read or not, and wrote
```

In the web, ✉ Mail on an agent's top bar shows the same history, with what
waits counted on it; a mail it read also shows in its conversation, as mail.
Mail stays as history once read or discarded; deleting an agent deletes the
mail it was sent, and what it wrote stays with its recipients.

Inside the isolation, `GERO_MAIL_URL` is each agent's own address: it carries a
token that names the sender, handed to that agent's process alone, and the
`gero-mail` skill says how to use it (`GET $GERO_MAIL_URL/agents`, `POST
$GERO_MAIL_URL/send?to=<id>`). The recipient reads who wrote, from where, and
that it is another agent and not you, ahead of the text.

A mail is an instruction to the agent that reads it, so it crosses isolations
only where you say: an isolation's agents write to their own isolation's, and
to those of the isolations `--to` names (kept by ID: a rename keeps the way
open, a new isolation given the old name gets none), if those take mail. The
tunnel proves which isolation a mail comes from. Within one isolation the
agents run as one user and can read each other's environment, so there the
token names the sender rather than proving it. A mail is at most 32 KiB, an
inbox holds 50, and an isolation's agents send 20 a minute at most. Agents
answering each other can go on forever: the skill tells them not to answer
what needs no answer, and `gero agent inbox <id> --clear` stops a loop.

## Shared folders

A folder of the Mac's can be put in an isolation, at `/shared/<name>`:

```sh
gero env add share notes --path ~/Projects/notes           # read-only
gero env add share out --path ./out --read-write           # a relative path is the CLI's own
gero iso expose dev share notes,out                        # dev restarts with both
gero iso unexpose dev share                                # ...and without them
```

It is a hole in the isolation, kept small:

- **Read-only unless `--read-write`.** The VM's kernel refuses a write to a
  read-only one ("Read-only file system").
- **A writable one is yours.** The VM does not enforce file permissions on
  it: an agent there overwrites a 0600 file, plants an executable git hook,
  deletes what it likes, and on the Mac all of it is your own (measured).
  Whatever runs from that folder on the Mac — git hooks, `.envrc`, scripts,
  a Makefile — is then the agents' to write. A link an agent makes there is
  a real one on the Mac, to the Mac's paths.
- **Only a folder in your home folder**, and never the home folder itself, a
  dot folder in it (`~/.ssh`, `~/.aws`, `~/.config`…), `~/Library`, or gero's
  own directories; links are followed before that is checked, and again
  whenever a container starts with the folder. Desktop, Documents and
  Downloads are refused as well: macOS keeps the container runtime out of
  them and the VM does not start ("Operation not permitted").

`container` mounts a folder only when it starts a VM, so turning a share on
or off for an isolation, or changing an exposed one, replaces its container,
the way a resize does: volumes, sessions and workspaces are kept, running
agents end and start again on their next message. The web and the TUI ask
first. A container that will not start with a new folder is put back with
the ones it had and the change is refused; one recreated later without a
folder that has gone starts without it. Inside, the `gero-share` skill says
which folder of the Mac's each one is, and how to treat it; Claude gets each
one as `--add-dir`, and Codex's auto sandbox may write to the writable ones.

## Verify

```sh
gero iso verify dev          # exit 0 ok / 1 a guarantee is broken / 2 setup only
gero iso verify              # every isolation
gero iso verify dev --json
```

Measures what an isolation can do, now, instead of trusting the
configuration: the fence afresh (every address the Mac holds, from inside),
that no Mac directory is mounted but its shares, each the folder it was
shared as and a read-only one read-only, and for each exposed proxy that the tunnel
carries its ports, that it refuses a request it must refuse — a wrong
workspace token, an unlisted repository — made from inside the isolation as
the agent, with curl or psql, and that each env still answers from the Mac
(`gero env check`). Then that the credentials file is owner-only and the
internet is still reachable. FAIL lines are broken guarantees, WARN lines
setup; a leak found here stops agents like any other measurement. The TUI
has it on `v` in the isolations page, the web on each isolation card.
