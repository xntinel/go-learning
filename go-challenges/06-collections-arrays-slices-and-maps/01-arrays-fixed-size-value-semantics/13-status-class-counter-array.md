# Exercise 13: Counting HTTP Status Classes in a Fixed [5]uint64 Array

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A reverse proxy or an API gateway metrics path needs to count responses by
status class — 1xx, 2xx, 3xx, 4xx, 5xx — on every single request, from every
goroutine handling one concurrently, which means the counter has to be as
cheap and as safe as a metrics increment can possibly be. The domain is
small, dense, and known at compile time: exactly five buckets, indexed by a
trivial formula off the status code. That is exactly the shape a fixed array
beats a map on every axis that matters. This module builds that counter,
with the index arithmetic guarded so a malformed status code returns an
error instead of corrupting memory, and every bucket backed by an
`atomic.Uint64` so it stays correct under real concurrent request traffic.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
statusclass/                 module example.com/statusclass
  go.mod                     go 1.24
  counter.go                 Counter{counts [5]atomic.Uint64}; Record/Count/Snapshot; ErrOutOfRange
  counter_test.go            record+count table, out-of-range codes, boundary classes,
                              snapshot isolation, unguarded-index-panics contrast,
                              concurrent record, ExampleCounter_Record
```

- Files: `counter.go`, `counter_test.go`.
- Implement: `Counter` backed by `counts [NumClasses]atomic.Uint64`; `classIndex(code int) (int, error)` computing `code/100-1` after validating `100 <= code <= 599`; `Record(code int) error`, `Count(class int) (uint64, error)`, and `Snapshot() [NumClasses]uint64` returning an independent array of atomically-loaded totals.
- Test: a table of recorded codes across all five classes matches `Count`; out-of-range codes (`0`, `42`, `600`, negative) are rejected on both `Record` and `Count` without perturbing any bucket; the two boundary codes of the first and last class (`100`, `199`, `500`, `599`); a `Snapshot` mutated by the caller does not affect the live `Counter`; indexing a raw `[NumClasses]uint64` at an unguarded out-of-range index panics; many goroutines recording across all five classes at once sum correctly under `-race`; and `ExampleCounter_Record` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a five-slot array beats a map here

`map[int]uint64` would work for this counter, but it is the wrong tool: it
hashes an `int` key, allocates buckets, and pays for growth machinery to
solve a problem that has exactly five possible answers, known before the
program even compiles. A `[5]atomic.Uint64` array has none of that overhead.
Its zero value is already a valid, fully-initialized counter — no `make`, no
nil check — and indexing it is a single bounds-checked array access, the
cheapest read/write operation the language has. On a request path handling
tens of thousands of responses per second from many goroutines at once, that
difference is not academic: it is the gap between a hash and a memory
offset. The general rule is that whenever a domain is small, dense, and
enumerable at compile time — status classes, log levels, weekdays, priority
tiers — a fixed array indexed by a formula is both faster and simpler than a
map.

The index arithmetic is also where the risk lives. `code/100 - 1` maps 100
to 0, 599 to 4, and any code outside `[100, 599]` to something outside
`[0, 4]` — including negative indices for anything under 100. Indexing an
array outside its bounds does not silently clamp or wrap in Go; it panics
immediately:

```go
var counts [NumClasses]uint64
idx := 999/100 - 1 // -1 without a guard
counts[idx] = 1    // panics: index out of range [-1]
```

A panicking metrics call in a request handler is a self-inflicted outage.
`classIndex` centralizes the arithmetic behind one guard so neither `Record`
nor `Count` can ever reach the array with a bad index; the fragment above is
never part of the package API, it exists only in the test file to show what
skipping the guard actually does.

Each bucket is an `atomic.Uint64` rather than a plain `uint64` because the
whole point of a status-class counter is to be incremented from every
request-handling goroutine concurrently, and a plain `counts[idx]++` is a
read-modify-write that is not safe under concurrent writers — two
goroutines incrementing the same bucket at once can lose an update. This
module does not go further and pad each counter to its own cache line; the
next exercise in this lesson covers that refinement, which matters once
contention itself, not just correctness, is the bottleneck.

Create `counter.go`:

```go
// Package statusclass counts HTTP responses by status class (1xx..5xx) in a
// fixed-size array, cheap enough to update on every single request a
// reverse proxy or API gateway handles.
package statusclass

