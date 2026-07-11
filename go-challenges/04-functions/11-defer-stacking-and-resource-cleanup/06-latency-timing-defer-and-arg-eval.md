# Exercise 6: Handler Latency Timer — defer Closures vs the Argument-Evaluation Trap

`defer record(time.Since(start))` records a near-zero duration, always. The
arguments to a deferred call are evaluated at the `defer` statement, not when it
runs, so `time.Since(start)` is frozen the instant you write it — before any work
happens. This module builds a latency-and-outcome middleware the correct way,
with a deferred closure that reads live values at return, and demonstrates the
trap with a side-by-side buggy version measured against a fake clock.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
latency/                    independent module: example.com/latency
  go.mod
  latency/latency.go         Timed (closure form), TimedBuggy (arg-eval trap), statusForError
  cmd/demo/main.go           time a healthy and a not-found handler
  latency/latency_test.go    fake clock: closure records real duration; buggy records ~0; status from err
```

- Files: `latency/latency.go`, `cmd/demo/main.go`, `latency/latency_test.go`.
- Implement: `Timed(now, route, record, work)` that captures `start := now()` and defers a closure recording `now().Sub(start)` and the status mapped from the named return error; and `TimedBuggy`, the arg-eval form, kept to demonstrate the trap.
- Test: with an injected fake clock, work that advances the clock a fixed amount is recorded exactly by `Timed`; `TimedBuggy` records ~0 for the same work; an error in the body is recorded as the mapped status because the closure reads `err` at return time.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/latency/latency ~/go-exercises/latency/cmd/demo
cd ~/go-exercises/latency
go mod init example.com/latency
```

### The rule, stated precisely

When Go executes a `defer` statement, it evaluates the deferred function's value,
its receiver, and every argument *right then*, and stores those values. The call
itself is what waits until return. So

```go
start := now()
defer record(now().Sub(start))
```

calls `now()` at the `defer` statement — before `work()` runs — so `now().Sub(start)`
is essentially zero, and that zero is what gets recorded no matter how long the
handler actually takes. This is not a subtle edge case; it is the most common
`defer` bug in real code, and it produces a metric that is silently, uniformly
wrong: every request looks instant.

The fix is to defer a *closure* with no arguments. A closure captures `start`,
`now`, and the named return `err` by reference and runs its body at return time,
so it observes their final values — the real elapsed time and the real outcome:

```go
start := now()
defer func() {
	record(Metric{Route: route, Duration: now().Sub(start), Status: statusForError(err)})
}()
```

The second thing the closure buys you is reading the *outcome*. The status code
is decided by whatever the handler returns, which is not known at the `defer`
statement. Because the closure reads the named return `err` at return time, it can
map that final error to a status — 200, 404, 500 — and record it. An argument
form could not; it would have to freeze a status before the work even started.

To test this deterministically without sleeping, the clock is injected as a
`now func() time.Time`. The test's work function advances a fake clock by a fixed
amount, so `now().Sub(start)` in the closure is an exact, asserted duration.

Create `latency/latency.go`:

```go
package latency

import (
	"errors"
	"net/http"
	"time"
)

// ErrNotFound is a sentinel the caller wraps to signal a 404 outcome.
var ErrNotFound = errors.New("not found")

// Clock returns the current time. Injecting it makes latency deterministic in
// tests; production passes time.Now.
type Clock func() time.Time

// Metric is one recorded observation.
type Metric struct {
	Route    string
	Duration time.Duration
	Status   int
}

// statusForError maps a handler's returned error to an HTTP status.
func statusForError(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// Timed runs work and records its latency and outcome. The deferred CLOSURE reads
// now() and the named return err at return time, so it captures the real elapsed
// time and the real status.
func Timed(now Clock, route string, record func(Metric), work func() error) (err error) {
	start := now()
	defer func() {
		record(Metric{
			Route:    route,
			Duration: now().Sub(start),
			Status:   statusForError(err),
		})
	}()
	return work()
}

// TimedBuggy demonstrates the argument-evaluation trap. now().Sub(start) is
// evaluated at the defer STATEMENT, before work runs, so it freezes a near-zero
// duration and can never see the real outcome. Do not ship this shape.
func TimedBuggy(now Clock, route string, record func(Metric), work func() error) error {
	start := now()
	defer record(Metric{
		Route:    route,
		Duration: now().Sub(start), // frozen at ~0 here, not at return
		Status:   http.StatusOK,    // frozen before work; can never be the real status
	})
	return work()
}
```

