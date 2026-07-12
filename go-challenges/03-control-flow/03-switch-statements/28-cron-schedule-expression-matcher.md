# Exercise 28: Validate If a Timestamp Matches a Cron-Like Schedule

**Nivel: Intermedio** — validacion rapida (un test corto).

A job scheduler that supports cron-style expressions has to answer one
question, over and over, for every job on every tick: does this job's
schedule fire at this exact minute? Pulling in a third-party cron library
just to answer that is often overkill for a scheduler that already owns
its own tick loop and persistence, and understanding the parsing by hand
is what makes debugging a misfiring schedule tractable at 3am. This module
parses the five classic cron fields and evaluates them against a
`time.Time`, dispatching on each field's *syntactic shape* — wildcard,
step, range, list, or literal — with a tagless switch. It is self-contained:
its own `go mod init`, code, demo, and test.

## What you'll build

```text
cronmatch/                 independent module: example.com/cron-schedule-expression-matcher
  go.mod                    go 1.24
  cronmatch.go               package cronmatch; Schedule; Parse(expr) (Schedule, error); (Schedule) Matches(t) (bool, error)
  cmd/demo/main.go           runnable demo over a business-hours weekday schedule
  cronmatch_test.go          table over the boundary of every field type, plus malformed-field errors
```

- Implement: `Parse(expr string) (Schedule, error)` and `(s Schedule) Matches(t time.Time) (bool, error)` — `matchField` is a tagless switch dispatching on field shape (`*`, `*/n`, `a-b`, `a,b,c`, or a bare number).
- Test: a table crossing every field against a fixed timestamp at its exact matching and non-matching boundary, an invalid field count, and an out-of-range range bound.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two switches, dispatching on two different things

This exercise deliberately uses a switch for two unrelated jobs, and
conflating them is the mistake to avoid. `matchField`'s switch dispatches
on the field's *syntax* — is this text a wildcard, a step, a range, a
list, or a literal — because each shape needs an entirely different
parsing routine; there is no shared numeric comparison across the five
shapes, only a shared *return type*. `Matches` itself doesn't switch at
all; it loops over five `(field, value, max)` triples and delegates each
one to `matchField`, because the *decision* — does this timestamp's
minute/hour/day/month/weekday satisfy its field — is identical regardless
of which of the five fields is being checked, and duplicating that loop
five times would be the "share a body" case the concepts file addresses
with a helper, not a chain of near-identical `if` statements.

The order inside `matchField`'s tagless switch matters for a subtle
reason: `strings.Contains(field, "-")` must be checked before the bare
`strconv.Atoi(field)` fallback, and `strings.HasPrefix(field, "*/")` must
be checked before the plain `"*"` case is reached — but since `"*/15"`
does not equal `"*"`, and a literal number never contains a hyphen or a
comma, none of these five shapes can actually collide for valid input.
The order is defensive rather than load-bearing here: it reads top to
bottom as "most specific wildcard first, most general literal last,"
which is the same judgment call the concepts file asks you to make
explicit whenever cases could overlap, even when — as here — careful
construction means they never actually do.

Create `cronmatch.go`:

```go
// Package cronmatch evaluates a five-field cron expression against a
// timestamp without pulling in a third-party cron library -- the kind of
// thing a scheduler service needs when it has to answer "does this job's
// schedule fire at this minute?" for thousands of jobs on every tick, and
// shelling out to a parser library per tick is not an option.
package cronmatch

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression: minute, hour,
// day-of-month, month, and day-of-week, each still stored as its raw field
// text. Parsing eagerly here would mean picking one internal representation
// for "*", "*/15", "1-5", and "1,3,5" all at once; instead each field keeps
// its original syntax and Matches re-interprets it per field, which is
// exactly the class of decision a switch is for.
type Schedule struct {
	minute, hour, day, month, weekday string
}

// Parse splits a standard five-field cron expression ("minute hour
// day-of-month month day-of-week") into a Schedule. It validates field
// count only; individual field syntax is validated lazily by Matches, the
// same way a real cron daemon defers pattern validation to evaluation time.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cronmatch: expected 5 fields, got %d in %q", len(fields), expr)
	}
	return Schedule{
		minute:  fields[0],
		hour:    fields[1],
		day:     fields[2],
		month:   fields[3],
		weekday: fields[4],
	}, nil
}

// Matches reports whether t falls within every field of s. All five fields
// must match; cron's classic day-of-month/day-of-week OR-quirk is
// deliberately not implemented here to keep the exercise's semantics
// unambiguous -- every production cron parser has to make this same
// documented choice explicitly rather than let it be an accident.
func (s Schedule) Matches(t time.Time) (bool, error) {
	checks := []struct {
		field string
		value int
		max   int
	}{
		{s.minute, t.Minute(), 59},
		{s.hour, t.Hour(), 23},
		{s.day, t.Day(), 31},
		{s.month, int(t.Month()), 12},
		{s.weekday, int(t.Weekday()), 6},
	}

	for _, c := range checks {
		ok, err := matchField(c.field, c.value, c.max)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// matchField decides whether value satisfies one cron field's pattern. The
// switch dispatches on the field's *shape* (wildcard, step, range, list, or
// a bare literal) rather than its value, because each shape needs entirely
// different parsing logic -- this is a tagless switch used for syntactic
// classification, not numeric range checks.
func matchField(field string, value, max int) (bool, error) {
	switch {
	case field == "*":
		return true, nil

	case strings.HasPrefix(field, "*/"):
		step, err := strconv.Atoi(field[2:])
		if err != nil || step <= 0 {
			return false, fmt.Errorf("cronmatch: invalid step field %q", field)
		}
		return value%step == 0, nil

	case strings.Contains(field, "-"):
		lo, hi, err := parseRange(field, max)
		if err != nil {
			return false, err
		}
		return value >= lo && value <= hi, nil

	case strings.Contains(field, ","):
		for _, part := range strings.Split(field, ",") {
			n, err := strconv.Atoi(part)
			if err != nil {
				return false, fmt.Errorf("cronmatch: invalid list entry %q in %q", part, field)
			}
			if n == value {
				return true, nil
			}
		}
		return false, nil

	default:
		n, err := strconv.Atoi(field)
		if err != nil {
			return false, fmt.Errorf("cronmatch: invalid field %q", field)
		}
		return n == value, nil
	}
}

// parseRange parses "a-b" into its two bounds, rejecting a range whose
// upper bound is out of the field's valid domain -- a malformed expression
// like "9-99" in the hour field should fail loudly at match time, not
// silently accept every hour because 99 is never less than it.
func parseRange(field string, max int) (lo, hi int, err error) {
	parts := strings.SplitN(field, "-", 2)
	lo, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("cronmatch: invalid range start in %q", field)
	}
	hi, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("cronmatch: invalid range end in %q", field)
	}
	if hi > max || lo > hi {
		return 0, 0, fmt.Errorf("cronmatch: range %q out of bounds for max %d", field, max)
	}
	return lo, hi, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	cronmatch "example.com/cron-schedule-expression-matcher"
)

func main() {
	// "every 15 minutes, business hours 9-17, weekdays only"
	sched, err := cronmatch.Parse("*/15 9-17 * * 1-5")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}

	times := []time.Time{
		time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC),   // Monday 09:00 -- matches
		time.Date(2024, 3, 4, 9, 5, 0, 0, time.UTC),   // Monday 09:05 -- wrong minute
		time.Date(2024, 3, 4, 18, 0, 0, 0, time.UTC),  // Monday 18:00 -- after hours
		time.Date(2024, 3, 3, 9, 0, 0, 0, time.UTC),   // Sunday 09:00 -- weekend
		time.Date(2024, 3, 8, 17, 45, 0, 0, time.UTC), // Friday 17:45 -- matches
	}

	for _, t := range times {
		ok, err := sched.Matches(t)
		if err != nil {
			fmt.Printf("%s -> error: %v\n", t.Format(time.RFC3339), err)
			continue
		}
		fmt.Printf("%s (%s) -> %v\n", t.Format(time.RFC3339), t.Weekday(), ok)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
2024-03-04T09:00:00Z (Monday) -> true
2024-03-04T09:05:00Z (Monday) -> false
2024-03-04T18:00:00Z (Monday) -> false
2024-03-03T09:00:00Z (Sunday) -> false
2024-03-08T17:45:00Z (Friday) -> true
```

### Tests

`TestMatches` runs a table over the exact schedule from the demo, hitting
the step boundary, the business-hours boundary, and the weekday boundary
directly. `TestParseRejectsWrongFieldCount` and
`TestMatchesRejectsOutOfRangeField` check the two error paths: a
malformed expression and a range whose upper bound exceeds the field's
valid domain.

Create `cronmatch_test.go`:

```go
package cronmatch

import (
	"testing"
	"time"
)

func TestMatches(t *testing.T) {
	t.Parallel()

	sched, err := Parse("*/15 9-17 * * 1-5")
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"on the step, start of business hours, a weekday", time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC), true},
		{"off the 15-minute step", time.Date(2024, 3, 4, 9, 5, 0, 0, time.UTC), false},
		{"after business hours", time.Date(2024, 3, 4, 18, 0, 0, 0, time.UTC), false},
		{"within business hours but a Sunday", time.Date(2024, 3, 3, 9, 0, 0, 0, time.UTC), false},
		{"last valid minute of the last valid hour, a Friday", time.Date(2024, 3, 8, 17, 45, 0, 0, time.UTC), true},
	}

	for _, tc := range tests {
		got, err := sched.Matches(tc.at)
		if err != nil {
			t.Errorf("%s: Matches() unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Matches(%s) = %v, want %v", tc.name, tc.at.Format(time.RFC3339), got, tc.want)
		}
	}
}

func TestParseRejectsWrongFieldCount(t *testing.T) {
	t.Parallel()

	if _, err := Parse("* * * *"); err == nil {
		t.Fatal("Parse() with 4 fields, want error")
	}
}

func TestMatchesRejectsOutOfRangeField(t *testing.T) {
	t.Parallel()

	sched, err := Parse("0 9-99 * * *")
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if _, err := sched.Matches(time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("Matches() with out-of-range hour bound, want error")
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The matcher is correct when every one of the five fields must independently
match for `Matches` to return true, when a step field like `*/15` checks
divisibility rather than a fixed set of literal values, and when a
malformed or out-of-range field fails loudly with an error instead of
silently matching everything or nothing. Carry this forward: when a
function has to interpret several different *shapes* of the same kind of
input (a config field, a query parameter, a cron field), a tagless switch
dispatching on the input's shape — checked most-specific-first — is the
natural replacement for a chain of `if/else if` string checks, and it
documents the closed set of shapes the parser actually understands.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [POSIX crontab format](https://pubs.opengroup.org/onlinepubs/9699919799/utilities/crontab.html) — the field syntax this exercise parses a subset of.
- [strconv package](https://pkg.go.dev/strconv) — parsing the numeric literals inside each field.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-load-shedding-admission-controller.md](27-load-shedding-admission-controller.md) | Next: [29-feature-flag-rule-evaluator.md](29-feature-flag-rule-evaluator.md)
