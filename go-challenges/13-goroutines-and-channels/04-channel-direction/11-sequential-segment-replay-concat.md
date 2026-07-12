# Exercise 11: Concatenate Ordered Segment Streams Into One Recovery Stream

**Level: Intermediate**

A write-ahead-log recovery routine replays a series of on-disk log segments in
strict order: segment `k+1` must not be applied until segment `k` has been fully
replayed, or the recovered state is wrong. Each segment is exposed as its own
receive-only stream, and the naive instinct — fan them in with a merge — is
exactly backwards, because a merge interleaves and destroys the ordering that
recovery depends on. This exercise builds `Concat`, the ordered opposite of
fan-in: it drains its inputs one at a time, in argument order, into a single
output it owns and closes exactly once.

This module is self-contained: its own module, a `concat` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
concat/                      independent module: example.com/concat
  go.mod                     go 1.26
  concat.go                  Concat[T any](cs ...<-chan T) <-chan T
  cmd/demo/main.go           runnable demo: replay three ordered log segments
  concat_test.go             strict order, exactly-once close, zero inputs, empty-segment skip, drain-before-advance
```

- Files: `concat.go`, `cmd/demo/main.go`, `concat_test.go`.
- Implement: generic `Concat(cs ...<-chan T) <-chan T` — a single drain goroutine that ranges over each input in order and forwards to one owned output.
- Test: the output is the exact in-order concatenation (not an interleaving), it closes exactly once after the last input drains, zero inputs yields an immediately-closed output, interleaved empty inputs are skipped, and a later segment is not read until the earlier one is exhausted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/04-channel-direction/11-sequential-segment-replay-concat/cmd/demo
cd go-solutions/13-goroutines-and-channels/04-channel-direction/11-sequential-segment-replay-concat
```

### Why one drain goroutine, not one per input

