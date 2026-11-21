package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/kanbanproxy"
	"github.com/hongzio/psps/internal/store"
)

// Kanban boards (internal/proxy/kanbanproxy). The daemon answers agents on
// mail's port, at each agent's own address, on the boards their isolation
// is given; and the operator through the API, on every board. Both go
// through a kanbanScope, which says who acts, on which boards, with what
// rights; the store keeps a line of history for each change.

// kanbanScope is who acts on the boards, and what they may do.
type kanbanScope struct {
	by store.KanbanActor
	// me is the agent that acts; the operator has none.
	me store.Agent
	// iso is the agent's isolation's name, for what is said.
	iso string
	// boards are those it sees; nil is every board (the operator).
	boards []string
	def    string
	allow  kanbanproxy.Settings
}

func (s kanbanScope) operator() bool { return s.me.ID == "" }

func (s kanbanScope) sees(board string) bool {
	return s.boards == nil || slices.Contains(s.boards, board)
}

func (s kanbanScope) need(right string) error {
	if s.operator() || s.allow.Allows(right) {
		return nil
	}
	var help string
	for _, r := range kanbanproxy.Rights {
		if r.Name == right {
			help = r.Help
		}
	}
	return httpError{http.StatusForbidden, fmt.Errorf("kanban: the agents of isolation %s may not %s (%s is not allowed; they may %s); only your operator allows more",
		s.iso, help, right, orNone(strings.Join(s.allow.Allow, ", ")))}
}

// kanbanOf is isolation iso's (by ID) kanban exposure: the boards it is
// given, the default, and what its agents may do; on is false while the
// service is off there.
func (m *Manager) kanbanOf(iso string) (x store.Exposure, s kanbanproxy.Settings, on bool, err error) {
	if _, err := m.registry().Get(kanbanproxy.Name); err != nil {
		return x, s, false, nil
	}
	x, err = m.store.Exposure(iso, kanbanproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return x, s, false, nil
	}
	if err != nil {
		return x, s, false, err
	}
	s.Allow = kanbanproxy.DefaultAllow
	if len(x.Settings) > 0 {
		json.Unmarshal(x.Settings, &s)
	}
	return x, s, true, nil
}

// kanbanEnv is an agent process's boards' address, while its isolation
// has kanban.
func (m *Manager) kanbanEnv(a store.Agent) []string {
	if _, _, on, err := m.kanbanOf(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: kanban token: %v", a.ID, err)
		return nil
	}
	return proxy.AgentEnv("KANBAN_URL", kanbanproxy.URL(tok), m.fromGero(a.IsolationID))
}

// kanbanColors are every board's color, by name.
func (m *Manager) kanbanColors() (map[string]string, []string, error) {
	envs, err := m.store.Envs(kanbanproxy.Name)
	if err != nil {
		return nil, nil, err
	}
	colors := map[string]string{}
	var names []string
	for _, e := range envs {
		var v kanbanproxy.Env
		json.Unmarshal(e.Config, &v)
		if v.Color == "" {
			v.Color = kanbanproxy.DefaultColor
		}
		colors[e.Name] = v.Color
		names = append(names, e.Name)
	}
	return colors, names, nil
}

