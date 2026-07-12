# Exercise 13: Per-Goroutine Result Slices Merged With slices.Concat

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Fanning work out across goroutines and gathering the results into one slice
is one of the most common concurrency shapes in backend Go — a search
request scattered across index shards, a batch job splitting rows across
worker goroutines, a fan-out to several backend replicas whose responses get
merged. The instinctive way to write the collection step is to let every
goroutine `append` its results onto one slice declared outside the loop.
That is a data race: `append` reads the shared slice header, decides
whether there is room, and writes the new element and header back as
separate, unsynchronized steps, so two goroutines racing through that
sequence at once can silently drop a write or corrupt the backing array,
and Go's race detector will flag it every time it observes the
interleaving.

The fix is not a mutex around the shared `append` — that works, but it
serializes every goroutine on that one lock and defeats a chunk of the
concurrency's purpose. The better fix is to never share the slice in the
first place: give each goroutine its own private result slice, let a
`sync.WaitGroup` guarantee every goroutine has finished before anything
reads them, and merge the per-goroutine slices with `slices.Concat` once
they are all done. Since goroutines finish in a nondeterministic order,
determinism is restored by sorting the merged result afterward, not by
controlling how the goroutines interleave.

This module builds that scatter-gather step as a package: a `Scatterer`
constructed with a validated worker count, whose `Gather` method never lets
two goroutines touch the same slice. The racy alternative — every worker
appending onto one shared slice — never appears anywhere in that type. It
lives only in the test file, as a structural contrast that shows exactly
what makes the pattern unsafe without ever asking `go test -race` to catch
a live race, because a race, once actually triggered, is precisely what
`-race` is built to fail the build over — and this module's test suite must
always pass under it.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fanoutcollect/             module example.com/fanoutcollect
  go.mod                   go 1.24
  fanoutcollect.go          Result, ProcessFunc, Scatterer; New, Gather
  fanoutcollect_test.go     worker-count table, single item, empty items,
                           repeated-run determinism under -race, the shared-
                           append contrast, ExampleScatterer_Gather
```

- Files: `fanoutcollect.go`, `fanoutcollect_test.go`.
- Implement: `New(workers int) (*Scatterer, error)` rejecting a non-positive worker count with `ErrInvalidWorkerCount`; `(*Scatterer).Gather(items []int, fn ProcessFunc) []Result`, which shards `items` across up to `workers` goroutines, has each goroutine append into a private `local` slice sized with `make([]Result, 0, end-start)`, writes each goroutine's finished slice into its own index of a preallocated `[][]Result`, waits on a `sync.WaitGroup`, merges with `slices.Concat`, and sorts the merged result by `Item` before returning it.
- Test: a non-positive worker count rejected with `errors.Is`; a table over worker counts (`1`, `2`, `7`, `100` on 37 items) all producing the identical sorted result; a single-item batch; empty `items` returning `nil`; a repeated-run test that calls `Gather` 20 times with `workers=16` on 200 items and asserts bit-identical sorted output every time, run under `-race`; a `gatherWorkerAppend` contrast pinning the structural cause of the race without ever triggering it; and `ExampleScatterer_Gather` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/13-per-goroutine-append-then-concat
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/13-per-goroutine-append-then-concat
go mod edit -go=1.24
```

### What the racy version looks like, and why this module cannot let go test -race run it

It is worth writing the unsafe version out, because the failure it produces
is subtle enough that seeing the code helps more than being told "it's a
race": every goroutine calls `shared = append(shared, fn(item))` against one
`var shared []Result` declared outside the goroutines, with no mutex and no
channel.

```go
var shared []Result   // one slice header, declared outside every goroutine
for w := 0; w < workers; w++ {
    go func(items []int) {
        defer wg.Done()
        for _, item := range items {
            shared = append(shared, fn(item))   // every goroutine races here
        }
    }(shardOf(w))
}
```

`append`'s three-step sequence — read the current header, decide if
`len < cap`, write the element and the new header — is not atomic. Two
goroutines can both read the same header, both conclude there is room, and
both write their element to the *same* index, so one write is lost
outright; or, if capacity is exhausted, one goroutine can start
reallocating to a new backing array while another is still writing through
the old pointer. `go test -race` catches this reliably once two goroutines
actually race through it concurrently — but that is exactly why this
module's tests never call the racy shape with real goroutines: a race,
once triggered, is what `-race` is designed to fail the run over, and this
package's test suite is required to always pass under `-race`. So the test
file pins the *structural* cause — every worker's append target is the
identical `shared` variable — deterministically, with zero goroutines,
instead of reproducing the nondeterministic symptom.

