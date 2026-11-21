package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func kinds(es []KanbanEvent) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Kind
	}
	return out
}

func TestEveryChangeToACardIsALineOfItsHistory(t *testing.T) {
	s := open(t)
	agent := KanbanActor{Agent: "a1", Name: "lead"}
	c, err := s.AddCard(KanbanCard{Board: "psps", Title: "BLEU drops", Status: CardTodo}, agent)
	if err != nil {
		t.Fatal(err)
	}
	title, status, who := "Find the BLEU regression", CardDoing, "a1"
	same := "psps"
	c, err = s.UpdateCard(c.ID, CardPatch{Title: &title, Status: &status, Assignee: &who, Board: &same}, Operator)
	if err != nil || c.Title != title || c.Status != CardDoing || c.Assignee != "a1" {
		t.Fatalf("updated %+v, %v", c, err)
	}
	// Nothing changes: no line.
	if _, err := s.UpdateCard(c.ID, CardPatch{Title: &title}, Operator); err != nil {
		t.Fatal(err)
	}
	cm, err := s.AddComment(c.ID, "tape 3 only", agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditComment(cm.ID, "tape 3 only", Operator); !errors.Is(err, ErrCommentNotYours) {
		t.Fatalf("the operator edited the agent's comment: %v", err)
	}
	if _, err := s.EditComment(cm.ID, "tape 3 only, since the tokenizer bump", agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteComment(cm.ID, agent); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.Comments(c.ID); len(cs) != 0 {
		t.Fatalf("a deleted comment is still shown: %+v", cs)
	}
	if _, err := s.DeleteCard(c.ID, Operator); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.Cards([]string{"psps"}); len(cs) != 0 {
		t.Fatalf("a deleted card is still on its board: %+v", cs)
	}
	if _, err := s.UpdateCard(c.ID, CardPatch{Title: &title}, Operator); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted card was changed: %v", err)
	}

	es, err := s.KanbanEvents(KanbanFilter{Card: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(kinds(es))
	want := `["deleted","comment_deleted","comment_edited","comment","assignee","status","title","created"]`
	if string(got) != want {
		t.Fatalf("history %s, want %s", got, want)
	}
	if es[1].From != "tape 3 only, since the tokenizer bump" || es[2].From != "tape 3 only" {
		t.Fatalf("a comment's history lost its text: %+v", es[1:3])
	}
	if es[5].From != CardTodo || es[5].To != CardDoing || es[5].By != Operator {
		t.Fatalf("status line %+v", es[5])
	}
	if es[7].By != agent {
		t.Fatalf("made by %+v, want %+v", es[7].By, agent)
	}

	if _, err := s.RestoreCard(c.ID, Operator); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.Cards([]string{"psps"}); len(cs) != 1 {
		t.Fatalf("a restored card is not back: %+v", cs)
	}
}

func TestABoardsCardsAndHistoryFollowItsEnvsRename(t *testing.T) {
	s := open(t)
	if err := s.PutEnv(Env{Service: "kanban", Name: "psps"}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.AddCard(KanbanCard{Board: "psps", Title: "a", Status: CardTodo}, Operator)
	s.AddCard(KanbanCard{Board: "other", Title: "b", Status: CardDone}, Operator)
	if _, err := s.RenameEnv("kanban", "psps", "gero"); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Cards([]string{"gero"})
	if len(cs) != 1 || cs[0].ID != c.ID {
		t.Fatalf("cards on gero: %+v", cs)
	}
	if es, _ := s.KanbanEvents(KanbanFilter{Boards: []string{"gero"}}); len(es) != 1 {
		t.Fatalf("gero's history: %+v", es)
	}
	if n, _ := s.LiveCards("psps"); n != 0 {
		t.Fatalf("%d cards left on psps", n)
	}
}

func TestABoardListsItsColumnsInOrderAndMovedCardsLast(t *testing.T) {
	s := open(t)
	a, _ := s.AddCard(KanbanCard{Board: "b", Title: "a", Status: CardDone}, Operator)
	b, _ := s.AddCard(KanbanCard{Board: "b", Title: "b", Status: CardTodo}, Operator)
	c, _ := s.AddCard(KanbanCard{Board: "b", Title: "c", Status: CardTodo}, Operator)
	todo := CardTodo
	s.UpdateCard(a.ID, CardPatch{Status: &todo}, Operator)
	cs, _ := s.Cards([]string{"b"})
	var ids []int64
	for _, x := range cs {
		ids = append(ids, x.ID)
	}
	got, _ := json.Marshal(ids)
	want, _ := json.Marshal([]int64{b.ID, c.ID, a.ID})
	if string(got) != string(want) {
		t.Fatalf("order %s, want %s", got, want)
	}
}

func TestACardsChecklistIsItsBodysTaskItemsOutsideCode(t *testing.T) {
	body := "Steps:\n- [ ] pin the tokenizer\n- [x] rerun tape 3\n```\n- [ ] not an item\n```\n  * [X] nested, done\n> - [ ] quoted\n1. [ ] numbered\n- [ ]\n- [] no"
	ts := CardTasks(body)
	got, _ := json.Marshal(ts)
	want := `[{"done":false,"text":"pin the tokenizer"},{"done":true,"text":"rerun tape 3"},{"done":true,"text":"nested, done"},{"done":false,"text":"quoted"},{"done":false,"text":"numbered"}]`
	if string(got) != want {
		t.Fatalf("tasks %s\nwant  %s", got, want)
	}
	b, err := CheckTask(body, 0, true)
	if err != nil || CardTasks(b)[0].Done != true || strings.Count(b, "\n") != strings.Count(body, "\n") {
		t.Fatalf("checked: %q %v", b, err)
	}
	if b2, _ := CheckTask(b, 2, false); !strings.Contains(b2, "  * [ ] nested, done") {
		t.Fatalf("unchecked: %q", b2)
	}
	if _, err := CheckTask(body, 5, true); err == nil {
		t.Fatal("checked an item there is not")
	}
}

func TestCheckingItemsIsALineEachNotTheBodyTwice(t *testing.T) {
	s := open(t)
	c, _ := s.AddCard(KanbanCard{Board: "b", Title: "t", Status: CardTodo, Body: "- [ ] one\n- [ ] two\n- [x] three"}, Operator)
	b, _ := CheckTask(c.Body, 0, true)
	b, _ = CheckTask(b, 2, false)
	if _, err := s.UpdateCard(c.ID, CardPatch{Body: &b}, Operator); err != nil {
		t.Fatal(err)
	}
	more := b + "\n- [ ] four"
	s.UpdateCard(c.ID, CardPatch{Body: &more}, Operator)
	es, _ := s.KanbanEvents(KanbanFilter{Card: c.ID})
	var lines []string
	for _, e := range es {
		lines = append(lines, e.Kind+" "+e.To)
	}
	want := "body " + more + ",unchecked three,checked one,created todo"
	if strings.Join(lines, ",") != want {
		t.Fatalf("history %q\nwant    %q", strings.Join(lines, ","), want)
	}
}

func TestACardIsKnownByItsBoardsSlugAndANumberThere(t *testing.T) {
	s := open(t)
	s.PutEnv(Env{Service: "kanban", Name: "acme", Config: []byte(`{"slug":"ACME"}`)})
	s.PutEnv(Env{Service: "kanban", Name: "eval", Config: []byte(`{"slug":"EVAL"}`)})
	a1, _ := s.AddCard(KanbanCard{Board: "acme", Title: "a", Status: CardTodo}, Operator)
	a2, _ := s.AddCard(KanbanCard{Board: "acme", Title: "b", Status: CardTodo}, Operator)
	e1, _ := s.AddCard(KanbanCard{Board: "eval", Title: "c", Status: CardTodo}, Operator)
	if a1.Num != 1 || a2.Num != 2 || e1.Num != 1 {
		t.Fatalf("numbers %d %d %d", a1.Num, a2.Num, e1.Num)
	}
	// Moved, it is the next card on its new board; its old key still finds it.
	eval := "eval"
	moved, _ := s.UpdateCard(a2.ID, CardPatch{Board: &eval}, Operator)
	if moved.Num != 2 {
		t.Fatalf("moved to %d", moved.Num)
	}
	for _, k := range []struct {
		slug string
		num  int64
	}{{"ACME", 2}, {"EVAL", 2}} {
		if c, err := s.CardByKey(k.slug, k.num); err != nil || c.ID != a2.ID {
			t.Fatalf("%s-%d: %+v %v", k.slug, k.num, c, err)
		}
	}
	// A slug is not one another board's cards are known by.
	if used, _ := s.SlugUsed("ACME", "eval"); !used {
		t.Fatal("ACME is how a card now on eval was known")
	}
	// A new slug keys the board's cards anew; the old keys still work.
	s.PutEnv(Env{Service: "kanban", Name: "acme", Config: []byte(`{"slug":"AX"}`)})
	if err := s.ReslugBoard("acme"); err != nil {
		t.Fatal(err)
	}
	if c, err := s.CardByKey("AX", 1); err != nil || c.ID != a1.ID {
		t.Fatalf("AX-1: %+v %v", c, err)
	}
	if c, err := s.CardByKey("ACME", 1); err != nil || c.ID != a1.ID {
		t.Fatalf("ACME-1 after the new slug: %+v %v", c, err)
	}
	// The next on acme is after every number its slug has had.
	a3, _ := s.AddCard(KanbanCard{Board: "acme", Title: "d", Status: CardTodo}, Operator)
	if a3.Num != 2 {
		t.Fatalf("next on AX is %d", a3.Num)
	}
}

func TestBoardsFromBeforeSlugsAreGivenTheirNameAndCardsKeepTheirNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psps.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.PutEnv(Env{Service: "kanban", Name: "acme", Config: []byte(`{"color":"gray"}`)})
	s.PutEnv(Env{Service: "kanban", Name: "eval", Config: []byte(`{"color":"red"}`)})
	var ids []int64
	for _, b := range []string{"acme", "eval", "acme"} {
		c, _ := s.AddCard(KanbanCard{Board: b, Title: b, Status: CardTodo}, Operator)
		ids = append(ids, c.ID)
	}
	// As it was: no slug, no numbers, no keys.
	s.db.Exec(`UPDATE envs SET config = json_remove(config, '$.slug')`)
	s.db.Exec(`UPDATE kanban_cards SET num = 0`)
	s.db.Exec(`DELETE FROM kanban_keys`)
	s.db.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	for name, want := range map[string]string{"acme": `{"color":"gray","slug":"ACME"}`, "eval": `{"color":"red","slug":"EVAL"}`} {
		e, _ := s.Env("kanban", name)
		if string(e.Config) != want {
			t.Fatalf("%s: %s, want %s", name, e.Config, want)
		}
	}
	for i, k := range []string{"ACME", "EVAL", "ACME"} {
		if c, err := s.CardByKey(k, ids[i]); err != nil || c.ID != ids[i] || c.Num != ids[i] {
			t.Fatalf("%s-%d: %+v %v", k, ids[i], c, err)
		}
	}
	if c, _ := s.AddCard(KanbanCard{Board: "acme", Title: "new", Status: CardTodo}, Operator); c.Num != ids[2]+1 {
		t.Fatalf("the next on ACME is %d", c.Num)
	}
}
