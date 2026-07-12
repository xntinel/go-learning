# Exercise 9: Ordering Query Results: Implementing sort.Interface with a Mutating Swap

Sorting query results by a composite key is a daily task, and `sort.Interface` is
where receiver semantics meet a mutating `Swap`. This module implements a
`byPriority` slice type ordering tasks by `(priority, createdAt)`, explains why a
value-receiver `Swap` on a slice type still mutates the shared backing array, and
checks equivalence with `sort.SliceStable`.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
tasksort/                   independent module: example.com/tasksort
  go.mod                    go 1.25
  sort.go                   type Task; byPriority (Len, Less, Swap); SortTasks
  cmd/
    demo/
      main.go               sort a batch, print order
  sort_test.go              deterministic order, tie-break, stability, SliceStable parity
```

- Files: `sort.go`, `cmd/demo/main.go`, `sort_test.go`.
- Implement: a `Task` (priority, createdAt, id) and a `byPriority []Task` implementing `sort.Interface`, plus a `SortTasks` helper using `sort.Stable`.
- Test: assert deterministic order including the `createdAt` tie-break; compare `sort.Sort(byPriority(xs))` against `sort.SliceStable`; assert stability for fully equal keys.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why a value receiver on a slice type still sorts in place

`sort.Sort` takes a `sort.Interface` — a value with `Len`, `Less`, and `Swap`. It
calls `Swap(i, j)` many times, and each `Swap` must permanently exchange two
elements of the data being sorted. That sounds like it needs a pointer receiver,
but for a *slice* type it does not, and the reason is worth understanding.

A slice value is a small header — a pointer to a backing array, a length, and a
capacity. Passing `byPriority(xs)` to `sort.Sort` copies the *header*, but the
header still points at the *same backing array*. So a value-receiver
`Swap(i, j)` that writes `b[i], b[j] = b[j], b[i]` mutates the shared backing
array through the copied header, and the change is visible to the original slice.
The value receiver works precisely because the mutation target lives behind the
pointer inside the slice header, not in the header itself. (Contrast a wrapper
*struct* holding a slice by value passed around by copy: a value receiver there can
still reach the backing array, but if `Swap` tried to reslice or replace the slice
field, that change would be lost — which is the failure mode the concepts file
warns about.)

`sort.Sort` is not stable; `sort.Stable` and `sort.SliceStable` are. Here `Less`
compares `priority` and breaks ties on `createdAt`, so the order is fully
determined except when two tasks share both — and that residual tie is where
stability (preserving input order) matters. `SortTasks` uses `sort.Stable` so equal
keys keep their arrival order, which is the behavior a job queue usually wants.

Create `sort.go`:

```go
// sort.go
package tasksort

import "sort"

// Task is a unit of work ordered by priority (lower runs first), then by
// createdAt (earlier first), with id as an identity for stability checks.
type Task struct {
	ID        string
	Priority  int
	CreatedAt int64 // unix seconds
}

// byPriority orders tasks by (priority, createdAt). It is a slice type, so a
// value receiver on Swap still mutates the shared backing array.
type byPriority []Task

func (b byPriority) Len() int { return len(b) }

func (b byPriority) Less(i, j int) bool {
	if b[i].Priority != b[j].Priority {
		return b[i].Priority < b[j].Priority
	}
	return b[i].CreatedAt < b[j].CreatedAt
}

func (b byPriority) Swap(i, j int) { b[i], b[j] = b[j], b[i] }

// Compile-time contract: byPriority satisfies sort.Interface.
var _ sort.Interface = byPriority(nil)

// SortTasks stably sorts tasks in place by (priority, createdAt). Stability keeps
// tasks with identical keys in their original arrival order.
func SortTasks(tasks []Task) {
	sort.Stable(byPriority(tasks))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/tasksort"
)

