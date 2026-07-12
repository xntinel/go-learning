# Exercise 7: Map/Filter/Reduce over Domain Records

Row-to-DTO transforms are the daily work of a service layer: take `[]UserRow` from
the database, drop the soft-deleted ones, project to `[]UserDTO` for the API, and
reduce to an aggregate. This exercise builds the generic combinators and, just as
importantly, shows when a plain loop or a stdlib helper is the better call.

## What you'll build

```text
collops/                     independent module: example.com/collops
  go.mod                     go 1.25
  collops.go                 Map, Filter, Reduce; UserRow -> UserDTO transforms
  collops_test.go            length/order, filter drops soft-deleted, reduce aggregate, no-mutation
  cmd/demo/
    main.go                  runs the row->dto->aggregate pipeline
```

- Files: `collops.go`, `collops_test.go`, `cmd/demo/main.go`.
- Implement: `Map[T,U]`, `Filter[T]`, `Reduce[T,U]`, plus `toDTO` and a soft-delete predicate over `UserRow`.
- Test: `Map` preserves length and order and does not mutate the source; `Filter` drops exactly the soft-deleted rows preserving order; `Reduce` computes a known aggregate; each handles nil/empty input; compare `Filter` against `slices.DeleteFunc`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The three combinators, and when not to use them

`Map[T,U](s []T, f func(T) U) []U` applies `f` to each element and returns a new
slice of the same length in the same order. `Filter[T](s []T, keep func(T) bool)
[]T` returns a new slice containing only the elements for which `keep` is true, in
order. `Reduce[T,U](s []T, init U, f func(U,T) U) U` folds the slice into a single
accumulator. All three take behavior as a first-class function — the strategy shape.

The non-negotiable rule is that these helpers do not mutate the source. `Map`
allocates a fresh result slice; `Filter` appends to a new slice rather than sliding
elements within the input's backing array. A `Filter` that reused the caller's
backing array (as `slices.DeleteFunc` does in place) would surprise a caller who
still holds the original slice. The no-mutation test pins this.

Now the honesty the concepts demanded: these combinators are a tool, not a
lifestyle. A single `for range` loop that does the filter and the projection at once
is often clearer and allocates one slice instead of two intermediates. And the
standard library already ships `slices.DeleteFunc` (in-place removal),
`slices.IndexFunc`, and `slices.Collect` (materialize an `iter.Seq`). Reach for
`Map`/`Filter`/`Reduce` when chaining them removes a real branch and reads better;
reach for a loop when it does not. The test deliberately shows the `slices.DeleteFunc`
equivalent of the filter so the trade-off is concrete.

Create `collops.go`:

```go
package collops

// Map applies f to every element and returns a new slice of the same length and
// order. It never mutates s.
func Map[T, U any](s []T, f func(T) U) []U {
	out := make([]U, len(s))
	for i, v := range s {
		out[i] = f(v)
	}
	return out
}

// Filter returns a new slice of the elements for which keep is true, in order.
// It never mutates s.
func Filter[T any](s []T, keep func(T) bool) []T {
	out := make([]T, 0, len(s))
	for _, v := range s {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// Reduce folds s into an accumulator starting from init.
func Reduce[T, U any](s []T, init U, f func(U, T) U) U {
	acc := init
	for _, v := range s {
		acc = f(acc, v)
	}
	return acc
}

// UserRow is a database row.
type UserRow struct {
	ID        int64
	Name      string
	Balance   int64 // cents
	DeletedAt int64 // unix seconds; 0 means live
}

// UserDTO is the API projection of a live user.
type UserDTO struct {
	ID   int64
	Name string
}

// live reports whether a row has not been soft-deleted.
func live(r UserRow) bool { return r.DeletedAt == 0 }

// toDTO projects a row to its API DTO.
func toDTO(r UserRow) UserDTO { return UserDTO{ID: r.ID, Name: r.Name} }
```

### The runnable demo

The demo runs the real pipeline: filter out soft-deleted rows, map the survivors to
DTOs, and reduce the live rows to a total balance.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/collops"
)

func main() {
	rows := []collops.UserRow{
		{ID: 1, Name: "alice", Balance: 1500, DeletedAt: 0},
		{ID: 2, Name: "bob", Balance: 900, DeletedAt: 1710000000}, // soft-deleted
		{ID: 3, Name: "carol", Balance: 2500, DeletedAt: 0},
	}

	liveRows := collops.Filter(rows, collops.Live)
	dtos := collops.Map(liveRows, collops.ToDTO)
	total := collops.Reduce(liveRows, int64(0), func(sum int64, r collops.UserRow) int64 {
		return sum + r.Balance
	})

	fmt.Printf("live users: %d\n", len(dtos))
	for _, d := range dtos {
		fmt.Printf("  %d %s\n", d.ID, d.Name)
	}
	fmt.Printf("total live balance: %d\n", total)
}
```

The demo needs exported helpers, so add thin exported wrappers next to the
unexported ones. Append to `collops.go`:

```go
// Live reports whether a row is not soft-deleted (exported for cmd/demo).
func Live(r UserRow) bool { return live(r) }

