# Exercise 20: Filter-Map Chain with Transformation Error Accumulation

**Nivel: Intermedio** — validacion rapida (un test corto).

Processing a batch of records usually means filtering out the ones that
don't apply and transforming the rest — and a transform can fail per item
without the whole batch being worthless. `FilterMap` chains a `Predicate`
and a `Transform` over a slice, skipping filtered items entirely and
collecting every transform failure with `errors.Join` instead of aborting
on the first one.

## What you'll build

```text
pipeline/                    independent module: example.com/pipeline
  go.mod                     go 1.24
  pipeline.go                type Predicate[T], Transform[T, R]; func FilterMap
  pipeline_test.go           all-pass, filtered-out items, accumulated errors, empty result
```

- Files: `pipeline.go`, `pipeline_test.go`.
- Implement: `Predicate[T any] func(T) bool`, `Transform[T, R any] func(T) (R, error)`, and `FilterMap[T, R any](items []T, filter Predicate[T], transform Transform[T, R]) ([]R, error)`.
- Test: items the filter rejects never reach `transform`; a transform failure is excluded from the result but its error survives in the joined error, recoverable with `errors.Is`; a batch where every item is filtered out returns an empty result and a nil error, not an empty-slice error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/20-filter-and-map-with-transform-error
cd go-solutions/04-functions/10-higher-order-functions/20-filter-and-map-with-transform-error
go mod edit -go=1.24
```

### Two independent seams, one loop

`FilterMap` takes two higher-order parameters with two different
failure shapes: `filter` cannot fail — it can only say yes or no — while
`transform` can fail per item. Keeping them as separate function types
rather than folding filtering into the transform (say, by having transform
return `(R, bool, error)`) keeps each seam doing exactly one job:
`Predicate[T]` answers "does this item belong in the batch at all," and
`Transform[T, R]` answers "what does this item become, and did that
succeed."

The loop processes items in order, and for each one: skip immediately if
`filter` rejects it — `transform` is never called for a filtered-out item,
which matters if `transform` does expensive or side-effecting work.
Otherwise call `transform`; on success append to `results`, on failure
append the error to `errs` and move on to the next item rather than
returning immediately. Continuing past a single transform failure is what
lets one bad record in a batch of a thousand not block processing of the
other 999.

`errors.Join(errs...)` at the end does two things: if `errs` is empty (no
transform failures, whether because everything passed or because
everything was filtered out before reaching transform), it returns `nil`
— a batch with zero failures must report success, not an empty non-nil
error. If `errs` has one or more entries, it wraps them all into a single
error that `errors.Is` can still unwrap to find any individual cause,
because each per-item error from `transform` typically wraps a sentinel
via `%w`.

Create `pipeline.go`:

```go
package pipeline

import "errors"

// Predicate reports whether v should continue through the pipeline.
type Predicate[T any] func(T) bool

// Transform maps a T to an R, or fails for that specific item.
type Transform[T, R any] func(T) (R, error)

// FilterMap applies filter then transform to every item in items in
// order: items filter rejects are skipped entirely (transform never sees
// them), and items transform fails on are skipped from the result but
// their error is kept. Instead of stopping at the first transform
// failure, every failure is accumulated with errors.Join, so a caller can
// recover any individual cause with errors.Is/As after processing the
// whole input.
func FilterMap[T, R any](items []T, filter Predicate[T], transform Transform[T, R]) ([]R, error) {
	var results []R
	var errs []error

	for _, item := range items {
		if !filter(item) {
			continue
		}
		r, err := transform(item)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		results = append(results, r)
	}

	return results, errors.Join(errs...)
}
```

### Tests

`TestFilterMapAllPass` and `TestFilterMapSkipsFilteredItems` cover the
happy paths: everything succeeds, and the filter's rejections shrink the
result as expected. `TestFilterMapAccumulatesTransformErrors` proves the
core contract — two failing items among four still leave the two
successful results in the output, and both failure causes are recoverable
from the single joined error via `errors.Is`. `TestFilterMapAllFilteredOutReturnsNoError`
guards the empty-result edge: no candidates surviving the filter must
report a nil error, not a spurious one. `TestFilterMapTransformNeverSeesFilteredItems`
proves the ordering contract directly by recording every value `transform`
was actually called with.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"errors"
	"fmt"
	"testing"
)

var errNegative = errors.New("negative value")

func parsePositive(n int) (int, error) {
	if n < 0 {
		return 0, fmt.Errorf("item %d: %w", n, errNegative)
	}
	return n * 10, nil
}

func isEven(n int) bool { return n%2 == 0 }

func TestFilterMapAllPass(t *testing.T) {
	t.Parallel()

	got, err := FilterMap([]int{2, 4, 6}, isEven, parsePositive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{20, 40, 60}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFilterMapSkipsFilteredItems(t *testing.T) {
	t.Parallel()

	got, err := FilterMap([]int{1, 2, 3, 4, 5}, isEven, parsePositive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{20, 40} // only 2 and 4 pass the even filter
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFilterMapAccumulatesTransformErrors(t *testing.T) {
	t.Parallel()

	got, err := FilterMap([]int{2, -4, 6, -8}, isEven, parsePositive)
	if err == nil {
		t.Fatal("expected an accumulated error for the two negative items")
	}
	if !errors.Is(err, errNegative) {
		t.Fatal("joined error does not wrap errNegative")
	}

	want := []int{20, 60} // -4 and -8 fail transform and are excluded
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFilterMapAllFilteredOutReturnsNoError(t *testing.T) {
	t.Parallel()

	got, err := FilterMap([]int{1, 3, 5}, isEven, parsePositive)
	if err != nil {
		t.Fatalf("unexpected error when nothing passes the filter: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestFilterMapTransformNeverSeesFilteredItems(t *testing.T) {
	t.Parallel()

	var seen []int
	transform := func(n int) (int, error) {
		seen = append(seen, n)
		return n, nil
	}

	_, err := FilterMap([]int{1, 2, 3, 4}, isEven, transform)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []int{2, 4}
	if len(seen) != len(want) {
		t.Fatalf("transform saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("transform saw %v, want %v", seen, want)
		}
	}
}
```

## Review

`FilterMap` is correct when a transform failure removes exactly that item
from the result while leaving every other item's outcome untouched — the
`continue` after recording the error, rather than an early `return`, is
the whole mechanism. Keeping `errors.Join` at the very end rather than
returning on the first failure is what turns "one bad record breaks the
whole batch" into "one bad record is reported, the other 999 still
process." The two type parameters, `T` and `R`, let the same `FilterMap`
validate-and-parse raw strings into structured records, or filter-and-convert
domain values into DTOs, without the filter and transform types ever
needing to agree on more than the shape of their input and output.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining multiple per-item failures into one inspectable error.
- [Go 1.20 Release Notes: Wrapping multiple errors](https://go.dev/doc/go1.20#errors) — the language change that made `errors.Join` and multi-error `%w` possible.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-broadcast-tee-multiple-subscribers.md](19-broadcast-tee-multiple-subscribers.md) | Next: [21-permission-checker-with-inheritance.md](21-permission-checker-with-inheritance.md)
