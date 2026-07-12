# Exercise 2: Ordered Merge Across Shards

A basic fan-in emits values in arrival order, which is useless when the consumer needs them globally sorted — paginating a query whose rows are partitioned across database shards, or replaying time-ordered events split across replicas. Each shard already returns its slice in key order; your job is to merge N sorted streams into one sorted stream without buffering everything in memory, and to surface a shard failure rather than silently returning a short result. This exercise builds a streaming k-way merge over channels with per-source error propagation.

This module is fully self-contained: its own `go mod init`, every symbol defined inline, its own demo and tests. It imports no other exercise.

## What you'll build

```text
shardmerge.go        Record, SourceItem, StreamRecords, MergeOrdered (heap k-way merge)
cmd/
  demo/
    main.go          merge three sorted shards into one ordered stream
shardmerge_test.go   global-order check, fail-fast error propagation, empty case, race load
```

- Files: `shardmerge.go`, `cmd/demo/main.go`, `shardmerge_test.go`.
- Implement: `MergeOrdered(sources ...<-chan SourceItem) (<-chan Record, <-chan error)`, plus the `Record` and `SourceItem` types and a `StreamRecords` source constructor.
- Test: assert the merged stream is globally sorted by `Key` across all shards, that a mid-stream shard error is delivered on the error channel, that zero/empty sources close cleanly with a nil error, and that many shards merge race-free.
- Verify: `go test -race ./...`

### Why arrival order is not enough, and what a k-way merge does

If you fan in the shards with the basic `Merge` from exercise 1, you get every record exactly once, but in scheduler-decided arrival order. That cannot be globally sorted, because the record currently sitting in a fast shard's channel may have a larger key than one still in transit from a slow shard: emitting "whatever arrived first" can emit a key-9 record before a key-3 record that simply hadn't been received yet. Sorting per source is not the question; combining sorted sources into a sorted whole is.

The streaming answer is a k-way merge backed by a min-heap. The merge reads exactly one record ahead from every source — the *head* of each stream — and keeps those heads in a heap ordered by `Key`. The global minimum is always at the heap's root, because each source is internally sorted, so no unread record from any source can be smaller than that source's current head. The loop pops the root, emits it, then pulls the *next* record from the source that root came from and pushes it. When a source is exhausted it contributes no new head, so the heap shrinks; when the heap empties, every source is drained and the output closes. Memory is bounded by the number of sources, not the total record count: only one head per source is ever held. This is the same algorithm an external-sort merge phase or an LSM-tree compaction uses, expressed over channels.

### Carrying a terminal error alongside the values

A shard is not just "more data or done" — it can fail mid-stream. To distinguish a clean end from a failure, each source emits a `SourceItem` that is either a `Record` or a terminal `Err`. The merge's `pull` helper reads one item from a source and reports three outcomes: a record, clean exhaustion (the channel closed), or an error (the item carried `Err`). On an error the merge fails fast: it sends the error on a dedicated error channel and stops, abandoning the remaining heap. That is the correct default when a missing shard makes the whole ordered result untrustworthy — a half-complete "sorted" page is worse than an explicit error.

The error channel is buffered with capacity one. That lets the merge goroutine deposit the error and return without waiting for a reader, and it makes the consumer protocol simple: range the record channel to completion, then read the error channel exactly once. A clean run closes the error channel without sending, so that final read yields a nil error; a failed run leaves the error buffered for that read. Both `out` and `errc` are closed by the same `defer`s, so the consumer can rely on ranging `out` first and reading `errc` after.

Create `shardmerge.go`:

