# psps reference

Everything psps does, and how. The [README](../README.md) is the tour; this
is the manual.

## Install

On an Apple silicon Mac:

```sh
brew trust hongzio/tap   # Homebrew 7 loads no third-party tap it does not trust
brew tap hongzio/tap
brew install psps        # the release's binary; brings Apple's container
```

`brew upgrade psps` is all an upgrade takes: the next psps command moves the
daemon onto the new version, its agents carrying on. The fence helper is a
copy of psps the upgrade leaves as it was, which is fine until a release
needs a helper that behaves differently: then the fence shows `run sudo psps
pf install` (in `psps iso ls`, `psps pf status` and the web) and agents wait
for it. Before
`brew uninstall psps`, run `psps daemon uninstall` and `sudo psps pf uninstall`.

`psps backup FILE` saves what psps keeps on the Mac to one file: a snapshot
of the store (taken while the daemon runs), `config.toml`, the credentials
and their key, your skills, the web token and a bastion's known hosts. It leaves out the
daemon's socket, locks and log, and the isolations' volumes, which container
keeps whatever happens to psps. The file holds the credentials, so it is
the owner's alone (0600). `psps restore FILE` puts it back with no daemon
running (`psps daemon stop` first); over a store that is there it goes only
with `--force`. Names in the file are relative (`store.db`, `config/...`,
`data/...`, `state/...`), so it restores at other paths too, and it then
says which isolations' volumes container no longer has: those start empty.

A release is `sh scripts/release.sh 1.2.3`: it tags main, builds psps, attaches
it to the GitHub release and points the tap's formula (from
[packaging/psps.rb](../packaging/psps.rb)) at it.

## Build

```sh
mise install
mise run build        # cross-compiles the linux shim, embeds it, writes bin/psps
mise run test
```

A build of your own says it is version `dev`; the formula stamps the release's.

### Claude Code and Codex in the image

The isolations' image pins both CLIs ([internal/isolation/image.go](../internal/isolation/image.go)):
the drivers parse what they print, so newer ones are a change of psps's,
checked before an agent runs on them.

1. `sh scripts/cli-check.sh` (npm's latest; or `sh scripts/cli-check.sh 2.1.320 0.170.0`)
   names any method or flag the drivers use that the new versions no longer
   have, and shows Codex's protocol schema diff, Claude Code's `--help` diff
   and both changelogs in between. It spends no tokens and touches no login.
2. Change the two versions in image.go, `mise run build` and restart the
   daemon. Isolations keep the image they have; a new one gets the new image.
3. The live check, in a new isolation (`psps iso create canary`, its CLIs
   logged in), one agent of each kind on its cheapest model, approvals `ask`:
   - a short answer streams in and it ends idle;
   - a shell command asks first: allow one, deny one;
   - Claude, asked to put a question with choices to you first, does, and
     your answer reaches it;
   - a long task (`sleep 300`) stops on `/stop`;
   - after the daemon restarts, it still knows the conversation;
   - rewound to an earlier message, it forgets what came after;
   - `/` lists its commands and models; Codex's `/compact` compacts and `$`
     lists its skills.
4. `psps iso upgrade` each isolation, then `psps iso rm canary --purge`.
   Going back is the old versions in image.go and another upgrade. With the
   shim unchanged since, that is the old image, still there, so nothing is
   built; a new shim makes it a new image, the old versions built with it.
   Keep the old image until the new versions have proven themselves.

## Use

```sh
bin/psps                                  # start the daemon (registers a LaunchAgent)
sudo bin/psps pf install                  # once: the network fence helper (see below)
bin/psps iso create dev                   # first run builds the base image
bin/psps iso login dev codex              # or claude; or from the web / TUI (see below)
bin/psps iso upgrade dev                  # after a psps upgrade: new image, same volumes
bin/psps iso set dev --memory 8G --cpus 6  # resize: a new container, same volumes
bin/psps iso set dev --swap 8G            # a swap file in the VM (0: none)
bin/psps iso rename dev work              # agents keep running
bin/psps iso verify dev                   # measure the fence, the proxies, the mounts
bin/psps agent new --iso dev --kind codex "fix the flaky test"
bin/psps agent new --iso dev --kind claude  # no prompt: it waits for its first message
bin/psps agent new --iso dev --kind codex --id login "fix the login bug"
bin/psps agent ls
bin/psps agent log <id> -f
bin/psps agent send <id> "also update the changelog"
bin/psps agent inbox <id> [--rm N|--clear] # mail other agents sent it (see Mail)
bin/psps agent cron <id> --add "0 9 * * 1-5" "check CI"  # a message on a schedule (see Cron)
bin/psps agent rename <id> login bug      # only the name changes; a turn carries on
bin/psps agent id <id> login              # an ID of your own (--clear gives it back)
bin/psps agent team <id> backend          # its team: a group in the sidebar it moves within;
                                          # a new name makes one (--clear: none; psps team ls|rename|rm;
                                          # a lead that makes agents of its own: see Teams)
bin/psps agent archive <id>               # stops it; its history is kept
bin/psps agent fork <id>                  # live or archived: a new agent, <id>-xxxx, in its
                                          # directory, knowing its conversation; no crons or mail
bin/psps agent restore <id>               # an archived one carries on where it left off,
                                          # under its line's ID; a live session there is archived
bin/psps agent rm <id> [--purge]          # an archived one: gone from psps; --purge also
                                          # removes its workspace and session transcript
```

### TUI

```sh
bin/psps tui
```

Agents on the left (status at a glance; archived ones folded at the bottom,
`r` renames one, `i` gives it an ID of your own, `t` puts it in a team, `d` deletes one;
a team is a group under its name that `enter` folds to its agents' icons, and on its name `r` and `d` rename and delete it,
`l` opens its page, its lead and the terms (see Teams), and `e` runs its eval),
the selected agent's conversation in the middle, the input at the bottom with
the agent's settings behind `ctrl+o`. `ctrl+n` makes an agent (its first message, typed in the form or later in the input, starts it and names it; its directory, a workspace of its own unless you type one, offers the isolation's directories as you type, as the web form does), `ctrl+g`
manages isolations (environment, `s` to rename or resize one, upgrades, `v` to verify one, `l` its accounts
(what Claude and Codex log in with), and with `p` the credential proxies each one serves), `ctrl+r` the
proxies' environments (secrets and
browser logins included), `ctrl+a` answers a pending approval, `f1` lists
every key. Rebind any of them:

```sh
bin/psps tui --keys > ~/.config/psps/keys.toml   # then edit
```

### Web

```sh
bin/psps web          # opens http://127.0.0.1:4376 signed in
```

