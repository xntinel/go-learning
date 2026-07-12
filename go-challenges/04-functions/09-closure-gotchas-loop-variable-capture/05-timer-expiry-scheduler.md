# Exercise 5: Per-Item Expiry Scheduler: AfterFunc Closure Capture

An expiry scheduler arms one `time.AfterFunc` per session/cache entry so each
fires its own eviction callback. Timer callbacks are closures that run later, on
another goroutine, so they are a prime loop-capture trap: capture a shared item
and every timer evicts the last key. You build a scheduler that binds the correct
entry per timer, records evictions under a mutex, and can cancel a pending timer
on shutdown.

## What you'll build

```text
expiry/                      independent module: example.com/expiry
  go.mod                     go 1.26
  scheduler.go               Scheduler; Arm (one AfterFunc per key), Cancel, Evicted
  cmd/
    demo/
      main.go                runnable demo: arm keys, wait, print evicted set
  scheduler_test.go          per-key eviction set, Stop cancels, -race clean
```

- Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
- Implement: `Scheduler.Arm(entries)` that starts one `time.AfterFunc` per entry, each callback binding its own key and appending to a mutex-guarded evicted list; `Cancel(key)` that stops a pending timer via `Timer.Stop`.
- Test: arm keys with tiny distinct durations and assert the evicted set equals the input set exactly (a shared capture would evict the last key N times); assert `Cancel` stops a pending timer so its callback never fires; `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/05-timer-expiry-scheduler/cmd/demo
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/05-timer-expiry-scheduler
go mod edit -go=1.26
```

### Why the callback must bind its own key

`time.AfterFunc(d, f)` starts a timer that, after duration `d`, calls `f` on its
own goroutine, and returns a `*time.Timer` you can `Stop`. The callback `f` is a
closure. If it captures the loop variable directly on a pre-1.22 module, every
timer that fires reads the final key, so the scheduler evicts the last entry N
times and the other N-1 entries never get evicted — a cache that appears to
"leak" every session except the last. On a `go 1.26` module the per-iteration
variable makes the direct capture correct, but because the callback runs on a
*different goroutine at a later time*, it is the highest-stakes place to get this
right: the bug is invisible until timers fire in production.

Two other properties matter for real expiry code. First, the eviction callback
runs concurrently with whatever else touches the scheduler, so the evicted-set it
appends to must be mutex-guarded — the intended shared state (the eviction log)
is exactly the kind of captured state that must be synchronized when the closure
escapes to multiple goroutines. Second, timers must be cancellable: on graceful
shutdown, or when an entry is refreshed, you call `Timer.Stop` so a stale
callback does not fire against a gone entry. `Stop` returns false if the timer
already fired or was already stopped; the scheduler stores each `*Timer` keyed by
its entry so `Cancel` can find and stop it.

Create `scheduler.go`:

```go
package expiry

import (
	"sort"
	"sync"
	"time"
)

// Entry is an item that should be evicted after TTL from arm time.
type Entry struct {
	Key string
	TTL time.Duration
}

// Scheduler arms one timer per entry and records which keys were evicted.
type Scheduler struct {
	mu      sync.Mutex
	timers  map[string]*time.Timer
	evicted []string
}

// New returns a ready Scheduler.
func New() *Scheduler {
	return &Scheduler{timers: make(map[string]*time.Timer)}
}

// Arm starts one AfterFunc per entry. Each callback binds its own key.
func (s *Scheduler) Arm(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		key := e.Key
		s.timers[key] = time.AfterFunc(e.TTL, func() {
			s.evict(key)
		})
	}
}

// Cancel stops a pending timer for key, if any, so its callback never fires.
// It reports whether a timer was stopped before firing.
func (s *Scheduler) Cancel(key string) bool {
	s.mu.Lock()
	t, ok := s.timers[key]
	s.mu.Unlock()
	if !ok {
		return false
	}
	return t.Stop()
}

func (s *Scheduler) evict(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evicted = append(s.evicted, key)
}

// Evicted returns the sorted set of keys evicted so far.
func (s *Scheduler) Evicted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.evicted...)
	sort.Strings(out)
	return out
}
```

