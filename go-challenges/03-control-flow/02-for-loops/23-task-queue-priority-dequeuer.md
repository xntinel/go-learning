# Exercise 23: Priority-Based Task Dequeuing with Graceful Drain

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A worker that consumes from a high-priority and a low-priority queue has to
prefer the high-priority one whenever both have work waiting, run forever
under normal operation, and — the part that is easy to get wrong — on
shutdown it must flush whatever is already buffered instead of dropping it
on the floor or blocking forever trying to accept more. This module builds
that worker as two loop shapes stacked together: an infinite `for { select
{...} }` for steady-state consumption, and a bounded `for range` drain for
graceful shutdown.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
taskqueue/                     module example.com/taskqueue
  go.mod                       go 1.24
  taskqueue.go                 Task; Run(ctx, high, low, out) int; drain
  taskqueue_test.go              already-cancelled drain, steady-state priority, drain snapshot bound, empty drain
  cmd/demo/
    main.go                     pre-loaded high/low queues drained by an already-cancelled context
```

- Files: `taskqueue.go`, `taskqueue_test.go`, `cmd/demo/main.go`.
- Implement: `Run(ctx context.Context, high, low <-chan Task, out chan<- Task) int` — an infinite `for { select { ... } }` loop that checks `high` non-blockingly first, then blocks on `ctx.Done()`/`high`/`low`; a private `drain(ch, out) int` that ranges over `len(ch)` (a Go 1.22 integer range) to flush exactly what is buffered right now.
- Test: an already-cancelled context drains buffered high-then-low tasks in priority order; steady-state consumption prefers high over low while both have work; `drain` stops at its snapshot length and never blocks on an empty channel.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/taskqueue/cmd/demo
cd ~/go-exercises/taskqueue
go mod init example.com/taskqueue
go mod edit -go=1.24
```

### Why the drain is bounded by a snapshot length, not "while not empty"

The steady-state loop is the canonical infinite consumer from the concepts
file: its only structural exit is `ctx.Done()`, and everything else is a
`continue` back to the top. The interesting design decision is entirely in
`drain`. The tempting shape is `for len(ch) > 0 { out <- <-ch }` — "keep
draining while there's something in the channel" — but that is a
condition-only loop with no independent bound, and if a producer is still
sending concurrently (which is exactly the situation during a real
shutdown, where in-flight producers may not have noticed cancellation yet),
`len(ch) > 0` can stay true indefinitely and the "graceful drain" never
actually returns. Taking `n := len(ch)` once, up front, and then `for range
n` claims responsibility for exactly the tasks that were buffered at the
instant shutdown began — nothing sent after that snapshot is this drain's
problem, which keeps the shutdown path provably bounded regardless of what
producers do next. The send `out <- t` inside the drain still blocks if
`out` itself is full, which is the correct backpressure behavior — a
graceful drain that empties `high`/`low` by dropping tasks on the floor when
`out` is momentarily full would not be graceful at all.

Create `taskqueue.go`:

```go
package taskqueue

import "context"

// Task is one unit of work carrying a priority queue label for bookkeeping.
type Task struct {
	ID       string
	Priority string
}

// Run consumes from high and low priority queues, always preferring a
// buffered high-priority task over a low-priority one, until ctx is
// cancelled. On cancellation it gracefully drains whatever is already
// buffered in both channels into out before returning, so no in-flight work
// is silently dropped.
//
// The steady-state consumption is an infinite for-select loop: its only
// exits are ctx.Done() and (in a real deployment) the caller stopping the
// producers. The shutdown drain is a bounded for-range loop -- range over
// len(ch), a Go 1.22 integer range -- that flushes exactly the tasks
// buffered in the channel at the moment of cancellation, not one more,
// respecting backpressure on out (a full out channel simply makes the drain
// wait, rather than dropping tasks).
func Run(ctx context.Context, high, low <-chan Task, out chan<- Task) int {
	processed := 0

loop:
	for {
		// Prefer a buffered high-priority task without blocking.
		select {
		case t := <-high:
			out <- t
			processed++
			continue loop
		default:
		}

		select {
		case <-ctx.Done():
			break loop
		case t := <-high:
			out <- t
			processed++
		case t := <-low:
			out <- t
			processed++
		}
	}

	processed += drain(high, out)
	processed += drain(low, out)
	return processed
}

// drain flushes exactly the tasks buffered in ch right now -- a snapshot
// bound taken via len(ch) -- into out. Bounding the loop by the length
// observed at the start (rather than looping "while len(ch) > 0") guarantees
// termination even if a producer keeps sending concurrently: this drain
// only ever claims responsibility for what was already waiting.
func drain(ch <-chan Task, out chan<- Task) int {
	n := len(ch)
	for range n {
		t := <-ch
		out <- t
	}
	return n
}
```

### The runnable demo

The demo pre-loads two high-priority and two low-priority tasks, then calls
`Run` with an already-cancelled context — simulating a worker asked to shut
down before it ever got to consume anything — so the whole output comes from
the shutdown drain, in high-then-low order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/taskqueue"
)

