# Exercise 10: Admission Drain Gate: Shed New Work While Finishing In-Flight

**Level: Intermediate**

A service behind a rolling deploy is marked for termination and must stop
accepting new requests the instant that happens, yet let every already-admitted
request run to completion before the process exits. The naive approach flips a
`bool` flag, but a flag read outside a lock races the write and lets a request
slip in after shutdown began, while a flag under a mutex serializes every
handler on one lock. This exercise builds the correct primitive: a `Gate` whose
single `close` broadcasts "draining, reject-new" to every concurrent `Admit` at
once, paired with a barrier that holds `Drain` open until the last in-flight
request finishes.

This module is self-contained: its own module, an `admit` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
admit/                       independent module: example.com/admit
  go.mod                     go 1.26
  admit.go                   type Gate; New, Admit, Drain, InFlight
  cmd/demo/main.go           runnable demo: admit, flip to draining, finish in-flight
  admit_test.go              shed-after-close, drain-waits, ctx-cancel, idempotent, race
```

- Files: `admit.go`, `cmd/demo/main.go`, `admit_test.go`.
- Implement: `func New() *Gate`; `func (g *Gate) Admit() (release func(), ok bool)`; `func (g *Gate) Drain(ctx context.Context) error`; `func (g *Gate) InFlight() int`.
- Test: `Admit` counts and releases; `Admit` sheds immediately once draining is closed (non-blocking, no timing); `Drain` returns nil only after every release ran; `Drain` honors ctx while a request is held; `Drain` is idempotent; a request admitted just before `Drain` still completes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/admit/cmd/demo
cd ~/go-exercises/admit
go mod init example.com/admit
go get go.uber.org/goleak
go mod tidy
```

### The gate is a readiness barrier inverted

A readiness gate closes a channel to *open* admission: handlers park on
`<-ready` and all proceed the moment it closes. A drain gate is the mirror
image. It starts admitting, and one `close(draining)` flips it permanently into
"reject-new", which every concurrent `Admit` observes at once through a
non-blocking read of the now-closed channel. The close is the whole signal; there
is no value to interpret and no per-handler bookkeeping, which is exactly why a
`chan struct{}` close is the right fan-out primitive here and a `bool` flag is
not.

The protocol has three moving parts and one ordering rule that makes it
race-free:

1. `Admit` takes a short lock, does a non-blocking `select` on `draining`, and
   only if it is still open records the request: increment the in-flight counter
   and `wg.Add(1)`. It hands back a `release` closure the caller runs exactly once
   when the request finishes.
2. `Drain` closes `draining` once under the same lock. Because the close and every
   `wg.Add` are serialized by that lock, no `Add` can begin after the close. That
   is the invariant that keeps the `WaitGroup` legal: every `Add` happens-before
   the close, and the close happens-before the wait.
3. `Drain` then blocks until the counter reaches zero. `wg.Wait` cannot be
   cancelled, so we bridge it onto a channel and `select` on that channel versus
   `ctx.Done()`. A held request plus a fired context returns `ctx.Err()` instead of
   hanging.

The failure mode this avoids is the "one request slips through" bug: a handler
that checked an unsynchronized flag as `false`, then proceeded, while shutdown
flipped the flag and began tearing down its dependencies. Here, the lock plus the
closed-channel latch make the decision atomic — after the close, every `Admit`
sheds, deterministically, with no timing window.

Create `admit.go`:

