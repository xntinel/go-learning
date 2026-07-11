# Exercise 22: Reduce Operation with Early Termination Predicate

**Nivel: Intermedio** — validacion rapida (un test corto).

Summing every line item to check whether an order exceeds a fraud
threshold doesn't need to look at every line item once the threshold is
already crossed. `FoldUntil` is a fold — the general reduce operation
behind `sum`, `count`, and `join` — with one addition: a stop predicate
that ends the loop early once the accumulator satisfies some condition,
so items after that point are never touched.

## What you'll build

```text
reduce/                      independent module: example.com/reduce
  go.mod                     go 1.24
  reduce.go                  type StopFunc[A]; func FoldUntil
  reduce_test.go             early stop, never-stop, stop-immediately, empty input, struct accumulator
```

- Files: `reduce.go`, `reduce_test.go`.
- Implement: `StopFunc[A any] func(acc A) bool` and `FoldUntil[T, A any](items []T, init A, combine func(A, T) A, stop StopFunc[A]) A`.
- Test: folding stops the moment `stop` reports true and later items are never passed to `combine`; a `stop` that never returns true processes every item; a `stop` that is true immediately after the first item stops after exactly one combine; an empty slice returns `init` unchanged; the accumulator can be a struct, not just a scalar.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reduce
cd ~/go-exercises/reduce
go mod init example.com/reduce
go mod edit -go=1.24
```

### Stop is checked after combining, not before

`FoldUntil` has the same shape as any fold: an accumulator seeded with
`init`, a loop over `items`, and `combine` applied one item at a time. The
one addition is `stop`, checked immediately after each `combine` call, on
the *new* accumulator value that call just produced. Checking after,
rather than before, is what makes "stop once the running total reaches
50" mean "process the item that pushes the total to 50, then stop" rather
than "stop one item too early because the check ran on the stale total."
The moment `stop` returns `true`, the loop `break`s — items after that
point are never passed to `combine` at all, which is the entire value of
"early termination" over folding the whole slice and checking the final
result afterward: for a slice of a million items where the threshold is
hit at item three, `FoldUntil` does three units of work, not a million.

`StopFunc[A]` is generic over the accumulator type, not the item type,
because the decision to stop only ever depends on where the fold has
gotten to — a running sum, a running count, a small struct tracking
several fields at once — never on the raw items being folded. That is
also why `combine`'s signature, `func(A, T) A`, takes the accumulator
first: it mirrors the order every fold in the standard idioms (and
`slices.Reduce`-shaped helpers) already use.

Create `reduce.go`:

```go
package reduce

// StopFunc reports whether folding should stop given the accumulator
// produced so far.
type StopFunc[A any] func(acc A) bool

// FoldUntil folds items into an accumulator, starting from init and
// combining one item at a time with combine, but stops — without
// touching any remaining items — as soon as stop reports true for the
// accumulator produced after incorporating the most recent item.
func FoldUntil[T, A any](items []T, init A, combine func(A, T) A, stop StopFunc[A]) A {
	acc := init
	for _, item := range items {
		acc = combine(acc, item)
		if stop(acc) {
			break
		}
	}
	return acc
}
```

### Tests

`TestFoldUntilStopsWhenThresholdCrossed` is the central test: it counts
how many times `combine` actually ran and asserts it stopped at exactly
three, not five, proving items after the threshold crossing are genuinely
untouched rather than just ignored in the result.
`TestFoldUntilNeverStoppingCoversEveryItem` and `TestFoldUntilStopsImmediately`
are the two extremes of the stop predicate — always false, always true.
`TestFoldUntilEmptyInputReturnsInit` guards the zero-items edge.
`TestFoldUntilWithStructAccumulator` proves the generic accumulator type
works for something richer than an int, tracking both a running count and
a running total in one struct.

Create `reduce_test.go`:

```go
package reduce

import "testing"

func sum(acc int, n int) int { return acc + n }

func TestFoldUntilStopsWhenThresholdCrossed(t *testing.T) {
	t.Parallel()

	items := []int{10, 20, 30, 40, 50}
	var touched int
	combine := func(acc, n int) int {
		touched++
		return sum(acc, n)
	}
	stop := func(acc int) bool { return acc >= 50 }

	got := FoldUntil(items, 0, combine, stop)

	// 10 -> 10 (continue), 10+20=30 (continue), 30+30=60 (stop)
	if got != 60 {
		t.Fatalf("FoldUntil() = %d, want 60", got)
	}
	if touched != 3 {
		t.Fatalf("combine was called %d times, want 3 (remaining items untouched)", touched)
	}
}

func TestFoldUntilNeverStoppingCoversEveryItem(t *testing.T) {
	t.Parallel()

	items := []int{1, 2, 3, 4, 5}
	never := func(int) bool { return false }

	got := FoldUntil(items, 0, sum, never)
	if got != 15 {
		t.Fatalf("FoldUntil() = %d, want 15", got)
	}
}

func TestFoldUntilStopsImmediately(t *testing.T) {
	t.Parallel()

	items := []int{100, 200, 300}
	var touched int
	combine := func(acc, n int) int {
		touched++
		return sum(acc, n)
	}
	always := func(int) bool { return true }

	got := FoldUntil(items, 0, combine, always)

	if got != 100 {
		t.Fatalf("FoldUntil() = %d, want 100 (only the first item combined)", got)
	}
	if touched != 1 {
		t.Fatalf("combine was called %d times, want 1", touched)
	}
}

func TestFoldUntilEmptyInputReturnsInit(t *testing.T) {
	t.Parallel()

	got := FoldUntil[int, int](nil, 42, sum, func(int) bool { return true })
	if got != 42 {
		t.Fatalf("FoldUntil() = %d, want 42 (init, unchanged)", got)
	}
}

func TestFoldUntilWithStructAccumulator(t *testing.T) {
	t.Parallel()

	type stats struct {
		count int
		total int
	}

	items := []int{5, 5, 5, 5, 5}
	combine := func(acc stats, n int) stats {
		return stats{count: acc.count + 1, total: acc.total + n}
	}
	stop := func(acc stats) bool { return acc.count == 3 }

	got := FoldUntil(items, stats{}, combine, stop)
	if got.count != 3 || got.total != 15 {
		t.Fatalf("FoldUntil() = %+v, want {count:3 total:15}", got)
	}
}
```

## Review

`FoldUntil` is correct when `stop` is evaluated on the accumulator
*produced by* the most recent `combine` call, and the loop breaks before
any further item is touched — the `touched` counter in the threshold test
is what actually proves items past the stopping point are skipped, since
the final accumulator value alone can't distinguish "stopped early" from
"processed everything and happened to land on the same number." A fold
with `stop` always `false` degrades exactly to a plain fold over the whole
slice, and a `stop` that is `true` immediately after the first item
degrades to "only ever look at the first element" — both are useful
sanity checks that the early-termination logic doesn't change behavior
when it isn't needed. The two independent type parameters mean the same
`FoldUntil` works whether the accumulator is a running `int` total or a
richer struct tracking multiple running values at once.

## Resources

- [Effective Go: Generics](https://go.dev/doc/effective_go) — the general pattern of type-parameterizing over both the item and accumulator types.
- [slices package](https://pkg.go.dev/slices) — the standard library's non-early-terminating collection helpers this exercise's early-stop variant complements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-permission-checker-with-inheritance.md](21-permission-checker-with-inheritance.md) | Next: [23-lazy-iterator-collect-transform.md](23-lazy-iterator-collect-transform.md)