// ToDTO projects a row to its DTO (exported for cmd/demo).
func ToDTO(r UserRow) UserDTO { return toDTO(r) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
live users: 2
  1 alice
  3 carol
total live balance: 4000
```

Bob is soft-deleted, so `Filter` drops him; the two survivors project to DTOs and
their balances (1500 + 2500) reduce to 4000.

### Tests

The tests cover the contract: `Map` preserves length and order and leaves the
source untouched, `Filter` drops exactly the soft-deleted rows, `Reduce` computes
the known total, and each handles a nil slice without panicking. One test computes
the same filter with `slices.DeleteFunc` to show the stdlib equivalent — and to
highlight that `DeleteFunc` works in place while `Filter` returns a fresh slice.

Create `collops_test.go`:

```go
package collops

import (
	"slices"
	"testing"
)

func fixture() []UserRow {
	return []UserRow{
		{ID: 1, Name: "alice", Balance: 1500, DeletedAt: 0},
		{ID: 2, Name: "bob", Balance: 900, DeletedAt: 1710000000},
		{ID: 3, Name: "carol", Balance: 2500, DeletedAt: 0},
	}
}

func TestMapPreservesLengthAndOrder(t *testing.T) {
	t.Parallel()

	rows := fixture()
	dtos := Map(rows, toDTO)
	if len(dtos) != len(rows) {
		t.Fatalf("len = %d, want %d", len(dtos), len(rows))
	}
	for i := range rows {
		if dtos[i].ID != rows[i].ID || dtos[i].Name != rows[i].Name {
			t.Fatalf("dto[%d] = %+v, does not match row", i, dtos[i])
		}
	}
}

func TestMapDoesNotMutateSource(t *testing.T) {
	t.Parallel()

	rows := fixture()
	before := slices.Clone(rows)
	_ = Map(rows, func(r UserRow) int64 { return r.Balance * 2 })
	if !slices.Equal(rows, before) {
		t.Fatal("Map mutated its source slice")
	}
}

func TestFilterDropsSoftDeleted(t *testing.T) {
	t.Parallel()

	live := Filter(fixture(), live)
	if len(live) != 2 {
		t.Fatalf("live count = %d, want 2", len(live))
	}
	if live[0].ID != 1 || live[1].ID != 3 {
		t.Fatalf("live IDs = %d,%d, want 1,3 (order preserved)", live[0].ID, live[1].ID)
	}
}

func TestFilterMatchesSlicesDeleteFunc(t *testing.T) {
	t.Parallel()

	viaFilter := Filter(fixture(), live)

	// slices.DeleteFunc removes elements in place; delete the NOT-live ones.
	viaDelete := slices.DeleteFunc(fixture(), func(r UserRow) bool { return !live(r) })

	if !slices.Equal(viaFilter, viaDelete) {
		t.Fatalf("Filter = %v, DeleteFunc = %v", viaFilter, viaDelete)
	}
}

func TestReduceComputesAggregate(t *testing.T) {
	t.Parallel()

	live := Filter(fixture(), live)
	total := Reduce(live, int64(0), func(sum int64, r UserRow) int64 { return sum + r.Balance })
	if total != 4000 {
		t.Fatalf("total = %d, want 4000", total)
	}
}

func TestEmptyAndNilInputs(t *testing.T) {
	t.Parallel()

	if got := Map[UserRow, int64](nil, func(r UserRow) int64 { return r.Balance }); len(got) != 0 {
		t.Fatalf("Map(nil) len = %d, want 0", len(got))
	}
	if got := Filter[UserRow](nil, live); len(got) != 0 {
		t.Fatalf("Filter(nil) len = %d, want 0", len(got))
	}
	if got := Reduce[UserRow](nil, int64(7), func(a int64, r UserRow) int64 { return a + r.Balance }); got != 7 {
		t.Fatalf("Reduce(nil) = %d, want 7 (init returned)", got)
	}
}
```

## Review

The combinators are correct when `Map` preserves length and order, `Filter` keeps
exactly the elements the predicate accepts in order, and `Reduce` folds to the known
aggregate — with nil input yielding an empty result (or the init, for `Reduce`) and
no panic. The one rule you must not break is source immutability: `Map` and `Filter`
allocate a fresh slice; they never write into the caller's backing array, which is
the difference from `slices.DeleteFunc`. And keep perspective — the
`slices.DeleteFunc` comparison test exists to remind you the stdlib already covers
the in-place case, and a plain `for range` that filters and projects in one pass is
often clearer than chaining two combinators. Reach for the combinator when it
removes a real branch, not to look functional.

## Resources

- [slices package](https://pkg.go.dev/slices) — `DeleteFunc`, `IndexFunc`, `Collect`, `Clone`, `Equal`.
- [iter package](https://pkg.go.dev/iter) — `Seq` and the range-over-func iterators `Collect` consumes.
- [Go blog: range over function types](https://go.dev/blog/range-functions) — when the iterator form beats an eager slice.

---

Back to [06-lazy-init-oncevalue.md](06-lazy-init-oncevalue.md) | Next: [08-command-dispatcher-registry.md](08-command-dispatcher-registry.md)
