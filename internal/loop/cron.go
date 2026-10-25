package loop

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A cron expression in this package is the standard five-field form:
//
//	minute  hour  day-of-month  month  day-of-week
//	0-59    0-23  1-31          1-12   0-6 (0 = Sunday)
//
// Each field supports `*`, a single number, a range `a-b`, a step `*/n` or
// `a-b/n`, and a comma-separated list of those. Month and day-of-week also
// accept their three-letter English names (JAN, MON, ...). The five shorthands
// `@hourly @daily @midnight @weekly @monthly` are accepted as exact aliases.
//
// A field with three stars is deliberately *not* a schedule, and neither is
// "every minute": an interval below a minute is what `kintsugi loop` is for.
// Rejecting garbage loudly is the whole point of parsing here rather than
// handing the string to some later scheduler that would silently do nothing.
//
// The parser is pure: no clock is read, no I/O happens, so a schedule can be
// tested exactly. Next is the only function that reads a time, and it takes
// the reference time as an argument.

// Schedule is a parsed cron expression.
type Schedule struct {
	// Text is the expression as the operator wrote it.
	Text string
	// minute/hour/month are bitmasks over their own ranges (bit n = value n).
	minute uint64
	hour   uint64
	month  uint64
	// dom/dow are bitmasks plus a "restricted" flag: cron's classic rule is
	// that an unrestricted day-of-month and an unrestricted day-of-week mean
	// "either matches", while restricting one of them means "that one decides".
	dom, dow       uint64
	domSet, dowSet bool
}

// Shorthands, mapped to the exact expressions they stand for.
var shorthands = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
}

// monthNames and dayNames let an operator write MON instead of 1.
var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// ParseCron parses a five-field cron expression.
func ParseCron(expr string) (*Schedule, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return nil, fmt.Errorf("cron: empty expression (want five fields, e.g. \"0 3 * * *\")")
	}
	text := raw
	if alias, ok := shorthands[strings.ToLower(raw)]; ok {
		text = alias
	}
	fields := strings.Fields(text)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: %q has %d fields, want 5 (minute hour day-of-month month day-of-week)", raw, len(fields))
	}

	s := &Schedule{Text: raw}
	var err error
	if s.minute, err = parseField(fields[0], 0, 59, nil, "minute"); err != nil {
		return nil, err
	}
	if s.hour, err = parseField(fields[1], 0, 23, nil, "hour"); err != nil {
		return nil, err
	}
	s.dom, err = parseField(fields[2], 1, 31, nil, "day-of-month")
	if err != nil {
		return nil, err
	}
	s.domSet = fields[2] != "*"
	if s.month, err = parseField(fields[3], 1, 12, monthNames, "month"); err != nil {
		return nil, err
	}
	s.dow, err = parseField(fields[4], 0, 7, dayNames, "day-of-week")
	if err != nil {
		return nil, err
	}
	// 7 is Sunday in some cron dialects, so it folds onto 0 rather than being
	// rejected: "0 0 * * 7" must mean what the operator meant. The field accepts
	// 0-7 for that reason only; 8 and up are still refused. The fold is done on
	// the final mask, so "0-7" and "5-7" behave too.
	if s.dow&(1<<7) != 0 {
		s.dow = (s.dow &^ (1 << 7)) | 1<<0
	}
	s.dowSet = fields[4] != "*"
	return s, nil
}

// parseField parses one field into a bitmask. min/max are inclusive; names
// maps a three-letter word to its number (nil when the field has none).
func parseField(field string, min, max int, names map[string]int, label string) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("cron: %s field %q has an empty list entry", label, field)
		}
		lo, hi, step := min, max, 1

		rangePart, stepPart, hasStep := strings.Cut(part, "/")
		if hasStep {
			n, err := strconv.Atoi(stepPart)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron: %s field %q has a bad step %q (want a positive integer)", label, field, stepPart)
			}
			step = n
		}
		if rangePart != "*" {
			a, b, isRange := strings.Cut(rangePart, "-")
			v, err := parseValue(a, names, min, max, label, field)
			if err != nil {
				return 0, err
			}
			lo, hi = v, v
			if isRange {
				w, err := parseValue(b, names, min, max, label, field)
				if err != nil {
					return 0, err
				}
				if w < lo {
					return 0, fmt.Errorf("cron: %s field %q has a backwards range %q", label, field, rangePart)
				}
				hi = w
			} else if hasStep {
				// "5/10" means "from 5 to the end of the field, every 10".
				hi = max
			}
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	if mask == 0 {
		return 0, fmt.Errorf("cron: %s field %q matches nothing", label, field)
	}
	return mask, nil
}

