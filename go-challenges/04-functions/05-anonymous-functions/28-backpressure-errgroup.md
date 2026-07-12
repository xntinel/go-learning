# Exercise 28: Backpressure-Bounded Processing using errgroup Goroutine Literals

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

Queuing every request behind a slow downstream dependency, instead of
shedding load once it's saturated, is how one overloaded shard turns into a
cascading outage. This module builds a load-shedding processor: one
`errgroup` goroutine literal per request, all competing for a fixed-capacity
buffered channel standing in for scarce downstream capacity, where losing
that race means shedding the request instead of blocking for it.

This module is fully self-contained. It uses `golang.org/x/sync/errgroup`;
nothing here imports another exercise.

## What you'll build

```text
loadshed/                     module example.com/loadshed
  go.mod                      requires golang.org/x/sync
  loadshed.go                  Result, Process: errgroup literals racing for a semaphore
  loadshed_test.go               capacity 0 sheds all, headroom processes all, peak bound
  cmd/demo/main.go              capacity 0 vs capacity >= demand
```

- Files: `loadshed.go`, `loadshed_test.go`, `cmd/demo/main.go`.
- Implement: `Process(ctx, requests, capacity, handle)` launching one `errgroup` goroutine literal per request, each doing a non-blocking `select` against a buffered channel semaphore of size `capacity`, writing only `results[i]`.
- Test: `capacity == 0` sheds every request deterministically; `capacity >= len(requests)` processes every request deterministically; peak concurrent `handle` calls never exceed `capacity` under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
go get golang.org/x/sync/errgroup@v0.10.0
```

### A non-blocking select is what turns a queue into a shed

Every request gets its own `g.Go(func() error { ... })` literal, so all of
them are submitted and can start running concurrently. Inside, each literal
attempts `sem <- struct{}{}` on a buffered channel of capacity `capacity`
inside a `select` with a `default` case. If the send succeeds, the literal
holds a slot, runs `handle`, and releases the slot via a deferred receive.
If the send would block — the channel is full — the `default` case fires
immediately and the literal records the request as shed, *without ever
calling `handle`*. That `default` case is the entire backpressure
mechanism: nothing here queues a sixth request behind five in-flight ones;
it refuses the sixth outright, which is what keeps total in-flight work
bounded by `capacity` no matter how many requests arrive at once. Because
shedding is an expected, routine outcome and not a failure of the pipeline
itself, every literal returns `nil` to `errgroup` — a shed or a `handle`
error is recorded in that request's own `results[i]` slot, never surfaced
through `Wait`, so one saturated request can never cancel or short-circuit
its siblings.

Create `loadshed.go`:

```go
package loadshed

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Result is what happened to one request: either it was Processed (and Err
// holds whatever the handler returned), or it was shed because no capacity
// was free.
type Result struct {
	Processed bool
	Err       error
}

// Process launches one errgroup goroutine literal per request. Every
// literal competes for a slot in sem, a fixed-capacity buffered channel
// standing in for scarce downstream capacity (a connection pool, a worker
// slot, a rate-limited API). A literal that cannot acquire a slot
// immediately -- the non-blocking select's default case -- sheds its
// request instead of queuing behind the others; that refusal to block is
// what keeps total in-flight work bounded by capacity no matter how many
// requests arrive at once. Each literal writes only results[i], its own
// slot, so the shared slice needs no lock even though every literal can
// run concurrently.
func Process(ctx context.Context, requests []string, capacity int, handle func(context.Context, string) error) []Result {
	results := make([]Result, len(requests))
	sem := make(chan struct{}, capacity)

	g, gctx := errgroup.WithContext(ctx)
	for i, req := range requests {
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				results[i] = Result{Processed: true, Err: handle(gctx, req)}
			default:
				results[i] = Result{Processed: false}
			}
			// Shedding is an expected outcome, not a group-fatal error: it
			// must never cancel gctx or stop sibling literals from getting
			// their own chance at a slot, so every literal returns nil here.
			return nil
		})
	}
	_ = g.Wait()
	return results
}
```

### The runnable demo

The demo shows the two deterministic extremes: no spare capacity at all,
and capacity that fully covers demand.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/loadshed"
)

func main() {
	requests := []string{"r0", "r1", "r2", "r3", "r4"}
	handle := func(context.Context, string) error { return nil }

	// No spare capacity at all: every request is shed, deterministically.
	shedAll := loadshed.Process(context.Background(), requests, 0, handle)
	processed, shed := count(shedAll)
	fmt.Printf("capacity=0 processed=%d shed=%d\n", processed, shed)

	// Capacity covers demand: nothing is ever shed, deterministically.
	shedNone := loadshed.Process(context.Background(), requests, len(requests), handle)
	processed, shed = count(shedNone)
	fmt.Printf("capacity=%d processed=%d shed=%d\n", len(requests), processed, shed)
}

func count(results []loadshed.Result) (processed, shed int) {
	for _, r := range results {
		if r.Processed {
			processed++
		} else {
			shed++
		}
	}
	return processed, shed
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
capacity=0 processed=0 shed=5
capacity=5 processed=5 shed=0
```