// agentKanban is the scope of the live agent of isolation iso whose
// address carries tok, while the isolation has kanban.
func (m *Manager) agentKanban(iso, tok string) (kanbanScope, error) {
	x, s, on, err := m.kanbanOf(iso)
	if err != nil {
		return kanbanScope{}, err
	}
	name := m.isoName(iso)
	if !on {
		return kanbanScope{}, httpError{http.StatusForbidden, fmt.Errorf("kanban: isolation %s has no boards; the operator gives it some with `psps iso expose %s kanban BOARD`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return kanbanScope{}, httpError{http.StatusForbidden, fmt.Errorf("kanban: this is not the address of an agent in isolation %s; use $PSPS_KANBAN_URL as your process was started with it", name)}
	}
	colors, _, err := m.kanbanColors()
	if err != nil {
		return kanbanScope{}, err
	}
	boards := []string{}
	for _, b := range x.Envs {
		if _, ok := colors[b]; ok {
			boards = append(boards, b)
		}
	}
	return kanbanScope{by: store.KanbanActor{Agent: a.ID, Name: agentLabel(a)}, me: a, iso: name, boards: boards, def: x.Default, allow: s}, nil
}

// operatorKanban is the operator's scope: every board, every right.
func operatorKanban() kanbanScope { return kanbanScope{by: store.Operator} }

// kanbanBoards are the boards s sees, those of isolation iso when the
// operator names one.
func (m *Manager) kanbanBoards(s kanbanScope, iso string) ([]api.KanbanBoard, map[string]string, error) {
	colors, names, err := m.kanbanColors()
	if err != nil {
		return nil, nil, err
	}
	def := s.def
	if s.operator() && iso != "" {
		i, err := m.isolationNamed(iso)
		if err != nil {
			return nil, nil, err
		}
		x, _, on, err := m.kanbanOf(i.ID)
		if err != nil {
			return nil, nil, err
		}
		s.boards = []string{}
		if on {
			s.boards, def = x.Envs, x.Default
		}
	}
	order := names
	if s.boards != nil {
		order = s.boards
	}
	out := []api.KanbanBoard{}
	for _, n := range order {
		c, ok := colors[n]
		if !ok {
			continue
		}
		b := api.KanbanBoard{Name: n, Color: c, Default: n == def, Isolations: []string{}}
		ids, _ := m.store.Exposing(kanbanproxy.Name, n)
		b.Isolations = append(b.Isolations, m.isoNames(ids)...)
		out = append(out, b)
	}
	return out, colors, nil
}

// agentLabel is what an agent is called on a board: its name, or its ID.
func agentLabel(a store.Agent) string {
	if a.Name != "" {
		return a.Name
	}
	return shownID(a)
}

func cardKey(id int64) string { return "K-" + strconv.FormatInt(id, 10) }

// agentName is an agent's name as it is now, or what it was when it acted.
func (m *Manager) agentName(id, was string) string {
	if a, err := m.store.Agent(id); err == nil {
		return agentLabel(a)
	}
	if was != "" {
		return was
	}
	return id
}

func (m *Manager) apiCard(c store.KanbanCard, colors map[string]string) api.KanbanCard {
	v := api.KanbanCard{KanbanCard: c, Key: cardKey(c.ID), Color: colors[c.Board]}
	if v.Color == "" {
		v.Color = kanbanproxy.DefaultColor
	}
	if c.Assignee != "" {
		v.AssigneeName = m.agentName(c.Assignee, "")
	}
	if c.CreatedBy.Agent != "" {
		v.CreatedBy.Name = m.agentName(c.CreatedBy.Agent, c.CreatedBy.Name)
	}
	return v
}

// kanbanEvents are es as they are shown: with their cards' keys and
// titles now, and names as they are now.
func (m *Manager) kanbanEvents(es []store.KanbanEvent, colors map[string]string) []api.KanbanEvent {
	titles := map[int64]string{}
	out := make([]api.KanbanEvent, 0, len(es))
	for _, e := range es {
		t, ok := titles[e.Card]
		if !ok {
			if c, err := m.store.Card(e.Card); err == nil {
				t = c.Title
			}
			titles[e.Card] = t
		}
		if e.By.Agent != "" {
			e.By.Name = m.agentName(e.By.Agent, e.By.Name)
		}
		v := api.KanbanEvent{KanbanEvent: e, Key: cardKey(e.Card), CardTitle: t, Color: colors[e.Board]}
		if e.Kind == store.KanbanAssignee {
			if e.From != "" {
				v.FromName = m.agentName(e.From, "")
			}
			if e.To != "" {
				v.ToName = m.agentName(e.To, "")
			}
		}
		if v.Color == "" {
			v.Color = kanbanproxy.DefaultColor
		}
		out = append(out, v)
	}
	return out
}

// kanbanView is what s sees: boards and their cards; mine keeps those
// assigned to s's agent, board and status narrow it.
func (m *Manager) kanbanView(s kanbanScope, iso, board, status string, mine bool) (api.Kanban, error) {
	boards, colors, err := m.kanbanBoards(s, iso)
	if err != nil {
		return api.Kanban{}, err
	}
	v := api.Kanban{Isolation: iso, Boards: boards, Cards: []api.KanbanCard{}}
	if !s.operator() {
		v.Isolation, v.Allow = s.iso, s.allow.Allow
	}
	if status != "" {
		if status, err = kanbanproxy.ParseStatus(status); err != nil {
			return v, badRequest(fmt.Errorf("kanban: %w", err))
		}
	}
	var names []string
	for _, b := range boards {
		if board == "" || b.Name == board {
			names = append(names, b.Name)
		}
	}
	if board != "" && len(names) == 0 {
		return v, httpError{http.StatusNotFound, fmt.Errorf("kanban: no board %s here (%s)", board, boardList(boards))}
	}
	cards, err := m.store.Cards(names)
	if err != nil {
		return v, err
	}
	for _, c := range cards {
		if (status != "" && c.Status != status) || (mine && c.Assignee != s.me.ID) {
			continue
		}
		v.Cards = append(v.Cards, m.apiCard(c, colors))
	}
	return v, nil
}

func boardList(bs []api.KanbanBoard) string {
	var n []string
	for _, b := range bs {
		n = append(n, b.Name)
	}
	if len(n) == 0 {
		return "it is given no board"
	}
	return "its boards: " + strings.Join(n, ", ")
}

// kanbanCard is card id, if s sees its board.
func (m *Manager) kanbanCard(s kanbanScope, id int64) (store.KanbanCard, error) {
	c, err := m.store.Card(id)
	if err != nil || !s.sees(c.Board) || (c.DeletedAt != nil && !s.operator()) {
		return c, httpError{http.StatusNotFound, fmt.Errorf("kanban: no card %s on the boards here", cardKey(id))}
	}
	return c, nil
}

func (m *Manager) kanbanDetail(s kanbanScope, id int64) (api.KanbanCardDetail, error) {
	c, err := m.kanbanCard(s, id)
	if err != nil {
		return api.KanbanCardDetail{}, err
	}
	colors, _, err := m.kanbanColors()
	if err != nil {
		return api.KanbanCardDetail{}, err
	}
	cs, err := m.store.Comments(id)
	if err != nil {
		return api.KanbanCardDetail{}, err
	}
	for i := range cs {
		if cs[i].By.Agent != "" {
			cs[i].By.Name = m.agentName(cs[i].By.Agent, cs[i].By.Name)
		}
	}
	es, err := m.store.KanbanEvents(store.KanbanFilter{Card: id, Limit: 500})
	if err != nil {
		return api.KanbanCardDetail{}, err
	}
	return api.KanbanCardDetail{KanbanCard: m.apiCard(c, colors), Comments: cs, History: m.kanbanEvents(es, colors)}, nil
}

func checkTitle(t string) (string, error) {
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return t, badRequest(errors.New(`kanban: a card needs a "title"`))
	case strings.ContainsAny(t, "\r\n"):
		return t, badRequest(errors.New(`kanban: a card's "title" is one line; put the rest in "body"`))
	case utf8.RuneCountInString(t) > kanbanproxy.MaxTitle:
		return t, badRequest(fmt.Errorf(`kanban: a card's "title" is at most %d characters; put the rest in "body"`, kanbanproxy.MaxTitle))
	}
	return t, nil
}

func checkBody(b string) error {
	if len(b) > kanbanproxy.MaxBody || !utf8.ValidString(b) {
		return badRequest(fmt.Errorf(`kanban: a card's "body" is UTF-8 text of at most %d bytes; put the rest where it can be read and say where`, kanbanproxy.MaxBody))
	}
	return nil
}

// kanbanAssignee is who assignee names, for a card on board: "me", an
// agent's ID or handle, or "" for none. The agent is one whose isolation
// sees the board.
func (m *Manager) kanbanAssignee(s kanbanScope, board, assignee string) (string, error) {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return "", nil
	}
	if assignee == "me" {
		if s.operator() {
			return "", badRequest(errors.New(`kanban: "me" is an agent assigning itself; name the agent`))
		}
		return s.me.ID, nil
	}
	a, err := m.storedAgent(assignee)
	if err != nil || a.ArchivedAt != nil {
		return "", badRequest(fmt.Errorf("kanban: no live agent %s to assign", assignee))
	}
	x, _, on, err := m.kanbanOf(a.IsolationID)
	if err != nil {
		return "", err
	}
	if !on || !slices.Contains(x.Envs, board) {
		return "", badRequest(fmt.Errorf("kanban: %s cannot be assigned a card on %s: its isolation %s does not see that board", assignee, board, m.isoName(a.IsolationID)))
	}
	return a.ID, nil
}