func main() {
	tasks := []tasksort.Task{
		{ID: "a", Priority: 2, CreatedAt: 100},
		{ID: "b", Priority: 1, CreatedAt: 200},
		{ID: "c", Priority: 1, CreatedAt: 150},
		{ID: "d", Priority: 3, CreatedAt: 50},
	}

	tasksort.SortTasks(tasks)

	for _, t := range tasks {
		fmt.Printf("%s p=%d t=%d\n", t.ID, t.Priority, t.CreatedAt)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
c p=1 t=150
b p=1 t=200
a p=2 t=100
d p=3 t=50
```

### Tests

`TestSortDeterministic` asserts the exact order including the `createdAt`
tie-break. `TestParityWithSliceStable` sorts an independent copy with
`sort.SliceStable` using the same key and asserts the two results match — proving
the `sort.Interface` implementation agrees with the closure form.
`TestStability` gives several tasks fully equal keys and asserts their input order
is preserved.

Create `sort_test.go`:

```go
// sort_test.go
package tasksort

import (
	"sort"
	"testing"
)

func ids(tasks []Task) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSortDeterministic(t *testing.T) {
	t.Parallel()

	tasks := []Task{
		{ID: "a", Priority: 2, CreatedAt: 100},
		{ID: "b", Priority: 1, CreatedAt: 200},
		{ID: "c", Priority: 1, CreatedAt: 150},
		{ID: "d", Priority: 3, CreatedAt: 50},
	}
	SortTasks(tasks)

	want := []string{"c", "b", "a", "d"}
	if got := ids(tasks); !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestParityWithSliceStable(t *testing.T) {
	t.Parallel()

	base := []Task{
		{ID: "a", Priority: 2, CreatedAt: 100},
		{ID: "b", Priority: 1, CreatedAt: 200},
		{ID: "c", Priority: 1, CreatedAt: 150},
		{ID: "d", Priority: 3, CreatedAt: 50},
		{ID: "e", Priority: 2, CreatedAt: 90},
	}

	viaInterface := append([]Task(nil), base...)
	sort.Sort(byPriority(viaInterface))

	viaClosure := append([]Task(nil), base...)
	sort.SliceStable(viaClosure, func(i, j int) bool {
		if viaClosure[i].Priority != viaClosure[j].Priority {
			return viaClosure[i].Priority < viaClosure[j].Priority
		}
		return viaClosure[i].CreatedAt < viaClosure[j].CreatedAt
	})

	if got, want := ids(viaInterface), ids(viaClosure); !equal(got, want) {
		t.Fatalf("sort.Sort order %v != sort.SliceStable order %v", got, want)
	}
}

func TestStability(t *testing.T) {
	t.Parallel()

	// All four share the same (priority, createdAt): stable sort keeps input order.
	tasks := []Task{
		{ID: "w", Priority: 1, CreatedAt: 10},
		{ID: "x", Priority: 1, CreatedAt: 10},
		{ID: "y", Priority: 1, CreatedAt: 10},
		{ID: "z", Priority: 1, CreatedAt: 10},
	}
	SortTasks(tasks)

	want := []string{"w", "x", "y", "z"}
	if got := ids(tasks); !equal(got, want) {
		t.Fatalf("stable order = %v, want input order %v", got, want)
	}
}
```

## Review

The sorter is correct when the order is a total function of `(priority,
createdAt)` with ties broken by input order, which `TestSortDeterministic` and
`TestStability` pin together. The receiver point is subtle but load-bearing:
`Swap` has a value receiver and still sorts in place because `byPriority` is a
slice, whose header — copied by value into `sort.Sort` — still points at one shared
backing array. `TestParityWithSliceStable` cross-checks the hand-written
`sort.Interface` against the closure-based `sort.SliceStable`; when they agree, the
`Less`/`Swap` pair is consistent. Reach for `sort.Interface` when the ordering is a
reusable named type; reach for `sort.SliceStable` for a one-off comparator.

## Resources

- [sort.Interface](https://pkg.go.dev/sort#Interface) — `Len`, `Less`, `Swap` and how `Sort` drives them.
- [sort.Stable](https://pkg.go.dev/sort#Stable) — stable ordering that preserves input order for equal keys.
- [sort.SliceStable](https://pkg.go.dev/sort#SliceStable) — the closure form used for the parity check.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-stringer-addressability-in-collections.md](08-stringer-addressability-in-collections.md) | Next: [10-io-writer-pointer-receiver-metering.md](10-io-writer-pointer-receiver-metering.md)
