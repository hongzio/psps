package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/kanbanproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

// Kanban is off until an isolation is given boards; its agents then keep
// cards on those boards alone, within what the exposure allows, and every
// change they and the operator make is in the history, as theirs.
func TestAgentsKeepCardsOnTheBoardsTheirIsolationIsGiven(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, kanbanproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "lab"}, nil); err != nil {
		t.Fatal(err)
	}
	agent := func(iso, handle string) store.Agent {
		a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: iso, Kind: driver.Codex, Handle: handle})
		if err != nil {
			t.Fatal(err)
		}
		e.waitFor(handle, func() bool { return status(m, a.ID) == event.StatusIdle })
		return a
	}
	lead := agent("dev", "lead")
	grader := agent("lab", "grader")

	kb := func(a store.Agent, method, path, body string) (int, string) {
		t.Helper()
		return mailAs(t, m, a.IsolationID, a.ID, method, "/kanban"+path, body)
	}

	// Off by default: no address, and the port refuses.
	if env := m.kanbanEnv(lead); env != nil {
		t.Fatalf("an address while off: %q", env)
	}
	if code, out := kb(lead, "GET", "", ""); code != http.StatusForbidden || !strings.Contains(out, "psps iso expose dev kanban") {
		t.Fatalf("off: %d %s", code, out)
	}

	if _, err := m.PutEnv(ctx, "kanban", "noslug", proxy.Patch{}, true); err == nil || !strings.Contains(err.Error(), "--slug NOSLUG") {
		t.Fatalf("a board without a slug: %v", err)
	}
	for name, color := range map[string]string{"psps": "violet", "mtlab": "teal", "infra": ""} {
		p := proxy.Patch{Set: map[string]any{"color": color, "slug": strings.ToLower(name)}}
		if color == "" {
			p = proxy.Patch{Set: map[string]any{"slug": name}}
		}
		v, err := m.PutEnv(ctx, "kanban", name, p, true)
		if err != nil {
			t.Fatal(err)
		}
		var cfg kanbanproxy.Env
		json.Unmarshal(v.Config, &cfg)
		if want := map[string]string{"psps": "violet", "mtlab": "teal", "infra": "gray"}[name]; cfg.Color != want {
			t.Fatalf("%s's color %q, want %q", name, cfg.Color, want)
		}
		if cfg.Slug != strings.ToUpper(name) {
			t.Fatalf("%s's slug %q", name, cfg.Slug)
		}
	}
	if _, err := m.PutEnv(ctx, "kanban", "other", proxy.Patch{Set: map[string]any{"slug": "PSPS"}}, true); err == nil || !strings.Contains(err.Error(), "slug is PSPS already") {
		t.Fatalf("a slug taken: %v", err)
	}
	if _, err := m.PutEnv(ctx, "kanban", "bad", proxy.Patch{Set: map[string]any{"color": "#ff0000"}}, true); err == nil {
		t.Fatal("a color not in the palette")
	}
	if _, err := m.Expose(ctx, "dev", "kanban", api.Expose{}); err == nil || !strings.Contains(err.Error(), "name the boards") {
		t.Fatalf("no board: %v", err)
	}
	if _, err := m.Expose(ctx, "dev", "kanban", api.Expose{Envs: []string{"psps", "mtlab"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "lab", "kanban", api.Expose{Envs: []string{"mtlab"},
		Settings: proxy.Patch{Set: map[string]any{"allow": []string{"comment"}}}}); err != nil {
		t.Fatal(err)
	}
	tok, _ := m.store.MailToken(lead.ID)
	if env := m.kanbanEnv(lead); len(env) == 0 || env[0] != "PSPS_KANBAN_URL="+kanbanproxy.URL(tok) {
		t.Fatalf("env %q", env)
	}
	if code, out := mailAt(m, testIsoID, "GET", "/a/nope/kanban", ""); code != http.StatusForbidden || !strings.Contains(out, "not the address of an agent") {
		t.Fatalf("a wrong token: %d %s", code, out)
	}

	var view api.Kanban
	code, out := kb(lead, "GET", "", "")
	json.Unmarshal([]byte(out), &view)
	if code != 200 || len(view.Boards) != 2 || view.Boards[0].Name != "psps" || !view.Boards[0].Default || view.Boards[1].Color != "teal" ||
		strings.Join(view.Allow, ",") != "create,edit,move,comment" {
		t.Fatalf("view: %d %s", code, out)
	}

	// A card on the default board, taken by its maker.
	var c api.KanbanCard
	code, out = kb(lead, "POST", "", `{"title":"Find why BLEU drops","assignee":"me"}`)
	json.Unmarshal([]byte(out), &c)
	if code != http.StatusCreated || c.Board != "psps" || c.Status != store.CardTodo || c.Assignee != lead.ID || c.Key != "PSPS-1" || c.Color != "violet" {
		t.Fatalf("made: %d %s", code, out)
	}
	for _, body := range []string{`{}`, `{"title":"two\nlines"}`, `{"title":"x","board":"infra"}`, `{"title":"x","status":"later"}`,
		`{"title":"x","board":"mtlab","assignee":"nobody"}`, `not json`} {
		if code, out := kb(lead, "POST", "", body); code < 400 || code >= 500 {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}
	// lab's grader does not see psps: it cannot be assigned a card there,
	// nor see one.
	if code, out := kb(lead, "PATCH", "/PSPS-1", `{"assignee":"grader"}`); code != http.StatusBadRequest || !strings.Contains(out, "does not see that board") {
		t.Fatalf("assigned out of sight: %d %s", code, out)
	}
	if code, _ := kb(grader, "GET", "/PSPS-1", ""); code != http.StatusNotFound {
		t.Fatalf("lab saw a psps card: %d", code)
	}
	if code, _ := kb(lead, "GET", "/NOPE-1", ""); code != http.StatusNotFound {
		t.Fatalf("a key no card has: %d", code)
	}
	// Moved to mtlab, which lab sees: the assignee is kept (dev sees it too).
	// It is MTLAB-1 there; PSPS-1 still finds it.
	code, out = kb(lead, "PATCH", "/psps-1", `{"status":"In Progress","board":"mtlab"}`)
	json.Unmarshal([]byte(out), &c)
	if code != 200 || c.Status != store.CardDoing || c.Board != "mtlab" || c.Assignee != lead.ID || c.Key != "MTLAB-1" {
		t.Fatalf("moved: %d %s", code, out)
	}
	// lab may comment, and nothing more.
	if code, out := kb(grader, "PATCH", "/PSPS-1", `{"status":"done"}`); code != http.StatusForbidden || !strings.Contains(out, "may not move") {
		t.Fatalf("lab moved a card: %d %s", code, out)
	}
	if code, out := kb(grader, "POST", "", `{"title":"x"}`); code != http.StatusForbidden {
		t.Fatalf("lab made a card: %d %s", code, out)
	}
	var cm store.KanbanComment
	code, out = kb(grader, "POST", "/PSPS-1/comments", `{"text":"tape 3 only"}`)
	json.Unmarshal([]byte(out), &cm)
	if code != http.StatusCreated || cm.By.Agent != grader.ID {
		t.Fatalf("comment: %d %s", code, out)
	}
	if code, out := kb(lead, "PATCH", "/comments/"+itoa(cm.ID), `{"text":"mine now"}`); code != http.StatusForbidden {
		t.Fatalf("an agent edited another's comment: %d %s (comment %+v)", code, out, cm)
	}
	if code, out := kb(grader, "PATCH", "/comments/"+itoa(cm.ID), `{"text":"tape 3 only, since the tokenizer bump"}`); code != 200 {
		t.Fatalf("edit: %d %s", code, out)
	}
	if code, out := kb(lead, "DELETE", "/PSPS-1", ""); code != http.StatusForbidden {
		t.Fatalf("deleted without the right: %d %s", code, out)
	}

	// A checklist in the body: an item checked by number, guarded by its
	// text; counted on the card.
	if code, out := kb(lead, "PATCH", "/PSPS-1", `{"body":"- [ ] pin the tokenizer\n- [ ] rerun tape 3"}`); code != 200 {
		t.Fatalf("body: %d %s", code, out)
	}
	code, out = kb(lead, "POST", "/PSPS-1/check", `{"item":1,"checked":true,"text":"rerun tape 3"}`)
	json.Unmarshal([]byte(out), &c)
	if code != 200 || c.Tasks != 2 || c.TasksDone != 1 || !strings.Contains(c.Body, "- [x] rerun tape 3") {
		t.Fatalf("check: %d %s", code, out)
	}
	if code, out := kb(lead, "POST", "/PSPS-1/check", `{"item":0,"checked":true,"text":"something else"}`); code != http.StatusConflict {
		t.Fatalf("a stale checklist: %d %s", code, out)
	}
	if code, _ := kb(lead, "POST", "/PSPS-1/check", `{"item":2,"checked":true}`); code != http.StatusBadRequest {
		t.Fatalf("an item there is not: %d", code)
	}
	if code, _ := kb(grader, "POST", "/PSPS-1/check", `{"item":0,"checked":true}`); code != http.StatusForbidden {
		t.Fatalf("lab checked without edit: %d", code)
	}

	// The operator sees every board, and the card's whole history.
	all, err := m.Kanban("", "", "")
	if err != nil || len(all.Boards) != 3 || len(all.Cards) != 1 {
		t.Fatalf("operator: %+v %v", all, err)
	}
	labs, _ := m.Kanban("lab", "", "")
	if len(labs.Boards) != 1 || labs.Boards[0].Name != "mtlab" || len(labs.Cards) != 1 {
		t.Fatalf("lab's: %+v", labs)
	}
	if _, err := m.DeleteKanbanCard(1, false); err != nil {
		t.Fatal(err)
	}
	d, err := m.KanbanCard(1)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, h := range d.History {
		lines = append(lines, h.By.Name+" "+h.Kind)
	}
	want := "operator deleted,lead checked,lead body,grader comment_edited,grader comment,lead status,lead board,lead assignee,lead created"
	if strings.Join(lines, ",") != want {
		t.Fatalf("history %s\nwant    %s", strings.Join(lines, ","), want)
	}
	if code, _ := kb(lead, "GET", "/PSPS-1", ""); code != http.StatusNotFound {
		t.Fatalf("an agent saw a deleted card: %d", code)
	}
	log, _ := m.KanbanLog("dev", "", nil, 0, 0)
	if len(log) != 9 || log[0].Key != "MTLAB-1" || log[0].CardTitle != "Find why BLEU drops" || log[0].Color != "teal" {
		t.Fatalf("dev's log: %+v", log)
	}

	// A board with cards is not removed; renaming it moves them.
	m.DeleteKanbanCard(1, true)
	if err := m.DeleteEnv(ctx, "kanban", "mtlab", true); err == nil || !strings.Contains(err.Error(), "has 1 cards") {
		t.Fatalf("removed a board with cards: %v", err)
	}
	if _, err := m.RenameEnv(ctx, "kanban", "mtlab", "lab"); err != nil {
		t.Fatal(err)
	}
	code, out = kb(grader, "GET", "?board=lab", "")
	json.Unmarshal([]byte(out), &view)
	if code != 200 || len(view.Cards) != 1 || view.Cards[0].Board != "lab" {
		t.Fatalf("after the rename: %d %s", code, out)
	}
}
