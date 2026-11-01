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

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/paths"
)

// teamCmd manages teams, the sidebar's named groups of live agents, and
// their leads. An agent joins one with `gero agent team`.
func teamCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero team ls|new|rename|rm|lead|show|eval|resume")
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
				run = fmt.Sprintf("%s; %s of %s tokens", t.Run.Status, tokenCount(t.Run.Spent), tokenCount(t.Run.Budget))
				if t.Run.Best != nil {
					run += fmt.Sprintf("; best %g", *t.Run.Best)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.ID, orDash(lead[t.ID]), orDash(strings.Join(members[t.ID], ",")), run)
		}
		return tw.Flush()

	case "new":
		if len(args) < 2 {
			return errors.New("usage: gero team new NAME...")
		}
		t, err := c.CreateTeam(ctx, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("team %q made; an agent joins it with:\n  gero agent team AGENT %q\n", t.Name, t.Name)
		return nil

	case "rename":
		if len(args) < 3 {
			return errors.New("usage: gero team rename TEAM NAME...")
		}
		t, err := c.RenameTeam(ctx, args[1], strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("team %s is now %q\n", t.ID, t.Name)
		return nil

	case "rm":
		if len(args) != 2 {
			return errors.New("usage: gero team rm TEAM")
		}
		if err := c.DeleteTeam(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("team %s deleted; its agents are in none, where it was\n", args[1])
		return nil

	case "lead":
		clear := fs.Bool("clear", false, "leave the team without a lead; the agents it made stay, yours")
		budget := fs.String("budget", "", "the tokens the lead and its agents may use, sent anew and written (5M, 800k)")
		maxLive := fs.Int("max-live", 0, "how many agents of the lead's may be live at once (default 3)")
		ceiling := fs.Int("plan-ceiling", 0, "percent of a plan window past which nothing new starts on it (default 90)")
		turn := fs.Int("turn-minutes", 0, "how long a turn of an agent of the lead's may run, unless it sets one (default 60)")
		eval := fs.String("eval", "", "a shell command gero runs to score the work; its last \"SCORE: x\" line is the score, higher better")
		evalDir := fs.String("eval-dir", "", "where in the isolation the eval runs (default: the lead's directory)")
		evalMinutes := fs.Int("eval-minutes", 0, "how long the eval may run (default 20)")
		target := fs.String("target", "", "a score that ends the run once reached")
		noTarget := fs.Bool("no-target", false, "no target")
		patience := fs.Int("patience", 0, "evals in a row without a new best that end the run (default 3)")
		pos := parse(fs, args[1:])
		if *clear {
			if len(pos) != 1 {
				return errors.New("usage: gero team lead TEAM --clear")
			}
			if err := c.ClearLead(ctx, pos[0]); err != nil {
				return err
			}
			fmt.Printf("team %s has no lead; the agents it made are yours\n", pos[0])
			return nil
		}
		if len(pos) != 2 {
			return errors.New("usage: gero team lead TEAM AGENT --budget N [--eval CMD] [...]; gero team lead TEAM --clear")
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
		fmt.Printf("%s leads team %q. Its isolation needs the team service for it to act:\n  gero iso expose ISOLATION team\n", run.Lead, run.Team.Name)
		return printRun(run)

	case "show":
		asJSON := fs.Bool("json", false, "print the run as JSON")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero team show TEAM [--json]")
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
			return errors.New("usage: gero team eval TEAM")
		}
		if err := c.EvalTeam(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("the eval runs; its result goes to the lead, and shows in `gero team show`")
		return nil

	case "resume":
		if len(args) != 2 {
			return errors.New("usage: gero team resume TEAM")
		}
		run, err := c.TeamRun(ctx, args[1])
		if err != nil {
			return err
		}
		if run, err = c.SetLead(ctx, args[1], api.PutLead{Agent: run.Lead}); err != nil {
			return err
		}
		fmt.Printf("team %q runs again; evals so far count no more toward its patience\n", run.Team.Name)
		return nil
	}
	return fmt.Errorf("unknown team command %q", args[0])
}

// printRun shows a team's run.
func printRun(run api.TeamRun) error {
	s := run.Settings
	fmt.Printf("team %q, led by %s since %s: %s\n", run.Team.Name, run.Lead, run.Since.Local().Format("Jan 2 15:04"), run.Status)
	fmt.Printf("  spent  %s of %s tokens (%s more read from cache)\n", tokenCount(run.Spent.New()), tokenCount(s.Budget), tokenCount(run.Spent.Cached))
	fmt.Printf("  terms  %d live at most, turns of %dm, nothing new past %d%% of a plan window\n", s.MaxLive, s.TurnMinutes, s.PlanCeiling)
	for _, w := range run.Plan {
		fmt.Printf("  plan   %s %s %.0f%%\n", w.Kind, w.Window, w.Percent)
	}
	if s.Eval == "" {
		fmt.Println("  eval   none")
	} else {
		fmt.Printf("  eval   %s (in %s, %dm at most), patience %d", s.Eval, s.EvalDir, s.EvalMinutes, s.Patience)
		if s.Target != nil {
			fmt.Printf(", target %g", *s.Target)
		}
		fmt.Println()
	}
	for _, e := range run.Evals {
		score := "no score"
		if e.Score != nil {
			score = strconv.FormatFloat(*e.Score, 'g', -1, 64)
		}
		fmt.Printf("  #%-4d %s  %-10s %s, %s\n", e.ID, e.At.Local().Format("Jan 2 15:04"), score, e.Why, e.Took.Round(time.Second))
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

// parseTokens reads a count of tokens: 5000000, 5M, 800k.
func parseTokens(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(t, "m"):
		mult, t = 1e6, strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "k"):
		mult, t = 1e3, strings.TrimSuffix(t, "k")
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("--budget %q is not a count of tokens (5M, 800k, 2000000)", s)
	}
	return int64(v * mult), nil
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
