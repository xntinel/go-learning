# Exercise 16: A Bounded Queue With DropOldest and RejectNew Overflow

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An in-memory bounded queue — a recent-events ring for a dashboard, a
pending-work buffer in front of a slow downstream, a rate limiter's sample
window — has to decide what happens when a producer tries to add past
capacity. Two policies cover most real systems: `DropOldest`, which evicts
the oldest entry to make room (a metrics ring buffer, a "last N log lines"
view), and `RejectNew`, which refuses the new entry and signals back-pressure
(an admission-controlled task queue). Both are easy to get wrong the same
way: reaching for `append` on overflow, or reslicing off the front and
appending onto that reslice, silently grows the backing array past the
ceiling the queue was supposed to enforce, defeating the entire point of
bounding it.

This module builds the queue as a package you can drop into a service: `New`
validates its capacity and returns an error, `Enqueue` never appends past the
array `New` allocated, and `Values` hands back a copy the caller cannot use
to reach into the queue's internals. The naive DropOldest form that reslices
and re-appends is not part of that API. It lives in the test file, where it
belongs, as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
boundedqueue/                 module example.com/boundedqueue
  go.mod                      go 1.24
  boundedqueue.go              OverflowPolicy, sentinel errors, Queue; New, Enqueue, Len, Cap, Values
  boundedqueue_test.go         boundary table, capacity-never-grows over 500 enqueues, Values is a copy,
                               naive-reslice contrast, ExampleQueue_Enqueue
```

- Files: `boundedqueue.go`, `boundedqueue_test.go`.
- Implement: `New(capacity int, policy OverflowPolicy) (*Queue, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Queue).Enqueue(v int) error` appending while there is room, and on overflow either shifting the buffer down in place and writing `v` into the freed last slot (`DropOldest`) or returning the sentinel `ErrFull` leaving the queue untouched (`RejectNew`); `Len()`, `Cap()`, `Values() []int`.
- Test: a table over both policies at exactly capacity, one past, and several past; `New` rejecting non-positive capacity; 500-enqueue capacity-stability checks for each policy; `Values` returning a copy; a `enqueueNaiveDropOldest` contrast proving the reslice-and-append idiom periodically reallocates while `Queue.Enqueue` never does; and `ExampleQueue_Enqueue` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/boundedqueue
cd ~/go-exercises/boundedqueue
go mod init example.com/boundedqueue
go mod edit -go=1.24
```

### Why capacity has to be fixed at allocation, not re-derived from append

`New` allocates the backing array exactly once, with `make([]int, 0,
capacity)`, and every subsequent operation is written to never grow past
that array. The obvious-looking alternative for `DropOldest` — on overflow,
reslice off the oldest element and append onto that: `buf = append(buf[1:],
v)` — looks like it reuses the array, and most of the time it does. But
`buf[1:]` inherits whatever spare capacity `buf` itself had past its length,
and once the queue is genuinely full that spare capacity is already zero, so
`buf[1:]` also has zero spare capacity of its own. The very next append has
nowhere to write and must allocate a new, larger array. That new array then
has a little slack again, which the next couple of overflow calls consume
before hitting the same wall — so the naive form does not fail once and stay
failed, it reallocates on a *recurring* cadence for as long as the queue keeps
receiving values, each time growing the backing array a little further past
the ceiling `capacity` was supposed to be. `RejectNew`'s equivalent trap is
simpler but just as real: a version that "helpfully" tries `append` first and
undoes it if the queue turns out to be full has already paid for whatever
allocation that `append` performed before the check ran.

`Queue.Enqueue` avoids both traps by never calling `append` once the queue is
full. `DropOldest`'s eviction is `copy(q.buf, q.buf[1:])` followed by writing
the new value into the last slot: `copy` moves the surviving elements down by
one position within the *same* backing array (it is defined to work correctly
on overlapping slices), and the final write lands in the slot `copy` just
freed. No index ever goes out of bounds, and no allocation ever happens after
the one `make` call in `New`. `RejectNew` is simpler still — it just declines
to touch `q.buf` at all once `len(q.buf) == capacity`.

Create `boundedqueue.go`:

```go
// Package boundedqueue implements a slice-backed queue whose backing array
// is allocated exactly once, at construction, and never grows past that
// ceiling. Overflow is handled by rearranging the existing array in place
// (DropOldest) or by refusing the write (RejectNew) -- never by letting
// append decide to allocate a larger one.
package boundedqueue

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by New and Enqueue. Callers should test for them
// with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidCapacity means New was called with a non-positive capacity.
	ErrInvalidCapacity = errors.New("boundedqueue: capacity must be positive")
	// ErrFull means Enqueue was called on a RejectNew queue already at
	// capacity.
	ErrFull = errors.New("boundedqueue: queue is full")
)

// OverflowPolicy selects what Enqueue does when the queue is already at
// capacity.
type OverflowPolicy int

const (
	// DropOldest discards the oldest queued value to make room for the new
	// one, so Enqueue always succeeds.
	DropOldest OverflowPolicy = iota
	// RejectNew leaves the queue untouched and returns ErrFull.
	RejectNew
)

// Queue is a slice-backed bounded queue of ints. Its backing array is
// allocated once, at capacity, in New, and no operation ever grows it past
// that ceiling: DropOldest overflow shifts the existing elements down in
// place instead of appending past capacity.
//
// A Queue is not safe for concurrent use. Callers that share one across
// goroutines must synchronize their own access, for example with a mutex.
type Queue struct {
	buf      []int
	capacity int
	policy   OverflowPolicy
}

// New returns an empty queue with room for at most capacity values, using
// the given overflow policy once that room is exhausted. It returns
// ErrInvalidCapacity if capacity is not positive.
func New(capacity int, policy OverflowPolicy) (*Queue, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Queue{
		buf:      make([]int, 0, capacity),
		capacity: capacity,
		policy:   policy,
	}, nil
}

// Enqueue adds v to the back of the queue. If the queue has spare room, v is
// simply appended. If the queue is full, behavior depends on the policy: a
// DropOldest queue shifts every existing element one slot toward the front
// (discarding the current oldest value) and writes v into the freed last
// slot, all within the existing backing array, so cap(q.buf) never changes.
// A RejectNew queue leaves its contents untouched and returns ErrFull.
func (q *Queue) Enqueue(v int) error {
	if len(q.buf) < q.capacity {
		q.buf = append(q.buf, v)
		return nil
	}

	if q.policy == RejectNew {
		return fmt.Errorf("%w: capacity %d", ErrFull, q.capacity)
	}
	copy(q.buf, q.buf[1:])
	q.buf[len(q.buf)-1] = v
	return nil
}

// Len reports the number of values currently queued.
func (q *Queue) Len() int { return len(q.buf) }

// Cap reports the backing array's capacity, which is fixed at New's
// capacity argument for the lifetime of the queue.
func (q *Queue) Cap() int { return cap(q.buf) }

// Values returns a copy of the queued values, oldest first. The returned
// slice is freshly allocated and does not alias the queue's internal
// buffer, so a caller may retain, sort, or mutate it without affecting the
// queue.
func (q *Queue) Values() []int {
	return append([]int(nil), q.buf...)
}
```

### Using it

Construct a `Queue` once with the capacity your subsystem needs and the
policy that matches its failure mode — `DropOldest` for a rolling window
that always accepts new data, `RejectNew` for a buffer that needs to push
back-pressure onto its producer. `New` validates the capacity for you and
returns `ErrInvalidCapacity` rather than leaving you to discover a zero-size
queue by watching every `Enqueue` mysteriously fail.

Two contracts cross the package boundary. `Cap()` is documented to never
move away from the value passed to `New`, which is the whole point of a
*bounded* queue rather than a queue that merely starts small — enforced by
`TestCapacityNeverGrows` and `TestRejectNewCapacityNeverGrows` across 500
enqueues each. And `Values()` returns a copy, so a caller can hold onto the
result, mutate it, or hand it to another goroutine without any risk of
reaching back into the queue's live buffer — `TestValuesReturnsACopy` pins
that. The module has no `main.go`, because a bounded queue is a library, not
a tool. Its executable demonstration is `ExampleQueue_Enqueue`: `go test`
runs it and compares its standard output against the `// Output:` comment,
so the usage shown below cannot drift away from the code.

