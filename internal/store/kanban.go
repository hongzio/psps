package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kanban boards (internal/proxy/kanbanproxy): a kanban env is a board, and
// a card sits on one, in one of three columns. Nothing on a board is ever
// changed without a line in its history (KanbanEvent): a card made, each
// field changed, moved, deleted; a comment written, edited, deleted. A
// deleted card or comment stays in the store, out of sight, so its history
// still reads.

// The columns a card is in.
const (
	CardTodo  = "todo"
	CardDoing = "doing"
	CardDone  = "done"
)

// CardStatuses are the columns, in order.
var CardStatuses = []string{CardTodo, CardDoing, CardDone}

// What a KanbanEvent records.
const (
	KanbanCreated        = "created"
	KanbanTitle          = "title"
	KanbanBody           = "body"
	KanbanStatus         = "status"
	KanbanAssignee       = "assignee"
	KanbanBoard          = "board"
	KanbanDeleted        = "deleted"
	KanbanRestored       = "restored"
	KanbanCommented      = "comment"
	KanbanCommentEdited  = "comment_edited"
	KanbanCommentDeleted = "comment_deleted"
	// A checklist item in the body checked or unchecked, and nothing else
	// changed: To is the item's text.
	KanbanChecked   = "checked"
	KanbanUnchecked = "unchecked"
)

// A KanbanActor is who did something on a board: an agent (by ID, with
// its name then), or the operator (Agent empty).
type KanbanActor struct {
	Agent string `json:"agent,omitempty"`
	Name  string `json:"name"`
}

// Operator is the operator as an actor.
var Operator = KanbanActor{Name: "operator"}

