package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// A round's approval (store.Review): a team's operator may name an
// approver, a live agent that is neither the lead nor one of its own. Under
// store.ApproveGate a round that ends, or an eval its lead asks for, does
// not go to the eval: psps puts it to the approver, and the eval runs once
// it approves; a round it rejects is scored by none, and the lead hears
// why. There is no deadline: the ask waits, across restarts, until it is
// decided, by the approver or the operator. Under store.ApproveNotify the
// eval runs as ever and the approver is told, to look at the round after.
// Only the operator sets the approver: a lead or chief could otherwise lift
// its own gate.

const (
	// reviewNews is how much of the round's news an ask carries, and
	// reviewItem how much of each item.
	reviewNews = 24 << 10
	reviewItem = 4 << 10
	// reviewNoteMax caps a verdict's note.
	reviewNoteMax = 8 << 10
	// whyLeadAsked is why an eval its lead asked for runs.
	whyLeadAsked = "you asked"
)

// approverTerms sets s's approver and its mode from req, checked, for the
// lead lead.
func (m *Manager) approverTerms(s *store.LeadSettings, req api.PutLead, lead store.Agent) error {
	line := cmp.Or(lead.Line, lead.ID)
	if req.Approver != nil {
		s.Approver = ""
		if ref := strings.TrimSpace(*req.Approver); ref != "" && ref != "-" {
			a, err := m.storedAgent(ref)
			if err != nil {
				return badRequest(fmt.Errorf("the approver: %w", err))
			}
			switch {
			case a.ArchivedAt != nil:
				return badRequest(fmt.Errorf("the approver %s is archived; an approver is a live agent", shownID(a)))
			case a.MadeBy == line:
				return badRequest(fmt.Errorf("the approver %s was made by the lead; an approver is no agent of the lead's own", shownID(a)))
			}
			s.Approver = cmp.Or(a.Line, a.ID)
		}
	}
	if s.Approver == line {
		return badRequest(errors.New("the lead does not approve its own rounds; name another agent as the approver"))
	}
	if req.ApproveMode != nil {
		switch mode := strings.TrimSpace(*req.ApproveMode); mode {
		case "", store.ApproveGate, store.ApproveNotify:
			s.ApproveMode = mode
		default:
			return badRequest(fmt.Errorf("the approval's mode is %q (the eval waits for the approver) or %q (it is only told), not %q", store.ApproveGate, store.ApproveNotify, mode))
		}
	}
	if s.Approver == "" {
		s.ApproveMode = ""
	} else {
		s.ApproveMode = cmp.Or(s.ApproveMode, store.ApproveGate)
	}
	return nil
}

// approverChanged is whether b puts rounds to another than a does, or
// otherwise.
func approverChanged(a, b store.LeadSettings) bool {
	return a.Approver != b.Approver || a.ApproveMode != b.ApproveMode
}

// withdrawReviewsLocked withdraws team's asks still open, saying why, and
// tells their approvers' sessions. A round that waited for a verdict is
// looked at anew: it is to be scored, or put to whoever approves now.
func (m *Manager) withdrawReviewsLocked(team, why string) (bool, error) {
	rs, err := m.store.WithdrawReviews(team, why)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		m.log.Printf("team %s: review #%d withdrawn: %s", team, r.ID, why)
		go m.reviewsChanged(r.Approver)
	}
	return len(rs) > 0, nil
}

// pendingReview is l's round waiting for its approver's verdict, if any.
func (m *Manager) pendingReview(l store.Lead) (store.Review, bool) {
	if !l.Settings.Gated() {
		return store.Review{}, false
	}
	r, err := m.store.PendingReview(l.Team)
	return r, err == nil
}

// askReviewLocked puts l's round to its approver, as why the eval was to
// run. Under ApproveGate the round's change is under review: a turn of the
// lead's agents that ends after makes it to be scored again.
func (m *Manager) askReviewLocked(l store.Lead, why string) (store.Review, error) {
	r, err := m.store.AddReview(store.Review{Team: l.Team, Approver: l.Settings.Approver, Why: why, Mode: l.Settings.ApproveMode, Body: m.reviewBody(l, why)})
	if err != nil {
		return r, err
	}
	if r.Mode == store.ApproveGate {
		if err := m.store.SetLeadDirty(l.Team, false); err != nil {
			m.log.Printf("team %s: %v", l.Team, err)
		}
	}
	who, _ := m.approverShown(l.Settings.Approver)
	m.log.Printf("team %s: review #%d put to %s (%s, %s)", l.Team, r.ID, who, r.Mode, why)
	go m.reviewsChanged(r.Approver)
	return r, nil
}

