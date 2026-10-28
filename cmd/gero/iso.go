package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/services"
)

func isoCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gero iso create|ls|upgrade|rm|env|login|logout|account|shell|verify|services|expose|unexpose")
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	fs := flag.NewFlagSet("iso "+args[0], flag.ExitOnError)
	switch args[0] {
	case "create":
		cpus := fs.Int("cpus", 0, "CPUs for the container (default: container's)")
		memory := fs.String("memory", "4G", "memory for the container")
		fence := fs.String("fence", "on", "on: agents start only behind a verified network fence; off: let it reach the Mac and LAN")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero iso create NAME [--cpus N] [--memory 4G] [--fence off]")
		}
		fmt.Println("creating isolation", pos[0], "(the first one builds the base image; that takes a few minutes)")
		iso, err := c.CreateIsolation(ctx, api.CreateIsolation{Name: pos[0], CPUs: *cpus, Memory: *memory, Fence: *fence})
		if err != nil {
			return err
		}
		fmt.Printf("isolation %s is up (container %s, network %s, image %s)\n", iso.Name, iso.Container, iso.Network, iso.Image)
		fmt.Printf("log the agents in:  gero iso login %s claude   /   gero iso login %s codex\n", iso.Name, iso.Name)
		if v, err := c.Isolation(ctx, iso.Name); err == nil && v.FenceState != "verified" && v.FenceState != "off" {
			fmt.Println("network fence:", v.FenceState)
		}
		return nil

	case "ls":
		isos, err := c.Isolations(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATE\tFENCE\tPROXIES\tIMAGE\tENV")
		short := func(s string) string {
			if r := []rune(s); len(r) > 60 {
				return string(r[:59]) + "…"
			}
			return orDash(s)
		}
		for _, iso := range isos {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", iso.Name, iso.State, short(iso.FenceState), short(iso.Proxies), iso.Image, strings.Join(iso.EnvKeys, ","))
		}
		return tw.Flush()

	case "upgrade":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero iso upgrade NAME")
		}
		iso, err := c.UpgradeIsolation(ctx, pos[0])
		if err != nil {
			return err
		}
		fmt.Printf("isolation %s now runs %s; sessions and workspaces were kept\n", iso.Name, iso.Image)
		return nil

	case "rm":
		purge := fs.Bool("purge", false, "also delete the volumes: every session and workspace in it")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero iso rm NAME [--purge]")
		}
		return c.DeleteIsolation(ctx, pos[0], *purge)

	case "env":
		pos := parse(fs, args[1:])
		if len(pos) < 2 {
			return errors.New("usage: gero iso env NAME KEY=VALUE... (KEY= removes)")
		}
		set := api.SetEnv{}
		for _, kv := range pos[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return fmt.Errorf("%q is not KEY=VALUE", kv)
			}
			if v == "" {
				set[k] = nil
			} else {
				v := v
				set[k] = &v
			}
		}
		if err := c.SetEnv(ctx, pos[0], set); err != nil {
			return err
		}
		fmt.Println("saved; agents pick it up the next time their process starts")
		return nil

	case "verify":
		asJSON := fs.Bool("json", false, "print the checks as JSON")
		names := parse(fs, args[1:])
		if len(names) == 0 {
			isos, err := c.Isolations(ctx)
			if err != nil {
				return err
			}
			for _, iso := range isos {
				names = append(names, iso.Name)
			}
			if len(names) == 0 {
				return errors.New("no isolations to verify; create one with `gero iso create NAME`")
			}
		}
		var reports []api.Verify
		for _, n := range names {
			if !*asJSON {
				fmt.Fprintf(os.Stderr, "measuring %s…\n", n)
			}
			v, err := c.Verify(ctx, n)
			if err != nil {
				return err
			}
			reports = append(reports, v)
		}
		code := renderVerify(os.Stdout, reports, *asJSON)
		if code != api.VerifyOK {
			return exitCode(code)
		}
		return nil

	case "services":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero iso services NAME")
		}
		xs, err := c.Exposures(ctx, pos[0])
		if err != nil {
			return err
		}
		reg := services.Registry()
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tSTATE\tENVS\tDEFAULT\tSETTINGS\tIN THE ISOLATION")
		for _, x := range xs {
			where := ""
			if svc, err := reg.Get(x.Service); err == nil {
				var addrs []string
				for _, port := range svc.Ports() {
					addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port))
				}
				where = strings.Join(addrs, ",")
			}
			if !x.Enabled {
				fmt.Fprintf(tw, "%s\toff\t-\t-\t-\t-\n", x.Service)
				continue
			}
			envs := strings.Join(x.Envs, ",")
			if len(x.Missing) > 0 {
				envs += " (gone: " + strings.Join(x.Missing, ",") + ")"
			}
			fmt.Fprintf(tw, "%s\ton\t%s\t%s\t%s\t%s\n", x.Service, orDash(envs), orDash(x.Default), compact(x.Settings), where)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if iso, err := c.Isolation(ctx, pos[0]); err == nil && iso.Proxies != "" && iso.Proxies != "up" {
			fmt.Println("proxy tunnel:", iso.Proxies)
		}
		return nil

	case "expose":
		if len(args) < 3 {
			return errors.New("usage: gero iso expose NAME SERVICE [ENV,...] [--default-env ENV] [flags]  (gero env help SERVICE)")
		}
		svc, err := services.Registry().Get(args[2])
		if err != nil {
			return err
		}
		var def string
		patch, pos, err := parsePatch("iso expose", svc.Settings(), append([]string{args[1]}, args[3:]...), func(fs *flag.FlagSet) {
			fs.StringVar(&def, "default-env", "", "the env the unsuffixed variables and bare addresses use")
		})
		if err != nil {
			return err
		}
		if len(pos) < 1 || len(pos) > 2 {
			return errors.New("usage: gero iso expose NAME SERVICE [ENV,...] [--default-env ENV]")
		}
		req := api.Expose{Default: def, Settings: patch}
		if len(pos) == 2 {
			req.Envs = []string{}
			for _, n := range strings.Split(pos[1], ",") {
				if n = strings.TrimSpace(n); n != "" {
					req.Envs = append(req.Envs, n)
				}
			}
		}
		x, err := c.Expose(ctx, pos[0], svc.Name(), req)
		if err != nil {
			return err
		}
		fmt.Printf("%s is on for %s: envs %s%s; settings %s\n", svc.Name(), pos[0], orStr(strings.Join(x.Envs, ","), "(none)"),
			map[bool]string{true: ", default " + x.Default}[x.Default != ""], compact(x.Settings))
		var addrs []string
		for _, port := range svc.Ports() {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port))
		}
		fmt.Printf("inside the isolation: %s; agents pick up its environment at their next idle moment\n", strings.Join(addrs, ", "))
		return nil

	case "unexpose":
		pos := parse(fs, args[1:])
		if len(pos) != 2 {
			return errors.New("usage: gero iso unexpose NAME SERVICE")
		}
		if err := c.Unexpose(ctx, pos[0], pos[1]); err != nil {
			return err
		}
		fmt.Printf("%s is off for %s; its files and variables are removed from the isolation\n", pos[1], pos[0])
		return nil

	case "login":
		pos := parse(fs, args[1:])
		switch len(pos) {
		case 1:
			return printCLIs(ctx, c, pos[0], os.Stdout)
		case 2:
			return loginCLI(ctx, c, pos[0], pos[1], os.Stdin, os.Stdout)
		}
		return errors.New("usage: gero iso login NAME [claude|codex]  (no CLI: show both logins)")

	case "logout":
		pos := parse(fs, args[1:])
		if len(pos) != 2 {
			return errors.New("usage: gero iso logout NAME claude|codex")
		}
		if err := c.CLILogout(ctx, pos[0], pos[1]); err != nil {
			return err
		}
		fmt.Printf("%s is logged out in %s; a token set with `gero iso env` stays\n", pos[1], pos[0])
		return nil

	case "account":
		pos := parse(fs, args[1:])
		switch len(pos) {
		case 2:
			iso, err := c.Isolation(ctx, pos[0])
			if err != nil {
				return err
			}
			if a := iso.Accounts[driver.Kind(pos[1])]; a != "" {
				fmt.Printf("%s in %s uses account %s\n", pos[1], pos[0], a)
			} else {
				fmt.Printf("%s in %s uses a login inside it (gero iso login %s)\n", pos[1], pos[0], pos[0])
			}
			return nil
		case 3:
			account := pos[2]
			if account == "-" {
				account = ""
			}
			if err := c.UseAccount(ctx, pos[0], pos[1], account); err != nil {
				return err
			}
			if account == "" {
				fmt.Printf("%s in %s goes back to a login inside it; its idle agents restart\n", pos[1], pos[0])
			} else {
				fmt.Printf("%s in %s uses account %s; its idle agents restart with it\n", pos[1], pos[0], account)
			}
			return nil
		}
		return errors.New("usage: gero iso account NAME claude|codex [ACCOUNT|-]")

	case "shell":
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: gero iso shell NAME")
		}
		iso, err := c.Isolation(ctx, pos[0])
		if err != nil {
			return err
		}
		cmd, err := interactive(ctx, iso.Container, []string{"bash", "-l"})
		if err != nil {
			return err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("unknown iso command %q", args[0])
}

// interactive builds argv in the isolation as the agent user, for this
// terminal. It talks to `container` directly: a shell is between the user
// and the isolation.
func interactive(ctx context.Context, container string, argv []string) (*exec.Cmd, error) {
	cli, err := isolation.FindCLI()
	if err != nil {
		return nil, err
	}
	return cli.Exec(ctx, container, isolation.ExecOpts{Interactive: true, TTY: true, User: isolation.AgentUser,
		Workdir: isolation.AgentHome, Env: []string{"HOME=" + isolation.AgentHome}}, argv...), nil
}
