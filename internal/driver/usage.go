package driver

import (
	"encoding/json"
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
	// Limits are the account's plan windows, five_hour and seven_day.
	Limits []Limit `json:"limits,omitempty"`
}

// Limit is how much of a plan window is used, and when it starts over.
type Limit struct {
	Window   string  `json:"window"`
	Percent  float64 `json:"percent"`
	ResetsAt int64   `json:"resets_at,omitempty"` // unix seconds
}

// shown is u for State: nil while the log has said nothing of it.
func (u Usage) shown() *Usage {
	if u.Model == "" && u.Cwd == "" && u.Context == 0 && u.CostUSD == 0 && u.Limits == nil {
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

// resultUsage takes a result's cost and the window of the model in use.
// The cost is the conversation's so far, a resumed one's included; an
// error result may carry zeros, which say nothing.
func (u *Usage) resultUsage(cost float64, models map[string]struct {
	ContextWindow int `json:"contextWindow"`
}) {
	if cost > 0 {
		u.CostUSD = cost
	}
	if w := models[u.Model].ContextWindow; w > 0 {
		u.Window = w
	}
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
