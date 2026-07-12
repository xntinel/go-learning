# Exercise 14: Contiguous Watermark Offset-Commit Consumer

**Level: Advanced**

An at-least-once broker consumer (Kafka, SQS) processes messages with bounded
concurrency and, for throughput, finishes them out of order. The trap is the
commit: if it commits the highest *completed* offset, a crash-and-restart resumes
past an offset that was still in flight and that message is lost forever. The fix
is to commit only the highest *contiguous* processed offset, so that replaying
from committed+1 can never skip unprocessed work. This exercise builds a consumer
whose committer goroutine ranges a results channel and advances a watermark across
the contiguous run of completed offsets, holding the line at any gap.

This module is self-contained: its own module, a `watermark` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
watermark/                   independent module: example.com/watermark
  go.mod                     go 1.26
  watermark.go               type Msg, Consumer; New; Run advances a contiguous watermark
  cmd/demo/main.go           runnable demo: a gap holds the watermark, filling it lets it jump
  watermark_test.go          hold-then-jump invariant, monotonicity, all-done, bounded concurrency, goleak
```

- Files: `watermark.go`, `cmd/demo/main.go`, `watermark_test.go`.
- Implement: `New(process func(Msg), concurrency int) *Consumer` and `func (c *Consumer) Run(in <-chan Msg) (committed int64)`, which ranges `in`, dispatches under a bounded worker set, and folds completions from an internal results channel into a contiguous watermark.
- Test: the watermark never passes a gap while a lower offset is in flight, then jumps when the gap fills; the watermark is non-decreasing; the final commit equals the last offset when all complete; no more than N workers run at once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak
go mod tidy
```

### Why "highest completed" is the wrong watermark

The committer's job is to record a durable promise: *every offset at or below the
committed mark is fully processed.* On restart the consumer replays from
committed+1. That promise is only safe if the mark advances across a **contiguous**
run. Consider offsets 1..5 with concurrency 4: workers finish 1, 2, 4, 5 quickly
but 3 is slow. If the committer naively committed the highest completed offset it
would commit 5, and a crash right then would restart at 6 — offset 3 never ran and
is silently dropped. At-least-once turned into at-most-once by a one-line bug.

The correct rule is a low-water mark. Track a set of completed offsets and a mark.
On each completion, fold forward while the *next* expected offset (mark+1) is in the
completed set, deleting it and bumping the mark, and stop the instant a predecessor
is missing. With 1, 2, 4, 5 done and 3 pending, the mark sits at 2: offsets 4 and 5
are processed but *held back* from the commit because 3 is the gap. The moment 3
completes, the fold runs 3, 4, 5 in a single advance and the mark jumps to 5.

The protocol has three collaborating parts, and the ownership of each channel is the
whole design:

1. **Dispatch** ranges `in` and, for every message, acquires one slot of a
   `concurrency`-sized semaphore before launching a worker. Acquiring before launch
   is what bounds live workers; the loop ends when the producer closes `in`.
2. **Workers** run `process` and then send their completed offset on a `results`
   channel. They never touch the watermark — they only report completion.
3. **The committer** is the sole owner of the watermark and the done-set. Because it
   is the only goroutine that reads or writes them, no lock is needed. It ranges
   `results` until dispatch closes it (after `wg.Wait`), folding each completion and
   advancing the mark across the contiguous run.

`results` is closed exactly once, by the single goroutine that owns it (the Run body,
after every worker has finished). That single close is what ends the committer's
`range` cleanly and lets it record the final mark — a fan-in close discipline applied
to a commit loop.

Create `watermark.go`:

```go
// Package watermark implements an at-least-once broker consumer that processes
// messages with bounded concurrency, may finish them out of order, yet only ever
// commits the highest CONTIGUOUS processed offset. That contiguity rule is what
// makes a crash-and-restart safe: the committed watermark is a promise that every
// offset at or below it is durably processed, so replaying from committed+1 can
// never skip an unprocessed message.
package watermark

import (
	"sync"
	"sync/atomic"
)

// Msg is one broker record. Offsets are an ascending run; the watermark starts at
// 0 and advances to an offset only once every offset from committed+1 up to it has
// completed. A gap (a lower offset still in flight) holds the line.
type Msg struct {
	Offset int64
	Body   string
}

// Consumer processes messages with a bounded worker set and folds completions into
// a contiguous watermark. A Consumer is single-use: call Run exactly once.
type Consumer struct {
	process     func(Msg)
	concurrency int
	committed   atomic.Int64

	// onResult, when non-nil, is called by the committer goroutine after each
	// completion is folded in, with the offset that completed and the watermark
	// that resulted. It is a test seam for observing the watermark deterministically
	// without exposing internal state; production code leaves it nil.
	onResult func(offset, committed int64)
}

// New builds a Consumer that runs process on each message with at most concurrency
// workers active at once.
func New(process func(Msg), concurrency int) *Consumer {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Consumer{process: process, concurrency: concurrency}
}

// Committed returns the current watermark. Safe to call from any goroutine while
// Run executes; it observes the value the committer last stored.
func (c *Consumer) Committed() int64 { return c.committed.Load() }

// Run ranges in, processes each message under a bounded worker set, and reports
// each completion over an internal results channel. A single committer goroutine
// ranges that results channel and advances the watermark across the contiguous run
// of completed offsets, never past an offset whose predecessor is still in flight.
// Run returns the final committed offset after every message has been processed.
func (c *Consumer) Run(in <-chan Msg) (committed int64) {
	results := make(chan int64)               // each worker sends its completed offset here
	sem := make(chan struct{}, c.concurrency) // bounds workers to concurrency
	committerDone := make(chan struct{})

	// The committer OWNS the watermark: it is the only goroutine that reads or
	// writes the done set and advances committed, so no lock guards them. It ranges
	// results until Run closes the channel, then records the final watermark.
	go func() {
		defer close(committerDone)
		done := make(map[int64]struct{})
		var mark int64
		for off := range results {
			done[off] = struct{}{}
			// Advance across every contiguous completed offset above the mark.
			for {
				next := mark + 1
				if _, ok := done[next]; !ok {
					break // predecessor still in flight: hold the line
				}
				delete(done, next)
				mark = next
			}
			c.committed.Store(mark)
			if c.onResult != nil {
				c.onResult(off, mark)
			}
		}
	}()

	// Dispatch: the producer (whoever owns in) closes in; this loop ends when it
	// does. Acquiring sem before launching bounds live workers to concurrency.
	var wg sync.WaitGroup
	for msg := range in {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			c.process(msg)
			results <- msg.Offset
		})
	}
	wg.Wait()      // every worker has sent its offset
	close(results) // exactly one close, by the sole owner of results
	<-committerDone

	return c.committed.Load()
}
```

### The runnable demo

The demo makes the invariant visible without any timing. Run A feeds offsets
1, 2, 4, 5 — offset 3 is missing, as if still in flight — and even though 4 and 5
fully process, the watermark holds at 2. Run B feeds the full 1..5, and the
watermark reaches 5 because the gap is filled. Both final values are deterministic
regardless of completion order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/watermark"
)

// feed pushes the given offsets as messages and closes the channel, so the
// consumer's dispatch range terminates. The producer owns and closes the channel.
func feed(offsets ...int64) <-chan watermark.Msg {
	in := make(chan watermark.Msg, len(offsets))
	for _, off := range offsets {
		in <- watermark.Msg{Offset: off, Body: fmt.Sprintf("payload-%d", off)}
	}
	close(in)
	return in
}

