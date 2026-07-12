# Exercise 5: Graceful-shutdown drain barrier (wait for in-flight to hit zero)

When a service receives SIGTERM it must stop accepting new requests, let the
in-flight ones finish, and only then exit — kill the process early and you drop
work on the floor; wait forever and the orchestrator SIGKILLs you anyway. This
module builds the coordination core of that sequence explicitly: an in-flight
tracker whose `Drain(ctx)` blocks until the count crosses zero, which is the
piece `net/http.Server.Shutdown` implements internally for its own connections.

## What you'll build

```text
drain/                      independent module: example.com/drain
  go.mod                    module path example.com/drain
  tracker.go                type Tracker: Begin, End, InFlight, Drain(ctx); ErrDraining
  cmd/
    demo/
      main.go               3 in-flight requests, drain, reject a late request
  tracker_test.go           per-End release order, ErrDraining, ctx deadline, -race churn
```

- Files: `tracker.go`, `cmd/demo/main.go`, `tracker_test.go`.
- Implement: a `Tracker` where `Begin() error` admits a request (or rejects it with a wrapped `ErrDraining` once draining), `End()` retires it, and `Drain(ctx) error` flips the draining flag and blocks until in-flight reaches zero or `ctx` fires.
- Test: `Drain` stays parked until the LAST of three `End` calls; `Begin` after drain-start fails with `errors.Is(err, ErrDraining)`; a `ctx` deadline returns `context.DeadlineExceeded` without corrupting the count; `-race` churn.
- Verify: `go test -count=1 -race ./...`

### One mutex, two roles: admission gate and zero-crossing barrier

The tracker's state is two fields under one mutex: an `inflight` count and a
`draining` flag. The same lock serves two predicates. `Begin` reads `draining`
as an admission gate — once shutdown starts, new requests are rejected
immediately with a sentinel error the HTTP layer can map to a 503, instead of
being accepted and then abandoned. `Drain` waits on the predicate
`inflight == 0`, and `End` fires the `Broadcast` exactly on the zero crossing —
signalling on every decrement would be harmless but pointless, since no waiter's
predicate can become true while the count is still positive.

`Broadcast`, not `Signal`, on both transitions. Reaching zero is a terminal
state for this shutdown cycle, and more than one goroutine may be parked in
`Drain` (a signal handler and a health-check controller can both legitimately
wait for quiescence). The terminal-state discipline from the concepts file
applies: any transition into a terminal state (`draining` set, in-flight zero)
Broadcasts so every waiter — present or future waiter class — re-evaluates.
`Drain` also Broadcasts right after setting the flag for the same reason; it
costs nothing and keeps the invariant simple as the type grows.

### Cancellation: the watcher-goroutine pattern, for real

`Cond.Wait` has no deadline, but an operator will not wait forever — Kubernetes
gives you `terminationGracePeriodSeconds` and then SIGKILL, so `Drain` takes a
`ctx` and must honor it. The module applies the canonical compensation pattern:
a watcher goroutine `select`s on `ctx.Done()` and Broadcasts the `Cond` so the
parked `Drain` wakes and re-checks `ctx.Err()` at the top of its predicate loop.
Two details carry the correctness. The watcher takes the mutex before
broadcasting, so the wakeup cannot fall into a gap between the waiter's
predicate check and its `Wait` — `Wait` releases the lock atomically, so any
Broadcast issued under the lock reaches it. And the `done` channel, closed by
`defer` when `Drain` returns, guarantees the watcher exits on the happy path
too; without it every successful drain would leak one goroutine that lives
until the context is eventually cancelled — or forever, for a background
context.

One deliberate policy decision: a timed-out `Drain` does NOT reset `draining`.
Shutdown is one-way — the caller is about to force-exit, and flipping the gate
back open would admit requests into a dying process. The count itself is left
intact, so the still-running requests can `End` cleanly and diagnostics can
report how many were abandoned.

Create `tracker.go`:

```go
package drain

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrDraining is returned by Begin once Drain has started: the service is
// shutting down and must not admit new work.
var ErrDraining = errors.New("draining")

// Tracker counts in-flight requests and lets a shutdown path wait for the
// count to reach zero while rejecting new admissions.
type Tracker struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	draining bool
}

// New returns an empty Tracker accepting requests.
func New() *Tracker {
	t := &Tracker{}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// Begin admits one request. It fails with a wrapped ErrDraining once Drain
// has started.
func (t *Tracker) Begin() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return fmt.Errorf("begin request: %w", ErrDraining)
	}
	t.inflight++
	return nil
}

// End retires one request admitted by Begin. The zero crossing releases every
// goroutine parked in Drain.
func (t *Tracker) End() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight == 0 {
		panic("drain: End without matching Begin")
	}
	t.inflight--
	if t.inflight == 0 {
		t.cond.Broadcast()
	}
}

// InFlight reports the current number of admitted, unfinished requests.
func (t *Tracker) InFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inflight
}

// Drain stops admissions and blocks until every in-flight request has ended
// or ctx is done. A ctx error is returned as-is; draining stays set either
// way — shutdown is one-way.
func (t *Tracker) Drain(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.draining = true
	t.cond.Broadcast() // terminal transition: let every waiter re-evaluate
	if t.inflight == 0 {
		return nil
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			t.cond.Broadcast() // kick Drain so it re-checks ctx.Err
			t.mu.Unlock()
		case <-done: // Drain returned first; exit without leaking
		}
	}()

	for t.inflight > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.cond.Wait()
	}
	return nil
}
```

