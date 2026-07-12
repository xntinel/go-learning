# Exercise 28: Cron Expression Iterator — Parse Cron Schedule and Yield Next N Fire Times

**Nivel: Intermedio** — validacion rapida (un test corto).

A job scheduler that stores `"0 9 * * MON-FRI"` in a config file is useless
until something can answer "when does this fire next" without pulling in an
external cron library -- validating that an on-call rotation's schedule is
reachable at all, computing the next batch window for a UI, or driving a
heartbeat timer all reduce to the same primitive: parse the five fields and
walk the clock forward one minute at a time until every field matches. This
exercise is an independent module with its own `go mod init`.

## What you'll build

```text
cron/                      independent module: example.com/cron-expression-iterator
  go.mod                    module example.com/cron-expression-iterator
  cron.go                   Schedule, Parse, Next
  cmd/
    demo/
      main.go               runnable demo: "0 9 * * MON-FRI" next 5 fire times
  cron_test.go               weekday skip, dom/dow union, step lists, early-stop, unsatisfiable schedule, parse errors
```

Implement: `Parse(expr string) (*Schedule, error)` for five-field cron expressions (minute, hour, day-of-month, month, day-of-week, with `*`, `*/step`, ranges, lists, and month/weekday names) and `(*Schedule) Next(from time.Time, n int) iter.Seq[time.Time]` yielding the next `n` fire times strictly after `from`.
Test: `"0 9 * * MON-FRI"` from a Friday afternoon skips the weekend to the next Monday; a schedule with both day-of-month and day-of-week restricted matches either field, not their conjunction; `"*/15 9 * * *"` steps within the hour and rolls to the next day; a consumer break stops the search; an unsatisfiable schedule (`"0 0 30 FEB *"`) terminates instead of looping forever; malformed expressions return an error.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/28-cron-expression-iterator/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/28-cron-expression-iterator
go mod edit -go=1.24
```

Two things about this parser are not obvious from the cron syntax itself.
First, the day-of-month/day-of-week interaction is a union, not an
intersection, whenever *both* fields are restricted: `"0 12 1 * MON"` means
"noon on the 1st of the month, OR noon on any Monday," not "noon on any
Monday that happens to be the 1st." Treating all five fields as a plain AND
would make that OR inexpressible and would silently narrow a schedule the
operator wrote to fire far more often into one that almost never fires. The
rule only applies when neither day field is a wildcard -- `"0 12 1 * *"`
means the 1st regardless of weekday, and `"0 12 * * MON"` means every Monday
regardless of day-of-month, because a wildcard field never restricts
anything to begin with. Second, `Next` cannot search forever: an
expression like day-of-month 31 combined with a months field that only ever
names 30-day months can never fire, and a naive `for { if matches { break } }`
loop would spin one minute at a time until the process is killed. Bounding
the search to `maxLookahead` (four years of minutes) turns an unsatisfiable
schedule into an empty result instead of a hang -- exactly the kind of
validation a scheduler wants to run at config-load time, before a bad
schedule ever reaches a deployed job.

Create `cron.go`:

```go
package cron

import (
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"
)

// maxLookahead bounds how far into the future Next will search for a match
// before giving up. Without a bound, a schedule that can never fire (a
// day-of-month field restricted to a day that no month has, like 31 paired
// with a months field that never includes a 31-day month) would make Next
// spin forever, one minute at a time, and never return control to the
// consumer. Four years of minutes is generous enough for every legal cron
// field combination that does eventually fire, including "the 29th of
// February," while still bounding the search.
const maxLookahead = 4 * 366 * 24 * time.Hour / time.Minute

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// Schedule is a parsed five-field cron expression: minute, hour,
// day-of-month, month, day-of-week.
type Schedule struct {
	minutes, hours, doms, months, dows map[int]bool
	domWildcard, dowWildcard           bool
}

