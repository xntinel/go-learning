# Exercise 12: A Run Group That Stops Every Component When the First One Exits

**Level: Advanced**

A backend service is rarely one goroutine; it is a set of heterogeneous long-lived
components — an HTTP server, a queue consumer, a metrics flusher, a
replication-lag watcher — each running until it returns. The naive approach starts
them all with bare `go` and never links their lifecycles, so when one dies the
rest keep running: a half-stopped service that still holds ports, connections, and
queue leases. This exercise builds a run group (the oklog/run actor pattern) where
the exit of any one component interrupts every other component in reverse start
order, and `Run` blocks until all have exited.

This module is self-contained: its own module, a `rungroup` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
rungroup/                    independent module: example.com/rungroup
  go.mod                     go 1.26
  rungroup.go                type Group; Add, Run; reverse-order coordinated shutdown, ErrPanic recovery
  cmd/demo/main.go           runnable demo: four components, one dies, all stop in reverse order
  rungroup_test.go           first-error, interrupt-once-reverse-order, panic-wraps-ErrPanic, empty-group, goleak
```

- Files: `rungroup.go`, `cmd/demo/main.go`, `rungroup_test.go`.
- Implement: `Add(execute func() error, interrupt func(error))` and `Run() error`
  on a `Group`; a package-level `ErrPanic`. `Run` launches every actor, and on the
  first return interrupts every OTHER actor exactly once in reverse add order,
  joins all, and returns the first actor's error.
- Test: `Run` returns exactly the first actor's error; every other interrupt fires
  exactly once in reverse order; a panic is recovered into an error wrapping
  `ErrPanic` and still triggers shutdown; an empty group returns nil immediately;
  no goroutine leaks.
- Verify: `go test -count=1 -race ./...`

Set up the module. This module depends on `go.uber.org/goleak`, so run
`go mod tidy` after writing the test:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/12-run-group-coordinated-component-shutdown/cmd/demo
cd go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/12-run-group-coordinated-component-shutdown
go mod edit -go=1.26
go mod tidy
```

### An actor is an (execute, interrupt) pair, and Run owns the whole set

The whole model rests on decomposing each component into two functions that a
coordinator can call:

- `execute func() error` is the component's blocking main loop. It runs until it
  decides to return — a clean stop, a fatal error, whatever. The group never
  cancels `execute` directly; Go has no way to kill a goroutine.
- `interrupt func(error)` is the request to leave. It must cause a currently
  running `execute` to return, and it must be safe to call even after `execute`
  has already returned (interrupts and exits race). The `error` argument carries
  the reason the group is shutting down, so a component can log or classify it.

`Run` implements a precise protocol over the set of actors:

1. Launch every `execute` in its own goroutine with `wg.Go`, each reporting its
   index and error onto a buffered channel of capacity `n`. The buffer matters: a
   finished actor must be able to deposit its result and exit even before anyone
   reads it, which is exactly what lets the final `wg.Wait` join them all instead
   of deadlocking.
2. Receive once. The first value to arrive is the first actor to return, because
   every other actor is designed to keep running until interrupted — so this
   receive selects the genuine first exit and its error.
3. Interrupt every OTHER actor exactly once, iterating indices from `n-1` down to
   `0` and skipping the one that returned. Reverse add order is deliberate: the
   component started first (the HTTP server accepting traffic) is torn down last,
   so downstream consumers stop before the front door closes. Because this loop
   runs in `Run`'s single goroutine, the interrupt order is deterministic without
   any timing.
4. `wg.Wait`. Signal and wait are two separate steps; step 3 only asked the
   components to leave. `Run` returns only once every goroutine has actually
   exited, so it never hands control back to the caller with a component still
   running.

A component's `execute` might panic — a bad message, a nil dereference deep in a
handler. An unrecovered panic at the top of a goroutine crashes the entire
process, taking down every other component. So each `execute` runs behind a
`recover` boundary that converts the panic into an error wrapping `ErrPanic`. That
recovered error flows through the exact same path as any other first return: it
becomes the shutdown reason and interrupts the rest. One component's panic
degrades to a coordinated shutdown instead of a crash.

Create `rungroup.go`:

