# Exercise 16: Event Debouncer That Coalesces Rapid Calls Within a Deadline

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A search-as-you-type box that fires a query on every keystroke wastes work
and races itself: keystroke three's response can arrive before keystroke
two's. A debouncer fixes this by coalescing a burst of rapid notifications
into a single event, delivered only once the caller has been quiet for a
deadline — and it does this with two closures sharing one private state
machine, not an exported type a caller could misuse.

## What you'll build

```text
debounce/                    independent module: example.com/debounce
  go.mod                     go 1.24
  debounce.go                func New[T] returning (notify, ready) closures
  debounce_test.go           coalescing, one-shot fire, re-arming, concurrency
  cmd/demo/
    main.go                  drives a fake clock through a burst of notifies
```

- Files: `debounce.go`, `debounce_test.go`, `cmd/demo/main.go`.
- Implement: `New[T any](quiet time.Duration, clock func() time.Time) (notify func(T), ready func() (T, bool))`.
- Test: a burst of notifies inside the quiet window produces no fire until the deadline from the *last* notify passes; the coalesced event is the last one notified, not the first; firing is one-shot — a second `ready()` without a new `notify` returns false; concurrent notifies never corrupt the coalesced value or panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two closures, one shared state machine

A debouncer needs two operations — record an event, and ask whether it is
time to fire — and they must share state: the last event, the deadline,
and whether anything is pending. Rather than exporting a `Debouncer`
struct whose fields a caller could read or mutate directly, `New` returns
exactly the two functions the contract needs, both closing over the same
private `mu`, `value`, `deadline`, and `armed` variables. There is no zero
value to misuse and no field to accidentally read without the lock.

`notify` always does the same two things regardless of whether anything
was already pending: it overwrites `value` with the newest event and pushes
`deadline` to `clock().Add(quiet)`. This is what "coalesce" means here —
each new notify doesn't add to a queue, it *replaces* the pending event and
resets the clock, so a burst of ten calls only ever leaves the last one
waiting to fire.

`ready` is a poll, not a callback fired by a background timer: a caller
(a real ticker loop, an event loop, or in this exercise a test manually
advancing a fake clock) calls it periodically. It fires — returning the
coalesced value and clearing `armed` — only if something is pending *and*
the clock has passed `deadline`. Both the check (`armed` and the deadline
comparison) and the act (clearing `armed`) happen under the same lock, so a
concurrent `ready` cannot see "armed and past deadline" twice for the same
event.

Create `debounce.go`:

```go
package debounce

import (
	"sync"
	"time"
)

// New returns a pair of closures sharing one private state machine: notify
// records event as the latest one seen and pushes the firing deadline
// quiet further into the future, while ready reports whether the deadline
// has passed without a new notify and, if so, returns the single
// coalesced event exactly once. Both closures share a mutex, clock, and
// deadline captured from the surrounding call to New — there is no
// exported type whose zero value could be used incorrectly.
func New[T any](quiet time.Duration, clock func() time.Time) (notify func(event T), ready func() (T, bool)) {
	var mu sync.Mutex
	var value T
	var deadline time.Time
	var armed bool

	notify = func(event T) {
		mu.Lock()
		defer mu.Unlock()
		value = event
		deadline = clock().Add(quiet)
		armed = true
	}

	ready = func() (T, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !armed || clock().Before(deadline) {
			var zero T
			return zero, false
		}
		v := value
		armed = false
		return v, true
	}

	return notify, ready
}
```

### The runnable demo

The demo simulates three rapid keystrokes on a fake clock, shows that
`ready()` reports nothing while still inside the quiet window, then
advances the clock and shows the coalesced last value fire exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/debounce"
)

