package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A team is a named group of the sidebar's live agents. It holds lines, not
// agents, so a new session stays in its line's team, and so does one
// restored. A team is drawn where its line, TeamLine, is in the live list,
// and its own list, the same name, orders its lines: a line in a team is
// in no other list.
const teamSchema = `
CREATE TABLE IF NOT EXISTS teams (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS team_lines (
	line TEXT PRIMARY KEY,
	team TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE
);
`

type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// TeamLine is team id's line in the live list, and the name of its own.
func TeamLine(id string) string { return teamPrefix + id }

const teamPrefix = "team:"

// TeamOf is the team a list or a line of the live list names, if it names
// one.
func TeamOf(list string) (string, bool) { return strings.CutPrefix(list, teamPrefix) }

// CreateTeam stores a new team, at the bottom of the live list; no other
// team may have its name.
func (s *Store) CreateTeam(t Team) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := teamNameFree(tx, t.Name, ""); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO teams (id, name, created_at) VALUES (?, ?, ?)`, t.ID, t.Name, ts(t.CreatedAt)); err != nil {
		return err
	}
	live, err := listLines(tx, LiveList)
	if err != nil {
		return err
	}
	if err := setOrder(tx, LiveList, append(live, TeamLine(t.ID))); err != nil {
		return err
	}
	return tx.Commit()
}

func teamNameFree(tx *sql.Tx, name, id string) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM teams WHERE name = ? AND id != ?`, name, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("team %q %w", name, ErrExists)
	}
	return nil
}

// Teams are every team, oldest first.
func (s *Store) Teams() ([]Team, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM teams ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var t Team
		var created string
		if err := rows.Scan(&t.ID, &t.Name, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTS(created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Team is the team called ref, or else the one whose ID it is.
func (s *Store) Team(ref string) (Team, error) {
	var t Team
	var created string
	err := s.db.QueryRow(`SELECT id, name, created_at FROM teams WHERE name = ? OR id = ? ORDER BY name = ? DESC LIMIT 1`, ref, ref, ref).
		Scan(&t.ID, &t.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("team %s: %w", ref, ErrNotFound)
	}
	t.CreatedAt = parseTS(created)
	return t, err
}

func (s *Store) RenameTeam(id, name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := teamNameFree(tx, name, id); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE teams SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("team %s: %w", id, ErrNotFound)
	}
	return tx.Commit()
}

// DeleteTeam forgets a team. Its lines are in none then, each in the live
// list where the team was, in the team's order.
func (s *Store) DeleteTeam(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	members, err := teamLines(tx, id)
	if err != nil {
		return err
	}
	live, err := listLines(tx, LiveList)
	if err != nil {
		return err
	}
	var next []string
	for _, l := range live {
		if l == TeamLine(id) {
			next = append(next, members...)
		} else {
			next = append(next, l)
		}
	}
	if err := setOrder(tx, LiveList, next); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_order WHERE list = ?`, TeamLine(id)); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM teams WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("team %s: %w", id, ErrNotFound)
	}
	return tx.Commit()
}

// teamLines are team id's lines in its order, those it has not ordered yet
// after them.
func teamLines(tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.Query(`SELECT t.line FROM team_lines t LEFT JOIN agent_order o ON o.list = ? AND o.line = t.line
		WHERE t.team = ? ORDER BY o.pos IS NULL, o.pos, t.rowid`, TeamLine(id), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetTeam puts line in team, at its bottom, or in none for "": it is then
// in the live list just under the team it leaves.
func (s *Store) SetTeam(line, team string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var was string
	err = tx.QueryRow(`SELECT team FROM team_lines WHERE line = ?`, line).Scan(&was)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if was == team {
		return nil
	}
	if team != "" {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM teams WHERE id = ?`, team).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("team %s: %w", team, ErrNotFound)
		}
	}
	if _, err := tx.Exec(`DELETE FROM team_lines WHERE line = ?`, line); err != nil {
		return err
	}
	if was != "" {
		if _, err := tx.Exec(`DELETE FROM agent_order WHERE list = ? AND line = ?`, TeamLine(was), line); err != nil {
			return err
		}
	}
	live, err := listLines(tx, LiveList)
	if err != nil {
		return err
	}
	var next []string
	for _, l := range live {
		if l == line {
			continue
		}
		next = append(next, l)
		if team == "" && l == TeamLine(was) {
			next = append(next, line)
		}
	}
	if err := setOrder(tx, LiveList, next); err != nil {
		return err
	}
	if team != "" {
		if _, err := tx.Exec(`INSERT INTO team_lines (line, team) VALUES (?, ?)`, line, team); err != nil {
			return err
		}
		lines, err := listLines(tx, TeamLine(team))
		if err != nil {
			return err
		}
		if err := setOrder(tx, TeamLine(team), append(lines, line)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LineTeams are the lines in a team, each with its team's ID.
func (s *Store) LineTeams() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT line, team FROM team_lines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var line, team string
		if err := rows.Scan(&line, &team); err != nil {
			return nil, err
		}
		out[line] = team
	}
	return out, rows.Err()
}

// LineTeam is the team line is in, or "".
func (s *Store) LineTeam(line string) (string, error) {
	var team string
	err := s.db.QueryRow(`SELECT team FROM team_lines WHERE line = ?`, line).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return team, err
}
