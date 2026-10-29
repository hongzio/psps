package driver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/event"
)

func names(cs []Command) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Prefix+c.Name)
	}
	return out
}

// Each claude process is asked once for its commands, and what it answers
// is the driver's from the log alone: a fresh driver replaying it neither
// asks again nor forgets.
func TestClaudeIsAskedForItsCommands(t *testing.T) {
	tp := &tape{}
	d := newClaude("a1", "", Options{})
	d.Observe(tp.started())
	d.Send("hi")
	lines := d.Next()
	if len(lines) != 2 || !strings.Contains(string(lines[0]), `"subtype":"initialize"`) || !strings.Contains(string(lines[1]), `"hi"`) {
		t.Fatalf("Next = %s", lines)
	}
	for _, l := range lines {
		d.Observe(tp.in(string(l)))
	}
	if again := d.Next(); len(again) != 0 {
		t.Fatalf("asked again: %s", again)
	}
	d.Observe(tp.out(`{"type":"system","subtype":"init","session_id":"s1","terminal_slash_commands":["doctor"]}`))
	d.Observe(tp.out(`{"type":"control_response","response":{"subtype":"success","request_id":"gero-init-1","response":{` +
		`"commands":[{"name":"compact","description":"Free up context","argumentHint":"<instructions>"},` +
		`{"name":"model","description":"Set the model"},{"name":"clear"},{"name":"doctor"},{"name":"__internal"},` +
		`{"name":"grill-me","description":"Interview me (user)","argumentHint":""}],` +
		`"models":[{"value":"opus","displayName":"Opus","description":"Opus 5.5"},{"value":"haiku","displayName":"Haiku"}]}}}`))
	// The answer to anything else gero asked is not one.
	d.Observe(tp.out(`{"type":"control_response","response":{"subtype":"success","request_id":"gero-1-1","response":{"commands":[]}}}`))

	want := []Command{
		{Prefix: "/", Name: "compact", Description: "Free up context", Hint: "<instructions>"},
		{Prefix: "/", Name: "grill-me", Description: "Interview me (user)"},
	}
	wantModels := []Choice{{Value: "opus", Description: "Opus · Opus 5.5"}, {Value: "haiku", Description: "Haiku"}}
	if got := d.Commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands %+v", got)
	}
	if got := d.Models(); !reflect.DeepEqual(got, wantModels) {
		t.Fatalf("models %+v", got)
	}

	re := newClaude("a1", "", Options{})
	feed(re, tp.recs)
	if got := re.Commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed %+v", got)
	}
	if lines := re.Next(); len(lines) != 0 {
		t.Fatalf("a replay asked again: %s", lines)
	}

	// A new process is asked afresh, and keeps the last answer meanwhile.
	d.Observe(tp.exited(0))
	d.Observe(tp.started())
	if lines := d.Next(); len(lines) != 1 || !strings.Contains(string(lines[0]), `"gero-init-2"`) {
		t.Fatalf("Next = %s", lines)
	}
	if len(d.Commands()) != 2 {
		t.Fatalf("%+v", d.Commands())
	}
}

func codexReady(t *testing.T, d *codex, tp *tape) {
	t.Helper()
	next := func() {
		for _, l := range d.Next() {
			d.Observe(tp.in(string(l)))
		}
	}
	d.Observe(tp.started())
	next()
	d.Observe(tp.out(`{"id":1,"result":{}}`))
	next()
	d.Observe(tp.out(`{"id":2,"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}}`))
	next()
	d.Observe(tp.out(`{"id":3,"result":{"thread":{"id":"th-1"}}}`))
}

func TestCodexSkillsModelsAndCompact(t *testing.T) {
	tp := &tape{}
	d := newCodex("a1", "", Options{})
	codexReady(t, d, tp)
	lines := d.Next()
	if len(lines) != 2 {
		t.Fatalf("Next = %s", lines)
	}
	for _, l := range lines {
		d.Observe(tp.in(string(l)))
	}
	d.Observe(tp.out(`{"id":4,"result":{"data":[{"cwd":"/w","errors":[],"skills":[` +
		`{"name":"review-pr","description":"Review a pull request in depth","shortDescription":"Review a PR","enabled":true,"path":"/s/a","scope":"user"},` +
		`{"name":"off","description":"x","enabled":false,"path":"/s/b","scope":"user"},` +
		`{"name":"plan","description":"Plan a change","enabled":true,"path":"/s/c","scope":"repo"}]}]}}`))
	if evs := d.Observe(tp.out(`{"id":5,"error":{"code":-32603,"message":"no catalog"}}`)); len(evs) != 0 {
		t.Fatalf("a failed model/list said %+v", evs)
	}
	want := []Command{cxCompact,
		{Prefix: "$", Name: "review-pr", Description: "Review a PR"},
		{Prefix: "$", Name: "plan", Description: "Plan a change"}}
	if got := d.Commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands %+v", got)
	}
	if d.Models() != nil {
		t.Fatalf("models %+v", d.Models())
	}

	// /compact is not text for the model but codex told to compact, as a
	// turn of its own; what is sent after it waits for that turn to end.
	d.Send(" /compact ")
	d.Send("after")
	lines = d.Next()
	var m map[string]any
	if len(lines) != 1 || json.Unmarshal(lines[0], &m) != nil || m["method"] != "thread/compact/start" {
		t.Fatalf("Next = %s", lines)
	}
	evs := d.Observe(tp.in(string(lines[0])))
	if len(evs) < 1 || evs[0].Kind != event.KindUser || evs[0].Text != "/compact" || d.State().Status != event.StatusWorking {
		t.Fatalf("events %+v, %s", evs, d.State().Status)
	}
	d.Observe(tp.out(`{"id":6,"result":{}}`))
	d.Observe(tp.out(`{"method":"turn/started","params":{"turn":{"id":"tu-c"}}}`))
	if lines := d.Next(); len(lines) != 0 {
		t.Fatalf("sent during the compaction: %s", lines)
	}
	d.Observe(tp.out(`{"method":"item/completed","params":{"turnId":"tu-c","item":{"type":"contextCompaction","id":"c1"}}}`))
	d.Observe(tp.out(`{"method":"turn/completed","params":{"turn":{"id":"tu-c","status":"completed"}}}`))
	lines = d.Next()
	if len(lines) != 1 || !strings.Contains(string(lines[0]), `"turn/start"`) || !strings.Contains(string(lines[0]), `"after"`) {
		t.Fatalf("Next = %s", lines)
	}
	d.Observe(tp.in(string(lines[0])))
	// It is no message to go back to.
	if len(d.prompts) != 1 {
		t.Fatalf("prompts %+v", d.prompts)
	}

	// A compaction codex refuses leaves the agent free.
	d.Observe(tp.out(`{"id":7,"result":{"turn":{"id":"tu-2"}}}`))
	d.Observe(tp.out(`{"method":"turn/completed","params":{"turn":{"id":"tu-2","status":"completed"}}}`))
	d.Send("/compact")
	for _, l := range d.Next() {
		d.Observe(tp.in(string(l)))
	}
	evs = d.Observe(tp.out(`{"id":8,"error":{"code":-32600,"message":"nothing to compact"}}`))
	if len(evs) == 0 || evs[0].Kind != event.KindError || d.State().Status != event.StatusIdle {
		t.Fatalf("events %+v, %s", evs, d.State().Status)
	}
}
