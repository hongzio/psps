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
	"github.com/hongzio/psps/internal/proxy/kanbanproxy"
	"github.com/hongzio/psps/internal/store"
)

const kanbanUsage = `usage:
  psps kanban [ls] [--iso ISO] [--board B] [--status S]   the cards: an isolation's boards, or every board
  psps kanban show K-12                  a card, its comments and its history
  psps kanban new BOARD TITLE... [--body TEXT] [--status S] [--assignee AGENT]
  psps kanban mv K-12 todo|doing|done    move it to another column
  psps kanban edit K-12 [--title T] [--body TEXT] [--assignee AGENT|""] [--board B]
  psps kanban check K-12 N               check the N-th item of its checklist (- [ ] in its body); uncheck K-12 N
  psps kanban comment K-12 TEXT...       write on it
  psps kanban comment-edit N TEXT...     change a comment of yours; comment-rm N deletes it
  psps kanban rm K-12                    take it off its board (its history stays); restore K-12 puts it back
  psps kanban log [K-12] [--iso ISO] [--board B] [--limit N]   the history, newest first

A board is a kanban env: psps env add kanban NAME --color COLOR; an
isolation's agents work on the boards it is given: psps iso expose ISO kanban BOARD,...`

// kanbanCmd reads and keeps the kanban boards, as the operator.
func kanbanCmd(p paths.Paths, args []string) error {
	verb := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	if verb == "help" || verb == "-h" || verb == "--help" {
		fmt.Println(kanbanUsage)
		return nil
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	fs := flag.NewFlagSet("kanban "+verb, flag.ExitOnError)
	card := func(pos []string, usage string) (int64, error) {
		if len(pos) == 0 {
			return 0, errors.New("usage: " + usage)
		}
		return kanbanproxy.ParseCard(pos[0])
	}

	switch verb {
	case "ls":
		iso := fs.String("iso", "", "the boards this isolation is given")
		board := fs.String("board", "", "one board")
		status := fs.String("status", "", "one column: todo, doing or done")
		if pos := parse(fs, args); len(pos) != 0 {
			return errors.New(kanbanUsage)
		}
		v, err := c.Kanban(ctx, *iso, *board, *status)
		if err != nil {
			return err
		}
		if len(v.Boards) == 0 {
			if *iso != "" {
				fmt.Printf("Isolation %s has no boards: psps iso expose %s kanban BOARD,...\n", *iso, *iso)
			} else {
				fmt.Println("No boards yet: psps env add kanban NAME --color COLOR")
			}
			return nil
		}
		if len(v.Cards) == 0 {
			fmt.Println("No cards.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CARD\tBOARD\tSTATUS\tASSIGNEE\tCHECKED\tCOMMENTS\tUPDATED\tTITLE")
		for _, x := range v.Cards {
			checked := "-"
			if x.Tasks > 0 {
				checked = fmt.Sprintf("%d/%d", x.TasksDone, x.Tasks)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", x.Key, x.Board, kanbanproxy.StatusNames[x.Status], orDash(x.AssigneeName),
				checked, x.Comments, x.UpdatedAt.Local().Format("Jan 2 15:04"), x.Title)
		}
		return tw.Flush()

	case "show":
		id, err := card(parse(fs, args), "psps kanban show K-12")
		if err != nil {
			return err
		}
		d, err := c.KanbanCard(ctx, id)
		if err != nil {
			return err
		}
		printCard(d)
		return nil

	case "new":
		body := fs.String("body", "", "what the card is about")
		status := fs.String("status", "", "its column: todo (default), doing or done")
		assignee := fs.String("assignee", "", "an agent's ID")
		pos := parse(fs, args)
		if len(pos) < 2 {
			return errors.New("usage: psps kanban new BOARD TITLE... [--body TEXT] [--status S] [--assignee AGENT]")
		}
		x, err := c.AddKanbanCard(ctx, api.NewCard{Board: pos[0], Title: strings.Join(pos[1:], " "), Body: *body, Status: *status, Assignee: *assignee})
		if err != nil {
			return err
		}
		fmt.Printf("%s on %s, in %s.\n", x.Key, x.Board, kanbanproxy.StatusNames[x.Status])
		return nil

	case "mv":
		pos := parse(fs, args)
		if len(pos) != 2 {
			return errors.New("usage: psps kanban mv K-12 todo|doing|done")
		}
		id, err := card(pos, "")
		if err != nil {
			return err
		}
		x, err := c.UpdateKanbanCard(ctx, id, store.CardPatch{Status: &pos[1]})
		if err != nil {
			return err
		}
		fmt.Printf("%s is in %s.\n", x.Key, kanbanproxy.StatusNames[x.Status])
		return nil

	case "edit":
		var p store.CardPatch
		title := fs.String("title", "", "its title")
		body := fs.String("body", "", "its text")
		assignee := fs.String("assignee", "", `an agent's ID, or "" for none`)
		board := fs.String("board", "", "the board to move it to")
		id, err := card(parse(fs, args), "psps kanban edit K-12 [--title T] [--body TEXT] [--assignee AGENT] [--board B]")
		if err != nil {
			return err
		}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "title":
				p.Title = title
			case "body":
				p.Body = body
			case "assignee":
				p.Assignee = assignee
			case "board":
				p.Board = board
			}
		})
		x, err := c.UpdateKanbanCard(ctx, id, p)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s, on %s, in %s.\n", x.Key, x.Title, x.Board, kanbanproxy.StatusNames[x.Status])
		return nil

	case "check", "uncheck":
		pos := parse(fs, args)
		if len(pos) != 2 {
			return fmt.Errorf("usage: psps kanban %s K-12 N", verb)
		}
		id, err := card(pos, "")
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(pos[1])
		if err != nil || n < 1 {
			return fmt.Errorf("%q is not an item's number (1 is the first)", pos[1])
		}
		x, err := c.CheckKanbanTask(ctx, id, api.CheckTask{Item: n - 1, Checked: verb == "check"})
		if err != nil {
			return err
		}
		fmt.Printf("%s: %d of %d checked.\n", x.Key, x.TasksDone, x.Tasks)
		return nil

	case "comment":
		pos := parse(fs, args)
		if len(pos) < 2 {
			return errors.New("usage: psps kanban comment K-12 TEXT...")
		}
		id, err := card(pos, "")
		if err != nil {
			return err
		}
		cm, err := c.AddKanbanComment(ctx, id, strings.Join(pos[1:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("Comment %d on K-%d.\n", cm.ID, id)
		return nil

	case "comment-edit", "comment-rm":
		pos := parse(fs, args)
		if len(pos) == 0 || (verb == "comment-edit") != (len(pos) > 1) {
			return errors.New("usage: psps kanban comment-edit N TEXT... | comment-rm N")
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a comment number", pos[0])
		}
		var text *string
		if verb == "comment-edit" {
			t := strings.Join(pos[1:], " ")
			text = &t
		}
		if _, err := c.ChangeKanbanComment(ctx, id, text); err != nil {
			return err
		}
		return nil

	case "rm", "restore":
		id, err := card(parse(fs, args), "psps kanban "+verb+" K-12")
		if err != nil {
			return err
		}
		if verb == "rm" {
			x, err := c.DeleteKanbanCard(ctx, id)
			if err == nil {
				fmt.Printf("%s is off %s; psps kanban restore %s puts it back.\n", x.Key, x.Board, x.Key)
			}
			return err
		}
		x, err := c.RestoreKanbanCard(ctx, id)
		if err == nil {
			fmt.Printf("%s is back on %s, in %s.\n", x.Key, x.Board, kanbanproxy.StatusNames[x.Status])
		}
		return err

	case "log":
		iso := fs.String("iso", "", "the boards this isolation is given")
		board := fs.String("board", "", "one board")
		limit := fs.Int("limit", 50, "how many lines")
		pos := parse(fs, args)
		var es []api.KanbanEvent
		if len(pos) > 0 {
			id, err := card(pos, "")
			if err != nil {
				return err
			}
			d, err := c.KanbanCard(ctx, id)
			if err != nil {
				return err
			}
			es = d.History
		} else if es, err = c.KanbanLog(ctx, *iso, *board, *limit); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "WHEN\tCARD\tBOARD\tBY\tWHAT")
		for _, e := range es {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.At.Local().Format("Jan 2 15:04"), e.Key, e.Board, e.By.Name, kanbanChange(e))
		}
		return tw.Flush()
	}
	return fmt.Errorf("unknown kanban command %q\n\n%s", verb, kanbanUsage)
}