```go
func ExampleQueue_Enqueue() {
	drop, err := New(3, DropOldest)
	if err != nil {
		panic(err)
	}
	for _, v := range []int{1, 2, 3, 4, 5} {
		_ = drop.Enqueue(v)
		fmt.Printf("DropOldest enqueue(%d): values=%v cap=%d\n", v, drop.Values(), drop.Cap())
	}

	reject, err := New(3, RejectNew)
	if err != nil {
		panic(err)
	}
	for _, v := range []int{1, 2, 3, 4, 5} {
		err := reject.Enqueue(v)
		fmt.Printf("RejectNew enqueue(%d): values=%v cap=%d err=%v\n", v, reject.Values(), reject.Cap(), err)
	}

	// Output:
	// DropOldest enqueue(1): values=[1] cap=3
	// DropOldest enqueue(2): values=[1 2] cap=3
	// DropOldest enqueue(3): values=[1 2 3] cap=3
	// DropOldest enqueue(4): values=[2 3 4] cap=3
	// DropOldest enqueue(5): values=[3 4 5] cap=3
	// RejectNew enqueue(1): values=[1] cap=3 err=<nil>
	// RejectNew enqueue(2): values=[1 2] cap=3 err=<nil>
	// RejectNew enqueue(3): values=[1 2 3] cap=3 err=<nil>
	// RejectNew enqueue(4): values=[1 2 3] cap=3 err=boundedqueue: queue is full: capacity 3
	// RejectNew enqueue(5): values=[1 2 3] cap=3 err=boundedqueue: queue is full: capacity 3
}
```

`cap=3` never changes for either policy across all five enqueues.
`DropOldest`'s window slides (`[1 2 3]` becomes `[2 3 4]` becomes `[3 4 5]`),
while `RejectNew` freezes at `[1 2 3]` and reports a wrapped `ErrFull` for
every enqueue past capacity.

### Tests

`TestEnqueueAtBoundary` is a table over both policies at exactly capacity,
one past it, and several past it, asserting both the resulting queue
contents and the returned error for every call in the sequence.
`TestCapacityNeverGrows` and `TestRejectNewCapacityNeverGrows` each drive
500 enqueues into a capacity-4 queue and assert `Cap()` stays 4 after every
single one — the property that makes this a *bounded* queue and not just a
queue that happens to look small right now. `TestValuesReturnsACopy` proves
`Values()` cannot be used to reach back into the queue's internal buffer.

`TestNaiveDropOldestReallocatesWhileBoundedQueueDoesNot` is the contrast
test at the heart of the module. It drives 300 overflow calls through an
already-full `Queue` and confirms zero allocations, then drives the same 300
calls through the unexported `enqueueNaiveDropOldest` helper and confirms a
nonzero allocation count. That helper reallocates only on roughly one call in
three, not every call, which rules out `testing.AllocsPerRun`: it computes
its average with integer division of the total allocation count by the run
count, so any true average below 1 — like the naive helper's roughly 0.33 —
truncates to the same 0 the bounded queue reports, silently hiding a real,
periodic allocation. The test reads `runtime.MemStats.Mallocs` directly
instead, before and after each loop, and asserts only that the naive delta
exceeds the bounded delta — a property, never a specific count, and one that
still catches the bug a fractional-average check would miss.

Create `boundedqueue_test.go`:

