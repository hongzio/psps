package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/paths"
	"github.com/hongzio/psps/internal/proxy/askproxy"
)

const askUsage = `usage:
  psps ask [--agent ID]          the asks waiting for you; with --agent, that agent's, answered or not
  psps ask show N
  psps ask reply N [--pick K] [TEXT...]   answer a question: an option, your own words, or both
  psps ask yes|no N [TEXT...]    give or refuse a go-ahead (a confirm)
  psps ask done|cant N [TEXT...] say you did, or could not do, what a task asks
  psps ask dismiss N [TEXT...]   close it without an answer; the agent is told`

// askCmd lists and answers the asks agents leave you (the ask service).
// Every answer reaches its agent once it is idle.
func askCmd(p paths.Paths, args []string) error {
	verb := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	if verb == "help" || verb == "-h" || verb == "--help" {
		fmt.Println(askUsage)
		return nil
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	fs := flag.NewFlagSet("ask "+verb, flag.ExitOnError)

	switch verb {
	case "ls":
		agent := fs.String("agent", "", "one agent's asks, answered or not")
		if pos := parse(fs, args); len(pos) != 0 {
			return errors.New(askUsage)
		}
		var as []api.Ask
		if *agent != "" {
			as, err = c.AgentAsks(ctx, *agent)
		} else {
			as, err = c.OpenAsks(ctx)
		}
		if err != nil {
			return err
		}
		if len(as) == 0 {
			if *agent == "" {
				fmt.Println("No ask waits for you.")
			}
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ASK\tAGENT\tISOLATION\tKIND\tSTATE\tASKED\tTITLE")
		for _, a := range as {
			fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.AgentID, a.Isolation, a.Kind, a.Verdict,
				a.CreatedAt.Local().Format("Jan 2 15:04"), a.Title)
		}
		return tw.Flush()

	case "show":
		pos := parse(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: psps ask show N")
		}
		id, err := askNumber(pos[0])
		if err != nil {
			return err
		}
		a, err := c.Ask(ctx, id)
		if err != nil {
			return err
		}
		printAsk(a)
		return nil

	case "reply", "yes", "no", "done", "cant", "dismiss":
		pick := 0
		if verb == "reply" {
			fs.IntVar(&pick, "pick", 0, "the option you choose, from 1")
		}
		pos := parse(fs, args)
		if len(pos) == 0 {
			return errors.New(askUsage)
		}
		id, err := askNumber(pos[0])
		if err != nil {
			return err
		}
		req := api.AnswerAsk{Pick: pick, Text: strings.Join(pos[1:], " ")}
		switch verb {
		case "yes":
			req.Outcome = askproxy.Yes
		case "no":
			req.Outcome = askproxy.No
		case "done":
			req.Outcome = askproxy.Done
		case "cant":
			req.Outcome = askproxy.Cant
		case "dismiss":
			req.Outcome = "dismiss"
		}
		a, err := c.AnswerAsk(ctx, id, req)
		if err != nil {
			return err
		}
		fmt.Printf("#%d %s; %s hears of it when it is next idle.\n", a.ID, a.Verdict, a.AgentID)
		return nil
	}
	return fmt.Errorf("unknown ask command %q\n\n%s", verb, askUsage)
}

func askNumber(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not an ask number", s)
	}
	return id, nil
}

func printAsk(a api.Ask) {
	fmt.Printf("#%d %s · %s from %s in %s, %s\n", a.ID, a.Verdict, a.Kind, a.AgentID, a.Isolation, a.CreatedAt.Local().Format("Jan 2 15:04"))
	fmt.Printf("\n%s\n", a.Title)
	if a.Body != "" {
		fmt.Printf("\n%s\n", a.Body)
	}
	if len(a.Options) > 0 {
		fmt.Println()
		for i, o := range a.Options {
			mark := " "
			if a.Pick == i+1 {
				mark = "*"
			}
			if o.Note != "" {
				fmt.Printf("%s %d. %s: %s\n", mark, i+1, o.Label, o.Note)
			} else {
				fmt.Printf("%s %d. %s\n", mark, i+1, o.Label)
			}
		}
	}
	if a.Text != "" {
		fmt.Printf("\nYour words: %s\n", a.Text)
	}
	if a.State == "open" {
		switch a.Kind {
		case askproxy.Question:
			fmt.Printf("\npsps ask reply %d [--pick K] [TEXT...]\n", a.ID)
		case askproxy.Confirm:
			fmt.Printf("\npsps ask yes|no %d [TEXT...]\n", a.ID)
		case askproxy.Task:
			fmt.Printf("\npsps ask done|cant %d [TEXT...]\n", a.ID)
		}
	}
}