import (
	"errors"
	"sync/atomic"
)

// NumClasses is the number of HTTP status classes: 1xx, 2xx, 3xx, 4xx, 5xx.
const NumClasses = 5

// ErrOutOfRange means the status code does not fall in the 100-599 range
// that maps onto one of the five status classes.
var ErrOutOfRange = errors.New("statusclass: code out of range 100-599")

// Counter tallies HTTP responses by status class in a fixed
// [NumClasses]atomic.Uint64 array indexed by code/100-1. For a metrics hot
// path that only ever needs five dense, known-up-front buckets, the array
// beats a map[int]uint64 on every axis that matters: no hashing, no bucket
// allocation, and a zero value that is already a valid empty counter with
// no map initialization required.
//
// Counter is safe for concurrent use by multiple goroutines: every access
// goes through atomic.Uint64, so many request-handling goroutines may call
// Record at once with no external lock. It does not pad each counter to its
// own cache line -- see the shard-counters exercise in this lesson for that
// further refinement, which matters once contention, not just correctness,
// becomes the bottleneck.
type Counter struct {
	counts [NumClasses]atomic.Uint64
}

// classIndex converts an HTTP status code to its class index (0 for 1xx,
// 4 for 5xx) or reports ErrOutOfRange for anything outside 100-599. This is
// the one place the index arithmetic lives, so both Record and Count apply
// the same guard before touching the array.
func classIndex(code int) (int, error) {
	if code < 100 || code > 599 {
		return 0, ErrOutOfRange
	}
	return code/100 - 1, nil
}

// Record increments the bucket for code's status class. It returns
// ErrOutOfRange, and leaves every bucket unchanged, if code is not a valid
// HTTP status code.
func (c *Counter) Record(code int) error {
	idx, err := classIndex(code)
	if err != nil {
		return err
	}
	c.counts[idx].Add(1)
	return nil
}

// Count returns how many times a code in class's HTTP status class has been
// recorded. It returns ErrOutOfRange if class is not a valid status code
// (any code within the class works, e.g. 404 and 499 both select the 4xx
// bucket).
func (c *Counter) Count(class int) (uint64, error) {
	idx, err := classIndex(class)
	if err != nil {
		return 0, err
	}
	return c.counts[idx].Load(), nil
}

// Snapshot returns the five class totals as an independent array value,
// each loaded atomically. Because the return type is [NumClasses]uint64,
// not a slice, the caller receives a copy that further calls to Record
// cannot change and cannot corrupt the live Counter by mutating.
func (c *Counter) Snapshot() [NumClasses]uint64 {
	var snap [NumClasses]uint64
	for i := range c.counts {
		snap[i] = c.counts[i].Load()
	}
	return snap
}
```

### Using it

A `Counter`'s zero value is ready to use — no constructor is needed, which
is itself a property of a fixed array with no dynamic state to initialize.
Embed one in a metrics registry, call `Record` from every response-writing
path, and expose `Snapshot` to whatever scrapes the `/metrics` endpoint;
`TestConcurrentRecordSumsCorrectly` is the direct proof that many goroutines
calling `Record` across all five classes at once, under `-race`, still land
on the exact expected totals.

The aliasing contract lives on `Snapshot`: the returned `[NumClasses]uint64`
is a fresh array, so a caller may hold it, format it, or mutate it while
`Record` keeps running concurrently, with neither side able to observe or
corrupt the other's state — `TestSnapshotIsIndependentCopy` pins that. The
module has no `main.go`, because a metrics counter is a library, not a
tool. Its executable demonstration is `ExampleCounter_Record`: `go test`
runs it and compares its standard output against the `// Output:` comment,
so the usage shown below cannot drift away from the code.

### Tests

