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
	"time"
	"unicode/utf8"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

// Mail between agents (internal/proxy/mailproxy). The daemon answers each
// isolation's mail port itself, since it keeps the inboxes; each agent's
// runtime reads its inbox one mail per turn whenever it is idle.

// shownID is what an agent is called: its own ID, or its key without one.
func shownID(a store.Agent) string {
	if a.Handle != "" {
		return a.Handle
	}
	return a.ID
}

// mailReach is whether isolation iso (by ID) takes mail, and which other
// isolations its agents may write to, by ID.
func (m *Manager) mailReach(iso string) (bool, []string, error) {
	if _, err := m.registry().Get(mailproxy.Name); err != nil {
		return false, nil, nil
	}
	x, err := m.store.Exposure(iso, mailproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	var s struct {
		To []string `json:"to"`
	}
	json.Unmarshal(x.Settings, &s)
	return true, s.To, nil
}

// mailEnv is an agent process's own address, while its isolation takes
// mail.
func (m *Manager) mailEnv(a store.Agent) []string {
	if on, _, err := m.mailReach(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: mail token: %v", a.ID, err)
		return nil
	}
	return []string{"GERO_MAIL_URL=" + mailproxy.URL(tok)}
}

// letter is mail as its recipient reads it, naming the sender as it is
// called now if it is still there.
func (m *Manager) letter(ml store.Mail) string {
	from := ml.FromID
	if a, err := m.store.Agent(ml.From); err == nil {
		from = shownID(a)
	}
	return mailproxy.Letter(ml.ID, from, m.isoName(ml.FromIsolation), ml.Body)
}

// inboxChanged tells a live agent its inbox changed.
func (m *Manager) inboxChanged(agent string) {
	m.mu.Lock()
	r := m.runtimes[agent]
	m.mu.Unlock()
	if r != nil {
		r.inboxChanged()
	}
}

func (m *Manager) mailBucket(iso string) *proxy.TokenBucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.mailBuckets[iso]
	if !ok {
		b = proxy.NewTokenBucket(mailproxy.PerMinute, mailproxy.Burst, nil)
		m.mailBuckets[iso] = b
	}
	return b
}

// --- what agents ask, through their isolation's tunnel ----------------------

// mailAgent is an agent as another agent's directory shows it.
type mailAgent struct {
	ID        string       `json:"id"`
	Name      string       `json:"name,omitempty"`
	Kind      driver.Kind  `json:"kind"`
	Isolation string       `json:"isolation"`
	Status    event.Status `json:"status,omitempty"`
	Inbox     int          `json:"inbox"`
}

type mailDirectory struct {
	You    mailAgent   `json:"you"`
	Agents []mailAgent `json:"agents"`
}

// tooSoon refuses mail for the sending budget, saying when to send again.
type tooSoon struct {
	httpError
	retry time.Duration
}

type mailReceipt struct {
	ID      int64  `json:"id"`
	To      string `json:"to"`
	Waiting int    `json:"waiting"`
	Note    string `json:"note"`
}

// mailSender is who is writing: the live agent of isolation iso whose
// address carries tok, and where that isolation's agents may write.
type mailSender struct {
	agent store.Agent
	reach []string
}

// mailHandler answers isolation iso's mail port. Nothing it logs carries a
// path: the path carries the sender's token.
func (m *Manager) mailHandler(iso string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{token}/agents", func(w http.ResponseWriter, r *http.Request) {
		from, err := m.mailSender(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.mailDirectory(from)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /a/{token}/send", func(w http.ResponseWriter, r *http.Request) {
		from, err := m.mailSender(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, mailproxy.MaxBody+1))
		switch {
		case err != nil:
			reply(w, nil, badRequest(errors.New("mail: the body could not be read")))
			return
		case len(body) > mailproxy.MaxBody:
			reply(w, nil, httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("mail: a mail is at most %d bytes; put the rest where the other agent can read it and say where", mailproxy.MaxBody)})
			return
		case !utf8.Valid(body):
			reply(w, nil, badRequest(errors.New("mail: a mail is text, in UTF-8")))
			return
		case strings.TrimSpace(string(body)) == "":
			reply(w, nil, badRequest(errors.New("mail: the body is the mail, and it is empty")))
			return
		}
		v, err := m.postMail(iso, from, r.URL.Query().Get("to"), string(body))
		var soon tooSoon
		switch {
		case errors.As(err, &soon):
			w.Header().Set("Retry-After", strconv.Itoa(int(soon.retry.Seconds())+1))
			err = soon.httpError
		case err == nil:
			// reply sets the type; the status follows it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
		}
		reply(w, v, err)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, httpError{http.StatusNotFound, errors.New(`mail: there is GET "$GERO_MAIL_URL/agents" and POST "$GERO_MAIL_URL/send?to=AGENT" (the gero-mail skill)`)})
	})
	return mux
}

