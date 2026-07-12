# Exercise 17: Batch Accumulator with Threshold and Flush Callback

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Writing one row per insert to a database is slow; writing a thousand rows
per batch is efficient but risks losing work if the buffer only flushes
when it happens to be full. A batch collector accumulates items behind a
closure and hands a complete batch to a callback the moment a threshold is
hit — or on demand, so a caller can force out whatever is left before
shutting down.

## What you'll build

```text
batch/                       independent module: example.com/batch
  go.mod                     go 1.24
  batch.go                   func NewCollector[T] returning (add, flush) closures
  batch_test.go              threshold flush, manual flush, empty flush, concurrency
  cmd/demo/
    main.go                  fills a collector past its threshold and force-flushes the rest
```

- Files: `batch.go`, `batch_test.go`, `cmd/demo/main.go`.
- Implement: `NewCollector[T any](threshold int, onFlush func([]T)) (add func(T), flush func())`.
- Test: reaching the threshold triggers exactly one automatic flush of exactly `threshold` items and resets the buffer; `flush()` sends whatever partial batch remains; `flush()` on an empty buffer is a no-op that never calls `onFlush`; concurrent `add` calls never lose, duplicate, or split an item across two batches.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/17-batch-collector-flush-callback/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/17-batch-collector-flush-callback
go mod edit -go=1.24
```

### Swap the buffer under the lock, call the callback outside it

The classic bug in a batch accumulator is checking "is the buffer full?"
and appending in two separate steps: two goroutines can both see `len(buf)
== threshold-1`, both append, and both believe they are the one that filled
it — either double-flushing the same items or never flushing at all. `add`
avoids this by doing the entire check-then-act as one critical section: it
appends the item, checks the new length, and — if it just crossed the
threshold — swaps in a brand-new backing slice for `buf` before releasing
the lock. The full slice is captured in a local variable so it survives
the swap.

The `onFlush` call itself happens *after* `mu.Unlock()`, not inside the
critical section. If `onFlush` ran while `mu` was held, a slow callback
(writing to a database, calling a webhook) would block every other `add`
call for however long it took — and a callback that itself called `add` on
the same collector would deadlock, since Go's `sync.Mutex` is not
reentrant. Copying the full batch into a local variable before unlocking,
then calling `onFlush` after, keeps the lock's critical section to just
the bookkeeping.

`flush` is nearly identical to the threshold branch of `add`, just
unconditional: it swaps out whatever is currently buffered — even a
partial batch — and, only if that partial batch is non-empty, calls
`onFlush`. Calling `flush` on an empty buffer must be a safe no-op, which
is why the swap only happens under a `len(buf) > 0` guard.

Create `batch.go`:

```go
package batch

import "sync"

// NewCollector returns two closures sharing one buffer: add appends item
// and, once the buffer reaches threshold, atomically swaps in a fresh
// buffer and hands the full one to onFlush; flush does the same swap
// unconditionally, for a caller that wants to force out a partial batch
// (e.g. on shutdown). onFlush always runs outside the internal lock, so a
// slow or reentrant callback can never block a concurrent add.
func NewCollector[T any](threshold int, onFlush func([]T)) (add func(T), flush func()) {
	var mu sync.Mutex
	buf := make([]T, 0, threshold)

	add = func(item T) {
		mu.Lock()
		buf = append(buf, item)
		var full []T
		if len(buf) >= threshold {
			full = buf
			buf = make([]T, 0, threshold)
		}
		mu.Unlock()

		if full != nil {
			onFlush(full)
		}
	}

	flush = func() {
		mu.Lock()
		var pending []T
		if len(buf) > 0 {
			pending = buf
			buf = make([]T, 0, threshold)
		}
		mu.Unlock()

		if pending != nil {
			onFlush(pending)
		}
	}

	return add, flush
}
```

### The runnable demo

Five items go into a collector with a threshold of three: the first three
trigger an automatic flush, and the remaining two are force-flushed with
an explicit call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batch"
)

func main() {
	var batches [][]string
	add, flush := batch.NewCollector(3, func(items []string) {
		batches = append(batches, items)
	})

	for _, item := range []string{"order-1", "order-2", "order-3", "order-4", "order-5"} {
		add(item)
	}
	flush() // force out the partial batch left over

	for i, b := range batches {
		fmt.Printf("batch %d: %v\n", i, b)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch 0: [order-1 order-2 order-3]
batch 1: [order-4 order-5]
```

The third `add` call crosses the threshold and flushes `order-1` through
`order-3` immediately; `order-4` and `order-5` sit in the buffer until the
explicit `flush()` call sends them as a shorter, final batch.

### Tests

`TestCollectorFlushesAtThreshold` checks the exact contents and count of
automatic flushes for a run that crosses the threshold twice.
`TestCollectorManualFlushSendsPartialBatch` and
`TestCollectorFlushOnEmptyBufferDoesNothing` cover the two edges of
`flush`: it must send a non-empty partial batch and must not call
`onFlush` at all when there is nothing buffered.
`TestCollectorBufferResetsAfterFlush` confirms the buffer is genuinely
reset — not just logically empty but a fresh slice — after an automatic
flush, by adding more items afterward and checking they form their own
batch. `TestCollectorConcurrentAddNeverLosesOrDuplicatesItems` fires 400
concurrent `add` calls at a small threshold under `-race` and asserts
every one of the 400 distinct items appears in exactly one flushed batch.

