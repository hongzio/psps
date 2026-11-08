package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/store"
)

// Watches (store.Watch): an agent that writes to another through the
// control service hears from psps what came of it, so it need not poll,
// and cannot forget to look. psps judges the target's turns as they end,
// from the turn that reads the message on:
//
//   - one that waits for the operator's approval: said once per approval;
//   - its turn ends in error or interrupted: said, and the watch is done;
//   - it ends idle, with nothing in the background and nothing more on its
//     way to it: said with its last answers, and the watch is done (under
//     WatchTurn, whatever runs in the background);
//   - it ends idle with tasks in the background: said once, the first
//     time; the watch waits for a turn that ends with none;
//   - its process ends while tasks run in the background, out of a turn:
//     the work went with it, and the watch is stalled; nothing is said, as
//     psps resumes nothing: the operator does, and the turn after is
//     judged as any.
//
// What is said waits as receipts until the sender is free, then goes to
// it in one message, as a lead's news does. The sender's mail need not be
// on: receipts are psps's, of what the sender did itself.

const (
	// maxWatches is how many watches an agent has open at once.
	maxWatches = 20
	// maxWatchMinutes caps how long a sender may ask to wait before it
	// hears that its target is still at work.
	maxWatchMinutes = 7 * 24 * 60
	// excerptLen is how much of a message a receipt quotes to say which.
	excerptLen = 80
	// earlierAnswer is how much of each earlier answer a receipt carries,
	// earlierAnswers how many of them.
	earlierAnswer  = 1500
	earlierAnswers = 3
)

// watchSpec is how a sender asks to hear of what it sends: mode "" for
// not at all.
type watchSpec struct {
	mode    string
	minutes int
}

// watchQuery reads ?watch= and ?minutes= of a request that sends or
// makes: settle unless it says otherwise.
func watchQuery(q url.Values) (watchSpec, error) {
	s := watchSpec{mode: strings.TrimSpace(q.Get("watch"))}
	switch s.mode {
	case "", store.WatchSettle:
		s.mode = store.WatchSettle
	case store.WatchTurn:
	case "none":
		s.mode = ""
	default:
		return watchSpec{}, badRequest(fmt.Errorf(`control: watch is "settle" (the default), "turn" or "none", not %q`, s.mode))
	}
	if v := strings.TrimSpace(q.Get("minutes")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxWatchMinutes {
			return watchSpec{}, badRequest(fmt.Errorf("control: minutes is a whole number from 1 to %d", maxWatchMinutes))
		}
		if s.mode == "" {
			return watchSpec{}, badRequest(errors.New("control: minutes says when to hear that it is still at work, and watch=none hears nothing"))
		}
		s.minutes = n
	}
	return s, nil
}

// watch is the watch s asks for, of sender on body; nil for none.
func (s watchSpec) watch(sender store.Agent, body string) *store.Watch {
	if s.mode == "" {
		return nil
	}
	return &store.Watch{Sender: sender.ID, Mode: s.mode, Minutes: s.minutes, Excerpt: excerpt(body)}
}

// watched is what came of asking for a watch with a message: its ID, or
// why there is none although one was asked for.
type watched struct {
	id  int64
	err error
}

// excerpt is the start of text, on one line.
func excerpt(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(s) <= excerptLen {
		return s
	}
	return string([]rune(s)[:excerptLen]) + "…"
}

// watchNote is what a sender is told of a watch it asked for, as it sends.
func watchNote(target store.Agent, w watched, spec watchSpec) string {
	switch {
	case spec.mode == "":
		return "you asked to hear nothing of it"
	case w.err != nil:
		return fmt.Sprintf("it went, but psps will not tell you what came of it: you have %d watches open, as many as you may (GET $PSPS_CONTROL_URL/watches; DELETE one that no longer matters)", maxWatches)
	}
	when := "when " + shownID(target) + " has finished with it (its turn ended and it runs nothing in the background)"
	if spec.mode == store.WatchTurn {
		when = "when the turn of " + shownID(target) + " that reads it ends"
	}
	return "psps tells you, as a message of its own once you are idle, " + when + ", or stops, or waits for approval, however long that takes. Do not poll for it: end your turn if you have nothing else to do."
}

