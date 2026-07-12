# Exercise 11: Growing a Bulk-Insert Row Slice Without Repeated Reallocation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A batch job that flushes to the database with a bulk `INSERT ... VALUES
(?,?),(?,?),...` does not run once; it runs the same loop forever — drain up
to N items off a queue, build the row slice, send it, repeat. The instinctive
way to write that loop declares a fresh `var batch []Row` at the top of every
cycle and appends into it. Each cycle then pays the full cost of growing a
slice from nothing: Go reallocates the backing array and copies everything
appended so far every time capacity runs out, and the next cycle throws that
work away and starts over from zero. On a hot batching path that runs
thousands of times a day, that is thousands of avoidable allocation storms.

The fix is not a bigger one-shot `make`, because a batcher does not know its
final size once — it needs the same capacity, cycle after cycle, for as long
as the process runs. `slices.Grow` is the operation for exactly that shape:
reserve room for N more elements on a slice that already exists, without
discarding what is already there. Combined with keeping the same backing
array between cycles — resetting to `s[:0]` instead of `nil` after each
flush — a batcher converges on a stable capacity after its first cycle or
two and allocates only its return value from then on.

This module builds that batcher as a package: a `Builder` that reserves
capacity at construction, grows in controlled steps only when a batch
outgrows it, and hands back an independent copy of the batch on every flush
so a caller can never corrupt the Builder's internal state by mutating what
it received. The naive form — rebuilding the batch from `nil` every cycle —
never appears in the package API; it lives only in the test file, as the
thing the allocation test proves worse.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
bulkinsert/               module example.com/bulkinsert
  go.mod                  go 1.24
  bulkinsert.go            Row, Builder; New, Add, Len, Flush; one sentinel error
  bulkinsert_test.go       add/flush table, aliasing, hint-of-one edge, allocation
                          property vs a naive nil-start rebuild, ExampleBuilder
```

- Files: `bulkinsert.go`, `bulkinsert_test.go`.
- Implement: `New(hint int) (*Builder, error)` rejecting a non-positive hint with `ErrInvalidBatchHint`; `(*Builder).Add(row Row)` appending into reserved capacity and calling `slices.Grow(b.rows, b.hint)` only when the batch has filled it; `(*Builder).Len() int`; `(*Builder).Flush() []Row` returning `slices.Clone` of the batch and resetting to `b.rows[:0]` for reuse.
- Test: the add/flush table across empty, single-row, and past-the-hint batches sharing one `Builder` across sequential cycles; a non-positive hint rejected with `errors.Is`; the flushed batch never aliasing the `Builder`'s storage (both by mutation and by `unsafe.SliceData`); a hint of exactly `1`; a `buildBatchNaive` contrast proving a reused `Builder` allocates strictly less per cycle than rebuilding from `nil`; and `ExampleBuilder` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### slices.Grow is preallocation for a slice that already has content

`make([]T, 0, n)` only works when a slice is being created fresh, because
`make` always starts from nothing. A batcher's `Builder` is never created
fresh after its first cycle — it already holds whatever capacity the
previous cycle left it with, and `Add` needs to reserve more room *on top
of* that, not discard it and start over. `slices.Grow(s, n)` is the named
operation for that: it returns a slice with the same length and elements as
`s`, but capacity guaranteed for at least `n` more appends without
reallocating. It is `make(..., 0, n)`'s counterpart for a slice that is not
starting from zero.

The naive version of a batching loop looks like this, and it is worth
writing out because nothing about it looks wrong at the call site:

```go
func buildBatchNaive(n int) []Row {
    var batch []Row              // nil: cap 0, every single cycle
    for i := 0; i < n; i++ {
        batch = append(batch, Row{ID: i})
    }
    return batch
}
```

Every call starts from `cap == 0`. Go's growth curve then reallocates the
backing array and copies everything appended so far several times before
`batch` reaches its final size — work this function repeats, in full, on
every call, forever. A `Builder` that keeps its backing array between cycles
and only calls `Grow` when it actually runs out of room does the equivalent
growth work once, the very first time it needs more than its initial hint,
and reuses the resulting capacity indefinitely after that.

Create `bulkinsert.go`:

```go
// Package bulkinsert accumulates rows for a bulk SQL INSERT one batch at a
// time, reusing the same backing array across every batch cycle instead of
// rebuilding the row slice from nil for each new batch.
//
// A batch job that flushes to the database usually runs the same batching
// loop forever: drain up to N items off a queue, build the row slice for
// "INSERT ... VALUES (?,?),(?,?),...", send it, repeat. Rebuilding that row
// slice from nil on every cycle throws away everything the previous cycle's
// growth already paid for. Builder keeps its backing array between cycles
// and uses slices.Grow, not append-from-nil, whenever it needs more room, so
// a long-running batcher allocates a handful of times total, not once per
// batch forever.
package bulkinsert

import (
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidBatchHint means the configured batch-size hint was not positive.
var ErrInvalidBatchHint = errors.New("bulkinsert: batch size hint must be positive")

// Row is one row queued for a bulk INSERT statement.
type Row struct {
	ID   int
	Cols []any
}

// Builder accumulates rows for one bulk INSERT batch at a time and is meant
// to be kept alive across many batch cycles: construct one at startup, call
// Add for every queued item, call Flush to obtain and clear the batch, and
// repeat with the same Builder.
//
// A Builder is not safe for concurrent use. The caller must synchronize
// access to a shared *Builder -- with a mutex, or by giving each worker
// goroutine its own Builder.
type Builder struct {
	rows []Row
	hint int
}

// New returns a Builder that reserves capacity for hint rows immediately and
// re-reserves hint more rows at a time whenever a batch outgrows its current
// capacity, so steady-state batching converges on a stable backing array
// instead of growing by append's default geometric steps. It returns
// ErrInvalidBatchHint if hint is not positive.
func New(hint int) (*Builder, error) {
	if hint <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidBatchHint, hint)
	}
	return &Builder{rows: make([]Row, 0, hint), hint: hint}, nil
}

// Add queues row for the batch currently being built. When the batch has
// filled its reserved capacity, Add calls slices.Grow to reserve room for
// hint more rows in one step, rather than letting a plain append grow the
// backing array by whatever increment the runtime's growth curve picks.
func (b *Builder) Add(row Row) {
	if len(b.rows) == cap(b.rows) {
		b.rows = slices.Grow(b.rows, b.hint)
	}
	b.rows = append(b.rows, row)
}

// Len reports how many rows are queued in the batch that has not yet been
// flushed.
func (b *Builder) Len() int { return len(b.rows) }

// Flush returns the queued rows as a freshly cloned slice and clears the
// Builder for the next batch cycle.
//
// The returned slice is an independent copy: it never aliases the Builder's
// internal storage, so the caller may retain it, hand it to a driver, or
// mutate it, and later Add calls on this Builder cannot affect it. Flush
// always returns a non-nil slice, even for an empty batch, so a caller that
// serializes it gets [] rather than null.
//
// Clearing keeps the Builder's backing array (b.rows[:0]) instead of
// discarding it, so the next cycle's Add calls reuse the same reserved
// capacity rather than starting from nil.
func (b *Builder) Flush() []Row {
	out := slices.Clone(b.rows)
	b.rows = b.rows[:0]
	return out
}
```

### Using it

Construct one `Builder` at startup with a hint sized from whatever bound
your batching loop actually has — a queue drain limit, a configured flush
size — and keep it alive for the life of the batcher. Call `Add` as items
arrive, `Flush` when it is time to send, and hand the returned slice
straight to the driver: the aliasing contract on `Flush` guarantees it is
safe to retain past the next `Add`. Because `Builder` is not safe for
concurrent use, a batcher with multiple producer goroutines needs either a
mutex around it or one `Builder` per goroutine merged downstream.

`ExampleBuilder` is the runnable demonstration of this module: `go test`
executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift away from the code.

```go
func ExampleBuilder() {
	b, err := New(2)
	if err != nil {
		panic(err)
	}

	b.Add(Row{ID: 1})
	b.Add(Row{ID: 2})
	first := b.Flush()
	fmt.Println("batch 1:", len(first), first[0].ID, first[1].ID)

	b.Add(Row{ID: 3})
	fmt.Println("pending:", b.Len())
	second := b.Flush()
	fmt.Println("batch 2:", len(second), second[0].ID)

	// Output:
	// batch 1: 2 1 2
	// pending: 1
	// batch 2: 1 3
}
```

### Tests

