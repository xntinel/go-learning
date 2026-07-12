# Exercise 2: Measure Map Memory — Presize vs Grow, and the Delete Trap

Capacity planning for a large map comes down to two questions a senior must be able
to answer with numbers, not folklore: how much does presizing save, and does deleting
entries give memory back. This exercise builds a small measurement harness that
answers both against the real built-in map, and encodes the delete-does-not-shrink
gotcha as a testable fact.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
mapfoot/                   independent module: example.com/mapfoot
  go.mod                   go 1.24
  mapfoot.go               Measure(n) Footprint; ProveDeleteDoesNotShrink(n) ShrinkReport
  cmd/
    demo/
      main.go              runnable demo: prints the memory conclusions
  mapfoot_test.go          AllocsPerRun presize<grow, delete-does-not-shrink, Example
```

- Files: `mapfoot.go`, `cmd/demo/main.go`, `mapfoot_test.go`.
- Implement: `Measure(n)` returning a `Footprint` (bytes and heap objects for a presized vs a grown map, plus bytes-per-entry), and `ProveDeleteDoesNotShrink(n)` returning a `ShrinkReport` (heap after building, after deleting every key, and after assigning a fresh map).
- Test: `testing.AllocsPerRun` asserting a presized build performs strictly fewer allocations than a grown one; a delete-does-not-shrink assertion using `runtime.GC` before each `runtime.ReadMemStats`; an `Example` over `maps.Clone`/`clear`/`delete` with a fixed `// Output`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Measuring heap memory honestly

`runtime.ReadMemStats` fills a `runtime.MemStats`. The field that matters here is
`HeapAlloc`: bytes of allocated, still-live heap objects. To attribute memory to a
map you sample `HeapAlloc` before and after building it, and the delta is the map's
footprint — but only if you do two things. First, call `runtime.GC()` immediately
before each read, so the numbers reflect live memory rather than not-yet-collected
garbage. Second, call `runtime.KeepAlive(m)` after the second read, so the compiler
cannot decide the map is dead and collect it *before* you measure it. Skip the
`KeepAlive` and an optimizing build may free the map early, and your "footprint"
collapses to noise.

Absolute byte counts are not portable — they move with GOARCH, pointer size, and GC
tuning — so the harness returns them for information but the *tests* assert only
relative facts. `Measure` reports bytes-per-entry as a rough planning figure; for an
`int`-keyed, `int`-valued map that number is comfortably above the raw 16 bytes of
key+value because of control bytes and load-factor slack, but the exact value is not
something to hard-code.

### Why presizing wins, measured deterministically

A grown map — `make(map[K]V)` then `n` inserts — crosses several load-factor
thresholds and rehashes at each one, allocating fresh tables every time. A presized
map — `make(map[K]V, n)` — allocates once. The clean way to prove this is
`testing.AllocsPerRun(runs, f)`, which runs `f` a few times under a GC that counts
allocations and returns the average. It is deterministic (it counts allocation events,
not wall-clock time), which is exactly what you want in a test. One caveat:
`AllocsPerRun` temporarily forces `GOMAXPROCS` to 1, so tests that call it must not run
in parallel with each other — mark them non-parallel.

### The delete trap, as a returnable fact

`ProveDeleteDoesNotShrink` builds a large presized map, records its heap footprint,
deletes every key, records again, then drops the reference by assigning a fresh empty
map and records a third time. The result encodes the lesson: after deleting all keys
the footprint is essentially unchanged (the map keeps its capacity — maps never
shrink), while assigning a fresh map and letting GC run reclaims it. With a
million-entry map the signal is tens of megabytes, so a "shrank" verdict thresholded
at half the full size is robust against measurement noise. This is why a bounded or
TTL cache built on a raw map leaks: `delete` is not the way to give memory back.

Create `mapfoot.go`:

