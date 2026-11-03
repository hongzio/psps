// psps orchestrates coding agents in isolated containers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/client"
	"github.com/hongzio/psps/internal/daemon"
	"github.com/hongzio/psps/internal/launchd"
	"github.com/hongzio/psps/internal/paths"
)

const usage = `psps — run coding agents in isolated containers

  psps                          start the daemon if it is not running
  psps daemon run|status|stop|uninstall
  psps version
  psps backup FILE              the store, settings, credentials and skills in one file; the daemon may run
  psps restore FILE [--force]   put one back, with no daemon running; --force replaces a store that is here

  sudo psps pf install           install the network fence helper (keeps isolations off the Mac and LAN)
  sudo psps pf uninstall
  psps pf status [-v]

  psps iso create NAME [--cpus N] [--memory 4G] [--swap 8G] [--fence off]
  psps iso ls
  psps iso rename NAME NEW-NAME     nothing else changes; agents keep running
  psps iso set NAME [--cpus N] [--memory 8G] [--swap 8G]   resize the VM (replaces the container, keeping volumes) or set its swap
  psps iso upgrade NAME         move onto the current base image, keeping volumes
  psps iso rm NAME [--purge]
  psps iso env NAME KEY=VALUE... | KEY=
  psps iso login NAME [claude|codex]   log an agent CLI in inside it; no CLI: show both logins
  psps iso logout NAME claude|codex
  psps iso account NAME claude|codex [ACCOUNT|-]   the account a CLI uses there; - goes back to a login inside
  psps iso shell NAME
  psps iso verify [NAME...] [--json]   measure the guarantees now; exit 0 ok / 1 isolation / 2 config
  psps iso kill NAME                   SIGKILL its VM, for one that no longer answers (stop hangs too)
  psps iso start NAME                  bring it up again; one with live agents comes up by itself
  psps iso services NAME        which credential proxies the isolation has
  psps iso expose NAME SERVICE [ENV,...] [--default-env ENV] [flags]
  psps iso unexpose NAME SERVICE

  psps account ls               agent CLI accounts: defined once, used by any number of isolations
  psps account add NAME --kind claude-token|anthropic-api-key   prompts; or one line on stdin
  psps account set NAME         replace its secret
  psps account rm NAME [--force]

  psps env ls [SERVICE]         environments a credential proxy can serve
  psps env add|edit SERVICE NAME [flags]   (psps env help SERVICE lists them)
  psps env rm SERVICE NAME [--force]
  psps env secret SERVICE NAME [--clear]   prompts; or one line per field on stdin
  psps env choices SERVICE NAME FIELD   what a field can list, looked up with the env's credential (git: repos)
  psps env check SERVICE NAME   exercise it from the host, now

  psps agent new --iso NAME --kind claude|codex [--id ID] [--team TEAM] [--repo URL] [--workdir DIR] [--model M] [--effort E] [--approvals never|auto|ask|default] [PROMPT...]
  psps agent ls [--all]
  psps agent log ID [-f]
  psps agent send ID TEXT...
  psps agent inbox ID [--rm N | --clear]   mail from other agents, read one per turn when idle
  psps agent mail ID   its mail history: what it was sent and what it wrote
  psps agent approve ID APPROVAL accept|decline|cancel
  psps agent answer ID APPROVAL ANSWER...   answer a question the agent asks, one answer per question
  psps agent interrupt ID
  psps agent set ID [--model M] [--effort E] [--approvals never|auto|ask|default]
  psps agent rename ID NAME...   only the name changes; a running turn carries on
  psps agent id ID NEW-ID|--clear   an ID of your own, unique among all agents; usable wherever an ID is
  psps agent team ID TEAM|--clear   its team, the sidebar's group it is in and moves within; a new name makes one
  psps agent archive ID
  psps agent fork ID            a new agent in its directory, its conversation copied; no crons or mail
  psps agent restore ID         an archived one, back where it left off; a live session of its line is archived for it
  psps agent rm ID [--purge]    an archived one; --purge also removes its workspace and session transcript

  psps team ls                  teams: named groups of agents in the sidebar
  psps team new NAME...
  psps team rename TEAM NAME...
  psps team rm TEAM             its agents stay, in no team, where it was
  psps team lead TEAM AGENT [--budget N] [--eval CMD] [--eval-dir DIR] [--eval-iso ISO] [--target X] [--patience N] [--max-live N] [--turn-minutes N] [--plan-ceiling PCT]
                                its lead: makes agents of its own in the team and hears when their turns end; --clear: none
  psps team show TEAM [--json]  its run: spending, the eval's scores, the lead's agents
  psps team eval TEAM           run its eval now; the result goes to the lead
  psps team reset-best TEAM     the evals so far count for no best; the next scored is it
  psps team resume TEAM         a run psps stopped (budget, target, no new best) goes on
  psps team help                how a team and its lead work, from a first team on

  psps tui [--keys]             the terminal UI; --keys prints a keys.toml with every default
  psps web [--url] [--rotate]   open the web UI signed in (and print the phone link via Tailscale)
`

func main() {
	err := run(os.Args[1:])
	var code exitCode
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "psps:", err)
		os.Exit(1)
	}
}