func main() {
	// process does no real work here; the point is the commit protocol, not the
	// payload. Concurrency 4 lets offsets finish out of order.
	noop := func(watermark.Msg) {}

	// Run A: offset 3 is missing (still in flight from the consumer's view). Even
	// though 4 and 5 fully process, the watermark holds at 2 because the contiguous
	// run stops at the gap. A restart would correctly replay from offset 3.
	a := watermark.New(noop, 4)
	committedA := a.Run(feed(1, 2, 4, 5))
	fmt.Printf("run A: processed offsets 1,2,4,5 (3 missing) -> committed=%d\n", committedA)

	// Run B: the gap is filled. The watermark jumps across 3,4,5 in one fold.
	b := watermark.New(noop, 4)
	committedB := b.Run(feed(1, 2, 3, 4, 5))
	fmt.Printf("run B: processed offsets 1,2,3,4,5           -> committed=%d\n", committedB)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
run A: processed offsets 1,2,4,5 (3 missing) -> committed=2
run B: processed offsets 1,2,3,4,5           -> committed=5
```

### Tests

`TestHoldsLineThenJumps` pins the invariant deterministically with a per-offset
gate, not timing. Offset 3 blocks on a channel while offsets 1, 2, 4, 5 complete;
the test drains the four ungated completions through the `onResult` seam, asserting
each observed watermark is at most 2, then asserts `Committed() == 2` before
releasing 3. Closing the gate lets 3 complete, and the test asserts the watermark
jumps to 5 in the single fold and that `Run` returns 5. `TestMonotonicAndAllDone`
records every watermark the committer produces and asserts the sequence never
decreases, and that the final commit equals the last offset when all 50 messages
complete. `TestBoundedConcurrency` uses an atomic gauge and a release barrier: it
waits until exactly `concurrency` workers are blocked in `process` (proving the
bound is reached), and the semaphore guarantees the gauge can never exceed it.
`TestMain` runs `goleak.VerifyTestMain`, which fails if any worker or the committer
outlives the tests.

Create `watermark_test.go`:

```go
package watermark

import (
	"runtime"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

// goleak fails the run if any goroutine (a worker or the committer) outlives the
// tests, proving Run closes the results channel and every goroutine returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func feed(offsets ...int64) chan Msg {
	in := make(chan Msg, len(offsets))
	for _, off := range offsets {
		in <- Msg{Offset: off}
	}
	close(in)
	return in
}

// TestHoldsLineThenJumps pins the core invariant deterministically with a per-offset
// gate rather than timing: offset 3 blocks while 4 and 5 complete, and the watermark
// must never pass 2; releasing 3 makes it jump straight to 5.
func TestHoldsLineThenJumps(t *testing.T) {
	gate3 := make(chan struct{})
	process := func(m Msg) {
		if m.Offset == 3 {
			<-gate3 // block until the test releases offset 3
		}
	}
	c := New(process, 8) // room for all five to be in flight at once

	type ev struct{ off, committed int64 }
	events := make(chan ev, 5) // buffered so the committer never blocks reporting
	c.onResult = func(off, committed int64) { events <- ev{off, committed} }

	done := make(chan int64, 1)
	go func() { done <- c.Run(feed(1, 2, 3, 4, 5)) }()

	// Offsets 1,2,4,5 complete without the gate. While 3 is in flight the contiguous
	// run stops at the gap, so the watermark can never exceed 2.
	seen := make(map[int64]bool)
	for len(seen) < 4 {
		e := <-events
		if e.committed > 2 {
			t.Fatalf("watermark = %d while offset 3 in flight, want <= 2", e.committed)
		}
		seen[e.off] = true
	}
	if seen[3] {
		t.Fatal("offset 3 committed while still gated")
	}
	if got := c.Committed(); got != 2 {
		t.Fatalf("committed = %d with 1,2,4,5 done and 3 pending, want 2", got)
	}

	// Fill the gap: the watermark folds across 3,4,5 in a single advance.
	close(gate3)
	last := <-events
	if last.off != 3 {
		t.Fatalf("final completion offset = %d, want 3", last.off)
	}
	if last.committed != 5 {
		t.Fatalf("committed after releasing 3 = %d, want 5", last.committed)
	}
	if final := <-done; final != 5 {
		t.Fatalf("Run returned %d, want 5", final)
	}
}

// TestMonotonicAndAllDone asserts the watermark is non-decreasing across the whole
// run and that, when every message completes, the final commit equals the last offset.
func TestMonotonicAndAllDone(t *testing.T) {
	const n = 50
	c := New(func(Msg) {}, 8)

	var seq []int64 // written only by the committer goroutine; read after Run returns
	c.onResult = func(_, committed int64) { seq = append(seq, committed) }

	offsets := make([]int64, n)
	for i := range n {
		offsets[i] = int64(i + 1)
	}

	final := c.Run(feed(offsets...))
	if final != n {
		t.Fatalf("final committed = %d, want %d", final, n)
	}

	// The committer folds completions serially, so recorded watermarks are in commit
	// order and must never step backwards.
	var prev int64
	for i, v := range seq {
		if v < prev {
			t.Fatalf("committed decreased at step %d: %d after %d", i, v, prev)
		}
		prev = v
	}
	if len(seq) != n {
		t.Fatalf("recorded %d commit events, want %d", len(seq), n)
	}
}

// TestBoundedConcurrency proves no more than N workers process at once, and that the
// bound is actually reached, using an atomic gauge and a release barrier.
func TestBoundedConcurrency(t *testing.T) {
	const concurrency = 4
	const total = 12

	var gauge, maxSeen atomic.Int64
	release := make(chan struct{})

	process := func(Msg) {
		cur := gauge.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		<-release // hold the worker so the gauge piles up to the bound
		gauge.Add(-1)
	}
	c := New(process, concurrency)

	done := make(chan int64, 1)
	go func() { done <- c.Run(feed(seqOffsets(total)...)) }()

	// The semaphore caps dispatch at `concurrency`, so exactly that many workers can
	// sit blocked in process. Waiting for the gauge to reach it proves the bound is
	// tight; it can never exceed it because a worker frees its slot only after process.
	for gauge.Load() < concurrency {
		runtime.Gosched()
	}
	if g := gauge.Load(); g > concurrency {
		t.Fatalf("gauge = %d, exceeds concurrency %d", g, concurrency)
	}

	close(release)
	if final := <-done; final != total {
		t.Fatalf("Run returned %d, want %d", final, total)
	}
	if m := maxSeen.Load(); m != concurrency {
		t.Fatalf("max concurrent = %d, want exactly %d", m, concurrency)
	}
}

func seqOffsets(n int) []int64 {
	out := make([]int64, n)
	for i := range n {
		out[i] = int64(i + 1)
	}
	return out
}
```

## Review

Correct here means the committed watermark is always the highest offset such that
every offset from 1 up to it has been processed — never a gap-skipping "highest
completed". The fold-forward loop guarantees it: the mark advances only while the
next expected offset is in the done-set and stops dead at the first missing one, so
a lower in-flight offset holds the line while higher completed offsets wait, held
back until the gap fills. `TestHoldsLineThenJumps` proves this without any sleep: a
gate keeps offset 3 in flight, the seam observes the watermark pinned at 2 across
all four other completions, and releasing 3 folds 3, 4, 5 in one advance to 5. The
committer owning the watermark and done-set exclusively is why no lock is needed;
the single `close(results)` after `wg.Wait` is why the committer's `range` ends
exactly once and records the final mark; the semaphore acquired before launch is
why concurrency is bounded, which `TestBoundedConcurrency` pins to exactly N. The
production bug this prevents is the silent data loss of committing a completed-but-
non-contiguous offset, which turns an at-least-once pipeline into one that drops
every message caught behind a slow predecessor at crash time.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in and single-owner-closes discipline the committer loop applies to commits.
- [pkg.go.dev: sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 launch-and-track helper used for the bounded worker set.
- [pkg.go.dev: sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64) — the lock-free watermark and concurrency gauge used here.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — verifies the committer and workers all exit and nothing leaks.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-per-tenant-isolating-webhook-dispatcher.md](13-per-tenant-isolating-webhook-dispatcher.md) | Next: [../07-done-channel-pattern/00-concepts.md](../07-done-channel-pattern/00-concepts.md)
