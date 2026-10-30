// gero orchestrates coding agents in isolated containers.
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

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/client"
	"github.com/hongzio/gero/internal/daemon"
	"github.com/hongzio/gero/internal/launchd"
	"github.com/hongzio/gero/internal/paths"
)

const usage = `gero — run coding agents in isolated containers

  gero                          start the daemon if it is not running
  gero daemon run|status|stop|uninstall
  gero version

  sudo gero pf install           install the network fence helper (keeps isolations off the Mac and LAN)
  sudo gero pf uninstall
  gero pf status [-v]

  gero iso create NAME [--cpus N] [--memory 4G] [--fence off]
  gero iso ls
  gero iso rename NAME NEW-NAME     nothing else changes; agents keep running
  gero iso set NAME [--cpus N] [--memory 8G]   resize the VM: replaces the container, keeping volumes
  gero iso upgrade NAME         move onto the current base image, keeping volumes
  gero iso rm NAME [--purge]
  gero iso env NAME KEY=VALUE... | KEY=
  gero iso login NAME [claude|codex]   log an agent CLI in inside it; no CLI: show both logins
  gero iso logout NAME claude|codex
  gero iso account NAME claude|codex [ACCOUNT|-]   the account a CLI uses there; - goes back to a login inside
  gero iso shell NAME
  gero iso verify [NAME...] [--json]   measure the guarantees now; exit 0 ok / 1 isolation / 2 config
  gero iso services NAME        which credential proxies the isolation has
  gero iso expose NAME SERVICE [ENV,...] [--default-env ENV] [flags]
  gero iso unexpose NAME SERVICE

  gero account ls               agent CLI accounts: defined once, used by any number of isolations
  gero account add NAME --kind claude-token|anthropic-api-key   prompts; or one line on stdin
  gero account set NAME         replace its secret
  gero account rm NAME [--force]

  gero env ls [SERVICE]         environments a credential proxy can serve
  gero env add|edit SERVICE NAME [flags]   (gero env help SERVICE lists them)
  gero env rm SERVICE NAME [--force]
  gero env secret SERVICE NAME [--clear]   prompts; or one line per field on stdin
  gero env choices SERVICE NAME FIELD   what a field can list, looked up with the env's credential (git: repos)
  gero env check SERVICE NAME   exercise it from the host, now

  gero agent new --iso NAME --kind claude|codex [--id ID] [--repo URL] [--workdir DIR] [--model M] [--effort E] [--approvals never|auto|ask|default] [PROMPT...]
  gero agent ls [--all]
  gero agent log ID [-f]
  gero agent send ID TEXT...
  gero agent inbox ID [--rm N | --clear]   mail from other agents, read one per turn when idle
  gero agent mail ID   its mail history: what it was sent and what it wrote
  gero agent approve ID APPROVAL accept|decline|cancel
  gero agent answer ID APPROVAL ANSWER...   answer a question the agent asks, one answer per question
  gero agent interrupt ID
  gero agent set ID [--model M] [--effort E] [--approvals never|auto|ask|default]
  gero agent rename ID NAME...   only the name changes; a running turn carries on
  gero agent id ID NEW-ID|--clear   an ID of your own, unique among all agents; usable wherever an ID is
  gero agent archive ID
  gero agent restore ID         an archived one, back where it left off; a live session of its line is archived for it
  gero agent rm ID [--purge]    an archived one; --purge also removes its workspace and session transcript

  gero tui [--keys]             the terminal UI; --keys prints a keys.toml with every default
  gero web [--url] [--rotate]   open the web UI signed in (and print the phone link via Tailscale)
`

func main() {
	err := run(os.Args[1:])
	var code exitCode
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gero:", err)
		os.Exit(1)
	}
}

// exitCode ends gero with that status and nothing printed: the command has
// already said everything (`gero iso verify`).
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
		fmt.Println("gero", api.Version)
		return nil
	case "daemon":
		return daemonCmd(p, args[1:])
	case "iso", "isolation":
		return isoCmd(p, args[1:])
	case "agent":
		return agentCmd(p, args[1:])
	case "env":
		return envCmd(p, args[1:])
	case "account":
		return accountCmd(p, args[1:])
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
// crash or a reboot. With GERO_HOME set (development) it forks a plain
// background daemon instead, leaving the user's launchd alone.
//
// A daemon launchd runs from this very binary but on another version is one
// the binary was replaced under (`brew upgrade gero`): it is restarted onto
// the new one. Its agents carry on, as they do across any daemon restart.
func ensureDaemon(p paths.Paths, verbose bool) error {
	c := client.New(p.Socket())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	h, err := c.Health(ctx)
	cancel()
	exe, exeErr := os.Executable()
	if err == nil {
		if h.Version == api.Version || exeErr != nil || os.Getenv("GERO_HOME") != "" || !launchd.Runs(exe, p) {
			if verbose {
				fmt.Printf("gero daemon is running (pid %d)\n", h.PID)
			}
			return nil
		}
		if err := launchd.Restart(); err != nil {
			return err
		}
		return waitDaemon(c, p, func(h api.Health) bool { return h.Version == api.Version }, func(h api.Health) {
			if verbose {
				fmt.Printf("gero daemon restarted on %s (pid %d)\n", api.Version, h.PID)
			}
		})
	}
	if exeErr != nil {
		return exeErr
	}
	if os.Getenv("GERO_HOME") != "" {
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
			fmt.Printf("gero daemon started (pid %d)\n", h.PID)
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
		return errors.New("usage: gero daemon run|status|stop|uninstall")
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
		// development daemon (GERO_HOME) was never registered and must not
		// take the real one down with it.
		if os.Getenv("GERO_HOME") == "" {
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