```go
package boundedqueue

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"
)

func TestEnqueueAtBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		policy     OverflowPolicy
		enqueue    []int
		wantValues []int
		wantErrs   []error
	}{
		{
			name:       "DropOldest fills exactly to capacity with no drops",
			policy:     DropOldest,
			enqueue:    []int{1, 2, 3},
			wantValues: []int{1, 2, 3},
			wantErrs:   []error{nil, nil, nil},
		},
		{
			name:       "DropOldest evicts the oldest on the overflowing enqueue",
			policy:     DropOldest,
			enqueue:    []int{1, 2, 3, 4},
			wantValues: []int{2, 3, 4},
			wantErrs:   []error{nil, nil, nil, nil},
		},
		{
			name:       "DropOldest keeps evicting across repeated overflow",
			policy:     DropOldest,
			enqueue:    []int{1, 2, 3, 4, 5},
			wantValues: []int{3, 4, 5},
			wantErrs:   []error{nil, nil, nil, nil, nil},
		},
		{
			name:       "RejectNew fills exactly to capacity with no rejection",
			policy:     RejectNew,
			enqueue:    []int{1, 2, 3},
			wantValues: []int{1, 2, 3},
			wantErrs:   []error{nil, nil, nil},
		},
		{
			name:       "RejectNew rejects the overflowing enqueue and keeps contents",
			policy:     RejectNew,
			enqueue:    []int{1, 2, 3, 4},
			wantValues: []int{1, 2, 3},
			wantErrs:   []error{nil, nil, nil, ErrFull},
		},
		{
			name:       "RejectNew keeps rejecting once full",
			policy:     RejectNew,
			enqueue:    []int{1, 2, 3, 4, 5},
			wantValues: []int{1, 2, 3},
			wantErrs:   []error{nil, nil, nil, ErrFull, ErrFull},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q, err := New(3, tc.policy)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for i, v := range tc.enqueue {
				err := q.Enqueue(v)
				if !errors.Is(err, tc.wantErrs[i]) {
					t.Fatalf("enqueue(%d) err = %v, want %v", v, err, tc.wantErrs[i])
				}
			}
			if got := q.Values(); !slices.Equal(got, tc.wantValues) {
				t.Errorf("Values() = %v, want %v", got, tc.wantValues)
			}
		})
	}
}

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1} {
		if _, err := New(capacity, DropOldest); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d, ...) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestCapacityNeverGrows(t *testing.T) {
	t.Parallel()

	q, err := New(4, DropOldest)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 500; i++ {
		if err := q.Enqueue(i); err != nil {
			t.Fatalf("enqueue(%d) unexpected error: %v", i, err)
		}
		if q.Cap() != 4 {
			t.Fatalf("after %d enqueues: cap = %d, want 4", i+1, q.Cap())
		}
	}
	if want := []int{496, 497, 498, 499}; !slices.Equal(q.Values(), want) {
		t.Errorf("Values() = %v, want %v", q.Values(), want)
	}
}

func TestRejectNewCapacityNeverGrows(t *testing.T) {
	t.Parallel()

	q, err := New(4, RejectNew)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 500; i++ {
		_ = q.Enqueue(i)
		if q.Cap() != 4 {
			t.Fatalf("after %d enqueues: cap = %d, want 4", i+1, q.Cap())
		}
	}
	if want := []int{0, 1, 2, 3}; !slices.Equal(q.Values(), want) {
		t.Errorf("Values() = %v, want %v", q.Values(), want)
	}
}

func TestValuesReturnsACopy(t *testing.T) {
	t.Parallel()

	q, err := New(3, DropOldest)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = q.Enqueue(1)
	_ = q.Enqueue(2)

	got := q.Values()
	got[0] = 999

	if q.Values()[0] == 999 {
		t.Fatal("mutating the slice returned by Values mutated the queue's internal buffer")
	}
}

// enqueueNaiveDropOldest is the version of DropOldest overflow that looks
// like it reuses the array: reslice off the oldest element, then append the
// new one onto that reslice. It is unexported and lives only here, never in
// boundedqueue.go, because it does not actually enforce the bound. Once the
// queue is full, buf[1:] has zero spare capacity of its own -- the original
// array had none left either -- so this append periodically reallocates
// instead of the zero reallocations Queue.Enqueue achieves by shifting the
// existing array in place.
func enqueueNaiveDropOldest(buf []int, v int) []int {
	return append(buf[1:], v)
}

// TestNaiveDropOldestReallocatesWhileBoundedQueueDoesNot is the heart of the
// module. Once a Queue is full, every further Enqueue call shifts the
// existing backing array in place and allocates nothing. The naive
// reslice-and-append idiom, driven from the same full state, reallocates
// periodically, because reslicing off the front returns none of the spare
// capacity append needs.
//
// This measures runtime.MemStats.Mallocs directly rather than reaching for
// testing.AllocsPerRun. The naive helper only reallocates on roughly one
// call in three (verified above by hand); AllocsPerRun computes its average
// with integer division of the total by the run count, so any true average
// below 1 truncates to the same 0 the bounded queue reports, silently
// hiding a real, periodic allocation. A raw Mallocs delta has no such
// rounding and still lets the test assert a property -- naive > bounded --
// rather than a specific count.
//
// This test does not call t.Parallel, for the same reason AllocsPerRun
// forbids it: Mallocs is a process-wide counter, and a concurrently running
// test allocating in the background would add noise to the delta.
func TestNaiveDropOldestReallocatesWhileBoundedQueueDoesNot(t *testing.T) {
	const capacity = 4
	const iterations = 300

	q, err := New(capacity, DropOldest)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < capacity; i++ {
		_ = q.Enqueue(i)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < iterations; i++ {
		_ = q.Enqueue(i)
	}
	runtime.ReadMemStats(&after)
	boundedMallocs := after.Mallocs - before.Mallocs

	naive := make([]int, capacity)
	for i := range naive {
		naive[i] = i
	}
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < iterations; i++ {
		naive = enqueueNaiveDropOldest(naive, capacity+i)
	}
	runtime.ReadMemStats(&after)
	naiveMallocs := after.Mallocs - before.Mallocs

	if boundedMallocs != 0 {
		t.Fatalf("Queue.Enqueue on a full queue did %d allocations over %d calls, want exactly 0", boundedMallocs, iterations)
	}
	if !(naiveMallocs > boundedMallocs) {
		t.Fatalf("allocations over %d calls: naive = %d, bounded = %d; want naive > bounded", iterations, naiveMallocs, boundedMallocs)
	}
}

// ExampleQueue_Enqueue is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleQueue_Enqueue() {
	drop, err := New(3, DropOldest)
	if err != nil {
		panic(err)
	}
	for _, v := range []int{1, 2, 3, 4, 5} {
		_ = drop.Enqueue(v)
		fmt.Printf("DropOldest enqueue(%d): values=%v cap=%d\n", v, drop.Values(), drop.Cap())
	}

	reject, err := New(3, RejectNew)
	if err != nil {
		panic(err)
	}
	for _, v := range []int{1, 2, 3, 4, 5} {
		err := reject.Enqueue(v)
		fmt.Printf("RejectNew enqueue(%d): values=%v cap=%d err=%v\n", v, reject.Values(), reject.Cap(), err)
	}

	// Output:
	// DropOldest enqueue(1): values=[1] cap=3
	// DropOldest enqueue(2): values=[1 2] cap=3
	// DropOldest enqueue(3): values=[1 2 3] cap=3
	// DropOldest enqueue(4): values=[2 3 4] cap=3
	// DropOldest enqueue(5): values=[3 4 5] cap=3
	// RejectNew enqueue(1): values=[1] cap=3 err=<nil>
	// RejectNew enqueue(2): values=[1 2] cap=3 err=<nil>
	// RejectNew enqueue(3): values=[1 2 3] cap=3 err=<nil>
	// RejectNew enqueue(4): values=[1 2 3] cap=3 err=boundedqueue: queue is full: capacity 3
	// RejectNew enqueue(5): values=[1 2 3] cap=3 err=boundedqueue: queue is full: capacity 3
}
```

