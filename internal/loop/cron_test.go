package loop

import (
	"strings"
	"testing"
	"time"
)

// TestParseCronAcceptsTheStandardForms pins the expressions that must work. A
// cron parser that rejects something reasonable is as bad as one that accepts
// garbage: the operator cannot tell which without reading the source.
func TestParseCronAcceptsTheStandardForms(t *testing.T) {
	valid := []string{
		"* * * * *",
		"0 3 * * *",
		"*/15 * * * *",
		"0 0 1 * *",
		"30 9 * * MON-FRI",
		"0 0 * * 7", // 7 is Sunday in some dialects — accepted
		"0 0 * JAN,JUL *",
		"0 */6 * * *",
		"5/10 * * * *", // step from an offset
		"0 0 1,15 * *",
		"@hourly",
		"@daily",
		"@midnight",
		"@weekly",
		"@monthly",
	}
	for _, expr := range valid {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q) rejected a valid expression: %v", expr, err)
		}
	}
}

// TestParseCronRejectsGarbage is the other half: garbage is refused with a
// reason, not silently accepted as a schedule that never fires.
func TestParseCronRejectsGarbage(t *testing.T) {
	bad := map[string]string{
		"":             "empty",
		"* * * *":      "4 fields",
		"* * * * * *":  "6 fields",
		"60 * * * *":   "out of range minute",
		"0 24 * * *":   "out of range hour",
		"0 0 32 * *":   "out of range day",
		"0 0 * 13 *":   "out of range month",
		"0 0 * * 8":    "out of range weekday",
		"0 0 * * XYZ":  "unknown weekday name",
		"*/0 * * * *":  "zero step",
		"*/x * * * *":  "non-numeric step",
		"5-1 * * * *":  "backwards range",
		"0 0 * * ,":    "empty list entry",
		"0 0 * * MON-": "dangling range",
		"@fortnightly": "unknown shorthand",
		"every minute": "prose",
	}
	for expr, why := range bad {
		_, err := ParseCron(expr)
		if err == nil {
			t.Errorf("ParseCron(%q) accepted garbage (%s)", expr, why)
			continue
		}
		if !strings.Contains(err.Error(), "cron:") {
			t.Errorf("ParseCron(%q) error does not name the parser: %v", expr, err)
		}
	}
}

// TestCronNextDaily proves the reference time and the fired time relate
// correctly: strictly after the reference, never equal to it.
func TestCronNextDaily(t *testing.T) {
	sch, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// At 02:59:59 the next firing is the same day's 03:00.
	ref := time.Date(2026, 10, 26, 2, 59, 59, 0, time.UTC)
	got, ok := sch.Next(ref)
	if !ok {
		t.Fatal("Next reported no firing")
	}
	want := time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next(%s) = %s, want %s", ref, got, want)
	}

	// At exactly 03:00:00 the next firing is tomorrow: a job that just fired is
	// not re-fired by the same reference time.
	at := time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)
	got, ok = sch.Next(at)
	if !ok {
		t.Fatal("Next reported no firing")
	}
	want = time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next(%s) = %s, want %s (strictly after)", at, got, want)
	}
}

// TestCronNextWeekdayRange proves day-of-week restrictions work.
func TestCronNextWeekdayRange(t *testing.T) {
	sch, err := ParseCron("0 9 * * MON-FRI")
	if err != nil {
		t.Fatal(err)
	}
	// Saturday 2026-10-31 10:00 → next is Monday 2026-11-02 09:00.
	ref := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	got, ok := sch.Next(ref)
	if !ok {
		t.Fatal("Next reported no firing")
	}
	want := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next(%s) = %s (%s), want %s", ref, got, got.Weekday(), want)
	}
}

// TestCronNextStep proves a step expression enumerates its minutes.
func TestCronNextStep(t *testing.T) {
	sch, err := ParseCron("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	ref := time.Date(2026, 10, 26, 9, 1, 0, 0, time.UTC)
	for _, want := range []int{15, 30, 45, 0} {
		got, ok := sch.Next(ref)
		if !ok {
			t.Fatal("Next reported no firing")
		}
		if got.Minute() != want {
			t.Fatalf("Next(%s) = %s, want minute %d", ref, got, want)
		}
		ref = got
	}
}

// TestCronNeverFires reports false rather than searching forever.
func TestCronNeverFires(t *testing.T) {
	sch, err := ParseCron("0 0 30 2 *") // 30 February
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := sch.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("a 30 February schedule reported %s", got)
	}
}

// TestCronFieldsRoundTrip proves the normalized field view is readable.
func TestCronFieldsRoundTrip(t *testing.T) {
	sch, err := ParseCron("0 3 * * MON-FRI")
	if err != nil {
		t.Fatal(err)
	}
	fields := sch.CronFields()
	if len(fields) != 5 {
		t.Fatalf("CronFields returned %d fields", len(fields))
	}
	if fields[0] != "0" || fields[1] != "3" {
		t.Errorf("fields = %v, want minute 0 and hour 3", fields)
	}
	if fields[4] != "1,2,3,4,5" {
		t.Errorf("weekday field = %q, want the Mon-Fri list", fields[4])
	}
}
