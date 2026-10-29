package driver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/shimproto"
)

// startedAs is a started record for the command the driver asked for, as
// the shim logs it.
func (t *tape) startedAs(argv []string) shimproto.Record {
	return t.add(shimproto.DirSys, "", &shimproto.Sys{Kind: shimproto.SysStarted, PID: 42, Argv: argv})
}

// write feeds what the driver writes now back as the log's "in" records and
// returns the last one.
func write(d Driver, tp *tape) (last shimproto.Record, evs []event.Event) {
	for _, l := range d.Next() {
		last = tp.in(string(l))
		evs = append(evs, d.Observe(last)...)
	}
	return last, evs
}

func rewinds(evs []event.Event) []event.Rewind {
	var out []event.Rewind
	for _, e := range evs {
		if e.Kind == event.KindRewind {
			out = append(out, *e.Rewind)
		}
	}
	return out
}

// claudeTurn runs one turn whose single printed entry is uuid, and returns
// the record that carried the message.
func claudeTurn(t *testing.T, d *claude, tp *tape, text, uuid string) shimproto.Record {
	t.Helper()
	d.Send(text)
	msg, _ := write(d, tp)
	if !strings.Contains(msg.Line, text) {
		t.Fatalf("wrote %q, want %q", msg.Line, text)
	}
	d.Observe(tp.out(`{"type":"system","subtype":"init","session_id":"` + d.sessionID + `"}`))
	d.Observe(tp.out(`{"type":"assistant","uuid":"` + uuid + `","parent_tool_use_id":null,"message":{"id":"m-` + uuid + `","content":[{"type":"text","text":"ok"}]}}`))
	d.Observe(tp.out(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`))
	return msg
}

func TestClaudeRewindResumesTheSessionAtTheEntryBefore(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "s-1", Options{})
	d.Observe(tp.startedAs(d.Command()))
	one := claudeTurn(t, d, tp, "one", "u-1")
	// A subagent's entries are in a transcript of their own.
	d.Observe(tp.out(`{"type":"assistant","uuid":"sub-1","parent_tool_use_id":"tu-9","message":{"id":"m-sub","content":[]}}`))
	two := claudeTurn(t, d, tp, "two", "u-2")

	if err := d.Rewind(two.Seq, "deux"); err != nil {
		t.Fatal(err)
	}
	if st := d.State(); !st.Restart || st.Queued != 1 {
		t.Fatalf("state = %+v: the live process must be replaced, the message kept for the next", st)
	}
	if lines := d.Next(); len(lines) != 0 {
		t.Fatalf("wrote %s to the process being replaced", lines)
	}
	d.Observe(tp.exited(128 + 15))
	argv := strings.Join(d.Command(), " ")
	if !strings.Contains(argv, "--resume s-1 --resume-session-at u-1") {
		t.Fatalf("command = %s", argv)
	}
	start := tp.startedAs(d.Command())
	d.Observe(start)
	if msg, _ := write(d, tp); !strings.Contains(msg.Line, "deux") {
		t.Fatalf("wrote %q", msg.Line)
	}
	evs := d.Observe(tp.out(`{"type":"system","subtype":"init","session_id":"s-1"}`))
	live := rewinds(evs)
	if !reflect.DeepEqual(live, []event.Rewind{{From: two.Seq, To: start.Seq}}) {
		t.Fatalf("rewind = %+v", live)
	}
	d.Observe(tp.out(`{"type":"assistant","uuid":"u-3","message":{"id":"m3","content":[{"type":"text","text":"ok"}]}}`))
	d.Observe(tp.out(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`))
	if argv := strings.Join(d.Command(), " "); !strings.Contains(argv, "--resume s-1") || strings.Contains(argv, "--resume-session-at") {
		t.Fatalf("the generation after = %s", argv)
	}

	// Back to the first message: there is nothing before it to resume.
	if err := d.Rewind(one.Seq, "uno"); err != nil {
		t.Fatal(err)
	}
	argv = strings.Join(d.Command(), " ")
	if strings.Contains(argv, "--resume") || strings.Contains(argv, "--session-id s-1") {
		t.Fatalf("command = %s, want a new session", argv)
	}
	d.Observe(tp.exited(128 + 15))
	start = tp.startedAs(d.Command())
	d.Observe(start)
	write(d, tp)
	fresh := d.rewind
	if fresh != nil {
		t.Fatal("the rewind was not taken up by the generation started with it")
	}
	evs = d.Observe(tp.out(`{"type":"system","subtype":"init","session_id":"s-2"}`))
	if got := rewinds(evs); !reflect.DeepEqual(got, []event.Rewind{{From: one.Seq, To: start.Seq}}) {
		t.Fatalf("rewind = %+v", got)
	}
	live = append(live, rewinds(evs)...)
	if d.State().SessionID != "s-2" || len(d.prompts) != 1 {
		t.Fatalf("session %s, prompts %+v", d.State().SessionID, d.prompts)
	}

	// The log alone says the same.
	replay := newClaude("a1", "s-1", Options{})
	if got := rewinds(feed(replay, tp.recs)); !reflect.DeepEqual(got, live) {
		t.Fatalf("replayed rewinds = %+v, live %+v", got, live)
	}
	if replay.State().SessionID != "s-2" || !reflect.DeepEqual(replay.prompts, d.prompts) {
		t.Fatalf("replay ended in %s %+v, live in %s %+v", replay.State().SessionID, replay.prompts, d.State().SessionID, d.prompts)
	}
}

