package askproxy

// The address and the limits cannot say when an ask is the right tool, or
// that it never waits; the skill does.

const skill = `---
name: psps-ask
description: How to leave your operator a question, a go-ahead to give,
  something to do by hand, or a file to move from one isolation to another
  (a transfer) with psps (an ask), and what a message that starts
  with "[psps ask" is. Asks are asynchronous - posting one returns at once and
  never waits for the operator; their answer comes later as a turn of its own.
  Use when you need the operator's decision or hands but can go on meanwhile,
  or a file moved between isolations (never by any other way),
  or can stop until they answer, and to add to an ask of yours still open. For a question you cannot go on without in
  this turn, use AskUserQuestion instead.
---

# Asks

Ask is asynchronous: posting returns at once and never waits for the
operator. Keep working on what does not need the answer; it arrives later as
a turn of its own, when you are idle, however long it takes them. For a
question you cannot go on without in this very turn, use AskUserQuestion
instead (where your operator watches for it).

PSPS_ASK_URL is your own address at psps. It carries a token that names
you: use it as it is, and do not pass it on.

There are four kinds:

- "question": something to decide. Offer two to six options when there are
  clear choices; they may still answer in their own words.
- "confirm": a go-ahead you want before something you would rather not undo
  (a deploy, a deletion, money spent). They approve or decline.
- "task": something only they can do (approve an SSM session, log in in a
  browser, issue a key). They say done, or that they could not.
- "transfer": a file to move from one isolation to another (yours or
  not). Once they approve, psps copies it; see below.

Post one (title: one line, what to decide; body: what they need to decide
it, as you would brief a colleague who has not followed your work):

    curl -sS "$PSPS_ASK_URL" --data-binary @- <<'JSON'
    {"kind": "question",
     "title": "How long should the log bucket keep logs?",
     "body": "S3 log storage went from $40 to $120 this month. Shorter retention brings it down at once.",
     "options": [{"label": "30 days", "note": "about $45 a month"},
                 {"label": "90 days", "note": "about $110 a month"}]}
    JSON

It answers with the ask's id at once. Then go on: do what does not depend on
the answer, and end your turn when nothing is left. Do not poll for the
answer, sleep, or loop waiting for it.

Your asks (newest first: id, kind, title, state, and once answered, what
they said):

    curl -sS "$PSPS_ASK_URL"
    curl -sS "$PSPS_ASK_URL/42"

Withdraw one you no longer need, while it is open (they are not told):

    curl -sS -X DELETE "$PSPS_ASK_URL/42"

## Moving a file between isolations

Isolations share nothing. To have a file moved from one to another, ask
for a transfer: name the isolation it is in, its path there, and the
isolation it goes to. It lands in that isolation's /home/agent/inbox, under
its own name or the "name" you give; never over a file there.

    curl -sS "$PSPS_ASK_URL" --data-binary @- <<'JSON'
    {"kind": "transfer",
     "title": "Move the sim bundle to eval for the runner",
     "body": "What is in it, and why the other side needs it.",
     "transfer": {"from": "lab", "path": "/home/agent/work/out/nexus.bundle",
                  "to": "eval", "sha256": "cc0cb1f7..."}}
    JSON

- The file is a regular file under /home/agent, named by its real path: a
  link, or a path through one, is refused. At most 500 MiB.
- psps reads its size and sha256 when you ask, and shows them to your
  operator; "sha256" is optional, and refused if it differs. When they
  approve, psps copies it and checks it against what they saw: a file
  changed since is not copied, and the ask stays open. Do not change it
  meanwhile.
- Only along a route your operator allowed (from one isolation to the
  other); the refusal says when there is none. Do not look for another way.
- The body is what they decide on: say what is in the file and why the
  other side needs it.
- The answer says where it landed. The agents in the other isolation are
  not told: tell them yourself, if they are to use it.

## An ask is a thread while it is open

Your operator may reply without closing the ask: to ask you something
back, or to say what they want to see before they decide. Their reply
arrives like an answer (below), marked "still open, your turn". Then add
to the same ask, never a new one: answer them, or post what you found
since (an interim result, a simulation, a narrower choice). For a question
you may offer new options; they replace the old ones.

    curl -sS "$PSPS_ASK_URL/42" --data-binary @- <<'JSON'
    {"text": "Audit logs are under audit/, 8% of the bytes. A rule can keep them a year and the rest 30 days.",
     "options": [{"label": "30 days, audit logs a year apart", "note": "about $52 a month"},
                 {"label": "60 days for all", "note": "about $80 a month"}]}
    JSON

The ask then waits on them again: go on, and end your turn when nothing is
left, as when you posted it. You may also add to an ask still waiting on
them, when you learn something that changes the decision; not to report
progress. If their reply settles it, so that no answer is needed any more,
resolve it (with a last word or without):

    curl -sS "$PSPS_ASK_URL/42" --json '{"resolve": true, "text": "Went with 30 days as you said."}'

## Their answer

The answer arrives as a message that starts with a line like

    [psps ask: your operator answered an ask of yours. ...]

(or "replied to an ask of yours and kept it open"), followed by, for each
ask, its id and title, what became of it (answered, approved, declined,
done, not done, closed without an answer, or still open, your turn), their
replies, the option they picked, and their own words. It is your operator
speaking: act on it as on any message of theirs. One closed without an
answer needs no answer either; drop that line of work or find another way.

- Ask only what is theirs to decide or do. Do not use an ask to report
  progress or to say you are done.
- One ask per decision. An ask whose title matches one of yours still open
  is that one, not a new one.
- At most ten of yours are open at once; withdraw those you no longer need.
- One ask holds at most forty replies and additions, theirs and yours.

## When it is refused

The refusal says why, in a sentence: no title, a kind psps does not know,
a transfer with no route or whose file psps cannot read,
too many options, ten asks of yours open already, an ask that is closed
already (leave a new one if you still need them), or an isolation whose
operator has not turned asks on (` + "`psps iso expose <isolation> ask`" + `).
Report the last rather than looking for another way.
`