// parseValue reads one number or three-letter name for a field.
func parseValue(s string, names map[string]int, min, max int, label, field string) (int, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	if key == "" {
		return 0, fmt.Errorf("cron: %s field %q is missing a value", label, field)
	}
	if names != nil {
		if v, ok := names[key]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(key)
	if err != nil {
		return 0, fmt.Errorf("cron: %s field %q has an unknown value %q (want %d-%d)", label, field, s, min, max)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("cron: %s field %q has an out-of-range value %d (want %d-%d)", label, field, v, min, max)
	}
	return v, nil
}

// Next returns the first firing strictly after `after`, in `after`'s location.
// It searches forward one day at a time and then within the day, so a schedule
// that never fires (a 30 February) costs a bounded search and reports false
// rather than looping forever.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	if s == nil || s.minute == 0 || s.hour == 0 {
		return time.Time{}, false
	}
	loc := after.Location()
	start := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)

	// Eight years covers every leap-year shape a five-field cron can express,
	// so a schedule that cannot fire (a 30 February) reports false instead of
	// searching forever.
	const maxDaysAhead = 366 * 8
	for day := 0; day <= maxDaysAhead; day++ {
		date := start.AddDate(0, 0, day)
		if !s.dayMatches(date) {
			continue
		}
		for h := 0; h < 24; h++ {
			if s.hour&(1<<uint(h)) == 0 {
				continue
			}
			for m := 0; m < 60; m++ {
				if s.minute&(1<<uint(m)) == 0 {
					continue
				}
				cand := time.Date(date.Year(), date.Month(), date.Day(), h, m, 0, 0, loc)
				// Strictly after: "0 3 * * *" at 03:00:00 fires tomorrow, not
				// now, so a job that just fired is not re-fired by the same
				// reference time.
				if cand.After(after) {
					return cand, true
				}
			}
		}
	}
	return time.Time{}, false
}

// dayMatches applies the month and the classic dom/dow rule: when both day
// fields are restricted, either may match; when only one is, it decides.
func (s *Schedule) dayMatches(d time.Time) bool {
	if s.month&(1<<uint(int(d.Month()))) == 0 {
		return false
	}
	domOK := s.dom&(1<<uint(d.Day())) != 0
	dowOK := s.dow&(1<<uint(int(d.Weekday()))) != 0
	switch {
	case s.domSet && s.dowSet:
		return domOK || dowOK
	case s.domSet:
		return domOK
	case s.dowSet:
		return dowOK
	default:
		return true
	}
}

// String renders the schedule as the operator wrote it.
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.Text
}

// CronFields returns the five normalized fields, for display. Every field is
// rendered as a comma-joined value list, in range order.
func (s *Schedule) CronFields() []string {
	if s == nil {
		return nil
	}
	return []string{
		maskList(s.minute, 0, 59),
		maskList(s.hour, 0, 23),
		maskList(s.dom, 1, 31),
		maskList(s.month, 1, 12),
		maskList(s.dow, 0, 6),
	}
}

// maskList renders a bitmask as a comma-joined value list.
func maskList(mask uint64, min, max int) string {
	var out []string
	for v := min; v <= max; v++ {
		if mask&(1<<uint(v)) != 0 {
			out = append(out, strconv.Itoa(v))
		}
	}
	if len(out) == 0 {
		return "—"
	}
	return strings.Join(out, ",")
}
