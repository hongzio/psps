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
	"github.com/hongzio/gero/internal/store"
)

func agentCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero agent new|ls|log|send|inbox|mail|cron|approve|interrupt|set|rename|id|archive|restore|rm")
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
		approvals := fs.String("approvals", "", "never (default): run unattended; auto: unattended, a reviewer model refuses risky actions; ask: surface tool use for approval; default: the CLI's own settings in the isolation decide")
		return func() driver.Options {
			return driver.Options{Model: *model, Effort: *effort, Approvals: driver.Approvals(*approvals)}
		}
	}

	switch args[0] {
	case "new":
		iso := fs.String("iso", "", "isolation to run in (required)")
		kind := fs.String("kind", "claude", "claude or codex")
		repo := fs.String("repo", "", "git URL to clone into the workspace")
		workdir := fs.String("workdir", "", "where in the isolation it works, made if missing; ~ is the agent's home (default: a workspace of its own under ~/work)")
		name := fs.String("name", "", "display name (default: the first line of its first message)")
		handle := fs.String("id", "", "an ID of your own for it, unique among all agents (default: an adjective-noun no agent has, like brave-otter)")
		opts := optFlags()
		prompt := strings.Join(parse(fs, args[1:]), " ")
		if *iso == "" {
			return errors.New("usage: gero agent new --iso NAME --kind claude|codex [--id ID] [--repo URL] [--workdir DIR] [PROMPT...]")
		}
		a, err := c.CreateAgent(ctx, api.CreateAgent{Isolation: *iso, Kind: driver.Kind(*kind), Prompt: prompt,
			Name: *name, Handle: *handle, Repo: *repo, Workdir: *workdir, Options: opts()})
		if err != nil {
			return err
		}
		id := shownID(a)
		if prompt == "" {
			fmt.Printf("agent %s created in %s (%s); its first message starts it:\n  gero agent send %s TEXT...\n", id, a.Isolation, a.Workdir, id)
			return nil
		}
		fmt.Printf("agent %s started in %s (%s)\nfollow it:  gero agent log %s -f\n", id, a.Isolation, a.Workdir, id)
		return nil

	case "ls":
		all := fs.Bool("all", false, "include archived agents")
		parse(fs, args[1:])
		agents, err := c.Agents(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tKIND\tISOLATION\tMAIL\tNAME")
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
			mail := "-"
			if a.Inbox > 0 {
				mail = fmt.Sprint(a.Inbox)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", shownID(a), status, a.Kind, a.Isolation, mail, orDash(a.Name))
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
		sent, err := c.Send(ctx, pos[0], strings.Join(pos[1:], " "))
		if sent.Done != "" {
			fmt.Println(sent.Done)
		}
		return err

	case "approve":
		pos := parse(fs, args[1:])
		if len(pos) != 3 {
			return errors.New("usage: gero agent approve ID APPROVAL accept|decline|cancel")
		}
		d, err := driver.ParseDecision(pos[2])
		if err != nil {
			return err
		}
		return c.Approve(ctx, pos[0], pos[1], d, nil)

	case "answer":
		pos := parse(fs, args[1:])
		if len(pos) < 3 {
			return errors.New("usage: gero agent answer ID APPROVAL ANSWER... (one per question, in order)")
		}
		a, err := c.Agent(ctx, pos[0])
		if err != nil {
			return err
		}
		evs, err := c.Events(ctx, a.ID, 0)
		if err != nil {
			return err
		}
		var qs []event.Question
		for _, e := range evs {
			if e.Kind == event.KindApproval && e.Approval.ID == pos[1] {
				qs = e.Approval.Questions
			}
		}
		if qs == nil {
			return fmt.Errorf("approval %s asks no question", pos[1])
		}
		if len(pos)-2 != len(qs) {
			return fmt.Errorf("it asks %d question(s); give one answer each, in order", len(qs))
		}
		answers := map[string]string{}
		for i, q := range qs {
			answers[q.Question] = pos[2+i]
		}
		return c.Approve(ctx, a.ID, pos[1], driver.Accept, answers)

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
			return errors.New("usage: gero agent set ID [--model M] [--effort E] [--approvals never|auto|ask|default]")
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

	case "rename":
		pos := parse(fs, args[1:])
		if len(pos) < 2 {
			return errors.New("usage: gero agent rename ID NAME...")
		}
		name := strings.Join(pos[1:], " ")
		a, err := c.UpdateAgent(ctx, pos[0], api.UpdateAgent{Name: &name})
		if err != nil {
			return err
		}
		fmt.Printf("agent %s is now %q\n", shownID(a), a.Name)
		return nil

	case "id":
		clear := fs.Bool("clear", false, "take its own ID away: it goes by the one gero gave it")
		pos := parse(fs, args[1:])
		if len(pos) != 2 && !(len(pos) == 1 && *clear) {
			return errors.New("usage: gero agent id AGENT NEW-ID | gero agent id AGENT --clear")
		}
		handle := ""
		if !*clear {
			handle = pos[1]
		}
		a, err := c.UpdateAgent(ctx, pos[0], api.UpdateAgent{Handle: &handle})
		if err != nil {
			return err
		}
		if handle == "" {
			fmt.Printf("agent %s goes by %s again\n", pos[0], a.ID)
		} else {
			fmt.Printf("agent %s is now %s\n", a.ID, a.Handle)
		}
		return nil

	case "inbox":
		clear := fs.Bool("clear", false, "take every waiting mail out, unread")
		rm := fs.Int64("rm", 0, "take mail `N` out, unread")
		pos := parse(fs, args[1:])
		if len(pos) != 1 || (*clear && *rm != 0) {
			return errors.New("usage: gero agent inbox AGENT [--rm N | --clear]")
		}
		switch {
		case *clear:
			n, err := c.ClearInbox(ctx, pos[0])
			if err != nil {
				return err
			}
			fmt.Printf("took %d mail(s) out of %s's inbox unread\n", n, pos[0])
			return nil
		case *rm != 0:
			if err := c.DiscardMail(ctx, pos[0], *rm); err != nil {
				return err
			}
			fmt.Printf("took mail #%d out of %s's inbox unread\n", *rm, pos[0])
			return nil
		}
		mails, err := c.Inbox(ctx, pos[0])
		if err != nil {
			return err
		}
		if len(mails) == 0 {
			fmt.Printf("no mail waits for %s\n", pos[0])
			return nil
		}
		// Oldest first: the order the agent reads them in, one per turn
		// whenever it is idle.
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tFROM\tISOLATION\tSENT\tMAIL")
		for _, m := range mails {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", m.ID, m.From, m.FromIsolation, m.CreatedAt.Local().Format("Jan 2 15:04"), mailLine(m.Text))
		}
		return tw.Flush()

	case "mail":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent mail AGENT   (what it was sent and what it wrote, newest first)")
		}
		mails, err := c.Mail(ctx, pos[0])
		if err != nil {
			return err
		}
		if len(mails) == 0 {
			fmt.Printf("%s has neither sent nor been sent mail\n", pos[0])
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tSENT\tFROM/TO\tSTATE\tMAIL")
		for _, m := range mails {
			with := "← " + m.From + " (" + m.FromIsolation + ")"
			if m.Sent {
				with = "→ " + m.To + " (" + m.ToIsolation + ")"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", m.ID, m.CreatedAt.Local().Format("Jan 2 15:04"), with, m.State, mailLine(m.Text))
		}
		return tw.Flush()

	case "cron":
		add := fs.String("add", "", "add a cron on `SCHEDULE`: a cron expression in the Mac's time (quoted), @every 45m, @daily...; its message follows")
		set := fs.Int64("set", 0, "change cron `N`: --schedule, --wait or --skip, and its message if one follows")
		schedule := fs.String("schedule", "", "with --set: its new `SCHEDULE`")
		wait := fs.Bool("wait", false, "a run that finds the agent busy waits until it is idle")
		skip := fs.Bool("skip", false, "a run that finds the agent busy is skipped (a new cron's default)")
		on := fs.Int64("on", 0, "turn cron `N` on")
		off := fs.Int64("off", 0, "turn cron `N` off")
		rm := fs.Int64("rm", 0, "delete cron `N`")
		pos := parse(fs, args[1:])
		usage := errors.New(`usage: gero agent cron AGENT                                   its crons
       gero agent cron AGENT --add SCHEDULE [--wait] MESSAGE...
       gero agent cron AGENT --set N [--schedule SCHEDULE] [--wait|--skip] [MESSAGE...]
       gero agent cron AGENT --on N | --off N | --rm N`)
		acts := 0
		for _, x := range []bool{*add != "", *set != 0, *on != 0, *off != 0, *rm != 0} {
			if x {
				acts++
			}
		}
		if len(pos) < 1 || acts > 1 || (*wait && *skip) || (*schedule != "" && *set == 0) {
			return usage
		}
		agent, msg := pos[0], strings.Join(pos[1:], " ")
		var busy *string
		if *wait {
			busy = new(store.CronWait)
		} else if *skip {
			busy = new(store.CronSkip)
		}
		var req api.PutCron
		var n int64
		switch {
		case *add != "":
			if msg == "" {
				return usage
			}
			cr, err := c.AddCron(ctx, agent, api.PutCron{Schedule: add, Prompt: &msg, IfBusy: busy})
			if err != nil {
				return err
			}
			fmt.Printf("cron #%d of %s: %s\n", cr.ID, agent, cronLine(cr))
			return nil
		case *set != 0:
			n, req.IfBusy = *set, busy
			if *schedule != "" {
				req.Schedule = schedule
			}
			if msg != "" {
				req.Prompt = &msg
			}
		case *on != 0 || *off != 0:
			n, req.Enabled = max(*on, *off), new(*on != 0)
		case *rm != 0:
			if err := c.DeleteCron(ctx, agent, *rm); err != nil {
				return err
			}
			fmt.Printf("deleted cron #%d of %s\n", *rm, agent)
			return nil
		}
		if n != 0 {
			if len(pos) > 1 && *set == 0 {
				return usage
			}
			cr, err := c.UpdateCron(ctx, agent, n, req)
			if err != nil {
				return err
			}
			fmt.Printf("cron #%d of %s: %s\n", cr.ID, agent, cronLine(cr))
			return nil
		}
		if len(pos) != 1 {
			return usage
		}
		v, err := c.Crons(ctx, agent)
		if err != nil {
			return err
		}
		if len(v.Crons) == 0 {
			fmt.Printf("%s has no cron\n", agent)
			return nil
		}
		fmt.Printf("times are the Mac's: %s\n", v.Zone)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tSCHEDULE\tIF BUSY\tNEXT\tLAST\tMESSAGE")
		for _, cr := range v.Crons {
			next := "off"
			if cr.Enabled && cr.NextAt != nil {
				next = cr.NextAt.Local().Format("Jan 2 15:04")
			}
			last := "—"
			if cr.LastAt != nil {
				last = cr.LastAt.Local().Format("Jan 2 15:04") + " " + cr.LastResult
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", cr.ID, cr.Schedule, cr.IfBusy, next, last, mailLine(cr.Prompt))
		}
		return tw.Flush()

	case "archive":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent archive ID")
		}
		return c.Archive(ctx, pos[0])

	case "restore":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent restore ID   (an archived agent)")
		}
		a, err := c.Restore(ctx, pos[0])
		if err != nil {
			return err
		}
		fmt.Printf("restored agent %s\n", shownID(a))
		return nil

	case "rm":
		purge := fs.Bool("purge", false, "also remove its workspace and its CLI's session transcript from the isolation")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero agent rm ID [--purge]   (an archived agent)")
		}
		a, err := c.Agent(ctx, pos[0])
		if err != nil {
			return err
		}
		// The daemon resolves the prefix itself, refusing an ambiguous one.
		if err := c.DeleteAgent(ctx, pos[0], *purge); err != nil {
			return err
		}
		if *purge {
			fmt.Printf("deleted agent %s, with its workspace and session transcript in %s\n", shownID(a), a.Isolation)
		} else {
			fmt.Printf("deleted agent %s; its workspace %s stays in %s (--purge removes it)\n", shownID(a), a.Workdir, a.Isolation)
		}
		return nil
	}
	return fmt.Errorf("unknown agent command %q", args[0])
}

