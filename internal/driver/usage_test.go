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
	tp.out(`{"type":"result","subtype":"success","total_cost_usd":13.2,"modelUsage":{"claude-opus-5-5":{"contextWindow":1000000,"inputTokens":6,"cacheCreationInputTokens":7033,"cacheReadInputTokens":50964,"outputTokens":397},"claude-haiku-5-5":{"contextWindow":200000,"inputTokens":14547,"outputTokens":1057}}}`)
	// An error claude writes itself says nothing of either.
	tp.out(`{"type":"assistant","message":{"id":"m3","model":"<synthetic>","usage":{"input_tokens":0},"content":[{"type":"text","text":"Not logged in"}]}}`)
	tp.out(`{"type":"result","subtype":"success","is_error":true,"total_cost_usd":0,"modelUsage":{}}`)
	feed(c, tp.recs)
	want := &Usage{Model: "claude-opus-5-5", Cwd: "/home/agent/work/trevia", Context: 463675, Cached: 463335, Window: 1000000, CostUSD: 13.2,
		Spent:  Tokens{Input: 6 + 7033 + 14547, Cached: 50964, Output: 397 + 1057},
		Limits: []Limit{{Window: "five_hour", Percent: 9, ResetsAt: 1793350200}, {Window: "seven_day", Percent: 31, ResetsAt: 1793732400}}}
	if got := c.State().Usage; !reflect.DeepEqual(got, want) {
		t.Fatalf("usage\n got %+v\nwant %+v", got, want)
	}
}

// The lines are cut down from a real codex 0.166 log: a thread resumed in
// a new process says its total again before it grows.
func TestTheStatusLineIsTheLatestCodexSaid(t *testing.T) {
	d := newCodex("a1", "", Options{})
	tp := &tape{}
	open := func(result string) {
		d.Observe(tp.started())
		for _, id := range []int{1, 2, 3} {
			for _, l := range d.Next() {
				d.Observe(tp.in(string(l)))
			}
			switch id {
			case 1:
				d.Observe(tp.out(`{"id":1,"result":{}}`))
			case 2:
				d.Observe(tp.out(`{"id":2,"result":{"account":{"type":"chatgpt"},"requiresOpenaiAuth":true}}`))
			case 3:
				d.Observe(tp.out(`{"id":3,"result":` + result + `}`))
			}
		}
	}
	open(`{"thread":{"id":"th-1"},"model":"gpt-6-luna","cwd":"/home/agent/work/a1"}`)
	if d.State().Usage == nil || d.State().Usage.Model != "gpt-6-luna" {
		t.Fatalf("thread/start's model: %+v", d.State().Usage)
	}
	tp.out(`{"method":"thread/tokenUsage/updated","params":{"threadId":"th-1","turnId":"u1","tokenUsage":{"total":{"totalTokens":14179,"inputTokens":14145,"cachedInputTokens":12288,"outputTokens":34,"reasoningOutputTokens":13},"last":{"totalTokens":14179,"inputTokens":14145,"cachedInputTokens":12288,"outputTokens":34,"reasoningOutputTokens":13},"modelContextWindow":258400}}}`)
	tp.out(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":3,"windowDurationMins":300,"resetsAt":1793170860},"secondary":{"usedPercent":2,"windowDurationMins":10080,"resetsAt":1793671263},"limitId":"codex","planType":"plus"}}}`)
	tp.exited(0)
	feed(d, tp.recs[len(tp.recs)-3:])
	// A new process, the same thread.
	open(`{"thread":{"id":"th-1"},"model":"gpt-6-luna","cwd":"/home/agent/work/a1"}`)
	n := len(tp.recs)
	tp.out(`{"method":"thread/tokenUsage/updated","params":{"threadId":"th-1","turnId":"u1","tokenUsage":{"total":{"totalTokens":14179,"inputTokens":14145,"cachedInputTokens":12288,"outputTokens":34,"reasoningOutputTokens":13},"last":{"totalTokens":14179,"inputTokens":14145,"cachedInputTokens":12288,"outputTokens":34,"reasoningOutputTokens":13},"modelContextWindow":258400}}}`)
	tp.out(`{"method":"thread/tokenUsage/updated","params":{"threadId":"th-1","turnId":"u2","tokenUsage":{"total":{"totalTokens":28392,"inputTokens":28351,"cachedInputTokens":26240,"outputTokens":41,"reasoningOutputTokens":20},"last":{"totalTokens":14213,"inputTokens":14206,"cachedInputTokens":13952,"outputTokens":7,"reasoningOutputTokens":7},"modelContextWindow":258400}}}`)
	// A sparse update: the window it leaves out stays as it was.
	tp.out(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":4,"windowDurationMins":300,"resetsAt":1793170860}}}}`)
	feed(d, tp.recs[n:])
	want := &Usage{Model: "gpt-6-luna", Cwd: "/home/agent/work/a1", Context: 14206, Cached: 13952, Window: 258400,
		Spent:  Tokens{Input: 28351 - 26240, Cached: 26240, Output: 41},
		Limits: []Limit{{Window: "five_hour", Percent: 4, ResetsAt: 1793170860}, {Window: "seven_day", Percent: 2, ResetsAt: 1793671263}}}
	if got := d.State().Usage; !reflect.DeepEqual(got, want) {
		t.Fatalf("usage\n got %+v\nwant %+v", got, want)
	}
}