```go
package admit

import (
	"context"
	"sync"
	"sync/atomic"
)

// Gate is an admission barrier for graceful shutdown. Before draining it admits
// new requests; a single Drain flips it permanently into "reject-new" and then
// waits for every already-admitted request to finish. The flip is a close of the
// draining channel, so every concurrent Admit observes it at once.
type Gate struct {
	// draining is closed exactly once by Drain. A closed channel is a permanent,
	// broadcast "reject-new" latch that every Admit sees via a non-blocking read.
	draining chan struct{}
	once     sync.Once

	// mu serializes the admit decision against the close in Drain so that no
	// wg.Add can begin after draining is closed. That ordering is what keeps the
	// WaitGroup usage race-free: every Add happens-before the close, and the
	// close happens-before wg.Wait.
	mu sync.Mutex

	inflight atomic.Int64
	wg       sync.WaitGroup
}

// New returns a Gate that is admitting.
func New() *Gate {
	return &Gate{draining: make(chan struct{})}
}

// Admit reports whether a new request may proceed. Before draining it returns
// (release, true) and the caller MUST call release exactly once when the request
// finishes. Once draining it returns (nil, false) immediately: the non-blocking
// select on the closed draining channel never touches the WaitGroup, so shedding
// costs nothing and cannot block behind in-flight work.
func (g *Gate) Admit() (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	select {
	case <-g.draining:
		return nil, false
	default:
	}

	g.inflight.Add(1)
	g.wg.Add(1)

	var done sync.Once
	return func() {
		done.Do(func() {
			g.inflight.Add(-1)
			g.wg.Done()
		})
	}, true
}

// Drain closes the draining channel once (a broadcast: every future Admit sheds)
// then blocks until every admitted request has released or ctx is done. It
// returns nil when the last release ran and ctx.Err() if ctx fired first. Drain
// is idempotent: sync.Once makes a second call safe and non-panicking.
func (g *Gate) Drain(ctx context.Context) error {
	g.mu.Lock()
	g.once.Do(func() { close(g.draining) })
	g.mu.Unlock()

	// Bridge wg.Wait (uncancellable) onto a channel so we can also select on ctx.
	// The helper goroutine exits when the last in-flight request releases, which
	// the caller contract guarantees, so it never outlives the work it waits on.
	waited := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InFlight returns the number of admitted requests that have not yet released.
func (g *Gate) InFlight() int {
	return int(g.inflight.Load())
}
```

### The runnable demo

The demo admits two requests, starts draining in the background, confirms that a
fresh `Admit` is now rejected while the two remain in flight, then releases them
and watches `Drain` return `nil`. A second gate shows the context-cancel path and
that a repeated `Drain` is a no-op. The output is deterministic: the in-flight
count reported during draining is fixed by the protocol, not by scheduling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"runtime"

	"example.com/admit"
)