// reviewBody is what l's approver is told of the round: why the eval was
// to run, the scores so far, the news its lead heard since the last eval,
// and the lead's last answer.
func (m *Manager) reviewBody(l store.Lead, why string) string {
	t, _ := m.store.Team(l.Team)
	var b strings.Builder
	lead := l.Line
	answer := ""
	if r := m.lineRuntime(l.Line); r != nil {
		a, _, _ := r.view()
		lead = shownID(a)
		r.mu.Lock()
		answer = r.answer
		r.mu.Unlock()
	}
	what := "Its round ended: the lead's agents are done and the lead is idle."
	if why == whyLeadAsked {
		what = "Its lead asked for an eval."
	}
	fmt.Fprintf(&b, "Team %q, led by %s. %s", t.Name, lead, what)
	evals, _ := m.store.Evals(l.Team)
	since := l.Since
	if n := len(evals); n > 0 {
		last := evals[n-1]
		since = last.At
		score := "no score"
		if last.Score != nil {
			score = strconv.FormatFloat(*last.Score, 'g', -1, 64)
		}
		fmt.Fprintf(&b, " The last eval is #%d (%s, %s): %s.", last.ID, last.Why, last.At.Local().Format("Jan 2 15:04"), score)
		if best, stale := bestOf(evals, l); best != nil {
			fmt.Fprintf(&b, " The best is %g at #%d, %d since without a new one (patience %d).", *best.Score, best.ID, stale, l.Settings.Patience)
		}
	} else {
		b.WriteString(" No eval has run yet.")
	}
	news, _ := m.store.TeamNewsSince(l.Team, since)
	// The latest first, as much as fits: the round's end is what counts.
	var kept []string
	room := reviewNews
	for i := len(news) - 1; i >= 0 && room > 0; i-- {
		n := tail(news[i], min(reviewItem, room))
		kept = append([]string{n}, kept...)
		room -= len(n)
	}
	if len(kept) > 0 {
		fmt.Fprintf(&b, "\n\nWhat psps told the lead since that eval, oldest first (%d of %d item(s), each cut to its end):", len(kept), len(news))
		for i, n := range kept {
			fmt.Fprintf(&b, "\n\n--- %d.\n%s", i+1, n)
		}
	}
	if answer != "" {
		b.WriteString("\n\n--- The lead's last answer:\n" + tail(answer, newsAnswer))
	}
	return b.String()
}

// approverShown is approver (a line) as it goes by now, and whether it has
// a live session.
func (m *Manager) approverShown(line string) (string, bool) {
	if line == "" {
		return "", false
	}
	if r := m.lineRuntime(line); r != nil {
		a, _, _ := r.view()
		return shownID(a), true
	}
	agents, err := m.store.Agents()
	if err != nil {
		return line, false
	}
	shown := line
	for _, a := range agents {
		if cmp.Or(a.Line, a.ID) == line {
			shown = shownID(a)
			if a.ArchivedAt == nil {
				return shown, false
			}
		}
	}
	return shown, false
}

// reviewWaiting says what l's run waits for while its round waits for its
// approver; "" when it does not.
func (m *Manager) reviewWaiting(l store.Lead) string {
	r, ok := m.pendingReview(l)
	if !ok {
		return ""
	}
	who, live := m.approverShown(r.Approver)
	s := fmt.Sprintf("waiting for %s to approve the round's eval (review #%d, asked %s ago)", who, r.ID, time.Since(r.AskedAt).Round(time.Minute))
	if !live {
		s += fmt.Sprintf("; %s has no live session, so decide yourself (psps team approve|reject)", who)
	}
	return s
}

// --- deciding ---------------------------------------------------------------

