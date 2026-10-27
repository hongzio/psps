package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hongzio/gero/internal/paths"
	"github.com/hongzio/gero/internal/tui"
)

func tuiCmd(p paths.Paths, args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	printKeys := fs.Bool("keys", false, "print a keys.toml with every default binding")
	fs.Parse(args)
	if *printKeys {
		fmt.Print(tui.DefaultKeysTOML())
		return nil
	}
	c, err := connect(p)
	if err != nil {
		return err
	}
	keys, warnings := tui.LoadKeymap(filepath.Join(p.Config, "keys.toml"))
	login := func(container, tool string) *exec.Cmd {
		cmd, err := interactive(context.Background(), container, loginArgv(tool))
		if err != nil {
			return exec.Command("sh", "-c", "echo "+err.Error()+"; sleep 2")
		}
		return cmd
	}
	// Ask the terminal for its background now: the query reads stdin, and
	// done lazily inside the program it swallows the user's first keys.
	lipgloss.HasDarkBackground()
	_, err = tea.NewProgram(tui.New(c, login, keys, warnings), tea.WithAltScreen()).Run()
	return err
}