func main() {
	high := make(chan taskqueue.Task, 4)
	low := make(chan taskqueue.Task, 4)
	out := make(chan taskqueue.Task, 8)

	high <- taskqueue.Task{ID: "restart-payment-worker", Priority: "high"}
	low <- taskqueue.Task{ID: "send-weekly-digest", Priority: "low"}
	high <- taskqueue.Task{ID: "rotate-expiring-cert", Priority: "high"}
	low <- taskqueue.Task{ID: "compact-old-logs", Priority: "low"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate an already-shutting-down worker: drain what's buffered

	processed := taskqueue.Run(ctx, high, low, out)
	close(out)

	fmt.Printf("processed %d tasks (high priority first):\n", processed)
	for t := range out {
		fmt.Printf("  [%s] %s\n", t.Priority, t.ID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
processed 4 tasks (high priority first):
  [high] restart-payment-worker
  [high] rotate-expiring-cert
  [low] send-weekly-digest
  [low] compact-old-logs
```

### Tests

`TestRunOnAlreadyCancelledContextDrainsBufferedTasks` cancels the context
*before* calling `Run` at all, which makes the whole test fully
deterministic — no goroutines, no timing — and still exercises the real
drain path end to end. `TestRunPrefersHighPriorityDuringSteadyState`
pre-loads both queues and gives `Run` a short real timeout so it processes
everything buffered and then returns on its own once the context expires;
because every task is buffered up front, the assertion on output order is
still exact. `TestDrainStopsAtSnapshotLength` and `TestDrainEmptyChannel`
test the drain helper directly, including that it never blocks on an empty
channel.

Create `taskqueue_test.go`:

```go
package taskqueue

import (
	"context"
	"testing"
	"time"
)

func TestRunOnAlreadyCancelledContextDrainsBufferedTasks(t *testing.T) {
	t.Parallel()

	high := make(chan Task, 4)
	low := make(chan Task, 4)
	out := make(chan Task, 8)

	high <- Task{ID: "h1", Priority: "high"}
	high <- Task{ID: "h2", Priority: "high"}
	low <- Task{ID: "l1", Priority: "low"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Run is ever called

	processed := Run(ctx, high, low, out)
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
	close(out)

	var gotIDs []string
	for t := range out {
		gotIDs = append(gotIDs, t.ID)
	}
	want := []string{"h1", "h2", "l1"}
	if len(gotIDs) != len(want) {
		t.Fatalf("out = %v, want %v", gotIDs, want)
	}
	for i, id := range want {
		if gotIDs[i] != id {
			t.Errorf("out[%d] = %q, want %q (high priority must drain before low)", i, gotIDs[i], id)
		}
	}
}

func TestRunPrefersHighPriorityDuringSteadyState(t *testing.T) {
	t.Parallel()

	high := make(chan Task, 4)
	low := make(chan Task, 4)
	out := make(chan Task, 8)

	// Pre-load both queues before Run ever looks at them, so the very first
	// non-blocking high-priority check already has a match.
	high <- Task{ID: "h1"}
	low <- Task{ID: "l1"}
	high <- Task{ID: "h2"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	processed := Run(ctx, high, low, out)
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
	close(out)

	var gotIDs []string
	for t := range out {
		gotIDs = append(gotIDs, t.ID)
	}
	// Both high-priority tasks must come out before the low-priority one.
	if len(gotIDs) != 3 || gotIDs[0] != "h1" || gotIDs[1] != "h2" || gotIDs[2] != "l1" {
		t.Fatalf("out = %v, want [h1 h2 l1]", gotIDs)
	}
}

func TestDrainStopsAtSnapshotLength(t *testing.T) {
	t.Parallel()

	ch := make(chan Task, 4)
	ch <- Task{ID: "a"}
	ch <- Task{ID: "b"}
	out := make(chan Task, 4)

	n := drain(ch, out)
	if n != 2 {
		t.Fatalf("drain returned %d, want 2", n)
	}
	close(out)
	var got []string
	for t := range out {
		got = append(got, t.ID)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("out = %v, want [a b]", got)
	}
}

func TestDrainEmptyChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan Task, 2)
	out := make(chan Task, 2)

	if n := drain(ch, out); n != 0 {
		t.Fatalf("drain returned %d, want 0", n)
	}
}
```

## Review

`Run` is correct when it never returns a low-priority task ahead of a
high-priority one that was already buffered, and when it flushes every
buffered task on shutdown without blocking forever. The common mistake this
design avoids is writing the shutdown drain as `for len(ch) > 0`, which
looks equivalent to the snapshot-bound version in every simple test but is
not provably terminating — under `-race` with a concurrent producer still
sending, that shape can loop far longer than the caller expects, or in the
worst case never converge if the producer never stops. Bounding the drain by
`len(ch)` taken once is what makes "flush what was already there" a real
guarantee instead of a best effort. Run `go test -count=1 ./...` (add
`-race` to also confirm there is no data race on the shared channels under
concurrent producers).

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the infinite form and the Go 1.22 integer `range` form used here.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the non-blocking `select`/`default` pattern used for the priority check.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees that make a channel send/receive pair safe to reason about under `-race`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-two-phase-commit-coordinator.md](22-two-phase-commit-coordinator.md) | Next: [24-shard-leader-detector.md](24-shard-leader-detector.md)
