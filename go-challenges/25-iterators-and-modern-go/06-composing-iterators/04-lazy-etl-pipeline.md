# Exercise 4: Lazy ETL Pipeline

A production ETL job rarely gets to load its input into memory: the source is a Kafka topic, a multi-gigabyte file, or a network cursor that is effectively unbounded. The discipline that makes such a job tractable is laziness — each record flows through parse, filter, map, batch, and take one at a time, and the pipeline pulls only as many records as the final stage actually needs. This exercise builds that pipeline as composed `iter.Seq` stages and proves, by counting pulls against a billion-element source, that the chain consumes exactly the records required to fill the requested batches and not one more.

This module is fully self-contained. It begins with its own `go mod init`, defines every stage it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
etl.go               Parse, Filter, Map, Batch (chunking + final flush), Take
cmd/
  demo/
    main.go          parse -> filter -> map -> batch -> take over a 1M source
etl_test.go          pipeline output + batching, exact-pull laziness, Parse drops, partial flush
```

- Files: `etl.go`, `cmd/demo/main.go`, `etl_test.go`.
- Implement: `Parse`, `Filter`, `Map`, `Take`, and the chunking stage `Batch[V](seq iter.Seq[V], size int) iter.Seq[[]V]`.
- Test: a `Parse -> Filter -> Map -> Batch -> Take` pipeline yields the right batches; over a billion-element source it pulls *exactly* the records needed to fill the taken batches; `Batch` flushes a short final batch; `Parse` drops unparseable records.
- Verify: `go test -race ./...`

### The pipeline is a chain of pull-driven stages

The five stages form a straight line: raw records enter `Parse`, which turns each into a typed value (and silently drops the ones that fail to parse); `Filter` keeps the values that match a predicate; `Map` rewrites them; `Batch` regroups the stream into fixed-size slices; and `Take` keeps the first few batches. Each stage is an `iter.Seq` that wraps the stage before it, so the composition `Take(Batch(Map(Filter(Parse(source, ...), ...), ...), size), n)` is one lazy object that has done no work yet. Work happens only when something ranges over the outermost stage, and every value is pulled on demand: `Take` asks `Batch` for a batch, `Batch` asks `Map` for values until it has `size` of them, `Map` asks `Filter`, `Filter` asks `Parse`, `Parse` asks the source. One pull at the top draws exactly the records it needs up through the chain.

The load-bearing property is that a *stop* at the top propagates all the way down. When `Take` has its `n` batches it returns; that makes `Batch`'s `yield` return `false`, so `Batch` returns; that makes `Map`'s `yield` return `false`, and the stop climbs link by link until the source's `yield` returns `false` and the source stops producing. This is why the pipeline can be pointed at an unbounded source without hanging: nothing downstream of the source ever loops on its own; each stage only pulls when pulled. Miss the `if !yield(...) { return }` check in any one stage and that link keeps draining its upstream after everyone above it has left — over an unbounded source, forever.

### Batch is the stage that buffers

`Batch` is the only stage that holds state between values: a growing slice. It appends each upstream value and, the instant the slice reaches `size`, yields that batch and starts a fresh one. Two details matter. First, it allocates a new backing slice for each batch (`make([]V, 0, size)`) rather than reusing one, because a consumer that retains a yielded batch must not see it overwritten by the next group — reusing the array is a classic aliasing bug. Second, after the `for v := range seq` loop ends (the source is exhausted), a partial batch may remain; `Batch` flushes it with a final `yield` so the last few records are not silently dropped. A non-positive `size` yields nothing, which keeps the stage total.

`Batch` interacts cleanly with `Take`'s exact-pull discipline. `Take` is written to yield first, then count, then return — so it pulls *exactly* `n` batches, never `n+1`. Because a full batch is yielded the moment its last element arrives, `Batch` does not pull an extra upstream value to "look ahead"; the element that completes batch `n` is the last one pulled when `Take` wants `n` batches. That is what makes the pull count exactly predictable, and it is what the laziness test pins down.

Create `etl.go`:

```go
package etl

import "iter"