```go
// Package rungroup coordinates a set of heterogeneous, long-lived components so
// that the exit of any one of them tears down all the others. It is the
// oklog/run actor-group pattern: each component is an (execute, interrupt) pair,
// Run launches every execute, and when the first one returns it interrupts every
// other component in reverse start order and blocks until all have exited.
package rungroup

import (
	"errors"
	"fmt"
	"sync"
)

// ErrPanic wraps the value of an actor that panicked. Run recovers such a panic
// into an error that wraps ErrPanic; callers match it with errors.Is.
var ErrPanic = errors.New("rungroup: actor panicked")

// actor is one component: execute runs until it returns, and interrupt asks it
// to return early. interrupt receives the error that triggered the shutdown.
type actor struct {
	execute   func() error
	interrupt func(error)
}

// Group is a collection of actors run as a unit. The zero Group is ready to use.
type Group struct {
	actors []actor
}

// Add registers one actor. execute is the component's main loop; interrupt must
// cause a currently-running execute to return, and must be safe to call even if
// execute has already returned. Add is not safe for concurrent use; build the
// group from a single goroutine, then call Run.
func (g *Group) Add(execute func() error, interrupt func(error)) {
	g.actors = append(g.actors, actor{execute: execute, interrupt: interrupt})
}

// result carries which actor returned and with what error.
type result struct {
	index int
	err   error
}

// Run launches every actor's execute in its own goroutine and blocks until they
// have all returned. The moment the first actor returns (cleanly, with an error,
// or via a recovered panic), Run interrupts every OTHER actor exactly once in
// reverse add order, then joins all goroutines. It returns the error produced by
// the first actor to return. An empty group returns nil immediately.
func (g *Group) Run() error {
	if len(g.actors) == 0 {
		return nil
	}

	n := len(g.actors)
	// Buffered to n so a slow reader never blocks a finished actor's send: every
	// actor can deposit its result and exit, which is what lets Wait join them.
	done := make(chan result, n)

	var wg sync.WaitGroup
	for i := range g.actors {
		execute := g.actors[i].execute
		wg.Go(func() {
			done <- result{index: i, err: safeExecute(execute)}
		})
	}

	// The first actor to return decides the shutdown error. Every other actor
	// only sends after we interrupt it, so this receive picks the genuine first.
	first := <-done

	// Interrupt every other actor exactly once, newest-started first. The loop
	// runs in this single goroutine, so the interrupt order is deterministic and
	// needs no timing to be reverse-of-add.
	for i := n - 1; i >= 0; i-- {
		if i == first.index {
			continue
		}
		g.actors[i].interrupt(first.err)
	}

	// Signal was sent above; now wait. Join every goroutine before returning so
	// Run never leaves a component running behind the caller's back.
	wg.Wait()
	return first.err
}

// safeExecute runs execute and converts a panic into an error wrapping ErrPanic,
// so one component's panic triggers coordinated shutdown instead of crashing the
// whole process.
func safeExecute(execute func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	return execute()
}
```

### The runnable demo

The demo models four components started in a fixed order: an HTTP server, a queue
consumer, a metrics flusher, and a replication-lag watcher. Each blocking
component's `execute` parks until it is interrupted; the queue consumer instead
hits a poison message and returns an error, which is guaranteed to be the first
return because everyone else is still parked. Watch the interrupts fire in reverse
start order — and note the queue consumer itself is absent from that list, because
it already returned.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sync"

	"example.com/rungroup"
)

// errPoison is the fatal condition that takes the whole service down.
var errPoison = errors.New("bad payload")

// component is a long-lived service part. Its execute blocks until interrupted;
// interrupt records the shutdown order and unblocks execute exactly once.
type component struct {
	name string
	stop chan struct{}
	once sync.Once
}

func newComponent(name string) *component {
	return &component{name: name, stop: make(chan struct{})}
}

func (c *component) execute() error {
	<-c.stop // run until the group interrupts us
	return nil
}