func TestClaudeRewindRefusals(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "s-1", Options{})
	d.Observe(tp.startedAs(d.Command()))
	claudeTurn(t, d, tp, "one", "u-1")
	d.Send("long task")
	busy, _ := write(d, tp)
	if err := d.Rewind(busy.Seq, "x"); err == nil || !strings.Contains(err.Error(), "working") {
		t.Fatalf("rewind while working: %v", err)
	}
	d.Send("and also this")
	queued, _ := write(d, tp)
	d.Observe(tp.out(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`))
	d.Observe(tp.out(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`))
	if err := d.Rewind(queued.Seq, "x"); err == nil || !strings.Contains(err.Error(), "while it was working") {
		t.Fatalf("rewind to a message sent mid-turn: %v", err)
	}
	if err := d.Rewind(9999, "x"); err == nil {
		t.Fatal("rewind to a record that is no message")
	}
	if err := d.Rewind(busy.Seq, "x"); err != nil {
		t.Fatal(err)
	}
	if err := d.Rewind(busy.Seq, "y"); err == nil {
		t.Fatal("a second rewind before the first one started")
	}
}

func TestClaudeRewindThatDoesNotResumeLeavesTheConversation(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "s-1", Options{})
	d.Observe(tp.startedAs(d.Command()))
	claudeTurn(t, d, tp, "one", "u-1")
	two := claudeTurn(t, d, tp, "two", "u-2")
	d.Observe(tp.exited(0))
	if err := d.Rewind(two.Seq, "deux"); err != nil {
		t.Fatal(err)
	}
	d.Observe(tp.startedAs(d.Command()))
	write(d, tp)
	d.Observe(tp.add(shimproto.DirErr, "Error: no message u-1 in session s-1", nil))
	evs := d.Observe(tp.exited(1))
	if len(rewinds(evs)) != 0 || d.State().Status != event.StatusError || !strings.Contains(d.State().Error, "could not go back") {
		t.Fatalf("events %v, state %+v", kinds(evs, false), d.State())
	}
	if argv := strings.Join(d.Command(), " "); !strings.Contains(argv, "--resume s-1") || strings.Contains(argv, "--resume-session-at") {
		t.Fatalf("the generation after = %s", argv)
	}
	if err := d.Rewind(two.Seq, "deux"); err != nil {
		t.Fatalf("the message is still there to go back to: %v", err)
	}
}

// codexTurn runs one Codex turn to completion and returns the record that
// carried the message.
func codexTurn(t *testing.T, d *codex, tp *tape, text, turn string) shimproto.Record {
	t.Helper()
	d.Send(text)
	msg, _ := write(d, tp)
	return codexFinish(t, d, tp, msg, turn)
}