`TestBuilderAddAndFlush` deliberately shares one `Builder` across its whole
table instead of constructing a fresh one per case, because the property
under test — that `Flush` clears the batch and the next cycle starts clean —
only exists across cycles; the subtests therefore run sequentially, without
`t.Parallel`. `TestFlushDoesNotAliasBuilder` and
`TestFlushReturnsIndependentBackingArray` are two independent proofs of the
same aliasing contract: the first mutates a flushed batch and checks the
Builder is unaffected, the second uses `unsafe.SliceData` to show the
returned slice's backing array is never the Builder's own.
`TestBuilderWithHintOfOne` is the tightest boundary this design has — a hint
of `1` means every `Add` past the first forces a `Grow` — and
`TestNewRejectsNonPositiveHint` locks in the constructor's validation.

`TestBuilderReuseAllocatesLessThanNaive` is the heart of the module.
`buildBatchNaive` is unexported and unreachable from the package API; it
exists so the test can state the allocation cost as a measured property —
`reused < naive` — rather than a claim in a comment, and never as an exact
reallocation count, since the runtime's growth curve is not a contract this
test can depend on. It deliberately skips `t.Parallel`, because
`testing.AllocsPerRun` panics if it runs from a parallel test: a concurrent
goroutine allocating in the background would corrupt the measurement.

Create `bulkinsert_test.go`:

```go
package bulkinsert

import (
	"errors"
	"fmt"
	"testing"
	"unsafe"
)

// buildBatchNaive is the batching loop as it is usually written first, and
// as it ships: a fresh nil slice on every batch cycle, grown by repeated
// append. It is never exported and never reachable from the package API; it
// exists only so the tests can pin how much more it allocates than a Builder
// reused across cycles.
func buildBatchNaive(n int) []Row {
	var batch []Row
	for i := 0; i < n; i++ {
		batch = append(batch, Row{ID: i})
	}
	return batch
}

func TestNewRejectsNonPositiveHint(t *testing.T) {
	t.Parallel()

	for _, hint := range []int{0, -1} {
		if _, err := New(hint); !errors.Is(err, ErrInvalidBatchHint) {
			t.Errorf("New(%d) error = %v, want ErrInvalidBatchHint", hint, err)
		}
	}
}

// TestBuilderAddAndFlush shares one Builder across its whole table on
// purpose, each case building on the batch state the previous case left
// behind (Flush clears it), so the subtests run sequentially and do not
// call t.Parallel.
func TestBuilderAddAndFlush(t *testing.T) {
	b, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name    string
		add     []int
		wantIDs []int
	}{
		{name: "empty batch flushes empty", add: nil, wantIDs: []int{}},
		{name: "single row", add: []int{1}, wantIDs: []int{1}},
		{name: "batch past the reserved hint", add: []int{1, 2, 3, 4, 5}, wantIDs: []int{1, 2, 3, 4, 5}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range tc.add {
				b.Add(Row{ID: id})
			}
			if b.Len() != len(tc.add) {
				t.Fatalf("Len() = %d, want %d", b.Len(), len(tc.add))
			}

			batch := b.Flush()
			if len(batch) != len(tc.wantIDs) {
				t.Fatalf("len(batch) = %d, want %d: %+v", len(batch), len(tc.wantIDs), batch)
			}
			for i, want := range tc.wantIDs {
				if batch[i].ID != want {
					t.Errorf("batch[%d].ID = %d, want %d", i, batch[i].ID, want)
				}
			}
			if batch == nil {
				t.Error("batch is nil; Flush must always return a non-nil slice")
			}
			if b.Len() != 0 {
				t.Errorf("Len() after Flush = %d, want 0", b.Len())
			}
		})
	}
}

// TestFlushDoesNotAliasBuilder proves the returned batch is an independent
// copy: mutating it, or queuing more rows on the Builder afterward, must
// never change a batch that was already flushed.
func TestFlushDoesNotAliasBuilder(t *testing.T) {
	t.Parallel()

	b, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.Add(Row{ID: 1})
	first := b.Flush()

	first[0].ID = 999
	b.Add(Row{ID: 2})
	second := b.Flush()

	if second[0].ID != 2 {
		t.Fatalf("second batch = %+v, want ID 2; mutating the first batch leaked into the Builder", second)
	}
}

// TestBuilderWithHintOfOne covers the smallest valid hint: every single Add
// past the first must force a Grow, since there is never more than one slot
// of spare capacity at a time.
func TestBuilderWithHintOfOne(t *testing.T) {
	t.Parallel()

	b, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 1; i <= 3; i++ {
		b.Add(Row{ID: i})
	}
	batch := b.Flush()
	for i, want := range []int{1, 2, 3} {
		if batch[i].ID != want {
			t.Errorf("batch[%d].ID = %d, want %d", i, batch[i].ID, want)
		}
	}
}

// TestFlushReturnsIndependentBackingArray proves the batch Flush returns
// does not share a backing array with the Builder's own storage -- the
// aliasing contract documented on Flush -- by comparing data pointers with
// unsafe.SliceData rather than relying only on the mutation test above.
func TestFlushReturnsIndependentBackingArray(t *testing.T) {
	t.Parallel()

	b, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.Add(Row{ID: 1})

	batch := b.Flush()
	if unsafe.SliceData(batch) == unsafe.SliceData(b.rows[:cap(b.rows)]) {
		t.Fatal("Flush returned the Builder's own backing array; batch is not independent")
	}
}

// TestBuilderReuseAllocatesLessThanNaive is the point of this module: a
// Builder kept alive across batch cycles allocates once per cycle (the
// Clone in Flush), while rebuilding the batch from nil every cycle pays for
// however many reallocations the runtime's growth curve needs to reach the
// same size. The exact reallocation count is never asserted, only the
// property that reused allocates strictly less than naive.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestBuilderReuseAllocatesLessThanNaive(t *testing.T) {
	const rowsPerBatch = 200

	b, err := New(rowsPerBatch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Warm up once so the Builder's first-ever Grow does not skew the
	// measurement of its steady-state behavior.
	for i := 0; i < rowsPerBatch; i++ {
		b.Add(Row{ID: i})
	}
	b.Flush()

	reused := testing.AllocsPerRun(50, func() {
		for i := 0; i < rowsPerBatch; i++ {
			b.Add(Row{ID: i})
		}
		_ = b.Flush()
	})
	naive := testing.AllocsPerRun(50, func() {
		_ = buildBatchNaive(rowsPerBatch)
	})

	if !(reused < naive) {
		t.Fatalf("allocations per cycle: reused = %v, naive = %v; want reused < naive", reused, naive)
	}
}

// ExampleBuilder is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleBuilder() {
	b, err := New(2)
	if err != nil {
		panic(err)
	}

	b.Add(Row{ID: 1})
	b.Add(Row{ID: 2})
	first := b.Flush()
	fmt.Println("batch 1:", len(first), first[0].ID, first[1].ID)

	b.Add(Row{ID: 3})
	fmt.Println("pending:", b.Len())
	second := b.Flush()
	fmt.Println("batch 2:", len(second), second[0].ID)

	// Output:
	// batch 1: 2 1 2
	// pending: 1
	// batch 2: 1 3
}
```

