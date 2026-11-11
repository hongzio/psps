package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Bookmark kinds: what the message bookmarked was.
const (
	BookmarkUser      = "user"      // a message the operator sent
	BookmarkAssistant = "assistant" // an agent's reply
	BookmarkMail      = "mail"      // a mail another agent sent it
)

// A Bookmark is a message the operator kept, with a note of their own. It
// holds a copy of what the message said, so it outlives its agent: an
// archived or deleted one's bookmarks stay, and only open its conversation
// while that is still there.
type Bookmark struct {
	ID int64 `json:"id"`
	// Agent and Event are the message: the agent's ID and its event's.
	Agent string `json:"agent"`
	Event int64  `json:"event"`
	// AgentName is what the agent was called when it was kept.
	AgentName string `json:"agent_name"`
	Kind      string `json:"kind"`
	// From is a mail's sender.
	From string `json:"from,omitempty"`
	Text string `json:"text"`
	Note string `json:"note"`
	// At is when the message was; CreatedAt when it was kept.
	At        time.Time `json:"at"`
	CreatedAt time.Time `json:"created_at"`
}

const bookmarkSchema = `
CREATE TABLE IF NOT EXISTS bookmarks (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	agent      TEXT NOT NULL,
	event      INTEGER NOT NULL,
	agent_name TEXT NOT NULL,
	kind       TEXT NOT NULL,
	sender     TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	at         TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (agent, event)
);
`

const bookmarkCols = `id, agent, event, agent_name, kind, sender, text, note, at, created_at`

func scanBookmark(row interface{ Scan(...any) error }) (Bookmark, error) {
	var b Bookmark
	var at, created string
	err := row.Scan(&b.ID, &b.Agent, &b.Event, &b.AgentName, &b.Kind, &b.From, &b.Text, &b.Note, &at, &created)
	b.At, b.CreatedAt = parseTS(at), parseTS(created)
	return b, err
}

// AddBookmark keeps b and returns it with its ID. A message is kept once:
// kept already, it is returned as it was, its note unchanged. b's CreatedAt,
// if set, is kept (one let go and taken back keeps its place); else it is now.
func (s *Store) AddBookmark(b Bookmark) (Bookmark, error) {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if _, err := s.db.Exec(`INSERT INTO bookmarks (agent, event, agent_name, kind, sender, text, note, at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (agent, event) DO NOTHING`,
		b.Agent, b.Event, b.AgentName, b.Kind, b.From, b.Text, b.Note, ts(b.At), ts(b.CreatedAt)); err != nil {
		return b, err
	}
	return scanBookmark(s.db.QueryRow(`SELECT `+bookmarkCols+` FROM bookmarks WHERE agent = ? AND event = ?`, b.Agent, b.Event))
}

// Bookmarks are every one kept, the latest first.
func (s *Store) Bookmarks() ([]Bookmark, error) {
	rows, err := s.db.Query(`SELECT ` + bookmarkCols + ` FROM bookmarks ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bookmark{}
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	// By time, not the text it is stored as: RFC 3339 with its trailing
	// zeros dropped does not sort as the times do.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, rows.Err()
}

// SetBookmarkNote replaces a bookmark's note; empty, it has none.
func (s *Store) SetBookmarkNote(id int64, note string) (Bookmark, error) {
	b, err := scanBookmark(s.db.QueryRow(`UPDATE bookmarks SET note = ? WHERE id = ? RETURNING `+bookmarkCols, note, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("bookmark %d: %w", id, ErrNotFound)
	}
	return b, err
}

// DeleteBookmark lets a bookmark go; the message stays where it is.
func (s *Store) DeleteBookmark(id int64) error {
	res, err := s.db.Exec(`DELETE FROM bookmarks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("bookmark %d: %w", id, ErrNotFound)
	}
	return nil
}
