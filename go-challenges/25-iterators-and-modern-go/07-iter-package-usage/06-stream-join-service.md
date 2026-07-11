# Exercise 6: Stream-Join Service — Fan-In, Dedupe, Re-expose

A real ingestion service rarely consumes one stream. It fans in several push sources, has to look at the head of each at once to order and deduplicate them on a key, and must hand the result back downstream as a single push stream that other stages can range over. This exercise builds `Join`: a fan-in over many already-key-sorted `iter.Seq[Record]` sources that bridges each to pull form, runs a k-way merge with key dedupe, and re-exposes the merged result as one push `iter.Seq[Record]` — the shape of a streaming stream-join.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
join.go              Record{Key,Val}; Join(sources ...iter.Seq[Record]) iter.Seq[Record]
cmd/
  demo/
    main.go          fan three keyed shards in, print the deduped key-sorted union
join_test.go         dedupe across sources, no-sources, and clean stop of every input
```

- Files: `join.go`, `cmd/demo/main.go`, `join_test.go`.
- Implement: `Join(sources ...iter.Seq[Record]) iter.Seq[Record]` returning the key-sorted union of several key-sorted inputs with each key collapsed to its first occurrence (lowest source index wins on ties).
- Test: dedupe duplicate keys spanning sources, handle zero sources, and prove an early `break` stops every input rather than draining them.
- Verify: `go test -run 'TestJoin' -race ./...`

Set up the module:

```bash
mkdir -p stream-join-service/cmd/demo && cd stream-join-service
go mod init example.com/stream-join-service
```

### Why a stream-join must pull, fan-in, and re-push

The inputs are push iterators: each `Record` shard owns its own loop and shoves values at whoever ranges over it. A stream-join cannot work against that grain. To order keys across N shards it has to compare the current head of every shard simultaneously and emit the smallest, then advance only the shard it emitted from — exactly the random, head-at-a-time lookahead the push model forbids. So the first move is to pull: inside the returned `Seq`, call `iter.Pull` on each source, turning every push shard into an independent `next`/`stop` pair, and `defer` every `stop` immediately so that no matter how the join ends — natural exhaustion, an early consumer `break`, a panic — every source goroutine is torn down.

With all heads pullable, the core is a k-way merge. Prime one `next()` per source to load each head, then repeat: scan the live heads for the smallest key (a strict `<` keeps the scan biased to the lowest index, so ties resolve to the earliest source — that is what "first writer wins" means here), capture that record, advance its source with one more `next()`, and yield it. Dedupe rides on top of one invariant: because every shard is key-sorted and we always emit the global minimum, all records sharing a key are emitted adjacently. So a single remembered `lastKey` is enough — if the record just selected repeats `lastKey`, skip it (we already emitted that key from a lower-index source) and loop without yielding. No set, no unbounded memory; the dedupe state is one key.

The third move is what makes this a service and not a one-shot function: the whole thing is wrapped as `func(yield func(Record) bool) { ... }`, i.e. it returns a push `iter.Seq[Record]`. Downstream stages range over it exactly like any other source; it can itself be a shard fed into another `Join`. And every `yield` is guarded — if it returns `false` the consumer broke out, the join returns at once, and the deferred `stop`s fan back out to halt all N parked source goroutines. Pull in, merge-and-dedupe in the middle, push back out: the same push → pull → push sandwich as a two-way merge, widened to N inputs with a key collapse in the seam.

Create `join.go`:

```go
// Create `join.go`
package streamjoin

import "iter"

// Record is one keyed event in a shard. Shards are key-sorted ascending.
type Record struct {
	Key string
	Val int
}

