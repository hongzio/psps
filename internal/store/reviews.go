package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A Review is a team's round put to its approver (LeadSettings.Approver):
// under ApproveGate the round's eval waits for its verdict, under
// ApproveNotify the approver is only told. It waits to be delivered to the
// approver's live session, exactly once, as receipts do, and under
// ApproveGate stays pending until it is decided or withdrawn: there is no
// deadline.
type Review struct {
	ID   int64  `json:"id"`
	Team string `json:"team"`
	// Approver is the approver's line.
	Approver string `json:"approver"`
	// Why is why the eval was to run: the round ended, or the lead asked.
	Why  string `json:"why"`
	Mode string `json:"mode"`
	// Body is what the approver is told.
	Body        string     `json:"body,omitempty"`
	AskedAt     time.Time  `json:"asked_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	// Verdict is "" while pending, else a Verdict*; Note what came with
	// it, By who decided: the approver as it was shown, or "operator".
	Verdict string `json:"verdict,omitempty"`
	Note    string `json:"note,omitempty"`
	By      string `json:"by,omitempty"`
}

// The approver's modes.
const (
	ApproveGate   = "gate"
	ApproveNotify = "notify"
)

// A review's verdicts.
const (
	VerdictApprove   = "approve"
	VerdictReject    = "reject"
	VerdictWithdrawn = "withdrawn"
)

// Pending is whether r waits for a verdict.
func (r Review) Pending() bool { return r.Mode == ApproveGate && r.Verdict == "" }

const reviewSchema = `
CREATE TABLE IF NOT EXISTS team_reviews (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	team         TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	approver     TEXT NOT NULL,
	why          TEXT NOT NULL,
	mode         TEXT NOT NULL,
	body         TEXT NOT NULL,
	asked_at     TEXT NOT NULL,
	delivered_at TEXT,
	decided_at   TEXT,
	verdict      TEXT NOT NULL DEFAULT '',
	note         TEXT NOT NULL DEFAULT '',
	decided_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS team_reviews_team ON team_reviews(team, id);
CREATE INDEX IF NOT EXISTS team_reviews_approver ON team_reviews(approver, id);
`

const reviewCols = `id, team, approver, why, mode, body, asked_at, delivered_at, decided_at, verdict, note, decided_by`

func scanReview(row interface{ Scan(...any) error }) (Review, error) {
	var r Review
	var asked string
	var delivered, decided sql.NullString
	err := row.Scan(&r.ID, &r.Team, &r.Approver, &r.Why, &r.Mode, &r.Body, &asked, &delivered, &decided, &r.Verdict, &r.Note, &r.By)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.AskedAt = parseTS(asked)
	for _, t := range []struct {
		v  sql.NullString
		to **time.Time
	}{{delivered, &r.DeliveredAt}, {decided, &r.DecidedAt}} {
		if t.v.Valid {
			at := parseTS(t.v.String)
			*t.to = &at
		}
	}
	return r, err
}

func (s *Store) reviews(q string, args ...any) ([]Review, error) {
	rows, err := s.db.Query(`SELECT `+reviewCols+` FROM team_reviews WHERE `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddReview asks r's approver about r's round. Under ApproveGate a team
// has one pending at a time: another is ErrConflict.
func (s *Store) AddReview(r Review) (Review, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	if r.Mode == ApproveGate {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM team_reviews WHERE team = ? AND mode = ? AND verdict = ''`, r.Team, ApproveGate).Scan(&n); err != nil {
			return r, err
		}
		if n > 0 {
			return r, fmt.Errorf("team %s's round waits for its approver already: %w", r.Team, ErrConflict)
		}
	}
	r.AskedAt = time.Now().UTC()
	res, err := tx.Exec(`INSERT INTO team_reviews (team, approver, why, mode, body, asked_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.Team, r.Approver, r.Why, r.Mode, r.Body, ts(r.AskedAt))
	if err != nil {
		return r, err
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// Review is review id.
func (s *Store) Review(id int64) (Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM team_reviews WHERE id = ?`, id))
}