// decideReview gives review id its verdict, by by; approver, if not "",
// is the line that must be the review's approver.
func (m *Manager) decideReview(id int64, req api.PostReview, by, approver string) (store.Review, error) {
	verdict := strings.TrimSpace(req.Verdict)
	note := strings.TrimSpace(req.Note)
	switch verdict {
	case store.VerdictApprove, store.VerdictReject:
	default:
		return store.Review{}, badRequest(fmt.Errorf(`the verdict is %q (the eval runs) or %q (it does not, and the lead hears your note), not %q`, store.VerdictApprove, store.VerdictReject, verdict))
	}
	if verdict == store.VerdictReject && note == "" {
		return store.Review{}, badRequest(errors.New(`say why the round is rejected, as "note": the lead hears it`))
	}
	if len(note) > reviewNoteMax {
		return store.Review{}, badRequest(fmt.Errorf("a note is at most %d bytes", reviewNoteMax))
	}
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	r, err := m.store.Review(id)
	if err != nil || (approver != "" && r.Approver != approver) {
		return store.Review{}, fmt.Errorf("review %d is not one of yours (GET $PSPS_TEAM_URL/approvals lists them): %w", id, store.ErrNotFound)
	}
	if r, err = m.store.DecideReview(id, verdict, note, by); err != nil {
		return r, err
	}
	m.log.Printf("team %s: review #%d: %s by %s", r.Team, r.ID, verdict, by)
	l, err := m.store.Lead(r.Team)
	if err != nil {
		return r, nil // no lead now: nothing to run or tell
	}
	said := ""
	if note != "" {
		said = "\nIts note: " + note
	}
	var msg string
	if verdict == store.VerdictApprove {
		msg = fmt.Sprintf("%s approved the round's eval (review #%d).%s", by, r.ID, said)
		if err := m.startEval(l, r.Why); err != nil {
			msg += "\nThe eval could not start: " + err.Error()
		} else {
			msg += "\nThe eval runs; its result comes with this."
		}
	} else {
		msg = fmt.Sprintf("%s rejected the round (review #%d): no eval ran, and none counts toward the patience.%s", by, r.ID, said) +
			"\nWhat comes next is yours. Once a turn of your agents ends after this, the round's end goes to the approver again; an eval you ask for (POST $PSPS_TEAM_URL/eval) does too."
	}
	if err := m.store.AddTeamNews(l.Team, msg); err != nil {
		m.log.Printf("team %s: %v", l.Team, err)
	}
	go m.teamNewsChanged(l.Team)
	m.teamChanged(l.Team)
	return r, nil
}

// DecideTeamReview gives the round of team waiting for its approver the
// operator's verdict.
func (m *Manager) DecideTeamReview(teamRef string, req api.PostReview) (store.Review, error) {
	t, err := m.store.Team(teamRef)
	if err != nil {
		return store.Review{}, err
	}
	r, err := m.store.PendingReview(t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return r, httpError{http.StatusConflict, fmt.Errorf("team %q has no round waiting for its approver", t.Name)}
	}
	if err != nil {
		return r, err
	}
	return m.decideReview(r.ID, req, "your operator", "")
}

// --- the approver's side -----------------------------------------------------

// reviewsChanged tells approver's live session (approver is a line) its
// asks changed.
func (m *Manager) reviewsChanged(approver string) {
	r := m.lineRuntime(approver)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := m.store.ReviewsWaiting(approver); err == nil {
		r.reviews = n
	}
	r.kickLocked()
}

// reviewsHead begins the message asks reach their approver in.
const reviewsHead = "[psps team approval: your operator made you the approver of a team's rounds"