Fan-in (the previous chapter's `Merge`) launches one goroutine per input so all
inputs drain concurrently, then closes the output with a WaitGroup-and-lone-closer
once every input finishes. `Concat` needs the exact opposite consumption
discipline, and it gets there with a *single* goroutine instead of one per input.

The signature is all direction. Each input is `cs ...<-chan T`: receive-only, so
`Concat` may only drain a segment — the compiler forbids it from sending into a
segment or closing one. Segments are owned by whoever produced them; `Concat`
borrows them read-only. The output is a `chan T` created and owned internally and
returned narrowed to `<-chan T`, so the caller can only drain it and `Concat`
remains the sole party that closes it.

The ordering invariant is enforced structurally by the loop shape:

1. A single goroutine iterates the segments in argument order: `for _, c := range
   cs`.
2. For each segment it runs `for v := range c`, which receives until that segment
   is closed. This *fully drains* segment `c` before the outer loop can advance —
   the inner `range` cannot end until `c` is closed by its producer.
3. Only when the inner loop ends does the outer loop move to the next segment.
   There is no concurrency across segments, so there is no interleaving: every
   value from `cs[0]` is forwarded before `Concat` ever performs a receive on
   `cs[1]`.
4. A single `defer close(out)` fires when the outer loop finishes — after the
   last segment has drained. Because exactly one goroutine ever touches `out` and
   it closes `out` once, the exactly-once close is guaranteed by construction, not
   by coordination.

This is why `Concat` needs no `WaitGroup`, no mutex, and no closer goroutine: with
a single consumer there is no race to close, and strict serialization of the reads
is the ordering guarantee itself. The degenerate cases fall out for free — zero
inputs means the outer loop never iterates and the deferred close runs at once, and
an empty segment is an already-closed channel whose inner `range` executes zero
iterations, so the outer loop steps past it without stalling. The failure mode this
avoids is silent log reordering: an interleaving replay could apply a later
segment's writes before an earlier segment's, corrupting recovered state in a way
that no crash reports and that surfaces only as diverged data much later.

Create `concat.go`:

```go
package concat

// Concat drains its receive-only inputs strictly in argument order, forwarding
// every value to a single receive-only output, and closes that output exactly
// once after the final input drains. Each input is <-chan T, so Concat may only
// receive from it, never send or close; the output is a chan T internally,
// returned as <-chan T, so Concat is the sole owner that closes it.
func Concat[T any](cs ...<-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out) // exactly one close, after the last input drains
		for _, c := range cs {
			for v := range c { // fully drains c before advancing to the next input
				out <- v
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo replays three ordered log segments into one recovery stream. The middle
segment is empty — a segment whose records were all superseded — and must be
skipped without stalling. Because `Concat` is strictly ordered, printing the
values as they arrive is deterministic with no sorting needed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/concat"
)

// segment replays one on-disk log segment as a receive-only stream.
func segment(records ...string) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for _, r := range records {
			ch <- r
		}
	}()
	return ch
}

func main() {
	// Three ordered segments; the middle one is empty (a segment with no
	// surviving records) and must be skipped without stalling the replay.
	recovery := concat.Concat(
		segment("seg0/rec-a", "seg0/rec-b"),
		segment(), // empty segment, skipped
		segment("seg2/rec-a", "seg2/rec-b", "seg2/rec-c"),
	)

	for rec := range recovery {
		fmt.Println(rec)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
seg0/rec-a
seg0/rec-b
seg2/rec-a
seg2/rec-b
seg2/rec-c
```

### Tests

`TestConcatPreservesStrictSegmentOrder` feeds three segments whose producers all
emit eagerly and concurrently and asserts the output is their exact concatenation,
not an interleaving — the property a fan-in merge would violate.
`TestConcatClosesOutputExactlyOnceAfterLastInput` drains the stream, then asserts
a further receive reports closed immediately; `-race` catches any stray second
close. `TestConcatZeroInputsClosesImmediately` pins the degenerate case.
`TestConcatSkipsEmptySegments` interleaves empty segments before, between, and
after non-empty ones and asserts they are skipped without stalling or reordering.
`TestConcatWaitsForEarlierSegmentToDrain` is the load-bearing ordering test: a
later segment's producer is gated so it cannot emit until the earlier segment is
exhausted, and the gate can only open after `Concat` has drained the earlier
segment — so a `Concat` that read the later segment first would deadlock, caught by
the `collect` helper's `time.After` guard, and the observed order proves the
earlier segment is consumed in full before the later is touched. No `time.Sleep`
gates any assertion.

Create `concat_test.go`:

```go
package concat

import (
	"slices"
	"testing"
	"time"
)

// stream replays vals eagerly on its own goroutine and closes when done. The
// producer does not coordinate with any other segment, so if Concat interleaved
// its inputs the ordering test would observe a non-block-ordered result.
func stream(vals ...int) <-chan int {
	ch := make(chan int)
	go func() {
		defer close(ch)
		for _, v := range vals {
			ch <- v
		}
	}()
	return ch
}

// collect drains out to completion, failing if it does not finish in time so a
// misordered or stalling Concat surfaces as a failure rather than a hung test.
func collect(t *testing.T, out <-chan int) []int {
	t.Helper()
	var got []int
	timeout := time.After(2 * time.Second)
	for {
		select {
		case v, ok := <-out:
			if !ok {
				return got
			}
			got = append(got, v)
		case <-timeout:
			t.Fatalf("Concat stalled: output did not drain in time (got %v)", got)
		}
	}
}

func TestConcatPreservesStrictSegmentOrder(t *testing.T) {
	t.Parallel()

	// Both segments produce eagerly and concurrently. A fan-in merge would
	// interleave them nondeterministically; Concat must emit all of seg0, then
	// all of seg1, then all of seg2.
	seg0 := []int{0, 1, 2, 3, 4}
	seg1 := []int{10, 11, 12}
	seg2 := []int{20, 21, 22, 23}
	out := Concat(stream(seg0...), stream(seg1...), stream(seg2...))

	got := collect(t, out)
	want := slices.Concat(seg0, seg1, seg2)
	if !slices.Equal(got, want) {
		t.Fatalf("Concat order = %v, want strict concatenation %v", got, want)
	}
}

func TestConcatClosesOutputExactlyOnceAfterLastInput(t *testing.T) {
	t.Parallel()

	out := Concat(stream(1, 2), stream(3))
	got := collect(t, out)
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("got %v, want [1 2 3]", got)
	}
	// A further receive must report the channel closed, and immediately: the
	// single deferred close ran exactly once after the last input drained.
	select {
	case v, ok := <-out:
		if ok {
			t.Fatalf("output delivered %v after draining, want closed", v)
		}
	case <-time.After(time.Second):
		t.Fatal("output never closed after all inputs drained")
	}
}

func TestConcatZeroInputsClosesImmediately(t *testing.T) {
	t.Parallel()

	out := Concat[int]()
	select {
	case v, ok := <-out:
		if ok {
			t.Fatalf("zero-input Concat delivered %v, want closed", v)
		}
	case <-time.After(time.Second):
		t.Fatal("zero-input Concat never closed")
	}
}

func TestConcatSkipsEmptySegments(t *testing.T) {
	t.Parallel()

	// Empty segments before, between, and after non-empty ones must be skipped
	// without stalling the replay or corrupting the order.
	out := Concat(
		stream(),
		stream(1, 2),
		stream(),
		stream(3),
		stream(),
	)
	got := collect(t, out)
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("got %v, want [1 2 3] with empty segments skipped", got)
	}
}

func TestConcatWaitsForEarlierSegmentToDrain(t *testing.T) {
	t.Parallel()

	// seg1's producer is gated: it may not emit until seg0 is fully exhausted.
	// The gate closes only after seg0's producer has handed off every record,
	// which can only happen once Concat has drained seg0. If Concat tried to
	// receive from seg1 before draining seg0, seg1 would never be released and
	// the run would deadlock (caught by collect's timeout). Observing the exact
	// order 1..6 proves seg0 is consumed entirely before seg1 is touched.
	gate := make(chan struct{})

	seg0 := make(chan int)
	go func() {
		defer close(seg0)
		for _, v := range []int{1, 2, 3} {
			seg0 <- v
		}
		close(gate)
	}()

	seg1 := make(chan int)
	go func() {
		defer close(seg1)
		<-gate
		for _, v := range []int{4, 5, 6} {
			seg1 <- v
		}
	}()

	out := Concat[int](seg0, seg1)
	got := collect(t, out)
	want := []int{1, 2, 3, 4, 5, 6}
	if !slices.Equal(got, want) {
		t.Fatalf("Concat order = %v, want %v (later segment read before earlier drained)", got, want)
	}
}
```

## Review

`Concat` is correct when its output is the exact in-order concatenation of its
inputs — every value of `cs[k]` before any value of `cs[k+1]` — and the output
closes exactly once after the final input drains. The single-goroutine serial
drain is the whole guarantee: `for _, c := range cs` visits segments in argument
order, the inner `for v := range c` cannot advance until `c` is closed, so a later
segment is never received from until the earlier one is exhausted, and the lone
`defer close(out)` on one goroutine makes the close trivially exactly-once with no
WaitGroup or mutex. The strict-order test proves the no-interleaving property that
a merge would fail, and the gated-producer test proves segment `k+1` is not read
until segment `k` fully drains by making a wrong order deadlock. The production
bug this prevents is silent log reordering during recovery: interleaving segments
applies later writes before earlier ones and corrupts the recovered state, a fault
that surfaces only as diverged data long after the run "succeeds."

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the pipeline and fan-in patterns this exercise deliberately inverts into an ordered concatenation.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the receive-only input and receive-only output types that make Concat a drain-only, owns-the-close API.
- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) -- how `for v := range c` terminates on close, the mechanism that fully drains one segment before advancing.
- [`slices.Concat`](https://pkg.go.dev/slices#Concat) -- the slice analogue used in the test to state the expected in-order result.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-load-balancing-stream-distributor.md](10-load-balancing-stream-distributor.md) | Next: [12-bridge-nested-result-streams.md](12-bridge-nested-result-streams.md)