func (m *Manager) kanbanBoard(s kanbanScope, board string) (string, error) {
	board = strings.TrimSpace(board)
	if board == "" {
		board = s.def
	}
	if board == "" {
		return "", badRequest(errors.New(`kanban: name the card's "board"`))
	}
	colors, _, err := m.kanbanColors()
	if err != nil {
		return "", err
	}
	if _, ok := colors[board]; !ok || !s.sees(board) {
		if s.operator() {
			return "", httpError{http.StatusNotFound, fmt.Errorf("no kanban board %s; make one with `psps env add kanban %s --color COLOR`", board, board)}
		}
		return "", httpError{http.StatusNotFound, fmt.Errorf("kanban: isolation %s is not given a board %s (it is given %s)", s.iso, board, orNone(strings.Join(s.boards, ", ")))}
	}
	return board, nil
}

func (m *Manager) addKanbanCard(s kanbanScope, req api.NewCard) (api.KanbanCard, error) {
	if err := s.need(kanbanproxy.Create); err != nil {
		return api.KanbanCard{}, err
	}
	c := store.KanbanCard{Status: store.CardTodo}
	var err error
	if c.Board, err = m.kanbanBoard(s, req.Board); err != nil {
		return api.KanbanCard{}, err
	}
	if c.Title, err = checkTitle(req.Title); err != nil {
		return api.KanbanCard{}, err
	}
	if err := checkBody(req.Body); err != nil {
		return api.KanbanCard{}, err
	}
	c.Body = req.Body
	if req.Status != "" {
		if c.Status, err = kanbanproxy.ParseStatus(req.Status); err != nil {
			return api.KanbanCard{}, badRequest(fmt.Errorf("kanban: %w", err))
		}
	}
	if c.Assignee, err = m.kanbanAssignee(s, c.Board, req.Assignee); err != nil {
		return api.KanbanCard{}, err
	}
	c, err = m.store.AddCard(c, s.by)
	if err != nil {
		return api.KanbanCard{}, err
	}
	m.pokeChanges()
	colors, _, _ := m.kanbanColors()
	return m.apiCard(c, colors), nil
}

