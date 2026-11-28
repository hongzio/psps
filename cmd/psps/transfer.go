package main

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/hongzio/psps/internal/paths"
)

const transferUsage = `usage:
  psps transfer ls               the directions agents may ask for files to be moved in
  psps transfer allow FROM TO    let them ask for files from isolation FROM to TO's inbox; each still waits on your yes
  psps transfer deny FROM TO     no longer; transfers asked for along it wait, or are declined

An agent asks for a transfer as an ask (psps ask); psps ask yes N copies the
file, as long as it is still what the ask showed you.`

// transferCmd keeps the routes files may be transferred along between
// isolations (the transfer asks).
func transferCmd(p paths.Paths, args []string) error {
	verb := "ls"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	if verb == "help" || verb == "-h" || verb == "--help" {
		fmt.Println(transferUsage)
		return nil
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	switch verb {
	case "ls":
		if len(args) != 0 {
			return errors.New(transferUsage)
		}
		rs, err := c.Routes(ctx)
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			fmt.Println("No transfers are allowed. psps transfer allow FROM TO allows them from one isolation to another.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "FROM\tTO\tSINCE")
		for _, r := range rs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.From, r.To, r.CreatedAt.Local().Format("Jan 2 15:04"))
		}
		return tw.Flush()
	case "allow":
		if len(args) != 2 {
			return errors.New("usage: psps transfer allow FROM TO")
		}
		r, err := c.AllowRoute(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Agents may ask for files from %s to be moved to %s's inbox; each waits on your yes.\n", r.From, r.To)
		return nil
	case "deny":
		if len(args) != 2 {
			return errors.New("usage: psps transfer deny FROM TO")
		}
		if err := c.DenyRoute(ctx, args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("No more transfers from %s to %s.\n", args[0], args[1])
		return nil
	}
	return fmt.Errorf("unknown transfer command %q\n\n%s", verb, transferUsage)
}
