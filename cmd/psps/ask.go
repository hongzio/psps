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
	"github.com/hongzio/psps/internal/store"
)

const askUsage = `usage:
  psps ask [--agent ID]          the open asks, those waiting for you first; with --agent, that agent's, answered or not
  psps ask show N                the ask and what you and the agent added to it
  psps ask say N TEXT...         reply and keep it open: the agent adds to it in turn
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
				fmt.Println("No ask is open.")
			}
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ASK\tAGENT\tISOLATION\tKIND\tSTATE\tNOTES\tASKED\tTITLE")
		for _, a := range as {
			fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", a.ID, a.AgentID, a.Isolation, a.Kind, askState(a), len(a.Notes),
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

	case "say":
		pos := parse(fs, args)
		if len(pos) < 2 {
			return errors.New("usage: psps ask say N TEXT...")
		}
		id, err := askNumber(pos[0])
		if err != nil {
			return err
		}
		a, err := c.ReplyAsk(ctx, id, api.ReplyAsk{Text: strings.Join(pos[1:], " ")})
		if err != nil {
			return err
		}
		fmt.Printf("#%d stays open; %s hears your reply when it is next idle.\n", a.ID, a.AgentID)
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

// sameOptions is whether a and b are the same slice of options: those the
// pick refers to.
func sameOptions(a, b []store.AskOption) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

func askNumber(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not an ask number", s)
	}
	return id, nil
}

// askState is what became of an ask, or, open, whose word it waits on.
func askState(a api.Ask) string {
	switch {
	case a.State != store.AskOpen:
		return a.Verdict
	case a.Turn == store.TurnAgent:
		return "waits on the agent"
	}
	return "waits on you"
}

func printOptions(opts []store.AskOption, pick int) {
	for i, o := range opts {
		mark := " "
		if pick == i+1 {
			mark = "*"
		}
		if o.Note != "" {
			fmt.Printf("%s %d. %s: %s\n", mark, i+1, o.Label, o.Note)
		} else {
			fmt.Printf("%s %d. %s\n", mark, i+1, o.Label)
		}
	}
}

func printAsk(a api.Ask) {
	fmt.Printf("#%d %s · %s from %s in %s, %s\n", a.ID, askState(a), a.Kind, a.AgentID, a.Isolation, a.CreatedAt.Local().Format("Jan 2 15:04"))
	fmt.Printf("\n%s\n", a.Title)
	if a.Body != "" {
		fmt.Printf("\n%s\n", a.Body)
	}
	current := a.Current()
	if len(a.Options) > 0 {
		fmt.Println()
		pick := 0
		if len(a.Notes) == 0 || sameOptions(a.Options, current) {
			pick = a.Pick
		}
		printOptions(a.Options, pick)
	}
	for _, n := range a.Notes {
		who := a.AgentID
		if n.Author == store.TurnOperator {
			who = "You"
		}
		fmt.Printf("\n— %s, %s\n", who, n.CreatedAt.Local().Format("Jan 2 15:04"))
		if n.Text != "" {
			fmt.Println(n.Text)
		}
		if len(n.Options) > 0 {
			pick := 0
			if sameOptions(n.Options, current) {
				pick = a.Pick
			}
			printOptions(n.Options, pick)
		}
	}
	if a.Text != "" {
		fmt.Printf("\nYour words: %s\n", a.Text)
	}
	if a.State == store.AskOpen {
		fmt.Println()
		switch a.Kind {
		case askproxy.Question:
			fmt.Printf("psps ask reply %d [--pick K] [TEXT...]   answer and close\n", a.ID)
		case askproxy.Confirm:
			fmt.Printf("psps ask yes|no %d [TEXT...]             answer and close\n", a.ID)
		case askproxy.Task:
			fmt.Printf("psps ask done|cant %d [TEXT...]          answer and close\n", a.ID)
		}
		fmt.Printf("psps ask say %d TEXT...                  reply, keep it open\n", a.ID)
	}
}