### Tests

`TestProcessCapacityZeroShedsEverything` uses a zero-capacity channel,
where a non-blocking send can never succeed, to deterministically prove
every request is shed and `handle` is never called. `TestProcessCapacityCoversDemandProcessesAll`
uses `capacity == len(requests)`, guaranteeing every literal acquires a
slot, to deterministically prove nothing is shed when there's no
contention. `TestProcessNeverExceedsCapacityConcurrently` measures peak
concurrent `handle` invocations with an atomic counter and a short sleep
(the same peak-measurement idiom used to certify `SetLimit` elsewhere in
this chapter) against 20 requests and `capacity = 2`, asserting the peak
never exceeds capacity — a property the semaphore guarantees structurally,
not by timing luck — and that at least one request was shed given the
mismatch between demand and capacity.

Create `loadshed_test.go`:

```go
package loadshed

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessCapacityZeroShedsEverything(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	handle := func(context.Context, string) error {
		calls.Add(1)
		return nil
	}
	requests := []string{"a", "b", "c", "d", "e"}

	results := Process(context.Background(), requests, 0, handle)
	if len(results) != len(requests) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(requests))
	}
	for i, r := range results {
		if r.Processed {
			t.Errorf("results[%d].Processed = true, want false (capacity 0 must shed all)", i)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handle was called %d times, want 0", got)
	}
}

func TestProcessCapacityCoversDemandProcessesAll(t *testing.T) {
	t.Parallel()
	requests := []string{"a", "b", "c", "d", "e"}
	handle := func(_ context.Context, req string) error { return nil }

	results := Process(context.Background(), requests, len(requests), handle)
	for i, r := range results {
		if !r.Processed {
			t.Errorf("results[%d].Processed = false, want true (capacity >= demand must never shed)", i)
		}
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
	}
}

func TestProcessNeverExceedsCapacityConcurrently(t *testing.T) {
	t.Parallel()
	const capacity = 2
	var concurrent, peak atomic.Int64
	handle := func(context.Context, string) error {
		cur := concurrent.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		concurrent.Add(-1)
		return nil
	}

	requests := make([]string, 20)
	for i := range requests {
		requests[i] = "r"
	}

	results := Process(context.Background(), requests, capacity, handle)
	if got := peak.Load(); got > capacity {
		t.Fatalf("peak concurrent handle calls = %d, want <= %d", got, capacity)
	}

	var processed, shed int
	for _, r := range results {
		if r.Processed {
			processed++
		} else {
			shed++
		}
	}
	if processed+shed != len(requests) {
		t.Fatalf("processed(%d)+shed(%d) = %d, want %d", processed, shed, processed+shed, len(requests))
	}
	if shed == 0 {
		t.Fatal("expected at least one shed request when demand (20) far exceeds capacity (2)")
	}
}
```

## Review

`Process` is correct when three things hold under `-race`: peak concurrent
`handle` invocations never exceed `capacity` (mechanism-guaranteed by the
channel's buffer, not by timing), every request ends up either processed or
shed with no request unaccounted for, and shedding never cancels or blocks
a sibling literal. The subtlety worth internalizing is why the tasks return
`nil` unconditionally: if a shed request's `default` case returned an error
instead, `errgroup`'s first-error-cancels-the-rest semantics (the same
mechanism the fan-out exercise in this chapter relies on) would turn a
routine, expected shed into a cascading cancellation of every other
in-flight request — exactly the failure mode backpressure exists to
prevent. Load shedding and fatal errors need two different channels out of
a goroutine literal, and conflating them is the one way this pattern
silently breaks.

## Resources

- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup)
- [Go Language Specification: Select statements](https://go.dev/ref/spec#Select_statements)
- [Go blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-canary-flag-iife.md](27-canary-flag-iife.md) | Next: [29-request-logging-deferred.md](29-request-logging-deferred.md)
