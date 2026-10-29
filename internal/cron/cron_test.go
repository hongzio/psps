package cron

import (
	"strings"
	"testing"
	"time"
)

var seoul = time.FixedZone("KST", 9*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, seoul)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	for _, c := range []struct{ spec, from, want string }{
		{"*/30 * * * *", "2026-10-29 14:10", "2026-10-29 14:30"},
		{"*/30 * * * *", "2026-10-29 14:30", "2026-10-29 15:00"},
		{"0 9 * * 1-5", "2026-10-30 09:00", "2026-11-02 09:00"}, // Friday, then Monday
		{"0 9 * * mon-fri", "2026-10-31 08:00", "2026-11-02 09:00"},
		{"30 18 * * 0", "2026-10-29 00:00", "2026-11-01 18:30"},
		{"30 18 * * 7", "2026-10-29 00:00", "2026-11-01 18:30"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 1 * *", "2026-12-15 00:00", "2027-01-01 12:00"},
		// Both day fields restricted: either one.
		{"0 8 1 * 1", "2026-10-27 00:00", "2026-11-01 08:00"},
		{"0 8 15 * 1", "2026-11-01 09:00", "2026-11-02 08:00"},
		// One of them a star (even a stepped one): both.
		{"0 8 */2 * 1", "2026-11-01 09:00", "2026-11-09 08:00"},
		{"15 10-12/2 * * *", "2026-10-29 10:16", "2026-10-29 12:15"},
		{"5/20 * * * *", "2026-10-29 10:46", "2026-10-29 11:05"},
		{"0,45 3 * jan,oct *", "2026-10-29 03:30", "2026-10-29 03:45"},
		{"@daily", "2026-10-29 03:30", "2026-10-30 00:00"},
		{"@hourly", "2026-10-29 03:30", "2026-10-29 04:00"},
		{"@weekly", "2026-10-29 03:30", "2026-11-01 00:00"},
	} {
		s, err := ParseIn(c.spec, seoul)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := s.Next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s after %s: %s, want %s", c.spec, c.from, got.Format("2006-01-02 15:04 Mon"), c.want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for spec, want := range map[string]string{
		"":               "empty",
		"* * * *":        "has 4 fields",
		"60 * * * *":     "minute \"60\" is not one",
		"* 24 * * *":     "hour",
		"* * 0 * *":      "day of month",
		"* * * 13 *":     "month",
		"* * * * 8":      "day of week",
		"*/0 * * * *":    "a step is a number",
		"5-1 * * * *":    "runs backwards",
		"* * * * funday": "day of week",
		"0 0 30 2 *":     "never comes",
		"0 0 31 4,6,9 *": "never comes",
		"@sometimes":     "not a schedule",
		"@every 30s":     "too often",
		"@every 90s":     "whole minutes",
		"@every soon":    "is not one",
		"1,,2 * * * *":   "minute",
	} {
		if _, err := ParseIn(spec, seoul); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", spec, err, want)
		}
	}
	// February 30 never comes, but a Monday does.
	if _, err := ParseIn("0 0 30 2 1", seoul); err != nil {
		t.Fatal(err)
	}
}

func TestAnIntervalKeepsItsBeat(t *testing.T) {
	s, err := ParseIn("@every  45m", seoul)
	if err != nil || s.String() != "@every 45m" || s.Every() != 45*time.Minute {
		t.Fatal(s, err)
	}
	set := at("2026-10-29 10:00")
	if got := s.After(set, set); !got.Equal(at("2026-10-29 10:45")) {
		t.Fatal(got)
	}
	// Run late, at 10:47: the next is still on the beat.
	if got := s.After(at("2026-10-29 10:45"), at("2026-10-29 10:47")); !got.Equal(at("2026-10-29 11:30")) {
		t.Fatal(got)
	}
	// Asleep for hours: what it missed is not made up.
	if got := s.After(at("2026-10-29 10:45"), at("2026-10-29 16:00")); !got.Equal(at("2026-10-29 16:45")) {
		t.Fatal(got)
	}
}

// The Mac changing its clocks neither stops a schedule nor runs it twice.
func TestDaylightSaving(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// 2026-03-08 02:00 does not exist there; that day's 02:30 is skipped.
	s, _ := ParseIn("30 2 * * *", ny)
	got := s.Next(time.Date(2026, 3, 8, 1, 0, 0, 0, ny))
	if want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny); !got.Equal(want) {
		t.Fatalf("spring: %s", got)
	}
	// 2026-11-01 01:00-02:00 happens twice; hourly goes on, once an hour.
	s, _ = ParseIn("0 * * * *", ny)
	t0 := time.Date(2026, 11, 1, 0, 30, 0, 0, ny)
	var seen []string
	for range 4 {
		t0 = s.Next(t0)
		seen = append(seen, t0.Format("15:04 MST"))
	}
	if got := strings.Join(seen, ", "); got != "01:00 EDT, 01:00 EST, 02:00 EST, 03:00 EST" {
		t.Fatal(got)
	}
}