// Parse parses a standard five-field cron expression ("minute hour dom
// month dow"). Fields accept "*", "*/step", single values, "a-b" ranges,
// "a-b/step" stepped ranges, and comma-separated lists of any of those.
// Month and day-of-week fields additionally accept three-letter names
// (JAN..DEC, SUN..SAT), case-insensitively.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields (minute hour dom month dow), got %d", len(fields))
	}

	minutes, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("cron: minute field: %w", err)
	}
	hours, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("cron: hour field: %w", err)
	}
	doms, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("cron: day-of-month field: %w", err)
	}
	months, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, fmt.Errorf("cron: month field: %w", err)
	}
	dows, err := parseField(fields[4], 0, 6, dowNames)
	if err != nil {
		return nil, fmt.Errorf("cron: day-of-week field: %w", err)
	}

	return &Schedule{
		minutes:     minutes,
		hours:       hours,
		doms:        doms,
		months:      months,
		dows:        dows,
		domWildcard: fields[2] == "*",
		dowWildcard: fields[4] == "*",
	}, nil
}

// parseField expands one comma-separated cron field into the set of
// integers it selects, resolving names via names (nil for numeric-only
// fields) and rejecting anything outside [min, max].
func parseField(field string, min, max int, names map[string]int) (map[int]bool, error) {
	result := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		spec, step := part, 1
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			spec = part[:idx]
			s, err := strconv.Atoi(part[idx+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step = s
		}

		var lo, hi int
		var err error
		switch {
		case spec == "*":
			lo, hi = min, max
		case strings.Contains(spec, "-"):
			bounds := strings.SplitN(spec, "-", 2)
			lo, err = resolveToken(bounds[0], names)
			if err != nil {
				return nil, err
			}
			hi, err = resolveToken(bounds[1], names)
			if err != nil {
				return nil, err
			}
		default:
			lo, err = resolveToken(spec, names)
			if err != nil {
				return nil, err
			}
			hi = lo
		}

		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("range %q outside [%d,%d]", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			result[v] = true
		}
	}
	return result, nil
}

// resolveToken resolves a single field token to an integer, checking the
// name table (month or day-of-week names) before falling back to a plain
// numeric parse.
func resolveToken(tok string, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToUpper(tok)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("invalid token %q", tok)
	}
	return v, nil
}