`Gather` sidesteps the entire problem by construction: no goroutine ever
touches a slice another goroutine can also touch. Each goroutine builds
`local := make([]Result, 0, end-start)`, appends only into `local`, and
writes the finished `local` into `shards[w]` — its own, statically assigned
index of a `[][]Result` allocated before any goroutine starts. Writing to
*different* indices of the same outer slice concurrently is not a race (the
memory being written does not overlap), so there is nothing here for
`-race` to find. `sync.WaitGroup.Wait()` is the barrier that makes it safe
to read `shards` afterward.

Create `fanoutcollect.go`:

```go
// Package fanoutcollect fans work out across N goroutines and gathers their
// results without ever appending to a slice more than one goroutine can
// reach.
package fanoutcollect

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
)

// ErrInvalidWorkerCount means the configured worker count was not positive.
var ErrInvalidWorkerCount = errors.New("fanoutcollect: worker count must be positive")

// Result is one processed item.
type Result struct {
	Item  int
	Value int
}

// ProcessFunc transforms one input item into a Result. Gather calls it from
// multiple goroutines at once, so it must not touch any state shared with
// other calls.
type ProcessFunc func(item int) Result

// Scatterer fans work out across a fixed number of goroutines and gathers
// the results back into one sorted slice.
//
// A Scatterer is immutable after construction and is safe for concurrent
// use: multiple goroutines may call Gather on the same *Scatterer at once,
// each call building its own private state.
type Scatterer struct {
	workers int
}

// New returns a Scatterer that shards work across up to workers goroutines.
// It returns ErrInvalidWorkerCount if workers is not positive.
func New(workers int) (*Scatterer, error) {
	if workers <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidWorkerCount, workers)
	}
	return &Scatterer{workers: workers}, nil
}

// Gather splits items into up to s.workers shards and processes each shard
// in its own goroutine, appending into a slice private to that goroutine.
// Only after every goroutine has returned -- synchronized by a
// sync.WaitGroup -- are the per-goroutine slices merged with slices.Concat.
// Because no goroutine ever appends to a slice another goroutine can also
// append to, there is nothing for the race detector to flag no matter how
// the goroutines interleave.
//
// The merge order across goroutines is not deterministic -- shard w can
// finish before or after shard w+1 depending on scheduling -- so the result
// is sorted by Item before it is returned. Sorting, not append order, is
// what makes Gather's output reproducible. Gather returns nil for an empty
// items, and the returned slice is freshly allocated and never aliases
// items.
func (s *Scatterer) Gather(items []int, fn ProcessFunc) []Result {
	if len(items) == 0 {
		return nil
	}
	workers := s.workers
	if workers > len(items) {
		workers = len(items)
	}

	shards := make([][]Result, workers)
	chunk := (len(items) + workers - 1) / workers

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * chunk
		if start >= len(items) {
			continue
		}
		end := min(start+chunk, len(items))

		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			local := make([]Result, 0, end-start)
			for _, item := range items[start:end] {
				local = append(local, fn(item))
			}
			shards[w] = local
		}(w, start, end)
	}
	wg.Wait()

	merged := slices.Concat(shards...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Item < merged[j].Item })
	return merged
}
```

### Using it

Construct one `Scatterer` with `New`, sized to the parallelism your workload
can actually use, and call `Gather` per request or per batch. Because a
`Scatterer` holds no mutable state after construction, one value can be
shared across every caller goroutine without a mutex — `TestGatherRepeatedRunsAreDeterministic`
holds that promise to real concurrency under `-race`, not just to the
type's doc comment. The slice `Gather` returns is freshly built by
`slices.Concat` and sorted by `Item`; a caller may retain or mutate it
freely.

`ExampleScatterer_Gather` is the runnable demonstration of this module: `go
test` executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift away from the code.

