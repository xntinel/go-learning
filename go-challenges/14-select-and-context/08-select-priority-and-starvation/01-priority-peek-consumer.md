# Exercise 1: Cancellation-first priority consumer for a request handler

A request handler often pulls from two internal channels — an urgent one and a
best-effort one — and must abandon both the instant the request context is
cancelled. This exercise builds that consumer as a two-step `select`: a strict
cancellation check and a non-blocking high-priority peek, then a blocking
fall-through over both channels. It is the primitive every later module builds on.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
priority/                    module example.com/priority
  go.mod
  priority.go                type Item; func Priority(ctx, hi, lo) (Item, bool)
  cmd/
    demo/
      main.go                fills hi and lo with 100 items each, drains, prints counts
  priority_test.go           contract table + cancellation-before-high + Example
```

Files: `priority.go`, `cmd/demo/main.go`, `priority_test.go`.
Implement: `Priority(ctx, hi, lo) (Item, bool)` — prefer `hi`, fall through to
`lo`, and return `(Item{}, false)` the instant `ctx` is cancelled, with
cancellation strictly winning over a buffered high item.
Test: prefers high when both ready; reads low only when high is empty; stops on
cancel; deadline path returns promptly; cancellation strictly beats a buffered
high item; `ExamplePriority` output.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/08-select-priority-and-starvation/01-priority-peek-consumer/cmd/demo
cd go-solutions/14-select-and-context/08-select-priority-and-starvation/01-priority-peek-consumer
```

### Why three selects, not two

The naive two-channel priority idiom is a peek `select { case <-hi: ...; default:
}` followed by a blocking `select` over `hi` and `lo`. That gives high preference
over low, but it is not enough here, because the consumer must also honor
cancellation — and *how* you fold `ctx.Done()` in changes the behavior.

If you merge cancellation into the peek — `select { case <-ctx.Done(): ...; case
v := <-hi: ...; default: }` — you hit the uniform-random rule: when the context
is already cancelled *and* `hi` has a buffered item, both cases are ready, and
`select` picks one at random. Roughly half the time a cancelled request is handed
a high item anyway. For a request-scoped consumer that is wrong: once the request
is gone, no more work should be produced for it.

So this consumer uses three selects in sequence:

1. A strict, non-blocking `ctx.Done()` check. Cancellation is tested *alone*
   first, so it deterministically wins over any buffered high item.
2. A strict, non-blocking `hi` peek. If high has work, take it; low never gets a
   look this iteration.
3. A blocking fall-through over `ctx.Done()`, `hi`, and `lo`. This is the only
   place the goroutine parks. `ctx.Done()` appears here too so that an *idle*
   consumer, blocked with both channels empty, still wakes on a later cancel.

The cost is one extra `select` per call versus the two-select form — a few
nanoseconds against the channel operations. What you buy is a deterministic
guarantee: cancellation preempts everything, high preempts low, and an idle
consumer never busy-spins because it always lands on the blocking fall-through.

Create `priority.go`:

```go
package priority

import "context"

// Item is a unit of work flowing through a priority consumer.
type Item struct {
	Label string
	N     int
}

// Priority returns the next Item, preferring hi over lo, and abandons the
// consumer the instant ctx is cancelled. Cancellation is checked in its own
// select before the high-priority peek, so it strictly wins even when hi holds
// a buffered item; a pure hi-vs-ctx peek would pick randomly between them.
func Priority(ctx context.Context, hi, lo <-chan Item) (Item, bool) {
	for {
		// 1. Cancellation, strictly first and non-blocking.
		select {
		case <-ctx.Done():
			return Item{}, false
		default:
		}
		// 2. High-priority peek, non-blocking.
		select {
		case v := <-hi:
			return v, true
		default:
		}
		// 3. Blocking fall-through: the only place this loop parks.
		select {
		case <-ctx.Done():
			return Item{}, false
		case v := <-hi:
			return v, true
		case v := <-lo:
			return v, true
		}
	}
}
```

### The runnable demo

The demo preserves the original scenario: fill `hi` and `lo` with 100 items each,
drain 200 times, and print the counts. Because the peek wins every time `hi` has
data, all 100 high items come out first, then the 100 low items — a live
demonstration that strict priority drains high completely before touching low.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/priority"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hi := make(chan priority.Item, 100)
	lo := make(chan priority.Item, 100)
	for i := range 100 {
		hi <- priority.Item{Label: "hi", N: i}
	}
	for i := range 100 {
		lo <- priority.Item{Label: "lo", N: i}
	}

	hiCount, loCount := 0, 0
	for range 200 {
		got, ok := priority.Priority(ctx, hi, lo)
		if !ok {
			break
		}
		if got.Label == "hi" {
			hiCount++
		} else {
			loCount++
		}
	}
	fmt.Println("hi:", hiCount, "lo:", loCount)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hi: 100 lo: 100