```go
package shardmerge

import "container/heap"

// Record is one element of a shard's sorted stream. Shards emit records in
// non-decreasing Key order.
type Record struct {
	Key   int64
	Value string
}

// SourceItem is one item produced by a shard channel: either a Record (Err nil)
// or a terminal failure (Err set). A shard emits its records in Key order and
// then closes; if it fails it emits a single item with Err set and closes.
type SourceItem struct {
	Record Record
	Err    error
}

// StreamRecords returns a source channel that emits each record in order and
// then closes. It is the clean-shard constructor used by the demo and tests.
func StreamRecords(recs ...Record) <-chan SourceItem {
	ch := make(chan SourceItem)
	go func() {
		defer close(ch)
		for _, r := range recs {
			ch <- SourceItem{Record: r}
		}
	}()
	return ch
}

// entry is a heap element: a record together with the index of the source it
// came from, so the next record can be pulled from the same source.
type entry struct {
	rec Record
	src int
}

type minHeap []entry

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].rec.Key < h[j].rec.Key }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *minHeap) Push(x any) { *h = append(*h, x.(entry)) }

func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

// MergeOrdered performs a streaming k-way merge of sorted shard streams into one
// globally Key-ordered stream. Each source must emit records in non-decreasing
// Key order. The returned record channel closes when every source is exhausted
// or when any source reports an error. In the error case the error is sent on
// the returned error channel (fail-fast: at most one error, remaining sources
// abandoned). The error channel is buffered with capacity one and is closed
// after the record channel, so the consumer ranges the records, then reads the
// error channel once to learn whether the stream ended cleanly (nil) or failed.
func MergeOrdered(sources ...<-chan SourceItem) (<-chan Record, <-chan error) {
	out := make(chan Record)
	errc := make(chan error, 1)

	go func() {
		defer close(errc)
		defer close(out)

		h := &minHeap{}

		// pull reads the next item from source i: a record (ok true), clean
		// exhaustion (ok false, err nil), or a failure (err set).
		pull := func(i int) (rec Record, ok bool, err error) {
			item, open := <-sources[i]
			if !open {
				return Record{}, false, nil
			}
			if item.Err != nil {
				return Record{}, false, item.Err
			}
			return item.Record, true, nil
		}

		// Prime the heap with the first record of each source.
		for i := range sources {
			rec, ok, err := pull(i)
			if err != nil {
				errc <- err
				return
			}
			if ok {
				heap.Push(h, entry{rec: rec, src: i})
			}
		}

		for h.Len() > 0 {
			e := heap.Pop(h).(entry)
			out <- e.rec

			rec, ok, err := pull(e.src)
			if err != nil {
				errc <- err
				return
			}
			if ok {
				heap.Push(h, entry{rec: rec, src: e.src})
			}
		}
	}()

	return out, errc
}
```

The two `defer`s run last-in-first-out, so `close(out)` runs before `close(errc)`: by the time the consumer sees `out` close and turns to read `errc`, any error has already been buffered. `pull` is the single place that interprets a source item, which keeps the priming loop and the main loop identical in their error handling — both forward an error and return, abandoning the heap. Because the merge only ever holds one head per source in the heap, a stream of a million records per shard costs the same memory as a stream of ten.

### The runnable demo

The demo defines three shards whose keys interleave — shard A holds 1 and 4, shard B holds 2 and 5, shard C holds 3 — and merges them into one stream printed in global key order. The output is deterministic because the merge emits by key, not by arrival.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/shardmerge"
)

func main() {
	a := shardmerge.StreamRecords(
		shardmerge.Record{Key: 1, Value: "alice"},
		shardmerge.Record{Key: 4, Value: "dora"},
	)
	b := shardmerge.StreamRecords(
		shardmerge.Record{Key: 2, Value: "bob"},
		shardmerge.Record{Key: 5, Value: "erin"},
	)
	c := shardmerge.StreamRecords(
		shardmerge.Record{Key: 3, Value: "carol"},
	)

	out, errc := shardmerge.MergeOrdered(a, b, c)
	for r := range out {
		fmt.Printf("%d:%s\n", r.Key, r.Value)
	}
	if err := <-errc; err != nil {
		fmt.Println("merge error:", err)
		return
	}
	fmt.Println("all shards merged cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1:alice
2:bob
3:carol
4:dora
5:erin
all shards merged cleanly
```

### Tests

The tests pin the four properties. `TestMergeOrderedProducesGloballySortedStream` merges three interleaved shards and asserts the output keys are non-decreasing and complete. `TestMergeOrderedPropagatesSourceError` gives one shard a terminal error after a couple of records and asserts that exact error arrives on the error channel. `TestMergeOrderedNoSources` checks the zero-source and all-empty cases close cleanly with a nil error. `TestMergeOrderedManyShardsRaceFree` merges many shards under `-race` and asserts global order and exact count.

Create `shardmerge_test.go`:

```go
package shardmerge

import (
	"errors"
	"testing"
)

func recordsFromKeys(keys ...int64) <-chan SourceItem {
	recs := make([]Record, len(keys))
	for i, k := range keys {
		recs[i] = Record{Key: k, Value: "v"}
	}
	return StreamRecords(recs...)
}

// failingSource emits one record per key in order, then a terminal error.
func failingSource(err error, keys ...int64) <-chan SourceItem {
	ch := make(chan SourceItem)
	go func() {
		defer close(ch)
		for _, k := range keys {
			ch <- SourceItem{Record: Record{Key: k, Value: "v"}}
		}
		ch <- SourceItem{Err: err}
	}()
	return ch
}

func TestMergeOrderedProducesGloballySortedStream(t *testing.T) {
	t.Parallel()

	out, errc := MergeOrdered(
		recordsFromKeys(1, 4, 7, 10),
		recordsFromKeys(2, 5, 8),
		recordsFromKeys(0, 3, 6, 9, 12),
	)

	var keys []int64
	for r := range out {
		keys = append(keys, r.Key)
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(keys) != 12 {
		t.Fatalf("got %d records, want 12", len(keys))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Fatalf("output not globally sorted at %d: %v", i, keys)
		}
	}
}

func TestMergeOrderedPropagatesSourceError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("shard unreachable")
	out, errc := MergeOrdered(
		recordsFromKeys(1, 2, 3, 4, 5),
		failingSource(wantErr, 10, 11),
	)

	for range out {
	}
	err := <-errc
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want %v", err, wantErr)
	}
}