func (m *Manager) updateKanbanCard(s kanbanScope, id int64, p store.CardPatch) (api.KanbanCard, error) {
	cur, err := m.kanbanCard(s, id)
	if err != nil {
		return api.KanbanCard{}, err
	}
	if cur.DeletedAt != nil {
		return api.KanbanCard{}, httpError{http.StatusConflict, fmt.Errorf("card %s is deleted; restore it first", cardKey(id))}
	}
	if p.Title == nil && p.Body == nil && p.Status == nil && p.Assignee == nil && p.Board == nil {
		return api.KanbanCard{}, badRequest(errors.New(`kanban: nothing to change; send any of "title", "body", "status", "assignee", "board"`))
	}
	if p.Title != nil || p.Body != nil || p.Assignee != nil || p.Board != nil {
		if err := s.need(kanbanproxy.Edit); err != nil {
			return api.KanbanCard{}, err
		}
	}
	if p.Status != nil {
		if err := s.need(kanbanproxy.Move); err != nil {
			return api.KanbanCard{}, err
		}
		st, err := kanbanproxy.ParseStatus(*p.Status)
		if err != nil {
			return api.KanbanCard{}, badRequest(fmt.Errorf("kanban: %w", err))
		}
		p.Status = &st
	}
	if p.Title != nil {
		t, err := checkTitle(*p.Title)
		if err != nil {
			return api.KanbanCard{}, err
		}
		p.Title = &t
	}
	if p.Body != nil {
		if err := checkBody(*p.Body); err != nil {
			return api.KanbanCard{}, err
		}
	}
	board := cur.Board
	if p.Board != nil {
		if strings.TrimSpace(*p.Board) == "" {
			return api.KanbanCard{}, badRequest(errors.New(`kanban: "board" names the board to move the card to`))
		}
		if board, err = m.kanbanBoard(s, *p.Board); err != nil {
			return api.KanbanCard{}, err
		}
		p.Board = &board
	}
	if p.Assignee != nil {
		a, err := m.kanbanAssignee(s, board, *p.Assignee)
		if err != nil {
			return api.KanbanCard{}, err
		}
		p.Assignee = &a
	} else if p.Board != nil && cur.Assignee != "" {
		// An assignee that will not see the card on its new board is let go.
		if _, err := m.kanbanAssignee(operatorKanban(), board, cur.Assignee); err != nil {
			none := ""
			p.Assignee = &none
		}
	}
	c, err := m.store.UpdateCard(id, p, s.by)
	if err != nil {
		return api.KanbanCard{}, err
	}
	m.pokeChanges()
	colors, _, _ := m.kanbanColors()
	return m.apiCard(c, colors), nil
}

