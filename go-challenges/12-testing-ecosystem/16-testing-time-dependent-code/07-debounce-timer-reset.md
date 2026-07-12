# Exercise 7: A debouncer on time.AfterFunc + Timer.Reset, tested under synctest

Debouncing coalesces a burst of events into a single action after a quiet window:
a search box that fires one query after typing stops, a file watcher that rebuilds
once after a flurry of saves, a config reloader that applies the last of several
rapid changes. The canonical implementation is a single `time.AfterFunc` timer
reset on every trigger. Because `AfterFunc` timers have no channel, `Reset` needs
no drain — which is exactly why they are the right primitive. This exercise builds
that debouncer and proves its coalescing under a `synctest` bubble.

## What you'll build

```text
debounce/                      independent module: example.com/debounce
  go.mod
  debounce.go                  Debouncer: Trigger (AfterFunc/Reset), Stop
  cmd/
    demo/
      main.go                  fire a burst on a real timer; show one action runs
  debounce_test.go             synctest: N rapid triggers -> one action; later trigger -> second
```

Files: `debounce.go`, `cmd/demo/main.go`, `debounce_test.go`.
Implement: a `Debouncer` where each `Trigger()` (re)arms a single `time.AfterFunc` timer to run the action only after a quiet `window`; a `Stop()` that cancels a pending fire.
Test: `synctest.Test` — fire several `Trigger()` calls with sub-window virtual gaps, advance past the window, assert the action ran exactly once; trigger again, advance, assert twice; `Stop` prevents a stale fire.
Verify: `go test -count=1 -race ./...`

Set up the module (synctest is stable in Go 1.25):

```bash
mkdir -p go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/07-debounce-timer-reset/cmd/demo
cd go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/07-debounce-timer-reset
go mod edit -go=1.25
```

### Why AfterFunc, and the Reset-without-drain point

A channel timer (`time.NewTimer`) is awkward for a debouncer: to re-arm it you
must `Stop` it and, if it already fired, drain its channel before `Reset`, or a
stale value lurks in `C`. `time.AfterFunc` sidesteps all of that. It runs a
*function* in its own goroutine when it fires; there is no channel to drain, so
`Reset` on an `AfterFunc` timer is safe to call unconditionally. That is the whole
reason `AfterFunc` is the idiomatic debounce primitive.

The logic: the first `Trigger()` creates the timer with `time.AfterFunc(window,
action)`. Every subsequent `Trigger()` calls `timer.Reset(window)`, pushing the
fire instant to `window` after the *latest* trigger. So a burst of triggers spaced
closer than `window` apart keeps resetting the deadline, and the action fires
exactly once, `window` after the final trigger. A trigger that arrives after the
window has already elapsed (the action already ran) simply re-arms the timer for
the next fire. `Stop()` stops the timer so a pending action does not run after
shutdown.

Everything is guarded by a mutex because `Trigger`, `Stop`, and the timer's own
goroutine can race on the `*Timer` field. Acquiring an uncontended mutex does not
block, so this is safe inside a `synctest` bubble; the action itself increments an
`atomic.Int64` so the test can read the count without additional locking.

Create `debounce.go`:

```go
package debounce

import (
	"sync"
	"time"
)

// Debouncer coalesces bursty triggers into a single action that runs only after
// a quiet window with no further triggers. It uses a single time.AfterFunc timer
// reset on each trigger.
type Debouncer struct {
	window time.Duration
	action func()

	mu    sync.Mutex
	timer *time.Timer
}

func New(window time.Duration, action func()) *Debouncer {
	return &Debouncer{window: window, action: action}
}

// Trigger (re)arms the timer to fire the action window from now. Rapid triggers
// within the window collapse into a single fire after the last one.
func (d *Debouncer) Trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer == nil {
		d.timer = time.AfterFunc(d.window, d.action)
		return
	}
	d.timer.Reset(d.window)
}

// Stop cancels any pending fire. After Stop, a scheduled action will not run
// unless Trigger is called again.
func (d *Debouncer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
	}
}
```

### The runnable demo

The demo fires five triggers 5ms apart against a real 30ms window, then waits for
the window to elapse — showing a single action despite five triggers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"example.com/debounce"
)

