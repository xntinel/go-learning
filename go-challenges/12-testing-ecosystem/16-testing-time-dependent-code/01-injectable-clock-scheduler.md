# Exercise 1: Inject a Clock into a Scheduler and test it deterministically with a FakeClock

The oldest and most portable way to make time testable is to stop calling
`time.Now()` directly and take a narrow `Clock` interface instead. This exercise
builds a small scheduler that decides which events are due, injects a `Clock`,
and proves its behavior with a `FakeClock` the test advances by hand — no
sleeping, fully deterministic, works on every Go version.

## What you'll build

```text
clockscheduler/                independent module: example.com/clockscheduler
  go.mod
  clock.go                     Clock interface (Now, After); RealClock; Scheduler (ScheduleAt, Run)
  fake_clock.go                FakeClock: Now, After, Advance (mutex-guarded)
  cmd/
    demo/
      main.go                  build a scheduler on a FakeClock, advance it, print due events
  clock_test.go                past/future selection, Advance, RealClock bounds, empty run, After
```

Files: `clock.go`, `fake_clock.go`, `cmd/demo/main.go`, `clock_test.go`.
Implement: a 2-method `Clock`, a production `RealClock`, a `Scheduler` that returns due events by consulting the injected clock, and a `FakeClock` with `Advance`.
Test: table/unit tests injecting `FakeClock` — only due events return, `Advance` makes a future event due, `RealClock.Now()` falls between two `time.Now()` reads, an empty scheduler returns nothing, and `After` fires at the advanced instant.
Verify: `go test -count=1 -race ./...`

### Why a Clock interface, and why exactly two methods

A `Scheduler` that called `time.Now()` internally would be untestable without
sleeping: to prove an event scheduled one hour out is not yet due, you would have
to actually not wait an hour, and to prove it becomes due you would have to wait
one. Injecting a `Clock` turns "what time is it" into an input. Production passes
`RealClock{}`, which forwards to the stdlib; a test passes a `FakeClock` it can
set to any instant.

The interface has exactly two methods — `Now()` for "what time is it" and
`After(d)` for "give me a channel that fires after d" — because those are the
only time operations this code performs. Interface segregation is a real cost
saving here: every extra method on `Clock` is an extra method every fake must
implement. `After` returns a *channel* rather than blocking, so callers can
`select` on it against cancellation, and so the fake can hand back a pre-loaded
channel instead of a real timer.

`Scheduler.Run` is a pure function of the injected clock and the stored events:
an event is due when its instant is not after `clock.Now()`. Note the `!e.at.After(now)`
formulation rather than `e.at.Before(now) || e.at.Equal(now)` — an event scheduled
for exactly *now* is due, and `!After` captures the `<=` boundary in one call.

Create `clock.go`:

```go
package clockscheduler

import "time"

// Clock is the minimal time surface this package needs: the current instant and
// a channel that fires after a duration. Production uses RealClock; tests use
// FakeClock.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock forwards to the standard library.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type event struct {
	at  time.Time
	tag string
}

// Scheduler holds tagged events and reports which are due, according to the
// injected clock. It never calls time.Now directly.
type Scheduler struct {
	clock  Clock
	events []event
}

func NewScheduler(c Clock) *Scheduler {
	return &Scheduler{clock: c}
}

// ScheduleAt registers tag to fire at instant t.
func (s *Scheduler) ScheduleAt(t time.Time, tag string) {
	s.events = append(s.events, event{at: t, tag: tag})
}

// Run returns the tags of every event whose instant is at or before the clock's
// current time. An event scheduled for exactly now is due.
func (s *Scheduler) Run() []string {
	now := s.clock.Now()
	var out []string
	for _, e := range s.events {
		if !e.at.After(now) {
			out = append(out, e.tag)
		}
	}
	return out
}
```

### The FakeClock

`FakeClock` implements `Clock` with a stored instant the test moves with
`Advance`. `After(d)` returns a one-buffered channel already carrying
`now.Add(d)`: a caller that `select`s on it receives immediately, which models
"the timer has already fired" without any real waiting. The stored instant is
guarded by a mutex so that a test which advances the clock from one goroutine
while the code under test reads it from another stays race-clean under `-race`.

Create `fake_clock.go`:

```go
package clockscheduler

import (
	"sync"
	"time"
)

// FakeClock is a Clock the test drives by hand. Now returns a stored instant;
// Advance moves it forward. It is safe for concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel already carrying the instant d from now, so a select
// on it fires immediately with no real timer.
func (f *FakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- f.now.Add(d)
	return ch
}

// Advance moves the clock forward by d.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
```