func (m *Manager) mailSender(iso, tok string) (mailSender, error) {
	on, reach, err := m.mailReach(iso)
	if err != nil {
		return mailSender{}, err
	}
	name := m.isoName(iso)
	if !on {
		return mailSender{}, httpError{http.StatusForbidden, fmt.Errorf("mail: isolation %s takes no mail; the operator turns it on with `gero iso expose %s mail`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return mailSender{}, httpError{http.StatusForbidden, fmt.Errorf("mail: this is not the address of an agent in isolation %s; use $GERO_MAIL_URL as your process was started with it", name)}
	}
	return mailSender{agent: a, reach: reach}, nil
}

// mayWrite is whether from may write to the agents of isolation iso: its
// own, or one its isolation's reach names, if iso takes mail.
func (m *Manager) mayWrite(from mailSender, iso string) (bool, error) {
	if iso != from.agent.IsolationID && !slices.Contains(from.reach, iso) {
		return false, nil
	}
	on, _, err := m.mailReach(iso)
	return on, err
}

func (m *Manager) mailDirectory(from mailSender) (mailDirectory, error) {
	all, err := m.store.Agents()
	if err != nil {
		return mailDirectory{}, err
	}
	counts, err := m.store.InboxCounts()
	if err != nil {
		return mailDirectory{}, err
	}
	dir := mailDirectory{Agents: []mailAgent{}}
	may := map[string]bool{}
	for _, a := range all {
		if a.ArchivedAt != nil {
			continue
		}
		ok, seen := may[a.IsolationID]
		if !seen {
			if ok, err = m.mayWrite(from, a.IsolationID); err != nil {
				return mailDirectory{}, err
			}
			may[a.IsolationID] = ok
		}
		if !ok {
			continue
		}
		v := m.view(a, counts[a.ID])
		ma := mailAgent{ID: shownID(v.Agent), Name: v.Name, Kind: v.Kind, Isolation: v.Isolation, Status: v.State.Status, Inbox: v.Inbox}
		if a.ID == from.agent.ID {
			dir.You = ma
			continue
		}
		dir.Agents = append(dir.Agents, ma)
	}
	return dir, nil
}

// postMail puts body in the inbox of the agent to names, exactly by its own
// ID or its key: a prefix could be someone else's tomorrow.
func (m *Manager) postMail(iso string, from mailSender, to, body string) (mailReceipt, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return mailReceipt{}, badRequest(errors.New(`mail: say who it is for: "$GERO_MAIL_URL/send?to=AGENT"; "$GERO_MAIL_URL/agents" lists who you can write to`))
	}
	rec, err := m.store.AgentByHandle(to)
	if errors.Is(err, store.ErrNotFound) {
		rec, err = m.store.Agent(to)
	}
	if errors.Is(err, store.ErrNotFound) {
		return mailReceipt{}, httpError{http.StatusNotFound, fmt.Errorf(`mail: there is no agent %q; "$GERO_MAIL_URL/agents" lists who you can write to`, to)}
	}
	if err != nil {
		return mailReceipt{}, err
	}
	switch {
	case rec.ID == from.agent.ID:
		return mailReceipt{}, badRequest(errors.New("mail: that is your own address"))
	case rec.ArchivedAt != nil:
		return mailReceipt{}, httpError{http.StatusConflict, fmt.Errorf("mail: agent %s is archived and reads no mail", to)}
	}
	recIso := m.isoName(rec.IsolationID)
	if rec.IsolationID != from.agent.IsolationID && !slices.Contains(from.reach, rec.IsolationID) {
		ours := m.isoName(iso)
		flags := ""
		for _, n := range append(m.isoNames(from.reach), recIso) {
			flags += " --to " + n
		}
		return mailReceipt{}, httpError{http.StatusForbidden, fmt.Errorf("mail: agent %s is in isolation %s, which isolation %s's agents may not write to; the operator decides that (`gero iso expose %s mail%s`)",
			to, recIso, ours, ours, flags)}
	}
	if on, _, err := m.mailReach(rec.IsolationID); err != nil {
		return mailReceipt{}, err
	} else if !on {
		return mailReceipt{}, httpError{http.StatusForbidden, fmt.Errorf("mail: agent %s's isolation %s takes no mail; the operator turns it on with `gero iso expose %s mail`", to, recIso, recIso)}
	}
	// Spent only on mail that would be delivered: a refusal costs none.
	if ok, retry := m.mailBucket(iso).Allow(); !ok {
		return mailReceipt{}, tooSoon{httpError{http.StatusTooManyRequests, fmt.Errorf("mail: isolation %s's agents send at most %d mails a minute; wait and send again",
			m.isoName(iso), mailproxy.PerMinute)}, retry}
	}
	ml, waiting, err := m.store.AddMail(store.Mail{To: rec.ID, From: from.agent.ID, FromID: shownID(from.agent), FromIsolation: iso, Body: body}, mailproxy.MaxWaiting)
	if errors.Is(err, store.ErrInboxFull) {
		return mailReceipt{}, httpError{http.StatusTooManyRequests, fmt.Errorf("mail: agent %s's inbox is full (%d mails wait); let it read them first", to, waiting)}
	}
	if err != nil {
		return mailReceipt{}, err
	}
	m.log.Printf("isolation %s: mail #%d from %s to %s (%d bytes)", m.isoName(iso), ml.ID, from.agent.ID, rec.ID, len(body))
	m.inboxChanged(rec.ID)
	return mailReceipt{ID: ml.ID, To: shownID(rec), Waiting: waiting,
		Note: "it is read when " + shownID(rec) + " is next idle, one mail per turn; any answer comes as a mail to you"}, nil
}

// --- what the operator asks ------------------------------------------------

// mailHistory is how much of an agent's mail Mail shows.
const mailHistory = 500

// mailViews shows mail as the operator reads it, of being the agent whose
// mail it is.
func (m *Manager) mailViews(mails []store.Mail, of string) []api.Mail {
	names := map[string]string{}
	called := func(key, fallback string) string {
		if n, ok := names[key]; ok {
			return n
		}
		n := fallback
		if a, err := m.store.Agent(key); err == nil {
			n = shownID(a)
		}
		names[key] = n
		return n
	}
	out := make([]api.Mail, 0, len(mails))
	for _, ml := range mails {
		v := api.Mail{ID: ml.ID, From: called(ml.From, ml.FromID), FromIsolation: m.isoName(ml.FromIsolation),
			To: called(ml.To, ml.To), Sent: ml.From == of, State: api.MailWaiting, Text: ml.Body, CreatedAt: ml.CreatedAt,
			DeliveredAt: ml.DeliveredAt, DiscardedAt: ml.DiscardedAt}
		if a, err := m.store.Agent(ml.To); err == nil {
			v.ToIsolation = m.isoName(a.IsolationID)
		}
		switch {
		case ml.DeliveredAt != nil:
			v.State = api.MailDelivered
		case ml.DiscardedAt != nil:
			v.State = api.MailDiscarded
		}
		out = append(out, v)
	}
	return out
}

// Inbox is the mail waiting for an agent, in the order it will read it.
func (m *Manager) Inbox(idOrPrefix string) ([]api.Mail, error) {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return nil, err
	}
	in, err := m.store.Inbox(a.ID)
	if err != nil {
		return nil, err
	}
	return m.mailViews(in, a.ID), nil
}