func main() {
	g := admit.New()

	// Phase 1: before draining, Admit accepts and hands back a release.
	rel1, ok1 := g.Admit()
	rel2, ok2 := g.Admit()
	fmt.Printf("admitted before drain: %v, %v; in-flight: %d\n", ok1, ok2, g.InFlight())

	// Phase 2: start draining in the background. It closes the draining channel
	// immediately, then blocks until rel1 and rel2 run.
	drainErr := make(chan error, 1)
	go func() { drainErr <- g.Drain(context.Background()) }()

	// Spin until the gate begins shedding. Once Drain has closed the draining
	// channel, Admit returns ok=false; any accept before that is released so it
	// does not perturb the in-flight count we report next.
	for {
		r, ok := g.Admit()
		if !ok {
			break
		}
		r()
		runtime.Gosched()
	}
	fmt.Println("admit after drain requested: rejected")

	// The two requests admitted before draining are still in flight.
	fmt.Printf("in-flight during drain: %d\n", g.InFlight())
	rel1()
	rel2()
	fmt.Printf("drain returned: %v; in-flight: %d\n", <-drainErr, g.InFlight())

	// Phase 3: a fresh gate showing the ctx-cancel path and idempotent Drain.
	g2 := admit.New()
	rel, _ := g2.Admit()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fmt.Printf("drain with cancelled ctx: %v; in-flight: %d\n", g2.Drain(ctx), g2.InFlight())
	rel() // release the held request so no drain helper is left waiting
	fmt.Printf("second drain (idempotent): %v\n", g2.Drain(context.Background()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
admitted before drain: true, true; in-flight: 2
admit after drain requested: rejected
in-flight during drain: 2
drain returned: <nil>; in-flight: 0
drain with cancelled ctx: context canceled; in-flight: 1
second drain (idempotent): <nil>
```

### Tests

`TestAdmitBeforeDrainCountsAndReleases` pins the counter contract: `Admit` moves
`InFlight` from 0 to 1 and `release` moves it back.
`TestAdmitShedsImmediatelyAfterDrainCloses` flips the gate synchronously with an
already-cancelled context, then asserts `Admit` returns `(nil, false)` at once
while a request is still held — the shed is decided by the closed channel, with no
sleep. `TestDrainBlocksUntilEveryReleaseRan` proves the barrier: with two requests
in flight, `Drain` provably cannot return after only one release (the second is
still counted), and returns `nil` only after the last. `TestRequestAdmittedJust
BeforeDrainCompletes` shows a pre-drain admission is honored and counted.
`TestDrainReturnsCtxErrWhileRequestHeld` holds a request and confirms a
short-deadline `Drain` returns `context.DeadlineExceeded` rather than hanging.
`TestDrainIdempotent` calls `Drain` twice and relies on `sync.Once` to avoid a
double-close panic. `TestConcurrentAdmitDuringDrainNoRace` runs an admit storm
against a concurrent `Drain` under `-race`. A `goleak` `TestMain` proves no
goroutine — including the drain bridge — is left running.

Create `admit_test.go`:

```go
package admit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain runs every test then asserts no goroutine leaked. The drain helper
// goroutine must exit once its in-flight requests release, so a forgotten
// release or a never-returning wait would surface here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestAdmitBeforeDrainCountsAndReleases(t *testing.T) {
	g := New()
	if g.InFlight() != 0 {
		t.Fatalf("new gate in-flight = %d, want 0", g.InFlight())
	}
	release, ok := g.Admit()
	if !ok || release == nil {
		t.Fatalf("Admit before drain = (release==nil:%v, ok:%v), want (non-nil, true)", release == nil, ok)
	}
	if g.InFlight() != 1 {
		t.Fatalf("in-flight after Admit = %d, want 1", g.InFlight())
	}
	release()
	if g.InFlight() != 0 {
		t.Fatalf("in-flight after release = %d, want 0", g.InFlight())
	}
}

func TestAdmitShedsImmediatelyAfterDrainCloses(t *testing.T) {
	g := New()
	// One request is admitted and deliberately held in flight.
	rel, ok := g.Admit()
	if !ok {
		t.Fatal("first Admit should succeed")
	}
	// Flip into draining synchronously: an already-cancelled ctx makes Drain
	// close the draining channel and return at once, with a request still held.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain(cancelled) = %v, want context.Canceled", err)
	}
	// The draining channel is closed, so Admit sheds immediately via the
	// non-blocking select, even though a request is still in flight. No timing.
	if r, ok := g.Admit(); ok || r != nil {
		t.Fatalf("Admit after drain = (release==nil:%v, ok:%v), want (nil, false)", r == nil, ok)
	}
	if g.InFlight() != 1 {
		t.Fatalf("held request should still count, in-flight = %d, want 1", g.InFlight())
	}
	rel()
	if g.InFlight() != 0 {
		t.Fatalf("in-flight after release = %d, want 0", g.InFlight())
	}
}

func TestDrainBlocksUntilEveryReleaseRan(t *testing.T) {
	g := New()
	rel1, _ := g.Admit()
	rel2, _ := g.Admit()

	done := make(chan error, 1)
	go func() { done <- g.Drain(context.Background()) }()

	// Drain cannot return while rel2 is outstanding: wg count is still positive.
	rel1()
	select {
	case err := <-done:
		t.Fatalf("Drain returned %v before the last release", err)
	default:
	}
	// The final release drops the count to zero; only now may Drain return nil.
	rel2()
	if err := <-done; err != nil {
		t.Fatalf("Drain = %v, want nil after all released", err)
	}
	if g.InFlight() != 0 {
		t.Fatalf("in-flight after drain = %d, want 0", g.InFlight())
	}
}

func TestRequestAdmittedJustBeforeDrainCompletes(t *testing.T) {
	g := New()
	rel, ok := g.Admit() // admitted while still accepting
	if !ok {
		t.Fatal("Admit before drain should succeed")
	}
	done := make(chan error, 1)
	go func() { done <- g.Drain(context.Background()) }()

	rel() // the pre-drain request finishes; Drain must count it and then return
	if err := <-done; err != nil {
		t.Fatalf("Drain = %v, want nil", err)
	}
}

func TestDrainReturnsCtxErrWhileRequestHeld(t *testing.T) {
	g := New()
	rel, _ := g.Admit()
	defer rel() // release so the drain helper goroutine exits before goleak runs

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	// A request is held, so Drain must observe ctx before the WaitGroup and must
	// not hang.
	if err := g.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain(short deadline) = %v, want context.DeadlineExceeded", err)
	}
}

