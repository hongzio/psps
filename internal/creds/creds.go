// Package creds is the one file every credential proxy reads its secrets
// from: an INI whose sections are `[<service> <env>]`, the same pair the
// env registry is keyed by, so "which secret does prod Datadog use" has one
// answer on the host. The format is isolate's, so a file can move between
// the two.
//
// It is read per request and never cached: rotating a secret takes effect on
// the next call, and no second copy lives in any process's memory longer
// than a request. A file group or other can read is refused, with the chmod
// spelled out, rather than repaired — silently fixing it would hide that the
// secret sat exposed.
package creds

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Fields is what each service's section must hold.
var Fields = map[string][]string{
	"pg":         {"password"},
	"datadog":    {"api_key", "app_key"},
	"slack":      {"token"},
	"clickhouse": {"user", "password"},
	"grafana":    {"token"},
	"notion":     {"client_id", "refresh_token"},
}

// Optional fields are kept when present and never demanded: notion's access
// token is minted from the refresh token whenever it is missing or stale.
var Optional = map[string][]string{
	"notion": {"access_token", "expires_at"},
}

// LoginServices have sections written by a browser login rather than by
// hand, so the fix a refusal names is the login.
var LoginServices = map[string]bool{"notion": true}

// Denied is a refusal whose Reason is fit to show the operator — and the
// agent, through a proxy's error — as the exact fix.
type Denied struct{ Reason string }

func (d *Denied) Error() string { return d.Reason }

type Store struct {
	Path string
	mu   sync.Mutex
}

func section(service, env string) string { return service + " " + env }

// Stanza is the edit that fixes a refusal, which is also what `gero env
// secret` writes.
func (s *Store) Stanza(service, env string) string {
	if LoginServices[service] {
		return fmt.Sprintf("run `gero env secret %s %s` to log in in the browser (it writes [%s] in %s)",
			service, env, section(service, env), s.Path)
	}
	var lines []string
	for _, f := range Fields[service] {
		lines = append(lines, f+" = <"+f+">")
	}
	return fmt.Sprintf("add to %s (mode 0600):\n[%s]\n%s\nor run `gero env secret %s %s`",
		s.Path, section(service, env), strings.Join(lines, "\n"), service, env)
}

type file struct {
	order    []string
	sections map[string]map[string]string
	keys     map[string][]string // key order per section, for stable writes
}

func (s *Store) load() (*file, error) {
	st, err := os.Stat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, &Denied{fmt.Sprintf("cannot read %s: %v", s.Path, err)}
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, &Denied{fmt.Sprintf("%s is readable by group or other; it holds live credentials — run `chmod 600 %s`", s.Path, s.Path)}
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, &Denied{fmt.Sprintf("cannot read %s: %v", s.Path, err)}
	}
	defer f.Close()
	out, err := parse(bufio.NewScanner(f))
	if err != nil {
		return nil, &Denied{fmt.Sprintf("%s is not parseable: %v", s.Path, err)}
	}
	return out, nil
}

// parse reads the INI subset Python's configparser writes and accepts:
// `[section]`, `key = value` or `key: value`, # and ; comments, keys
// case-folded, indented continuation lines. Values are opaque — no
// interpolation, so a % in a password is just a %.
func parse(sc *bufio.Scanner) (*file, error) {
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	f := &file{sections: map[string]map[string]string{}, keys: map[string][]string{}}
	cur, lastKey := "", ""
	for n := 1; sc.Scan(); n++ {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			lastKey = ""
			continue
		}
		if raw[0] == ' ' || raw[0] == '\t' {
			if cur == "" || lastKey == "" {
				return nil, fmt.Errorf("line %d: continuation without a key", n)
			}
			f.sections[cur][lastKey] += "\n" + line
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: unterminated section header", n)
			}
			cur = line[1 : len(line)-1]
			if _, dup := f.sections[cur]; dup {
				return nil, fmt.Errorf("line %d: section [%s] appears twice", n, cur)
			}
			f.sections[cur] = map[string]string{}
			f.order = append(f.order, cur)
			lastKey = ""
			continue
		}
		if cur == "" {
			return nil, fmt.Errorf("line %d: a value outside any section", n)
		}
		i := strings.IndexAny(line, "=:")
		if i <= 0 {
			return nil, fmt.Errorf("line %d: expected key = value", n)
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		if _, dup := f.sections[cur][key]; dup {
			return nil, fmt.Errorf("line %d: %s appears twice in [%s]", n, key, cur)
		}
		f.sections[cur][key] = strings.TrimSpace(line[i+1:])
		f.keys[cur] = append(f.keys[cur], key)
		lastKey = key
	}
	return f, sc.Err()
}