The same agents, conversations, approvals, isolation settings, accounts
(what Claude and Codex log in with) and credential proxies (the Services page, and each isolation's
services), laid out for a
desktop browser or a phone. Isolations, Services and Accounts are each a list
and one of it: an isolation's overview, its services (a switch each), its
logins, environment variables and settings; a service's environments and the
isolations that have it; an isolation's logins or a shared account. On a phone
or in a narrow window the list is the page and one of it replaces it, with
the way back on top. Each has an address of its own
(`#/isolations/dev/services`), so a page can be bookmarked and the back
gesture goes where it should. In the sidebar an idle agent's grey dot means it
wants a look; tapping it parks the agent (a dimmer grey: it needs none), and
tapping again undoes that. Its next turn takes it out of being parked. An
agent whose turn ended, working to idle, while another was open has a red dot
at the end of its name until it is opened (or parked); the browser keeps it,
and a folded team rings that agent's dot in red. While the open agent works,
three dots in a bubble under its last message say so. The faint smile at
the end of an agent's second line gives it an icon of your choosing, in one
of nine colors (picked lately ones, as they were, on a row of their own:
the browser keeps those); tapping the icon changes or removes it. The daemon
keeps it for the agent's line (`PATCH /v1/agents/ID` with `icon`), so its
`/new` and archived sessions wear it too, and a fork does not. A team
(an agent's Edit, or New agent, names it; a new name makes one) is a group
there that folds to a dot for each of its agents, the ones that want a look
first. A long press moves an agent within its list: a team's agents within
the team only, and the team as one row among the rest. A team holds an
agent's line, so `/new`, a restore and a fork stay in it. The pencil on a
team's name opens its dialog: its name, its lead and the terms (see Teams),
and, once it has a lead, how the run goes, with Run eval now and, when psps
stopped it, Resume. The daemon serves it on loopback only, and
every API call needs the token `psps web` signs you in with (`--rotate`
issues a new one and signs every browser out). To use it from a phone,
expose it to your tailnet yourself — `psps web` prints the exact
`tailscale serve` command and the phone link — so Tailscale provides the
HTTPS certificate and decides which devices can reach it.

For development, `PSPS_HOME=$PWD/.dev` keeps all state under `.dev/` and runs
the daemon as a plain background process instead of touching launchd.

#### Autocompact

Settings (the gear by the sidebar's title) has **Autocompact**, kept in the
daemon. Turned on, it takes a number of minutes (30 at first); neither
counts until **Apply** (Revert, or closing Settings, drops them). Once a
minute psps looks at every live agent, and sends `/compact` to one that is
idle, with nothing on its way to it and nothing in the background, whose
last message is that many minutes old, as if you had typed it. One whose
last request was `/compact` already is left alone until it has another
message, so a quiet agent is compacted once. Agents a team's lead made are
left to their lead, and an agent in an open watch, watched or watching, to
the watch: its `/compact` would be taken for the work watched. That turn does not unpark a parked agent, mark it
unread, or send its next auto todo. No `/compact`, yours or autocompact's,
counts as a message: the sidebar's age of an agent's last message, and the
minutes autocompact waits, are from the one before it. The API is `GET|PUT /v1/autocompact`
(`{"on": true, "minutes": 30}`).

#### Admin mode

Settings (the gear by the sidebar's title) also has **Admin mode**. Turned on, it
asks for a working directory on the Mac (recent ones first, then what is
there as you type: Tab completes, ↑↓ moves) and Claude or Codex, and starts
**Admin**: an agent on the Mac itself, as you, in no isolation and behind no
fence. It sits above every other agent in the sidebar, in a warning color
with a `HOST` tag, and its page says where it runs. It can do whatever you
can, `psps` included, so its approvals start as `default`: the CLI's own
settings on the Mac decide what it asks first, and it asks here. `/approvals`
changes that as for any agent. Its CLI logs in with a shared account from
Accounts, picked in the same dialog (a Claude token or API key; Codex has
none to share), or with its own login on the Mac: the Keychain, or what a
login shell sets, which `~/.zshrc` is not, since it is not read. Another
account alone keeps the conversation and restarts the CLI with it when idle;
the account lists Admin among what uses it. Its process belongs to a shim the daemon
starts on the Mac (`psps host-shim`, in a session of its own, its log under
`~/.local/state/psps/host/`), so it outlives a daemon restart as an
isolation's agents do. Settings shows it while it is on; **Change…** starts
it over in another directory or with the other CLI, or changes its account, and turning the switch
off stops it and archives it with its conversation. It is not forked, and an
archived one is restored only while admin mode is off. The API is
`GET|PUT|DELETE /v1/admin` (`{"kind": "claude", "dir": "~/src/app"}`) and
`GET /v1/admin/dirs?path=`.

### Commands and files in a message

In the web UI, `/` at the start of a message offers commands as you type
(↑↓ picks, Tab completes, Enter completes and sends a whole command, Esc
closes), `@` anywhere offers the agent's workspace files (git's list in a
repository, so what it ignores is left out; a folder opens as you pick it),
and with Codex `$` offers its skills. The TUI does not offer them yet, but
what is typed there works the same.

- psps's own, which psps does instead of sending them, wherever a message is
  typed (`psps agent send` too): `/model <model>`, `/effort <level>`,
  `/approvals <mode>` (alone, each says what it is now), `/stop`, which
  stops the turn, and `/new`, which starts the agent over: a new session
  in the same isolation and directory takes its ID, settings, crons and
  waiting mail, and the old one is archived. The web UI and the TUI go on
  in the new one; an agent's archived sessions fold together under
  Archived, whatever ID they went by. Restoring one (Restore in the web UI,
  `u` in the TUI, `psps agent restore`) goes back to it: its session
  resumes and it takes the ID, crons and waiting mail back from the
  session there now, which is archived. One session of an agent is live at
  a time. Forking one, live or archived (Fork in the web UI, `f` in the
  TUI, `psps agent fork`), makes a new agent instead: its ID is the
  agent's with a random suffix, it is right under the agent in the
  sidebar (in its team, if it has one), it works in the same isolation and
  directory, and its session starts as a copy of the agent's (Claude's
  `--fork-session`, Codex's `thread/fork`), with no crons or mail. Its
  conversation in the web UI and the TUI starts with a copy of the agent's
  too, up to a line saying it was forked; a copied message of yours cannot
  be edited and resent there, and an approval that was still open is shown
  void.
- Claude's: its slash commands and skills, as the running `claude` lists
  them (asked once a process, which costs no turn). Its `/model` and
  `/effort` are left out for psps's, which last beyond the process, and
  `/clear` because it starts a session psps does not follow (`/new` is
  psps's).
- Codex's: its skills, `$name` anywhere in a message, and `/compact`, which
  psps has Codex do; its app-server takes any other slash command for text.

A new agent has no process yet to ask for its models, so New agent's Model
(web UI and TUI) offers the ones its CLI last said it runs in the isolation,
kept each time an agent's process there says them. Before any has, it offers
those said in another isolation, and says which, since a CLI's models differ
by its version and its login; before any has anywhere, there is nothing to
offer. A model it does not offer can still be typed. The API is
`GET /v1/isolations/{name}/models?kind=claude|codex`.

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
bin/psps account add claude --kind claude-token     # paste it
bin/psps iso account dev claude claude              # dev's Claude uses it (- goes back to a login inside)
bin/psps account set claude                         # rotate: idle agents restart with the new one
```

A ChatGPT login cannot be shared that way: its refresh token is single-use,
so copies of one login in several places log each other out, and OpenAI
offers Plus/Pro no long-lived token (Codex access tokens are for Business
and Enterprise workspaces). Codex therefore logs in inside each isolation.
A token that used to sit in an isolation's environment was moved into an
account, once, when the daemon first started with accounts.

**A login inside the isolation.** `psps iso login dev` shows both CLIs as
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
before. A login (or `psps iso logout`) restarts the isolation's idle agents
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
  comes to you as an approval. Edit those files with `psps iso shell`; a
  running agent picks the change up when its mode is set again or it
  restarts.

In auto, ask and default, Claude can also ask you something it needs to
know (its AskUserQuestion): the question and its options come to you, and
you answer by picking options, or in words of your own on the web (the TUI
takes picks by number; `psps agent answer ID APPROVAL ANSWER...` one per
question). Declining tells Claude you would not say. In never Claude has no
one to ask, so it does not.

## How it holds together

- **psps-shim** is each isolation container's main process. It owns the agent
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
- An agent's process lives from turn to turn. psps replaces it, resuming the
  same session, when what it starts with changes: its options, a rewind, or
  its isolation's services, login or CLI. Claude's approvals between ask and
  auto are the exception: the running process is told the new mode and takes
  it from its next tool call, in the turn it is in (a notice says so); one it
  refuses waits for a new process like the rest. That waits for an idle moment, and a
  change of the isolation's waits too until nothing Claude started runs in the
  background (a shell, a subagent), which would end with the process.
- An isolation is an ID to psps (`7f3a9c1b`); its name is what people call
  it. Everything that belongs to it names it by ID: its agents, exposures,
  tokens and accounts in the store, and its container, network and volumes
  (`psps-7f3a9c1b`, `-net`, `-home`, `-state`; `psps iso ls` shows the
  container). So `psps iso rename` changes the name and nothing else, and a
  new isolation never picks up an old one's volumes. Isolations from before
  IDs kept their name as their ID. `container` cannot resize a VM, so
  `psps iso set` replaces the container on the same volumes, as an upgrade
  does: running agents end, sessions and workspaces stay, and a size the new
  container will not start with is put back. `container` has no option for
  swap either: with `--swap`, root makes `/swapfile` in the VM and turns it
  on, each time the container starts and when the swap changes. `swapon`
  needs CAP_SYS_ADMIN, which psps gives only the container of an isolation
  with swap, so its first swap replaces the container; a later one, or none,
  is set in the running VM.
- An agent likewise has a key psps gives it (`77b17c2c`), which its
  workspace, log and history are named after, and an ID it goes by: yours
  (`--id`, `psps agent id`, the TUI's `i`, the web's Name & ID), or else an
  adjective-noun psps picks (`brave-otter`, one of 250,000). The ID names it wherever the key
  does and is shown in its place; no other agent may have it, as theirs or
  as their key, archived ones included, which is checked each time one is
  given. Taking it away (`--clear`, or empty) frees it, and the agent goes
  by its key. The key stays random: a deleted agent's workspace stays
  unless purged, and an ID that came back would find it.

## Network fence

Each isolation gets its own container network, and a small root helper
(`dev.psps.pfd`, installed by `sudo psps pf install`) keeps the host's packet
filter fencing those networks off the Mac and its LAN — IPv4 and IPv6 —
while internet egress stays open. The rules live in the `com.apple/psps`
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
pass or a leak. `psps iso create NAME --fence off` opts an
isolation out; without the helper installed, creating one with the fence on
is refused, since its agents would never start. `psps pf status -v` shows the loaded rules;
`sudo psps pf uninstall` removes everything the helper installed.

Isolations resolve names through their network's gateway, which forwards to
the Mac's resolver. A local DNS proxy on the Mac (Cloudflare WARP answers on
127.0.2.2) never answers it: the image build fails at `apt-get update` and
nothing resolves inside. Name nameservers on the internet instead, in
`~/.config/psps/config.toml`, and restart the daemon:

```toml
dns = ["1.1.1.1", "8.8.8.8"]
```

The image build and containers made from then on use them; an existing
isolation takes them on `psps iso upgrade`.

The fence measurement also tries 1.1.1.1:443 and 8.8.8.8:443, to tell a
resolver that fails from an isolation with no internet at all. That is not
a DNS setting: macOS InternetSharing, the NAT behind every container
network, can wedge while a network is made, leaving the isolation's
gateway unanswered. `psps iso ls` then says "no internet", and
`container system stop && container system start` brings it back,
restarting every isolation (psps recreates them from their volumes; `psps
iso start NAME` for any left stopped). Killing InternetSharing instead
drops every network's bridge.

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
127.0.0.1:3029   mail       PSPS_MAIL_URL, each agent's own (see Mail); PSPS_CRON_URL, PSPS_TEAM_URL
                            and PSPS_CONTROL_URL too (see Cron, Teams, Control)
127.0.0.1:3030   Linear     LINEAR_MCP_URL; every agent gets it as an MCP server
```

The proxies run inside the daemon; nothing of theirs listens on the Mac. Each isolation
has one tunnel — SSH over its `container exec` channel, from `psps-shim
tunnel` — and a connection on it reaches that isolation's proxies only, so the
tunnel is who is asking. Edits take effect on the next request.

An **environment** is defined once and exposed per isolation:

```sh
psps env add git work --allow-repo owner/repo [--read-only]
psps env secret git work                 # its GitHub token (or: --token-command gh)
psps env choices git work repos          # the repositories that token opens
psps env check git work                  # take its credential, ask GitHub who it is
psps iso expose dev git work             # dev now routes owner/repo with that credential
psps iso services dev
psps iso unexpose dev git
psps env rename git work personal        # its secret and every isolation exposing it follow
psps env help git                        # every flag
```

Renaming an env (`psps env rename`, or the name in the web's Edit form)
moves everything that names it at once: its `[service env]` section in the
credentials file, each exposing isolation's list and default, their
workspace tokens (so a planted token keeps working), and for a job runner
its jobs and the team evals that are its jobs. What the isolation sees by
the name changes with it, such as Datadog's `DD_API_KEY_<ENV>`; agents pick
that up once their turn ends. A secret left by a removed env under the new
name is refused rather than taken over: clear it first. A skill cannot be
renamed, since its name is the one its SKILL.md gives; add it again under
the new name.

git with no env (`psps iso expose dev git`) forwards public repositories with
no credential and refuses the rest. Inside, `GET $GITHUB_API_URL/user/repos`
(or `/orgs/OWNER/repos`) lists the repositories the exposed envs allow,
answered by the proxy from the allowlist, so an agent told only "trevia"
finds the owner instead of guessing it. What a service puts in the isolation — its
variables in `/etc/psps/env.sh` (and in every agent's environment), git's URL
rewrite in `/etc/psps/gitconfig`, a skill under `~/.claude/skills/psps-*` and
`~/.codex/skills/psps-*` explaining that a refusal is a decision — is written
by `psps-shim sync` and removed when the service is turned off.

Secrets for services that keep one live in `~/.config/psps/credentials`, an
INI of `[service env]` sections, 0600, read per request:
`psps env secret SERVICE NAME` prompts for them (or reads one line per field
from stdin). Each value is written encrypted (`enc:v1:...`, AES-256-GCM)
with a key kept apart from it, in `~/.local/state/psps/credentials.key`
(0600, made on the first write), so a stray `cat` or a commit of
`~/.config` shows the sections but no secret. The key is 32 raw bytes, not
text, so opening it by mistake shows noise that cannot be copied as it. It keeps nothing from a
process running as you, which can read the key as the daemon does. A value
added by hand in plain text is still read; `psps env ls` and verify warn of
it, and `psps env encrypt` (or any write to the file) encrypts it. git's is a GitHub token; an env may instead name a
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
psps env add aws dev --role-arn arn:aws:iam::123456789012:role/agent --aws-profile dev \
  --allow-instance i-0123456789abcdef0=session --allow-bucket transfer-bucket=read
psps env check aws dev                   # assume the role now, ask STS who it is
psps iso expose dev aws dev
psps env choices aws dev role_arn --aws-profile dev   # what that profile's login lists
```

An allowlist can also name AWS Batch and CloudWatch logs, so an isolation
may run jobs under a role that can do much more, with the session policy
the only thing narrowing it:

```sh
psps env add aws batch --role-arn ... --aws-profile prod --allow-bucket mtlab-work \
  --allow-batch-queue mtlab-eval --allow-batch-queue mtlab-dev \
  --allow-batch-definition mtlab-replay-eval --allow-batch-definition mtlab-replay-dev \
  --allow-log-group /mtlab/replay
```

A job is submitted only to a named queue with a named definition (any of
its revisions), as SubmitJob is authorized against both; queues and
definitions go together, and either alone is refused. What is done to a job
once it is there is not so narrow: CancelJob, TerminateJob and TagResource
(which SubmitJob's tags need) authorize against `job/<id>`, which names no
queue (a tagged SubmitJob also checks TagResource on the job queue and the
job definition, so the named queues and definitions, by name and revision,
can be tagged too), and DescribeJobs, ListJobs and the queue and definition lists
against no resource, so those reach every Batch job in the role's account,
what it runs and its environment included. psps warns of it as the env is
saved. A log group is read with `logs:GetLogEvents` on its streams.

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

The Mac's login expires, and with it every isolation's AWS credentials. An
env can instead name a **bastion**: a small EC2 instance whose instance
role assumes the env's role. AWS rotates an instance role's credentials
itself, so nothing needs a login. psps reaches it over
[tailcat](https://github.com/tailscale/tailcat) (WireGuard with no account
or control plane, `brew install tailcat` on the Mac) and runs the same aws
commands there, the form's lookups included:

```sh
# on the bastion (Amazon Linux, a t4g.nano is plenty; no inbound rule: tailcat dials out)
tailcat genkey --key=default            # prints its tailcat address
tailcat serve --allow=<the Mac's key> no-auth-ssh   # under systemd
# on the Mac
tailcat printpub                                    # the Mac's key, for --allow (none yet: tailcat genkey --client --key=client-default)
psps env add aws dev --bastion <its address> --role-arn arn:aws:iam::123456789012:role/agent \
  --allow-bucket transfer-bucket=read
```

The bastion's role needs `sts:AssumeRole` on the agent roles (which must
trust it) and, for the lookups, `iam:ListRoles`, `ec2:DescribeRegions`,
`ec2:DescribeInstances` and `s3:ListAllMyBuckets`. `--allow` keeps the
tunnel to the Mac's key, so the address alone opens nothing. An env with a
bastion is saved only once the bastion answers as an AWS principal, and
`psps env check` names both identities. psps runs the system ssh with
tailcat as its ProxyCommand, in batch mode, with its own
`~/.local/state/psps/bastion_known_hosts`: the first connection records the
bastion's host key, and a changed one is refused.

An SSM session is a shell: what goes over it is keystrokes, which nothing
shows anyone before they run. **Strict SSM** takes it away from an
isolation's agents and puts you in the way of each command instead:

```sh
psps iso expose dev aws prod --ssm strict       # back with --ssm direct
```

The isolation's credentials then open no SSM at all (sessions, commands,
their outputs), by AWS's own refusal, while S3 stays as the allowlist says.
Credentials it had from before stay valid until they expire (an hour at
most; STS cannot revoke them), but the sessions they opened are ended as
strict mode starts. An agent asks psps instead, at its own address
(`$PSPS_AWS_URL`; the psps-aws skill says how):

```sh
curl -sS "$PSPS_AWS_URL/ssm" --json '{"env": "prod", "instance": "i-...", "command": "tail -n 200 /var/log/app.log"}'
```

The request is an approval in that agent's conversation, with the whole
command, in the web and the TUI (or `psps agent approve AGENT ID
accept|decline|cancel`). Accepted, psps assumes the env's role as
`psps-run-ISOLATION` with that one instance open, runs the command through
AWS-RunShellScript as ssm-user (root where the instance has never had a
session, which it says), waits for it, and the agent's request answers with
its exit code, stdout and stderr (24,000 characters of each, SSM's limit).
Declined, it answers that you said no. A request waits nine minutes for both;
after that the agent fetches the outcome by its ID. The daemon keeps
requests in memory: one asked before a restart is closed when you answer it.
Ending the sessions needs `ssm:DescribeSessions` for the identity the
lookups use (the bastion's role, or the Mac's profile); without it the
sessions stay open until they end, and the daemon's log says why.

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
psps env add pg prod --host db.internal --user app_ro --sslmode verify-full --allow-db app
psps env secret pg prod                  # the role's password
psps env add clickhouse main --host ch.internal
psps env secret clickhouse main          # user and password
```

Datadog, Grafana and Slack are HTTP proxies over an allowlist of endpoint
families, reads by default and each write family a separate choice
(`--allow-api`, `--allow-write`; a write whose reads are not open is dropped
with a warning naming the flags that keep it). Datadog can pin a log-search
scope, Grafana bounds queries to named data sources and saves to named
folders, Slack can deny channels (its `users` family, who the U... ids
in messages are, reads no channel, so the deny list does not touch it). For these, as for ClickHouse, Notion and Linear,
the isolation's token is per env and its value picks the env.

Notion is its hosted MCP server, behind a fixed tool table: a tool the
table does not model does not exist from the isolation, and writes are
families an env opens by name. There is no integration to create; psps logs
in as you:

```sh
psps env add notion work                 # content and search; --allow-api pages ... for more
psps env secret notion work              # prints a URL; approve it in any browser
psps iso expose dev notion work
```

`psps env secret notion` works from a browser on another device too: the
page Notion sends you to afterwards will not load there; paste its address
back into the terminal.

Linear is its hosted MCP server the same way, logged in as you with no API
key. An env reads issues, projects, documents, teams and users; each write
is a family it opens by name: `issue-edits` (create and change issues),
`comments`, `labels`, `project-edits`, `document-edits` and `deletes`.

```sh
psps env add linear work                 # every read, no write
psps env edit linear work --allow-api issues --allow-api projects --allow-api documents \
  --allow-api teams --allow-api users --allow-api issue-edits --allow-api comments
                                         # the list is replaced, so the reads are named too
psps env secret linear work              # prints a URL; approve it in any browser
psps env check linear work               # who the login is, and any tool Linear offers
                                         # that the table does not model (shut until it does)
psps iso expose dev linear work
```

Port 3030 came after the others: an isolation made before it serves Linear
after `psps iso upgrade NAME`. Services that are MCP servers reach agents at launch
— Claude Code through `--mcp-config /etc/psps/mcp.json`, Codex through `-c
mcp_servers.…` — so neither tool's own configuration is touched.

Isolations created before the proxies need `psps iso upgrade NAME` once;
`psps iso ls` says so under PROXIES.

## Agents in psps

Five services give an isolation's agents a part in psps itself. Each is a
different kind of right, turned on apart:

| For agents to… | Turn on | What it is |
|---|---|---|
| ask, tell and answer each other | `mail` | a peer's letter: it waits in the inbox and is read, one a turn, when its agent is idle; both isolations take mail |
| come back to something on a schedule | `cron` | crons an agent sets on itself |
| work as a team toward an eval, within a budget | `team` | the agent you make a team's lead makes agents of its own, hears of their turns and has the eval run, within your terms |
| do some of what you do with the psps command | `control` | the actions its env ticks, on the agents of the isolations it reaches, as itself |
| have a command of yours run elsewhere on their work | `job` | a runner's command run in the runner's isolation on a copy of a directory of theirs; they get what it leaves them |

Each puts a skill in the isolation, and each skill sends what is another's
there: a question to a peer to mail, an instruction to control, a lead's own
agents to its team.

## Mail

Agents write to each other through psps. Every agent has an inbox; a mail
waits there until its agent is idle, then arrives as a turn of its own, one
mail at a time, oldest first. An agent that is in error or was interrupted
keeps its mail until your next message, which always goes before mail.

```sh
psps iso expose dev mail                  # dev's agents write to each other
psps iso expose scratch mail --to dev     # ...and scratch's to dev's too (one way)
psps agent inbox <id>                     # what waits; --rm N / --clear take it out unread
psps agent mail <id>                      # its history: what it was sent, read or not, and wrote
```

In the web, ✉ Mail on an agent's top bar shows the same history, with what
waits counted on it; a mail it read also shows in its conversation, as mail.
Mail stays as history once read or discarded; deleting an agent deletes the
mail it was sent, and what it wrote stays with its recipients.

Inside the isolation, `PSPS_MAIL_URL` is each agent's own address: it carries a
token that names the sender, handed to that agent's process alone, and the
`psps-mail` skill says how to use it (`GET $PSPS_MAIL_URL/agents`, `POST
$PSPS_MAIL_URL/send?to=<id>`). The recipient reads who wrote, from where, and
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
what needs no answer, and `psps agent inbox <id> --clear` stops a loop.

## Cron

An agent can be sent a message of yours on a schedule: a cron expression in
the Mac's time, `@daily` and the like, or `@every 45m`. A run is sent when it
finds the agent idle. One that finds it busy is skipped, or with `--wait`
waits and goes the moment the agent is idle; runs that come due while one
waits are folded into it. As with mail, an agent that is in error or was
interrupted is not idle until your next message, which goes first.

```sh
psps agent cron <id> --add "*/30 9-18 * * 1-5" "check CI; fix what is red"
psps agent cron <id> --add "@every 2h" --wait "write down where you are"
psps agent cron <id>                       # its crons: when next, what came of the last run
psps agent cron <id> --set 1 --schedule @hourly   # or --wait / --skip, or a new message
psps agent cron <id> --off 1               # --on 1, --rm 1
```

In the web, ◷ Cron on an agent's top bar sets the same: every N minutes or
hours, a time on some days of the week, or an expression. The agent reads,
ahead of the message, that it comes on a schedule and that you may not be
watching; the conversation shows it as the cron's. A time missed while the
Mac slept or psps was down comes due once, as soon as it can, not once for
each time missed. An archived agent's crons do not run; deleting it deletes
them.

Agents can set crons of their own, where you turn that on:

```sh
psps iso expose dev cron                   # dev's agents set crons of their own
```

Inside the isolation, `PSPS_CRON_URL` is each agent's own address for its
crons (on mail's port, at its mail address, so no `psps iso upgrade` is
needed), and the `psps-cron` skill says how to use it: `GET` lists its crons,
`POST` sets one (`{"schedule", "prompt", "if_busy"}`), `PATCH`/`DELETE
$PSPS_CRON_URL/<id>` change and remove one. An agent reaches its own crons
only: it sees yours, and changes and removes those it set; you change any.
Its own come as yours do, saying the agent set them; the web marks them "set
by the agent", `psps agent cron` under SET BY. An agent sets at most twenty,
none coming more often than every five minutes.

A cron that comes once (`--once`, the web's Once, `"once": true`) is sent the
first time it comes and then removed: with a schedule that names a time and a
day of a month, it is something at one later time. Its one run always waits if
the agent is busy.

```sh
psps agent cron <id> --add "30 14 3 11 *" --once "deploy the release branch"
```

A cron that compacts first (`--compact`, the web's Compact first, `"compact":
true`) sends the agent `/compact` as each run goes, and its message only once
the agent is idle after it, ahead of mail, todos and other crons, so nothing
comes between the two. A run that comes due while its message waits on the
`/compact` is folded into it. A run due when the agent's last request was
`/compact` already (yours, autocompact's) sends its message straight away.
`--set N --no-compact` turns it off.

```sh
psps agent cron <id> --add @daily --compact "go over yesterday's alerts"
```

## Teams

A team is a named group of agents in the sidebar (`psps agent team <id>
NAME`, an agent's Edit in the web, `t` in the TUI), from any isolations. A
team can have a lead: one of its agents, which makes agents of its own in the
team, in its isolation, for the roles the work needs, follows them, and
designs the next team when they are done. The way of working comes from EAT
(Evolving Agent Teams, [arXiv 2610.36848](https://arxiv.org/abs/2610.36848)),
whose supervisor improves how a team works rather than the work itself; psps
keeps the run going and within your terms.

```sh
psps iso expose dev team                   # dev's agents get a team address
psps team lead lab tara                    # tara leads team lab (it joins it)
psps team lead lab tara --budget 5M        # within 5M tokens
psps team lead lab tara --eval 'make bench | tail -1' --eval-dir ~/work/app --patience 3
psps team lead lab tara --eval-iso evaluator # the eval runs in evaluator, on a copy
psps team lead lab tara --compact-first    # /compact the lead before it hears a round's score
psps team lead lab tara --approver vera    # vera approves each round before its eval
psps team approve lab 'looks sound'        # or: psps team reject lab 'why'
psps agent send tara '/psps-team Build what SPEC.md describes.'   # the work, for the team
psps team show lab                         # spending, scores, the lead's agents
psps team eval lab                         # run the eval now
psps team stop-eval lab                    # stop the one under way (a runner's job); it counts as not run
psps team reset-best lab                   # the evals so far count for no best (the target, the patience)
psps team resume lab                       # go on after psps stopped the run
psps team lead lab --clear                 # no lead; its agents are yours
```

`psps team show` says what the eval is doing now: running (for how long,
and how long the last five took at the median), queued on its runner and
where in line, waiting on the round, or none since nothing changed. A round
that ended with no eval started for a minute is something wrong: the line
says so, as does the web, and the daemon logs it once. The web's team
heads carry the same, a line under the run's, and its team dialog the eval
under way with Stop eval, and the last five.

The same is set in a team's dialog in the web (the pencil on its name, or its
run's line under it) and on its page in the TUI (`l` on its name), which
offer to turn teams on in the lead's isolation if they are off.

Being made lead tells an agent nothing. Give it the work with the skill
named first, `/psps-team <the work>` for Claude or `$psps-team <the work>`
for Codex, so that it reads how to lead before it starts: an agent given
work it could do alone tends to do it alone, and then no round ends and no
eval runs. A session that has read the skill needs no reminder for later
work. `psps team help` has the gist of this section in the terminal, with a
first team to follow.

Inside the isolation, `PSPS_TEAM_URL` is each agent's address for its team
(on mail's port, so no `psps iso upgrade` is needed); only a lead's answers.
The `psps-team` skill says how to use it: `GET` the team, `POST
$PSPS_TEAM_URL/agents` an agent of its own (`{"role", "kind", "prompt",
"model", "effort", "workdir", "turn_minutes"}`), and per agent its last
answer, `send`, `interrupt` and `archive`. An agent a lead makes is a psps
agent like any: in the sidebar under its team, marked as the lead's, with the
lead's approvals and its own session, working in the lead's directory unless
the lead names another. It makes no agents itself. A lead reaches the agents
it made, and no other.

Turning teams off in the lead's isolation (`psps iso unexpose dev team`)
pauses its run: the lead's address answers 403, and psps tells it nothing
and does not ask it for the next team, so it spends no turn on what it
could not act on. Its agents finish their turns and the eval runs. Turned
on again, the lead starts with its address once idle, and what came
meanwhile reaches it as one message. `psps team show`, the web and the TUI
say the run is paused.

psps tells the lead, as a message of its own whenever it is idle:

- when a turn of one of its agents ends, with its last answer, and when one
  waits for you to approve something. A turn that ends with tasks still
  running in the background (Claude's background shells and subagents)
  leaves its agent at work until a turn of it ends with none: the lead hears
  of the first such turn and of the last, with what the agent said between,
  and no round ends meanwhile. The lead may count the agent done at once
  (`release`), or hear of each of its turns (`?watch=turn` as it makes it or
  writes to it); past its turn limit it counts as done, its tasks running
  on. One whose process ended with its tasks still running is stalled: it
  stays at work until you resume it, and psps says nothing of it;
- when a turn runs past its limit (`--turn-minutes`, 60 by default, or the
  lead's for one agent): psps stops it, asks for a final report, and the
  report is the news;
- what the eval scored, once a round is over: with an eval set, the lead
  hears of its agents' turns together, once none of them works, and may go
  on with the round's next step, one that needs the last one's work. When
  its turn on that news ends with none of its agents at work, psps runs the
  eval, on what is there then (the lead's own changes included), and tells
  it the score and that nothing runs. A team that works in steps is scored
  once, after the last. A turn of the lead's own that starts no agent ends
  no round: what it changed alone is scored when it asks for the eval. An
  eval set or changed scores no round that ended before it: its first comes
  after the next round, or when asked;
- that nothing runs any more, once after an eval it or you asked for (a
  round's eval says so itself), if it let its team end without making the
  next (and not while a plan window is past the ceiling, when it could
  make none);
- that psps stopped the run, and that you resumed it.

The eval is a shell command; the last line it prints that reads
`SCORE: <number>` is the score, higher better. It runs for `--eval-minutes`
at most (20), in one of two places:

- In the lead's isolation, as the agents, in `--eval-dir` (the lead's
  directory by default). The agents can change the eval, what it reads and
  what it runs with; a read-only shared folder (see Shared folders) keeps
  its files from them, not its home or its processes.
- In an eval isolation (`--eval-iso`), one with no services but control,
  which the team's agents never reach. For each eval psps ends what the last one
  left running there, copies `--eval-dir` from the lead's isolation to
  `/eval/work`, and runs the eval there as a user of its own (uid 1001, its
  home `/eval/home`, both made anew: `~` in the eval is that empty home, so
  give the evaluator's path in full), who can read but not change what you
  put in the isolation: the agent user's files (`psps iso shell`) and the
  image's. It has the internet, as any isolation does, and not the Mac or
  the LAN. Keep the evaluator and its data there, and take only what is
  scored from the copy: an eval that runs the copy's own benchmark or tests
  is the agents' to change. Before each eval psps also removes whatever
  that user left anywhere it can write, System V IPC included, so nothing
  passes from one eval to the next. The lead hears only the score: what the
  eval printed, which what is scored could fill with the evaluator, is yours
  (`psps team show`, the team's dialog and page). A service you turn on
  there anyway, say aws to record the evaluator's data, warns that the eval is
  refused until it is off again; each refused one shows as `not run` and
  counts for no patience.

Or the eval is a job of one of your runners (`--eval-job RUNNER` in place
of `--eval`; see Jobs): psps copies `--eval-dir` from the lead's isolation to
the runner's and runs the runner's command there, its `meta.json` saying the
caller is `system` and naming the team. The runner's terms are the eval's
(`--eval-minutes` and `--eval-iso` do not apply), the `score` number in its
`result.json` is the score, and the lead hears that alone; the rest of the
result and what it printed are yours. An agent with control cannot change
such an eval.

Either way the eval runs apart from the daemon, in a session of its own,
with its output and exit status kept by root in the isolation
(`/var/lib/psps-eval`, out of the eval user's reach). A daemon that stops
meanwhile, for an upgrade say, ends nothing of it: the next one follows it
on and scores it, also when it ended while no daemon was up. One whose
isolation started again meanwhile (its VM killed or replaced) left nothing
to score: it shows as `not run`, and counts for no patience. One the daemon
stopped before it ran, while it readied the eval isolation or copied the
work, is run again.

An evaluator for an eval isolation, say `evaluator`, lives in its agent
user's home: make it in `psps iso shell evaluator`, where `~` is `/home/agent` (clone it,
or write it there). The eval user reads and runs it and cannot change it.
An agent of yours may work there too, to set the isolation up or read the
results, but it runs as the agent user, the evaluator's owner: what it reads of
what the eval leaves (`/eval`, the output) is the work's to write, so set it
to read only, by hand. Control is the one service it may have there: its
address is in each agent's process alone, which the eval's user cannot
read, so long as the agent does not write it to a file.
Run with `--eval-iso evaluator --eval 'sh /home/agent/evaluator/run.sh'`:

```sh
#!/bin/sh
# /home/agent/evaluator/run.sh. It starts in /eval/work, the copy of --eval-dir;
# $HOME is /eval/home, empty.
set -e
G=/home/agent/evaluator                # yours: the eval reads it, cannot change it
npm ci >"$HOME/build.log" 2>&1         # the work's own build, its output kept apart
pass=0 total=0
for c in "$G"/cases/*.in; do           # your cases, never the copy's
  total=$((total + 1))
  timeout 10 node ./main.js <"$c" >"$HOME/out" 2>/dev/null || continue
  cmp -s "$HOME/out" "${c%.in}.out" && pass=$((pass + 1))
done
echo "SCORE: $((100 * pass / total))"  # last, after all the work printed
```

What it measures with (the cases, the expected output, the benchmark) is
the evaluator's; only what is measured comes from the copy. The work's build
may be the work's own, as it is part of the work. The eval user's `PATH` is
the image's: a tool you installed in the agent's home (mise,
`~/.local/bin`) needs its path in full, and with `~` empty each eval fetches
its caches anew. While the eval runs, the work runs as the same user as the
evaluator, so a work bent on it can still reach what the evaluator writes then
(`$HOME`, `/tmp`) and what it prints; it cannot reach the evaluator's files,
or anything the next eval starts from.

Either way the eval runs the agents' work, which can print a `SCORE:` line
of its own: print the score after the work's output, or keep the two apart.

psps stops the run, and the lead makes no agent and writes to none until you
resume it (`psps team resume`, or setting the lead again), when:

- the budget, if you set one, is used: the tokens sent anew and written
  (reads from the prompt cache not counted) by the lead and its agents since
  it became the lead, as their CLIs count them (`--budget`; `none` takes it
  off);
- the target is reached (`--target`);
- `--patience` evals in a row (3) came without a new best. One psps could
  not run (its isolation unfit, no verified fence, the work not copied)
  scored nothing of the work's and counts for none.

Resuming tells the lead, and asks it for the next team if nothing of its
team works, as it would otherwise wait, told only that the run stopped. The
evals before count no more toward the patience, so a run out of it can always
go on; one stopped by its budget or its target cannot while that still
holds: give it a larger budget, or a higher target or none (`--no-target`),
in the same `psps team lead` (or the dialog), which resumes it.

With `--compact-first` (in the web's dialog, "Compact before a round's
result"; `--compact-first=false` to stop), psps sends the lead `/compact`
once a round's eval has ended, and the round's news, its score with it, the
next time the lead is free, saying it was compacted. A lead whose last
request was `/compact` already (yours, autocompact's) hears it straight away,
with no second one. An eval the lead or you ask for comes as it is.

With `--approver AGENT` (in the web's dialog, "Round approval": Approver,
and Mode for `--approve-mode`), each round is put to AGENT before its eval:
a live agent that is neither the lead nor one of its own, in an isolation
with teams on so it can answer. psps tells it, whenever it is free, what the
lead heard of the round since the last eval, the lead's last answer and the
scores so far. Under `--approve-mode gate` (the
default) the eval waits for its verdict, given with
`POST $PSPS_TEAM_URL/approvals/ID` `{"verdict": "approve"|"reject", "note": ...}`:
an approval runs the eval, the note going to the lead with the score; a
rejection runs none, uses no patience and tells the lead why, and the round
goes to the approver again once a turn of the lead's agents ends after it. An
eval the lead asks for waits the same way; yours does not, and takes the place
of one waiting. There is no deadline, as the Mac may be off for hours: the
round waits, and `psps team show` says for whom and since when. Decide in its
place with `psps team approve TEAM [NOTE]` or `psps team reject TEAM NOTE`, or
the dialog's buttons. Under `--approve-mode notify` the eval runs as ever and
the approver is only told, to look at the round after. Only you set the
approver (`--approver -` takes it off); changing it withdraws what waited for
the one before.

Besides, at most `--max-live` agents of the lead's are live at once (3), and
none is made on a plan once a window of that plan is used past
`--plan-ceiling` percent (90): the plan of the account that kind of CLI runs on
in the isolation, as its agents last said it.

On a subscription the plan windows are the real limit, so a budget is
optional; without one, the ceiling is all that holds the run back, and it
counts the account's whole use, yours on the Mac too, so lower it to keep
some of the week. Without an eval either, psps neither ends the run nor
tells the lead that nothing runs: the team spends for as long as the lead
keeps work going, and stands idle, spending nothing, once it stops. A CLI
billed per use (an API key) has no plan window: where one is logged in, psps
refuses a lead without a budget, and its agents of that kind.

## Control

Some of what you do with the psps command can be opened to an isolation's
agents: an agent that keeps others going lists them, makes them, writes to
them, sets them crons. What is opened is an env, a set of psps commands ticked
one by one (checkboxes in the web's environment form), and an isolation has one:

```sh
psps env add control ops --allow agent.ls,agent.log,agent.new,agent.send --max-live 3
psps iso expose dev control ops              # dev's agents act on dev's agents
psps iso expose dev control ops --reach far  # ...and on far's
psps iso expose dev control ops --approvals auto  # agents they make on far run under auto
psps env edit control ops --allow agent.ls   # replaces the list
psps env help control                        # the actions an env can open
```

The actions: `agent.ls`, `agent.log` (a conversation), `agent.new`,
`agent.send`, `agent.interrupt`, `agent.archive`, `agent.restore`,
`agent.fork`, `agent.rename`, `agent.team`, `agent.set` (model and effort),
`agent.cron`, `agent.inbox`, `iso.ls`, `iso.services`, `iso.exec` (a
command in an isolation), `team.ls`,
`team.show`, `team.new`, `team.rename`, `team.rm`, `team.eval` and
`team.evalset`. An env made without `--allow` opens the reads alone. Nothing
that is credentials or rights is among them, so no env opens it: accounts,
envs and their secrets, services, an isolation's environment and logins,
approvals (an agent's mode, and approving or answering), a lead and its
terms, making, resizing, upgrading and removing isolations, removing agents,
resuming a run psps stopped, the fence and the daemon.

Inside the isolation, `PSPS_CONTROL_URL` is each agent's own address for it
(on mail's port, so no `psps iso upgrade` is needed), and the `psps-control`
skill shows what its env opens, with the request for each: the daemon's own
API paths, under the agent's address. What an agent does through it is done
as that agent:

- **It reaches its own isolation's agents, and those of the isolations
  `--reach` names; to it the others are not there.** Its own isolation's
  agents share its files and services, so acting on them gives it nothing it
  does not have. Reaching another isolation is reaching what that one's
  services open, through its agents: name one only where that is meant.
- **What it makes, writes or sets reads as its own.** A message arrives as
  `[psps control: from agent boss in isolation dev, ...]`, an agent it made
  starts on a prompt that says so, a cron it set says who set it (`psps agent
  cron` under SET BY), and the sidebar marks an agent made this way with an
  arrow. The daemon's log has a line for every change.
- **Approvals stay yours.** An agent it makes in its own isolation takes its
  maker's approvals mode, as a lead's do; one in another isolation asks you,
  unless you gave the exposure `--approvals auto`: then one whose maker runs
  under auto or never runs under auto, with auto's reviewer, and one whose
  maker asks still asks. Nothing gives never that way. An agent cannot set
  a mode, and `agent.set` changes model and effort only.
- **A command it runs, it could have had an agent run.** `iso.exec` runs a
  shell command as the agent user in an isolation it reaches, for a minute
  (ten at most), and gives back the end of what it printed and its exit code;
  only where an agent it made would run under auto: in its own isolation
  when the caller runs under auto or never, in another when you also gave
  the exposure `--approvals auto`. Reaching an isolation this way is reaching
  its files and services, an eval isolation's evaluator and data among them.
  The script can be the request's body itself, as text, with `dir` and
  `seconds` in the query, so it needs no quoting or encoding; then what it
  printed comes back as text, ending in `[psps: exit N]` where N is not 0.
  `{"command": ...}` as JSON still works and still answers JSON. `team.show`
  with `?format=text` answers as `psps team show` prints it.
- **It hears what came of what it writes.** What it sends (`agent.send`)
  or makes an agent on (`agent.new`) is watched, unless it says
  `?watch=none`: once the agent has finished with it (its turn ended, and it
  runs nothing in the background), stopped, or waits for your approval,
  psps tells the sender so, with the agent's last answer, as a message of
  its own once the sender is idle, as a lead hears its team's news. So it
  waits without polling, and its waiting outlives its process. A turn that
  ends with tasks in the background is told once; `?watch=turn` tells at the
  end of the turn that reads it; `&minutes=N` also tells once if the agent
  is still at work after N minutes of psps running. Nothing is resumed for
  it: an agent whose process ended with its background tasks is stalled
  until you resume it, and one stopped is yours to resume too. The web shows
  who waits on whom on both agents' top bars, and drops a watch for you. An
  agent has at most 20 open.
- **It has few made at once, and writes within mail's budget.** `--max-live`
  (3) agents made through it (`agent.new`, `agent.fork`, restoring one) by
  the isolation's agents are live at once, and what they write through it
  spends the isolation's mail budget, 20 a minute, so two agents writing to
  each other wind down either way.
- **Crons it changes are the agents'.** It sets crons on the agents it
  reaches, at most twenty of the agents' each and none more often than every
  five minutes, and changes and removes those agents set, never yours.
- **A team is its to change only whole.** It renames or removes a team only
  when it reaches all its agents, and sees a run or runs an eval where it
  reaches the lead.
- **An eval it sets runs where it could run commands.** `team.evalset` sets
  a team's eval command, its limit and its eval isolation, where it reaches
  the lead, on a copy of the lead's working directory or one in it: never in
  the lead's isolation, where an eval runs as the agents, and on nothing else
  of it. The eval isolation is the setting agent's own, or one it reaches
  where `iso.exec` would run (`--approvals auto`, and the agent under auto or
  never); left out, it stays where it is, or is the agent's own when the
  eval ran in the lead's. The rest of the run's
  terms stay yours, a stopped run stays stopped, and `psps team show`, the
  team's dialog and page say which agent set the eval, until you change it.
  The `psps-control` skill says what an eval is wherever its env touches
  one.

Turning control off for an isolation, or editing its env, takes effect on the
next request; its agents' processes start again when idle, with the address
and the skill or without them.

## Jobs

A runner is a command of yours that runs in an isolation of its own. A job
runs it there on a copy of a directory from another isolation, for a caller:
an agent, through its job address; psps itself, for a team's eval on the
runner (`--eval-job`); or you. psps says who asked in a file the command can
trust, passes the caller's options on unread, and gives the caller what the
command left for it, nothing more. What it is for is the command's: a
simulation, a benchmark, a build in a clean room.

```sh
psps env add job sim --isolation sim --command '/home/agent/sim/run'
psps env edit job sim --minutes 120 --memory 8G --max-live 3 --max-mb 1000
psps env edit job sim --about 'Replays the strategy in the copy on market tapes, on AWS Batch. Options: {"mode":"quick"|"full"}. Result: {"pnl":…,"fills":…}'
psps iso expose dev job sim          # dev's agents may ask for jobs on sim
psps job run sim --from dev --options '{"mode":"quick"}' --wait /home/agent/work/x
psps job ls                          # every job, newest first, kept 14 days
psps job show ID                     # all of it: result.json, what it printed
psps job cancel ID
psps job help                        # what the command gets and leaves
```

A runner's terms: `--isolation` (one), `--command` (a shell command),
`--minutes` (60), `--memory` (4G, no swap) and `--pids` (512) for all the job
starts, `--max-live` (2) jobs at once with `--max-queued` (20) more waiting,
psps's own first and never refused, and the copy's limits, `--max-mb` (500)
and `--max-files` (50000), and the names it leaves out wherever they are,
`--exclude` (`.git`). `--about` (at most 4 KiB, `""` clears it) is what the
agents it is open to read of it in their listing: what it does, the options it
takes, what its result holds. They never see its command, so without one a
runner is a name and its limits to them, and an agent that wants what it does
(a simulator with access its own isolation lacks) may not know it is there.

For each job psps readies `/var/lib/psps-job/ID` in the runner's isolation,
root's, which the agent user may pass through but not list or change, and a
cgroup of the job's own with its memory and process limits; copies the
directory there (`work/`), as the agent user reads it on one side and writes
it on the other, through the daemon and nowhere else on the Mac; and runs the
command there as the agent user, with the isolation's own environment (its
services' addresses and `psps iso env`) and nothing of root's:

| Variable | What it is |
|---|---|
| `PSPS_JOB_WORK` | the copy, the working directory: the job's alone |
| `PSPS_JOB_META` | who asked, written by psps, root's and read-only: `v`, `job`, `runner`, `caller` (`agent`, `system` or `operator`), `agent` (`id`, `handle`, `name`) or null, `isolation` (where the directory is), `team` (`id`, `name`) or null, `dir`, `why`, `limits` (`memory`, `pids`, `minutes`) or null, `requested_at`, `started_at`, `deadline` |
| `PSPS_JOB_OPTIONS` | the caller's options, a JSON object of at most 16 KiB that psps does not read |
| `PSPS_JOB_OUT` | where the command leaves `result.json` |
| `PSPS_JOB_ID`, `PSPS_JOB_RUNNER`, `PSPS_JOB_DEADLINE` | as named |

The copy and the options are the caller's to write. What the command trusts
of who asked is `meta.json`: check that it is root's, under
`/var/lib/psps-job/`, and that its `job` is `PSPS_JOB_ID`, and decide from
its `caller` what the job may do. A copy has files, directories and symbolic
links, links as links and never followed; devices, FIFOs and sockets are
left out, and one over the limits is refused before anything runs.
Where the isolation's cgroup cannot give a job a cgroup of its own with
controllers (Apple's container cannot: the container's cgroup holds
processes, and takes none into a child), the job runs without the memory and
pids limits, the VM's memory being the bound, and its own session holds it
together: `meta.json`'s `limits` is null then, and the job's note says so.
`timeout(1)` sends SIGTERM at `--minutes` and SIGKILL two minutes later; a
job cancelled is sent SIGTERM at once and ended two minutes later; whatever
the command leaves running ends with it, and its directory is removed.

`result.json`, a JSON object of at most 1 MiB, is kept whole for you, with
the end of what the command printed. Its `caller` is all the agent that asked
gets of it, and for a team's eval its `score` is all the lead hears. The job
is `done` when the command exits 0, `failed` otherwise (or when psps could not
run it: the error says which), `cancelled`, or `lost` when its isolation
started again under it. A job runs apart from the daemon: one that stops
meanwhile ends nothing, and the next follows it to its end.

Inside an isolation with the service, `PSPS_JOB_URL` is each agent's own
address for it, and the `psps-job` skill says how to use it: `GET` lists the
runners open to it, `POST /jobs` asks for one
(`{"runner", "dir", "options"}`, 202), `GET /jobs/ID?wait=120` follows one,
and `DELETE /jobs/ID` cancels it. An agent sees the jobs asked for from its
own isolation, never psps's or yours, and only the runners its exposure names
(`psps iso expose dev job sim,quick --default-env quick`).

## Shared folders

A folder of the Mac's can be put in an isolation, at `/shared/<name>`:

```sh
psps env add share notes --path ~/Projects/notes           # read-only
psps env add share out --path ./out --read-write           # a relative path is the CLI's own
psps iso expose dev share notes,out                        # dev restarts with both
psps iso unexpose dev share                                # ...and without them
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
  dot folder in it (`~/.ssh`, `~/.aws`, `~/.config`…), `~/Library`, or psps's
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
folder that has gone starts without it. Inside, the `psps-share` skill says
which folder of the Mac's each one is, and how to treat it; Claude gets each
one as `--add-dir`, and Codex's auto sandbox may write to the writable ones.

## Skills

A skill of your own — a folder with `SKILL.md` at its top, and whatever
scripts, references and assets it ships — can be put in an isolation, for
Claude Code and Codex alike:

```sh
psps env add skill pdf --path ~/skills/pdf      # checked, then copied to ~/.config/psps/skills/pdf
psps iso expose dev skill pdf,xlsx              # in dev's agents' skills, at once
psps env edit skill pdf --path ~/skills/pdf     # copied in again after you changed it
psps env edit skill pdf                         # ...or check the kept one after editing it there
psps env check skill pdf                        # what is kept, and anything worth fixing
psps env rm skill pdf                           # gone from the Mac and every isolation
```

Or fetch one from a public GitHub repository, as `npx skills add` does:

```sh
psps env add skill pdf --github anthropics/skills          # the skill called pdf in it, wherever it is
psps env add skill pdf --github anthropics/skills/skills/pdf   # ...or its folder, named
psps env add skill pdf --github https://github.com/anthropics/skills/tree/main/skills/pdf
psps env add skill pdf --github anthropics/skills --ref v1.2.0   # a tag or commit stays put
psps env choices skill pdf github --github anthropics/skills     # the skills it has
psps env check skill pdf                       # is the repository ahead of what is kept?
psps env edit skill pdf --update               # take what its ref has now
psps env edit skill pdf --ref main             # follow another ref, fetched now
```

Without a folder, a repository is searched for the skill whose `name` is
NAME: its top folder, `skills/`, `.claude/skills`, `.codex/skills`,
`.agents/skills`, then up to 4 folders down (`node_modules` aside); a skill
there not by that name lists the names it has. The daemon fetches it over
HTTPS (the GitHub API's tarball, no `git` needed), checks it as it would a
folder, links refused, and keeps it in the same place; the isolation never
reaches GitHub for it. Only public repositories, and GitHub's limit of 60
unauthenticated requests an hour from the Mac applies.

The env keeps where it came from: the repository, the ref it follows, the
folder, the commit it was fetched at and that folder's git tree. Nothing is
updated by itself, since a skill's scripts run as the agent: `psps env check`
(and the web, about once a day while it lists skills) compares the folder's
tree at what the ref names now, so a commit elsewhere in the repository is
no update, and lists the files an update changes. `--update` fetches and
checks it, and a skill that no longer passes is left as it was. A kept copy
edited on the Mac since it was fetched is not replaced without `--force`.
A ref that is a commit is pinned: there is nothing to follow. `--path`
copies a folder in again and forgets the GitHub source.

psps keeps its own copy in `~/.config/psps/skills/NAME`; the folder it came
from can move or change without touching what agents have. Both CLIs drop a
skill they cannot read without telling the agent, so a skill is checked as it
is added, and refused rather than kept broken:

- **`SKILL.md`**, named so, UTF-8, starting with a `---` line, its frontmatter
  closed by another, and YAML both CLIs can read (a value with `: ` or ` #`
  in it needs quotes).
- **`name`** is the skill's name as it is added: lowercase letters, digits
  and single hyphens, up to 64. Names starting with `psps` are psps's own.
- **`description`**, not empty, up to 1024 characters: how an agent knows
  when to use it.
- **Claude Code's fields** have the type it reads (`user-invocable: true`,
  `context: fork`, `metadata` of names to text…); one neither CLI reads is a
  warning, as is a link in `SKILL.md` to a file the skill does not have, or a
  `SKILL.md` past 500 lines.
- **Its files and nothing else**: a link (symlink) is refused rather than
  followed, so nothing else of the Mac's rides along; `.git` and `.DS_Store`
  are left out. Up to 256 files, 1 MiB each, 4 MiB in all. Nothing is taken
  from psps's own directories.

In the isolation each skill is at `~/.claude/skills/psps-skill-NAME` and
`~/.codex/skills/psps-skill-NAME`; both CLIs know it by its `name`. Executable
scripts stay executable, and the agent owns its copy. Ticked, unticked,
removed or copied in again, the isolation follows at once, and a file the new
copy no longer has goes too; an edit made in `~/.config/psps/skills` reaches it
with `psps env edit skill NAME`. Claude Code notices a changed skill as it
runs; Codex reads them as a session starts. Isolations made before skills need `psps iso upgrade
NAME` once for that, and for files that are not text; until then they get the
text files, and lose a skill only when the service is turned off.

A skill runs as the agent: its scripts are yours to trust, as you would a
package.

## Forwarded ports

A port of the isolation's — a dev server, a WAS an agent runs — can be put on
the Mac's `127.0.0.1`:

```sh
psps env add port web --port 5173                          # http://web.localhost:5173 on the Mac
psps env add port api --port 8080 --host-port 18080        # ...or another port of the Mac's
psps env add port db --port 5432 --mode tcp                # bytes unchanged, for what is not HTTP
psps iso expose dev port web,api
```

The daemon listens on the Mac and takes each connection through the
isolation's tunnel to its own loopback, so a server there that listens on
localhost only is reached too. Turning a port on or off changes only the
Mac's listener; the container stays. A port another isolation already has,
or something on the Mac already listens on (on any address), is refused
rather than shared. Isolations made before forwarded ports need `psps iso
upgrade NAME` once; the browser is told so.

It opens the isolation toward the Mac, kept small:

- **The Mac's loopback only**, never the LAN. The isolation still cannot
  connect to the Mac (the fence is measured as before): a connection only
  ever starts on the Mac.
- **Its cookies are its own.** A browser keeps cookies by name, not port:
  127.0.0.1's go to every port of it, psps's web session (the web token)
  among them. So an http port is given out under a name of its own,
  `http://<env>.localhost:<port>` (browsers and macOS resolve any name under
  localhost to the loopback; Vite, Django and Rails accept it as it is),
  where the browser sends it no other program's cookies and keeps what it
  sets from them; a cookie it sets loses any `Domain`. `127.0.0.1` and
  `localhost` still work, for a login whose callback is registered there;
  there the other cookies of that name reach it, all but psps's own. In
  `http` mode, the default, the daemon is an HTTP proxy that takes psps's
  cookie out of every request and out of every answer; WebSockets (a dev
  server's live reload) pass. `tcp` mode passes bytes unchanged: a
  browser pointed at a tcp port hands the isolation your session, and psps
  warns when one is defined. An http port answers only to `127.0.0.1`,
  `localhost` and `*.localhost`, so a web page elsewhere that points a name
  of its own at 127.0.0.1 (DNS rebinding) cannot read it through your
  browser.
- **What an agent serves runs in your browser**, as a page from 127.0.0.1.
  psps's own API refuses it (it needs a header a cross-origin page cannot
  send), but other things on the Mac may not; the `psps-port` skill tells
  agents which ports the Mac reaches and what not to serve. The web links
  each http port, opened with no hold on the psps tab.

## Kill

```sh
psps iso kill sim            # SIGKILL its VM, past container (the web's Kill)
```

A VM so short of memory that nothing starts in it holds up container's every
`exec`, in every isolation, and its `stop` and `kill` with them. `psps iso
kill` goes past container: it SIGKILLs the container's runtime helper and the
process its VM runs in (the one with its root disk open), as unplugging it
would. Its agents are interrupted, what its disks had not written is lost, and
it starts again when something next needs it: within a minute when it has
live agents, as they reconnect; otherwise at the next agent made in it, or
`psps iso start sim` (the web's Start).

A resize, an upgrade, a change of shared folders or `psps iso rm` replaces or
removes the container, which ends its agents anyway: on a VM that does not
finish a command in 5 s, psps kills it this way first, as `container rm`
would wait on it for good. While a VM does not answer, its agents wait for
it together, for at most a minute, and none holds the isolation up
meanwhile; one just seen not to finish a command is reported at once.

## Verify

```sh
psps iso verify dev          # exit 0 ok / 1 a guarantee is broken / 2 setup only
psps iso verify              # every isolation
psps iso verify dev --json
```

Measures what an isolation can do, now, instead of trusting the
configuration: the fence afresh (every address the Mac holds, from inside),
that no Mac directory is mounted but its shares, each the folder it was
shared as and a read-only one read-only, that the Mac listens for each forwarded
port on its loopback alone and the tunnel takes it in, and for each exposed
proxy that the tunnel carries its ports, that it refuses a request it must refuse — a wrong
workspace token, an unlisted repository — made from inside the isolation as
the agent, with curl or psql, and that each env still answers from the Mac
(`psps env check`). Then that the credentials file is owner-only and its
values encrypted, and the internet is still reachable. FAIL lines are broken guarantees, WARN lines
setup; a leak found here stops agents like any other measurement. The TUI
has it on `v` in the isolations page, the web as Verify on an isolation's page.

## Coming from gero

psps was called gero. `psps restore` takes a `gero backup` as it is, and an
isolation made then keeps gero's names, so nothing of it moves: its
container, network and volumes (`gero-7f3a9c1b`...), and inside it
`/var/lib/gero` and `/var/lib/gero-eval`. Its agents get each `PSPS_*`
variable as `GERO_*` too, for what they kept from then. While its image is
from then (until `psps iso upgrade`), its shim is
`/usr/local/bin/gero-shim`, its files are in `/etc/gero` (with `/etc/psps`
leading there) and its skills are `gero-*`; after, they are psps's, and the
`gero-*` skills are removed. Moving over: `gero backup FILE`,
`gero daemon uninstall`, `sudo gero pf uninstall`, then `psps restore FILE`,
`sudo psps pf install` and `psps`.