func codexFinish(t *testing.T, d *codex, tp *tape, msg shimproto.Record, turn string) shimproto.Record {
	t.Helper()
	var req struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	json.Unmarshal([]byte(msg.Line), &req)
	if req.Method != "turn/start" {
		t.Fatalf("wrote %s, want a turn/start", msg.Line)
	}
	id, _ := json.Marshal(req.ID)
	d.Observe(tp.out(`{"id":` + string(id) + `,"result":{"turn":{"id":"` + turn + `"}}}`))
	d.Observe(tp.out(`{"method":"turn/started","params":{"turn":{"id":"` + turn + `"}}}`))
	d.Observe(tp.out(`{"method":"turn/completed","params":{"turn":{"id":"` + turn + `","status":"completed"}}}`))
	return msg
}

func TestCodexRewindRevertsTheThreadBeforeTheTurn(t *testing.T) {
	tp := &tape{}
	d := newCodex("a1", "", Options{})
	codexOpen(t, d, tp)
	one := codexFinish(t, d, tp, tp.recs[len(tp.recs)-1], "t-1")
	two := codexTurn(t, d, tp, "two", "t-2")
	codexTurn(t, d, tp, "three", "t-3")

	if err := d.Rewind(two.Seq, "deux"); err != nil {
		t.Fatal(err)
	}
	req, _ := write(d, tp)
	var call struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	json.Unmarshal([]byte(req.Line), &call)
	if call.Method != "thread/revert" || call.Params["beforeTurnId"] != "t-2" || call.Params["threadId"] != "th-1" {
		t.Fatalf("wrote %s", req.Line)
	}
	if lines := d.Next(); len(lines) != 0 {
		t.Fatalf("wrote %s before codex answered the revert", lines)
	}
	id, _ := json.Marshal(call.ID)
	d.Observe(tp.out(`{"method":"thread/reverted","params":{"threadId":"th-1"}}`))
	evs := d.Observe(tp.out(`{"id":` + string(id) + `,"result":{"thread":{"id":"th-1"}}}`))
	if got := rewinds(evs); !reflect.DeepEqual(got, []event.Rewind{{From: two.Seq, To: req.Seq}}) {
		t.Fatalf("rewind = %+v", got)
	}
	msg, _ := write(d, tp)
	if !strings.Contains(msg.Line, "deux") {
		t.Fatalf("wrote %s, want the edited message", msg.Line)
	}
	codexFinish(t, d, tp, msg, "t-4")
	if len(d.prompts) != 2 || d.prompts[0].src != one.Seq || d.prompts[1].at != "t-4" {
		t.Fatalf("prompts = %+v", d.prompts)
	}

	replay := newCodex("a1", "", Options{})
	if got := rewinds(feed(replay, tp.recs)); !reflect.DeepEqual(got, []event.Rewind{{From: two.Seq, To: req.Seq}}) {
		t.Fatalf("replayed rewinds = %+v", got)
	}
	if !reflect.DeepEqual(replay.prompts, d.prompts) {
		t.Fatalf("replay ended with %+v, live with %+v", replay.prompts, d.prompts)
	}
}

func TestCodexRewindCodexRefusesGoesOnTop(t *testing.T) {
	tp := &tape{}
	d := newCodex("a1", "", Options{})
	codexOpen(t, d, tp)
	one := codexFinish(t, d, tp, tp.recs[len(tp.recs)-1], "t-1")
	if err := d.Rewind(one.Seq, "uno"); err != nil {
		t.Fatal(err)
	}
	req, _ := write(d, tp)
	var call struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(req.Line), &call)
	id, _ := json.Marshal(call.ID)
	evs := d.Observe(tp.out(`{"id":` + string(id) + `,"error":{"code":-32600,"message":"turn not found"}}`))
	if len(rewinds(evs)) != 0 || !strings.Contains(strings.Join(texts(evs), " "), "could not go back") {
		t.Fatalf("events = %+v", evs)
	}
	if msg, _ := write(d, tp); !strings.Contains(msg.Line, "uno") {
		t.Fatalf("wrote %s", msg.Line)
	}
}

func texts(evs []event.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Text)
	}
	return out
}