// A KanbanCard is one card on one board. It is known by its board's slug
// and its number there (ACME-12); ID is psps's own, for links that must
// outlive a move.
type KanbanCard struct {
	ID    int64  `json:"id"`
	Board string `json:"board"`
	// Num is its number on its board: a card moved to another board is
	// given the next number there.
	Num    int64  `json:"num"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	Status string `json:"status"`
	// Assignee is an agent's ID, or empty.
	Assignee  string      `json:"assignee,omitempty"`
	CreatedBy KanbanActor `json:"created_by"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	// MovedAt is when it came into its column: a column lists its cards
	// in that order.
	MovedAt   time.Time  `json:"moved_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Comments counts those not deleted.
	Comments int `json:"comments"`
}

// A KanbanComment is what someone wrote on a card.
type KanbanComment struct {
	ID        int64       `json:"id"`
	Card      int64       `json:"card"`
	By        KanbanActor `json:"by"`
	Text      string      `json:"text"`
	CreatedAt time.Time   `json:"created_at"`
	EditedAt  *time.Time  `json:"edited_at,omitempty"`
	DeletedAt *time.Time  `json:"deleted_at,omitempty"`
}

// A KanbanEvent is one change on a board: what changed (Kind), from what
// to what, by whom. A comment's carries its ID, and its text in To (From
// too, edited or deleted).
type KanbanEvent struct {
	ID      int64       `json:"id"`
	Card    int64       `json:"card"`
	Board   string      `json:"board"`
	By      KanbanActor `json:"by"`
	Kind    string      `json:"kind"`
	From    string      `json:"from,omitempty"`
	To      string      `json:"to,omitempty"`
	Comment int64       `json:"comment,omitempty"`
	At      time.Time   `json:"at"`
}

// A CardPatch changes the fields it sets.
type CardPatch struct {
	Title    *string `json:"title,omitempty"`
	Body     *string `json:"body,omitempty"`
	Status   *string `json:"status,omitempty"`
	Assignee *string `json:"assignee,omitempty"`
	Board    *string `json:"board,omitempty"`
}

const kanbanSchema = `
CREATE TABLE IF NOT EXISTS kanban_cards (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	board       TEXT NOT NULL,
	title       TEXT NOT NULL,
	body        TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	num         INTEGER NOT NULL DEFAULT 0,
	assignee    TEXT NOT NULL DEFAULT '',
	by_agent    TEXT NOT NULL DEFAULT '',
	by_name     TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	moved_at    TEXT NOT NULL,
	deleted_at  TEXT
);
CREATE INDEX IF NOT EXISTS kanban_cards_board ON kanban_cards(board, status);
CREATE TABLE IF NOT EXISTS kanban_comments (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	card        INTEGER NOT NULL REFERENCES kanban_cards(id),
	by_agent    TEXT NOT NULL DEFAULT '',
	by_name     TEXT NOT NULL DEFAULT '',
	text        TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	edited_at   TEXT,
	deleted_at  TEXT
);
CREATE INDEX IF NOT EXISTS kanban_comments_card ON kanban_comments(card, id);
CREATE TABLE IF NOT EXISTS kanban_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	card        INTEGER NOT NULL REFERENCES kanban_cards(id),
	board       TEXT NOT NULL,
	by_agent    TEXT NOT NULL DEFAULT '',
	by_name     TEXT NOT NULL DEFAULT '',
	kind        TEXT NOT NULL,
	old         TEXT NOT NULL DEFAULT '',
	new         TEXT NOT NULL DEFAULT '',
	comment     INTEGER NOT NULL DEFAULT 0,
	at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS kanban_events_card ON kanban_events(card, id);
CREATE INDEX IF NOT EXISTS kanban_events_board ON kanban_events(board, id);
CREATE TABLE IF NOT EXISTS kanban_keys (
	slug  TEXT NOT NULL,
	num   INTEGER NOT NULL,
	card  INTEGER NOT NULL REFERENCES kanban_cards(id),
	PRIMARY KEY (slug, num)
);
CREATE INDEX IF NOT EXISTS kanban_keys_card ON kanban_keys(card);
`

// ErrCommentNotYours refuses editing or deleting another's comment.
var ErrCommentNotYours = errors.New("not your comment")

const cardCols = `c.id, c.board, c.num, c.title, c.body, c.status, c.assignee, c.by_agent, c.by_name, c.created_at, c.updated_at, c.moved_at, c.deleted_at,
	(SELECT COUNT(*) FROM kanban_comments k WHERE k.card = c.id AND k.deleted_at IS NULL)`

func scanCard(row interface{ Scan(...any) error }) (KanbanCard, error) {
	var c KanbanCard
	var created, updated, moved string
	var deleted sql.NullString
	err := row.Scan(&c.ID, &c.Board, &c.Num, &c.Title, &c.Body, &c.Status, &c.Assignee, &c.CreatedBy.Agent, &c.CreatedBy.Name,
		&created, &updated, &moved, &deleted, &c.Comments)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.CreatedAt, c.UpdatedAt, c.MovedAt = parseTS(created), parseTS(updated), parseTS(moved)
	c.DeletedAt = parseNullTS(deleted)
	return c, err
}

func parseNullTS(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

func kanbanNow() time.Time { return time.Now().UTC() }

type kanbanTx interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func addKanbanEvent(tx kanbanTx, e KanbanEvent) error {
	_, err := tx.Exec(`INSERT INTO kanban_events (card, board, by_agent, by_name, kind, old, new, comment, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Card, e.Board, e.By.Agent, e.By.Name, e.Kind, e.From, e.To, e.Comment, ts(e.At))
	return err
}

// AddCard puts c on its board, made by by, and returns it with its ID.
func (s *Store) AddCard(c KanbanCard, by KanbanActor) (KanbanCard, error) {
	now := kanbanNow()
	tx, err := s.db.Begin()
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO kanban_cards (board, title, body, status, assignee, by_agent, by_name, created_at, updated_at, moved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.Board, c.Title, c.Body, c.Status, c.Assignee, by.Agent, by.Name, ts(now), ts(now), ts(now))
	if err != nil {
		return c, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return c, err
	}
	if _, err := numberCard(tx, id, c.Board); err != nil {
		return c, err
	}
	if err := addKanbanEvent(tx, KanbanEvent{Card: id, Board: c.Board, By: by, Kind: KanbanCreated, To: c.Status, At: now}); err != nil {
		return c, err
	}
	if c.Assignee != "" {
		if err := addKanbanEvent(tx, KanbanEvent{Card: id, Board: c.Board, By: by, Kind: KanbanAssignee, To: c.Assignee, At: now}); err != nil {
			return c, err
		}
	}
	if err := tx.Commit(); err != nil {
		return c, err
	}
	return s.Card(id)
}