func main() {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }

	notify, ready := debounce.New[string](200*time.Millisecond, clock)

	// Three rapid keystrokes, each resetting the quiet deadline.
	notify("h")
	now = now.Add(50 * time.Millisecond)
	notify("he")
	now = now.Add(50 * time.Millisecond)
	notify("hel")

	// Still inside the quiet window: nothing fires yet.
	if v, fired := ready(); fired {
		fmt.Printf("unexpected early fire: %q\n", v)
	} else {
		fmt.Println("t+100ms: not ready yet")
	}

	// Advance past the quiet window measured from the last notify.
	now = now.Add(250 * time.Millisecond)
	if v, fired := ready(); fired {
		fmt.Printf("t+350ms: fired with %q\n", v)
	}

	// Firing is one-shot: calling ready again without a new notify reports false.
	if _, fired := ready(); !fired {
		fmt.Println("t+350ms: already consumed, ready() is false")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t+100ms: not ready yet
t+350ms: fired with "hel"
t+350ms: already consumed, ready() is false
```

Each `notify` pushes the deadline 200ms further out from the clock at the
time it was called; the last one lands at `t=100ms`, so `ready()` at
`t=100ms` is still 200ms early. Advancing to `t=350ms` clears that deadline
and fires with `"hel"` — the last event notified, not the first.

### Tests

`TestDebouncerCoalescesRapidNotifies` proves the core contract: a burst
produces no fire until the deadline from the *last* notify, and the fired
value is the last one, not the first or a merge. `TestDebouncerFiresExactlyOnce`
and `TestDebouncerReArmsAfterFiring` pin the one-shot behavior and that a
fresh `notify` after a fire correctly arms a new cycle.
`TestDebouncerNeverReadyWithoutNotify` guards the zero-value case.
`TestDebouncerConcurrentNotifyIsRaceFree` fires twenty concurrent notifies
and asserts the mutex keeps the final coalesced value consistent (one of
the notified values, not a torn read) under `-race`.

Create `debounce_test.go`:

```go
package debounce

import (
	"sync"
	"testing"
	"time"
)

func TestDebouncerCoalescesRapidNotifies(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }
	notify, ready := New[string](200*time.Millisecond, clock)

	notify("a")
	now = now.Add(50 * time.Millisecond)
	notify("ab")
	now = now.Add(50 * time.Millisecond)
	notify("abc")

	if _, fired := ready(); fired {
		t.Fatal("ready() fired before the quiet window elapsed")
	}

	now = now.Add(250 * time.Millisecond) // 250ms after the last notify at t=100ms
	v, fired := ready()
	if !fired {
		t.Fatal("ready() should fire once the quiet window has elapsed")
	}
	if v != "abc" {
		t.Fatalf("ready() = %q, want %q (only the last event)", v, "abc")
	}
}

func TestDebouncerFiresExactlyOnce(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	notify, ready := New[int](100*time.Millisecond, clock)

	notify(1)
	now = now.Add(150 * time.Millisecond)

	if _, fired := ready(); !fired {
		t.Fatal("first ready() should fire")
	}
	if _, fired := ready(); fired {
		t.Fatal("second ready() should not fire without a new notify")
	}
}

func TestDebouncerReArmsAfterFiring(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	notify, ready := New[int](100*time.Millisecond, clock)

	notify(1)
	now = now.Add(150 * time.Millisecond)
	if v, fired := ready(); !fired || v != 1 {
		t.Fatalf("ready() = (%d, %v), want (1, true)", v, fired)
	}

	notify(2)
	now = now.Add(150 * time.Millisecond)
	if v, fired := ready(); !fired || v != 2 {
		t.Fatalf("ready() = (%d, %v), want (2, true)", v, fired)
	}
}

func TestDebouncerNeverReadyWithoutNotify(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	_, ready := New[int](100*time.Millisecond, clock)

	now = now.Add(time.Hour)
	if _, fired := ready(); fired {
		t.Fatal("ready() fired without any notify ever being called")
	}
}

func TestDebouncerConcurrentNotifyIsRaceFree(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	notify, ready := New[int](50*time.Millisecond, clock)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			notify(v)
		}(i)
	}
	wg.Wait()

	clockMu.Lock()
	now = now.Add(100 * time.Millisecond)
	clockMu.Unlock()

	v, fired := ready()
	if !fired {
		t.Fatal("ready() should fire after concurrent notifies settle")
	}
	if v < 0 || v >= 20 {
		t.Fatalf("ready() = %d, want one of the notified values [0,20)", v)
	}
}
```

## Review

The debouncer is correct when `notify` always resets the deadline relative
to when it was called, not when the burst started — that is what makes a
continuous stream of events never fire while it is still active, only once
it goes quiet. `ready` is correct when the "is it time, and if so clear the
flag" check happens as one atomic step under the lock; splitting the read
of `armed` from the clearing of `armed` would let two concurrent pollers
both believe they are the one delivering the event. Returning two plain
closures instead of a struct keeps the only entry points to the state
machine exactly the two operations the contract defines. Injecting `clock`
is what makes "assert no fire at t+100ms, fire at t+350ms" a fact instead
of a guess about real wall-clock timing.

## Resources

- [time package](https://pkg.go.dev/time) — `Time.Before`, `Time.Add`, the comparison this exercise is built on.
- [sync package](https://pkg.go.dev/sync) — `Mutex`, guarding a check-then-act state transition.
- [lodash debounce documentation](https://lodash.com/docs/4.17.15#debounce) — the same coalesce-on-quiet contract from a widely used implementation in another ecosystem.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-sliding-window-throttle-with-history.md](15-sliding-window-throttle-with-history.md) | Next: [17-batch-collector-flush-callback.md](17-batch-collector-flush-callback.md)
