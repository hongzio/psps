package store

import "fmt"

// The operator's order of the sidebar: lines, not agents, so a new session
// stays where its line is. The live agents and the archived ones are two
// lists, each ordered on its own. A list may hold the sidebar's dividers
// too, which the web UI names "divider:<id>"; they are no agent's line.
const (
	LiveList     = "live"
	ArchivedList = "archived"
)

const orderSchema = `
CREATE TABLE IF NOT EXISTS agent_order (
	list TEXT NOT NULL,
	line TEXT NOT NULL,
	pos  INTEGER NOT NULL,
	PRIMARY KEY (list, line)
);
`

// SetOrder makes lines the order of list, from the top; what it leaves out
// goes back to where it would be without one.
func (s *Store) SetOrder(list string, lines []string) error {
	if list != LiveList && list != ArchivedList {
		return fmt.Errorf("there is no list %q: %w", list, ErrNotFound)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_order WHERE list = ?`, list); err != nil {
		return err
	}
	for i, l := range lines {
		if _, err := tx.Exec(`INSERT INTO agent_order (list, line, pos) VALUES (?, ?, ?)`, list, l, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Lists are each list's lines from the top: lines of agents, and the
// sidebar's dividers between them.
func (s *Store) Lists() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT list, line FROM agent_order ORDER BY list, pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{LiveList: {}, ArchivedList: {}}
	for rows.Next() {
		var list, line string
		if err := rows.Scan(&list, &line); err != nil {
			return nil, err
		}
		if _, ok := out[list]; ok {
			out[list] = append(out[list], line)
		}
	}
	return out, rows.Err()
}

// Order is each list's lines' places in it.
func (s *Store) Order() (map[string]map[string]int, error) {
	rows, err := s.db.Query(`SELECT list, line, pos FROM agent_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{LiveList: {}, ArchivedList: {}}
	for rows.Next() {
		var list, line string
		var pos int
		if err := rows.Scan(&list, &line, &pos); err != nil {
			return nil, err
		}
		if out[list] != nil {
			out[list][line] = pos
		}
	}
	return out, rows.Err()
}