### The runnable demo

The demo arms three entries with short real durations, sleeps past the longest,
and prints the sorted evicted set — proof that each timer evicted its own key.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/expiry"
)

func main() {
	s := expiry.New()
	s.Arm([]expiry.Entry{
		{Key: "session-1", TTL: 10 * time.Millisecond},
		{Key: "session-2", TTL: 20 * time.Millisecond},
		{Key: "session-3", TTL: 30 * time.Millisecond},
	})

	time.Sleep(60 * time.Millisecond)
	fmt.Println("evicted:", s.Evicted())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
evicted: [session-1 session-2 session-3]
```

### Tests

`TestEachTimerEvictsItsOwnKey` arms keys with distinct tiny TTLs, waits past all
of them, and asserts the evicted set equals the input set — a shared capture
would evict the last key N times, failing this. `TestCancelStopsPendingTimer`
arms a key with a longer TTL, cancels it, waits, and asserts it was never
evicted.

Create `scheduler_test.go`:

```go
package expiry

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until true or the deadline passes, without sleeping the
// whole duration up front; keeps the timing-dependent test fast and stable.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func TestEachTimerEvictsItsOwnKey(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Key: "a", TTL: 5 * time.Millisecond},
		{Key: "b", TTL: 10 * time.Millisecond},
		{Key: "c", TTL: 15 * time.Millisecond},
		{Key: "d", TTL: 20 * time.Millisecond},
	}
	s := New()
	s.Arm(entries)

	if !waitFor(func() bool { return len(s.Evicted()) == len(entries) }, time.Second) {
		t.Fatalf("only %v evicted, want all four", s.Evicted())
	}

	want := []string{"a", "b", "c", "d"}
	got := s.Evicted()
	if len(got) != len(want) {
		t.Fatalf("evicted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evicted = %v, want %v (shared capture evicts last key N times)", got, want)
		}
	}
}

func TestCancelStopsPendingTimer(t *testing.T) {
	t.Parallel()

	s := New()
	s.Arm([]Entry{{Key: "keep", TTL: 50 * time.Millisecond}})

	if !s.Cancel("keep") {
		t.Fatal("Cancel returned false; timer already fired before Stop")
	}
	time.Sleep(80 * time.Millisecond)
	if got := s.Evicted(); len(got) != 0 {
		t.Fatalf("evicted = %v after cancel, want none", got)
	}
}

func TestArmIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	s := New()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Arm([]Entry{{Key: fmt.Sprintf("k%d", i), TTL: time.Millisecond}})
		}()
	}
	wg.Wait()
	waitFor(func() bool { return len(s.Evicted()) == 20 }, time.Second)
	if got := len(s.Evicted()); got != 20 {
		t.Fatalf("evicted count = %d, want 20", got)
	}
}

func ExampleScheduler_Cancel() {
	s := New()
	s.Arm([]Entry{{Key: "x", TTL: time.Hour}})
	fmt.Println(s.Cancel("x"))
	// Output: true
}
```

## Review

The scheduler is correct when the evicted set equals the armed set exactly and a
cancelled timer never fires. `TestEachTimerEvictsItsOwnKey` is the capture guard:
each `AfterFunc` callback must bind its own key, so a shared capture that evicts
`"d"` four times fails it. Two things carry the design: the eviction log is
mutex-guarded because callbacks run on timer goroutines concurrently, and each
`*Timer` is retained so `Cancel` can `Stop` it on shutdown or refresh. Note
`Stop` returning false means the timer already fired — real code that reuses the
entry must handle that race. Run `go test -race`; the concurrent `Arm` and the
callback appends must be clean.

## Resources

- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — deferred callback on its own goroutine; returns a stoppable `*Timer`.
- [`time.Timer.Stop`](https://pkg.go.dev/time#Timer.Stop) — cancelling a pending timer and its false-return semantics.
- [Go 1.22 release notes: loop variable scoping](https://go.dev/doc/go1.22#language) — why the per-iteration key is bound correctly.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-validation-rule-chain.md](06-validation-rule-chain.md)