func (m *Manager) deleteKanbanCard(s kanbanScope, id int64, restore bool) (api.KanbanCard, error) {
	if err := s.need(kanbanproxy.Delete); err != nil {
		return api.KanbanCard{}, err
	}
	if _, err := m.kanbanCard(s, id); err != nil {
		return api.KanbanCard{}, err
	}
	var c store.KanbanCard
	var err error
	if restore {
		c, err = m.store.RestoreCard(id, s.by)
	} else {
		c, err = m.store.DeleteCard(id, s.by)
	}
	if err != nil {
		return api.KanbanCard{}, err
	}
	m.pokeChanges()
	colors, _, _ := m.kanbanColors()
	return m.apiCard(c, colors), nil
}

func checkComment(t string) (string, error) {
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return t, badRequest(errors.New(`kanban: a comment needs "text"`))
	case len(t) > kanbanproxy.MaxComment || !utf8.ValidString(t):
		return t, badRequest(fmt.Errorf("kanban: a comment is UTF-8 text of at most %d bytes", kanbanproxy.MaxComment))
	}
	return t, nil
}

func (m *Manager) addKanbanComment(s kanbanScope, card int64, text string) (store.KanbanComment, error) {
	if err := s.need(kanbanproxy.Comment); err != nil {
		return store.KanbanComment{}, err
	}
	c, err := m.kanbanCard(s, card)
	if err != nil {
		return store.KanbanComment{}, err
	}
	if c.DeletedAt != nil {
		return store.KanbanComment{}, httpError{http.StatusConflict, fmt.Errorf("card %s is deleted; restore it first", cardKey(card))}
	}
	if text, err = checkComment(text); err != nil {
		return store.KanbanComment{}, err
	}
	v, err := m.store.AddComment(card, text, s.by)
	if err == nil {
		m.pokeChanges()
	}
	return v, err
}

