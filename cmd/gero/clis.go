package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/client"
)

// printCLIs is each agent CLI's login in the isolation, as its agents find
// it.
func printCLIs(ctx context.Context, c *client.Client, iso string, out io.Writer) error {
	cs, err := c.CLIs(ctx, iso)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLI\tVERSION\tACCOUNT\tLOGIN")
	for _, cl := range cs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cl.Tool, orDash(cl.Version), orStr(cl.Account, "(inside)"), cliState(cl))
	}
	return tw.Flush()
}

func cliState(c api.CLI) string {
	s := "not logged in"
	switch {
	case c.Error != "":
		s = "unknown: " + c.Error
	case c.LoggedIn:
		s = "logged in: " + orStr(c.Method, "yes")
		if c.Email != "" {
			s += ", " + c.Email
		}
	}
	if l := c.Login; l != nil && (l.State == api.CLILoginOpen || l.State == api.CLILoginVerifying) {
		s += " (a login is in progress)"
	}
	return s
}

// loginCLI logs an agent CLI in inside the isolation. The daemon runs the
// login there, the same one the web and the TUI run; this side shows where
// to go and, for Claude, hands back the code the page ends on.
func loginCLI(ctx context.Context, c *client.Client, iso, tool string, in io.Reader, out io.Writer) error {
	err := runCLILogin(ctx, c, iso, tool, in, out)
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s login cancelled; %s keeps the login it had", tool, iso)
	}
	return err
}

func runCLILogin(ctx context.Context, c *client.Client, iso, tool string, in io.Reader, out io.Writer) error {
	l, err := c.StartCLILogin(ctx, iso, tool)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.CancelCLILogin(cctx, iso, tool)
		}
	}()
	switch {
	case l.State != api.CLILoginOpen:
		return fmt.Errorf("%s login did not start: %s", tool, orStr(l.Message, l.State))
	case l.NeedsCode:
		fmt.Fprintf(out, "Open this address in a browser and sign in:\n\n  %s\n\nThe page ends on a code. Paste it here: ", l.URL)
		code, err := readLine(ctx, in)
		if err != nil {
			fmt.Fprintln(out)
			return err
		}
		if l, err = c.SendCLICode(ctx, iso, tool, code); err != nil {
			return err
		}
	default:
		fmt.Fprintf(out, "Open %s in a browser, sign in, and enter this code:\n\n  %s\n\nIt expires at %s. Waiting…\n",
			l.URL, l.Code, l.Expires.Local().Format("15:04"))
	}
	for l.State == api.CLILoginOpen || l.State == api.CLILoginVerifying {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if l, err = c.CLILogin(ctx, iso, tool); err != nil {
			return err
		}
	}
	finished = true
	if l.State != api.CLILoginDone {
		return fmt.Errorf("%s login failed: %s", tool, l.Message)
	}
	fmt.Fprintf(out, "%s: %s\n%s is logged in in %s; its idle agents restart with the login\n", tool, l.Message, tool, iso)
	return nil
}

// readLine reads one line, or gives up when ctx ends (Ctrl+C).
func readLine(ctx context.Context, in io.Reader) (string, error) {
	got := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			close(got)
			return
		}
		got <- line
	}()
	select {
	case line, ok := <-got:
		if !ok {
			return "", fmt.Errorf("no code was pasted")
		}
		return line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
