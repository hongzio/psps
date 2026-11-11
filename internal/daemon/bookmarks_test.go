package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

// A bookmark keeps what its message said, read from the history: a message
// sent, a mail (its body, and who sent it) or a reply; nothing else is one.
func TestABookmarkKeepsItsMessage(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Name: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(a.ID, "start"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the first turn", func() bool { return e.hasAssistant(a.ID, "echo: start") && status(m, a.ID) == event.StatusIdle })
	if err := m.Send(a.ID, mailproxy.Letter(7, "evaluator", "lab", "tape-11 timed out")); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the mail", func() bool { return status(m, a.ID) == event.StatusIdle && len(users(e, a.ID)) == 2 })

	var sent, mail, reply, other int64
	for _, ev := range e.events(a.ID) {
		switch {
		case ev.Kind == event.KindUser && ev.Text == "start":
			sent = ev.ID
		case ev.Kind == event.KindUser:
			mail = ev.ID
		case ev.Kind == event.KindAssistant && ev.Text == "echo: start":
			reply = ev.ID
		case ev.Kind != event.KindAssistant && other == 0:
			other = ev.ID
		}
	}
	want := map[int64]store.Bookmark{
		sent:  {Kind: store.BookmarkUser, Text: "start"},
		mail:  {Kind: store.BookmarkMail, From: "evaluator", Text: "tape-11 timed out"},
		reply: {Kind: store.BookmarkAssistant, Text: "echo: start"},
	}
	for id, w := range want {
		b, err := m.AddBookmark(a.ID, id, "", time.Time{})
		if err != nil || b.Kind != w.Kind || b.From != w.From || b.Text != w.Text || b.AgentName != "lead" || b.At.IsZero() {
			t.Fatalf("event %d kept as %+v, %v; want %+v", id, b, err, w)
		}
	}
	if _, err := m.AddBookmark(a.ID, other, "", time.Time{}); err == nil {
		t.Fatal("a status was bookmarked")
	}
	if _, err := m.AddBookmark(a.ID, 9999, "", time.Time{}); err == nil {
		t.Fatal("an event there is not was bookmarked")
	}
	bs, _ := m.Bookmarks()
	if len(bs) != 3 {
		t.Fatalf("%d bookmarks, want 3", len(bs))
	}
	if b, err := m.SetBookmarkNote(bs[0].ID, "for prod"); err != nil || b.Note != "for prod" {
		t.Fatalf("noted %+v, %v", b, err)
	}
}