func main() {
	// Start order: the HTTP server comes up first and is torn down last, so
	// in-flight requests keep working while everything downstream stops first.
	httpServer := newComponent("http-server")
	queueConsumer := newComponent("queue-consumer")
	metricsFlusher := newComponent("metrics-flusher")
	replicationWatcher := newComponent("replication-watcher")

	// order records the sequence in which interrupts fired. Run calls interrupts
	// from a single goroutine, so appends here are deterministic and race-free.
	var order []string

	var g rungroup.Group
	addBlocking := func(c *component) {
		g.Add(c.execute, func(error) {
			order = append(order, c.name)
			c.once.Do(func() { close(c.stop) })
		})
	}

	addBlocking(httpServer)

	// The queue consumer hits a poison message and returns first. Its interrupt
	// is never called (it already returned), which the reverse-order list shows.
	g.Add(
		func() error { return fmt.Errorf("queue-consumer: %w", errPoison) },
		func(error) { order = append(order, queueConsumer.name) },
	)

	addBlocking(metricsFlusher)
	addBlocking(replicationWatcher)

	err := g.Run()

	fmt.Println("component returned first: queue-consumer")
	fmt.Println("shutdown error:", err)
	fmt.Println("interrupts fired in reverse start order:")
	for i, name := range order {
		fmt.Printf("  %d. %s\n", i+1, name)
	}
	fmt.Println("all components stopped")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
component returned first: queue-consumer
shutdown error: queue-consumer: bad payload
interrupts fired in reverse start order:
  1. replication-watcher
  2. metrics-flusher
  3. http-server
all components stopped
```

### Tests

`TestMain` registers `goleak.VerifyTestMain(m)` so the suite fails if any actor
goroutine outlives its `Run` — the join guarantee, checked rigorously rather than
with a `NumGoroutine` snapshot, and the reason the tests use `VerifyTestMain`
rather than `VerifyNone` under `t.Parallel()`. `TestRunReturnsFirstActorsError`
pins that `Run` returns exactly the sentinel produced by the first actor to
return. `TestInterruptsFireOnceInReverseAddOrder` is the coordination core: an
atomic per-actor counter proves each other actor's interrupt fired exactly once
(and the returner's zero times), and a mutex-guarded order slice proves the
observed sequence is the reverse of add order. `TestPanicRecoveredWrapsErrPanicAndStillInterrupts`
pins that a panicking actor becomes an error wrapping `ErrPanic` and that the
panic still interrupts the survivor. `TestEmptyGroupReturnsNil` pins the empty-set
base case returning promptly. Every test builds a fresh group, so `-count=2`
re-runs are deterministic.

Create `rungroup_test.go`:

```go
package rungroup_test

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/rungroup"
	"go.uber.org/goleak"
)

// TestMain runs goleak after the whole suite: if any actor goroutine outlived
// its Group.Run, the suite fails. This is the leak-free-join guarantee, checked
// rigorously rather than by a NumGoroutine snapshot.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// blocker is a test actor whose execute blocks until it is interrupted. It
// records, atomically, how many times its interrupt fired, and appends its id to
// a shared order slice under a mutex so the test can assert interrupt ordering.
type blocker struct {
	id         int
	stop       chan struct{}
	once       sync.Once
	interrupts atomic.Int64
}

func newBlocker(id int) *blocker {
	return &blocker{id: id, stop: make(chan struct{})}
}

func (b *blocker) add(g *rungroup.Group, order *[]int, mu *sync.Mutex) {
	g.Add(
		func() error {
			<-b.stop
			return nil
		},
		func(error) {
			b.interrupts.Add(1)
			mu.Lock()
			*order = append(*order, b.id)
			mu.Unlock()
			b.once.Do(func() { close(b.stop) })
		},
	)
}

// TestRunReturnsFirstActorsError pins that Run returns exactly the error of the
// first actor to return, unwrappable with errors.Is.
func TestRunReturnsFirstActorsError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("first actor exit")

	var g rungroup.Group
	var order []int
	var mu sync.Mutex

	// Actor 0 returns the sentinel immediately; the two blockers only return once
	// interrupted, so the sentinel is unambiguously first.
	g.Add(func() error { return sentinel }, func(error) {})
	newBlocker(1).add(&g, &order, &mu)
	newBlocker(2).add(&g, &order, &mu)

	err := g.Run()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v", err, sentinel)
	}
}