func main() {
	var runs atomic.Int64
	d := debounce.New(30*time.Millisecond, func() {
		runs.Add(1)
	})

	for range 5 {
		d.Trigger()
		time.Sleep(5 * time.Millisecond) // bursts closer than the window
	}
	time.Sleep(60 * time.Millisecond) // let the quiet window elapse

	fmt.Printf("triggers: 5, actions: %d\n", runs.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
triggers: 5, actions: 1
```

### Tests

`TestBurstCoalesces` runs inside a bubble: it fires three triggers with
half-window virtual gaps (each `Reset` pushing the deadline), calling
`synctest.Wait()` after each to let the bubble settle, asserts the action has not
run yet, then advances a full window and asserts exactly one run. It then triggers
again, advances, and asserts a second run — proving the debouncer re-arms.
`TestStopPreventsFire` triggers, then `Stop`s before the window, advances past it,
and asserts the action never ran.

Create `debounce_test.go`:

```go
package debounce

import (
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestBurstCoalesces(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int64
		window := time.Second
		d := New(window, func() { runs.Add(1) })

		// Three triggers, each half a window apart: every Reset pushes the
		// deadline, so nothing fires yet.
		for range 3 {
			d.Trigger()
			time.Sleep(window / 2)
			synctest.Wait()
			if got := runs.Load(); got != 0 {
				t.Fatalf("action ran mid-burst: runs = %d, want 0", got)
			}
		}

		// Quiet window elapses -> exactly one action.
		time.Sleep(window)
		synctest.Wait()
		if got := runs.Load(); got != 1 {
			t.Fatalf("after burst: runs = %d, want 1", got)
		}

		// A later trigger produces a second action.
		d.Trigger()
		time.Sleep(window)
		synctest.Wait()
		if got := runs.Load(); got != 2 {
			t.Fatalf("after second trigger: runs = %d, want 2", got)
		}
	})
}

func TestStopPreventsFire(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int64
		window := time.Second
		d := New(window, func() { runs.Add(1) })

		d.Trigger()
		time.Sleep(window / 2)
		synctest.Wait()
		d.Stop()

		time.Sleep(2 * window)
		synctest.Wait()
		if got := runs.Load(); got != 0 {
			t.Fatalf("action fired after Stop: runs = %d, want 0", got)
		}
	})
}

// ExampleDebouncer shows the coalescing contract on a real timer: a burst of
// triggers spaced far closer than the window collapses into a single action once
// the quiet window elapses. The window is short and the trailing wait is a wide
// multiple of it, so the single fire is observed reliably.
func ExampleDebouncer() {
	var runs atomic.Int64
	d := New(20*time.Millisecond, func() { runs.Add(1) })

	for range 5 {
		d.Trigger()
		time.Sleep(2 * time.Millisecond) // bursts closer than the window
	}
	time.Sleep(100 * time.Millisecond) // let the quiet window elapse

	fmt.Printf("triggers=5 actions=%d\n", runs.Load())
	// Output:
	// triggers=5 actions=1
}
```

## Review

The debouncer is correct when a burst of triggers closer than `window` apart
yields exactly one action `window` after the last trigger, a later trigger yields
another, and `Stop` cancels a pending fire. The bubble proves the coalescing
deterministically: `synctest.Wait` after each virtual gap lets the action
goroutine (if any) settle before the count is read, and the final advance past the
window is what actually fires the single action. The primitive choice is the
lesson: `time.AfterFunc` gives a `Reset` with no channel to drain, which is why it
is preferred over `time.NewTimer` for this pattern — a `NewTimer` debouncer that
forgot to drain `C` before `Reset` would carry a stale fire. Run `go test -race`;
the mutex must guard the shared `*Timer` against the action goroutine.

## Resources

- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) and [`Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — the debounce primitive and its reset semantics.
- [`time.Timer.Stop`](https://pkg.go.dev/time#Timer.Stop) — cancelling a pending fire.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the bubble and `synctest.Wait` used to sequence the assertions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-synctest-context-timeout.md](06-synctest-context-timeout.md) | Next: [08-circuit-breaker-half-open.md](08-circuit-breaker-half-open.md)
