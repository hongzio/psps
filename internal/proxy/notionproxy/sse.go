package notionproxy

import (
	"strings"
)

// Event is one server-sent event: its non-data lines, its data joined, and
// the JSON-RPC message the data holds (nil when it holds none).
type Event struct {
	Fields  []string
	Data    string
	Message map[string]any
}

// SSEMessages splits an event-stream body into events.
func SSEMessages(raw []byte) []Event {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var out []Event
	for _, block := range strings.Split(text, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev Event
		var data []string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(rest, " "))
			} else {
				ev.Fields = append(ev.Fields, line)
			}
		}
		ev.Data = strings.Join(data, "\n")
		if len(data) > 0 {
			if m, err := decode([]byte(ev.Data)); err == nil && len(m) > 0 {
				ev.Message = m
			}
		}
		out = append(out, ev)
	}
	return out
}

// RewriteBody is the upstream answer with the result for requestID narrowed
// for e; anything else in it is left as it was.
func RewriteBody(raw []byte, contentType string, requestID any, kind string, e Env) []byte {
	fix := func(m map[string]any) bool {
		if m == nil || !sameID(m["id"], requestID) {
			return false
		}
		if _, ok := m["result"]; !ok {
			return false
		}
		m["result"] = Narrow(kind, m["result"], e)
		return true
	}
	if strings.Contains(contentType, "text/event-stream") {
		var blocks []string
		for _, ev := range SSEMessages(raw) {
			data := ev.Data
			if fix(ev.Message) {
				data = string(encode(ev.Message))
			}
			lines := append([]string(nil), ev.Fields...)
			if data != "" {
				for _, l := range strings.Split(data, "\n") {
					lines = append(lines, "data: "+l)
				}
			}
			blocks = append(blocks, strings.Join(lines, "\n"))
		}
		return []byte(strings.Join(blocks, "\n\n") + "\n\n")
	}
	m, err := decode(raw)
	if err != nil || !fix(m) {
		return raw
	}
	return encode(m)
}
