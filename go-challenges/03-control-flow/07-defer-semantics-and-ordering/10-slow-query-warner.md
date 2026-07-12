# Exercise 10: Slow-Query Warner — the `defer track()()` Idiom

**Nivel: Intermedio** — validacion rapida (un test corto).

Every "this query took 1.2s" log line in a production system comes from the
same idiom: `defer track()()`. A function starts a timer on entry, and a
closure returned by that same call reports the elapsed time on exit. This
module builds that idiom as a small, reusable `Track` helper and proves it
with an injectable clock, so the test runs in milliseconds instead of
sleeping through real thresholds.

## What you'll build

```text
track/                     independent module: example.com/slow-query-warner
  go.mod                    go 1.24
  track.go                  Track(name, threshold, warn) func(); TrackClock(..., now) func()
  track_test.go             table test over a fake clock: warns at/over threshold, not under
```

- Files: `track.go`, `track_test.go`.
- Implement: `Track(name string, threshold time.Duration, warn func(string, time.Duration)) func()` and its injectable-clock twin `TrackClock(name string, threshold time.Duration, warn func(string, time.Duration), now func() time.Time) func()`.
- Test: a fake clock closure that advances a manual time cursor; assert `warn` fires with the exact elapsed duration when it is at or over threshold, and does not fire when under.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The double call, and why the second `()` is not optional

The usage this idiom exists for:

```go
func fetchOrders(db *sql.DB) ([]Order, error) {
	defer Track("SELECT orders", 100*time.Millisecond, logWarn)()
	// ... run the query ...
}
```

Read that line as two separate calls happening at two separate times.
`Track("SELECT orders", 100*time.Millisecond, logWarn)` runs *immediately*,
right where `defer` sits in the source — it captures `time.Now()` as the
start instant and returns a closure. The trailing `()` is what gets
deferred: that returned closure, and only that closure, runs when
`fetchOrders` exits. It computes `elapsed` and calls `warn` if the query was
slow enough to matter.

Drop the second `()` — `defer Track("SELECT orders", 100*time.Millisecond, logWarn)`
— and Go defers `Track` itself. `Track` now runs at function exit instead of
at entry, so it captures "now" as both the start and end of the interval it
was supposed to measure. The elapsed time collapses to zero, `warn` never
fires because zero never crosses any real threshold, and the whole warner
goes silent without a single compiler error. This is a defer argument-eval
trap with a twist: it is not that an argument is evaluated too early, it is
that the *wrong function entirely* gets deferred.

### The injectable clock

`time.Now()` makes a warner correct in production and untestable without
sleeping. `TrackClock` takes `now func() time.Time` as an explicit
dependency; `Track` is a one-line wrapper that supplies `time.Now`. The test
below supplies a fake clock instead — a closure over a mutable cursor that
the test advances by hand — so asserting "the elapsed time was exactly 250ms"
never touches a real timer or a `time.Sleep`.

Create `track.go`:

```go
package track

import "time"

// TrackClock is the injectable-clock variant used by Track. It records the
// start time via now(), then returns a closure that computes elapsed at call
// time and invokes warn only if elapsed >= threshold.
func TrackClock(name string, threshold time.Duration, warn func(string, time.Duration), now func() time.Time) func() {
	start := now()
	return func() {
		elapsed := now().Sub(start)
		if elapsed >= threshold {
			warn(name, elapsed)
		}
	}
}

// Track records the current time when called and returns a closure that,
// when invoked, reports to warn if the elapsed time since Track was called
// is at least threshold.
//
// Usage: defer Track("SELECT orders", 100*time.Millisecond, logWarn)()
//
// Track runs immediately, capturing the start time. The value it returns is
// the closure that actually gets deferred; that closure runs at function
// exit and measures the elapsed time. Writing "defer Track(...)" without the
// trailing "()" defers Track itself instead of its result — Track would run
// at function exit instead of at the top, so start and "now" collapse to the
// same instant and nothing is ever measured as slow.
func Track(name string, threshold time.Duration, warn func(string, time.Duration)) func() {
	return TrackClock(name, threshold, warn, time.Now)
}
```

### Test

`TestTrackClock` drives `TrackClock` with a fake clock: `now` returns a
`cursor` variable the test advances by hand between starting the timer and
calling the returned closure. Three cases cover the boundary the whole
exercise hinges on — under threshold, exactly at threshold, and over
threshold — checking both whether `warn` fired and, when it did, that the
elapsed duration it received is exact.

Create `track_test.go`:

```go
package track

import (
	"testing"
	"time"
)

func TestTrackClock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		threshold   time.Duration
		advance     time.Duration
		wantWarn    bool
		wantElapsed time.Duration
	}{
		{"under threshold does not warn", 100 * time.Millisecond, 50 * time.Millisecond, false, 0},
		{"exactly at threshold warns", 100 * time.Millisecond, 100 * time.Millisecond, true, 100 * time.Millisecond},
		{"over threshold warns", 100 * time.Millisecond, 250 * time.Millisecond, true, 250 * time.Millisecond},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cursor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return cursor }

			var gotWarn bool
			var gotName string
			var gotElapsed time.Duration
			warn := func(name string, elapsed time.Duration) {
				gotWarn = true
				gotName = name
				gotElapsed = elapsed
			}

			stop := TrackClock("SELECT orders", tc.threshold, warn, now)
			cursor = cursor.Add(tc.advance)
			stop()

			if gotWarn != tc.wantWarn {
				t.Fatalf("warn fired = %v, want %v", gotWarn, tc.wantWarn)
			}
			if tc.wantWarn {
				if gotName != "SELECT orders" {
					t.Errorf("warn name = %q, want %q", gotName, "SELECT orders")
				}
				if gotElapsed != tc.wantElapsed {
					t.Errorf("warn elapsed = %v, want %v", gotElapsed, tc.wantElapsed)
				}
			}
		})
	}
}
```

## Review

`Track` is correct when the two halves of the idiom run at the times they are
supposed to: setup at entry, report at exit. The fake-clock test pins that
without a single real-time wait — the cursor stands in for the clock, so
"250ms elapsed" is an assertion about arithmetic, not about wall time. The
design point to carry forward: any `defer f()()` idiom is really two
functions with two different lifetimes, and the moment that stops being
obvious in the code is the moment someone drops the trailing `()` and breaks
it silently.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — arguments to a deferred call are evaluated when the `defer` statement executes.
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — the canonical cleanup idiom this pattern extends.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-graceful-shutdown-defer-stack.md](09-graceful-shutdown-defer-stack.md) | Next: [11-correlation-id-defer-argument-capture.md](11-correlation-id-defer-argument-capture.md)