// PendingReview is team's round waiting for its approver's verdict.
func (s *Store) PendingReview(team string) (Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM team_reviews WHERE team = ? AND mode = ? AND verdict = '' ORDER BY id DESC LIMIT 1`, team, ApproveGate))
}

// Reviews are team's latest n, oldest first.
func (s *Store) Reviews(team string, n int) ([]Review, error) {
	rs, err := s.reviews(`team = ? ORDER BY id DESC LIMIT ?`, team, n)
	slices.Reverse(rs)
	return rs, err
}

// ReviewsFor are the reviews waiting for approver (a line) to decide,
// oldest first.
func (s *Store) ReviewsFor(approver string) ([]Review, error) {
	return s.reviews(`approver = ? AND mode = ? AND verdict = '' ORDER BY id`, approver, ApproveGate)
}

// ReviewsWaiting is how many reviews wait to be delivered to approver.
func (s *Store) ReviewsWaiting(approver string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM team_reviews WHERE approver = ? AND delivered_at IS NULL AND verdict = ''`, approver).Scan(&n)
	return n, err
}

// DecideReview gives pending review id its verdict. One no longer pending
// is ErrConflict, with what it came to.
func (s *Store) DecideReview(id int64, verdict, note, by string) (Review, error) {
	res, err := s.db.Exec(`UPDATE team_reviews SET verdict = ?, note = ?, decided_by = ?, decided_at = ? WHERE id = ? AND mode = ? AND verdict = ''`,
		verdict, note, by, ts(time.Now()), id, ApproveGate)
	if err != nil {
		return Review{}, err
	}
	r, err := s.Review(id)
	if err != nil {
		return r, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		was := r.Verdict
		if r.Mode != ApproveGate {
			was = "told only (" + r.Mode + ")"
		}
		return r, fmt.Errorf("review %d is not pending: %s: %w", id, was, ErrConflict)
	}
	return r, nil
}

// WithdrawReviews withdraws team's reviews still pending or waiting to be
// delivered, saying why; it returns the gate ones that were pending.
func (s *Store) WithdrawReviews(team, why string) ([]Review, error) {
	rs, err := s.reviews(`team = ? AND verdict = '' AND (mode = ? OR delivered_at IS NULL) ORDER BY id`, team, ApproveGate)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE team_reviews SET verdict = ?, note = ?, decided_by = 'psps', decided_at = ? WHERE team = ? AND verdict = '' AND (mode = ? OR delivered_at IS NULL)`,
		VerdictWithdrawn, why, ts(time.Now()), team, ApproveGate); err != nil {
		return nil, err
	}
	var out []Review
	for _, r := range rs {
		if r.Mode == ApproveGate {
			out = append(out, r)
		}
	}
	return out, nil
}

// DeliverReviews moves the reviews waiting for approver into agent's
// outbox as one message, frame's of them, oldest first, in one
// transaction: each reaches it exactly once. With none waiting it is
// ErrNotFound.
func (s *Store) DeliverReviews(approver, agent string, after int64, frame func(rs []Review) string) (Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+reviewCols+` FROM team_reviews WHERE approver = ? AND delivered_at IS NULL AND verdict = '' ORDER BY id`, approver)
	if err != nil {
		return Pending{}, err
	}
	var rs []Review
	var ids []any
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			rows.Close()
			return Pending{}, err
		}
		rs, ids = append(rs, r), append(ids, r.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Pending{}, err
	}
	if len(rs) == 0 {
		return Pending{}, fmt.Errorf("reviews for %s: %w", approver, ErrNotFound)
	}
	now := ts(time.Now())
	if _, err := tx.Exec(`UPDATE team_reviews SET delivered_at = ? WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, append([]any{now}, ids...)...); err != nil {
		return Pending{}, err
	}
	// Told is all a notify review asks for.
	if _, err := tx.Exec(`UPDATE team_reviews SET decided_at = ? WHERE mode = ? AND id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, append([]any{now, ApproveNotify}, ids...)...); err != nil {
		return Pending{}, err
	}
	text := frame(rs)
	res, err := tx.Exec(`INSERT INTO outbox (agent, text, after, created_at) VALUES (?, ?, ?, ?)`, agent, text, after, now)
	if err != nil {
		return Pending{}, err
	}
	p := Pending{Text: text, After: after}
	if p.ID, err = res.LastInsertId(); err != nil {
		return Pending{}, err
	}
	return p, tx.Commit()
}

// TeamNewsSince are the bodies of the news team's lead was told after
// since, oldest first: what it heard of a round.
func (s *Store) TeamNewsSince(team string, since time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT body, created_at FROM team_news WHERE team = ? ORDER BY id DESC LIMIT 200`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b, at string
		if err := rows.Scan(&b, &at); err != nil {
			return nil, err
		}
		if !parseTS(at).After(since) {
			break
		}
		out = append(out, b)
	}
	slices.Reverse(out)
	return out, rows.Err()
}