```go
package mapfoot

import "runtime"

// Footprint reports the heap cost of building a map two ways.
type Footprint struct {
	N               int
	GrownBytes      uint64  // HeapAlloc delta for a grown (unsized) map
	PresizedBytes   uint64  // HeapAlloc delta for a presized map
	GrownObjects    uint64  // HeapObjects delta for the grown map
	PresizedObjects uint64  // HeapObjects delta for the presized map
	BytesPerEntry   float64 // PresizedBytes / N, a rough planning figure
}

// heapAlloc returns live heap bytes after forcing a GC, so the reading reflects
// live memory rather than uncollected garbage.
func heapAlloc() (bytes, objects uint64) {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc, m.HeapObjects
}

func buildBytes(n int, presize bool) (bytes, objects uint64) {
	beforeB, beforeO := heapAlloc()
	var m map[int]int
	if presize {
		m = make(map[int]int, n)
	} else {
		m = make(map[int]int)
	}
	for i := range n {
		m[i] = i
	}
	afterB, afterO := heapAlloc()
	runtime.KeepAlive(m) // keep the map live across the second reading
	return afterB - beforeB, afterO - beforeO
}

// Measure builds an n-entry map both presized and grown and reports the footprint.
func Measure(n int) Footprint {
	gb, go_ := buildBytes(n, false)
	pb, po := buildBytes(n, true)
	var bpe float64
	if n > 0 {
		bpe = float64(pb) / float64(n)
	}
	return Footprint{
		N:               n,
		GrownBytes:      gb,
		PresizedBytes:   pb,
		GrownObjects:    go_,
		PresizedObjects: po,
		BytesPerEntry:   bpe,
	}
}

// ShrinkReport records heap footprint at three moments in a map's life.
type ShrinkReport struct {
	FullBytes    uint64 // after building n entries
	AfterDelete  uint64 // after delete()-ing every key
	AfterFresh   uint64 // after assigning a fresh empty map (old one collectable)
	DeleteShrank bool   // did delete-all drop the footprint by more than half?
	FreshShrank  bool   // did assigning a fresh map drop it by more than half?
}

// ProveDeleteDoesNotShrink demonstrates that delete() never returns capacity: the
// footprint after deleting every key stays near the full size, while dropping the
// reference and assigning a fresh map lets GC reclaim it.
func ProveDeleteDoesNotShrink(n int) ShrinkReport {
	baseB, _ := heapAlloc()

	m := make(map[int]int, n)
	for i := range n {
		m[i] = i
	}
	fullB, _ := heapAlloc()
	full := fullB - baseB

	for k := range m {
		delete(m, k)
	}
	delB, _ := heapAlloc()
	afterDelete := delB - baseB
	runtime.KeepAlive(m)

	m = make(map[int]int) // drop the big map; it becomes collectable
	freshB, _ := heapAlloc()
	afterFresh := freshB - baseB
	runtime.KeepAlive(m)

	return ShrinkReport{
		FullBytes:    full,
		AfterDelete:  afterDelete,
		AfterFresh:   afterFresh,
		DeleteShrank: afterDelete < full/2,
		FreshShrank:  afterFresh < full/2,
	}
}
```

### The runnable demo

Because absolute byte counts vary by platform, the demo prints only the portable
conclusions: that deleting every key does not shrink the map, that a fresh map does,
and that per-entry cost clears a floor no `int`-keyed map can go under.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mapfoot"
)