// Read returns every field the service needs, plus any optional ones
// present, or a Denied carrying the exact edit.
func (s *Store) Read(service, env string) (map[string]string, error) {
	f, err := s.load()
	if errors.Is(err, os.ErrNotExist) {
		return nil, &Denied{fmt.Sprintf("no credentials file at %s; %s", s.Path, s.Stanza(service, env))}
	}
	if err != nil {
		return nil, err
	}
	sec, ok := f.sections[section(service, env)]
	if !ok {
		return nil, &Denied{fmt.Sprintf("%s has no [%s] section; %s", s.Path, section(service, env), s.Stanza(service, env))}
	}
	values := map[string]string{}
	var missing []string
	for _, k := range Fields[service] {
		if v := strings.TrimSpace(sec[k]); v != "" {
			values[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, &Denied{fmt.Sprintf("[%s] in %s is missing %s; %s", section(service, env), s.Path,
			strings.Join(missing, " and "), s.Stanza(service, env))}
	}
	for _, k := range Optional[service] {
		if v := strings.TrimSpace(sec[k]); v != "" {
			values[k] = v
		}
	}
	return values, nil
}

// Status says which of the service's required fields are set, without
// returning any value: what a settings screen may show.
func (s *Store) Status(service, env string) (missing []string, err error) {
	f, err := s.load()
	if errors.Is(err, os.ErrNotExist) {
		return Fields[service], nil
	}
	if err != nil {
		return nil, err
	}
	sec := f.sections[section(service, env)]
	for _, k := range Fields[service] {
		if strings.TrimSpace(sec[k]) == "" {
			missing = append(missing, k)
		}
	}
	return missing, nil
}

// Write replaces one section (nil values removes it), creating the file
// 0600 from its first byte. It is written beside and renamed over, so a
// proxy reading per request never sees half a file.
func (s *Store) Write(service, env string, values map[string]string) error {
	for k, v := range values {
		if strings.ContainsAny(k, "=:[]\n\r") || strings.TrimSpace(k) != k || k == "" {
			return &Denied{fmt.Sprintf("%q is not a usable credential field name", k)}
		}
		if strings.ContainsAny(v, "\n\r") || strings.TrimSpace(v) != v {
			return &Denied{fmt.Sprintf("the value for %s has a line break or surrounding spaces; the credentials file cannot hold that", k)}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}

	f, err := s.load()
	if errors.Is(err, os.ErrNotExist) {
		f, err = &file{sections: map[string]map[string]string{}, keys: map[string][]string{}}, nil
	}
	if err != nil {
		return err
	}
	name := section(service, env)
	if _, ok := f.sections[name]; !ok && values != nil {
		f.order = append(f.order, name)
	}
	if values == nil {
		delete(f.sections, name)
	} else {
		f.sections[name] = values
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		f.keys[name] = keys
	}

	var b strings.Builder
	for _, sec := range f.order {
		vals, ok := f.sections[sec]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "[%s]\n", sec)
		for _, k := range f.keys[sec] {
			if v, ok := vals[k]; ok {
				fmt.Fprintf(&b, "%s = %s\n", k, strings.ReplaceAll(v, "\n", "\n\t"))
			}
		}
		b.WriteString("\n")
	}
	tmp := filepath.Join(filepath.Dir(s.Path), "."+filepath.Base(s.Path)+"."+strconv.Itoa(os.Getpid())+".tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.Path)
}
