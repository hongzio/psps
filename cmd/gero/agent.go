package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/paths"
)

func agentCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero agent new|ls|log|send|approve|interrupt|set|archive")
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	fs := flag.NewFlagSet("agent "+args[0], flag.ExitOnError)
	optFlags := func() func() driver.Options {
		model := fs.String("model", "", "model")
		effort := fs.String("effort", "", "reasoning effort")
		approvals := fs.String("approvals", "", "never (default): run unattended; ask: surface tool use for approval")
		return func() driver.Options {
			return driver.Options{Model: *model, Effort: *effort, Approvals: driver.Approvals(*approvals)}
		}
	}

	switch args[0] {
	case "new":
		iso := fs.String("iso", "", "isolation to run in (required)")
		kind := fs.String("kind", "claude", "claude or codex")
		repo := fs.String("repo", "", "git URL to clone into the workspace")
		name := fs.String("name", "", "display name (default: from the prompt)")
		opts := optFlags()
		prompt := strings.Join(parse(fs, args[1:]), " ")
		if *iso == "" || prompt == "" {
			return errors.New("usage: gero agent new --iso NAME --kind claude|codex [--repo URL] PROMPT...")
		}
		a, err := c.CreateAgent(ctx, api.CreateAgent{Isolation: *iso, Kind: driver.Kind(*kind), Prompt: prompt,
			Name: *name, Repo: *repo, Options: opts()})
		if err != nil {
			return err
		}
		fmt.Printf("agent %s started in %s (%s)\nfollow it:  gero agent log %s -f\n", a.ID, a.Isolation, a.Workdir, a.ID)
		return nil

	case "ls":
		all := fs.Bool("all", false, "include archived agents")
		parse(fs, args[1:])
		agents, err := c.Agents(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tKIND\tISOLATION\tNAME")
		for _, a := range agents {
			status := string(a.State.Status)
			switch {
			case a.ArchivedAt != nil:
				if !*all {
					continue
				}
				status = "archived"
			case a.Problem != "":
				status += " (!)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, status, a.Kind, a.Isolation, a.Name)
		}
		return tw.Flush()

	case "log":
		follow := fs.Bool("f", false, "keep following")
		stream := fs.Bool("stream", false, "also print partial output as it streams")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent log ID [-f]")
		}
		a, err := c.Agent(ctx, pos[0])
		if err != nil {
			return err
		}
		pr := &printer{stream: *stream}
		if !*follow {
			evs, err := c.Events(ctx, a.ID, 0)
			if err != nil {
				return err
			}
			for _, e := range evs {
				pr.print(e)
			}
			return nil
		}
		err = c.Follow(ctx, a.ID, 0, func(e event.Event) error { pr.print(e); return nil })
		if ctx.Err() != nil {
			return nil
		}
		return err

	case "send":
		pos := parse(fs, args[1:])
		if len(pos) < 2 {
			return errors.New("usage: gero agent send ID TEXT...")
		}
		return c.Send(ctx, pos[0], strings.Join(pos[1:], " "))

	case "approve":
		pos := parse(fs, args[1:])
		if len(pos) != 3 {
			return errors.New("usage: gero agent approve ID APPROVAL accept|decline|cancel")
		}
		d, err := driver.ParseDecision(pos[2])
		if err != nil {
			return err
		}
		return c.Approve(ctx, pos[0], pos[1], d)

	case "interrupt":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent interrupt ID")
		}
		return c.Interrupt(ctx, pos[0])

	case "set":
		opts := optFlags()
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent set ID [--model M] [--effort E] [--approvals never|ask]")
		}
		a, err := c.Agent(ctx, pos[0])
		if err != nil {
			return err
		}
		o, cur := opts(), a.Options
		if o.Model != "" {
			cur.Model = o.Model
		}
		if o.Effort != "" {
			cur.Effort = o.Effort
		}
		if o.Approvals != "" {
			cur.Approvals = o.Approvals
		}
		return c.SetOptions(ctx, a.ID, cur)

	case "archive":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent archive ID")
		}
		return c.Archive(ctx, pos[0])
	}
	return fmt.Errorf("unknown agent command %q", args[0])
}

type printer struct {
	stream  bool
	midLine bool
}

func (p *printer) print(e event.Event) {
	if e.Ephemeral {
		if !p.stream || e.Kind != event.KindDelta {
			if e.Kind == event.KindNotice {
				p.line("notice", e.Text)
			}
			return
		}
		fmt.Print(e.Text)
		p.midLine = true
		return
	}
	switch e.Kind {
	case event.KindUser:
		p.line("you", e.Text)
	case event.KindAssistant:
		p.line("agent", e.Text)
	case event.KindReasoning:
		p.line("thinking", e.Text)
	case event.KindToolStart:
		p.line("tool", e.Tool.Name+" "+e.Tool.Summary)
	case event.KindToolEnd:
		mark := "ok"
		if e.Tool.IsError {
			mark = "failed"
		}
		out := strings.TrimSpace(e.Tool.Output)
		if len(out) > 400 {
			out = out[:400] + "…"
		}
		p.line("tool "+mark, out)
	case event.KindApproval:
		a := e.Approval
		p.line("approval", fmt.Sprintf("[%s] %s: %s", a.ID, a.Kind, a.Summary))
		if a.Reason != "" {
			p.line("", "reason: "+a.Reason)
		}
		p.line("", fmt.Sprintf("gero agent approve %s %s accept|decline|cancel", e.Agent, a.ID))
	case event.KindApprovalDone:
		p.line("approval", e.Approval.ID+" "+e.Approval.Decision)
	case event.KindStatus:
		s := string(e.Status)
		if e.Text != "" {
			s += ": " + e.Text
		}
		p.line("status", s)
	case event.KindTurnEnd:
		p.line("turn", e.Text)
	case event.KindError:
		p.line("error", e.Text)
	case event.KindNotice:
		p.line("notice", e.Text)
	}
}

func (p *printer) line(label, text string) {
	if p.midLine {
		fmt.Println()
		p.midLine = false
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if i > 0 {
			label = ""
		}
		fmt.Printf("%-10s │ %s\n", label, l)
	}
}