## Review

`Builder` is correct when every `Flush` returns exactly the rows queued
since the previous one, in order, as a slice the caller can freely mutate
without touching the Builder's own state. `New` rejects a non-positive hint
with `ErrInvalidBatchHint`, checkable with `errors.Is`. The core technique is
`slices.Grow`, called only when `Add` finds the batch has filled its
reserved capacity, paired with resetting to `b.rows[:0]` — not `nil` — after
every `Flush`, so a long-lived batcher converges on a stable backing array
after its first cycle or two instead of paying a full reallocation storm on
every single cycle the way `buildBatchNaive` does. `TestBuilderReuseAllocatesLessThanNaive`
is the test that pins that difference, as a property (`reused < naive`)
rather than a specific count, since the exact reallocation curve is a
runtime detail and not part of any contract. `Builder` is not safe for
concurrent use; a caller with concurrent producers must synchronize access
itself. Run `go test -count=1 -race ./...`.

## Resources

- [`slices.Grow`](https://pkg.go.dev/slices#Grow) — reserves capacity on an existing slice without discarding its elements.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the independent copy `Flush` returns, backed by a fresh array.
- [`append`](https://go.dev/ref/spec#Appending_and_copying_slices) — why repeated appends from `nil` reallocate and copy more than once.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-make-len-vs-cap-mapper-bug.md](10-make-len-vs-cap-mapper-bug.md) | Next: [12-sql-in-clause-args-prealloc.md](12-sql-in-clause-args-prealloc.md)