// --- the target's side ------------------------------------------------------

// sendWatchedLocked is sendLocked, with a watch of the sender's on the
// message if w is not nil, stored with it at once.
func (r *runtime) sendWatchedLocked(text string, w *store.Watch) (watched, error) {
	// Before catch-up lastSeen trails the log; LastSrc is the furthest this
	// daemon's store has read, which keeps an old identical message in the
	// replay from being mistaken for this one.
	after := max(r.lastSeen, r.agent.LastSrc)
	var id int64
	var wd watched
	var err error
	if w == nil {
		id, err = r.m.store.AddPending(r.agent.ID, text, after)
	} else {
		id, wd.id, wd.err, err = r.m.store.AddPendingWatched(r.agent.ID, text, after, *w, maxWatches)
	}
	if err != nil {
		return watched{}, err
	}
	r.outbox = append(r.outbox, store.Pending{ID: id, Text: text, After: after})
	r.noteBusyLocked()
	r.nameLocked(text)
	if r.caughtUp {
		r.drv.Send(text)
		r.kickLocked()
	}
	if wd.id != 0 {
		r.m.log.Printf("agent %s: watch #%d of %s on its message", r.agent.ID, wd.id, w.Sender)
		r.m.pokeChanges()
	}
	return wd, nil
}

// sendWatched is sendWatchedLocked, taking the lock.
func (r *runtime) sendWatched(text string, w *store.Watch) (watched, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendWatchedLocked(text, w)
}

// watchNow starts a watch of w's sender on the agent with no message:
// from the turn it is in, or else its next.
func (r *runtime) watchNow(w store.Watch) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.drv.State(); st.Status == event.StatusWorking || st.Status == event.StatusBlocked {
		w.Reading, w.ReadSeq = true, max(r.lastSeen, r.agent.LastSrc)
	}
	w.Target = r.agent.ID
	id, err := r.m.store.AddWatch(w, maxWatches)
	if err == nil {
		r.m.log.Printf("agent %s: watch #%d of %s", r.agent.ID, id, w.Sender)
		r.m.pokeChanges()
	}
	return id, err
}

// watchReadLocked follows the agent reading messages at log record seq:
// read are the outbox messages it read, 0 for one that was not in it. A
// watch on one of them is read now; one read before learns that more than
// its message reached the agent since.
func (r *runtime) watchReadLocked(seq int64, read []int64) {
	ws, err := r.m.store.WatchesOn(r.agent.ID)
	if err != nil || len(ws) == 0 {
		return
	}
	for _, w := range ws {
		was := w
		for _, id := range read {
			switch {
			case !w.Reading && w.Pending != 0 && w.Pending == id:
				w.Reading, w.ReadSeq, w.Pending = true, seq, 0
			case w.Reading && seq > w.ReadSeq && w.Excerpt != "":
				w.Others = true
			}
		}
		if w.Reading != was.Reading || w.Others != was.Others {
			if err := r.m.store.PutWatch(w); err != nil {
				r.m.log.Printf("agent %s: watch #%d: %v", r.agent.ID, w.ID, err)
			}
		}
	}
}

// watchTurnStartLocked starts judging the watches on no message that wait
// for the agent's next turn: it began at log record seq.
func (r *runtime) watchTurnStartLocked(seq int64) {
	ws, err := r.m.store.WatchesOn(r.agent.ID)
	if err != nil {
		return
	}
	for _, w := range ws {
		if !w.Reading && w.Pending == 0 {
			w.Reading, w.ReadSeq = true, seq
			if err := r.m.store.PutWatch(w); err != nil {
				r.m.log.Printf("agent %s: watch #%d: %v", r.agent.ID, w.ID, err)
			}
		}
	}
}

