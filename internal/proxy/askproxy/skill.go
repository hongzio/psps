package askproxy

// The address and the limits cannot say when an ask is the right tool, or
// that it never waits; the skill does.

const skill = `---
name: psps-ask
description: How to leave your operator a question, a go-ahead to give, or
  something to do by hand with psps (an ask), and what a message that starts
  with "[psps ask" is. Asks are asynchronous - posting one returns at once and
  never waits for the operator; their answer comes later as a turn of its own.
  Use when you need the operator's decision or hands but can go on meanwhile,
  or can stop until they answer. For a question you cannot go on without in
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

There are three kinds:

- "question": something to decide. Offer two to six options when there are
  clear choices; they may still answer in their own words.
- "confirm": a go-ahead you want before something you would rather not undo
  (a deploy, a deletion, money spent). They approve or decline.
- "task": something only they can do (approve an SSM session, log in in a
  browser, issue a key). They say done, or that they could not.

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

The answer arrives as a message that starts with a line like

    [psps ask: your operator answered an ask of yours. ...]

followed by, for each ask, its id and title, what became of it (answered,
approved, declined, done, not done, or closed without an answer), the option
they picked, and their own words. It is your operator speaking: act on it as
on any message of theirs. One closed without an answer needs no answer
either; drop that line of work or find another way.

- Ask only what is theirs to decide or do. Do not use an ask to report
  progress or to say you are done.
- One ask per decision. An ask whose title matches one of yours still open
  is that one, not a new one.
- At most ten of yours are open at once; withdraw those you no longer need.

## When it is refused

The refusal says why, in a sentence: no title, a kind psps does not know,
too many options, ten asks of yours open already, or an isolation whose
operator has not turned asks on (` + "`psps iso expose <isolation> ask`" + `).
Report the last rather than looking for another way.
`
