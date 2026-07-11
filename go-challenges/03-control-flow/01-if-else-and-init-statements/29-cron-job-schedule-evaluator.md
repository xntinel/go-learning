# Exercise 29: Cron Schedule Evaluator: Decide Job Execution from Cron Expression

**Nivel: Intermedio** — validacion rapida (un test corto).

Not every scheduled job needs a full cron daemon running alongside the
service: a job runner that already wakes up once a minute (or is invoked by
an external scheduler on that cadence) only needs to answer one question
correctly — given the current timestamp, should this particular job run
right now? This module parses a standard 5-field cron expression and
evaluates it against a timestamp with a short-circuiting chain of field
checks, with zero dependency on a background ticker or an external cron
binary. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
cron/                        independent module: example.com/cron-job-schedule-evaluator
  go.mod                    go 1.24
  cron.go                   Parse(expr), Schedule.ShouldRun(t)
  cmd/
    demo/
      main.go               a weekday 9am job evaluated across four timestamps
  cron_test.go              Parse error table; ShouldRun table over minute/day/month/weekday fields
```

- Files: `cron.go`, `cmd/demo/main.go`, `cron_test.go`.
- Implement: `Parse(expr string) (Schedule, error)` parsing five space-separated fields (minute, hour, day, month, weekday), each `"*"` or a comma-separated integer list, and `Schedule.ShouldRun(t time.Time) bool` checking each field in order and returning false at the first mismatch.
- Test: a table of `Parse` errors (wrong field count, non-numeric value, out-of-range value), and a table of `ShouldRun` results across a business-hours weekday schedule, a wildcard schedule, and specific day-of-month and month fields.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cron/cmd/demo
cd ~/go-exercises/cron
go mod init example.com/cron-job-schedule-evaluator
go mod edit -go=1.24
```

### Why a wildcard is nil, not a sentinel value

`parseField` represents `"*"` as a `nil` slice, not as an empty slice or a
sentinel like `-1`. That choice removes an entire category of bug: a cron
field for minute `0` (a real, valid, common value — "run on the hour") must
never be confused with "any minute matches." If wildcards were represented
by a magic number, `0` colliding with that sentinel would be exactly the
kind of subtle, rarely-triggered bug that only shows up once someone writes
a schedule for the top of the hour. Representing "matches anything"
structurally, as the absence of a list rather than as a specific value
inside one, makes that confusion impossible by construction: `matches` only
has one thing to check — is `field` `nil`, or does the value appear in it —
and there is no number that could ever be mistaken for the wildcard.

Create `cron.go`:

```go
// Package cron parses a 5-field cron expression and decides, for a given
// timestamp, whether the job it describes should run — without a cron
// daemon, a background ticker, or any dependency beyond the standard
// library.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression. Each field is either nil, meaning
// "any value matches" (the "*" wildcard), or a sorted list of the specific
// values that match.
type Schedule struct {
	minute  []int // 0-59
	hour    []int // 0-23
	day     []int // 1-31
	month   []int // 1-12
	weekday []int // 0-6, Sunday = 0
}

// Parse reads a standard 5-field cron expression: minute hour day month
// weekday. Each field is "*" or a comma-separated list of integers.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron: expected 5 fields, got %d in %q", len(fields), expr)
	}

	minute, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: minute field: %w", err)
	}
	hour, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: hour field: %w", err)
	}
	day, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: day field: %w", err)
	}
	month, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: month field: %w", err)
	}
	weekday, err := parseField(fields[4], 0, 6)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: weekday field: %w", err)
	}

	return Schedule{minute: minute, hour: hour, day: day, month: month, weekday: weekday}, nil
}

// parseField parses one cron field. A bare "*" returns a nil slice, meaning
// "matches anything" — a real value of 0 is never confused with "no
// constraint" because the two are represented differently (nil versus a
// slice containing 0), not by a sentinel number.
func parseField(field string, min, max int) ([]int, error) {
	if field == "*" {
		return nil, nil
	}

	parts := strings.Split(field, ",")
	values := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q: %w", p, err)
		}
		if n < min || n > max {
			return nil, fmt.Errorf("value %d out of range [%d, %d]", n, min, max)
		}
		values = append(values, n)
	}
	return values, nil
}

// matches reports whether v satisfies field, where a nil field matches any
// value.
func matches(field []int, v int) bool {
	if field == nil {
		return true
	}
	for _, x := range field {
		if x == v {
			return true
		}
	}
	return false
}

// ShouldRun reports whether the job should run at t. Each field is checked
// in turn and the chain short-circuits on the first mismatch: there is no
// reason to check month and weekday once the minute itself already fails to
// match, so a schedule evaluated once a minute for years never does more
// than five cheap comparisons on the common "not this minute" path.
func (s Schedule) ShouldRun(t time.Time) bool {
	if !matches(s.minute, t.Minute()) {
		return false
	}
	if !matches(s.hour, t.Hour()) {
		return false
	}
	if !matches(s.day, t.Day()) {
		return false
	}
	if !matches(s.month, int(t.Month())) {
		return false
	}
	if !matches(s.weekday, int(t.Weekday())) {
		return false
	}
	return true
}
```

### The runnable demo

