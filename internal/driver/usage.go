package driver

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Usage is what the agent's CLI last said of its model and spending: what
// a status line under the conversation shows. Every field is the latest
// the log has, zero until it has one.
type Usage struct {
	Model string `json:"model,omitempty"`
	// Cwd is where the CLI says it runs.
	Cwd string `json:"cwd,omitempty"`
	// Context is the tokens the last request of the conversation sent, of
	// the model's Window; Cached of them were read from the prompt cache.
	Context int `json:"context,omitempty"`
	Cached  int `json:"cached,omitempty"`
	Window  int `json:"window,omitempty"`
	// CostUSD is the conversation's estimated cost so far.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Spent is what the conversation has used so far, a resumed one's
	// included, as its CLI counts it.
	Spent Tokens `json:"spent,omitzero"`
	// Limits are the account's plan windows, five_hour and seven_day.
	Limits []Limit `json:"limits,omitempty"`
}

// Tokens are a conversation's tokens: Input sent anew (written to the
// prompt cache or not), Cached read back from it, and Output written,
// reasoning included.
type Tokens struct {
	Input  int64 `json:"input"`
	Cached int64 `json:"cached"`
	Output int64 `json:"output"`
}

// New is the tokens sent anew and written: what a plan's use goes with,
// as a read from the prompt cache costs a fraction of either.
func (t Tokens) New() int64 { return t.Input + t.Output }

func (t Tokens) Plus(o Tokens) Tokens {
	return Tokens{Input: t.Input + o.Input, Cached: t.Cached + o.Cached, Output: t.Output + o.Output}
}

func (t Tokens) Minus(o Tokens) Tokens {
	return Tokens{Input: t.Input - o.Input, Cached: t.Cached - o.Cached, Output: t.Output - o.Output}
}

// Limit is how much of a plan window is used, and when it starts over.
type Limit struct {
	Window   string  `json:"window"`
	Percent  float64 `json:"percent"`
	ResetsAt int64   `json:"resets_at,omitempty"` // unix seconds
}

// shown is u for State: nil while the log has said nothing of it.
func (u Usage) shown() *Usage {
	if u.Model == "" && u.Cwd == "" && u.Context == 0 && u.CostUSD == 0 && u.Spent == (Tokens{}) && u.Limits == nil {
		return nil
	}
	return &u
}

// clUsage is a request's token counts, as claude prints them.
type clUsage struct {
	Input         int `json:"input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
}

// clRateLimit is a rate_limit_event's info: its unifiedWindows are every
// plan window, utilization a fraction of one.
type clRateLimit struct {
	Windows map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// assistantUsage takes the context of a main-conversation message: a
// subagent's runs in a context of its own, and a synthetic one (an error
// claude writes itself) was never sent.
func (u *Usage) assistantUsage(raw json.RawMessage) {
	var msg struct {
		Model string  `json:"model"`
		Usage clUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Model == "" || msg.Model == "<synthetic>" {
		return
	}
	u.Model = msg.Model
	u.Context = msg.Usage.Input + msg.Usage.CacheCreation + msg.Usage.CacheRead
	u.Cached = msg.Usage.CacheRead
}

// clModelUsage is a result's account of one model: the conversation's so
// far, a resumed one's included, as the cost is.
type clModelUsage struct {
	ContextWindow int   `json:"contextWindow"`
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CacheRead     int64 `json:"cacheReadInputTokens"`
	CacheCreation int64 `json:"cacheCreationInputTokens"`
}

// resultUsage takes a result's cost and tokens, and the window of the
// model in use. Both are the conversation's so far, every model's (a
// subagent's, the auto mode's classifier) included; an error result may
// carry zeros, which say nothing.
func (u *Usage) resultUsage(cost float64, models map[string]clModelUsage) {
	if cost > 0 {
		u.CostUSD = cost
	}
	var spent Tokens
	for _, m := range models {
		spent = spent.Plus(Tokens{Input: m.Input + m.CacheCreation, Cached: m.CacheRead, Output: m.Output})
	}
	if spent != (Tokens{}) {
		u.Spent = spent
	}
	if w := models[u.Model].ContextWindow; w > 0 {
		u.Window = w
	}
}

// cxTokens is one of codex's token counts: its input includes what was
// read from the cache, and its output the reasoning.
type cxTokens struct {
	Input  int64 `json:"inputTokens"`
	Cached int64 `json:"cachedInputTokens"`
	Output int64 `json:"outputTokens"`
}

// tokenUsage takes thread/tokenUsage/updated: total is the thread's so
// far, a resumed one's included, and last its latest request.
func (u *Usage) tokenUsage(params json.RawMessage) {
	var p struct {
		TokenUsage struct {
			Total  cxTokens `json:"total"`
			Last   cxTokens `json:"last"`
			Window *int     `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	t, l := p.TokenUsage.Total, p.TokenUsage.Last
	u.Spent = Tokens{Input: t.Input - t.Cached, Cached: t.Cached, Output: t.Output}
	if l.Input > 0 {
		u.Context, u.Cached = int(l.Input), int(l.Cached)
	}
	if w := p.TokenUsage.Window; w != nil && *w > 0 {
		u.Window = *w
	}
}

// cxWindows names codex's plan windows by their length, as claude names
// its own.
var cxWindows = map[int64]string{300: "five_hour", 10080: "seven_day"}

// codexRateLimits takes account/rateLimits/updated, which may leave out a
// window: one left out keeps what was said of it.
func (u *Usage) codexRateLimits(params json.RawMessage) {
	type window struct {
		Used     float64 `json:"usedPercent"`
		Mins     *int64  `json:"windowDurationMins"`
		ResetsAt *int64  `json:"resetsAt"`
	}
	var p struct {
		RateLimits struct {
			Primary   *window `json:"primary"`
			Secondary *window `json:"secondary"`
		} `json:"rateLimits"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	byName := map[string]Limit{}
	for _, l := range u.Limits {
		byName[l.Window] = l
	}
	for _, w := range []*window{p.RateLimits.Primary, p.RateLimits.Secondary} {
		if w == nil || w.Mins == nil {
			continue
		}
		name := cxWindows[*w.Mins]
		if name == "" {
			name = fmt.Sprintf("%dm", *w.Mins)
		}
		l := Limit{Window: name, Percent: min(w.Used, 100)}
		if w.ResetsAt != nil {
			l.ResetsAt = *w.ResetsAt
		}
		byName[name] = l
	}
	if len(byName) == 0 {
		return
	}
	ls := make([]Limit, 0, len(byName))
	for _, l := range byName {
		ls = append(ls, l)
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Window < ls[j].Window })
	u.Limits = ls
}

func (u *Usage) rateLimits(info clRateLimit) {
	if len(info.Windows) == 0 {
		return
	}
	var ls []Limit
	for name, w := range info.Windows {
		ls = append(ls, Limit{Window: name, Percent: min(w.Utilization*100, 100), ResetsAt: w.ResetsAt})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Window < ls[j].Window })
	u.Limits = ls
}
