package cronproxy

// The address and the limits cannot say when a cron is the right tool, or
// what its message should hold; the skill does.

const skill = `---
name: gero-cron
description: How to schedule a message to yourself with gero (a cron), and
  what a message that starts with "[gero cron" is. Use when asked to do
  something on a schedule, every so often, later at a set time, or to keep
  checking on something (CI, a deploy, a long job), or to see, change or stop
  such a schedule. Claude Code's own CronCreate, ScheduleWakeup and /loop do
  not outlive this process; this does.
---

# Crons of your own

A cron is a message gero sends you on a schedule, as a turn of its own, when
you are idle. Your operator can set them too; you can set your own through
GERO_CRON_URL, your own address at gero. It carries a token that names you:
use it as it is, and do not pass it on.

A cron's message starts with a line like

    [gero cron #3, on schedule "@every 1h". You set this message ...]

and the rest is what was written when it was set. Whoever reads it then is
you, without this conversation's context, so write the message the way you
would brief a colleague: what to do, where, and when to stop.

Your crons (JSON: the Mac's time zone, and each cron's id, schedule, prompt,
if_busy, enabled, next_at, last_result, and self, true for those you set):

    curl -sS "$GERO_CRON_URL"

Set one:

    curl -sS "$GERO_CRON_URL" --data-binary @- <<'JSON'
    {"schedule": "*/30 9-18 * * 1-5",
     "prompt": "Check CI on main in ~/work/app (gh run list -b main -L 3). If it is red, find out why and fix it. When it has been green three times in a row, stop this cron.",
     "if_busy": "skip"}
    JSON

- schedule is a cron expression in the Mac's time (minute hour day month
  weekday), one of @hourly @daily @weekly @monthly, or "@every 45m" / "@every 2h".
  It may come at most every five minutes.
- if_busy says what a run does when you are busy as it comes: "skip" (the
  default) lets it go; "wait" sends it as soon as you are idle, runs that come
  meanwhile folded into it.

Change one you set (any of schedule, prompt, if_busy, enabled), or remove it:

    curl -sS -X PATCH "$GERO_CRON_URL/3" --data-binary '{"enabled": false}'
    curl -sS -X DELETE "$GERO_CRON_URL/3"

- Set a cron only when you were asked to, or the work plainly needs coming
  back to. Every run costs a turn, and runs go on while nobody watches.
- Stop a cron yourself when its work is done; a cron for a single later time
  removes itself in its message ("... then DELETE $GERO_CRON_URL/<its id>").
- The crons your operator set are theirs: you see them, and only they change
  or remove them.

## When it is refused

The refusal says why, in a sentence: a schedule gero does not read or that
comes more often than every five minutes, twenty crons of your own already,
a cron of your operator's, or an isolation whose operator has not turned crons
on (` + "`gero iso expose <isolation> cron`" + `). Report the last rather than
looking for another way.
`