// mailLine is a mail's first line, short enough for a table.
// cronLine is a cron that was just set: when it comes next, and what a run
// that finds the agent busy does.
func cronLine(c store.Cron) string {
	if !c.Enabled || c.NextAt == nil {
		return c.Schedule + ", off"
	}
	busy := "skipped"
	if c.IfBusy == store.CronWait {
		busy = "waits"
	}
	return fmt.Sprintf("%s, next %s; a run that finds it busy %s", c.Schedule, c.NextAt.Local().Format("Mon Jan 2 15:04"), busy)
}

func mailLine(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if r := []rune(first); len(r) > 60 {
		first = string(r[:60]) + "…"
	}
	return first
}

// shownID is the ID an agent goes by: its own, or the key gero gave it.
func shownID(a api.Agent) string { return orStr(a.Handle, a.ID) }

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
		for i, q := range a.Questions {
			p.line("", fmt.Sprintf("%d. %s", i+1, q.Question))
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			if len(labels) > 0 {
				how := "one of"
				if q.MultiSelect {
					how = "any of, joined with \", \":"
				}
				p.line("", fmt.Sprintf("   %s %s (or your own words)", how, strings.Join(labels, " · ")))
			}
		}
		if a.Questions != nil {
			p.line("", fmt.Sprintf("gero agent answer %s %s ANSWER... · gero agent approve %s %s decline|cancel", e.Agent, a.ID, e.Agent, a.ID))
		} else {
			p.line("", fmt.Sprintf("gero agent approve %s %s accept|decline|cancel", e.Agent, a.ID))
		}
	case event.KindApprovalDone:
		p.line("approval", e.Approval.ID+" "+e.Approval.Decision)
		for q, ans := range e.Approval.Answers {
			p.line("", q+" → "+ans)
		}
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
