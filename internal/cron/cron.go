// Package cron reads the schedules gero sends an agent messages on: a
// five-field cron expression as crontab(5) has it, read in the Mac's local
// time, one of its @ names, or "@every" an interval.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed schedule.
type Schedule struct {
	spec string
	// every is an @every interval; zero for an expression.
	every time.Duration
	// Each field is a bit set of the values it takes.
	min, hour, dom, month, dow uint64
	// domStar and dowStar: the day fields start with "*". Days match
	// either field when both are restricted, both otherwise (crontab(5)).
	domStar, dowStar bool
	loc              *time.Location
}

// field is one of an expression's five, with what its values may be.
type field struct {
	name     string
	min, max int
	names    []string // names[i] stands for min+i
}

var fields = []field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	// 7 is Sunday too.
	{name: "day of week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

var named = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

// MinEvery is the shortest interval: an expression's own resolution.
const MinEvery = time.Minute

// Parse reads spec in the local time zone.
func Parse(spec string) (Schedule, error) { return ParseIn(spec, time.Local) }

// ParseIn reads spec in loc.
func ParseIn(spec string, loc *time.Location) (Schedule, error) {
	spec = strings.Join(strings.Fields(spec), " ")
	s := Schedule{spec: spec, loc: loc}
	if spec == "" {
		return s, errors.New("the schedule is empty")
	}
	if rest, ok := strings.CutPrefix(spec, "@every "); ok {
		d, err := time.ParseDuration(strings.ReplaceAll(rest, " ", ""))
		switch {
		case err != nil:
			return s, fmt.Errorf("@every takes an interval like 30m or 2h: %q is not one", rest)
		case d < MinEvery:
			return s, fmt.Errorf("@every %s is too often: once a minute at most", rest)
		case d%time.Minute != 0:
			return s, fmt.Errorf("@every %s is not whole minutes", rest)
		}
		s.every = d
		return s, nil
	}
	expr := spec
	if strings.HasPrefix(spec, "@") {
		e, ok := named[strings.ToLower(spec)]
		if !ok {
			return s, fmt.Errorf("%s is not a schedule gero knows: there are @hourly, @daily, @weekly, @monthly, @yearly and @every", spec)
		}
		expr = e
	}
	parts := strings.Fields(expr)
	if len(parts) != len(fields) {
		return s, fmt.Errorf("%q has %d fields; a cron expression has 5: minute hour day-of-month month day-of-week", spec, len(parts))
	}
	sets := make([]uint64, len(fields))
	for i, f := range fields {
		set, err := f.parse(parts[i])
		if err != nil {
			return s, err
		}
		sets[i] = set
	}
	s.min, s.hour, s.dom, s.month, s.dow = sets[0], sets[1], sets[2], sets[3], sets[4]
	if s.dow&(1<<7) != 0 {
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domStar, s.dowStar = parts[2][0] == '*', parts[4][0] == '*'
	if !s.dowStar || s.domStar {
		return s, nil
	}
	// Only the day of the month counts: it has to come in one of the
	// months, February with its 29th.
	for m := 1; m <= 12; m++ {
		if s.month&(1<<m) != 0 && s.dom&(1<<(daysIn(m)+1)-1) != 0 {
			return s, nil
		}
	}
	return s, fmt.Errorf("%q never comes: none of its months has that day", spec)
}

func daysIn(month int) int {
	switch month {
	case 2:
		return 29
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// parse reads one field: a list of *, N, N-M, each with an optional /step.
func (f field) parse(s string) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(s, ",") {
		rng, step, hasStep := strings.Cut(part, "/")
		lo, hi := f.min, f.max
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			hi = lo
			if isRange {
				if hi, err = f.value(b); err != nil {
					return 0, err
				}
			} else if hasStep {
				// N/step is N, then every step to the end.
				hi = f.max
			}
			if lo > hi {
				return 0, fmt.Errorf("%s %q runs backwards", f.name, rng)
			}
		}
		n := 1
		if hasStep {
			var err error
			if n, err = strconv.Atoi(step); err != nil || n < 1 {
				return 0, fmt.Errorf("%s %q: a step is a number from 1", f.name, part)
			}
		}
		for v := lo; v <= hi; v += n {
			set |= 1 << v
		}
	}
	return set, nil
}

func (f field) value(s string) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%s %q is not one: %d to %d", f.name, s, f.min, f.max)
	}
	return v, nil
}

func (s Schedule) String() string { return s.spec }

// Every is the schedule's interval, if it is one.
func (s Schedule) Every() time.Duration { return s.every }

// After is when the schedule next comes after now. An interval counts from
// prev, the last time it came (or when it was set), so it keeps its beat
// across runs that came late; times it missed are not made up.
func (s Schedule) After(prev, now time.Time) time.Time {
	if s.every > 0 {
		next := prev.Add(s.every)
		if !next.After(now) {
			next = prev.Add((now.Sub(prev)/s.every + 1) * s.every)
		}
		return next
	}
	return s.Next(now)
}

// Next is the first minute after t an expression matches; the zero time
// if none does in five years. For an interval it is t plus the interval.
func (s Schedule) Next(t time.Time) time.Time {
	if s.every > 0 {
		return t.Add(s.every)
	}
	// Truncated as an instant: a local 01:00 can be two of them.
	t = t.Truncate(time.Minute).Add(time.Minute).In(s.loc)
	end := t.AddDate(5, 0, 0)
	for t.Before(end) {
		y, mo, d := t.Date()
		switch {
		case s.month&(1<<int(mo)) == 0:
			t = later(t, time.Date(y, mo+1, 1, 0, 0, 0, 0, s.loc))
		case !s.day(t):
			t = later(t, time.Date(y, mo, d+1, 0, 0, 0, 0, s.loc))
		case s.hour&(1<<t.Hour()) == 0:
			t = later(t, time.Date(y, mo, d, t.Hour()+1, 0, 0, 0, s.loc))
		case s.min&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// later is next, or a minute after t where a change of clocks put next at
// or before it.
func later(t, next time.Time) time.Time {
	if next.After(t) {
		return next
	}
	return t.Add(time.Minute)
}

func (s Schedule) day(t time.Time) bool {
	dom := s.dom&(1<<t.Day()) != 0
	dow := s.dow&(1<<int(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}