// watchJudgeLocked judges the watches on the agent as something happened
// live: a turn ended (ended), it waits for an approval (waited), or its
// process ended out of a turn with lost tasks in the background.
func (r *runtime) watchJudgeLocked(ended event.Status, cut bool, waited string, lost int, at time.Time) {
	// A turn stopped ends idle; it did not finish.
	if ended == event.StatusIdle && cut {
		ended = event.StatusInterrupted
	}
	ws, err := r.m.store.WatchesOn(r.agent.ID)
	if err != nil || len(ws) == 0 {
		return
	}
	st := r.drv.State()
	more := len(r.outbox) > 0 || st.Queued > 0
	for _, w := range ws {
		if !w.Reading {
			continue
		}
		if waited != "" && !w.Noting("approval:"+waited) {
			w.Noted = append(w.Noted, "approval:"+waited)
			body := fmt.Sprintf("%s waits for your operator to approve: %s. It goes on once they decide; you cannot decide for it.", r.receiptWho(w), waited)
			if !r.receiptLocked(w, body, false) {
				continue
			}
		}
		switch judge(w, ended, lost, st.Background, more) {
		case judgedStopped:
			w.Stalled = 0
			how := "was interrupted (stopped, or its process ended)"
			if ended == event.StatusError {
				how = "ended in error"
				if st.Error != "" {
					how += " (" + st.Error + ")"
				}
			}
			body := fmt.Sprintf("%s stopped: its turn %s, %s.%s It does not go on by itself; your operator resumes it. Tell them; do not resume it yourself unless they asked you to.",
				r.receiptWho(w), how, since(w, at), r.othersNote(w))
			r.receiptLocked(w, body+r.answersText(w, true), true)
		case judgedFinished:
			w.Stalled = 0
			bg := " It runs nothing in the background."
			if st.Background > 0 {
				bg = fmt.Sprintf(" It still runs %s in the background (%s), as it was when its turn ended.", tasks(st.Background), taskNames(st.Tasks))
			}
			body := fmt.Sprintf("%s has finished with it: its turn ended, %s.%s It has used %s tokens.%s", r.receiptWho(w), since(w, at), bg, r.spentText(st), r.othersNote(w))
			r.receiptLocked(w, body+r.answersText(w, true), true)
		case judgedBackground:
			w.Stalled = 0
			w.Noted = append(w.Noted, "background")
			body := fmt.Sprintf("%s ended its turn, %s, but runs %s in the background (%s). psps tells you again when it has finished with it. If one of them never ends, look at it yourself, or drop the watch (DELETE $PSPS_CONTROL_URL/watches/%d).%s",
				r.receiptWho(w), since(w, at), tasks(st.Background), taskNames(st.Tasks), w.ID, r.othersNote(w))
			r.receiptLocked(w, body+r.answersText(w, true), false)
		case judgedWaiting:
			w.Stalled = 0
			if r.answer != "" {
				w.Answers = append(w.Answers, tail(r.answer, earlierAnswer))
				if len(w.Answers) > earlierAnswers {
					w.Answers = w.Answers[len(w.Answers)-earlierAnswers:]
				}
			}
			r.putWatchLocked(w)
		case judgedStalled:
			w.Stalled = lost
			r.putWatchLocked(w)
			r.m.log.Printf("agent %s: watch #%d stalled: its process ended with %d task(s) in the background", r.agent.ID, w.ID, lost)
			r.m.pokeChanges()
		}
	}
}

// judged is what a watch comes to as its target's turn ends, or its
// process does.
type judged int

const (
	judgedNothing judged = iota
	// judgedStopped: its turn ended in error or was interrupted; said, and
	// the watch is done.
	judgedStopped
	// judgedFinished: it has finished with it; said, and done.
	judgedFinished
	// judgedBackground: its turn ended with tasks in the background, the
	// first time; said, and the watch waits on.
	judgedBackground
	// judgedWaiting: not finished, and nothing to say; it waits on.
	judgedWaiting
	// judgedStalled: its process ended with tasks in the background, out
	// of a turn; nothing is said, and it waits for the operator.
	judgedStalled
)

// judge is what watch w comes to as its target's turn ends (ended), or
// its process ends out of a turn taking lost tasks in the background with
// it: background is how many it runs now, more whether more messages are
// on their way to it.
func judge(w store.Watch, ended event.Status, lost, background int, more bool) judged {
	switch {
	case ended == event.StatusError || ended == event.StatusInterrupted:
		return judgedStopped
	case ended == event.StatusIdle:
		switch {
		case w.Mode == store.WatchTurn || (background == 0 && !more):
			return judgedFinished
		case background > 0 && !w.Noting("background"):
			return judgedBackground
		}
		return judgedWaiting
	case ended == "" && lost > 0:
		return judgedStalled
	}
	return judgedNothing
}