`TestRecordAndCount` records a mix of codes across all five classes and
checks each class's total. `TestRecordOutOfRange` sweeps a set of invalid
codes and confirms every one is rejected with `ErrOutOfRange` and that none
of them perturbed any bucket; `TestCountOutOfRange` checks the same guard
from the read side. `TestRecordBoundaryClasses` pins the two edges of the
valid range, `100`/`199` and `500`/`599`, so an off-by-one in `classIndex`
would fail here rather than in production traffic that happens to avoid
those exact codes in testing.

`TestSnapshotIsIndependentCopy` reuses the array-value-semantics guarantee
from earlier in this lesson, now applied to a metrics read: a
`[N]uint64` snapshot is safe to hand to a `/metrics` handler with no lock and
no risk of the handler corrupting live counts. `TestUnguardedIndexPanics` is
the antipattern contrast: it shows, with `recover`, exactly what happens if
`classIndex`'s guard is skipped and a computed index is used to index a raw
array directly — the concrete justification for why `Record` and `Count`
never do that. `TestConcurrentRecordSumsCorrectly` is the concurrency case
this exercise exists for: fifty goroutines per class, all incrementing at
once, verified under `-race`, checking every class lands exactly on
`goroutines * records-per-goroutine` with no lost update.

Create `counter_test.go`:

```go
package statusclass

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestRecordAndCount(t *testing.T) {
	t.Parallel()

	var c Counter
	codes := []int{200, 201, 404, 404, 500, 301, 100}
	for _, code := range codes {
		if err := c.Record(code); err != nil {
			t.Fatalf("Record(%d) = %v, want nil", code, err)
		}
	}

	tests := []struct {
		class int
		want  uint64
	}{
		{100, 1}, // 1xx
		{200, 2}, // 2xx
		{301, 1}, // 3xx
		{404, 2}, // 4xx
		{500, 1}, // 5xx
	}
	for _, tc := range tests {
		got, err := c.Count(tc.class)
		if err != nil {
			t.Fatalf("Count(%d) = %v, want nil", tc.class, err)
		}
		if got != tc.want {
			t.Errorf("Count(%d) = %d, want %d", tc.class, got, tc.want)
		}
	}
}

func TestRecordOutOfRange(t *testing.T) {
	t.Parallel()

	var c Counter
	for _, code := range []int{0, 42, 99, 600, 999, -1} {
		if err := c.Record(code); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Record(%d) = %v, want ErrOutOfRange", code, err)
		}
	}

	// A rejected code must not have perturbed any bucket.
	for class := 1; class <= 5; class++ {
		got, err := c.Count(class * 100)
		if err != nil {
			t.Fatalf("Count(%d) = %v, want nil", class*100, err)
		}
		if got != 0 {
			t.Errorf("Count(%d) = %d after only out-of-range Records, want 0", class*100, got)
		}
	}
}

func TestCountOutOfRange(t *testing.T) {
	t.Parallel()

	var c Counter
	for _, class := range []int{0, 99, 600, -100} {
		if _, err := c.Count(class); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Count(%d) = %v, want ErrOutOfRange", class, err)
		}
	}
}

func TestRecordBoundaryClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
	}{
		{name: "lowest 1xx", code: 100},
		{name: "highest 1xx", code: 199},
		{name: "lowest 5xx", code: 500},
		{name: "highest 5xx", code: 599},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var c Counter
			if err := c.Record(tc.code); err != nil {
				t.Fatalf("Record(%d) = %v, want nil", tc.code, err)
			}
			got, err := c.Count(tc.code)
			if err != nil {
				t.Fatalf("Count(%d) = %v, want nil", tc.code, err)
			}
			if got != 1 {
				t.Fatalf("Count(%d) = %d, want 1", tc.code, got)
			}
		})
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	t.Parallel()

	var c Counter
	if err := c.Record(200); err != nil {
		t.Fatalf("Record: %v", err)
	}

	snap := c.Snapshot()
	snap[1] = 999 // mutate the caller's copy (index 1 is the 2xx bucket)

	got, err := c.Count(200)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 1 {
		t.Fatalf("Counter mutated via snapshot: Count(200) = %d, want 1", got)
	}
}

// TestUnguardedIndexPanics shows, with recover, what Record and Count avoid
// by always routing through classIndex: indexing a raw [NumClasses]uint64
// at an index computed without the range guard panics immediately.
func TestUnguardedIndexPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("indexing counts[] past NumClasses-1 should panic")
		}
	}()

	var counts [NumClasses]uint64
	idx := 999/100 - 1 // out of [0, NumClasses) but computed without the guard
	counts[idx] = 1     // panics: index out of range
}

// TestConcurrentRecordSumsCorrectly drives Record from many goroutines
// across every status class at once. Because every bucket is an
// atomic.Uint64, this must pass under -race with no lost increments.
func TestConcurrentRecordSumsCorrectly(t *testing.T) {
	t.Parallel()

	const goroutinesPerClass = 50
	const recordsPerGoroutine = 100
	classCodes := [NumClasses]int{100, 200, 300, 400, 500}

	var c Counter
	var wg sync.WaitGroup
	for _, code := range classCodes {
		for g := 0; g < goroutinesPerClass; g++ {
			wg.Add(1)
			go func(code int) {
				defer wg.Done()
				for i := 0; i < recordsPerGoroutine; i++ {
					if err := c.Record(code); err != nil {
						t.Errorf("Record(%d): %v", code, err)
					}
				}
			}(code)
		}
	}
	wg.Wait()

	want := uint64(goroutinesPerClass * recordsPerGoroutine)
	for _, code := range classCodes {
		got, err := c.Count(code)
		if err != nil {
			t.Fatalf("Count(%d): %v", code, err)
		}
		if got != want {
			t.Fatalf("Count(%d) = %d, want %d", code, got, want)
		}
	}
}

// ExampleCounter_Record is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment below.
func ExampleCounter_Record() {
	var c Counter
	for _, code := range []int{200, 201, 404, 500, 200} {
		if err := c.Record(code); err != nil {
			panic(err)
		}
	}

	snap := c.Snapshot()
	fmt.Printf("1xx=%d 2xx=%d 3xx=%d 4xx=%d 5xx=%d\n",
		snap[0], snap[1], snap[2], snap[3], snap[4])

	if err := c.Record(999); errors.Is(err, ErrOutOfRange) {
		fmt.Println("rejected:", err)
	}

	// Output:
	// 1xx=0 2xx=3 3xx=0 4xx=1 5xx=1
	// rejected: statusclass: code out of range 100-599
}
```

