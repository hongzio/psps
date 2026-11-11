package daemon

import (
	"errors"
	"time"

	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

// maxBookmarkNote is the longest note on a bookmark, in bytes.
const maxBookmarkNote = 16 << 10

// Bookmarks are the messages the operator kept, the latest first.
func (m *Manager) Bookmarks() ([]store.Bookmark, error) {
	return m.store.Bookmarks()
}

// AddBookmark keeps one of an agent's messages: one it was sent, a mail
// another agent sent it, or a reply of its own. What it says is read from
// the agent's history and kept with the bookmark, which outlives it.
// created, if set, is when it was first kept, for one taken back.
func (m *Manager) AddBookmark(agent string, eventID int64, note string, created time.Time) (store.Bookmark, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Bookmark{}, err
	}
	if len(note) > maxBookmarkNote {
		return store.Bookmark{}, badRequest(errors.New("a note is at most 16 KiB"))
	}
	e, err := m.store.Event(a.ID, eventID)
	if err != nil {
		return store.Bookmark{}, err
	}
	b := store.Bookmark{Agent: a.ID, Event: e.ID, AgentName: bookmarkName(a), Text: e.Text, Note: note, At: e.Time, CreatedAt: created}
	switch e.Kind {
	case event.KindUser:
		b.Kind = store.BookmarkUser
		if l, ok := mailproxy.ReadLetter(e.Text); ok {
			b.Kind, b.From, b.Text = store.BookmarkMail, l.From, l.Body
		}
	case event.KindAssistant:
		b.Kind = store.BookmarkAssistant
	default:
		return store.Bookmark{}, badRequest(errors.New("only a message is bookmarked: one sent to the agent, or its reply"))
	}
	return m.store.AddBookmark(b)
}

// bookmarkName is what a bookmark calls its agent: its name, as the web
// shows it, or the ID it goes by.
func bookmarkName(a store.Agent) string {
	switch {
	case a.Name != "":
		return a.Name
	case a.Handle != "":
		return a.Handle
	}
	return a.ID
}

// SetBookmarkNote replaces a bookmark's note; empty, it has none.
func (m *Manager) SetBookmarkNote(id int64, note string) (store.Bookmark, error) {
	if len(note) > maxBookmarkNote {
		return store.Bookmark{}, badRequest(errors.New("a note is at most 16 KiB"))
	}
	return m.store.SetBookmarkNote(id, note)
}

// DeleteBookmark lets a bookmark go; the message stays.
func (m *Manager) DeleteBookmark(id int64) error {
	return m.store.DeleteBookmark(id)
}