// watchDeadlineLocked tells w's sender that the agent is still at work
// after the minutes it asked to wait.
func (r *runtime) watchDeadlineLocked(w store.Watch) {
	w.Noted = append(w.Noted, "deadline")
	st := r.drv.State()
	what := "is still at work on it"
	switch {
	case !w.Reading:
		what = "has not read it yet (" + string(st.Status) + ")"
	case w.Stalled > 0:
		what = fmt.Sprintf("has not finished with it: its process ended while %s ran in the background, and only your operator resumes it", tasks(w.Stalled))
	case st.Status == event.StatusIdle && st.Background > 0:
		what = fmt.Sprintf("is still at work on it: its turn ended, and it runs %s in the background (%s)", tasks(st.Background), taskNames(st.Tasks))
	case st.Status != event.StatusWorking && st.Status != event.StatusBlocked:
		what = "has not finished with it, and is " + string(st.Status)
	}
	body := fmt.Sprintf("%s %s after %d minutes, as you asked to hear. psps still tells you when it has finished.", r.receiptWho(w), what, w.Minutes)
	r.receiptLocked(w, body, false)
}

func (r *runtime) putWatchLocked(w store.Watch) {
	if err := r.m.store.PutWatch(w); err != nil {
		r.m.log.Printf("agent %s: watch #%d: %v", r.agent.ID, w.ID, err)
	}
}

// receiptLocked writes body for w's sender, and keeps w as it is; done
// ends w. False if w had ended already.
func (r *runtime) receiptLocked(w store.Watch, body string, done bool) bool {
	ok, err := r.m.store.Receipt(w, body, done)
	if err != nil {
		r.m.log.Printf("agent %s: watch #%d: %v", r.agent.ID, w.ID, err)
		return false
	}
	if !ok {
		return false
	}
	if done {
		r.m.log.Printf("agent %s: watch #%d of %s done", r.agent.ID, w.ID, w.Sender)
	}
	go r.m.receiptsChanged(w.Sender)
	r.m.pokeChanges()
	return true
}

// receiptWho says who w waits on and what about.
func (r *runtime) receiptWho(w store.Watch) string {
	return receiptWho(r.agent, w)
}

func receiptWho(a store.Agent, w store.Watch) string {
	what := fmt.Sprintf("your message #%d (%q)", w.ID, w.Excerpt)
	if w.Excerpt == "" {
		what = fmt.Sprintf("what you watch (#%d)", w.ID)
	}
	return fmt.Sprintf("%s (%q, %s), on %s,", shownID(a), a.Name, a.Kind, what)
}

func (r *runtime) othersNote(w store.Watch) string {
	if !w.Others {
		return ""
	}
	return " Others wrote to it too since it read yours (your operator, mail or a cron), so what it said may be about that."
}

func (r *runtime) spentText(st driver.State) string {
	if st.Usage == nil {
		return "no"
	}
	return tokens(st.Usage.Spent.New())
}

// answersText is what the agent said: its last answer, and, if earlier,
// the ends of its turns before that no receipt carried.
func (r *runtime) answersText(w store.Watch, earlier bool) string {
	var b strings.Builder
	if r.answer != "" {
		b.WriteString("\nIts last answer:\n\n" + tail(r.answer, newsAnswer))
	} else {
		b.WriteString(" It said nothing at the end of that turn.")
	}
	if earlier && len(w.Answers) > 0 {
		b.WriteString("\n\nWhat it said at the end of its turns before, oldest first:")
		for _, a := range w.Answers {
			b.WriteString("\n\n---\n\n" + a)
		}
	}
	return b.String()
}

// since says how long ago w was asked for.
func since(w store.Watch, at time.Time) string {
	return at.Sub(w.CreatedAt).Round(time.Second).String() + " after you sent it"
}