### The runnable demo

The demo builds a scheduler on a `FakeClock`, schedules one past and one future
event, runs it, then advances two hours and runs again — showing the future event
become due without any real time passing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/clockscheduler"
)

func main() {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fc := clockscheduler.NewFakeClock(start)
	s := clockscheduler.NewScheduler(fc)

	s.ScheduleAt(start.Add(-time.Hour), "cleanup")
	s.ScheduleAt(start.Add(time.Hour), "report")

	fmt.Printf("at start: %v\n", s.Run())
	fc.Advance(2 * time.Hour)
	fmt.Printf("after +2h: %v\n", s.Run())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
at start: [cleanup]
after +2h: [cleanup report]
```

### Tests

The tests inject a `FakeClock` and never sleep. `TestSchedulerRunsPastEvents`
proves only due events return. `TestSchedulerAfterAdvance` proves `Advance` moves
a future event into the due set. `TestRealClockReturnsCurrentTime` sandwiches
`RealClock{}.Now()` between two `time.Now()` reads to prove the production clock
tracks wall time. `TestSchedulerEmptyRun` pins the "empty scheduler returns
empty" contract. `TestFakeClockAfterFires` proves the fake's `After` channel
carries the advanced instant.

Create `clock_test.go`:

```go
package clockscheduler

import (
	"fmt"
	"testing"
	"time"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestSchedulerRunsPastEvents(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(start)
	s := NewScheduler(fc)
	s.ScheduleAt(start.Add(-time.Hour), "past")
	s.ScheduleAt(start, "now")
	s.ScheduleAt(start.Add(time.Hour), "future")

	got := s.Run()
	want := []string{"past", "now"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Run = %v, want %v", got, want)
	}
}

func TestSchedulerAfterAdvance(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(start)
	s := NewScheduler(fc)
	s.ScheduleAt(start.Add(time.Hour), "event")

	if got := s.Run(); len(got) != 0 {
		t.Fatalf("Run before advance = %v, want empty", got)
	}
	fc.Advance(2 * time.Hour)
	if got := s.Run(); len(got) != 1 || got[0] != "event" {
		t.Fatalf("Run after advance = %v, want [event]", got)
	}
}

func TestRealClockReturnsCurrentTime(t *testing.T) {
	t.Parallel()
	before := time.Now()
	got := RealClock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now = %v, want in [%v, %v]", got, before, after)
	}
}

func TestSchedulerEmptyRun(t *testing.T) {
	t.Parallel()
	s := NewScheduler(NewFakeClock(start))
	if got := s.Run(); len(got) != 0 {
		t.Fatalf("empty scheduler Run = %v, want empty", got)
	}
}

func TestFakeClockAfterFires(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(start)
	fc.Advance(30 * time.Minute)
	select {
	case at := <-fc.After(time.Hour):
		want := start.Add(90 * time.Minute)
		if !at.Equal(want) {
			t.Fatalf("After fired at %v, want %v", at, want)
		}
	default:
		t.Fatal("FakeClock.After did not fire immediately")
	}
}

func ExampleScheduler_Run() {
	fc := NewFakeClock(start)
	s := NewScheduler(fc)
	s.ScheduleAt(start.Add(-time.Minute), "due")
	s.ScheduleAt(start.Add(time.Minute), "pending")
	fmt.Println(s.Run())
	// Output: [due]
}
```

## Review

The scheduler is correct when "due" is a pure function of the injected clock and
the stored instants: `!e.at.After(now)` includes the exact-now boundary and
excludes the future, and nothing calls `time.Now()` behind the interface. The
`FakeClock` proof is that advancing it turns a future event due with no wall-clock
time elapsed, and the `RealClock` proof is the sandwich test. The two mistakes to
avoid: making `Clock` fat (every method is a tax on the fake — keep it at two),
and reaching for `time.Sleep` in a test when `Advance` is exact and free. Run
`go test -race` to confirm the mutex-guarded `FakeClock` is safe if a later test
advances it concurrently.

## Resources

- [`time` package](https://pkg.go.dev/time) — `time.Now`, `time.After`, `time.Time.After`, `time.Time.Add`.
- [`testing` package](https://pkg.go.dev/testing) — `T.Parallel`, table tests, `Example` functions.
- [Go Code Review Comments: interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — keep interfaces small; define them where they are consumed.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-backoff-retry-injected-sleep.md](02-backoff-retry-injected-sleep.md)
