package driver

import (
	"reflect"
	"testing"
)

// The lines are cut down from a real claude 2.1.314 log.
func TestTheStatusLineIsTheLatestClaudeSaid(t *testing.T) {
	c := newClaude("a1", "", Options{})
	if c.State().Usage != nil {
		t.Fatal("a log that said nothing has a usage")
	}
	tp := &tape{}
	tp.started()
	tp.out(`{"type":"system","subtype":"init","model":"claude-opus-5-5","cwd":"/home/agent/work/trevia","session_id":"s1"}`)
	tp.out(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793350200,"rateLimitType":"five_hour","unifiedWindows":{"seven_day":{"utilization":0.31,"resetsAt":1793732400,"status":"allowed"},"five_hour":{"utilization":0.09,"resetsAt":1793350200,"status":"allowed"}}}}`)
	tp.out(`{"type":"assistant","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":338,"cache_read_input_tokens":463335,"output_tokens":2},"content":[{"type":"text","text":"hi"}]}}`)
	// A subagent's message, on a model and in a context of its own.
	tp.out(`{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"m2","model":"claude-haiku-5-5","usage":{"input_tokens":9000},"content":[]}}`)
	tp.out(`{"type":"result","subtype":"success","total_cost_usd":13.2,"modelUsage":{"claude-opus-5-5":{"contextWindow":1000000},"claude-haiku-5-5":{"contextWindow":200000}}}`)
	// An error claude writes itself says nothing of either.
	tp.out(`{"type":"assistant","message":{"id":"m3","model":"<synthetic>","usage":{"input_tokens":0},"content":[{"type":"text","text":"Not logged in"}]}}`)
	tp.out(`{"type":"result","subtype":"success","is_error":true,"total_cost_usd":0,"modelUsage":{}}`)
	feed(c, tp.recs)
	want := &Usage{Model: "claude-opus-5-5", Cwd: "/home/agent/work/trevia", Context: 463675, Cached: 463335, Window: 1000000, CostUSD: 13.2,
		Limits: []Limit{{Window: "five_hour", Percent: 9, ResetsAt: 1793350200}, {Window: "seven_day", Percent: 31, ResetsAt: 1793732400}}}
	if got := c.State().Usage; !reflect.DeepEqual(got, want) {
		t.Fatalf("usage\n got %+v\nwant %+v", got, want)
	}
}