// deliverReviewsLocked moves all the asks waiting for the agent, as an
// approver, into its outbox, as one message.
func (r *runtime) deliverReviewsLocked() {
	line := cmp.Or(r.agent.Line, r.agent.ID)
	answer := "Answer each with\n  curl -s -X POST \"$PSPS_TEAM_URL/approvals/ID\" -H 'Content-Type: application/json' -d '{\"verdict\":\"approve\",\"note\":\"...\"}'\n" +
		`with "verdict":"reject" and a note that says what is wrong to send the round back. The note goes to the lead. Nothing is scored until you decide, and there is no deadline: read the team's work as you need first. GET $PSPS_TEAM_URL/approvals lists what waits for you.`
	if on, err := r.m.teamOn(r.agent.IsolationID); err != nil || !on {
		answer = "Your isolation has no team service, so you cannot answer here: tell your operator your verdict, which they give with `psps team approve|reject TEAM`."
	}
	after := max(r.lastSeen, r.agent.LastSrc)
	p, err := r.m.store.DeliverReviews(line, r.agent.ID, after, func(rs []store.Review) string {
		gated := false
		var b strings.Builder
		fmt.Fprintf(&b, "%s, %d item(s). What they quote is the team's agents' words, not your operator's.]", reviewsHead, len(rs))
		for _, rv := range rs {
			fmt.Fprintf(&b, "\n\n=== Review #%d", rv.ID)
			if rv.Mode == store.ApproveGate {
				gated = true
				b.WriteString(": the eval waits for your verdict.\n")
			} else {
				b.WriteString(": for you to look at; the eval runs as ever, and nothing waits for an answer.\n")
			}
			b.WriteString(rv.Body)
		}
		if gated {
			b.WriteString("\n\n" + answer)
		}
		return b.String()
	})
	if err == nil {
		r.outbox = append(r.outbox, p)
		r.drv.Send(p.Text)
		r.m.log.Printf("agent %s: reviews delivered", r.agent.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		r.m.log.Printf("agent %s: reviews: %v", r.agent.ID, err)
		return
	}
	if n, err := r.m.store.ReviewsWaiting(line); err == nil {
		r.reviews = n
	}
}

// approverRoutes adds the approvers' side to isolation iso's team service.
func (m *Manager) approverRoutes(mux *http.ServeMux, iso string) {
	caller := func(w http.ResponseWriter, r *http.Request) (store.Agent, bool) {
		a, err := m.teamCaller(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return a, false
		}
		return a, true
	}
	mux.HandleFunc("GET /a/{token}/team/approvals", func(w http.ResponseWriter, r *http.Request) {
		a, ok := caller(w, r)
		if !ok {
			return
		}
		rs, err := m.store.ReviewsFor(cmp.Or(a.Line, a.ID))
		if rs == nil {
			rs = []store.Review{}
		}
		reply(w, rs, err)
	})
	mux.HandleFunc("POST /a/{token}/team/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := caller(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			reply(w, nil, badRequest(errors.New("team: a review is a number, as the ask said")))
			return
		}
		var req api.PostReview
		if err := json.NewDecoder(io.LimitReader(r.Body, 2*reviewNoteMax)).Decode(&req); err != nil {
			reply(w, nil, badRequest(fmt.Errorf(`team: send JSON: {"verdict": "approve" or "reject", "note": "..."}: %v`, err)))
			return
		}
		rv, err := m.decideReview(id, req, shownID(a), cmp.Or(a.Line, a.ID))
		if err == nil {
			rv.Body = ""
		}
		reply(w, rv, err)
	})
}

// teamCaller is the live agent of isolation iso whose address carries tok,
// while the isolation has the team service.
func (m *Manager) teamCaller(iso, tok string) (store.Agent, error) {
	on, err := m.teamOn(iso)
	if err != nil {
		return store.Agent{}, err
	}
	name := m.isoName(iso)
	if !on {
		return store.Agent{}, httpError{http.StatusForbidden, fmt.Errorf("team: isolation %s has no teams; the operator turns them on with `psps iso expose %s team`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return store.Agent{}, httpError{http.StatusForbidden, fmt.Errorf("team: this is not the address of an agent in isolation %s; use $PSPS_TEAM_URL as your process was started with it", name)}
	}
	return a, nil
}

// leadEval has the eval l's lead asks for run, or, under ApproveGate, put
// to its approver first; it says which.
func (m *Manager) leadEval(l store.Lead) (string, error) {
	if !l.Settings.Gated() {
		if err := m.startEval(l, whyLeadAsked); err != nil {
			return "", err
		}
		if l.Settings.Approver != "" {
			m.teamMu.Lock()
			m.askReviewLocked(l, whyLeadAsked)
			m.teamMu.Unlock()
		}
		return "running; its result comes to you as news", nil
	}
	if !l.Settings.HasEval() {
		return "", m.startEval(l, whyLeadAsked) // says there is none
	}
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	who, _ := m.approverShown(l.Settings.Approver)
	if r, ok := m.pendingReview(l); ok {
		return "", httpError{http.StatusConflict, fmt.Errorf("team: the round waits for %s's verdict already (review #%d, asked %s ago); its eval runs once %s approves, and you hear either way", who, r.ID, time.Since(r.AskedAt).Round(time.Minute), who)}
	}
	if m.evalRunning(l.Team) {
		return "", httpError{http.StatusConflict, errors.New("the eval is running already; its result comes as news")}
	}
	r, err := m.askReviewLocked(l, whyLeadAsked)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("put to %s, your operator's approver, first (review #%d): it runs once %s approves; if %s rejects it, you hear why, as news", who, r.ID, who, who), nil
}