// Mail is an agent's mail history, newest first: what it was sent, waiting
// or not, and what it wrote.
func (m *Manager) Mail(idOrPrefix string) ([]api.Mail, error) {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return nil, err
	}
	all, err := m.store.MailOf(a.ID, mailHistory)
	if err != nil {
		return nil, err
	}
	return m.mailViews(all, a.ID), nil
}

// DiscardMail takes one mail out of an agent's inbox unread. It stays in the
// history, as discarded.
func (m *Manager) DiscardMail(idOrPrefix string, id int64) error {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return err
	}
	if err := m.store.DiscardMail(a.ID, id); err != nil {
		return err
	}
	m.inboxChanged(a.ID)
	return nil
}

// ClearInbox takes all of an agent's mail out of its inbox unread.
func (m *Manager) ClearInbox(idOrPrefix string) (int, error) {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return 0, err
	}
	n, err := m.store.ClearInbox(a.ID)
	if err != nil {
		return 0, err
	}
	m.inboxChanged(a.ID)
	return n, nil
}

// --- isolations in settings, by ID ------------------------------------------

// settingsOut shows a service's stored settings: the isolations its
// Isolations fields keep by ID, by name. An isolation deleted since is
// left out.
func (m *Manager) settingsOut(svc proxy.Service, raw json.RawMessage) json.RawMessage {
	fields := isolationFields(svc)
	if len(fields) == 0 || len(raw) == 0 {
		return raw
	}
	var s map[string]any
	if json.Unmarshal(raw, &s) != nil {
		return raw
	}
	for _, k := range fields {
		if _, ok := s[k]; !ok {
			continue
		}
		names := []string{}
		for _, id := range proxy.Strings(s, k) {
			if iso, err := m.store.Isolation(id); err == nil {
				names = append(names, iso.Name)
			}
		}
		s[k] = names
	}
	b, _ := json.Marshal(s)
	return b
}

// settingsIn is settingsOut's reverse, for settings as typed: every name
// must be an isolation's.
func (m *Manager) settingsIn(svc proxy.Service, s map[string]any) error {
	for _, k := range isolationFields(svc) {
		if _, ok := s[k]; !ok {
			continue
		}
		ids := []string{}
		for _, n := range proxy.Strings(s, k) {
			iso, err := m.store.IsolationNamed(n)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("there is no isolation named %q", n)
			}
			if err != nil {
				return err
			}
			ids = append(ids, iso.ID)
		}
		s[k] = ids
	}
	return nil
}

func isolationFields(svc proxy.Service) []string {
	var out []string
	for _, f := range svc.Settings() {
		if f.Isolations {
			out = append(out, f.Key)
		}
	}
	return out
}