func main() {
	r := mapfoot.ProveDeleteDoesNotShrink(1_000_000)
	fmt.Println("delete(all) shrinks the map:", r.DeleteShrank)
	fmt.Println("fresh map reclaims memory:", r.FreshShrank)

	f := mapfoot.Measure(1_000_000)
	fmt.Println("bytes per entry at least 8:", f.BytesPerEntry >= 8)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delete(all) shrinks the map: false
fresh map reclaims memory: true
bytes per entry at least 8: true
```

### Tests

`TestPresizedFewerAllocs` uses `testing.AllocsPerRun` to assert the deterministic
fact that presizing allocates fewer times than growing. `TestDeleteDoesNotShrink`
asserts the two relative inequalities from the report (delete keeps the footprint,
a fresh map drops it); both call `runtime.GC` internally before every reading. The
`Example` uses the builtins that surround this topic — `maps.Clone` for an independent
copy, `delete` on the copy, and `clear` which empties but (like delete) keeps capacity
— with a fixed, deterministic `// Output`. Because `AllocsPerRun` and the memory
readings mutate global state (`GOMAXPROCS` and the GC), these tests are not marked
parallel.

Create `mapfoot_test.go`:

```go
package mapfoot

import (
	"fmt"
	"maps"
	"testing"
)

func TestPresizedFewerAllocs(t *testing.T) {
	const n = 10000
	grown := testing.AllocsPerRun(3, func() {
		m := make(map[int]int)
		for i := range n {
			m[i] = i
		}
		_ = m
	})
	presized := testing.AllocsPerRun(3, func() {
		m := make(map[int]int, n)
		for i := range n {
			m[i] = i
		}
		_ = m
	})
	if !(presized < grown) {
		t.Fatalf("presized allocs %v not fewer than grown %v", presized, grown)
	}
}

func TestDeleteDoesNotShrink(t *testing.T) {
	r := ProveDeleteDoesNotShrink(1_000_000)
	if r.DeleteShrank {
		t.Errorf("delete(all) shrank the map: full=%d afterDelete=%d", r.FullBytes, r.AfterDelete)
	}
	if !r.FreshShrank {
		t.Errorf("fresh map did not reclaim: full=%d afterFresh=%d", r.FullBytes, r.AfterFresh)
	}
}

func TestMeasureReportsCost(t *testing.T) {
	f := Measure(100000)
	if f.PresizedObjects == 0 {
		t.Error("PresizedObjects = 0, expected the map to allocate heap objects")
	}
	if f.BytesPerEntry < 8 {
		t.Errorf("BytesPerEntry = %v, want >= 8 for an int->int map", f.BytesPerEntry)
	}
}

func Example() {
	m := map[string]int{"a": 1, "b": 2, "c": 3}
	c := maps.Clone(m) // independent copy
	delete(c, "a")     // mutating the copy does not touch the original
	fmt.Println(len(m), len(c))
	clear(m) // empties the map (but keeps its capacity)
	fmt.Println(len(m))
	// Output:
	// 3 2
	// 0
}
```

## Review

The harness is correct when its conclusions are stable across platforms even though
the raw bytes are not: presizing always allocates fewer times, delete-all never drops
the footprint by half, and a fresh map always does. The measurement discipline is the
whole point — every reading is preceded by `runtime.GC()` and followed (where the map
must survive the read) by `runtime.KeepAlive`. Drop the `GC` and you measure garbage;
drop the `KeepAlive` and an optimizer may collect the map before you read it, so the
"footprint" vanishes and the test flakes.

The mistakes to avoid are the ones the tests are shaped to prevent. Do not assert an
absolute byte count — assert inequalities, or use `AllocsPerRun` for a deterministic
allocation count. Do not mark the memory tests `t.Parallel()`: `AllocsPerRun` forces
`GOMAXPROCS` to 1 and the readings share the global GC, so concurrent runs interfere.
And do not read `delete` or `clear` as memory reclamation — both keep the map's
capacity, which is exactly the trap `ProveDeleteDoesNotShrink` makes concrete: to give
memory back you must drop the reference and build a fresh map. Run `go test -race` to
confirm the harness is race-free (it is single-goroutine by construction).

## Resources

- [Faster Go maps with Swiss Tables (The Go Blog)](https://go.dev/blog/swisstable) — source of the microbenchmark (up to ~60%) and full-application geomean (~1.5%) figures.
- [Go 1.24 Release Notes](https://go.dev/doc/go1.24) — the Swiss Table map change (cites a ~2-3% average CPU improvement).
- [`runtime` package](https://pkg.go.dev/runtime) — `ReadMemStats`, `MemStats.HeapAlloc`/`HeapObjects`, `GC`, and `KeepAlive`.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — deterministic allocation counting, and its `GOMAXPROCS` caveat.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-swiss-table-open-addressing.md](01-swiss-table-open-addressing.md) | Next: [03-map-benchmark-suite.md](03-map-benchmark-suite.md)