// changeKanbanComment edits a comment of s's, or deletes it (text nil).
func (m *Manager) changeKanbanComment(s kanbanScope, id int64, text *string) (store.KanbanComment, error) {
	if err := s.need(kanbanproxy.Comment); err != nil {
		return store.KanbanComment{}, err
	}
	cm, err := m.store.Comment(id)
	if err == nil {
		_, err = m.kanbanCard(s, cm.Card)
	}
	if err != nil || cm.DeletedAt != nil {
		return store.KanbanComment{}, httpError{http.StatusNotFound, fmt.Errorf("kanban: no comment %d on the boards here", id)}
	}
	if text != nil {
		t, cerr := checkComment(*text)
		if cerr != nil {
			return store.KanbanComment{}, cerr
		}
		cm, err = m.store.EditComment(id, t, s.by)
	} else {
		cm, err = m.store.DeleteComment(id, s.by)
	}
	if errors.Is(err, store.ErrCommentNotYours) {
		return cm, httpError{http.StatusForbidden, fmt.Errorf("kanban: comment %d is not yours; only who wrote one edits or deletes it", id)}
	}
	if err == nil {
		m.pokeChanges()
	}
	return cm, err
}

// kanbanLog is the history s sees, newest first: of the boards of
// isolation iso when the operator names one, of board when given.
func (m *Manager) kanbanLog(s kanbanScope, iso, board string, kinds []string, before int64, limit int) ([]api.KanbanEvent, error) {
	boards, colors, err := m.kanbanBoards(s, iso)
	if err != nil {
		return nil, err
	}
	f := store.KanbanFilter{Kinds: kinds, Before: before, Limit: limit}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	if board != "" || !s.operator() || iso != "" {
		f.Boards = []string{}
		for _, b := range boards {
			if board == "" || b.Name == board {
				f.Boards = append(f.Boards, b.Name)
			}
		}
		if board != "" && len(f.Boards) == 0 {
			return nil, httpError{http.StatusNotFound, fmt.Errorf("kanban: no board %s here (%s)", board, boardList(boards))}
		}
	}
	es, err := m.store.KanbanEvents(f)
	if err != nil {
		return nil, err
	}
	return m.kanbanEvents(es, colors), nil
}

// --- the operator's, through the API -------------------------------------

func (m *Manager) Kanban(iso, board, status string) (api.Kanban, error) {
	return m.kanbanView(operatorKanban(), iso, board, status, false)
}

func (m *Manager) KanbanCard(id int64) (api.KanbanCardDetail, error) {
	return m.kanbanDetail(operatorKanban(), id)
}

func (m *Manager) AddKanbanCard(req api.NewCard) (api.KanbanCard, error) {
	return m.addKanbanCard(operatorKanban(), req)
}

func (m *Manager) UpdateKanbanCard(id int64, p store.CardPatch) (api.KanbanCard, error) {
	return m.updateKanbanCard(operatorKanban(), id, p)
}

func (m *Manager) DeleteKanbanCard(id int64, restore bool) (api.KanbanCard, error) {
	return m.deleteKanbanCard(operatorKanban(), id, restore)
}

func (m *Manager) AddKanbanComment(card int64, text string) (store.KanbanComment, error) {
	return m.addKanbanComment(operatorKanban(), card, text)
}

func (m *Manager) ChangeKanbanComment(id int64, text *string) (store.KanbanComment, error) {
	return m.changeKanbanComment(operatorKanban(), id, text)
}

func (m *Manager) KanbanLog(iso, board string, kinds []string, before int64, limit int) ([]api.KanbanEvent, error) {
	return m.kanbanLog(operatorKanban(), iso, board, kinds, before, limit)
}

