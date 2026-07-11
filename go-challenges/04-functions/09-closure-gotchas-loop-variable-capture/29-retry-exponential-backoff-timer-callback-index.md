# Exercise 29: Exponential Backoff: Time.AfterFunc Callbacks Capturing Retry Attempt Index

**Nivel: Intermedio** — validacion rapida (un test corto).

A retry scheduler queues one deferred callback per attempt, each meant to
fire with its own attempt number for logging and backoff math. The trap is
declaring `attempt` ONCE before the `for` statement instead of letting the
loop declare it: Go 1.22's per-iteration loop variables only apply to a
variable the `for` statement itself introduces (`for attempt := ...`), not to
one declared earlier and merely assigned inside the loop. Every callback
closes over that one shared variable, and by the time the scheduler actually
fires them, it holds its final, post-loop value.

## What you'll build

```text
backoff/                     independent module: example.com/backoff
  go.mod                     go 1.24
  backoff.go                 Scheduler, ManualScheduler, BuggyScheduleRetries, ScheduleRetries
  cmd/
    demo/
      main.go                runnable demo: schedule 4 retries, fire them, print attempts
  backoff_test.go            each callback keeps its own attempt vs all see the last one
```

- Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
- Implement: `ManualScheduler` recording callbacks without any real timer; `BuggyScheduleRetries` declaring `attempt` outside the `for` statement; `ScheduleRetries` declaring it as the loop's own variable.
- Test: fire all scheduled callbacks and assert `ScheduleRetries` fires attempts `0,1,2,3` while `BuggyScheduleRetries` fires the same, final, attempt number every time.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backoff/cmd/demo
cd ~/go-exercises/backoff
go mod init example.com/backoff
go mod edit -go=1.24
```

### Why Go 1.22 does not save this particular loop

Go 1.22's fix gives each iteration of `for attempt := 0; attempt < n;
attempt++` its own `attempt` — but only when the `for` statement declares
it. `BuggyScheduleRetries` instead writes `var attempt int` before the loop
and then `for attempt = 0; attempt < attempts; attempt++`, reusing that
pre-declared variable as the loop counter. That is still exactly one
variable for the entire function, on every Go version, because the language
change never touched variables declared outside the `for` clause. Every
callback's closure reads that same variable, and since callbacks only fire
later — after the loop has finished — they all see its final value: `attempts`
itself, one past the last valid index, because that is what the loop counter
holds the instant the loop's condition fails and it exits.

`ManualScheduler` stands in for `time.AfterFunc`: it just records callbacks
in a slice instead of starting real timers, so the test can fire them
deterministically with no sleeping and no timing flakiness.

Create `backoff.go`:

```go
package backoff

import (
	"slices"
	"sync"
	"time"
)

// Scheduler defers f to run at some point after delay. A real
// implementation wraps time.AfterFunc; ManualScheduler below just records
// the callback so tests can fire it deterministically, with no real timers
// and no sleeping.
type Scheduler interface {
	Schedule(delay time.Duration, f func())
}

// ManualScheduler collects scheduled callbacks in registration order without
// starting any real timer.
type ManualScheduler struct {
	mu    sync.Mutex
	Queue []func()
}

// Schedule records f; delay is ignored since no real timer runs.
func (m *ManualScheduler) Schedule(_ time.Duration, f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Queue = append(m.Queue, f)
}

// FireAll runs every scheduled callback, in the order they were scheduled.
func (m *ManualScheduler) FireAll() {
	m.mu.Lock()
	jobs := slices.Clone(m.Queue)
	m.mu.Unlock()
	for _, f := range jobs {
		f()
	}
}

// BuggyScheduleRetries schedules one retry callback per attempt, but
// `attempt` is declared ONCE outside the for statement instead of as the
// loop's own iteration variable, so every callback closes over that SAME
// variable. Go 1.22's per-iteration loop variables only apply to variables
// declared BY the for statement itself (`for attempt := ...`); a variable
// declared before the loop and merely assigned inside it is still one
// shared variable for the whole function, on any Go version. By the time
// the scheduler fires the callbacks, `attempt` holds its final value and
// every callback reports the same, last, attempt number.
func BuggyScheduleRetries(s Scheduler, attempts int, onFire func(attempt int)) {
	var attempt int // BUG: declared outside the loop, shared by every callback
	for attempt = 0; attempt < attempts; attempt++ {
		s.Schedule(time.Duration(1<<attempt)*time.Millisecond, func() {
			onFire(attempt) // reads the shared variable, not this iteration's value
		})
	}
}

