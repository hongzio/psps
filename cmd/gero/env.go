package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/services"
)

// Environments are defined once and exposed per isolation. Defining one
// (`gero env`) is a host-level act; exposing it (`gero iso expose`) is where
// an isolation's view changes.

func envCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero env ls|add|edit|rm|secret|choices|check ...  (gero env help SERVICE for its flags)")
	}
	reg := services.Registry()
	if args[0] == "help" {
		return envHelp(reg, args[1:])
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	switch args[0] {
	case "ls", "list":
		service := ""
		if len(args) > 1 {
			service = args[1]
		}
		envs, err := c.Envs(ctx, service)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tNAME\tSECRET\tEXPOSED TO\tDEFINITION")
		for _, e := range envs {
			secret := "-"
			if _, keeps := creds.Fields[e.Service]; keeps {
				secret = "set"
				switch {
				case e.SecretError != "":
					secret = "unreadable"
				case e.CredentialElsewhere:
					secret = "via command"
				case len(e.MissingSecrets) > 0:
					secret = "missing " + strings.Join(e.MissingSecrets, ",")
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Service, e.Name, secret, orDash(strings.Join(e.ExposedTo, ",")), compact(e.Config))
		}
		return tw.Flush()

	case "add", "edit":
		if len(args) < 3 {
			return fmt.Errorf("usage: gero env %s SERVICE NAME [flags]  (gero env help SERVICE)", args[0])
		}
		svc, err := reg.Get(args[1])
		if err != nil {
			return err
		}
		patch, pos, err := parsePatch("env "+args[0]+" "+svc.Name(), svc.Fields(), args[2:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: gero env %s %s NAME [flags]", args[0], svc.Name())
		}
		var e api.Env
		if args[0] == "add" {
			e, err = c.AddEnv(ctx, svc.Name(), pos[0], patch)
		} else {
			e, err = c.EditEnv(ctx, svc.Name(), pos[0], patch)
		}
		if err != nil {
			return err
		}
		for _, w := range e.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		fmt.Printf("%s env %s: %s\n", e.Service, e.Name, compact(e.Config))
		if args[0] == "add" && len(e.MissingSecrets) > 0 {
			fmt.Printf("store its secret with: gero env secret %s %s\n", e.Service, e.Name)
		}
		if len(e.ExposedTo) > 0 {
			fmt.Printf("exposed to %s; their proxies use the new definition now\n", strings.Join(e.ExposedTo, ", "))
		}
		return nil

	case "rm":
		fs := flag.NewFlagSet("env rm", flag.ExitOnError)
		force := fs.Bool("force", false, "also unexpose it from every isolation that serves it")
		pos := parse(fs, args[1:])
		if len(pos) != 2 {
			return errors.New("usage: gero env rm SERVICE NAME [--force]")
		}
		if err := c.DeleteEnv(ctx, pos[0], pos[1], *force); err != nil {
			return err
		}
		fmt.Printf("removed %s/%s\n", pos[0], pos[1])
		if _, keeps := creds.Fields[pos[0]]; keeps {
			fmt.Printf("its secret, if any, is still in %s; `gero env secret %s %s --clear` deletes it\n",
				p.Credentials(), pos[0], pos[1])
		}
		return nil

	case "secret":
		fs := flag.NewFlagSet("env secret", flag.ExitOnError)
		clear := fs.Bool("clear", false, "remove the stored secret")
		pos := parse(fs, args[1:])
		if len(pos) != 2 {
			return errors.New("usage: gero env secret SERVICE NAME [--clear]")
		}
		service, name := pos[0], pos[1]
		if *clear {
			if err := c.SetSecret(ctx, service, name, nil); err != nil {
				return err
			}
			fmt.Printf("removed the secret for %s/%s\n", service, name)
			return nil
		}
		fields, keeps := creds.Fields[service]
		if !keeps {
			return fmt.Errorf("%s keeps no secret in the credentials file", service)
		}
		if creds.LoginServices[service] {
			return browserLogin(ctx, c, service, name, os.Stdin, os.Stderr)
		}
		values, err := promptSecret(service, name, fields)
		if err != nil {
			return err
		}
		if err := c.SetSecret(ctx, service, name, values); err != nil {
			return err
		}
		fmt.Printf("stored [%s %s] in %s; proxies read it on their next request\n", service, name, p.Credentials())
		return nil

	case "choices":
		if len(args) != 4 {
			return errors.New("usage: gero env choices SERVICE NAME FIELD  (e.g. gero env choices git work repos)")
		}
		cs, err := c.Choices(ctx, args[1], args[3], api.ChoicesRequest{Env: args[2]})
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, ch := range cs {
			fmt.Fprintf(tw, "%s\t%s\n", ch.Value, ch.Note)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%d; list them with --%s, e.g. gero env edit %s %s --%s VALUE\n", len(cs), flagOf(args[1], args[3]), args[1], args[2], flagOf(args[1], args[3]))
		return nil

	case "check":
		if len(args) != 3 {
			return errors.New("usage: gero env check SERVICE NAME")
		}
		res, err := c.CheckEnv(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("%s/%s\n", args[1], args[2])
		for _, l := range strings.Split(strings.TrimRight(res.Output, "\n"), "\n") {
			if l != "" {
				fmt.Println("  " + l)
			}
		}
		if res.Error != "" {
			return errors.New(res.Error)
		}
		return nil
	}
	return fmt.Errorf("unknown env command %q", args[0])
}

func envHelp(reg *proxy.Registry, args []string) error {
	if len(args) == 0 {
		for _, s := range reg.All() {
			fmt.Printf("  %-10s %s\n", s.Name(), s.Summary())
		}
		return nil
	}
	svc, err := reg.Get(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("gero env add|edit %s NAME [flags] — %s\n\n", svc.Name(), svc.Summary())
	printFields(svc.Fields())
	if len(svc.Settings()) > 0 {
		fmt.Printf("\ngero iso expose ISOLATION %s [ENV,...] [--default-env NAME] [flags]\n\n", svc.Name())
		printFields(svc.Settings())
	}
	if f, ok := creds.Fields[svc.Name()]; ok {
		fmt.Printf("\nsecret (gero env secret %s NAME): %s\n", svc.Name(), strings.Join(f, ", "))
	}
	return nil
}

func printFields(fields []proxy.Field) {
	for _, f := range fields {
		flagText := "--" + f.Flag
		switch f.Kind {
		case proxy.Bool:
			if f.Neg != "" {
				flagText += " | --" + f.Neg
			}
		case proxy.List:
			flagText += " " + orStr(f.Metavar, "VALUE")
			if f.Clear != "" {
				flagText += "  (--" + f.Clear + ")"
			}
		default:
			flagText += " " + orStr(f.Metavar, strings.ToUpper(string(f.Kind)))
		}
		fmt.Printf("  %-44s %s\n", flagText, f.Help)
		if len(f.Choices) > 0 {
			fmt.Printf("  %-44s one of: %s\n", "", strings.Join(f.Choices, ", "))
		}
	}
}

// listFlag collects a repeated flag.
type listFlag struct {
	vals []string
	set  bool
}

func (l *listFlag) String() string { return strings.Join(l.vals, ",") }
func (l *listFlag) Set(v string) error {
	l.set = true
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			l.vals = append(l.vals, part)
		}
	}
	return nil
}

// parsePatch builds a Patch from the CLI flags a service's fields define.
// Only flags actually given end up in it, so an edit leaves the rest alone.
func parsePatch(name string, fields []proxy.Field, args []string, extra ...func(*flag.FlagSet)) (proxy.Patch, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	type binding struct {
		f       proxy.Field
		str     *string
		num     *int
		yes     *bool
		no      *bool
		list    *listFlag
		cleared *bool
	}
	var bs []*binding
	for _, f := range fields {
		b := &binding{f: f}
		switch f.Kind {
		case proxy.String:
			b.str = fs.String(f.Flag, "", f.Help)
		case proxy.Int:
			b.num = fs.Int(f.Flag, 0, f.Help)
		case proxy.Bool:
			b.yes = fs.Bool(f.Flag, false, f.Help)
			if f.Neg != "" {
				b.no = fs.Bool(f.Neg, false, "the opposite of --"+f.Flag)
			}
		case proxy.List:
			b.list = &listFlag{}
			fs.Var(b.list, f.Flag, f.Help)
			if f.Clear != "" {
				b.cleared = fs.Bool(f.Clear, false, "empty the list")
			}
		}
		bs = append(bs, b)
	}
	for _, e := range extra {
		e(fs)
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return proxy.Patch{}, nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	p := proxy.Patch{Set: map[string]any{}}
	for _, b := range bs {
		f := b.f
		switch f.Kind {
		case proxy.String:
			if given[f.Flag] {
				p.Set[f.Key] = *b.str
			}
		case proxy.Int:
			if given[f.Flag] {
				p.Set[f.Key] = *b.num
			}
		case proxy.Bool:
			if given[f.Flag] && f.Neg != "" && given[f.Neg] {
				return p, nil, fmt.Errorf("--%s and --%s contradict each other", f.Flag, f.Neg)
			}
			if given[f.Flag] {
				p.Set[f.Key] = *b.yes
			} else if f.Neg != "" && given[f.Neg] {
				p.Set[f.Key] = !*b.no
			}
		case proxy.List:
			if b.cleared != nil && *b.cleared {
				p.Clear = append(p.Clear, f.Key)
			}
			if b.list.set {
				p.Set[f.Key] = b.list.vals
			}
		}
	}
	return p, pos, nil
}

func promptSecret(service, name string, fields []string) (map[string]string, error) {
	values := map[string]string{}
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	var lines *bufio.Scanner
	if !tty {
		// Piped: one line per field, in order, so a secret never has to sit
		// in a shell's history or argv.
		lines = bufio.NewScanner(os.Stdin)
	}
	for _, f := range fields {
		var v string
		if tty {
			fmt.Fprintf(os.Stderr, "%s %s %s: ", service, name, f)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return nil, err
			}
			v = string(b)
		} else {
			if !lines.Scan() {
				if err := lines.Err(); err != nil && err != io.EOF {
					return nil, err
				}
				return nil, fmt.Errorf("stdin ended before %s", f)
			}
			v = lines.Text()
		}
		if v = strings.TrimSpace(v); v == "" {
			return nil, fmt.Errorf("%s is empty; nothing stored", f)
		}
		values[f] = v
	}
	return values, nil
}

func compact(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any:
			var items []string
			for _, x := range v {
				items = append(items, fmt.Sprint(x))
			}
			parts = append(parts, k+"="+strings.Join(items, ","))
		default:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " ")
}

func orDash(s string) string { return orStr(s, "-") }

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// flagOf is the CLI flag of a service's field.
func flagOf(service, field string) string {
	if svc, err := services.Registry().Get(service); err == nil {
		for _, f := range svc.Fields() {
			if f.Key == field {
				return f.Flag
			}
		}
	}
	return field
}