### The runnable demo

Three simulated requests are in flight when the shutdown path calls `Drain`;
they finish on staggered timers, `Drain` returns once the last one ends, and a
late `Begin` is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/drain"
)

func main() {
	tr := drain.New()

	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		if err := tr.Begin(); err != nil {
			panic(err)
		}
		wg.Go(func() {
			defer tr.End()
			time.Sleep(time.Duration(i*20) * time.Millisecond) // request work
		})
	}
	fmt.Println("in flight before drain:", tr.InFlight())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Drain(ctx); err != nil {
		fmt.Println("drain failed:", err)
		return
	}
	fmt.Println("drained; in flight:", tr.InFlight())

	if err := tr.Begin(); errors.Is(err, drain.ErrDraining) {
		fmt.Println("late request rejected:", err)
	}
	wg.Wait()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in flight before drain: 3
drained; in flight: 0
late request rejected: begin request: draining
```

### Tests

`TestDrainWaitsForLastEnd` is the heart: with three requests in flight and
`Drain` parked (confirmed by `synctest.Wait`), each of the first two `End`
calls is asserted NOT to release it — only the third does. `TestDrainDeadline`
runs the whole timeout path on `synctest`'s fake clock: no real 100ms elapse,
and the elapsed bubble time is asserted to be exactly the deadline.
`TestDrainChurn` races a hundred short requests against `Drain` under `-race`.

Create `tracker_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestDrainWaitsForLastEnd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		tr := New()
		for range 3 {
			if err := tr.Begin(); err != nil {
				t.Fatalf("Begin: %v", err)
			}
		}

		drained := make(chan error, 1)
		go func() { drained <- tr.Drain(context.Background()) }()
		synctest.Wait() // Drain is durably parked on the Cond

		if err := tr.Begin(); !errors.Is(err, ErrDraining) {
			t.Fatalf("Begin after drain-start = %v, want ErrDraining", err)
		}

		for remaining := 2; remaining >= 1; remaining-- {
			tr.End()
			synctest.Wait()
			select {
			case <-drained:
				t.Fatalf("Drain returned with %d request(s) still in flight", remaining)
			default:
			}
		}

		tr.End() // the zero crossing
		synctest.Wait()
		select {
		case err := <-drained:
			if err != nil {
				t.Fatalf("Drain = %v, want nil", err)
			}
		default:
			t.Fatal("Drain did not return after the last End")
		}
	})
}

func TestDrainDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		tr := New()
		if err := tr.Begin(); err != nil {
			t.Fatalf("Begin: %v", err)
		}

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := tr.Drain(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Drain = %v, want context.DeadlineExceeded", err)
		}
		if d := time.Since(start); d != 100*time.Millisecond {
			t.Fatalf("Drain gave up after %v, want exactly 100ms on the fake clock", d)
		}
		if got := tr.InFlight(); got != 1 {
			t.Fatalf("InFlight after timed-out Drain = %d, want 1 (count uncorrupted)", got)
		}
		tr.End() // the straggler still retires cleanly
	})
}

func TestDrainChurn(t *testing.T) {
	t.Parallel()

	tr := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := tr.Begin(); err != nil {
				return // rejected: drain already started
			}
			runtime.Gosched()
			tr.End()
		})
	}

	if err := tr.Drain(context.Background()); err != nil {
		t.Fatalf("Drain = %v, want nil", err)
	}
	wg.Wait()
	if got := tr.InFlight(); got != 0 {
		t.Fatalf("InFlight after drain = %d, want 0", got)
	}
}

func Example() {
	tr := New()
	_ = tr.Begin()
	go tr.End()
	_ = tr.Drain(context.Background())
	fmt.Println("in flight after drain:", tr.InFlight())
	fmt.Println(errors.Is(tr.Begin(), ErrDraining))
	// Output:
	// in flight after drain: 0
	// true
}
```

## Review

The invariant to hold in your head is that `inflight` can only stay at zero
once `Drain` has set `draining`: no `Begin` can succeed anymore, so the zero
crossing is final and the Broadcast cannot be a false dawn. That is why the
`for t.inflight > 0` loop, the admission check, and the flag all live under one
mutex — split them across two locks and a `Begin` could slip in between the
last `End` and `Drain` waking, making "drained" a lie.

The classic mistakes here are all timeout-related. Returning from `Drain` on
`ctx` without a watcher goroutine simply does not work — the goroutine is
parked inside `Wait` and never sees `ctx.Done()`. Adding the watcher but
forgetting the `done` channel leaks it on every successful drain. And resetting
`draining` on timeout reopens admissions into a process that is about to call
`os.Exit`. `TestDrainDeadline` pins the first two (a leaked watcher fails
`synctest.Test`'s leak check) and asserts the count survives the timeout, so
the abandoned request can still `End` without panicking.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — Wait, Broadcast, and the locking contract.
- [`net/http` Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the production API whose in-flight barrier this module reconstructs.
- [`context`](https://pkg.go.dev/context) — deadline propagation for the drain budget.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — fake-clock deadlines and durable-blocking assertions.

---

Back to [04-single-flight-cache.md](04-single-flight-cache.md) | Next: [06-pausable-worker-pool.md](06-pausable-worker-pool.md)