```go
func ExampleScatterer_Gather() {
	s, err := New(4)
	if err != nil {
		panic(err)
	}

	docIDs := make([]int, 12)
	for i := range docIDs {
		docIDs[i] = i + 1
	}

	results := s.Gather(docIDs, func(docID int) Result {
		return Result{Item: docID, Value: docID * docID % 97}
	})
	fmt.Println(results)

	// Output:
	// [{1 1} {2 4} {3 9} {4 16} {5 25} {6 36} {7 49} {8 64} {9 81} {10 3} {11 24} {12 47}]
}
```

### Tests

`TestGatherAcrossWorkerCounts` is the core table: the same 37 items,
deliberately not evenly divisible by any of the worker counts tried (`1`,
`2`, `7`, `100`), must all produce the exact same sorted result — the
sharpest check that chunking logic and the final sort together erase any
trace of how the work was split. `TestGatherSingleItem` and
`TestGatherEmptyItems` cover the boundary inputs a caller can pass by
mistake. `TestGatherRepeatedRunsAreDeterministic` is the one built
specifically to exercise real concurrency under the race detector: 200
items, 16 workers, 20 repeated calls, every one asserted identical to the
expected sorted output, and its passing under `-race` is the empirical
proof that `Gather`'s design is race-free.

`TestSharedAppendTargetIsOneBackingArrayAcrossWorkers` is the antipattern
contrast. `gatherWorkerAppend` is unexported and unreachable from the
package API; it models one worker's contribution to a shared slice, called
twice in sequence, never concurrently, so the test can pin the structural
cause of the race — both calls write through the identical `*shared`
pointer — without ever asking `go test -race` to catch an actual race,
which would fail the very requirement that this suite always passes under
it.

Create `fanoutcollect_test.go`:

```go
package fanoutcollect

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"unsafe"
)

func square(item int) Result {
	return Result{Item: item, Value: item * item}
}

func TestNewRejectsNonPositiveWorkers(t *testing.T) {
	t.Parallel()

	for _, workers := range []int{0, -1} {
		if _, err := New(workers); !errors.Is(err, ErrInvalidWorkerCount) {
			t.Errorf("New(%d) error = %v, want ErrInvalidWorkerCount", workers, err)
		}
	}
}

func TestGatherAcrossWorkerCounts(t *testing.T) {
	t.Parallel()

	items := make([]int, 37) // deliberately not evenly divisible by every worker count
	for i := range items {
		items[i] = i + 1
	}
	want := make([]Result, len(items))
	for i, item := range items {
		want[i] = square(item)
	}

	tests := []struct {
		name    string
		workers int
	}{
		{"single worker", 1},
		{"two workers", 2},
		{"seven workers", 7},
		{"more workers than items", 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := New(tc.workers)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := s.Gather(items, square)
			if !slices.Equal(got, want) {
				t.Fatalf("Gather(workers=%d) = %v, want %v", tc.workers, got, want)
			}
		})
	}
}

// TestGatherSingleItem covers the other end of the size range from the
// worker-count table: one item, more workers configured than items to
// shard, still correct.
func TestGatherSingleItem(t *testing.T) {
	t.Parallel()

	s, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := s.Gather([]int{42}, square)
	want := []Result{{Item: 42, Value: 42 * 42}}
	if !slices.Equal(got, want) {
		t.Fatalf("Gather([42]) = %v, want %v", got, want)
	}
}

func TestGatherEmptyItems(t *testing.T) {
	t.Parallel()

	s, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.Gather(nil, square); got != nil {
		t.Fatalf("Gather(nil) = %v, want nil", got)
	}
}

// TestGatherRepeatedRunsAreDeterministic runs the same input many times
// with a worker count that forces real concurrency, and asserts every run
// produces the exact same sorted output. Run with -race: since every
// goroutine only ever appends to its own private local slice, there is
// nothing for the race detector to report here, which is the direct,
// empirical proof that Gather's design is race-free -- not a claim, a
// passing test.
func TestGatherRepeatedRunsAreDeterministic(t *testing.T) {
	t.Parallel()

	items := make([]int, 200)
	for i := range items {
		items[i] = i
	}
	want := make([]Result, len(items))
	for i, item := range items {
		want[i] = square(item)
	}

	s, err := New(16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for run := 0; run < 20; run++ {
		got := s.Gather(items, square)
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: Gather = %v, want %v", run, got, want)
		}
	}
}

// gatherWorkerAppend models what one worker goroutine does in the racy
// version of scatter-gather that Gather does not contain: it appends
// straight onto a slice declared outside every worker, instead of a
// private local slice. It is never exported and never reachable from
// Scatterer -- Gather has no code path that looks like this.
func gatherWorkerAppend(shared *[]Result, item int, fn ProcessFunc) {
	*shared = append(*shared, fn(item))
}

// TestSharedAppendTargetIsOneBackingArrayAcrossWorkers demonstrates,
// deterministically and without spawning a single goroutine, exactly what
// the racy pattern gets wrong: every worker's append target is the *same*
// slice variable, so every worker reads and writes the same backing array
// through the same pointer. That shared, mutable target is what turns
// concurrent, unsynchronized calls to gatherWorkerAppend into a data race --
// go test -race would flag it the moment two goroutines called it on the
// same *shared at once. This test deliberately never does that: a test
// suite that must always pass under -race cannot also reproduce a genuine
// race, so it pins the structural cause instead of the nondeterministic
// symptom.
//
// Contrast this with Gather, where make([]Result, 0, end-start) inside each
// worker's own goroutine closure means there is no shared variable at all --
// nothing here for two goroutines to even race over.
func TestSharedAppendTargetIsOneBackingArrayAcrossWorkers(t *testing.T) {
	t.Parallel()

	var shared []Result
	gatherWorkerAppend(&shared, 1, square) // "worker 0"
	firstBackingArray := unsafe.SliceData(shared[:cap(shared)])

	gatherWorkerAppend(&shared, 2, square) // "worker 1", the identical *shared
	secondBackingArray := unsafe.SliceData(shared[:cap(shared)])

	if len(shared) != 2 || shared[0].Item != 1 || shared[1].Item != 2 {
		t.Fatalf("shared = %v, want two results written through the same *shared", shared)
	}
	if firstBackingArray != secondBackingArray {
		// A reallocation between the two calls is still the same underlying
		// problem: both "workers" wrote through the one *shared pointer,
		// which is exactly what a second, real goroutine would race on.
		t.Log("append reallocated between calls; both writes still went through the same *shared pointer")
	}
}

// ExampleScatterer_Gather is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below. It models a search fan-out over 12 document IDs with a purely
// deterministic scoring function, so its output is reproducible.
func ExampleScatterer_Gather() {
	s, err := New(4)
	if err != nil {
		panic(err)
	}

	docIDs := make([]int, 12)
	for i := range docIDs {
		docIDs[i] = i + 1
	}

	results := s.Gather(docIDs, func(docID int) Result {
		return Result{Item: docID, Value: docID * docID % 97}
	})
	fmt.Println(results)

	// Output:
	// [{1 1} {2 4} {3 9} {4 16} {5 25} {6 36} {7 49} {8 64} {9 81} {10 3} {11 24} {12 47}]
}
```