// kanbanChange is what a line of history says, in a line.
func kanbanChange(e api.KanbanEvent) string {
	q := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if r := []rune(s); len(r) > 60 {
			s = string(r[:59]) + "…"
		}
		return strconv.Quote(s)
	}
	col := func(s string) string { return orDash(kanbanproxy.StatusNames[s]) }
	switch e.Kind {
	case store.KanbanCreated:
		return "made it, in " + col(e.To)
	case store.KanbanTitle:
		return "title " + q(e.From) + " → " + q(e.To)
	case store.KanbanBody:
		return "changed the text"
	case store.KanbanStatus:
		return col(e.From) + " → " + col(e.To)
	case store.KanbanAssignee:
		return "assignee " + orDash(e.FromName) + " → " + orDash(e.ToName)
	case store.KanbanBoard:
		return "board " + e.From + " → " + e.To
	case store.KanbanChecked:
		return "checked " + q(e.To)
	case store.KanbanUnchecked:
		return "unchecked " + q(e.To)
	case store.KanbanDeleted:
		return "deleted it"
	case store.KanbanRestored:
		return "restored it"
	case store.KanbanCommented:
		return fmt.Sprintf("comment %d: %s", e.Comment, q(e.To))
	case store.KanbanCommentEdited:
		return fmt.Sprintf("edited comment %d: %s → %s", e.Comment, q(e.From), q(e.To))
	case store.KanbanCommentDeleted:
		return fmt.Sprintf("deleted comment %d: %s", e.Comment, q(e.From))
	}
	return e.Kind
}

func printCard(d api.KanbanCardDetail) {
	state := kanbanproxy.StatusNames[d.Status]
	if d.DeletedAt != nil {
		state = "deleted"
	}
	fmt.Printf("%s · %s · %s · made by %s, %s\n", d.Key, d.Board, state, d.CreatedBy.Name, d.CreatedAt.Local().Format("Jan 2 15:04"))
	if d.AssigneeName != "" {
		fmt.Printf("Assignee: %s\n", d.AssigneeName)
	}
	fmt.Printf("\n%s\n", d.Title)
	if d.Body != "" {
		fmt.Printf("\n%s\n", d.Body)
	}
	if len(d.Comments) > 0 {
		fmt.Println("\nComments:")
		for _, cm := range d.Comments {
			edited := ""
			if cm.EditedAt != nil {
				edited = " (edited)"
			}
			fmt.Printf("\n  [%d] %s, %s%s\n", cm.ID, cm.By.Name, cm.CreatedAt.Local().Format("Jan 2 15:04"), edited)
			for _, l := range strings.Split(cm.Text, "\n") {
				fmt.Printf("  %s\n", l)
			}
		}
	}
	fmt.Println("\nHistory:")
	for _, e := range d.History {
		fmt.Printf("  %s  %s  %s\n", e.At.Local().Format("Jan 2 15:04"), e.By.Name, kanbanChange(e))
	}
}