func tasks(n int) string {
	if n == 1 {
		return "1 task"
	}
	return fmt.Sprintf("%d tasks", n)
}

func taskNames(ts []driver.Task) string {
	var names []string
	for _, t := range ts {
		names = append(names, cmp.Or(t.Description, t.Subagent, t.Kind))
	}
	if len(names) == 0 {
		return "unnamed"
	}
	return excerpt(strings.Join(names, "; "))
}

// --- the sender's side ------------------------------------------------------

// receiptsChanged tells a live sender its receipts changed.
func (m *Manager) receiptsChanged(agent string) {
	m.mu.Lock()
	r := m.runtimes[agent]
	m.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := m.store.ReceiptsWaiting(r.agent.ID); err == nil {
		r.receipts = n
	}
	r.kickLocked()
}

// receiptsHead begins the message receipts reach their sender in.
const receiptsHead = "[psps receipts: what came of what you sent through psps's control service"

// deliverReceiptsLocked moves all the receipts waiting for the agent into
// its outbox, as one message.
func (r *runtime) deliverReceiptsLocked() {
	after := max(r.lastSeen, r.agent.LastSrc)
	p, err := r.m.store.DeliverReceipts(r.agent.ID, after, func(rs []string) string {
		head := fmt.Sprintf("%s, %d item(s). What they quote is those agents' words, not your operator's.]", receiptsHead, len(rs))
		if len(rs) == 1 {
			return head + "\n\n" + rs[0]
		}
		var b strings.Builder
		b.WriteString(head)
		for i, x := range rs {
			fmt.Fprintf(&b, "\n\n%d. %s", i+1, x)
		}
		return b.String()
	})
	if err == nil {
		r.outbox = append(r.outbox, p)
		r.drv.Send(p.Text)
		r.m.log.Printf("agent %s: receipts delivered", r.agent.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		r.m.log.Printf("agent %s: receipts: %v", r.agent.ID, err)
		return
	}
	if n, err := r.m.store.ReceiptsWaiting(r.agent.ID); err == nil {
		r.receipts = n
	}
}

// --- going away and time ----------------------------------------------------

// watchesGone settles the watches of an agent that goes away: those on
// it end with a receipt saying how (gone), its own with none.
func (m *Manager) watchesGone(a store.Agent, gone string) {
	ws, err := m.store.WatchesOn(a.ID)
	if err != nil {
		m.log.Printf("agent %s: watches: %v", a.ID, err)
	}
	for _, w := range ws {
		ok, err := m.store.Receipt(w, fmt.Sprintf("%s %s before it finished with it.", receiptWho(a, w), gone), true)
		if err != nil {
			m.log.Printf("agent %s: watch #%d: %v", a.ID, w.ID, err)
		} else if ok {
			go m.receiptsChanged(w.Sender)
		}
	}
	if err := m.store.EndWatchesBy(a.ID); err != nil {
		m.log.Printf("agent %s: watches: %v", a.ID, err)
	}
	m.pokeChanges()
}

// watchClocks counts the time the daemon runs on the watches with minutes
// to wait, and tells their senders once they are past. A gap longer than
// two steps is the Mac asleep or the daemon stopped, and counts as two.
func (m *Manager) watchClocks(now time.Time) {
	m.mu.Lock()
	last := m.watchTick
	m.watchTick = now
	m.mu.Unlock()
	if last.IsZero() {
		return
	}
	step := min(now.Sub(last), 2*teamStep)
	ws, err := m.store.OpenWatches()
	if err != nil {
		return
	}
	for _, w := range ws {
		if w.Minutes == 0 || w.Noting("deadline") {
			continue
		}
		w.Elapsed += int64(step / time.Second)
		m.mu.Lock()
		r := m.runtimes[w.Target]
		m.mu.Unlock()
		if r == nil {
			continue
		}
		r.mu.Lock()
		// What the runtime wrote since this was read stays.
		if cur, err := m.store.Watch(w.ID); err == nil && cur.DoneAt == nil {
			cur.Elapsed = w.Elapsed
			if cur.Elapsed >= int64(cur.Minutes)*60 {
				r.watchDeadlineLocked(cur)
			} else {
				r.putWatchLocked(cur)
			}
		}
		r.mu.Unlock()
	}
}

// --- what is shown ----------------------------------------------------------

// watchView is w as the operator and its sender see it.
func (m *Manager) watchView(w store.Watch, agents map[string]store.Agent) api.Watch {
	v := api.Watch{Watch: w, State: "waiting"}
	if s, ok := agents[w.Sender]; ok {
		v.SenderID = shownID(s)
	}
	if t, ok := agents[w.Target]; ok {
		v.TargetID, v.TargetName, v.TargetIsolation = shownID(t), t.Name, m.isoName(t.IsolationID)
	}
	m.mu.Lock()
	r := m.runtimes[w.Target]
	m.mu.Unlock()
	if r != nil {
		r.mu.Lock()
		st := r.drv.State()
		v.TargetStatus, v.Background, v.Tasks = st.Status, st.Background, st.Tasks
		if r.answer != "" {
			v.LastAnswer = tail(r.answer, newsAnswer)
		}
		if !r.seenAt.IsZero() {
			t := r.seenAt
			v.LastActivity = &t
		}
		r.mu.Unlock()
	}
	switch {
	case w.Stalled > 0:
		v.State = "stalled"
	case w.Reading && v.TargetStatus == event.StatusIdle && v.Background > 0:
		v.State = "background"
	case w.Reading:
		v.State = "reading"
	}
	return v
}

// watchViews are ws as watchView has them.
func (m *Manager) watchViews(ws []store.Watch) ([]api.Watch, error) {
	all, err := m.store.Agents()
	if err != nil {
		return nil, err
	}
	agents := map[string]store.Agent{}
	for _, a := range all {
		agents[a.ID] = a
	}
	out := []api.Watch{}
	for _, w := range ws {
		out = append(out, m.watchView(w, agents))
	}
	return out, nil
}

// AgentWatches are the open watches of an agent's, and on it.
func (m *Manager) AgentWatches(idOrPrefix string) (api.AgentWatches, error) {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return api.AgentWatches{}, err
	}
	by, err := m.store.WatchesBy(a.ID)
	if err != nil {
		return api.AgentWatches{}, err
	}
	on, err := m.store.WatchesOn(a.ID)
	if err != nil {
		return api.AgentWatches{}, err
	}
	var v api.AgentWatches
	if v.Sent, err = m.watchViews(by); err != nil {
		return v, err
	}
	v.Received, err = m.watchViews(on)
	return v, err
}

