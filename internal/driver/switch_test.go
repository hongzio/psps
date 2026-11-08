package driver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/shimproto"
)

// startedWith is a started record with the command line d starts.
func (t *tape) startedWith(d Driver) shimproto.Record {
	return t.add(shimproto.DirSys, "", &shimproto.Sys{Kind: shimproto.SysStarted, PID: 42, Argv: d.Command()})
}

// modeRequests are the set_permission_mode requests in lines, by id.
func modeRequests(t *testing.T, lines [][]byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range lines {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
				Mode    string `json:"mode"`
			} `json:"request"`
		}
		json.Unmarshal(l, &m)
		if m.Type == "control_request" && m.Request.Subtype == "set_permission_mode" {
			out[m.RequestID] = m.Request.Mode
		}
	}
	return out
}

// Between ask and auto the running claude is told the mode, once, and
// runs in it once it says so; a replay of the log knows the same.
func TestClaudeSwitchesBetweenAskAndAutoWhileItRuns(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "", Options{Approvals: ApprovalsAsk})
	d.Observe(tp.startedWith(d))
	feedNext := func() map[string]string {
		lines := d.Next()
		for _, l := range lines {
			d.Observe(tp.in(string(l)))
		}
		return modeRequests(t, lines)
	}
	if got := feedNext(); len(got) != 0 {
		t.Fatalf("a mode asked for with nothing changed: %v", got)
	}

	d.SetOptions(Options{Approvals: ApprovalsAuto})
	if d.State().Stale {
		t.Fatal("stale for a mode it switches to")
	}
	asked := feedNext()
	if len(asked) != 1 {
		t.Fatalf("asked %v", asked)
	}
	var id string
	for k, v := range asked {
		if id = k; v != "auto" {
			t.Fatalf("mode %q", v)
		}
	}
	if again := feedNext(); len(again) != 0 {
		t.Fatalf("asked twice: %v", again)
	}
	// A daemon that attaches now replays the log, and asks nothing more.
	re := newClaude("a1", d.sessionID, Options{Approvals: ApprovalsAuto})
	feed(re, tp.recs)
	if got := modeRequests(t, re.Next()); len(got) != 0 || re.State().Stale {
		t.Fatalf("after a replay: asked %v, stale %v", got, re.State().Stale)
	}

	evs := d.Observe(tp.out(`{"type":"control_response","response":{"subtype":"success","request_id":"` + id + `"}}`))
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "approvals are auto now") {
		t.Fatalf("events %+v", evs)
	}
	if d.running.Approvals != ApprovalsAuto || d.State().Stale {
		t.Fatalf("running %+v, stale %v", d.running, d.State().Stale)
	}
	if got := feedNext(); len(got) != 0 {
		t.Fatalf("asked again once it switched: %v", got)
	}

	// And back.
	d.SetOptions(Options{Approvals: ApprovalsAsk})
	if back := feedNext(); len(back) != 1 {
		t.Fatalf("back to ask: %v", back)
	}
}

// A mode claude refuses, or anything but a mode between ask and auto,
// waits for a new process: the driver says the one running is stale.
func TestClaudeIsStaleForWhatItCannotSwitchTo(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "", Options{Approvals: ApprovalsAsk})
	d.Observe(tp.startedWith(d))
	d.SetOptions(Options{Approvals: ApprovalsAuto})
	var id string
	for _, l := range d.Next() {
		d.Observe(tp.in(string(l)))
		for k := range modeRequests(t, [][]byte{l}) {
			id = k
		}
	}
	evs := d.Observe(tp.out(`{"type":"control_response","response":{"subtype":"error","request_id":"` + id + `","error":"auto_mode_unavailable"}}`))
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "auto_mode_unavailable") || !d.State().Stale {
		t.Fatalf("refused: %+v, stale %v", evs, d.State().Stale)
	}
	if again := modeRequests(t, d.Next()); len(again) != 0 {
		t.Fatalf("asked again after a refusal: %v", again)
	}
	// The next process starts with it, and is not stale.
	d.Observe(tp.exited(0))
	d.Observe(tp.startedWith(d))
	if d.State().Stale || d.running.Approvals != ApprovalsAuto {
		t.Fatalf("the new process: running %+v, stale %v", d.running, d.State().Stale)
	}

	for _, o := range []Options{{Approvals: ApprovalsNever}, {Approvals: ApprovalsDefault}, {Approvals: ApprovalsAuto, Model: "haiku"}, {Approvals: ApprovalsAuto, Effort: "high"}} {
		d.SetOptions(o)
		if !d.State().Stale || len(modeRequests(t, d.Next())) != 0 {
			t.Errorf("%+v: stale %v", o, d.State().Stale)
		}
	}
	// Set back as it runs: nothing to do.
	d.SetOptions(Options{Approvals: ApprovalsAuto})
	if d.State().Stale {
		t.Fatal("stale with the options it runs on")
	}
}

// A process whose command line psps cannot read is replaced for any new
// options, as before.
func TestClaudeWithAnUnreadCommandLineIsReplaced(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "", Options{Approvals: ApprovalsAsk})
	d.Observe(tp.started())
	if d.State().Stale {
		t.Fatal("stale before anything changed")
	}
	d.SetOptions(Options{Approvals: ApprovalsAuto})
	if !d.State().Stale || len(modeRequests(t, d.Next())) != 0 {
		t.Fatalf("stale %v", d.State().Stale)
	}
}

// One started in default is told auto or ask while it runs; back to
// default it waits for a new process, as no mode psps names is the
// settings' own.
func TestClaudeStartedInDefaultSwitchesToAutoWhileItRuns(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "", Options{Approvals: ApprovalsDefault})
	d.Observe(tp.startedWith(d))
	d.SetOptions(Options{Approvals: ApprovalsAuto})
	if d.State().Stale {
		t.Fatal("stale for auto, which it switches to")
	}
	var id string
	for _, l := range d.Next() {
		d.Observe(tp.in(string(l)))
		for k, v := range modeRequests(t, [][]byte{l}) {
			if v != "auto" {
				t.Fatalf("mode %q", v)
			}
			id = k
		}
	}
	if id == "" {
		t.Fatal("auto was not asked for")
	}
	d.Observe(tp.out(`{"type":"control_response","response":{"subtype":"success","request_id":"` + id + `"}}`))
	if d.running.Approvals != ApprovalsAuto || d.State().Stale {
		t.Fatalf("running %+v, stale %v", d.running, d.State().Stale)
	}
	d.SetOptions(Options{Approvals: ApprovalsDefault})
	if !d.State().Stale || len(modeRequests(t, d.Next())) != 0 {
		t.Fatalf("back to default: stale %v", d.State().Stale)
	}
}