Both `TestRecordAndCount` and `TestConcurrentRecordSumsCorrectly` construct
a `Counter` with `var c Counter` rather than a constructor function. That is
deliberate: a fixed array's zero value has every element already at its own
zero value, so there is no invariant a constructor would need to establish,
and no `New` to forget to call before the first `Record`.

## Review

The counter is correct when every recorded code lands in the class its
first digit implies, and `TestRecordAndCount` plus
`TestRecordBoundaryClasses` check that across all five classes and both
edges of the valid range. `TestRecordOutOfRange` and `TestCountOutOfRange`
are the load-bearing tests: they prove `classIndex`'s guard rejects every
malformed code cleanly on both the write and read paths, with no bucket
corrupted along the way, which is the entire reason the guard exists ahead
of a raw array index — `TestUnguardedIndexPanics` shows what skipping it
costs. `TestSnapshotIsIndependentCopy` confirms a `/metrics` handler can
read a `Snapshot` with no lock and no risk of corrupting live counts.
`TestConcurrentRecordSumsCorrectly` is why every bucket is an
`atomic.Uint64` rather than a plain integer: two hundred and fifty
goroutines racing `Record` across five classes, verified under `-race`,
with no lost increments. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — array indexing and its panic on an out-of-range index.
- [Go Specification: Array types](https://go.dev/ref/spec#Array_types) — why a fixed-size array's zero value is already fully usable.
- [sync/atomic package](https://pkg.go.dev/sync/atomic) — `atomic.Uint64` and the `Add`/`Load` methods each bucket uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-session-id-array-key-idempotency.md](12-session-id-array-key-idempotency.md) | Next: [14-cache-line-padded-shard-counters.md](14-cache-line-padded-shard-counters.md)
