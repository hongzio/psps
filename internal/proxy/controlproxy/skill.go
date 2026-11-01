package controlproxy

import (
	"fmt"
	"strings"
)

// The address cannot say what an env opens, or what comes of using it;
// the skill does, and shows only what the env opens.

// how is each action's usage, as the skill shows it.
var how = map[string]string{
	"agent.ls": `Every agent you reach, archived ones too, and one with its state:

    curl -sS "$GERO_CONTROL_URL/agents"
    curl -sS "$GERO_CONTROL_URL/agents/ID"`,
	"agent.log": `An agent's conversation: its last 50 events, or those after event N:

    curl -sS "$GERO_CONTROL_URL/agents/ID/events?last=50"
    curl -sS "$GERO_CONTROL_URL/agents/ID/events?after=N"`,
	"agent.new": `Make an agent; it starts on its prompt at once (201, the agent):

    curl -sS "$GERO_CONTROL_URL/agents" --json @- <<'AGENT'
    {"kind": "claude", "prompt": "...", "name": "profiler"}
    AGENT

- kind: "claude" or "codex"; prompt: what it starts on. The rest may be left
  out: name; isolation, one you reach (this one when left out); workdir;
  repo, a git URL cloned into its directory; model and effort, the CLI's;
  team, a team's name.
- Its prompt is all it knows of the work: write it as a brief for a colleague
  who has not seen this conversation. It reads that you made it.
- In this isolation it takes approvals as you do; in another, it asks your
  operator for them.
- The agents of this isolation may have %d made this way live at once; archive
  one that is done.`,
	"agent.send": `Write to an agent. It reads it as yours: at once if it is idle, otherwise
within or after the turn it is in.

    curl -sS "$GERO_CONTROL_URL/agents/ID/send" --data-binary @- <<'MSG'
    The profile is in notes/profile.md; start from its third finding.
    MSG`,
	"agent.interrupt": `Stop an agent's turn:

    curl -sS -X POST "$GERO_CONTROL_URL/agents/ID/interrupt"`,
	"agent.archive": `Archive an agent: its process ends and its history stays.

    curl -sS -X POST "$GERO_CONTROL_URL/agents/ID/archive"`,
	"agent.restore": `Restore an archived agent, back where it left off:

    curl -sS -X POST "$GERO_CONTROL_URL/agents/ID/restore"`,
	"agent.fork": `Fork an agent: a new one in its directory, its conversation copied, with no
crons or mail (it counts as one made through this service):

    curl -sS -X POST "$GERO_CONTROL_URL/agents/ID/fork"`,
	"agent.rename": `Rename an agent:

    curl -sS -X PATCH "$GERO_CONTROL_URL/agents/ID" --json '{"name": "profiler"}'`,
	"agent.team": `Put an agent in a team, made if there is none, or take it out with "":

    curl -sS -X PATCH "$GERO_CONTROL_URL/agents/ID" --json '{"team": "lab"}'`,
	"agent.set": `Set an agent's model or effort (its approvals are your operator's):

    curl -sS -X PUT "$GERO_CONTROL_URL/agents/ID/options" --json '{"model": "...", "effort": "high"}'`,
	"agent.cron": `An agent's crons, and crons you set on it: a message sent to it on a schedule
(a cron expression in the Mac's time, "@daily", "@every 2h"), skipped or
waiting if it is busy. It reads that you set it.

    curl -sS "$GERO_CONTROL_URL/agents/ID/crons"
    curl -sS "$GERO_CONTROL_URL/agents/ID/crons" --json '{"schedule": "@every 2h", "prompt": "...", "if_busy": "skip"}'
    curl -sS -X PATCH "$GERO_CONTROL_URL/agents/ID/crons/N" --json '{"enabled": false}'
    curl -sS -X DELETE "$GERO_CONTROL_URL/agents/ID/crons/N"

You change and remove the crons agents set, not your operator's. An agent has
at most 20 set by agents, none coming more often than every five minutes.`,
	"agent.inbox": `An agent's inbox, the mail other agents sent it, and clearing it, all or one:

    curl -sS "$GERO_CONTROL_URL/agents/ID/inbox"
    curl -sS -X DELETE "$GERO_CONTROL_URL/agents/ID/inbox"
    curl -sS -X DELETE "$GERO_CONTROL_URL/agents/ID/inbox/N"`,
	"iso.ls": `The isolations you reach:

    curl -sS "$GERO_CONTROL_URL/isolations"`,
	"iso.services": `Which services an isolation you reach has:

    curl -sS "$GERO_CONTROL_URL/isolations/NAME/services"`,
	"team.ls": `The teams that have an agent you reach, or none:

    curl -sS "$GERO_CONTROL_URL/teams"`,
	"team.show": `A team's run, where you reach its lead: spending, scores, the lead's agents:

    curl -sS "$GERO_CONTROL_URL/teams/TEAM/run"`,
	"team.new": `Make a team:

    curl -sS "$GERO_CONTROL_URL/teams" --json '{"name": "lab"}'`,
	"team.rename": `Rename a team whose agents you all reach:

    curl -sS -X PATCH "$GERO_CONTROL_URL/teams/TEAM" --json '{"name": "lab 2"}'`,
	"team.rm": `Remove a team whose agents you all reach; they stay, in no team:

    curl -sS -X DELETE "$GERO_CONTROL_URL/teams/TEAM"`,
	"team.eval": `Run the eval of a team whose lead you reach; its result goes to the lead:

    curl -sS -X POST "$GERO_CONTROL_URL/teams/TEAM/eval"`,
}

func skillFor(e Env) string {
	var b strings.Builder
	fmt.Fprintf(&b, `---
name: gero-control
description: Do some of what your operator does with the gero command (%s)
  through $GERO_CONTROL_URL. Use when your work needs gero agents or teams
  listed, made, written to or managed. Claude Code's own Agent, TeamCreate and
  SendMessage make no gero agents.
---

# gero's control service

Your operator opened some of what they do with the gero command to the agents
of this isolation. GERO_CONTROL_URL is your own address for it. It carries a
token that names you: use it as it is, and do not pass it on.

What you do through it is done as you: an agent you make, write to or set a
cron on reads that it comes from you, and gero keeps a log of it. You reach the
agents of this isolation and of the isolations your operator named; to you,
others are not there.

What it opens, the isolations you reach, and how many agents made through it
are live:

    curl -sS "$GERO_CONTROL_URL"

An agent is named by its ID (or a prefix of it), a team by its name or ID.

What is not this service's, where this isolation has the other:

- Asking, telling or answering a peer is mail (the gero-mail skill): it waits
  in the inbox and is read when the agent is idle. Writing here is an
  instruction.
- A cron on yourself alone is the gero-cron skill's.
- A team's lead makes and writes to its own agents through its team (the
  gero-team skill): only those count toward the team, report to the lead and
  stay within its terms.
`, orNothing(strings.Join(e.Allow, ", ")))
	for _, a := range Actions {
		if !e.Opens(a.Name) {
			continue
		}
		text := how[a.Name]
		if a.Name == "agent.new" {
			text = fmt.Sprintf(text, e.MaxLive)
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", a.Name, text)
	}
	b.WriteString(`
## When something is refused

The refusal says why, in a sentence: an action your operator did not open, an
agent or isolation you do not reach, as many agents made through it live as
they allow, an approvals mode, which is theirs. Tell your operator rather than
working around it; only they open more.
`)
	return b.String()
}

func orNothing(s string) string {
	if s == "" {
		return "nothing yet"
	}
	return s
}