// Parse converts each raw upstream value via convert, dropping any value for
// which convert reports ok == false. It pulls one upstream value per attempt
// and never materializes the source.
func Parse[A, B any](seq iter.Seq[A], convert func(A) (B, bool)) iter.Seq[B] {
	return func(yield func(B) bool) {
		for a := range seq {
			b, ok := convert(a)
			if !ok {
				continue
			}
			if !yield(b) {
				return
			}
		}
	}
}

// Filter yields only the values for which keep returns true.
func Filter[V any](seq iter.Seq[V], keep func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if keep(v) && !yield(v) {
				return
			}
		}
	}
}

// Map yields transform(v) for each upstream value, possibly changing the type.
func Map[A, B any](seq iter.Seq[A], transform func(A) B) iter.Seq[B] {
	return func(yield func(B) bool) {
		for a := range seq {
			if !yield(transform(a)) {
				return
			}
		}
	}
}

// Batch groups consecutive values into slices of length size, yielding each
// full batch as soon as it fills. A shorter final batch is flushed when the
// source ends mid-batch. Each batch is freshly allocated so yielded batches
// never alias one another. A non-positive size yields nothing.
func Batch[V any](seq iter.Seq[V], size int) iter.Seq[[]V] {
	return func(yield func([]V) bool) {
		if size <= 0 {
			return
		}
		batch := make([]V, 0, size)
		for v := range seq {
			batch = append(batch, v)
			if len(batch) == size {
				if !yield(batch) {
					return
				}
				batch = make([]V, 0, size)
			}
		}
		if len(batch) > 0 {
			yield(batch)
		}
	}
}

// Take yields at most the first n values and pulls exactly n from upstream
// (zero when n <= 0).
func Take[V any](seq iter.Seq[V], n int) iter.Seq[V] {
	return func(yield func(V) bool) {
		if n <= 0 {
			return
		}
		count := 0
		for v := range seq {
			if !yield(v) {
				return
			}
			count++
			if count == n {
				return
			}
		}
	}
}
```

Read the stages as the same loop with different bodies. `Parse` calls `convert`, and a `false` second result means "drop this record" — it `continue`s without yielding, so malformed input never reaches the rest of the pipeline. `Filter`'s `keep(v) && !yield(v)` yields only matching values and returns only on a downstream stop. `Map` changes the type parameter from `A` to `B`, which is why a stage can turn a stream of `int` into a stream of something else. `Batch` buffers and flushes as described. `Take` yields-then-counts so the upstream pull count is exactly `n`.

### The runnable demo

The demo wires the full chain over a source of a million records — each the string `"v=<i>"` — keeps the even numbers, doubles them, groups them three to a batch, and takes the first two batches. Because the chain is lazy, `Take(batches, 2)` stops everything after the second batch is full, so the "million-record" source is pulled only eleven times. The demo prints the source pull count to make that visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strconv"
	"strings"

	"example.com/etl"
)

func main() {
	pulled := 0
	source := func(yield func(string) bool) {
		for i := 0; i < 1_000_000; i++ {
			pulled++
			if !yield("v=" + strconv.Itoa(i)) {
				return
			}
		}
	}

	parse := func(s string) (int, bool) {
		_, after, found := strings.Cut(s, "=")
		if !found {
			return 0, false
		}
		n, err := strconv.Atoi(after)
		return n, err == nil
	}

	parsed := etl.Parse(source, parse)
	evens := etl.Filter(parsed, func(n int) bool { return n%2 == 0 })
	doubled := etl.Map(evens, func(n int) int { return n * 2 })
	batches := etl.Batch(doubled, 3)
	firstTwo := etl.Take(batches, 2)

	for b := range firstTwo {
		fmt.Println("batch:", b)
	}
	fmt.Println("source pulls:", pulled)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch: [0 4 8]
batch: [12 16 20]
source pulls: 11
```

### Tests

`TestPipelineBatchesAndIsLazy` is the headline. It builds the full `Parse -> Filter -> Map -> Batch -> Take` chain over a source that would yield a billion integers, collects the first two batches, and asserts both the batched output and the exact pull count. Two batches of three evens require the evens `0, 2, 4, 6, 8, 10`; to deliver the even `10` the source must be pulled through value `10`, which is values `0..10` — eleven pulls — and `Take` stops there, so nothing past `10` is produced. `TestBatchFlushesPartialFinalBatch` proves the short final batch is flushed. `TestParseDropsUnparseable` proves malformed records are skipped rather than crashing the pipeline.