## Review

Both policies are correct when `Cap()` never moves away from the value
passed to `New`, regardless of how many enqueues follow — the two capacity
tests drive that home with 500 calls each, far past what a table test alone
would catch, because a bug that appends past capacity only once every few
calls can hide in a short table but not across 500 iterations. `New` rejects
a non-positive capacity with `ErrInvalidCapacity`, checkable with
`errors.Is`, and `RejectNew`'s `ErrFull` is wrapped with the queue's capacity
for context. `DropOldest`'s sliding-window behavior and `RejectNew`'s
back-pressure behavior are both pinned by `TestEnqueueAtBoundary`'s table at
the boundary itself. The naive reslice-and-append idiom is proven strictly
worse than `Queue.Enqueue`, not merely "different," by a direct
`runtime.MemStats` comparison rather than a fragile `testing.AllocsPerRun`
average that a periodic allocation can hide. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [`copy` builtin — the Go spec](https://go.dev/ref/spec#Appending_and_copying_slices) — copy is defined to work correctly on overlapping slices, which is what the DropOldest shift relies on.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — used throughout the tests to compare queue contents.
- [`runtime.MemStats`](https://pkg.go.dev/runtime#MemStats) — the `Mallocs` counter used to detect the naive helper's periodic reallocation.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — why letting append decide capacity defeats a fixed bound.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-chunked-upload-three-index-split.md](15-chunked-upload-three-index-split.md) | Next: [17-flat-arena-backed-matrix-rows.md](17-flat-arena-backed-matrix-rows.md)
