package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/paths"
	"github.com/hongzio/psps/internal/store"
)

// jobCmd follows and runs jobs: a runner's command (an env of the job
// service) run in its isolation on a copy of a directory.
func jobCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: psps job ls|show|run|cancel  (psps job help: what a runner is and what its command gets)")
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(jobHelp)
		return nil
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	fs := flag.NewFlagSet("job "+args[0], flag.ExitOnError)

	switch args[0] {
	case "ls":
		live := fs.Bool("live", false, "only the jobs that have not ended")
		parse(fs, args[1:])
		js, err := c.Jobs(ctx, *live)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tRUNNER\tFOR\tFROM\tDIR\tASKED\tTOOK")
		for _, j := range js {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, jobStatus(j), j.Runner, jobFor(j), orDash(j.FromIsolation), j.Dir,
				j.RequestedAt.Local().Format("Jan 2 15:04"), jobTook(j))
		}
		return tw.Flush()

	case "show":
		asJSON := fs.Bool("json", false, "all of it, as JSON")
		wait := fs.Int("wait", 0, "wait up to this many seconds for it to end")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: psps job show ID [--json] [--wait SECONDS]")
		}
		j, err := c.Job(ctx, pos[0], *wait)
		if err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(j, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		printJob(j)
		return nil

	case "run":
		from := fs.String("from", "", "the isolation the directory is in (required)")
		options := fs.String("options", "", "the options, a JSON object, or @FILE for one in a file on this Mac")
		wait := fs.Bool("wait", false, "wait for it to end and show it")
		pos := parse(fs, args[1:])
		if len(pos) != 2 || *from == "" {
			return errors.New("usage: psps job run RUNNER --from ISOLATION [--options JSON|@FILE] [--wait] DIR")
		}
		req := api.PostJob{Runner: pos[0], Isolation: *from, Dir: pos[1]}
		if o := *options; o != "" {
			if strings.HasPrefix(o, "@") {
				b, err := os.ReadFile(o[1:])
				if err != nil {
					return err
				}
				o = string(b)
			}
			req.Options = json.RawMessage(o)
		}
		j, err := c.RunJob(ctx, req)
		if err != nil {
			return err
		}
		if !*wait {
			fmt.Printf("job %s %s on runner %s; follow it with:\n  psps job show %s --wait 300\n", j.ID, j.Status, j.Runner, j.ID)
			return nil
		}
		for !store.JobEnded(j.Status) {
			if j, err = c.Job(ctx, j.ID, 300); err != nil {
				return err
			}
		}
		printJob(j)
		if j.Status != store.JobDone {
			return fmt.Errorf("job %s %s", j.ID, j.Status)
		}
		return nil

	case "cancel":
		if len(args) != 2 {
			return errors.New("usage: psps job cancel ID")
		}
		j, err := c.CancelJob(ctx, args[1])
		if err != nil {
			return err
		}
		if j.Status == store.JobCancelled {
			fmt.Printf("job %s cancelled before it ran\n", j.ID)
		} else {
			fmt.Printf("job %s is asked to stop (SIGTERM), and is ended in two minutes if it has not\n", j.ID)
		}
		return nil
	}
	return fmt.Errorf("unknown job command %q", args[0])
}

func jobStatus(j api.Job) string {
	if j.Queued > 0 {
		return fmt.Sprintf("queued #%d", j.Queued)
	}
	return j.Status
}

// jobFor says who a job is for.
func jobFor(j api.Job) string {
	switch j.Caller {
	case store.CallerAgent:
		return "agent " + orStr(j.AgentID, j.Agent)
	case store.CallerSystem:
		if j.TeamName != "" {
			return "team " + j.TeamName
		}
		return "psps"
	}
	return "you"
}

func jobTook(j api.Job) string {
	switch {
	case j.StartedAt == nil:
		return "-"
	case j.EndedAt == nil:
		return time.Since(*j.StartedAt).Round(time.Second).String() + " so far"
	}
	return j.EndedAt.Sub(*j.StartedAt).Round(time.Second).String()
}

