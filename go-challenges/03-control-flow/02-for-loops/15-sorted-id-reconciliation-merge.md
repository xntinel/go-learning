# Exercise 15: Reconciling Two Sorted ID Lists with a Merge-Join

**Nivel: Intermedio** — validacion rapida (un test corto).

A nightly reconciliation job compares a ledger's transaction IDs against a
payment provider's IDs and reports which ones are missing on each side. With
both lists already sorted, the efficient way to do that is a single
condition-only pass that walks both at once — no map, no second pass, no
`O(n*m)` nested search. This module builds `Diff` around that merge-join.

This module is fully self-contained: its own `go mod init` and one test file.

## What you'll build

```text
reconcile/                    module example.com/reconcile
  go.mod                      go 1.24
  reconcile.go                 Result; Diff(a, b []string) Result
  reconcile_test.go             interleaved, disjoint, identical, empty sides, uneven tails
```

- Files: `reconcile.go`, `reconcile_test.go`.
- Implement: `Diff(a, b []string) Result` — a `for i < len(a) && j < len(b)` loop with a three-way `switch` on `a[i]` vs `b[j]` that advances whichever pointer is behind (or both, on a match), followed by two short trailing loops that drain whichever slice the merge did not finish.
- Test: interleaved IDs with all three outcomes present, fully disjoint lists, identical lists, either side empty, both empty, and a short `a` with a long unmatched tail in `b`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reconcile
cd ~/go-exercises/reconcile
go mod init example.com/reconcile
go mod edit -go=1.24
```

### Why one condition-only loop plus two drains beats a map

Both inputs are already sorted and duplicate-free, so a map-based set
difference would throw that ordering away for no benefit — it would cost an
extra allocation and a hash per element to answer a question a single pass
already answers in order. The loop condition `i < len(a) && j < len(b)`
stops the instant *either* pointer reaches the end, because past that point
there is nothing left to compare against — whatever remains in the longer
slice cannot possibly match anything, it can only be "only in that slice."
That is exactly what the two trailing `for` loops express: drain whatever the
merge did not reach, unconditionally, straight into the right result bucket.
Forgetting either trailing loop is the classic merge-join bug — it silently
drops every ID after the shorter slice runs out.

Create `reconcile.go`:

```go
package reconcile

// Result is the three-way split of a reconciliation between two sorted,
// duplicate-free ID slices.
type Result struct {
	OnlyInA []string
	OnlyInB []string
	InBoth  []string
}

// Diff walks two sorted, duplicate-free ID slices with a merge-join — the
// same shape a reconciliation job uses to compare a ledger's IDs against a
// payment provider's IDs without a map or a second pass. The condition-only
// loop advances whichever pointer holds the smaller ID (or both, on a match)
// and stops the instant either slice runs out; two short trailing loops then
// drain whatever the merge did not reach.
func Diff(a, b []string) Result {
	var res Result
	i, j := 0, 0

	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			res.OnlyInA = append(res.OnlyInA, a[i])
			i++
		case a[i] > b[j]:
			res.OnlyInB = append(res.OnlyInB, b[j])
			j++
		default:
			res.InBoth = append(res.InBoth, a[i])
			i++
			j++
		}
	}

	for ; i < len(a); i++ {
		res.OnlyInA = append(res.OnlyInA, a[i])
	}
	for ; j < len(b); j++ {
		res.OnlyInB = append(res.OnlyInB, b[j])
	}

	return res
}
```

### Tests

The table covers every shape the merge has to get right: `interleaved` hits
all three switch branches in one run; `disjoint` and `identical` are the two
extremes (nothing matches, everything matches); `a empty`/`b empty`/
`both empty` exercise the loop never running at all, so the whole result
comes from the trailing drains (or nothing); and `a exhausts first, b has a
long tail` is the sharpest check that the trailing drain for `b` actually
runs and does not stop after one element.

Create `reconcile_test.go`:

```go
package reconcile

import (
	"slices"
	"testing"
)

func TestDiff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		a, b       []string
		wantOnlyA  []string
		wantOnlyB  []string
		wantInBoth []string
	}{
		{
			name:       "interleaved",
			a:          []string{"1", "2", "4", "6"},
			b:          []string{"2", "3", "4", "5"},
			wantOnlyA:  []string{"1", "6"},
			wantOnlyB:  []string{"3", "5"},
			wantInBoth: []string{"2", "4"},
		},
		{
			name:       "disjoint",
			a:          []string{"1", "2"},
			b:          []string{"3", "4"},
			wantOnlyA:  []string{"1", "2"},
			wantOnlyB:  []string{"3", "4"},
			wantInBoth: nil,
		},
		{
			name:       "identical",
			a:          []string{"1", "2", "3"},
			b:          []string{"1", "2", "3"},
			wantOnlyA:  nil,
			wantOnlyB:  nil,
			wantInBoth: []string{"1", "2", "3"},
		},
		{
			name:       "a empty",
			a:          nil,
			b:          []string{"1", "2"},
			wantOnlyA:  nil,
			wantOnlyB:  []string{"1", "2"},
			wantInBoth: nil,
		},
		{
			name:       "b empty",
			a:          []string{"1", "2"},
			b:          nil,
			wantOnlyA:  []string{"1", "2"},
			wantOnlyB:  nil,
			wantInBoth: nil,
		},
		{
			name:       "both empty",
			a:          nil,
			b:          nil,
			wantOnlyA:  nil,
			wantOnlyB:  nil,
			wantInBoth: nil,
		},
		{
			name:       "a exhausts first, b has a long tail",
			a:          []string{"1"},
			b:          []string{"1", "2", "3", "4"},
			wantOnlyA:  nil,
			wantOnlyB:  []string{"2", "3", "4"},
			wantInBoth: []string{"1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Diff(tc.a, tc.b)
			if !slices.Equal(got.OnlyInA, tc.wantOnlyA) {
				t.Errorf("OnlyInA = %v, want %v", got.OnlyInA, tc.wantOnlyA)
			}
			if !slices.Equal(got.OnlyInB, tc.wantOnlyB) {
				t.Errorf("OnlyInB = %v, want %v", got.OnlyInB, tc.wantOnlyB)
			}
			if !slices.Equal(got.InBoth, tc.wantInBoth) {
				t.Errorf("InBoth = %v, want %v", got.InBoth, tc.wantInBoth)
			}
		})
	}
}
```

## Review

`Diff` is correct when every ID from both inputs ends up in exactly one
bucket, and the merge-join guarantees that structurally: the main loop
consumes both slices in lockstep as long as both have elements left, and the
two trailing loops guarantee nothing is lost once one side runs out first.
`TestDiff`'s `a exhausts first, b has a long tail` case is the one that
catches a missing or short-circuited trailing drain, which is the most common
bug in a hand-rolled merge. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the condition-only form with two independent bounds used here.
- [slices package](https://pkg.go.dev/slices) — `slices.Equal` used to compare result buckets in the tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-pricing-fixed-point-convergence.md](14-pricing-fixed-point-convergence.md) | Next: [16-circuit-breaker-exponential-reset.md](16-circuit-breaker-exponential-reset.md)
