package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hongzio/gero/internal/proxy"
)

// Forms for env definitions and exposure settings are drawn from the
// service's own field list, the same one the CLI builds its flags from, so a
// new proxy needs no TUI code.

const (
	optDefault   = "default"
	optUnchanged = "(unchanged)"
)

// genFields renders fields as form fields, prefilled from cur (a stored
// definition or settings). While adding, a bool or a choice offers
// "default": the service fills its defaults in.
func genFields(fields []proxy.Field, cur map[string]any, adding bool) []*field {
	var out []*field
	for _, f := range fields {
		hint := f.Help
		switch f.Kind {
		case proxy.Bool:
			opts := []string{"on", "off"}
			b, has := cur[f.Key].(bool)
			if adding || !has {
				opts = []string{optDefault, "on", "off"}
			}
			fd := selectField(f.Key, opts, hint)
			if has {
				fd.idx = indexOf(opts, map[bool]string{true: "on", false: "off"}[b])
			}
			out = append(out, fd)
		case proxy.String:
			s, _ := cur[f.Key].(string)
			if len(f.Choices) > 0 {
				opts := append([]string{optDefault}, f.Choices...)
				fd := selectField(f.Key, opts, hint)
				fd.idx = indexOf(opts, s)
				out = append(out, fd)
				continue
			}
			fd := textField(f.Key, f.Metavar, hint)
			if _, isString := cur[f.Key].(string); cur[f.Key] != nil && !isString {
				// Stored in a shape the field does not spell (git's pinned
				// token command): left as it is unless a value is typed.
				fd.text.Placeholder = optUnchanged
			}
			fd.text.SetValue(s)
			out = append(out, fd)
		case proxy.Int:
			fd := textField(f.Key, f.Metavar, hint)
			if n, ok := cur[f.Key].(float64); ok {
				fd.text.SetValue(strconv.Itoa(int(n)))
			}
			out = append(out, fd)
		case proxy.List:
			have := proxy.Strings(cur, f.Key)
			if len(f.Choices) > 0 {
				out = append(out, checkField(f.Key, f.Choices, have, "space ticks; "+hint))
				continue
			}
			fd := textField(f.Key, f.Metavar+", …", "comma-separated; "+hint)
			fd.text.SetValue(strings.Join(have, ", "))
			out = append(out, fd)
		}
	}
	return out
}

// patchFrom is the edit the form spells, in the CLI's terms: what was
// given is set, a list emptied is cleared, the rest is left alone.
func patchFrom(fields []proxy.Field, ff []*field, cur map[string]any) (proxy.Patch, error) {
	p := proxy.Patch{Set: map[string]any{}}
	for i, f := range fields {
		fd := ff[i]
		switch f.Kind {
		case proxy.Bool:
			switch fd.value() {
			case "on":
				p.Set[f.Key] = true
			case "off":
				p.Set[f.Key] = false
			}
		case proxy.String:
			if v := fd.value(); v != "" && v != optDefault {
				p.Set[f.Key] = v
			}
		case proxy.Int:
			if v := fd.value(); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return proxy.Patch{}, fmt.Errorf("%s must be a number, not %q", f.Key, v)
				}
				p.Set[f.Key] = n
			}
		case proxy.List:
			var items []string
			if fd.kind == fCheck {
				items = fd.values()
			} else {
				for _, s := range strings.Split(fd.value(), ",") {
					if s = strings.TrimSpace(s); s != "" {
						items = append(items, s)
					}
				}
			}
			switch {
			case len(items) > 0:
				p.Set[f.Key] = items
			case len(proxy.Strings(cur, f.Key)) > 0:
				p.Clear = append(p.Clear, f.Key)
			}
		}
	}
	return p, nil
}

// decodeMap is a stored definition as the form reads it.
func decodeMap(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) > 0 {
		json.Unmarshal(raw, &m)
	}
	return m
}

// compact is a definition on one line, keys in order: what `gero env ls`
// shows.
func compact(raw json.RawMessage) string {
	m := decodeMap(raw)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		var v string
		switch x := m[k].(type) {
		case []any:
			var s []string
			for _, e := range x {
				s = append(s, fmt.Sprint(e))
			}
			v = strings.Join(s, ",")
		default:
			v = fmt.Sprint(x)
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}