### The runnable demo

The demo uses the real `time.Now`, so it prints route and status (deterministic)
but not the wall-clock duration (which varies run to run).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/latency/latency"
)

func main() {
	record := func(m latency.Metric) {
		fmt.Printf("route=%s status=%d\n", m.Route, m.Status)
	}

	_ = latency.Timed(time.Now, "/health", record, func() error {
		return nil
	})
	_ = latency.Timed(time.Now, "/orders/42", record, func() error {
		return fmt.Errorf("lookup: %w", latency.ErrNotFound)
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
route=/health status=200
route=/orders/42 status=404
```

### Tests

A fake clock makes latency exact. The work function advances it a fixed amount, so
`Timed`'s closure records precisely that; `TimedBuggy`'s frozen argument records
zero for the identical work. The third test proves the closure reads the outcome
at return time.

Create `latency/latency_test.go`:

```go
package latency

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func base() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestTimedRecordsRealDuration(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: base()}
	var got Metric
	err := Timed(clk.Now, "/orders", func(m Metric) { got = m }, func() error {
		clk.advance(150 * time.Millisecond) // "work" takes 150ms of virtual time
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Duration != 150*time.Millisecond {
		t.Errorf("Duration = %v, want 150ms", got.Duration)
	}
	if got.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", got.Status)
	}
}

func TestTimedBuggyRecordsZero(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: base()}
	var got Metric
	_ = TimedBuggy(clk.Now, "/orders", func(m Metric) { got = m }, func() error {
		clk.advance(150 * time.Millisecond)
		return nil
	})
	// The arg-eval trap: the duration was frozen before work ran.
	if got.Duration != 0 {
		t.Errorf("Duration = %v, want 0 (the trap)", got.Duration)
	}
}

func TestTimedRecordsMappedStatusFromError(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: base()}
	var got Metric
	err := Timed(clk.Now, "/orders/42", func(m Metric) { got = m }, func() error {
		clk.advance(10 * time.Millisecond)
		return fmt.Errorf("lookup: %w", ErrNotFound)
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if got.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", got.Status)
	}
	if got.Duration != 10*time.Millisecond {
		t.Errorf("Duration = %v, want 10ms", got.Duration)
	}
}

func ExampleTimed() {
	clk := &fakeClock{t: base()}
	_ = Timed(clk.Now, "/health", func(m Metric) {
		fmt.Printf("%s %v %d\n", m.Route, m.Duration, m.Status)
	}, func() error {
		clk.advance(5 * time.Millisecond)
		return nil
	})
	// Output: /health 5ms 200
}
```

## Review

The timer is correct when the recorded duration equals the real elapsed time and
the recorded status reflects the handler's actual outcome. The fake clock makes
both exact: `Timed` records 150ms for 150ms of work and a 404 for an `ErrNotFound`
return, while `TimedBuggy` records 0 for the same 150ms because its argument froze
at the `defer` statement. The lesson is a rule, not a trick: a deferred call's
arguments are snapshotted at the `defer` statement; anything that must be read at
return time — a duration, a named return, a status set later — belongs inside a
deferred closure, not in its argument list. Run `go test -race`.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — "each time a defer statement executes, the function value and parameters ... are evaluated as usual and saved anew".
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [time.Since](https://pkg.go.dev/time#Since)
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-mutex-defer-unlock-critical-section.md](05-mutex-defer-unlock-critical-section.md) | Next: [07-panic-recover-http-middleware.md](07-panic-recover-http-middleware.md)
