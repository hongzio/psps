package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/paths"
)

// accountCmd manages agent CLI accounts: a credential defined once on the
// Mac and used by any number of isolations (`gero iso account`).
func accountCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero account ls|add|set|rm")
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	fs := flag.NewFlagSet("account "+args[0], flag.ExitOnError)
	switch args[0] {
	case "ls":
		as, err := c.Accounts(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tCLI\tUSED BY\tSECRET")
		for _, a := range as {
			secret := "stored"
			if a.SecretError != "" {
				secret = "missing"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.Kind, a.Tool, orDash(strings.Join(a.UsedBy, ",")), secret)
		}
		return tw.Flush()

	case "add":
		kind := fs.String("kind", "", "what it holds: "+accountKinds())
		pos := parse(fs, args[1:])
		if len(pos) != 1 || *kind == "" {
			return errors.New("usage: gero account add NAME --kind KIND  (" + accountKinds() + ")\n" + kindHelp())
		}
		secret, err := promptSecret("account", pos[0], []string{"token"})
		if err != nil {
			return err
		}
		a, err := c.AddAccount(ctx, pos[0], *kind, secret["token"])
		if err != nil {
			return err
		}
		fmt.Printf("added account %s (%s), stored in %s\n", a.Name, a.Kind, p.Credentials())
		fmt.Printf("use it in an isolation: gero iso account ISOLATION %s %s\n", a.Tool, a.Name)
		return nil

	case "set":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero account set NAME  (replaces its secret)")
		}
		secret, err := promptSecret("account", pos[0], []string{"token"})
		if err != nil {
			return err
		}
		a, err := c.SetAccountSecret(ctx, pos[0], secret["token"])
		if err != nil {
			return err
		}
		fmt.Printf("replaced the secret of account %s", a.Name)
		if len(a.UsedBy) > 0 {
			fmt.Printf("; idle agents in %s restart with it", strings.Join(a.UsedBy, ", "))
		}
		fmt.Println()
		return nil

	case "rm":
		force := fs.Bool("force", false, "also stop every isolation using it from doing so")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero account rm NAME [--force]")
		}
		if err := c.DeleteAccount(ctx, pos[0], *force); err != nil {
			return err
		}
		fmt.Printf("removed account %s and its secret\n", pos[0])
		return nil
	}
	return fmt.Errorf("unknown account command %q", args[0])
}

func accountKinds() string {
	var names []string
	for _, k := range api.AccountKinds {
		names = append(names, k.Name)
	}
	return strings.Join(names, " | ")
}

func kindHelp() string {
	var b strings.Builder
	for _, k := range api.AccountKinds {
		fmt.Fprintf(&b, "  %-18s %s for %s, as %s\n", k.Name, k.Summary, k.Tool, k.Env)
	}
	return strings.TrimRight(b.String(), "\n")
}