```

### Tests

The table pins the two states of the peek (high wins when ready; low is read only
when high is empty), the two cancellation exits (manual cancel and deadline), and
the starvation property (with both channels pre-filled, high drains entirely
before low). `TestPriorityAlternatesAcrossManyCalls` is the canary: if either
count is zero the loop is broken or priority has starved a channel to nothing.
`TestPriorityPeekHonorsContext` is the strict-cancellation proof: with a high item
buffered and the context cancelled, the next call must return `false` — if the
strict pre-check were merged into the high peek, this would flake.

Create `priority_test.go`:

```go
package priority

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPriorityPrefersHighWhenReady(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hi := make(chan Item, 1)
	lo := make(chan Item, 1)
	hi <- Item{Label: "hi", N: 1}
	lo <- Item{Label: "lo", N: 2}

	got, ok := Priority(ctx, hi, lo)
	if !ok {
		t.Fatal("Priority: ok = false, want true")
	}
	if got.Label != "hi" {
		t.Fatalf("Priority: got %q, want hi", got.Label)
	}
}

func TestPriorityFallsThroughToLowWhenHighEmpty(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hi := make(chan Item)
	lo := make(chan Item, 1)
	lo <- Item{Label: "lo", N: 1}

	got, ok := Priority(ctx, hi, lo)
	if !ok {
		t.Fatal("Priority: ok = false, want true")
	}
	if got.Label != "lo" {
		t.Fatalf("Priority: got %q, want lo", got.Label)
	}
}

func TestPriorityStopsOnContextDone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hi := make(chan Item)
	lo := make(chan Item)

	if _, ok := Priority(ctx, hi, lo); ok {
		t.Fatal("Priority: ok = true after cancel, want false")
	}
}

func TestPriorityContextDeadlinePropagates(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	hi := make(chan Item)
	lo := make(chan Item)

	start := time.Now()
	_, ok := Priority(ctx, hi, lo)
	elapsed := time.Since(start)
	if ok {
		t.Fatal("Priority: ok = true, want false on deadline")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("Priority returned in %v, want ~30ms", elapsed)
	}
}

func TestPriorityAlternatesAcrossManyCalls(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hi := make(chan Item, 50)
	lo := make(chan Item, 50)
	for i := range 50 {
		hi <- Item{Label: "hi", N: i}
	}
	for i := range 50 {
		lo <- Item{Label: "lo", N: i}
	}

	hiCount, loCount := 0, 0
	for i := range 100 {
		got, ok := Priority(ctx, hi, lo)
		if !ok {
			t.Fatalf("iteration %d: ok = false", i)
		}
		if got.Label == "hi" {
			hiCount++
		} else {
			loCount++
		}
	}
	if hiCount != 50 || loCount != 50 {
		t.Fatalf("hi=%d lo=%d, want 50/50 (high drains before low)", hiCount, loCount)
	}
}

func TestPriorityPeekHonorsContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	hi := make(chan Item, 1)
	hi <- Item{Label: "hi"} // a high item is buffered and ready
	lo := make(chan Item)
	cancel() // ... but the request is already cancelled

	got, ok := Priority(ctx, hi, lo)
	if ok {
		t.Fatalf("Priority: returned %+v, want cancellation to win", got)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

func ExamplePriority() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hi := make(chan Item, 1)
	hi <- Item{Label: "urgent"}
	got, _ := Priority(ctx, hi, make(chan Item))
	fmt.Println(got.Label)
	// Output: urgent
}
```

## Review

The consumer is correct when three preferences hold in order: a cancelled context
returns `(Item{}, false)` even if `hi` is non-empty; a non-empty `hi` is served
before `lo`; and an idle consumer parks (never spins) until a channel or the
context wakes it. The two mistakes that break it are folding `ctx.Done()` into the
high peek (which makes cancellation only *probably* win — `TestPriorityPeekHonorsContext`
catches that) and adding a `default` to the fall-through (which turns the idle
path into a CPU spin; no test can see 100% CPU, so you must keep the `default`
out of the fall-through by inspection). Run `go test -race` and confirm
`TestPriorityAlternatesAcrossManyCalls` sees exactly 50/50: high drains completely
before low, which is strict priority working as designed, not a fairness bug —
Exercise 2 adds the fairness that bounds it.

## Resources

- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the uniform-random choice among ready cases.
- [Go Concurrency Patterns (Pike, 2012)](https://go.dev/talks/2012/concurrency.slide) — select and cancellation idioms.
- [`context` package](https://pkg.go.dev/context) — `Done`, `Err`, `WithCancel`, `WithTimeout`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fair-priority-drain.md](02-fair-priority-drain.md)