// TestInterruptsFireOnceInReverseAddOrder pins the coordination core: every OTHER
// actor's interrupt runs exactly once, the returning actor's does not run, and
// the observed order is the reverse of add order.
func TestInterruptsFireOnceInReverseAddOrder(t *testing.T) {
	t.Parallel()

	var g rungroup.Group
	var order []int
	var mu sync.Mutex

	// Actor 0 returns first; blockers 1..3 are added in order and must be
	// interrupted 3, 2, 1.
	returner := errors.New("returner")
	var returnerInterrupts atomic.Int64
	g.Add(
		func() error { return returner },
		func(error) { returnerInterrupts.Add(1) },
	)
	blockers := []*blocker{newBlocker(1), newBlocker(2), newBlocker(3)}
	for _, b := range blockers {
		b.add(&g, &order, &mu)
	}

	if err := g.Run(); !errors.Is(err, returner) {
		t.Fatalf("Run() error = %v, want %v", err, returner)
	}

	if got := returnerInterrupts.Load(); got != 0 {
		t.Fatalf("returning actor interrupted %d times, want 0", got)
	}
	for _, b := range blockers {
		if got := b.interrupts.Load(); got != 1 {
			t.Fatalf("blocker %d interrupted %d times, want 1", b.id, got)
		}
	}
	if want := []int{3, 2, 1}; !slices.Equal(order, want) {
		t.Fatalf("interrupt order = %v, want %v", order, want)
	}
}

// TestPanicRecoveredWrapsErrPanicAndStillInterrupts pins that a panicking actor
// is recovered into an error wrapping ErrPanic, and that panic still triggers the
// coordinated shutdown of the remaining actors.
func TestPanicRecoveredWrapsErrPanicAndStillInterrupts(t *testing.T) {
	t.Parallel()

	var g rungroup.Group
	var order []int
	var mu sync.Mutex

	g.Add(func() error { panic("kaboom") }, func(error) {})
	b := newBlocker(1)
	b.add(&g, &order, &mu)

	err := g.Run()
	if !errors.Is(err, rungroup.ErrPanic) {
		t.Fatalf("Run() error = %v, want it to wrap ErrPanic", err)
	}
	if got := b.interrupts.Load(); got != 1 {
		t.Fatalf("survivor interrupted %d times, want 1", got)
	}
}

// TestEmptyGroupReturnsNil pins that an empty group's Run returns nil at once.
func TestEmptyGroupReturnsNil(t *testing.T) {
	t.Parallel()

	var g rungroup.Group
	done := make(chan error, 1)
	go func() { done <- g.Run() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("empty Run() = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("empty Run() did not return promptly")
	}
}
```

## Review

The run group is correct when the exit of any single component drives a bounded,
ordered, leak-free shutdown of the whole set. The invariant that guarantees it is
the four-step protocol in `Run`: launch every actor, receive the first return,
interrupt every other actor exactly once in reverse add order, then join. The
buffered result channel is what makes the join deadlock-free — a finished actor
always has somewhere to put its result — and running the interrupt loop in `Run`'s
own goroutine is what makes reverse ordering deterministic without any timing.
`TestInterruptsFireOnceInReverseAddOrder` proves the ordering and the exactly-once
count with an atomic counter and a mutex-guarded slice;
`TestRunReturnsFirstActorsError` proves the returned error is the first actor's;
the `recover` boundary plus `TestPanicRecoveredWrapsErrPanicAndStillInterrupts`
proves a panicking component degrades to a coordinated shutdown instead of a
process crash; and `goleak.VerifyTestMain` proves no actor goroutine survives
`Run`. The production bug this pattern prevents is the half-stopped service: one
component dies, the rest keep holding ports, queue leases, and connections, and
the process lingers in a state no health check can describe.

## Resources

- [oklog/run](https://pkg.go.dev/github.com/oklog/run) -- the actor-group pattern this exercise reconstructs, widely used to coordinate a service's top-level goroutines.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 launch-and-track helper that fuses Add, go, and Done.
- [`errors.Is` and wrapping](https://go.dev/blog/go1.13-errors) -- how `%w` and `errors.Is` let `ErrPanic` and each actor's error stay matchable through the group.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- goroutine-leak detection via `VerifyTestMain`, the join check used here.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-restartable-poller-start-stop-cycles.md](11-restartable-poller-start-stop-cycles.md) | Next: [13-per-tenant-dispatcher-registry-isolation.md](13-per-tenant-dispatcher-registry-isolation.md)
