package jobproxy

// The address cannot say what a runner is, or what comes back from one;
// the skill does.

const skill = `---
name: psps-job
description: How to have a psps job run, a runner's command set by your
  operator, run in another isolation on a copy of a directory of yours, its
  result coming back to you. Use when you are told to run something through a
  runner or a job, or to see how a job you asked for went.
---

# psps jobs

Your operator can give this isolation runners. A runner is a command of theirs
that runs in an isolation of its own. You ask for a job: psps copies a
directory of yours there, runs the runner's command on the copy with the
options you give it, and keeps the part of its result the command left for you.
Your files here are not changed; the copy is the job's alone and is removed when
the job ends.

PSPS_JOB_URL is your own address for it. It carries a token that names you:
use it as it is, and do not pass it on.

## Runners

The runners open to this isolation, with their terms (how long a job may run,
how big a copy may be, what it leaves out), and the jobs asked for here:

    curl -sS "$PSPS_JOB_URL"

A runner's "about", where it has one, is your operator's word on it: what it
does, the options it takes and what its result holds. Look here before you
decide something cannot be done from this isolation: a runner may be the way
your operator gave you to do it (a simulator, an evaluation, a build
elsewhere), with access this isolation has not.

## Asking for a job

    curl -sS "$PSPS_JOB_URL/jobs" --json @- <<'JOB'
    {"runner": "NAME", "dir": "/home/agent/work/project", "options": {"mode": "quick"}}
    JOB

- runner: one of the runners above; left out, the isolation's default one.
- dir: the directory to copy, absolute, or relative to your working directory.
  Files, directories and symbolic links are copied, links as links; devices,
  FIFOs and sockets are left out, as are the runner's excluded names (.git
  unless it says otherwise). A copy over the runner's limits is refused.
- options: a JSON object for the runner's command, at most 16 KiB. psps does not
  read it; what it may hold is the runner's to say: its "about", or else ask
  your operator or read what the runner refused.

It answers 202 with the job: its "id" and "status" (queued until the runner has
room, then running). The runner runs a few jobs at once and queues the rest, up
to a limit; past it psps answers 429.

## Following a job

    curl -sS "$PSPS_JOB_URL/jobs/ID?wait=120"

wait (seconds, at most 300) holds the answer until the job ends or the time is
up, so a loop of these follows a job without polling hard. status is one of
queued, running, done (the command exited 0), failed (it exited otherwise, or
psps could not run it: "error" says which), cancelled, or lost (its isolation
restarted under it). "result" is what the command left for you, as it chose,
and nothing else of its output: the rest is your operator's. A job's result is
there for a while after it ends.

Every job asked for from this isolation, newest first:

    curl -sS "$PSPS_JOB_URL/jobs"

Cancel one: the command is asked to stop (SIGTERM) and ended two minutes later
if it has not.

    curl -sS -X DELETE "$PSPS_JOB_URL/jobs/ID"
`