// exitCode ends psps with that status and nothing printed: the command has
// already said everything (`psps iso verify`).
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

func run(args []string) error {
	p := paths.Default()
	if len(args) == 0 {
		return ensureDaemon(p, true)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println("psps", api.Version)
		return nil
	case "daemon":
		return daemonCmd(p, args[1:])
	case "backup":
		return backupCmd(p, args[1:])
	case "restore":
		return restoreCmd(p, args[1:])
	case "iso", "isolation":
		return isoCmd(p, args[1:])
	case "agent":
		return agentCmd(p, args[1:])
	case "env":
		return envCmd(p, args[1:])
	case "account":
		return accountCmd(p, args[1:])
	case "team":
		return teamCmd(p, args[1:])
	case "pf":
		return pfCmd(args[1:])
	case "pfd":
		return pfdCmd(args[1:])
	case "tui":
		return tuiCmd(p, args[1:])
	case "web":
		return webCmd(p, args[1:])
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
}

// parse handles flags anywhere among the positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ensureDaemon makes sure a daemon answers on the socket. Normally that
// means registering the LaunchAgent, so the daemon also comes back after a
// crash or a reboot. With PSPS_HOME set (development) it forks a plain
// background daemon instead, leaving the user's launchd alone.
//
// A daemon launchd runs from this very binary but on another version is one
// the binary was replaced under (`brew upgrade psps`): it is restarted onto
// the new one. Its agents carry on, as they do across any daemon restart.
func ensureDaemon(p paths.Paths, verbose bool) error {
	c := client.New(p.Socket())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	h, err := c.Health(ctx)
	cancel()
	exe, exeErr := os.Executable()
	if err == nil {
		if h.Version == api.Version || exeErr != nil || os.Getenv("PSPS_HOME") != "" || !launchd.Runs(exe, p) {
			if verbose {
				fmt.Printf("psps daemon is running (pid %d)\n", h.PID)
			}
			return nil
		}
		if err := launchd.Restart(); err != nil {
			return err
		}
		return waitDaemon(c, p, func(h api.Health) bool { return h.Version == api.Version }, func(h api.Health) {
			if verbose {
				fmt.Printf("psps daemon restarted on %s (pid %d)\n", api.Version, h.PID)
			}
		})
	}
	if exeErr != nil {
		return exeErr
	}
	if os.Getenv("PSPS_HOME") != "" {
		if err := p.Ensure(); err != nil {
			return err
		}
		logf, err := os.OpenFile(p.Log(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "daemon", "run")
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		cmd.Process.Release()
	} else if err := launchd.Ensure(exe, p); err != nil {
		return err
	}
	return waitDaemon(c, p, func(api.Health) bool { return true }, func(h api.Health) {
		if verbose {
			fmt.Printf("psps daemon started (pid %d)\n", h.PID)
		}
	})
}

// waitDaemon waits for a daemon to answer as want, then hands it to up.
func waitDaemon(c *client.Client, p paths.Paths, want func(api.Health) bool, up func(api.Health)) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		h, err := c.Health(ctx)
		cancel()
		if err == nil && want(h) {
			up(h)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not come up; see %s", p.Log())
}

func connect(p paths.Paths) (*client.Client, error) {
	if err := ensureDaemon(p, false); err != nil {
		return nil, err
	}
	return client.New(p.Socket()), nil
}

func daemonCmd(p paths.Paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: psps daemon run|status|stop|uninstall")
	}
	switch args[0] {
	case "run":
		ctx, cancel := signalContext()
		defer cancel()
		logger := log.New(os.Stderr, "", log.LstdFlags)
		err := daemon.Run(ctx, p, logger)
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			// Under launchd's KeepAlive this exit is retried; a second
			// daemon has nothing to do while the first holds the lock.
			logger.Print(err)
			time.Sleep(time.Second)
		}
		return err
	case "status":
		c := client.New(p.Socket())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h, err := c.Health(ctx)
		reg := "not registered with launchd"
		if launchd.Loaded() {
			reg = "registered with launchd as " + launchd.Label
		}
		if err != nil {
			fmt.Printf("not running; %s\n", reg)
			return nil
		}
		fmt.Printf("running (pid %d, version %s); %s\nsocket %s\nlog    %s\n", h.PID, h.Version, reg, p.Socket(), p.Log())
		return nil
	case "stop", "uninstall":
		// The LaunchAgent belongs to the daemon at the default paths; a
		// development daemon (PSPS_HOME) was never registered and must not
		// take the real one down with it.
		if os.Getenv("PSPS_HOME") == "" {
			stopLaunchd := launchd.Stop
			if args[0] == "uninstall" {
				stopLaunchd = launchd.Uninstall
			}
			if err := stopLaunchd(); err != nil {
				return err
			}
		}
		return stopByLock(p)
	}
	return fmt.Errorf("unknown daemon command %q", args[0])
}

// stopByLock stops a daemon that launchd does not manage, by the pid it
// wrote into its lock file — never by matching a process name.
func stopByLock(p paths.Paths) error {
	b, err := os.ReadFile(p.Lock())
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return nil
	}
	c := client.New(p.Socket())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if h, err := c.Health(ctx); err != nil || h.PID != pid {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}
