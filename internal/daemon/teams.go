package daemon

import (
	"cmp"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// Teams are every team, oldest first.
func (m *Manager) Teams() ([]store.Team, error) { return m.store.Teams() }

// CreateTeam makes a team, at the bottom of the sidebar's live agents.
func (m *Manager) CreateTeam(name string) (store.Team, error) {
	name, err := teamName(name)
	if err != nil {
		return store.Team{}, err
	}
	t := store.Team{ID: newID(), Name: name, CreatedAt: time.Now().UTC()}
	return t, m.store.CreateTeam(t)
}

// RenameTeam gives a team, by name or ID, a new name.
func (m *Manager) RenameTeam(ref, name string) (store.Team, error) {
	name, err := teamName(name)
	if err != nil {
		return store.Team{}, err
	}
	t, err := m.store.Team(ref)
	if err != nil {
		return store.Team{}, err
	}
	if err := m.store.RenameTeam(t.ID, name); err != nil {
		return store.Team{}, err
	}
	t.Name = name
	return t, nil
}

// DeleteTeam forgets a team, by name or ID; its agents are in none then,
// where it was in the sidebar.
func (m *Manager) DeleteTeam(ref string) error {
	t, err := m.store.Team(ref)
	if err != nil {
		return err
	}
	return m.store.DeleteTeam(t.ID)
}

// teamNamed is the team called name, made if there is none; "" is none.
func (m *Manager) teamNamed(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	name, err := teamName(name)
	if err != nil {
		return "", err
	}
	for try := 0; ; try++ {
		t, err := m.store.Team(name)
		if err == nil {
			return t.ID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		// Another caller may make it meanwhile; then it is that one.
		t, err = m.CreateTeam(name)
		if !errors.Is(err, store.ErrExists) || try == 2 {
			return t.ID, err
		}
	}
}

// setTeam puts a's line in the team called name, made if there is none, or
// in none for "".
func (m *Manager) setTeam(a store.Agent, name string) error {
	team, err := m.teamNamed(name)
	if err != nil {
		return err
	}
	return m.store.SetTeam(cmp.Or(a.Line, a.ID), team)
}

// withTeams gives each agent its line's team.
func (m *Manager) withTeams(as []api.Agent) error {
	lines, err := m.store.LineTeams()
	if err != nil {
		return err
	}
	teams, err := m.store.Teams()
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, t := range teams {
		names[t.ID] = t.Name
	}
	for i, a := range as {
		if id, ok := lines[cmp.Or(a.Line, a.ID)]; ok {
			as[i].TeamID, as[i].Team = id, names[id]
		}
	}
	return nil
}

// teamNames are the names of the teams lines are in, by line.
func (m *Manager) teamNames() (map[string]string, error) {
	lines, err := m.store.LineTeams()
	if err != nil {
		return nil, err
	}
	teams, err := m.store.Teams()
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, t := range teams {
		names[t.ID] = t.Name
	}
	out := map[string]string{}
	for line, id := range lines {
		out[line] = names[id]
	}
	return out, nil
}

// teamName is name as a team's, trimmed: one line, not too long for a
// sidebar.
func teamName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", badRequest(errors.New("a team name is required"))
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", badRequest(errors.New("a team name is one line of text"))
	case len([]rune(name)) > 64:
		return "", badRequest(errors.New("a team name is at most 64 characters"))
	}
	return name, nil
}
