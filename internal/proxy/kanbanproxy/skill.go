package kanbanproxy

// The address cannot say what a board is for, or how cards are kept; the
// skill does.

const skill = `---
name: psps-kanban
description: How to read and keep the kanban boards psps gives this
  isolation - cards in To Do, In Progress and Done, with an assignee and
  comments. Use when your operator or another agent mentions a card (ACME-12),
  a board or the kanban, when you take on, finish or hand over a piece of
  work that has a card, and to leave a card for work that should not be
  lost.
---

# Kanban

PSPS_KANBAN_URL is your own address for this isolation's boards. It carries
a token that names you: use it as it is, and do not pass it on. Everything
you change is recorded as yours, in a history your operator reads; nothing
removes a line from it.

A board is a project (psps names it, as an env); an isolation may see
several. Each board has a slug, and a card is known by it and its number
on the board: ACME-12 is the twelfth card on the board whose slug is ACME
(the boards' slugs are in what GET "$PSPS_KANBAN_URL" answers). Name cards
by key, in the address and in what you write. A card moved to another board
is given that board's next number; its old key still finds it. A card is on one board, in one column: "todo" (To Do), "doing"
(In Progress) or "done" (Done). It has a title, a body, maybe an assignee
(an agent, by ID) and comments.

A card's body and comments are markdown, drawn as such where your operator
reads them: headings, lists, **bold**, ` + "`code`" + `, code blocks, tables
and links. A body's steps are best a checklist, one task item a line:

    - [ ] pin the tokenizer
    - [x] rerun tape 3

The card shows how many are checked (1/2), and checking one is a line of its
own in the history.

The boards you see, what you may do on them, and their cards:

    curl -sS "$PSPS_KANBAN_URL"
    curl -sS "$PSPS_KANBAN_URL?board=psps&status=doing"
    curl -sS "$PSPS_KANBAN_URL?mine=1"          # those assigned to you

One card, with its comments and its history:

    curl -sS "$PSPS_KANBAN_URL/ACME-12"

Make one (201, the card; "board" may be left out for the default board,
"status" for todo; "assignee" is an agent's ID, or "me"):

    curl -sS "$PSPS_KANBAN_URL" --json @- <<'CARD'
    {"board": "psps", "title": "Find why BLEU drops on tape 3",
     "body": "Only tape 3 since the tokenizer bump.", "assignee": "me"}
    CARD

Change one: any of "title", "body", "status", "assignee" ("" for none),
"board" (another board you see):

    curl -sS -X PATCH "$PSPS_KANBAN_URL/ACME-12" --json '{"status": "doing", "assignee": "me"}'

Check an item of its checklist, or uncheck it ("item" counts from 0, in the
order they are in the body; "text" is the item as you read it, so a
checklist changed since is refused rather than checked in the wrong place):

    curl -sS "$PSPS_KANBAN_URL/ACME-12/check" --json '{"item": 1, "checked": true, "text": "rerun tape 3"}'

Comment on one, and edit or delete a comment of yours:

    curl -sS "$PSPS_KANBAN_URL/ACME-12/comments" --json '{"text": "Pinned the tokenizer: back to baseline."}'
    curl -sS -X PATCH "$PSPS_KANBAN_URL/comments/7" --json '{"text": "..."}'
    curl -sS -X DELETE "$PSPS_KANBAN_URL/comments/7"

Delete a card (where allowed), and read the boards' history, newest first:

    curl -sS -X DELETE "$PSPS_KANBAN_URL/ACME-12"
    curl -sS "$PSPS_KANBAN_URL/log?board=psps&limit=50"

## Keeping the board true

- Move a card when its work moves: to "doing" when you start it, to
  "done" when it is done (not when you hope it is). A card that waits on
  someone stays where it is, with a comment saying on whom.
- Take a card by assigning it to yourself before you work on it, and do not
  take one assigned to another agent without asking them or your operator.
- Comment what someone picking the card up later needs: what you found,
  what is left, where the work is (a branch, a path). Not every step.
- Before making a card, look for one that already covers the work.
- Your operator reads the board; it is not a way to ask them something.

## When it is refused

The refusal says why, in a sentence: a board this isolation is not given, a
title too long, a comment of another's, or something your operator has not
allowed here (they allow it with ` + "`psps iso expose <isolation> kanban --allow ...`" + `).
Report the last rather than looking for another way.
`