// matches reports whether t (truncated to the minute) satisfies every field
// of the schedule. The day-of-month/day-of-week interaction follows
// standard cron semantics, which is the one subtlety that is not a simple
// AND of all five fields: if both fields are restricted (not "*"), a day
// matches when EITHER the day-of-month OR the day-of-week matches, not
// both. Only when exactly one of the two is a wildcard does the other field
// alone constrain the day. Implementing this as a plain AND across all five
// fields would make "day 1 OR every Monday" inexpressible -- it would
// instead require day 1 to also be a Monday, which is not what the fields
// mean independently.
func (s *Schedule) matches(t time.Time) bool {
	if !s.minutes[t.Minute()] || !s.hours[t.Hour()] || !s.months[int(t.Month())] {
		return false
	}
	domMatch := s.doms[t.Day()]
	dowMatch := s.dows[int(t.Weekday())]
	switch {
	case s.domWildcard && s.dowWildcard:
		return true
	case s.domWildcard:
		return dowMatch
	case s.dowWildcard:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

// Next yields the next n fire times strictly after from, minute-truncated,
// in chronological order. The search advances one minute at a time and
// stops after maxLookahead minutes even if fewer than n matches were found,
// which keeps an unsatisfiable schedule (day-of-month 31 paired with a
// months field that names only 30-day months) from hanging the iterator
// forever; a satisfiable schedule always finds all n well before that
// bound.
func (s *Schedule) Next(from time.Time, n int) iter.Seq[time.Time] {
	return func(yield func(time.Time) bool) {
		t := from.Truncate(time.Minute).Add(time.Minute)
		found := 0
		for step := time.Duration(0); step < maxLookahead && found < n; step++ {
			if s.matches(t) {
				found++
				if !yield(t) {
					return
				}
			}
			t = t.Add(time.Minute)
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/cron-expression-iterator"
)

func main() {
	sched, err := cron.Parse("0 9 * * MON-FRI")
	if err != nil {
		log.Fatal(err)
	}

	from := time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC) // a Friday, after 09:00
	for t := range sched.Next(from, 5) {
		fmt.Println(t.Format("2006-01-02 Mon 15:04"))
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
2024-03-04 Mon 09:00
2024-03-05 Tue 09:00
2024-03-06 Wed 09:00
2024-03-07 Thu 09:00
2024-03-08 Fri 09:00
```

`from` is already a Friday past 09:00, so the search skips the rest of that
Friday and the entire weekend and lands on the following Monday, then walks
one weekday at a time through Friday of the next week.

### Tests

Create `cron_test.go`:

```go
package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", expr, err)
	}
	return s
}

func TestNextWeekdayMorningSkipsWeekend(t *testing.T) {
	t.Parallel()

	s := mustParse(t, "0 9 * * MON-FRI")
	from := time.Date(2024, time.March, 1, 10, 0, 0, 0, time.UTC) // Friday, after 09:00

	var got []string
	for fire := range s.Next(from, 5) {
		got = append(got, fire.Format("2006-01-02 Mon"))
	}

	want := []string{
		"2024-03-04 Mon",
		"2024-03-05 Tue",
		"2024-03-06 Wed",
		"2024-03-07 Thu",
		"2024-03-08 Fri",
	}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestNextDomOrDowUnion(t *testing.T) {
	t.Parallel()

	// Day 1 OR every Monday: both fields restricted, so a match is either
	// condition, not their conjunction.
	s := mustParse(t, "0 12 1 * MON")
	from := time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC) // is itself day 1 at noon

	var got []string
	for fire := range s.Next(from, 3) {
		got = append(got, fire.Format("2006-01-02 Mon"))
	}

	want := []string{
		"2024-03-04 Mon", // next Monday
		"2024-03-11 Mon",
		"2024-03-18 Mon",
	}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestNextStepAndList(t *testing.T) {
	t.Parallel()

	s := mustParse(t, "*/15 9 * * *")
	from := time.Date(2024, time.March, 1, 9, 0, 0, 0, time.UTC)

	var got []string
	for fire := range s.Next(from, 4) {
		got = append(got, fire.Format("01-02 15:04"))
	}
	// Hour is restricted to 9, so after 09:45 the next match is 09:00 the
	// following day, not 10:00 the same day.
	want := []string{"03-01 09:15", "03-01 09:30", "03-01 09:45", "03-02 09:00"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestNextStopsEarlyOnBreak(t *testing.T) {
	t.Parallel()

	s := mustParse(t, "0 * * * *")
	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	count := 0
	for range s.Next(from, 100) {
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}

func TestNextUnsatisfiableScheduleTerminates(t *testing.T) {
	t.Parallel()

	// February never has a 30th day in any year, so this schedule can never
	// fire; Next must still return instead of looping forever.
	s := mustParse(t, "0 0 30 FEB *")
	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	count := 0
	for range s.Next(from, 5) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0 for an unsatisfiable schedule", count)
	}
}

func TestParseRejectsWrongFieldCount(t *testing.T) {
	t.Parallel()

	if _, err := Parse("0 9 * *"); err == nil {
		t.Fatal("expected error for a 4-field expression")
	}
}

func TestParseRejectsOutOfRangeValue(t *testing.T) {
	t.Parallel()

	if _, err := Parse("0 24 * * *"); err == nil {
		t.Fatal("expected error for hour 24 (valid range is 0-23)")
	}
}
```

## Review

The correctness that matters here is not the parsing -- field expansion into
a `map[int]bool` is mechanical -- it is `matches` getting the day-field union
right and `Next` refusing to search unboundedly. The common mistake in a
hand-rolled cron evaluator is writing the day check as a single `&&` across
all five fields, which quietly changes the meaning of every schedule that
restricts both day-of-month and day-of-week; the bug only surfaces the first
time an operator writes exactly that kind of schedule, and by then it has
likely already fired far less often than intended. The second common mistake
is trusting that any parsed schedule will eventually match and omitting the
lookahead bound entirely -- fine until a typo produces a field combination
that no calendar can satisfy, and the service validating the schedule hangs
instead of reporting a clear error.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [POSIX crontab specification](https://pubs.opengroup.org/onlinepubs/9699919799/utilities/crontab.html)
- [`time.Time` documentation](https://pkg.go.dev/time#Time)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-bloom-filter-space-efficient-dedup.md](27-bloom-filter-space-efficient-dedup.md) | Next: [29-feature-flag-targeting-evaluator.md](29-feature-flag-targeting-evaluator.md)