// DropWatch ends an open watch with no receipt.
func (m *Manager) DropWatch(id int64) error {
	ok, err := m.store.EndWatch(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("open watch #%d: %w", id, store.ErrNotFound)
	}
	m.log.Printf("watch #%d dropped", id)
	m.pokeChanges()
	return nil
}

// withWatches gives each agent what it shows of watches: how many it has
// open, those stalled among them, and who waits on it.
func (m *Manager) withWatches(as []api.Agent) error {
	ws, err := m.store.OpenWatches()
	if err != nil || len(ws) == 0 {
		return err
	}
	at := map[string]int{}
	for i, a := range as {
		at[a.ID] = i
	}
	var all map[string]store.Agent
	senderID := func(id string) string {
		if i, ok := at[id]; ok {
			return shownID(as[i].Agent)
		}
		if all == nil {
			all = map[string]store.Agent{}
			if list, err := m.store.Agents(); err == nil {
				for _, a := range list {
					all[a.ID] = a
				}
			}
		}
		if a, ok := all[id]; ok {
			return shownID(a)
		}
		return id
	}
	for _, w := range ws {
		if i, ok := at[w.Sender]; ok {
			as[i].Watching++
			if w.Stalled > 0 {
				as[i].WatchStalled++
			}
		}
		if i, ok := at[w.Target]; ok {
			if s := senderID(w.Sender); !slices.Contains(as[i].WatchedBy, s) {
				as[i].WatchedBy = append(as[i].WatchedBy, s)
			}
			if w.Stalled > 0 {
				as[i].WatchStalled++
			}
		}
	}
	return nil
}
