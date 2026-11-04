package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/paths"
)

// teamCmd manages teams, the sidebar's named groups of live agents, and
// their leads. An agent joins one with `psps agent team`.
func teamCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: psps team ls|new|rename|rm|lead|show|eval|reset-best|resume  (psps team help: how teams work)")
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(teamHelp)
		return nil
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	fs := flag.NewFlagSet("team "+args[0], flag.ExitOnError)

	switch args[0] {
	case "ls":
		order, err := c.AgentOrder(ctx)
		if err != nil {
			return err
		}
		agents, err := c.Agents(ctx)
		if err != nil {
			return err
		}
		members := map[string][]string{}
		lead := map[string]string{}
		for _, a := range agents {
			if a.ArchivedAt == nil && a.TeamID != "" {
				members[a.TeamID] = append(members[a.TeamID], shownID(a))
			}
		}
		for _, t := range order.Teams {
			if t.Run == nil {
				continue
			}
			for _, a := range agents {
				if a.ArchivedAt == nil && orLine(a) == t.Run.Lead {
					lead[t.ID] = shownID(a)
				}
			}
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tID\tLEAD\tAGENTS\tRUN")
		for _, t := range order.Teams {
			run := "-"
			if t.Run != nil {
				run = t.Run.Status + "; " + spentOf(t.Run.Spent, t.Run.Budget)
				if t.Run.Best != nil {
					run += fmt.Sprintf("; best %g", *t.Run.Best)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.ID, orDash(lead[t.ID]), orDash(strings.Join(members[t.ID], ",")), run)
		}
		return tw.Flush()

	case "new":
		if len(args) < 2 {
			return errors.New("usage: psps team new NAME...")
		}
		t, err := c.CreateTeam(ctx, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("team %q made; an agent joins it with:\n  psps agent team AGENT %q\n", t.Name, t.Name)
		return nil

	case "rename":
		if len(args) < 3 {
			return errors.New("usage: psps team rename TEAM NAME...")
		}
		t, err := c.RenameTeam(ctx, args[1], strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("team %s is now %q\n", t.ID, t.Name)
		return nil

	case "rm":
		if len(args) != 2 {
			return errors.New("usage: psps team rm TEAM")
		}
		if err := c.DeleteTeam(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("team %s deleted; its agents are in none, where it was\n", args[1])
		return nil

	case "lead":
		clear := fs.Bool("clear", false, "leave the team without a lead; the agents it made stay, yours")
		budget := fs.String("budget", "", "the tokens the lead and its agents may use, sent anew and written (5M, 800k); none (the default) leaves only the plan ceiling")
		maxLive := fs.Int("max-live", 0, "how many agents of the lead's may be live at once (default 3)")
		ceiling := fs.Int("plan-ceiling", 0, "percent of a plan window past which nothing new starts on it (default 90)")
		turn := fs.Int("turn-minutes", 0, "how long a turn of an agent of the lead's may run, unless it sets one (default 60)")
		eval := fs.String("eval", "", "a shell command psps runs to score the work; its last \"SCORE: x\" line is the score, higher better")
		evalDir := fs.String("eval-dir", "", "where in the lead's isolation the eval runs, or the work copied to --eval-iso (default: the lead's directory)")
		evalIso := fs.String("eval-iso", "", "an isolation, with no services but control, to run the eval in on a copy of --eval-dir (~ in the eval is then an empty home); - for the lead's own")
		evalMinutes := fs.Int("eval-minutes", 0, "how long the eval may run (default 20)")
		evalJob := fs.String("eval-job", "", "a runner (psps job help) the eval is a job of instead, on a copy of --eval-dir; its result.json's \"score\" is the score; - for none")
		target := fs.String("target", "", "a score that ends the run once reached")
		noTarget := fs.Bool("no-target", false, "no target")
		patience := fs.Int("patience", 0, "evals in a row without a new best that end the run (default 3)")
		pos := parse(fs, args[1:])
		if *clear {
			if len(pos) != 1 {
				return errors.New("usage: psps team lead TEAM --clear")
			}
			if err := c.ClearLead(ctx, pos[0]); err != nil {
				return err
			}
			fmt.Printf("team %s has no lead; the agents it made are yours\n", pos[0])
			return nil
		}
		if len(pos) != 2 {
			return errors.New("usage: psps team lead TEAM AGENT [--budget N] [--eval CMD] [...]; psps team lead TEAM --clear")
		}
		req := api.PutLead{Agent: pos[1], NoTarget: *noTarget}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["budget"] {
			n, err := parseTokens(*budget)
			if err != nil {
				return err
			}
			req.Budget = &n
		}
		for _, f := range []struct {
			name string
			from *int
			to   **int
		}{{"max-live", maxLive, &req.MaxLive}, {"plan-ceiling", ceiling, &req.PlanCeiling}, {"turn-minutes", turn, &req.TurnMinutes},
			{"eval-minutes", evalMinutes, &req.EvalMinutes}, {"patience", patience, &req.Patience}} {
			if set[f.name] {
				*f.to = f.from
			}
		}
		if set["eval"] {
			req.Eval = eval
		}
		if set["eval-dir"] {
			req.EvalDir = evalDir
		}
		if set["eval-iso"] {
			name := strings.TrimPrefix(*evalIso, "-")
			req.EvalIsolation = &name
		}
		if set["eval-job"] {
			name := strings.TrimPrefix(*evalJob, "-")
			req.EvalJob = &name
		}
		if set["target"] {
			v, err := strconv.ParseFloat(*target, 64)
			if err != nil {
				return fmt.Errorf("--target %q is not a number", *target)
			}
			req.Target = &v
		}
		run, err := c.SetLead(ctx, pos[0], req)
		if err != nil {
			return err
		}
		fmt.Printf("%s leads team %q.\n", run.Lead, run.Team.Name)
		if run.TeamsOff != "" {
			fmt.Printf("Its isolation has no teams, so the run is paused until:\n  psps iso expose %s team\n", run.TeamsOff)
		}
		return printRun(run)

	case "show":
		asJSON := fs.Bool("json", false, "print the run as JSON")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: psps team show TEAM [--json]")
		}
		run, err := c.TeamRun(ctx, pos[0])
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(run)
		}
		return printRun(run)

	case "eval":
		if len(args) != 2 {
			return errors.New("usage: psps team eval TEAM")
		}
		if err := c.EvalTeam(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("the eval runs; its result goes to the lead, and shows in `psps team show`")
		return nil

	case "reset-best":
		if len(args) != 2 {
			return errors.New("usage: psps team reset-best TEAM")
		}
		if err := c.ResetBest(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("the evals so far count no more for the best, the target or the patience; the next scored eval is the best, and the lead is told")
		return nil

	case "resume":
		if len(args) != 2 {
			return errors.New("usage: psps team resume TEAM")
		}
		run, err := c.TeamRun(ctx, args[1])
		if err != nil {
			return err
		}
		if run, err = c.SetLead(ctx, args[1], api.PutLead{Agent: run.Lead}); err != nil {
			return err
		}
		fmt.Printf("team %q runs again; evals so far count no more toward its patience, and its lead is told\n", run.Team.Name)
		return nil
	}
	return fmt.Errorf("unknown team command %q", args[0])
}

// teamHelp is `psps team help`: how a team and its lead work, from a first
// team on. The README's Teams section says the same at more length.
const teamHelp = `psps team — named groups of agents, and a lead that runs one

A team is a named group of agents in the sidebar, from any isolations. It may
have a lead: an agent of yours that makes agents of its own in the team, one
for each role the work needs, hears when their turns end, has your eval score
their work, and designs the next team from what came of it. The lead designs
and steers; its agents do the work. psps keeps the run going, and within the
terms you set.

Commands

  psps team ls                      the teams: lead, agents, how each run stands
  psps team new NAME...             a team (psps agent new --team makes one too)
  psps team rename TEAM NAME...
  psps team rm TEAM                 its agents stay, in no team
  psps team lead TEAM AGENT [flags] AGENT leads TEAM and joins it; the flags are
                                    its terms, below
  psps team lead TEAM --clear       no lead; the agents it made stay, yours now
  psps team show TEAM [--json]      the run: status, spending, plan windows,
                                    scores, the lead's agents
  psps team eval TEAM               run the eval now; the score goes to the lead
  psps team reset-best TEAM         the evals so far count for no best; the next scored is it
  psps team resume TEAM             a run psps stopped goes on

A first team

  psps iso create lab
  psps iso login lab claude         # or: psps iso account lab claude ACCOUNT
  psps iso expose lab team          # lab's agents get a team address
  psps agent new --iso lab --id tara --team calc --workdir '~/work/calc'
  psps iso shell lab                # put the task's files in ~/work/calc, and
                                    # score.sh, which prints SCORE: <n>
  psps team lead calc tara --eval 'sh score.sh' --target 100
  psps agent send tara '/psps-team Build the calculator SPEC.md describes.'
  psps team show calc

The lead

  Being made lead tells an agent nothing. How to lead is in the psps-team
  skill, there while teams are on in its isolation: name it first in the work
  you give the lead, /psps-team for Claude and $psps-team for Codex, and it
  reads how to lead before it starts. Without it, an agent given work it could
  do alone tends to do it alone, and then no round ends and no eval runs. A
  session that has read the skill needs no reminder for later work; a new
  session does.

  Inside the isolation PSPS_TEAM_URL is the lead's address for its team: how
  the team stands, and its agents to make, write to, interrupt and archive.
  An agent a lead makes is a psps agent like yours, in the sidebar under the
  team and marked as the lead's. It runs in the lead's isolation and, unless
  the lead names another, the lead's directory, with the lead's approvals (so
  under ask it waits for you) and a session of its own, and makes no agents.
  A lead reaches only the agents it made and leads one team at a time; an
  agent a lead made leads nothing.

News and rounds

  psps writes to the lead whenever it is idle, in a message that starts
  "[psps team news ...]": an agent of its ended a turn, with its last answer;
  one waits for you to approve something; one ran past its turn limit, so
  psps stopped it and asked for a final report, which is the news; the eval
  ran; psps stopped the run. News that comes while the lead works waits, and
  arrives as one message.

  With an eval, a round lasts until nothing of the team runs. The news of a
  round's turns waits while others of them still work, so the lead hears of
  them together; it may then start the next step, an agent that needs the
  last one's work, and the round goes on. Once its turn on the news ends with
  none of its agents at work, psps runs the eval on what is in the eval's
  directory then, the lead's own changes included, tells the lead the score,
  and asks it for the next team or why it stops. A team that works in steps
  is scored once, after the last. A turn of the lead's own that starts no
  agent ends no round: what it changed alone is scored when it asks for the
  eval (POST $PSPS_TEAM_URL/eval), or with psps team eval. Without an eval, the lead hears of each
  turn as it ends, and only the budget stops the run.

The eval

  --eval is a shell command; the last line it prints that reads
  "SCORE: <number>" is the score, higher better. It runs for --eval-minutes
  at most (20), in one of two places:

  - The lead's isolation (the default), as its agents, in --eval-dir (the
    lead's directory by default). The agents can change the eval and what it
    reads. The lead hears the score, or the end of what it printed if it
    printed none.
  - An eval isolation (--eval-iso ISO), which the team never reaches: it may
    have agents of yours and the control service, and no other service. For
    each eval psps copies --eval-dir from the lead's isolation to /eval/work
    there and runs the eval as a user of its own (uid 1001, its home
    /eval/home, both made anew), who can read but not change the agent user's
    files. Keep the grader there (psps iso shell ISO) and give its path in
    full. The lead hears the score alone; what the eval printed is yours.

  Or, with --eval-job RUNNER in place of --eval, the eval is a job of one of
  your runners (psps job help): psps copies --eval-dir from the lead's
  isolation to the runner's and runs its command there, its meta.json saying
  the caller is "system" and the team. The runner's terms are the eval's, its
  result.json's "score" is the score, and the lead hears that alone. Agents
  with control cannot change such an eval.

  An eval set or changed scores no round that ended before it: its first
  eval comes after the next round, or with psps team eval. The README's Teams
  section has a grader to start from.

Terms: psps team lead TEAM AGENT [flags]

  --budget N          tokens the lead and its agents may use from now on, sent
                      anew and written (5M, 800k; none by default); reads from
                      cache do not count. Without one only the plan ceiling
                      holds the team back, so a CLI billed per use (an API key)
                      needs one.
  --max-live N        how many of the lead's agents may be live at once (3)
  --turn-minutes N    how long one of their turns may run before psps stops it
                      and asks for a final report (60); the lead may set one
                      per agent
  --plan-ceiling PCT  how full a plan window may get before nothing new starts
                      on it (90)
  --eval CMD, --eval-dir DIR, --eval-iso ISO|-, --eval-minutes N, --eval-job RUNNER|-
                      the eval, above; --eval-iso - runs it in the lead's own
  --target X          a score that ends the run once reached (--no-target: none)
  --patience N        evals in a row without a new best that end the run (3)

  Run it again to change them: what you leave out stays. Naming another agent
  moves the lead and keeps the terms, with spending counted from then.

When psps holds the team back

  psps stops the run when the budget is used, the target is reached, or
  --patience evals in a row bring no new best, and tells the lead: no agent of
  its can be made or written to, and those at work finish their turns. An
  eval psps could not run (its isolation unfit, no fence) counts for none.
  psps team resume TEAM lets it go on, with the evals so far no longer
  counting toward patience, and tells the lead, which it asks for the next
  team. Changing the terms with psps team lead resumes it too. Neither does
  while what stopped the run still holds: a budget used needs a larger one
  (--budget), a target reached a higher one or none (--target, --no-target).

  While the run goes on, the lead is refused, with the reason, more live
  agents than --max-live, and anything new on a plan window past the
  ceiling; it is told to tell you rather than work around it.

  Turning teams off in the lead's isolation (psps iso unexpose ISO team)
  pauses the run: the lead hears nothing and can do nothing, while its agents
  finish their turns and the eval runs. Turned on again, what came meanwhile
  reaches the lead as one message.

Elsewhere

  The web (the pencil on a team's name, or its run's line) and the TUI (l on a
  team's name) set the same, and offer to turn teams on in the lead's
  isolation. Agents with psps's control service can list teams, see a run,
  run its eval and set one to run where they are, as far as its env allows
  (psps env help control).
`

// printRun shows a team's run.
func printRun(run api.TeamRun) error {
	s := run.Settings
	fmt.Printf("team %q, led by %s since %s: %s\n", run.Team.Name, run.Lead, run.Since.Local().Format("Jan 2 15:04"), run.Status)
	fmt.Printf("  spent  %s (%s more read from cache)\n", spentOf(run.Spent.New(), s.Budget), tokenCount(run.Spent.Cached))
	fmt.Printf("  terms  %d live at most, turns of %dm, nothing new past %d%% of a plan window\n", s.MaxLive, s.TurnMinutes, s.PlanCeiling)
	for _, w := range run.Plan {
		fmt.Printf("  plan   %s %s %.0f%%\n", w.Kind, w.Window, w.Percent)
	}
	if s.EvalJob != "" {
		fmt.Printf("  eval   a job of runner %s, on a copy of %s, patience %d", s.EvalJob, s.EvalDir, s.Patience)
		if s.Target != nil {
			fmt.Printf(", target %g", *s.Target)
		}
		fmt.Println()
	} else if s.Eval == "" {
		fmt.Println("  eval   none")
	} else {
		where := "in " + s.EvalDir
		if run.EvalIsolation != "" {
			where = "in isolation " + run.EvalIsolation + ", on a copy of " + s.EvalDir
		}
		fmt.Printf("  eval   %s (%s, %dm at most), patience %d", s.Eval, where, s.EvalMinutes, s.Patience)
		if s.Target != nil {
			fmt.Printf(", target %g", *s.Target)
		}
		if s.EvalSetBy != "" {
			fmt.Printf("; set by agent %s, through control", s.EvalSetBy)
		}
		fmt.Println()
	}
	for _, e := range run.Evals {
		score := "no score"
		if e.Score != nil {
			score = strconv.FormatFloat(*e.Score, 'g', -1, 64)
		} else if e.NotRun {
			score = "not run"
		}
		fmt.Printf("  #%-4d %s  %-10s %s, %s\n", e.ID, e.At.Local().Format("Jan 2 15:04"), score, e.Why, e.Took.Round(time.Second))
	}
	if n := len(run.Evals); n > 0 && run.Evals[n-1].Score == nil && run.Evals[n-1].Output != "" {
		e := run.Evals[n-1]
		fmt.Printf("  the end of #%d's output:\n", e.ID)
		for _, l := range strings.Split(e.Output, "\n") {
			fmt.Println("    " + l)
		}
	}
	if run.Best != nil {
		fmt.Printf("  best   %g at #%d; %d since without a new one\n", *run.Best.Score, run.Best.ID, run.Stale)
	}
	if len(run.Agents) == 0 {
		fmt.Printf("  agents none live (%d archived)\n", run.Archived)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tROLE\tKIND\tSTATUS\tTURN\tSPENT")
	for _, a := range run.Agents {
		turn := "-"
		if a.TurnSince != nil {
			turn = fmt.Sprintf("%s of %dm", time.Since(*a.TurnSince).Round(time.Minute), a.TurnMinutes)
		}
		status := string(a.Status)
		if a.Waits != "" {
			status += ": " + a.Waits
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Role, a.Kind, status, turn, tokenCount(a.Spent.New()))
	}
	if run.Archived > 0 {
		fmt.Fprintf(tw, "  (%d archived)\n", run.Archived)
	}
	return tw.Flush()
}

// parseTokens reads a budget: 5000000, 5M, 800k, or none (0).
func parseTokens(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if t == "none" {
		return 0, nil
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(t, "m"):
		mult, t = 1e6, strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "k"):
		mult, t = 1e3, strings.TrimSuffix(t, "k")
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("--budget %q is not a count of tokens (5M, 800k, 2000000, none)", s)
	}
	return int64(v * mult), nil
}

// spentOf is spent tokens, of budget if there is one.
func spentOf(spent, budget int64) string {
	if budget <= 0 {
		return tokenCount(spent) + " tokens, no budget"
	}
	return fmt.Sprintf("%s of %s tokens", tokenCount(spent), tokenCount(budget))
}

// tokenCount is n as people read it: 950, 12.3k, 4.1M.
func tokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

func orLine(a api.Agent) string {
	if a.Line != "" {
		return a.Line
	}
	return a.ID
}
