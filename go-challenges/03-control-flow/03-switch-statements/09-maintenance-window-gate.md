# Exercise 9: Gate Requests by Weekday and Hour With a switch-with-init

Operations teams gate writes during maintenance windows: a Sunday full-day freeze
for migrations, a nightly weekday window for index rebuilds. This module builds
that gate as an init-statement switch on the weekday combined with an inner
tagless switch on the hour — and, crucially, takes the current time as a
parameter so the whole thing is hermetically testable.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
window/                    independent module: example.com/maintenance-window-gate
  go.mod                   go 1.24
  window.go                InWindow(now time.Time) (bool, string)
  cmd/
    demo/
      main.go              runnable demo across weekdays and boundary hours
  window_test.go           fixed UTC time.Date table incl. 01:59/02:00/03:59/04:00
```

- Files: `window.go`, `cmd/demo/main.go`, `window_test.go`.
- Implement: `InWindow(now time.Time) (bool, string)` using `switch day := now.Weekday(); day` with an inner tagless switch on `now.Hour()`.
- Test: a table of fixed UTC `time.Date` values across weekdays and boundary hours asserting the in/out decision and the reason string.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/window/cmd/demo
cd ~/go-exercises/window
go mod init example.com/maintenance-window-gate
go mod edit -go=1.24
```

### Inject the clock, and scope the weekday to the switch

The single most important design decision is the signature: `InWindow` takes
`now time.Time` as a parameter instead of calling `time.Now()` internally. That
one choice makes the gate deterministic — a test constructs a fixed
`time.Date(..., time.UTC)` and asserts the exact decision, with no wall clock, no
sleeping, and no flakiness at a window boundary. A function that read `time.Now()`
itself could only be tested by mocking the clock or by running at the right
minute; injecting it is simpler and honest.

The outer branch is a switch-with-init: `switch day := now.Weekday(); day`. This
is the init form doing exactly what it is for — resolve a value (`day`), branch on
it, and keep the name scoped to the switch so it does not leak into the rest of
the function. Sunday is an all-day freeze; Saturday has no window; the `default`
handles the weekdays, where an inner tagless switch on `now.Hour()` encodes the
02:00-04:00 UTC window with the boundary chosen deliberately: `>= 2 && < 4`, so
02:00 is inside and 04:00 is outside. Normalizing to UTC with `now.UTC()` first
means a caller passing a time in any zone is judged against the ops window's UTC
definition, not the caller's local wall clock — a real bug source when a service
runs in one region and the window is defined in another.

The reason string travels alongside the boolean so callers can log *why* a write
was frozen, which is what an on-call engineer needs when a write is unexpectedly
rejected at 02:30 UTC.

Create `window.go`:

```go
package window

import "time"

// InWindow reports whether now falls inside a maintenance / write-freeze window,
// with a human reason. now is a parameter (not time.Now) so the gate is
// deterministic and hermetically testable. All judgments are made in UTC.
func InWindow(now time.Time) (bool, string) {
	now = now.UTC()
	switch day := now.Weekday(); day {
	case time.Sunday:
		return true, "sunday maintenance (all day)"
	case time.Saturday:
		return false, "saturday: no maintenance window"
	default:
		// Weekdays: nightly freeze 02:00-04:00 UTC. 02:00 is in, 04:00 is out.
		switch h := now.Hour(); {
		case h >= 2 && h < 4:
			return true, "weekday write-freeze window (02:00-04:00 UTC)"
		default:
			return false, "outside write-freeze window"
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/maintenance-window-gate"
)

func main() {
	times := []time.Time{
		time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC), // Sunday
		time.Date(2026, 7, 4, 3, 0, 0, 0, time.UTC),  // Saturday
		time.Date(2026, 7, 1, 1, 59, 0, 0, time.UTC), // Wednesday, before window
		time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC),  // Wednesday, window start
		time.Date(2026, 7, 1, 3, 59, 0, 0, time.UTC), // Wednesday, still in window
		time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),  // Wednesday, window end
	}
	for _, ts := range times {
		frozen, reason := window.InWindow(ts)
		fmt.Printf("%s %-6v %s\n", ts.Format("Mon 15:04"), frozen, reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Sun 12:00 true   sunday maintenance (all day)
Sat 03:00 false  saturday: no maintenance window
Wed 01:59 false  outside write-freeze window
Wed 02:00 true   weekday write-freeze window (02:00-04:00 UTC)
Wed 03:59 true   weekday write-freeze window (02:00-04:00 UTC)
Wed 04:00 false  outside write-freeze window
```

