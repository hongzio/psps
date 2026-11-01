package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/hongzio/gero/internal/paths"
)

// teamCmd manages teams, the sidebar's named groups of live agents. An
// agent joins one with `gero agent team`.
func teamCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero team ls|new|rename|rm")
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	switch args[0] {
	case "ls":
		teams, err := c.Teams(ctx)
		if err != nil {
			return err
		}
		agents, err := c.Agents(ctx)
		if err != nil {
			return err
		}
		members := map[string][]string{}
		for _, a := range agents {
			if a.ArchivedAt == nil && a.TeamID != "" {
				members[a.TeamID] = append(members[a.TeamID], shownID(a))
			}
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tID\tAGENTS")
		for _, t := range teams {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, t.ID, orDash(strings.Join(members[t.ID], ",")))
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
	}
	return fmt.Errorf("unknown team command %q", args[0])
}
