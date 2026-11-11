package store

import (
	"testing"
	"time"

	"github.com/hongzio/psps/internal/driver"
)

// A CLI's models are the isolation's own, else the ones it last said in
// any; one said earlier never takes the place of one said later.
func TestCLIModels(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	one := func(v string) []driver.Choice { return []driver.Choice{{Value: v, Description: v + "!"}} }
	value := func(kind driver.Kind, iso string) string {
		t.Helper()
		ms, from, err := s.CLIModels(kind, iso)
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) == 0 {
			return "none"
		}
		return ms[0].Value + "@" + from
	}
	if got := value(driver.Claude, "a"); got != "none" {
		t.Fatal(got)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetCLIModels(driver.Claude, "a", one("opus"), t0))
	must(s.SetCLIModels(driver.Claude, "b", one("sonnet"), t0.Add(time.Minute)))
	must(s.SetCLIModels(driver.Codex, "c", one("gpt"), t0.Add(2*time.Minute)))
	for _, c := range []struct {
		kind      driver.Kind
		iso, want string
	}{
		{driver.Claude, "a", "opus@a"},
		{driver.Claude, "b", "sonnet@b"},
		{driver.Claude, "z", "sonnet@b"}, // the latest said anywhere
		{driver.Codex, "a", "gpt@c"},
	} {
		if got := value(c.kind, c.iso); got != c.want {
			t.Fatalf("%s in %s: %s, want %s", c.kind, c.iso, got, c.want)
		}
	}
	// A log replayed says again what was said before.
	must(s.SetCLIModels(driver.Claude, "a", one("haiku"), t0.Add(-time.Hour)))
	if got := value(driver.Claude, "a"); got != "opus@a" {
		t.Fatal(got)
	}
	must(s.SetCLIModels(driver.Claude, "a", one("haiku"), t0.Add(time.Hour)))
	if got := value(driver.Claude, "a"); got != "haiku@a" {
		t.Fatal(got)
	}
}
