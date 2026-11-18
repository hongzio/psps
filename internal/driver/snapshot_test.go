package driver

import (
	"reflect"
	"slices"
	"testing"

	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/shimproto"
)

// Every field of a driver's state is either in its snapshot or one the
// caller sets on a fresh driver. A field added to a driver must be put in
// one or the other: one missing from the snapshot is lost to a daemon that
// starts again, which then knows less than the log says.
func TestSnapshotHasEveryField(t *testing.T) {
	for _, c := range []struct {
		v      any
		fields []string
	}{
		// agent, opts, host, held: set on a fresh driver by its caller.
		{base{}, []string{"agent", "opts", "forkOf", "host", "alive", "busy", "starting", "blocked", "interrupted", "errText", "lastStderr", "restart", "held", "status"}},
		{claude{}, []string{"base", "sessionID", "sessionUsed", "gen", "pendingTurns", "tasks", "queue", "pending", "stop", "stopping", "ctlSeq", "streamMsg",
			"prompts", "lastEntry", "compacted", "rewind", "going", "forking", "noted", "asked", "commands", "models", "terminal", "usage",
			"running", "known", "switching", "refused", "sentMode", "changed"}},
		{clPending{}, []string{"requestID", "publicID", "subtype", "input", "approval", "decision", "sent", "questions", "answers"}},
		{clRewind{}, []string{"at", "session", "from", "start", "before"}},
		{clSwitch{}, []string{"id", "mode"}},
		// roots: set on a fresh driver by its caller.
		{codex{}, []string{"base", "threadID", "gen", "phase", "nextID", "calls", "turnID", "pending", "queue", "sending", "defaults", "reading",
			"stop", "itemSum", "prompts", "revert", "reverting", "roots", "listed", "skills", "models", "usage"}},
		{cxPending{}, []string{"rpcID", "method", "publicID", "approval", "decision", "sent"}},
		{cxRevert{}, []string{"turn", "from", "req"}},
		{prompt{}, []string{"src", "at", "ok"}},
	} {
		typ := reflect.TypeOf(c.v)
		var got []string
		for i := range typ.NumField() {
			got = append(got, typ.Field(i).Name)
		}
		if !slices.Equal(got, c.fields) {
			t.Errorf("%s has fields %v; the snapshot was written for %v: put the new ones in snapshot.go", typ.Name(), got, c.fields)
		}
	}
}

// A driver restored from a snapshot taken at any record goes on exactly as
// one that folded the whole log: the same events from there, and the same
// state at the end.
func checkSnapshots(t *testing.T, fresh func() Driver, recs []shimproto.Record) {
	t.Helper()
	whole := fresh()
	var evs [][]event.Event
	for _, r := range recs {
		evs = append(evs, whole.Observe(r))
	}
	end, err := Snapshot(whole)
	if err != nil {
		t.Fatal(err)
	}
	for k := range len(recs) + 1 {
		before := fresh()
		feed(before, recs[:k])
		snap, err := Snapshot(before)
		if err != nil {
			t.Fatal(err)
		}
		d := fresh()
		if err := Restore(d, snap); err != nil {
			t.Fatalf("at %d: %v", k, err)
		}
		for i, r := range recs[k:] {
			if got := d.Observe(r); !reflect.DeepEqual(got, evs[k+i]) {
				t.Fatalf("restored at %d, record %d: events %+v, folded %+v", k, k+i+1, got, evs[k+i])
			}
		}
		if got, _ := Snapshot(d); string(got) != string(end) {
			t.Fatalf("restored at %d ends in\n%s\nfolded ends in\n%s", k, got, end)
		}
		if got, want := d.State(), whole.State(); !reflect.DeepEqual(got, want) {
			t.Fatalf("restored at %d: state %+v, folded %+v", k, got, want)
		}
	}
}

func TestCodexSnapshotGoesOnAsTheFold(t *testing.T) {
	recs := loadCodexFixture(t)
	checkSnapshots(t, func() Driver { return newCodex("a1", "", Options{}) }, recs)
}

func TestClaudeSnapshotGoesOnAsTheFold(t *testing.T) {
	// A log as a driver ran it: an approval in a turn, a question, the
	// process ending, and the next resuming.
	tp := &tape{}
	d := newClaude("a1", "s1", Options{Approvals: ApprovalsAsk})
	d.Observe(tp.started())
	run := func() {
		for _, l := range d.Next() {
			d.Observe(tp.in(string(l)))
		}
	}
	d.Send("list files")
	run()
	d.Observe(tp.out(`{"type":"system","subtype":"init","session_id":"s1"}`))
	d.Observe(tp.out(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1"}}}`))
	d.Observe(tp.out(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll"}}}`))
	d.Observe(tp.out(`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"I'll list them."},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]}}`))
	evs := d.Observe(tp.out(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tu_1"}}`))
	if err := d.Approve(evs[0].Approval.ID, Accept, nil); err != nil {
		t.Fatal(err)
	}
	run()
	d.Observe(tp.out(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"a\nb"}]}}`))
	evs = d.Observe(tp.out(`{"type":"control_request","request_id":"r2","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":` + askInput + `,"tool_use_id":"tu_2"}}`))
	if err := d.Approve(evs[0].Approval.ID, Accept, map[string]string{"Which fruit do you like?": "Apple", "Which colors do you like?": "Red"}); err != nil {
		t.Fatal(err)
	}
	run()
	d.Observe(tp.out(`{"type":"assistant","message":{"id":"msg_2","content":[{"type":"text","text":"Two files."}]}}`))
	d.Observe(tp.out(`{"type":"result","subtype":"success","is_error":false,"result":"Two files."}`))
	d.Observe(tp.exited(0))
	d.Observe(tp.started())
	d.Send("again")
	run()
	d.Observe(tp.out(`{"type":"system","subtype":"init","session_id":"s1"}`))

	checkSnapshots(t, func() Driver { return newClaude("a1", "s1", Options{Approvals: ApprovalsAsk}) }, tp.recs)
}

func TestRestoreRefusesAnotherKindOrVersion(t *testing.T) {
	snap, err := Snapshot(newCodex("a1", "", Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(newClaude("a1", "s1", Options{}), snap); err == nil {
		t.Fatal("a claude driver took a codex snapshot")
	}
	if err := Restore(newCodex("a1", "", Options{}), []byte(`{"version":0,"kind":"codex","codex":{}}`)); err == nil {
		t.Fatal("a snapshot of another version was taken")
	}
}

func TestSnapshotKeepsUsageLimitsEmptyOrNil(t *testing.T) {
	for _, limits := range [][]Limit{nil, {}} {
		c := newCodex("a1", "", Options{})
		c.usage.Limits = limits
		snap, err := Snapshot(c)
		if err != nil {
			t.Fatal(err)
		}
		d := newCodex("a1", "", Options{})
		if err := Restore(d, snap); err != nil {
			t.Fatal(err)
		}
		if (d.usage.Limits == nil) != (limits == nil) || !reflect.DeepEqual(d.State().Usage, c.State().Usage) {
			t.Fatalf("limits %#v came back %#v", limits, d.usage.Limits)
		}
	}
}