func printJob(j api.Job) {
	fmt.Printf("job %s: %s", j.ID, jobStatus(j))
	if j.Error != "" {
		fmt.Printf(" (%s)", j.Error)
	}
	fmt.Println()
	fmt.Printf("  runner   %s, in isolation %s\n", j.Runner, orDash(j.RunnerIsolation))
	fmt.Printf("  for      %s", jobFor(j))
	if j.Why != "" {
		fmt.Printf(" (%s)", j.Why)
	}
	fmt.Println()
	fmt.Printf("  copy of  %s in isolation %s\n", j.Dir, orDash(j.FromIsolation))
	fmt.Printf("  asked    %s\n", j.RequestedAt.Local().Format("Jan 2 15:04:05"))
	if j.StartedAt != nil {
		fmt.Printf("  ran      %s, %s, of %s at most\n", j.StartedAt.Local().Format("Jan 2 15:04:05"), jobTook(j), j.Limit)
	}
	if j.Note != "" {
		fmt.Printf("  note     %s\n", j.Note)
	}
	fmt.Printf("  options  %s\n", j.Options)
	if len(j.Result) > 0 {
		fmt.Printf("  result   %s\n", j.Result)
	}
	if out := strings.TrimSpace(j.Output); out != "" {
		fmt.Printf("\n%s\n", out)
	}
}

const jobHelp = `Jobs: a command of yours run elsewhere, on a copy of a directory

A runner is an env of the job service: an isolation, a command, and its terms.
A job runs the command in that isolation on a copy of a directory from another,
for a caller: an agent, through its job address (the psps-job skill); psps
itself, for a team's eval on a runner (psps team lead --eval-job RUNNER); or you
(psps job run). The runner runs max_live jobs at once and queues the rest,
psps's own first, up to max_queued (psps's own always wait).

  psps env add job sim --isolation sim --command './simulate' [--minutes 60]
      [--memory 4G] [--pids 512] [--max-live 2] [--max-queued 20]
      [--max-mb 500] [--max-files 50000] [--exclude .git] [--about TEXT]
      --about: what its agents read of it (what it does, its options, its
      result); its command they never see
  psps iso expose dev job sim[,other] [--default-env sim]
      dev's agents may ask for jobs on those runners

What the command gets

  It runs as the agent user in the runner's isolation, in its copy, with the
  isolation's own environment (its services' addresses, psps iso env) and
  nothing of root's, in a cgroup of the job's own: --memory (no swap) and
  --pids hold for all it starts, and whatever it leaves running ends with it.
  Where the isolation cannot give it such a cgroup (Apple's container), it runs
  without those limits, the VM's memory the bound, held together by a session
  of its own; meta.json's "limits" is null then.
  timeout(1) sends SIGTERM at --minutes and SIGKILL two minutes later; a job
  cancelled is sent SIGTERM at once and ended two minutes later.

    PSPS_JOB_WORK      the copy, the job's alone (the working directory)
    PSPS_JOB_META      who asked, written by psps, root's and read-only:
                       {"v":1, "job", "runner", "caller": "agent"|"system"|"operator",
                        "agent": {"id","handle","name"}|null, "isolation": where the
                        directory is, "team": {"id","name"}|null, "dir", "why",
                        "limits": {"memory","pids","minutes"}|null,
                        "requested_at", "started_at", "deadline"}
    PSPS_JOB_OPTIONS   the caller's options, a JSON object psps did not read
    PSPS_JOB_OUT       where it leaves result.json
    PSPS_JOB_ID, PSPS_JOB_RUNNER, PSPS_JOB_DEADLINE

  Everything in the copy is the caller's to write, options.json too: what the
  command trusts of who asked is meta.json, at /var/lib/psps-job/ID/meta.json,
  owned by root and not writable by the agent user.

What comes back

  result.json (at most 1 MiB, a JSON object) is kept whole for you. Its
  "caller" is all the agent that asked gets of it; for a team's eval its
  "score", a number, is the eval's score and all the lead hears of it. The rest
  of it, and what the command prints, are yours alone (psps job show). The job
  is done when the command exits 0, failed otherwise.

  The copy has files, directories and symbolic links, links as links and
  never followed; devices, FIFOs, sockets and the --exclude names are left out,
  and a copy over --max-mb or --max-files is refused. The job's directory,
  copy and all, is removed when it ends.

  psps job ls [--live]             jobs, newest first, kept 14 days
  psps job show ID [--json] [--wait SECONDS]
  psps job run RUNNER --from ISO [--options JSON|@FILE] [--wait] DIR
  psps job cancel ID
`