### Tests

`TestInWindow` uses fixed UTC `time.Date` values so the assertions are exact and
hermetic. The boundary rows — 01:59, 02:00, 03:59, 04:00 on a weekday — pin the
half-open window `[02:00, 04:00)`, which is where an off-by-one hour bug would
hide. `TestInWindowNormalizesZone` passes a non-UTC time and confirms the decision
is made in UTC.

Create `window_test.go`:

```go
package window

import (
	"testing"
	"time"
)

func TestInWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		now        time.Time
		wantFrozen bool
		wantReason string
	}{
		{"sunday noon", time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC), true, "sunday maintenance (all day)"},
		{"saturday in-hours", time.Date(2026, 7, 4, 3, 0, 0, 0, time.UTC), false, "saturday: no maintenance window"},
		{"weekday before", time.Date(2026, 7, 1, 1, 59, 0, 0, time.UTC), false, "outside write-freeze window"},
		{"weekday start", time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC), true, "weekday write-freeze window (02:00-04:00 UTC)"},
		{"weekday late", time.Date(2026, 7, 1, 3, 59, 0, 0, time.UTC), true, "weekday write-freeze window (02:00-04:00 UTC)"},
		{"weekday end", time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC), false, "outside write-freeze window"},
		{"weekday noon", time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), false, "outside write-freeze window"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotFrozen, gotReason := InWindow(tc.now)
			if gotFrozen != tc.wantFrozen || gotReason != tc.wantReason {
				t.Fatalf("InWindow(%s) = (%v, %q), want (%v, %q)",
					tc.now.Format(time.RFC3339), gotFrozen, gotReason, tc.wantFrozen, tc.wantReason)
			}
		})
	}
}

func TestInWindowNormalizesZone(t *testing.T) {
	t.Parallel()

	// 03:00 in a +05:00 zone is 22:00 UTC the previous day (a Tuesday), which is
	// outside the window. The gate must judge in UTC, not the caller's zone.
	zone := time.FixedZone("plus5", 5*60*60)
	local := time.Date(2026, 7, 1, 3, 0, 0, 0, zone)
	if frozen, reason := InWindow(local); frozen {
		t.Fatalf("InWindow(%s) = frozen (%q), want not frozen after UTC normalization",
			local.Format(time.RFC3339), reason)
	}
}
```

## Review

The gate is correct when the decision depends only on the injected `now`,
evaluated in UTC, and the window boundaries are exactly `[02:00, 04:00)` on
weekdays with Sunday fully frozen. Taking `now` as a parameter is the design move
that makes the boundary rows testable at all — a function reading `time.Now()`
could never assert 02:00-is-in and 04:00-is-out deterministically. The init-form
outer switch scopes `day` to the branch, and the inner tagless switch keeps the
hour-range logic where it belongs. `TestInWindowNormalizesZone` guards the
easy-to-miss requirement that the window is defined in UTC regardless of the
caller's zone.

## Resources

- [time.Weekday](https://pkg.go.dev/time#Weekday) — the `Sunday..Saturday` constants and `Time.Weekday`.
- [time.Time.Hour and time.Time.UTC](https://pkg.go.dev/time#Time.Hour) — hour-of-day and zone normalization.
- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the init-statement (`switch x := f(); x`) form.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-slug-validator.md](08-slug-validator.md) | Next: [10-rate-limit-tier-resolver.md](10-rate-limit-tier-resolver.md)