// kanbanRoutes adds the boards to isolation iso's mail port. Nothing it
// logs carries a path: the path carries the agent's token.
func (m *Manager) kanbanRoutes(mux *http.ServeMux, iso string) {
	scope := func(w http.ResponseWriter, r *http.Request) (kanbanScope, bool) {
		s, err := m.agentKanban(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return s, false
		}
		return s, true
	}
	card := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := kanbanproxy.ParseCard(r.PathValue("card"))
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf("kanban: %w", err)))
			return 0, false
		}
		return id, true
	}
	body := func(w http.ResponseWriter, r *http.Request, v any, shape string) bool {
		b, err := io.ReadAll(io.LimitReader(r.Body, kanbanproxy.MaxBody+16<<10))
		if err == nil {
			err = json.Unmarshal(b, v)
		}
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf("kanban: the body is JSON, as %s: %v", shape, err)))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /a/{token}/kanban", func(w http.ResponseWriter, r *http.Request) {
		if s, ok := scope(w, r); ok {
			q := r.URL.Query()
			v, err := m.kanbanView(s, "", q.Get("board"), q.Get("status"), q.Get("mine") == "1" || q.Get("mine") == "true")
			reply(w, v, err)
		}
	})
	mux.HandleFunc("POST /a/{token}/kanban", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		var req api.NewCard
		if !ok || !body(w, r, &req, `{"board": "...", "title": "...", "body": "...", "status": "todo", "assignee": "me"}`) {
			return
		}
		v, err := m.addKanbanCard(s, req)
		if err == nil {
			m.log.Printf("agent %s: kanban %s made on %s", s.me.ID, v.Key, v.Board)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		reply(w, v, err)
	})
	mux.HandleFunc("GET /a/{token}/kanban/log", func(w http.ResponseWriter, r *http.Request) {
		if s, ok := scope(w, r); ok {
			q := r.URL.Query()
			limit, _ := strconv.Atoi(q.Get("limit"))
			before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
			v, err := m.kanbanLog(s, "", q.Get("board"), nil, before, limit)
			reply(w, v, err)
		}
	})
	mux.HandleFunc("GET /a/{token}/kanban/{card}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		if !ok {
			return
		}
		if id, ok := card(w, r); ok {
			v, err := m.kanbanDetail(s, id)
			reply(w, v, err)
		}
	})
	mux.HandleFunc("PATCH /a/{token}/kanban/{card}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		if !ok {
			return
		}
		id, ok := card(w, r)
		var p store.CardPatch
		if !ok || !body(w, r, &p, `{"title": "...", "body": "...", "status": "doing", "assignee": "me", "board": "..."}`) {
			return
		}
		v, err := m.updateKanbanCard(s, id, p)
		reply(w, v, err)
	})
	mux.HandleFunc("DELETE /a/{token}/kanban/{card}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		if !ok {
			return
		}
		if id, ok := card(w, r); ok {
			v, err := m.deleteKanbanCard(s, id, false)
			reply(w, v, err)
		}
	})
	mux.HandleFunc("POST /a/{token}/kanban/{card}/comments", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		if !ok {
			return
		}
		id, ok := card(w, r)
		var req api.KanbanText
		if !ok || !body(w, r, &req, `{"text": "..."}`) {
			return
		}
		v, err := m.addKanbanComment(s, id, req.Text)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		reply(w, v, err)
	})
	comment := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("comment"), 10, 64)
		if err != nil || id <= 0 {
			reply(w, nil, badRequest(fmt.Errorf("kanban: %q is not a comment's id", r.PathValue("comment"))))
			return 0, false
		}
		return id, true
	}
	mux.HandleFunc("PATCH /a/{token}/kanban/comments/{comment}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		if !ok {
			return
		}
		id, ok := comment(w, r)
		var req api.KanbanText
		if !ok || !body(w, r, &req, `{"text": "..."}`) {
			return
		}
		v, err := m.changeKanbanComment(s, id, &req.Text)
		reply(w, v, err)
	})
	mux.HandleFunc("DELETE /a/{token}/kanban/comments/{comment}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scope(w, r)
		if !ok {
			return
		}
		if id, ok := comment(w, r); ok {
			v, err := m.changeKanbanComment(s, id, nil)
			reply(w, v, err)
		}
	})
}