// ScheduleRetries schedules one retry callback per attempt, with `attempt`
// declared BY the for statement so each callback closes over its own
// iteration's value.
func ScheduleRetries(s Scheduler, attempts int, onFire func(attempt int)) {
	for attempt := 0; attempt < attempts; attempt++ {
		s.Schedule(time.Duration(1<<attempt)*time.Millisecond, func() {
			onFire(attempt)
		})
	}
}
```

### The runnable demo

The demo schedules four retries with both variants and fires them all,
printing the attempt numbers each variant's callbacks actually reported.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/backoff"
)

func main() {
	var mu sync.Mutex

	buggySched := &backoff.ManualScheduler{}
	var buggyFired []int
	backoff.BuggyScheduleRetries(buggySched, 4, func(attempt int) {
		mu.Lock()
		buggyFired = append(buggyFired, attempt)
		mu.Unlock()
	})
	buggySched.FireAll()
	fmt.Println("buggy  fired attempts:", buggyFired)

	fixedSched := &backoff.ManualScheduler{}
	var fixedFired []int
	backoff.ScheduleRetries(fixedSched, 4, func(attempt int) {
		mu.Lock()
		fixedFired = append(fixedFired, attempt)
		mu.Unlock()
	})
	fixedSched.FireAll()
	fmt.Println("fixed  fired attempts:", fixedFired)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  fired attempts: [4 4 4 4]
fixed  fired attempts: [0 1 2 3]
```

### Tests

`TestScheduleRetriesEachCallbackKeepsOwnAttempt` fires four scheduled
callbacks and asserts they report `0,1,2,3` in order.
`TestBuggyScheduleRetriesAllCallbacksSeeLastAttempt` asserts every callback
instead reports `4` — one past the last valid attempt index, the loop
counter's value the instant the loop condition fails.

Create `backoff_test.go`:

```go
package backoff

import "testing"

func TestScheduleRetriesEachCallbackKeepsOwnAttempt(t *testing.T) {
	sched := &ManualScheduler{}
	var fired []int
	ScheduleRetries(sched, 4, func(attempt int) {
		fired = append(fired, attempt)
	})
	sched.FireAll()

	want := []int{0, 1, 2, 3}
	if len(fired) != len(want) {
		t.Fatalf("fired = %v, want %v", fired, want)
	}
	for i := range want {
		if fired[i] != want[i] {
			t.Fatalf("fired = %v, want %v", fired, want)
		}
	}
}

func TestBuggyScheduleRetriesAllCallbacksSeeLastAttempt(t *testing.T) {
	sched := &ManualScheduler{}
	var fired []int
	BuggyScheduleRetries(sched, 4, func(attempt int) {
		fired = append(fired, attempt)
	})
	sched.FireAll()

	if len(fired) != 4 {
		t.Fatalf("len(fired) = %d, want 4", len(fired))
	}
	for i, a := range fired {
		if a != 4 {
			t.Fatalf("fired[%d] = %d, want 4 (shared attempt variable holds its final, post-loop value)", i, a)
		}
	}
}
```

## Review

A retry scheduler is correct when every fired callback reports the attempt
number it was actually scheduled for. The mechanism to keep straight is that
Go 1.22's loop-variable fix is scoped to variables the `for` statement itself
declares — `var attempt int` before the loop, reused as the counter with
plain `=`, is still one variable shared by every closure built inside that
loop, no matter which Go version compiles it. `ManualScheduler` makes the
bug fully deterministic and instant to test: no real timer, no sleep, just
firing every recorded callback and checking what it reports. The fix costs
nothing — moving the declaration into the `for` statement (`for attempt :=
0; ...`) is all it takes.

## Resources

- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — exactly which variables the per-iteration change applies to.
- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — the real scheduler `Scheduler` stands in for.
- [Go spec: For statements](https://go.dev/ref/spec#For_statements) — the distinction between the for clause's own variables and variables declared outside it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-circuit-breaker-per-service-unguarded-closure-state.md](28-circuit-breaker-per-service-unguarded-closure-state.md) | Next: [30-sentinel-bootstrap-value-goroutine-fan-capture.md](30-sentinel-bootstrap-value-goroutine-fan-capture.md)