A business-hours job scheduled for 9:00 AM on weekdays is evaluated against
four timestamps: Monday at 9:00 (runs), Monday at 9:01 (does not, wrong
minute), Wednesday at 9:00 (runs), and Saturday at 9:00 (does not, wrong
weekday). A final call demonstrates the field-count parse error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	cron "example.com/cron-job-schedule-evaluator"
)

func main() {
	// "0 9 * * 1,2,3,4,5" — 9:00 AM on weekdays (Mon=1 ... Fri=5), a typical
	// business-hours batch job.
	sched, err := cron.Parse("0 9 * * 1,2,3,4,5")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}

	// 2024-01-01 is a Monday.
	monday9am := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	checks := []struct {
		label string
		t     time.Time
	}{
		{"Monday 09:00", monday9am},
		{"Monday 09:01", monday9am.Add(time.Minute)},
		{"Wednesday 09:00", monday9am.AddDate(0, 0, 2)},
		{"Saturday 09:00", monday9am.AddDate(0, 0, 5)},
	}

	for _, c := range checks {
		fmt.Printf("%-16s %s -> shouldRun=%v\n", c.label, c.t.Format("2006-01-02 15:04 Mon"), sched.ShouldRun(c.t))
	}

	if _, err := cron.Parse("0 9 * *"); err != nil {
		fmt.Println("expected parse error for wrong field count:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Monday 09:00     2024-01-01 09:00 Mon -> shouldRun=true
Monday 09:01     2024-01-01 09:01 Mon -> shouldRun=false
Wednesday 09:00  2024-01-03 09:00 Wed -> shouldRun=true
Saturday 09:00   2024-01-06 09:00 Sat -> shouldRun=false
expected parse error for wrong field count: cron: expected 5 fields, got 4 in "0 9 * *"
```

### Tests

The first table drives `Parse` through five distinct error conditions. The
second drives `ShouldRun` across a weekday business-hours schedule, a full
wildcard schedule, and schedules keyed on day-of-month and month
specifically, so each field's guard is exercised independently.

Create `cron_test.go`:

```go
package cron

import (
	"testing"
	"time"
)

func TestParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		expr string
	}{
		{name: "too few fields", expr: "0 9 * *"},
		{name: "too many fields", expr: "0 9 * * * *"},
		{name: "non-numeric value", expr: "abc 9 * * *"},
		{name: "minute out of range", expr: "60 9 * * *"},
		{name: "weekday out of range", expr: "0 9 * * 7"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tc.expr); err == nil {
				t.Errorf("Parse(%q) = nil error, want an error", tc.expr)
			}
		})
	}
}

func TestShouldRun(t *testing.T) {
	t.Parallel()

	// 2024-01-01 is a Monday.
	monday9am := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		expr string
		t    time.Time
		want bool
	}{
		{
			name: "weekday business-hours job runs at 9am Monday",
			expr: "0 9 * * 1,2,3,4,5",
			t:    monday9am,
			want: true,
		},
		{
			name: "wrong minute does not run",
			expr: "0 9 * * 1,2,3,4,5",
			t:    monday9am.Add(time.Minute),
			want: false,
		},
		{
			name: "runs on Wednesday too",
			expr: "0 9 * * 1,2,3,4,5",
			t:    monday9am.AddDate(0, 0, 2),
			want: true,
		},
		{
			name: "does not run on Saturday",
			expr: "0 9 * * 1,2,3,4,5",
			t:    monday9am.AddDate(0, 0, 5),
			want: false,
		},
		{
			name: "wildcard fields match every value",
			expr: "* * * * *",
			t:    monday9am.Add(37 * time.Minute),
			want: true,
		},
		{
			name: "specific day-of-month field",
			expr: "0 0 1 * *",
			t:    time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "specific day-of-month field, wrong day",
			expr: "0 0 1 * *",
			t:    time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "specific month field, wrong month",
			expr: "0 0 * 6 *",
			t:    time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sched, err := Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tc.expr, err)
			}
			if got := sched.ShouldRun(tc.t); got != tc.want {
				t.Errorf("ShouldRun(%v) with expr %q = %v, want %v", tc.t, tc.expr, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`ShouldRun` checks the fields in the fixed order minute, hour, day, month,
weekday — the same order they appear in the expression — which matters
because it is also, in practice, the cheapest-to-most-expensive order for
the common "doesn't match" case: a job polled once a minute fails the minute
check on 59 out of every 60 evaluations, so the chain almost always exits
after a single comparison. Carry this forward: when a guard chain checks
several independent fields, order the checks so the cheapest, most-likely-
to-fail one runs first, and let each guard return immediately rather than
accumulating a "matches so far" boolean through the whole chain.

## Resources

- [crontab(5) man page](https://man7.org/linux/man-pages/man5/crontab.5.html) — the field format this module's parser mirrors a subset of.
- [time.Weekday](https://pkg.go.dev/time#Weekday) — confirms Sunday is 0, matching the standard cron weekday convention this module relies on.
- [Kubernetes CronJob](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/) — a production scheduler built on this exact cron expression format.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-distributed-trace-span-propagation.md](28-distributed-trace-span-propagation.md) | Next: [30-atomic-config-reload-zero-downtime.md](30-atomic-config-reload-zero-downtime.md)
