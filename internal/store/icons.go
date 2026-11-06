package store

// A line's icon is one the operator picked for it, drawn in one of the
// sidebar's colors at its row's end. It is the line's, not a session's, so
// a new session wears it too, and so do the archived ones.
const iconSchema = `
CREATE TABLE IF NOT EXISTS line_icons (
	line  TEXT PRIMARY KEY,
	icon  TEXT NOT NULL,
	color TEXT NOT NULL DEFAULT ''
);
`

// Icon is a line's icon: its name, and its color's, "" for the text's own.
type Icon struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// SetLineIcon gives line the icon; one with no name takes its icon away.
func (s *Store) SetLineIcon(line string, i Icon) error {
	if i.Name == "" {
		_, err := s.db.Exec(`DELETE FROM line_icons WHERE line = ?`, line)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO line_icons (line, icon, color) VALUES (?, ?, ?)
		ON CONFLICT (line) DO UPDATE SET icon = excluded.icon, color = excluded.color`, line, i.Name, i.Color)
	return err
}

// LineIcons are the lines with an icon, each with it.
func (s *Store) LineIcons() (map[string]Icon, error) {
	rows, err := s.db.Query(`SELECT line, icon, color FROM line_icons`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Icon{}
	for rows.Next() {
		var line string
		var i Icon
		if err := rows.Scan(&line, &i.Name, &i.Color); err != nil {
			return nil, err
		}
		out[line] = i
	}
	return out, rows.Err()
}