Create `etl_test.go`:

```go
package etl

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func parseRec(s string) (int, bool) {
	_, after, found := strings.Cut(s, "=")
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(after)
	return n, err == nil
}

func equalBatches(a, b [][]int) bool {
	return slices.EqualFunc(a, b, func(x, y []int) bool { return slices.Equal(x, y) })
}

func TestPipelineBatchesAndIsLazy(t *testing.T) {
	t.Parallel()

	pulled := 0
	source := func(yield func(string) bool) {
		for i := 0; i < 1_000_000_000; i++ {
			pulled++
			if !yield("v=" + strconv.Itoa(i)) {
				return
			}
		}
	}

	parsed := Parse(source, parseRec)
	evens := Filter(parsed, func(n int) bool { return n%2 == 0 })
	doubled := Map(evens, func(n int) int { return n * 2 })
	batches := Batch(doubled, 3)
	firstTwo := Take(batches, 2)

	got := slices.Collect(firstTwo)
	want := [][]int{{0, 4, 8}, {12, 16, 20}}
	if !equalBatches(got, want) {
		t.Fatalf("pipeline = %v, want %v", got, want)
	}
	// Two batches of three evens need evens 0,2,4,6,8,10; the source must be
	// pulled through value 10, i.e. values 0..10 = 11 pulls, and not one more.
	if pulled != 11 {
		t.Fatalf("source pulled %d values, want exactly 11", pulled)
	}
}

func TestBatchFlushesPartialFinalBatch(t *testing.T) {
	t.Parallel()

	got := slices.Collect(Batch(slices.Values([]int{1, 2, 3, 4, 5}), 2))
	want := [][]int{{1, 2}, {3, 4}, {5}}
	if !equalBatches(got, want) {
		t.Fatalf("Batch = %v, want %v", got, want)
	}
}

func TestParseDropsUnparseable(t *testing.T) {
	t.Parallel()

	src := slices.Values([]string{"v=1", "junk", "v=2", "v=oops", "v=3"})
	got := slices.Collect(Parse(src, parseRec))
	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}
```

## Review

The pipeline is sound when every stage is a closure that does no work until ranged over and stops the instant its `yield` returns `false`. The decisive evidence is `TestPipelineBatchesAndIsLazy`: a billion-element source driven through the full chain is pulled exactly eleven times, which is only true if the stop signal from `Take` climbs through `Batch`, `Map`, `Filter`, and `Parse` to the source. Confirm the batched output is `[[0 4 8] [12 16 20]]`, that a five-element source batched by two flushes a trailing `[5]`, and that `Parse` drops `"junk"` and `"v=oops"` while keeping the numeric records.

Common mistakes for this pattern. The first is reusing one backing slice across batches in `Batch` — a consumer that keeps a batch then sees it mutated by the next group; allocating a fresh slice per batch avoids the aliasing. The second is forgetting to flush the partial final batch, which silently drops the last few records whenever the source length is not a multiple of `size`. The third is the familiar bare `yield(v)` with no stop check in some stage, which keeps draining an unbounded upstream after the consumer has left — here that means an ETL job that never terminates. The fourth is materializing the source first (collecting it into a slice "to make the code simpler"), which defeats the entire point: the pipeline exists precisely so the unbounded input is never fully held in memory.

## Resources

- [`iter` package](https://pkg.go.dev/iter) — the `Seq` type every ETL stage produces and consumes.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — how `for range` over a `func(yield func(V) bool)` drives the pull and how a downstream stop returns `false` up the chain.
- [`slices.Collect`](https://pkg.go.dev/slices#Collect) and [`slices.Values`](https://pkg.go.dev/slices#Values) — the slice sink and source used in the demo and tests.
- [Go Blog: An Introduction To Generics](https://go.dev/blog/intro-generics) — the type-parameter mechanics behind `Map[A, B]` and `Batch[V] iter.Seq[[]V]` changing a pipeline's element type.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-take-while-and-drop-while.md](03-take-while-and-drop-while.md) | Next: [05-log-top-n-pipeline.md](05-log-top-n-pipeline.md)