func TestMergeOrderedNoSources(t *testing.T) {
	t.Parallel()

	out, errc := MergeOrdered()
	for range out {
		t.Fatal("expected no records from zero sources")
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, errc = MergeOrdered(recordsFromKeys(), recordsFromKeys())
	for range out {
		t.Fatal("expected no records from empty sources")
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMergeOrderedManyShardsRaceFree(t *testing.T) {
	t.Parallel()

	const shards, perShard = 12, 400
	sources := make([]<-chan SourceItem, shards)
	for s := range shards {
		keys := make([]int64, perShard)
		for j := range perShard {
			// Strided keys keep every shard internally sorted while interleaving
			// globally across shards.
			keys[j] = int64(j*shards + s)
		}
		sources[s] = recordsFromKeys(keys...)
	}

	out, errc := MergeOrdered(sources...)
	var prev int64 = -1
	count := 0
	for r := range out {
		if r.Key < prev {
			t.Fatalf("out of order: %d after %d", r.Key, prev)
		}
		prev = r.Key
		count++
	}
	if err := <-errc; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != shards*perShard {
		t.Fatalf("got %d records, want %d", count, shards*perShard)
	}
}
```

## Review

The merge is correct when the heap invariant holds: the root is the global minimum because every source is internally sorted, so popping the root and refilling from that source's stream emits records in non-decreasing key order. Confirm the heap only ever holds one entry per live source — that bounded footprint is the whole reason to stream rather than collect-then-sort. Confirm the consumer protocol: range `out` fully, then read `errc` once; a clean run yields nil because `errc` is closed without a send, and a failed run yields the buffered error. The `defer` order matters — `close(out)` before `close(errc)` — so the error is buffered before the consumer turns to read it.

The traps here are specific to ordered, fallible fan-in. Reaching for arrival-order `Merge` and sorting afterward defeats the point and needs unbounded memory. Treating a failed shard as merely closed returns a short result that looks complete — the terminal `SourceItem.Err` is what prevents that. An unbuffered error channel would deadlock the merge goroutine on the error send if the consumer is still ranging records; capacity one decouples them. And feeding the merge an out-of-order source breaks the heap invariant silently — the contract is that each source is sorted, and it is the source's job to honor it.

## Resources

- [`container/heap`](https://pkg.go.dev/container/heap) — the `heap.Interface` (Push, Pop, and the embedded `sort.Interface`) this k-way merge implements.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in foundation this exercise extends with ordering and error propagation.
- [k-way merge algorithm](https://en.wikipedia.org/wiki/K-way_merge_algorithm) — the heap-based merge of sorted runs, the same primitive used by external sort and LSM compaction.

---

Back to [01-merging-channels.md](01-merging-channels.md) | Next: [03-scatter-gather-with-deadline.md](03-scatter-gather-with-deadline.md)