Create `batch_test.go`:

```go
package batch

import (
	"sync"
	"testing"
)

func TestCollectorFlushesAtThreshold(t *testing.T) {
	t.Parallel()

	var flushed [][]int
	add, _ := NewCollector(3, func(items []int) {
		flushed = append(flushed, append([]int(nil), items...))
	})

	for _, v := range []int{1, 2, 3, 4, 5, 6} {
		add(v)
	}

	want := [][]int{{1, 2, 3}, {4, 5, 6}}
	if len(flushed) != len(want) {
		t.Fatalf("flushed %v batches, want %v", flushed, want)
	}
	for i := range want {
		if len(flushed[i]) != len(want[i]) {
			t.Fatalf("batch %d = %v, want %v", i, flushed[i], want[i])
		}
		for j := range want[i] {
			if flushed[i][j] != want[i][j] {
				t.Fatalf("batch %d = %v, want %v", i, flushed[i], want[i])
			}
		}
	}
}

func TestCollectorManualFlushSendsPartialBatch(t *testing.T) {
	t.Parallel()

	var flushed [][]string
	add, flush := NewCollector(5, func(items []string) {
		flushed = append(flushed, append([]string(nil), items...))
	})

	add("a")
	add("b")
	flush()

	if len(flushed) != 1 {
		t.Fatalf("flushed %d batches, want 1", len(flushed))
	}
	if len(flushed[0]) != 2 {
		t.Fatalf("partial batch = %v, want length 2", flushed[0])
	}
}

func TestCollectorFlushOnEmptyBufferDoesNothing(t *testing.T) {
	t.Parallel()

	calls := 0
	_, flush := NewCollector[int](3, func(items []int) {
		calls++
	})

	flush()
	flush()

	if calls != 0 {
		t.Fatalf("onFlush called %d times, want 0 for an empty buffer", calls)
	}
}

func TestCollectorBufferResetsAfterFlush(t *testing.T) {
	t.Parallel()

	var flushed [][]int
	add, flush := NewCollector(2, func(items []int) {
		flushed = append(flushed, append([]int(nil), items...))
	})

	add(1)
	add(2) // triggers an automatic flush of [1 2]
	add(3)
	flush() // manual flush of the leftover [3]

	if len(flushed) != 2 {
		t.Fatalf("flushed %d batches, want 2", len(flushed))
	}
	if len(flushed[0]) != 2 || len(flushed[1]) != 1 {
		t.Fatalf("flushed = %v, want lengths [2 1]", flushed)
	}
}

func TestCollectorConcurrentAddNeverLosesOrDuplicatesItems(t *testing.T) {
	t.Parallel()

	const threshold = 4
	const total = 400

	var mu sync.Mutex
	seen := make(map[int]int)
	add, flush := NewCollector(threshold, func(items []int) {
		mu.Lock()
		defer mu.Unlock()
		if len(items) > threshold {
			t.Errorf("flushed batch of %d items, want at most %d", len(items), threshold)
		}
		for _, v := range items {
			seen[v]++
		}
	})

	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			add(v)
		}(i)
	}
	wg.Wait()
	flush()

	if len(seen) != total {
		t.Fatalf("saw %d distinct items, want %d", len(seen), total)
	}
	for v, count := range seen {
		if count != 1 {
			t.Fatalf("item %d flushed %d times, want exactly once", v, count)
		}
	}
}
```

## Review

The collector is correct when appending an item and deciding whether it
crossed the threshold happen inside one lock — that check-then-act is the
entire reason the concurrency test can assert every item lands in exactly
one batch instead of being lost or double-counted. Swapping in a fresh
backing slice, rather than truncating the old one with `buf[:0]`, matters
because the callback keeps a reference to the just-flushed slice after the
lock is released; reusing the same backing array would let a subsequent
`add` overwrite data `onFlush` hasn't finished reading yet. Calling
`onFlush` outside the lock is what keeps a slow or reentrant callback from
serializing every other `add` in the system behind it. Run `go test -race`
since production traffic means many goroutines calling `add` at once.

## Resources

- [sync package](https://pkg.go.dev/sync) — `Mutex`, the check-then-act critical section this exercise depends on.
- [Effective Go: Slices](https://go.dev/doc/effective_go#slices) — why `append` can grow into a shared backing array and when a fresh slice is required instead.
- [AWS SDK for Go: DynamoDB BatchWriteItem](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/dynamodb#Client.BatchWriteItem) — a real API shaped around exactly this accumulate-then-flush-as-a-batch pattern.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-event-debouncer-deadline-coalesce.md](16-event-debouncer-deadline-coalesce.md) | Next: [18-probability-sampler-for-observability.md](18-probability-sampler-for-observability.md)