func TestDrainIdempotent(t *testing.T) {
	g := New()
	if err := g.Drain(context.Background()); err != nil {
		t.Fatalf("first Drain = %v, want nil", err)
	}
	// Second Drain must not panic on a double close: sync.Once guards it.
	if err := g.Drain(context.Background()); err != nil {
		t.Fatalf("second Drain = %v, want nil", err)
	}
	if r, ok := g.Admit(); ok || r != nil {
		t.Fatalf("Admit after drain = (release==nil:%v, ok:%v), want (nil, false)", r == nil, ok)
	}
}

func TestConcurrentAdmitDuringDrainNoRace(t *testing.T) {
	g := New()
	var workers sync.WaitGroup
	for range 50 {
		workers.Go(func() {
			for range 200 {
				if r, ok := g.Admit(); ok {
					r()
				}
			}
		})
	}
	// Drain races the admit storm: every Admit either fully completes (Add then
	// Done) before the close or sheds after it, so wg.Wait converges.
	if err := g.Drain(context.Background()); err != nil {
		t.Fatalf("Drain during storm = %v, want nil", err)
	}
	workers.Wait()
	if g.InFlight() != 0 {
		t.Fatalf("in-flight after storm = %d, want 0", g.InFlight())
	}
}

func ExampleGate() {
	g := New()
	release, ok := g.Admit()
	fmt.Println("admitted:", ok)
	release()

	_ = g.Drain(context.Background())

	_, ok = g.Admit()
	fmt.Println("admitted after drain:", ok)
	// Output:
	// admitted: true
	// admitted after drain: false
}
```

## Review

Correct here means two guarantees hold together: after `Drain` closes the
`draining` channel no request is ever admitted again, and `Drain` returns `nil`
only once every request admitted before the close has released. The closed
channel is what makes the first guarantee a broadcast every `Admit` sees at the
same instant, and holding the close under the same lock as `wg.Add` is what makes
the `WaitGroup` legal — every `Add` happens-before the close, so `wg.Wait` can
converge. `TestDrainBlocksUntilEveryReleaseRan` proves the barrier by showing
`Drain` cannot return with a single request still counted, and
`TestAdmitShedsImmediatelyAfterDrainCloses` proves the shed is decided by the
channel, not a sleep. The production bug this prevents is the request that slips
through a rolling deploy: a handler that read an unsynchronized "shutting down"
flag as false and proceeded into dependencies that shutdown had already begun to
tear down, losing the response and corrupting the drain. The `goleak` `TestMain`
closes the loop by proving the drain bridge goroutine never outlives the work it
waits on.

## Resources

- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) -- the idempotent guard that makes a repeated Drain a safe no-op instead of a double-close panic.
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- the in-flight barrier; note the rule that positive Adds must happen-before Wait, which the lock enforces.
- [go.dev/blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) -- how ctx.Done() is the same closed-channel broadcast used to cancel a stuck Drain.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector that verifies the drain bridge goroutine exits.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-goroutine-leak-guard.md](09-goroutine-leak-guard.md) | Next: [11-config-reload-watch-broadcast.md](11-config-reload-watch-broadcast.md)
