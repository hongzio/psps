package store

// A line kept from archiving is one the operator ticked so it is not
// archived by mistake: archiving it is refused until they untick it. It is
// the line's, not a session's, so a new session of it is kept too.
const guardSchema = `
CREATE TABLE IF NOT EXISTS line_guards (
	line TEXT PRIMARY KEY
);
`

// SetLineGuarded keeps line from archiving, or stops keeping it.
func (s *Store) SetLineGuarded(line string, on bool) error {
	if !on {
		_, err := s.db.Exec(`DELETE FROM line_guards WHERE line = ?`, line)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO line_guards (line) VALUES (?) ON CONFLICT (line) DO NOTHING`, line)
	return err
}

// LineGuarded says whether line is kept from archiving.
func (s *Store) LineGuarded(line string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM line_guards WHERE line = ?`, line).Scan(&n)
	return n > 0, err
}

// GuardedLines are the lines kept from archiving.
func (s *Store) GuardedLines() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT line FROM line_guards`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out[line] = true
	}
	return out, rows.Err()
}
