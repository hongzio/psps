package store

import (
	"errors"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/driver"
)

func TestABookmarkIsKeptOnceAndOutlivesItsAgent(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(Agent{ID: "a1", Name: "a1", Kind: driver.Claude, IsolationID: "i1", Workdir: "/w", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	first, err := s.AddBookmark(Bookmark{Agent: "a1", Event: 3, AgentName: "lead", Kind: BookmarkAssistant, Text: "use RETRY_MAX=3", At: now})
	if err != nil || first.ID == 0 || first.Text != "use RETRY_MAX=3" {
		t.Fatalf("added %+v, %v", first, err)
	}
	if _, err := s.SetBookmarkNote(first.ID, "for prod"); err != nil {
		t.Fatal(err)
	}
	again, err := s.AddBookmark(Bookmark{Agent: "a1", Event: 3, AgentName: "lead", Kind: BookmarkAssistant, Text: "use RETRY_MAX=3", At: now})
	if err != nil || again.ID != first.ID || again.Note != "for prod" {
		t.Fatalf("kept twice, or its note lost: %+v, %v", again, err)
	}
	mail, _ := s.AddBookmark(Bookmark{Agent: "a1", Event: 5, AgentName: "lead", Kind: BookmarkMail, From: "evaluator", Text: "tape-11 timed out", At: now})
	bs, err := s.Bookmarks()
	if err != nil || len(bs) != 2 || bs[0].ID != mail.ID || bs[0].From != "evaluator" {
		t.Fatalf("bookmarks %+v, %v", bs, err)
	}

	if err := s.DeleteAgent("a1"); err != nil {
		t.Fatal(err)
	}
	if bs, _ := s.Bookmarks(); len(bs) != 2 {
		t.Fatalf("bookmarks went with their agent: %+v", bs)
	}

	// One let go and taken back keeps its place.
	if err := s.DeleteBookmark(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBookmark(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
	back, _ := s.AddBookmark(Bookmark{Agent: "a1", Event: 3, AgentName: "lead", Kind: BookmarkAssistant, Text: "use RETRY_MAX=3", Note: "for prod", At: now, CreatedAt: first.CreatedAt})
	if bs, _ := s.Bookmarks(); len(bs) != 2 || bs[1].ID != back.ID || bs[1].Note != "for prod" {
		t.Fatalf("taken back out of place: %+v", bs)
	}
	if _, err := s.SetBookmarkNote(999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("noted a bookmark there is not: %v", err)
	}
}
