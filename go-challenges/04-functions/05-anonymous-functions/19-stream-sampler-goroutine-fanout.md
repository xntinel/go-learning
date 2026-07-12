# Exercise 19: High-Volume Stream Sampler using Goroutine Fanout Literals

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

Sampling a high-volume stream is embarrassingly parallel — each item's keep/drop
decision is independent of every other item's — but a naive fan-out that has every
worker append to one shared slice turns that parallelism into a data race. This
module builds a sampler where each worker goroutine literal owns a disjoint slice of
indices, so the fan-out is race-free by construction, with no mutex at all.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
sampler/                       module example.com/sampler
  go.mod
  sampler.go                    Item, Sample (goroutine fanout, per-worker partitioning)
  sampler_test.go                order preserved, single vs multi worker agree, all/none, edge worker counts
  cmd/demo/main.go              sample 1000 items across 4 workers
```

- Files: `sampler.go`, `sampler_test.go`, `cmd/demo/main.go`.
- Implement: `Sample(items, workers, keep)` launching `workers` goroutine literals, each owning indices `w, w+workers, w+2*workers, ...` of a shared `[]bool`, so no two goroutines ever write the same slot.
- Test: kept items preserve original order; single-worker and multi-worker runs agree exactly; a predicate that always/never keeps behaves correctly; worker counts larger than the item count and a zero worker count are both handled. Under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Disjoint partitioning instead of a shared lock

`Sample` launches one goroutine literal per worker, and worker `w` handles exactly
the indices `w, w+workers, w+2*workers, ...` — a fixed-stride partition of the input.
Every goroutine writes only to `kept[i]` for the indices it owns, and no other
goroutine ever touches those same indices. That is what makes the fan-out race-free
without a mutex: the race detector has nothing to flag because there is no shared
mutable state between goroutines, only disjoint ownership of separate slots in one
slice. Contrast this with appending each kept item to one shared output slice from
every worker — the append would need a mutex, or a single collecting goroutine,
because `append` reads, resizes, and writes back a shared slice header.

The `keep` predicate is exactly where "sample probabilistically" lives: a caller
passes any function of an item to a `bool`, and a demo or test can make that
predicate as random or as deterministic as needed. Keeping it deterministic in tests
(for example, `id % 3 == 0`) is what makes the suite reproducible while the shape of
the code — partitioned, concurrent workers deciding independently — is exactly the
same as a genuinely probabilistic sampler would use.

Create `sampler.go`:

```go
package sampler

import "sync"

// Item is one record from a high-volume stream.
type Item struct {
	ID    int
	Value float64
}

// Sample fans work out across workers goroutine literals and decides, for
// each item, whether keep reports it should be kept — typically a
// probabilistic predicate over the item's ID or value. Each worker literal
// only ever touches the indices assigned to it: worker w handles indices w,
// w+workers, w+2*workers, ... This partitioning is disjoint by
// construction, so no two goroutines ever write the same slot of the
// results slice and no mutex is needed to make the fan-out race-free — the
// race detector has nothing to flag because there is no shared mutable
// state, only per-worker ownership of a slice range.
func Sample(items []Item, workers int, keep func(Item) bool) []Item {
	if workers <= 0 {
		workers = 1
	}

	kept := make([]bool, len(items))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < len(items); i += workers {
				kept[i] = keep(items[i])
			}
		}(w)
	}
	wg.Wait()

	out := make([]Item, 0, len(items))
	for i, k := range kept {
		if k {
			out = append(out, items[i])
		}
	}
	return out
}
```

### The runnable demo

The demo samples a thousand items, keeping roughly one in three, across four
workers. The predicate is deterministic so the count is fixed regardless of
scheduling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sampler"
)

func main() {
	items := make([]sampler.Item, 1000)
	for i := range items {
		items[i] = sampler.Item{ID: i, Value: float64(i) * 1.5}
	}

	// Deterministic "probabilistic" predicate: keep roughly one in three.
	kept := sampler.Sample(items, 4, func(it sampler.Item) bool {
		return it.ID%3 == 0
	})

	fmt.Println("input:", len(items))
	fmt.Println("sampled:", len(kept))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
input: 1000
sampled: 334
```

### Tests

`TestSampleKeepsExactlyMatchingItemsInOrder` checks kept items appear in their
original relative order. `TestSampleWithSingleWorkerMatchesMultiWorker` is the
load-bearing test: it runs the same predicate with one worker and with eight and
asserts identical results, proving the partitioning changes nothing about
correctness, only concurrency. `TestSampleKeepsNoneAndAll` and
`TestSampleWithMoreWorkersThanItems` cover the predicate and worker-count edges;
`TestSampleWithZeroWorkersDefaultsToOne` guards the fallback.

Create `sampler_test.go`:

```go
package sampler

import "testing"

func makeItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: i, Value: float64(i)}
	}
	return items
}

func TestSampleKeepsExactlyMatchingItemsInOrder(t *testing.T) {
	t.Parallel()
	items := makeItems(30)

	got := Sample(items, 4, func(it Item) bool { return it.ID%2 == 0 })

	if len(got) != 15 {
		t.Fatalf("len(got) = %d, want 15", len(got))
	}
	for i, it := range got {
		if it.ID != i*2 {
			t.Fatalf("got[%d].ID = %d, want %d", i, it.ID, i*2)
		}
	}
}

func TestSampleWithSingleWorkerMatchesMultiWorker(t *testing.T) {
	t.Parallel()
	items := makeItems(97)
	keep := func(it Item) bool { return it.Value > 50 }

	single := Sample(items, 1, keep)
	multi := Sample(items, 8, keep)

	if len(single) != len(multi) {
		t.Fatalf("single len = %d, multi len = %d, want equal", len(single), len(multi))
	}
	for i := range single {
		if single[i] != multi[i] {
			t.Fatalf("single[%d] = %+v, multi[%d] = %+v, want equal", i, single[i], i, multi[i])
		}
	}
}

func TestSampleKeepsNoneAndAll(t *testing.T) {
	t.Parallel()
	items := makeItems(10)

	if got := Sample(items, 3, func(Item) bool { return false }); len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
	if got := Sample(items, 3, func(Item) bool { return true }); len(got) != len(items) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(items))
	}
}

func TestSampleWithMoreWorkersThanItems(t *testing.T) {
	t.Parallel()
	items := makeItems(3)
	got := Sample(items, 16, func(it Item) bool { return it.ID == 1 })
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("got = %+v, want exactly item 1", got)
	}
}

func TestSampleWithZeroWorkersDefaultsToOne(t *testing.T) {
	t.Parallel()
	items := makeItems(5)
	got := Sample(items, 0, func(Item) bool { return true })
	if len(got) != len(items) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(items))
	}
}
```

## Review

The sampler is correct — and race-free — because every goroutine's writes land on a
range of indices no other goroutine ever visits; that invariant is checked by the
single-versus-multi-worker equality test, which would only diverge if two workers
somehow raced on the same slot. Correctness here does not come from synchronization
primitives at all, it comes from the shape of the partition: fixed-stride,
non-overlapping, decided entirely by each goroutine's own `worker` index passed as
an argument. That is the sharpest available contrast to a shared-append fan-out,
which needs a mutex or a single collector precisely because its workers do not own
disjoint state.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Data Race Detector](https://go.dev/doc/articles/race_detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-deferred-audit-logger.md](18-deferred-audit-logger.md) | Next: [20-webhook-retry-callback.md](20-webhook-retry-callback.md)