// Card is a card, deleted or not.
func (s *Store) Card(id int64) (KanbanCard, error) {
	c, err := scanCard(s.db.QueryRow(`SELECT `+cardCols+` FROM kanban_cards c WHERE c.id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return c, fmt.Errorf("no card K-%d: %w", id, ErrNotFound)
	}
	return c, err
}

// Cards are the cards on boards, not deleted: by column, then the order
// they came into it.
func (s *Store) Cards(boards []string) ([]KanbanCard, error) {
	out := []KanbanCard{}
	if len(boards) == 0 {
		return out, nil
	}
	q := `SELECT ` + cardCols + ` FROM kanban_cards c WHERE c.deleted_at IS NULL AND c.board IN (` + placeholders(len(boards)) + `)
		ORDER BY CASE c.status WHEN 'todo' THEN 0 WHEN 'doing' THEN 1 ELSE 2 END, c.moved_at, c.id`
	rows, err := s.db.Query(q, anys(boards)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LiveCards counts the cards on board that are not deleted.
func (s *Store) LiveCards(board string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM kanban_cards WHERE board = ? AND deleted_at IS NULL`, board).Scan(&n)
	return n, err
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// UpdateCard applies p to card id, by by: each field that changes is a
// line in its history. A card that is deleted is not changed.
func (s *Store) UpdateCard(id int64, p CardPatch, by KanbanActor) (KanbanCard, error) {
	cur, err := s.Card(id)
	if err != nil {
		return cur, err
	}
	if cur.DeletedAt != nil {
		return cur, fmt.Errorf("card K-%d is deleted: %w", id, ErrNotFound)
	}
	now := kanbanNow()
	tx, err := s.db.Begin()
	if err != nil {
		return cur, err
	}
	defer tx.Rollback()
	changed := false
	// The board first: the lines after it are on the board it went to.
	board := cur.Board
	set := func(kind, col string, old string, to *string) error {
		if to == nil || *to == old {
			return nil
		}
		changed = true
		if kind == KanbanBoard {
			board = *to
		}
		if _, err := tx.Exec(`UPDATE kanban_cards SET `+col+` = ? WHERE id = ?`, *to, id); err != nil {
			return err
		}
		// Checking items of its checklist is a line for each, not the
		// whole body twice.
		if kind == KanbanBody {
			if ts := taskToggles(old, *to); ts != nil {
				for _, t := range ts {
					k := KanbanUnchecked
					if t.Done {
						k = KanbanChecked
					}
					if err := addKanbanEvent(tx, KanbanEvent{Card: id, Board: board, By: by, Kind: k, To: t.Text, At: now}); err != nil {
						return err
					}
				}
				return nil
			}
		}
		if kind == KanbanStatus {
			if _, err := tx.Exec(`UPDATE kanban_cards SET moved_at = ? WHERE id = ?`, ts(now), id); err != nil {
				return err
			}
		}
		// On another board it is that board's next card; its old key
		// still finds it.
		if kind == KanbanBoard {
			if _, err := numberCard(tx, id, *to); err != nil {
				return err
			}
		}
		return addKanbanEvent(tx, KanbanEvent{Card: id, Board: board, By: by, Kind: kind, From: old, To: *to, At: now})
	}
	for _, f := range []struct {
		kind, col, old string
		to             *string
	}{
		{KanbanBoard, "board", cur.Board, p.Board},
		{KanbanTitle, "title", cur.Title, p.Title},
		{KanbanBody, "body", cur.Body, p.Body},
		{KanbanStatus, "status", cur.Status, p.Status},
		{KanbanAssignee, "assignee", cur.Assignee, p.Assignee},
	} {
		if err := set(f.kind, f.col, f.old, f.to); err != nil {
			return cur, err
		}
	}
	if !changed {
		return cur, nil
	}
	if _, err := tx.Exec(`UPDATE kanban_cards SET updated_at = ? WHERE id = ?`, ts(now), id); err != nil {
		return cur, err
	}
	if err := tx.Commit(); err != nil {
		return cur, err
	}
	return s.Card(id)
}

// DeleteCard takes a card off its board; it and its history stay in the
// store.
func (s *Store) DeleteCard(id int64, by KanbanActor) (KanbanCard, error) {
	return s.setCardDeleted(id, by, true)
}

// RestoreCard puts a deleted card back where it was.
func (s *Store) RestoreCard(id int64, by KanbanActor) (KanbanCard, error) {
	return s.setCardDeleted(id, by, false)
}

func (s *Store) setCardDeleted(id int64, by KanbanActor, del bool) (KanbanCard, error) {
	cur, err := s.Card(id)
	if err != nil {
		return cur, err
	}
	if (cur.DeletedAt != nil) == del {
		return cur, nil
	}
	now := kanbanNow()
	tx, err := s.db.Begin()
	if err != nil {
		return cur, err
	}
	defer tx.Rollback()
	var at any
	kind := KanbanRestored
	if del {
		at, kind = ts(now), KanbanDeleted
	}
	if _, err := tx.Exec(`UPDATE kanban_cards SET deleted_at = ?, updated_at = ? WHERE id = ?`, at, ts(now), id); err != nil {
		return cur, err
	}
	if err := addKanbanEvent(tx, KanbanEvent{Card: id, Board: cur.Board, By: by, Kind: kind, From: cur.Title, At: now}); err != nil {
		return cur, err
	}
	if err := tx.Commit(); err != nil {
		return cur, err
	}
	return s.Card(id)
}

const commentCols = `id, card, by_agent, by_name, text, created_at, edited_at, deleted_at`

func scanComment(row interface{ Scan(...any) error }) (KanbanComment, error) {
	var c KanbanComment
	var created string
	var edited, deleted sql.NullString
	err := row.Scan(&c.ID, &c.Card, &c.By.Agent, &c.By.Name, &c.Text, &created, &edited, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.CreatedAt = parseTS(created)
	c.EditedAt, c.DeletedAt = parseNullTS(edited), parseNullTS(deleted)
	return c, err
}

// AddComment writes text on card, by by.
func (s *Store) AddComment(card int64, text string, by KanbanActor) (KanbanComment, error) {
	cur, err := s.Card(card)
	if err != nil {
		return KanbanComment{}, err
	}
	if cur.DeletedAt != nil {
		return KanbanComment{}, fmt.Errorf("card K-%d is deleted: %w", card, ErrNotFound)
	}
	now := kanbanNow()
	tx, err := s.db.Begin()
	if err != nil {
		return KanbanComment{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO kanban_comments (card, by_agent, by_name, text, created_at) VALUES (?, ?, ?, ?, ?)`,
		card, by.Agent, by.Name, text, ts(now))
	if err != nil {
		return KanbanComment{}, err
	}
	id, _ := res.LastInsertId()
	if err := addKanbanEvent(tx, KanbanEvent{Card: card, Board: cur.Board, By: by, Kind: KanbanCommented, To: text, Comment: id, At: now}); err != nil {
		return KanbanComment{}, err
	}
	if _, err := tx.Exec(`UPDATE kanban_cards SET updated_at = ? WHERE id = ?`, ts(now), card); err != nil {
		return KanbanComment{}, err
	}
	if err := tx.Commit(); err != nil {
		return KanbanComment{}, err
	}
	return s.Comment(id)
}

// Comment is one comment, deleted or not.
func (s *Store) Comment(id int64) (KanbanComment, error) {
	c, err := scanComment(s.db.QueryRow(`SELECT `+commentCols+` FROM kanban_comments WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return c, fmt.Errorf("no comment %d: %w", id, ErrNotFound)
	}
	return c, err
}

// Comments are a card's, not deleted, oldest first.
func (s *Store) Comments(card int64) ([]KanbanComment, error) {
	rows, err := s.db.Query(`SELECT `+commentCols+` FROM kanban_comments WHERE card = ? AND deleted_at IS NULL ORDER BY id`, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KanbanComment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EditComment changes the text of a comment of by's.
func (s *Store) EditComment(id int64, text string, by KanbanActor) (KanbanComment, error) {
	return s.changeComment(id, &text, by)
}

// DeleteComment takes a comment of by's off its card; its text stays in
// the history.
func (s *Store) DeleteComment(id int64, by KanbanActor) (KanbanComment, error) {
	return s.changeComment(id, nil, by)
}

func (s *Store) changeComment(id int64, text *string, by KanbanActor) (KanbanComment, error) {
	cur, err := s.Comment(id)
	if err != nil {
		return cur, err
	}
	if cur.DeletedAt != nil {
		return cur, fmt.Errorf("comment %d is deleted: %w", id, ErrNotFound)
	}
	if cur.By.Agent != by.Agent {
		return cur, ErrCommentNotYours
	}
	if text != nil && *text == cur.Text {
		return cur, nil
	}
	card, err := s.Card(cur.Card)
	if err != nil {
		return cur, err
	}
	now := kanbanNow()
	tx, err := s.db.Begin()
	if err != nil {
		return cur, err
	}
	defer tx.Rollback()
	e := KanbanEvent{Card: cur.Card, Board: card.Board, By: by, From: cur.Text, Comment: id, At: now}
	if text != nil {
		e.Kind, e.To = KanbanCommentEdited, *text
		_, err = tx.Exec(`UPDATE kanban_comments SET text = ?, edited_at = ? WHERE id = ?`, *text, ts(now), id)
	} else {
		e.Kind = KanbanCommentDeleted
		_, err = tx.Exec(`UPDATE kanban_comments SET deleted_at = ? WHERE id = ?`, ts(now), id)
	}
	if err != nil {
		return cur, err
	}
	if err := addKanbanEvent(tx, e); err != nil {
		return cur, err
	}
	if err := tx.Commit(); err != nil {
		return cur, err
	}
	return s.Comment(id)
}

// KanbanFilter picks lines of history: of one card, or of some boards;
// before an event's ID, for the page after; at most Limit.
type KanbanFilter struct {
	Card   int64
	Boards []string
	Kinds  []string
	Before int64
	Limit  int
}

// KanbanEvents are the lines of history f picks, newest first.
func (s *Store) KanbanEvents(f KanbanFilter) ([]KanbanEvent, error) {
	where := []string{"1=1"}
	var args []any
	if f.Card != 0 {
		where = append(where, "card = ?")
		args = append(args, f.Card)
	}
	if f.Boards != nil {
		if len(f.Boards) == 0 {
			return []KanbanEvent{}, nil
		}
		where = append(where, "board IN ("+placeholders(len(f.Boards))+")")
		args = append(args, anys(f.Boards)...)
	}
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+placeholders(len(f.Kinds))+")")
		args = append(args, anys(f.Kinds)...)
	}
	if f.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	args = append(args, f.Limit)
	rows, err := s.db.Query(`SELECT id, card, board, by_agent, by_name, kind, old, new, comment, at FROM kanban_events
		WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KanbanEvent{}
	for rows.Next() {
		var e KanbanEvent
		var at string
		if err := rows.Scan(&e.ID, &e.Card, &e.Board, &e.By.Agent, &e.By.Name, &e.Kind, &e.From, &e.To, &e.Comment, &at); err != nil {
			return nil, err
		}
		e.At = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastKanbanEvent is the ID of the newest line of any board's history.
func (s *Store) LastKanbanEvent() (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(id) FROM kanban_events`).Scan(&id)
	return id.Int64, err
}

// renameBoard moves a board's cards and history to its new name, in the
// env rename's transaction.
func renameBoard(tx kanbanTx, name, to string) error {
	if _, err := tx.Exec(`UPDATE kanban_cards SET board = ? WHERE board = ?`, to, name); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE kanban_events SET board = ? WHERE board = ?`, to, name)
	return err
}

// A CardTask is an item of a checklist in a card's body: a markdown task
// list item, "- [ ] text" or "- [x] text".
type CardTask struct {
	// Line is its line in the body, from 0.
	Line int    `json:"-"`
	Done bool   `json:"done"`
	Text string `json:"text"`
}

var taskLine = regexp.MustCompile(`^(\s*(?:>\s*)*(?:[-*+]|\d{1,9}[.)])\s+\[)([ xX])(\]\s+)(\S.*?)\s*$`)
var fenceLine = regexp.MustCompile("^\\s*(```|~~~)")

// CardTasks are the checklist items in body, in order; those in a fenced
// code block are not.
func CardTasks(body string) []CardTask {
	var out []CardTask
	fence := ""
	for i, l := range strings.Split(body, "\n") {
		if m := fenceLine.FindStringSubmatch(l); m != nil {
			switch fence {
			case "":
				fence = m[1]
			case m[1]:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := taskLine.FindStringSubmatch(l); m != nil {
			out = append(out, CardTask{Line: i, Done: m[2] != " ", Text: m[4]})
		}
	}
	return out
}

// CheckTask is body with its item-th checklist item (from 0) checked or
// not.
func CheckTask(body string, item int, done bool) (string, error) {
	ts := CardTasks(body)
	if item < 0 || item >= len(ts) {
		return body, fmt.Errorf("the card's checklist has %d items, not an item %d", len(ts), item+1)
	}
	lines := strings.Split(body, "\n")
	mark := " "
	if done {
		mark = "x"
	}
	lines[ts[item].Line] = taskLine.ReplaceAllString(lines[ts[item].Line], "${1}"+mark+"${3}${4}")
	return strings.Join(lines, "\n"), nil
}

// taskToggles are the items checked or unchecked between old and new, when
// that is all that changed; nil otherwise.
func taskToggles(old, new string) []CardTask {
	ol, nl := strings.Split(old, "\n"), strings.Split(new, "\n")
	if len(ol) != len(nl) {
		return nil
	}
	ot, nt := CardTasks(old), CardTasks(new)
	if len(ot) != len(nt) {
		return nil
	}
	byLine := map[int]int{}
	for i, t := range ot {
		byLine[t.Line] = i
	}
	var out []CardTask
	for i := range ol {
		if ol[i] == nl[i] {
			continue
		}
		j, ok := byLine[i]
		if !ok || j >= len(nt) || nt[j].Line != i || nt[j].Text != ot[j].Text || nt[j].Done == ot[j].Done {
			return nil
		}
		out = append(out, nt[j])
	}
	return out
}

// --- keys: a board's slug and a card's number on it ---------------------

var slugRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// ValidSlug is a board's slug as it may be: an upper-case letter, then up
// to nine upper-case letters and digits.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// DefaultSlug is a slug made of a board's name: its letters and digits,
// upper case (acme -> ACME).
func DefaultSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		s = "B" + s
	}
	if len(s) > 10 {
		s = s[:10]
	}
	return s
}

// boardSlug is board's slug, as its env says, or one made of its name.
func boardSlug(q kanbanTx, board string) string {
	var slug sql.NullString
	q.QueryRow(`SELECT json_extract(config, '$.slug') FROM envs WHERE service = 'kanban' AND name = ?`, board).Scan(&slug)
	if slug.Valid && slug.String != "" {
		return slug.String
	}
	return DefaultSlug(board)
}

// numberCard gives card the next number on board, under its slug, and
// keeps the key.
func numberCard(tx kanbanTx, card int64, board string) (int64, error) {
	slug := boardSlug(tx, board)
	var max sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(num) FROM kanban_keys WHERE slug = ?`, slug).Scan(&max); err != nil {
		return 0, err
	}
	num := max.Int64 + 1
	if _, err := tx.Exec(`UPDATE kanban_cards SET num = ? WHERE id = ?`, num, card); err != nil {
		return 0, err
	}
	_, err := tx.Exec(`INSERT INTO kanban_keys (slug, num, card) VALUES (?, ?, ?)`, slug, num, card)
	return num, err
}

// CardByKey is the card slug-num names: its key now, or one it had (on a
// board it left, or under its board's slug before).
func (s *Store) CardByKey(slug string, num int64) (KanbanCard, error) {
	var id int64
	err := s.db.QueryRow(`SELECT card FROM kanban_keys WHERE slug = ? AND num = ?`, slug, num).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return KanbanCard{}, fmt.Errorf("no card %s-%d: %w", slug, num, ErrNotFound)
	}
	if err != nil {
		return KanbanCard{}, err
	}
	return s.Card(id)
}

// SlugUsed is whether slug is, or was, how cards not on board are known.
func (s *Store) SlugUsed(slug, board string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM kanban_keys k JOIN kanban_cards c ON c.id = k.card WHERE k.slug = ? AND c.board != ?`, slug, board).Scan(&n)
	return n > 0, err
}

// ReslugBoard keys board's cards under its slug as its env has it now:
// each keeps its number there, or, where another card has that key, is
// given the next one. The keys they had still find them.
func (s *Store) ReslugBoard(board string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := reslug(tx, board); err != nil {
		return err
	}
	return tx.Commit()
}

func reslug(tx *sql.Tx, board string) error {
	slug := boardSlug(tx, board)
	rows, err := tx.Query(`SELECT id, num FROM kanban_cards WHERE board = ? ORDER BY num, id`, board)
	if err != nil {
		return err
	}
	type card struct{ id, num int64 }
	var cs []card
	for rows.Next() {
		var c card
		rows.Scan(&c.id, &c.num)
		cs = append(cs, c)
	}
	rows.Close()
	for _, c := range cs {
		var owner sql.NullInt64
		tx.QueryRow(`SELECT card FROM kanban_keys WHERE slug = ? AND num = ?`, slug, c.num).Scan(&owner)
		switch {
		case c.num > 0 && owner.Valid && owner.Int64 == c.id:
		case c.num > 0 && !owner.Valid:
			if _, err := tx.Exec(`INSERT INTO kanban_keys (slug, num, card) VALUES (?, ?, ?)`, slug, c.num, c.id); err != nil {
				return err
			}
		default:
			if _, err := numberCard(tx, c.id, board); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateKanbanKeys gives boards from before slugs one made of their name
// (acme -> ACME), and their cards a number on them: the number they had
// (K-12 is ACME-12), so what was said of them still finds them.
func migrateKanbanKeys(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT name FROM envs WHERE service = 'kanban' AND COALESCE(json_extract(config, '$.slug'), '') = '' ORDER BY name`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		slug, base := DefaultSlug(n), DefaultSlug(n)
		for i := 2; ; i++ {
			var taken int
			tx.QueryRow(`SELECT COUNT(*) FROM envs WHERE service = 'kanban' AND json_extract(config, '$.slug') = ?`, slug).Scan(&taken)
			if taken == 0 {
				break
			}
			suffix := strconv.Itoa(i)
			slug = base[:min(len(base), 10-len(suffix))] + suffix
		}
		if _, err := tx.Exec(`UPDATE envs SET config = json_set(config, '$.slug', ?) WHERE service = 'kanban' AND name = ?`, slug, n); err != nil {
			return err
		}
	}
	// Cards from before numbers keep theirs.
	if _, err := tx.Exec(`UPDATE kanban_cards SET num = id WHERE num = 0`); err != nil {
		return err
	}
	rows, err = tx.Query(`SELECT DISTINCT c.board FROM kanban_cards c WHERE NOT EXISTS (SELECT 1 FROM kanban_keys k WHERE k.card = c.id)`)
	if err != nil {
		return err
	}
	var boards []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		boards = append(boards, b)
	}
	rows.Close()
	for _, b := range boards {
		if err := reslug(tx, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}
