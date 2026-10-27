package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/hongzio/gero/internal/event"
)

type theme struct {
	title, user, agent, tool, dim, ok, err, warn, working lipgloss.Style
	selected, selectedDim, focusRule, rule, key, flashErr lipgloss.Style
}

func newTheme() theme {
	c := func(light, dark string) lipgloss.AdaptiveColor {
		return lipgloss.AdaptiveColor{Light: light, Dark: dark}
	}
	accent := c("#5a4fcf", "#a99cff")
	return theme{
		title:       lipgloss.NewStyle().Bold(true),
		user:        lipgloss.NewStyle().Bold(true).Foreground(accent),
		agent:       lipgloss.NewStyle().Bold(true).Foreground(c("#1f7a4d", "#6fd6a0")),
		tool:        lipgloss.NewStyle().Foreground(c("#7a5a00", "#e0c070")),
		dim:         lipgloss.NewStyle().Foreground(c("#7a7a85", "#8a8a96")),
		ok:          lipgloss.NewStyle().Foreground(c("#1f7a4d", "#6fd6a0")),
		err:         lipgloss.NewStyle().Foreground(c("#b42318", "#ff8a80")),
		warn:        lipgloss.NewStyle().Foreground(c("#9a5b00", "#ffc46b")),
		working:     lipgloss.NewStyle().Foreground(accent),
		selected:    lipgloss.NewStyle().Bold(true).Foreground(c("#ffffff", "#15131f")).Background(accent),
		selectedDim: lipgloss.NewStyle().Foreground(c("#1d1a33", "#d8d4ff")).Background(c("#e4e1fb", "#2e2a4a")),
		focusRule:   lipgloss.NewStyle().Foreground(accent),
		rule:        lipgloss.NewStyle().Foreground(c("#d0d0d8", "#3a3a46")),
		key:         lipgloss.NewStyle().Bold(true),
		flashErr:    lipgloss.NewStyle().Foreground(c("#b42318", "#ff8a80")).Bold(true),
	}
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// statusIcon is how the sidebar shows an agent's status at a glance.
func (th theme) statusIcon(s event.Status, frame int) string {
	switch s {
	case event.StatusWorking:
		return th.working.Render(spinner[frame%len(spinner)])
	case event.StatusStarting:
		return th.dim.Render(spinner[frame%len(spinner)])
	case event.StatusBlocked:
		return th.warn.Render("!")
	case event.StatusInterrupted:
		return th.warn.Render("⏸")
	case event.StatusError:
		return th.err.Render("✗")
	}
	return th.dim.Render("○")
}