## Review

`Gather` is correct when its output is both race-free under `-race` and
identical, sorted, and complete regardless of how many workers processed
the input. The design earns both properties from two separate mechanisms
that are easy to conflate: isolation (each goroutine owns its own `local`
slice and its own index of `shards`, so there is no shared mutable state
for the race detector to catch) and determinism (the final sort by `Item`,
which is what makes `workers=1` and `workers=4` produce byte-identical
output despite completely different goroutine scheduling). A version that
got isolation right but skipped the sort would still be race-free and
still pass `-race`, but its output order would vary run to run — a bug
that only a repeated-run test like `TestGatherRepeatedRunsAreDeterministic`
reliably catches, since a single run can get lucky.
`TestSharedAppendTargetIsOneBackingArrayAcrossWorkers` exists to name the
antipattern precisely without ever letting the test suite depend on
actually racing — that would contradict the very property the suite is
there to guarantee. Run `go test -count=1 -race ./...`.

## Resources

- [`slices.Concat`](https://pkg.go.dev/slices#Concat) — flattens the per-goroutine `[][]Result` into one `[]Result` after every goroutine has finished.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the barrier that makes it safe to read every goroutine's shard after `Wait()` returns.
- [The Go Memory Model](https://go.dev/ref/mem) — why an unsynchronized shared `append` from multiple goroutines is undefined behavior, not just "usually fine."
- [Go blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) — how `-race` finds exactly the class of bug this module's racy alternative describes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-sql-in-clause-args-prealloc.md](12-sql-in-clause-args-prealloc.md) | Next: [14-zero-alloc-scratch-field-splitter.md](14-zero-alloc-scratch-field-splitter.md)
