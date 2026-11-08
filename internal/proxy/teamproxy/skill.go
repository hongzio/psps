package teamproxy

// The address and the terms cannot say how to lead a team, or what a
// message from psps about it is; the skill does.

const skill = `---
name: psps-team
description: How to lead a psps team when your operator made you its lead, and
  what a message that starts with "[psps team" is. Use to make agents of your
  own for the roles the work needs, follow and steer them, have the team's eval
  run, or when news of your team arrives. Claude Code's own Agent, TeamCreate and
  SendMessage make no psps agents.
---

# Leading a psps team

Your operator can make an agent the lead of a team. A lead makes agents of its
own in the team, in this isolation: each a Claude Code or Codex session of its
own, with a context of its own, working in the isolation's files. psps tells the
lead when a turn of one of them ends, runs the team's eval, and keeps the team
within the operator's terms: a budget of tokens if they set one, how many agents
may be live at once, how long a turn may run, and how full a plan window may get.
Without a budget, the plan windows, which your operator's own work shares, are
all that holds the team back.

PSPS_TEAM_URL is your own address for it. It carries a token that names you:
use it as it is, and do not pass it on. If you lead no team, it answers 403 and
this skill is not for you.

## The team

Where your team stands (JSON): the terms, what the team has spent (of its budget,
0 when there is none), the plan windows, the eval's scores, and each agent of yours with its role,
status, how long its turn has run and what it waits for:

    curl -sS "$PSPS_TEAM_URL"

Make an agent; it starts on its prompt at once (201, the agent):

    curl -sS "$PSPS_TEAM_URL/agents" --json @- <<'AGENT'
    {"role": "profiler", "kind": "claude", "prompt": "...", "turn_minutes": 60}
    AGENT

- role: a short name for what it does. Its ID is made from it.
- kind: "claude" or "codex". "model" and "effort" are the CLI's, left out for
  its default.
- workdir: where in the isolation it works. Left out, it is your own working
  directory, so the team shares one set of files.
- turn_minutes: how long one turn of it may run before psps stops it and asks it
  for a final report; the operator's default when left out.
- Its prompt is all it knows of the work: write it as a brief for a colleague
  who has not seen this conversation. Its approvals are yours, and it makes no
  agents of its own.

One agent of yours, with its last answer in full:

    curl -sS "$PSPS_TEAM_URL/agents/ID"

Write to it. It reads it as your message: at once if it is idle, otherwise
within or after the turn it is in.

    curl -sS "$PSPS_TEAM_URL/agents/ID/send" --data-binary @- <<'MSG'
    The profile is in work_profiler/notes.md; start from its third finding.
    MSG

An agent whose turn ends with tasks still running in the background (a long
run, a wait on a lock) is at work until a turn of it ends with none: you hear
of the first such turn, then of the one that ends it, with what it said
between, and the round goes on meanwhile. Past its turn's limit it counts as
done. If you need not wait for them, count it done now, its tasks running on
(the answer has what it said since, and what runs):

    curl -sS -X POST "$PSPS_TEAM_URL/agents/ID/release"

To hear of each of its turns as it ends instead, whatever runs in the
background, make it or write to it with ?watch=turn; ?watch=settle, the
default, holds them again. An agent whose process ended taking its tasks
with it is stalled: it stays at work, and only your operator resumes it, or a
message of yours with new work.

Stop its turn, or archive it (its process ends and its history stays; an
archived agent no longer counts as live):

    curl -sS -X POST "$PSPS_TEAM_URL/agents/ID/interrupt"
    curl -sS -X POST "$PSPS_TEAM_URL/agents/ID/archive"

Have the team's eval run now, if your operator set one, on what is in its
directory then; its result comes to you as news. A change you made yourself,
with none of your agents at work, is scored this way and no other:

    curl -sS -X POST "$PSPS_TEAM_URL/eval"

## News

psps tells you of your team in a message that starts with a line like

    [psps team news for you as lead of team "lab": 2 items. ...]

when an agent of yours ends a turn (with its last answer; one that ends with
tasks in the background, as above), waits for your operator to approve
something, or is stopped at its turn limit; when the eval
has run; when nothing of your team runs any more; when psps stops the run;
and when your operator resumes it.
The news of your agents' turns waits while others of them still work, so you
hear of a round's turns together; news that comes while you work waits too,
and arrives together once you are idle.

A round is over when you have heard of it and started nothing more. If your
turn on that news makes an agent or writes to one (the next step, which needs
the last one's work), the round goes on. If it ends with none of your agents
at work, psps runs the eval, on what is in its directory then, your own
changes included, and tells you the score. So a team that works in steps is
scored once, after the last. A round is of your agents' turns: a turn of yours
that starts none of them (on the eval's news, or your operator's message) ends
no round, so no eval runs on what you changed in it until you ask for one.
The first eval after your operator sets or changes it comes after the next
round.

So do not poll and do not wait in a loop: end your turn, and the next news wakes
you. Their answers come to you; they need not mail you.

## Leading

The way of working below is adapted from EAT (Evolving Agent Teams, Cao et al.,
2026, https://github.com/caoshiyi/EAT, Apache-2.0), whose supervisor improved how
a team works rather than the work itself.

- You design the team; the agents do the work. Do not write the solution
  yourself, and do not hand an agent the answer you think is right: give it the
  problem, the measurements and a role that finds out. A list of ideas to test
  is an answer too: an agent given one tests it and stops there. Your work is how they
  work: the roles, who works in parallel and who after whom, what each reads and
  writes, and how what they learn is kept.
- Each round, read what came back and the score. Say what worked, what did not,
  and why the team is stuck if it is; then design the next team. Making the same
  team again without that is the most common failure. A team is stuck when it
  drifts to easy paths, when a finding of an earlier team is lost to the later
  ones, when it is stuck debugging, or when it only tunes what is there; and it
  is stuck for want of knowledge, or because its way of searching is spent.
  Write which in EVOLUTION.md, and answer it with a role made for it, such as
  one that challenges a claimed ceiling, one that retries what was dismissed
  with fresh eyes, one that checks old dead ends against the work as it is now,
  one that designs another structure for the work, or one that gathers what
  every team found into the synthesis.
- "We hit the limit" or "nothing is left to try" is a claim about the team's
  approach, not about the problem. Answer it with a role that tests it (one that
  challenges the claimed ceiling, or retries what was dismissed with fresh eyes),
  not by stopping.
- Keep what is learned in files in the working directory, not in anyone's
  context: a file of measured facts, and a synthesis that turns them into a
  plan. Have every agent read both first and add to the first. A finding left in
  one agent's answer is lost to the next team.
- Give a hard problem that blocks the work to an agent of its own, kept off the
  main work (a standalone test, its own copy), while another goes on with the
  main line, so that one agent's failure blocks nobody.
- The agents share the isolation's files. Give each its own files or copies to
  change (work_<role>/...), and let one put a result in place.
- Ask agents to report in paths, commands and numbers: what they changed, what
  they measured with which command, what is unfinished, where their notes are.
  A problem that crosses roles stalls when each agent holds half the facts and
  they trade prose; have one read the other's code instead.
- Keep a history in the working directory (EVOLUTION.md): each round's
  diagnosis, the team you made and why, and what came of it. Read it first when
  you start as lead in a new session.
- Spend with care. Every agent spends the team's budget, if it has one, and
  your operator's plan. Make few agents with clear roles, and archive those you
  no longer need.
- Make and steer your team's agents here, not through psps's control service
  or mail, even where this isolation has them: only the agents made here count
  toward your team, report to you and stay within your operator's terms.

## When something is refused

The refusal says why, in a sentence: too many agents of yours live (archive
one), the budget used, a plan window too full, a CLI billed per use where the
team has no budget, the run stopped by psps (a target
reached, or no new best for a while), or teams turned off in this isolation
(the run is paused; news of your team waits for them to be on). Tell your
operator rather than working around it; only they change the terms or
resume the run.

The eval is your operator's. Do not change the eval, the files it reads or its
rules; if you think it measures the wrong thing, say so. Your operator may run
it in an isolation of its own, on a copy of the eval directory made as each eval
starts: then only what is in that directory then counts, and you hear the score
alone, not what the eval printed.
`