// Join fans several key-sorted push iterators into one key-sorted push
// iterator, collapsing duplicate keys: the first record seen for a key (lowest
// source index on a tie) is emitted and later records with that key are
// dropped. The returned Seq is itself a valid input to another Join.
func Join(sources ...iter.Seq[Record]) iter.Seq[Record] {
	return func(yield func(Record) bool) {
		n := len(sources)
		nexts := make([]func() (Record, bool), n)
		heads := make([]Record, n)
		live := make([]bool, n)
		for i, s := range sources {
			next, stop := iter.Pull(s)
			defer stop()
			nexts[i] = next
			heads[i], live[i] = next()
		}

		var lastKey string
		var haveLast bool
		for {
			best := -1
			for i := 0; i < n; i++ {
				if !live[i] {
					continue
				}
				if best == -1 || heads[i].Key < heads[best].Key {
					best = i
				}
			}
			if best == -1 {
				return
			}

			rec := heads[best]
			heads[best], live[best] = nexts[best]()

			if haveLast && rec.Key == lastKey {
				continue
			}
			lastKey = rec.Key
			haveLast = true
			if !yield(rec) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo models three shards of a keyed event stream — duplicate keys appear both within the fan-in and across shards — and joins them into one deduped, key-sorted stream that it ranges over. Because `Join` returns a push `Seq`, `slices.Collect` drains it like any ordinary iterator.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"
	"slices"

	"example.com/stream-join-service"
)

func main() {
	s1 := slices.Values([]streamjoin.Record{
		{Key: "a", Val: 1}, {Key: "c", Val: 3}, {Key: "e", Val: 5},
	})
	s2 := slices.Values([]streamjoin.Record{
		{Key: "b", Val: 2}, {Key: "c", Val: 30}, {Key: "d", Val: 4},
	})
	s3 := slices.Values([]streamjoin.Record{
		{Key: "a", Val: 100}, {Key: "f", Val: 6},
	})

	joined := slices.Collect(streamjoin.Join(s1, s2, s3))
	fmt.Printf("%d unique keys\n", len(joined))
	for _, r := range joined {
		fmt.Printf("%s=%d\n", r.Key, r.Val)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
6 unique keys
a=1
b=2
c=3
d=4
e=5
f=6
```

### Tests

`TestJoinDedupe` joins three shards whose keys overlap and asserts the output is the deduped key-sorted union with the lowest-index record winning each tie. `TestJoinNoSources` confirms a join over zero sources yields nothing. `TestJoinEarlyBreakStopsAll` is the one that proves the service is leak-free: it joins three tracked shards, breaks after the first record, then asserts every source ran its deferred cleanup (`stopped`) and none was drained to completion.

Create `join_test.go`:

```go
// Create `join_test.go`
package streamjoin

import (
	"iter"
	"slices"
	"testing"
)

// source is a tracked shard: it counts how many records it produced and records
// whether its deferred cleanup ran (which happens when iter.Pull's stop resumes
// it). Both fields are written on the shard's own coroutine and read by the test
// only after the Join has fully returned, so there is no race.
type source struct {
	recs     []Record
	produced int
	stopped  bool
}

func (s *source) seq() iter.Seq[Record] {
	return func(yield func(Record) bool) {
		defer func() { s.stopped = true }()
		for _, r := range s.recs {
			s.produced++
			if !yield(r) {
				return
			}
		}
	}
}

func TestJoinDedupe(t *testing.T) {
	t.Parallel()

	s1 := slices.Values([]Record{
		{Key: "a", Val: 1}, {Key: "c", Val: 3}, {Key: "e", Val: 5},
	})
	s2 := slices.Values([]Record{
		{Key: "b", Val: 2}, {Key: "c", Val: 30}, {Key: "d", Val: 4},
	})
	s3 := slices.Values([]Record{
		{Key: "a", Val: 100}, {Key: "f", Val: 6},
	})

	got := slices.Collect(Join(s1, s2, s3))
	want := []Record{
		{Key: "a", Val: 1},
		{Key: "b", Val: 2},
		{Key: "c", Val: 3},
		{Key: "d", Val: 4},
		{Key: "e", Val: 5},
		{Key: "f", Val: 6},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Join = %v, want %v", got, want)
	}
}

func TestJoinNoSources(t *testing.T) {
	t.Parallel()

	got := slices.Collect(Join())
	if len(got) != 0 {
		t.Fatalf("Join() = %v, want empty", got)
	}
}

func TestJoinEarlyBreakStopsAll(t *testing.T) {
	t.Parallel()

	sources := []*source{
		{recs: []Record{{Key: "a", Val: 1}, {Key: "d", Val: 4}, {Key: "g", Val: 7}}},
		{recs: []Record{{Key: "b", Val: 2}, {Key: "e", Val: 5}, {Key: "h", Val: 8}}},
		{recs: []Record{{Key: "c", Val: 3}, {Key: "f", Val: 6}, {Key: "i", Val: 9}}},
	}
	seqs := make([]iter.Seq[Record], len(sources))
	for i, s := range sources {
		seqs[i] = s.seq()
	}

	var got []Record
	for r := range Join(seqs...) {
		got = append(got, r)
		break
	}
	if len(got) != 1 || got[0].Key != "a" {
		t.Fatalf("early break got %v, want first record a", got)
	}

	// After the consumer broke at the first record, the deferred stops fired:
	// every shard's cleanup ran and none was driven to completion.
	for i, s := range sources {
		if !s.stopped {
			t.Fatalf("source %d was not stopped after early break", i)
		}
		if s.produced >= len(s.recs) {
			t.Fatalf("source %d over-consumed: produced %d of %d", i, s.produced, len(s.recs))
		}
	}
}
```

## Review

`Join` is correct when it pulls every source, defers every `stop` on the line after each `iter.Pull`, scans live heads for the strict minimum, advances only the emitted source, and collapses repeats against a single `lastKey`. The dedupe table is the algorithmic proof: keys that appear in two shards (`a` in sources 1 and 3, `c` in sources 1 and 2) collapse to the lowest-index record, and the strict `<` in the head scan is what makes the tie resolve to the earlier source rather than flapping. The no-sources case confirms the head scan reports `best == -1` immediately and the join ends without touching a nil slice.

The early-break test is the one that proves the service does not leak. When the consumer breaks at the first record, every source is parked mid-`yield`; the deferred `stop`s resume each parked goroutine with `yield` returning `false`, so each shard takes its `return` path and runs the deferred `stopped = true`. The per-source `produced` counters confirm none was drained. Delete a single `defer stop()` and that shard's goroutine lingers for the life of the program, holding whatever it wraps; forget to guard a `yield` and a consumer's `break` triggers the runtime's continued-iteration panic. The shape to keep is the widened sandwich: pull all inputs, defer all stops, merge-and-dedupe in the middle on a single remembered key, push one `Seq` back out.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the adapter that gives each shard the on-demand `next`/`stop` the k-way merge needs.
- [`iter.Seq`](https://pkg.go.dev/iter#Seq) — the single-value push iterator type `Join` both consumes and re-exposes.
- [`slices.Values`](https://pkg.go.dev/slices#Values) — turns the demo and test slices into the push shards `Join` fans in.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the design rationale for push and pull iterators and how a combinator converts between them.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-zip-generator-with-stop.md](05-zip-generator-with-stop.md) | Next: [07-job-queue-prefetch.md](07-job-queue-prefetch.md)
