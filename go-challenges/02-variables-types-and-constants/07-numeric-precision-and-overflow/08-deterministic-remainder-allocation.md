# Exercise 8: Split a Total Across Weighted Shares So the Parts Always Sum Back

A payout service splits a merchant's $10.00 three ways, or a fee is divided across
weighted recipients. Integer division alone loses the leftover cents, so the parts do
not sum to the whole — and if two services compute the split differently, the ledgers
disagree. This exercise builds a splitter using the largest-remainder (Hamilton)
method: every share gets its floor, the leftover minor units go to the largest
remainders in a deterministic order, and `sum(parts) == total` exactly, every time.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
allocate/                    independent module: example.com/allocate
  go.mod                     module path
  allocate.go                Allocate(total int64, weights []int64) ([]int64, error)
  cmd/
    demo/
      main.go                splits a payout, prints shares and their sum
  allocate_test.go           sum==total property, deterministic {34,33,33}, edge cases
```

Files: `allocate.go`, `cmd/demo/main.go`, `allocate_test.go`.
Implement: `Allocate(total int64, weights []int64) ([]int64, error)` that computes each
floor share with `big.Int` (to avoid `total*weight` overflow), distributes the leftover
to the largest remainders with a stable tie-break by index, and validates its inputs.
Test: a property test asserting `sum(allocation) == total` over many cases; that `100`
across three equal weights yields `{34,33,33}` deterministically; that the same inputs
always produce identical output; and the zero-total and single-recipient edges.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/allocate/cmd/demo
cd ~/go-exercises/allocate
go mod init example.com/allocate
```

### Why floor-and-distribute, and why it must be deterministic

The naive split — `share_i = total * weight_i / sumWeights` with integer division —
drops every fractional cent, so the shares sum to something less than `total`. The
missing cents have to go somewhere, and *where* is a policy that must be fixed and
reproducible. The largest-remainder method makes it exact and fair: give each share
its floor, then hand out the `leftover = total - sum(floors)` remaining units, one
each, to the shares whose discarded fractional part (the remainder) was largest. Since
each remainder is strictly less than the denominator, `leftover` is always fewer than
the number of shares, so every leftover unit lands on a distinct recipient and the
totals reconcile: `sum(parts) == total` by construction.

Two details make this production-grade. First, `total * weight_i` can overflow `int64`
for large amounts and weights, so the floor and remainder are computed with
`big.Int.QuoRem`, which returns the truncated quotient and the exact remainder
together — no intermediate wrap. Second, when several shares have equal remainders (as
they do for an even split like `100` across three `1`s), the tie must break the same
way in every service that computes it, or two systems will disagree on which recipient
got the extra cent. The rule here is: sort by remainder descending, and break ties by
ascending index using a *stable* sort (`slices.SortStableFunc` with `cmp.Compare`). So
`100` across `{1,1,1}` deterministically yields `{34,33,33}` — recipient 0 wins the
single leftover cent, always. Reproducibility is not a nicety here; it is what lets two
independent services agree bit-for-bit.

The function also validates: an empty recipient list, a negative weight, weights that
sum to zero, and a negative total are all rejected, because each would make the split
meaningless or the division undefined.

Create `allocate.go`:

```go
package allocate

import (
	"cmp"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// ErrInvalid marks an allocation request that cannot be split.
var ErrInvalid = errors.New("invalid allocation request")

// Allocate splits total across recipients in proportion to weights using the
// largest-remainder method, guaranteeing sum(result) == total. Ties in the
// remainder break by ascending index, so the split is fully deterministic.
func Allocate(total int64, weights []int64) ([]int64, error) {
	if len(weights) == 0 {
		return nil, fmt.Errorf("no recipients: %w", ErrInvalid)
	}
	if total < 0 {
		return nil, fmt.Errorf("negative total %d: %w", total, ErrInvalid)
	}
	var sumW int64
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("negative weight %d: %w", w, ErrInvalid)
		}
		sumW += w
	}
	if sumW == 0 {
		return nil, fmt.Errorf("weights sum to zero: %w", ErrInvalid)
	}

	type share struct {
		idx   int
		floor int64
		rem   *big.Int
	}
	den := big.NewInt(sumW)
	shares := make([]share, len(weights))
	var allocated int64
	for i, w := range weights {
		prod := new(big.Int).Mul(big.NewInt(total), big.NewInt(w))
		q := new(big.Int)
		r := new(big.Int)
		q.QuoRem(prod, den, r) // q = floor, r = remainder, no int64 overflow
		f := q.Int64()
		shares[i] = share{idx: i, floor: f, rem: r}
		allocated += f
	}

	// Order recipients by remainder descending, breaking ties by ascending index.
	order := make([]int, len(shares))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if c := shares[b].rem.Cmp(shares[a].rem); c != 0 {
			return c
		}
		return cmp.Compare(shares[a].idx, shares[b].idx)
	})

	out := make([]int64, len(shares))
	for i := range shares {
		out[i] = shares[i].floor
	}
	leftover := total - allocated
	for k := int64(0); k < leftover; k++ {
		out[order[k]]++
	}
	return out, nil
}
```

### The runnable demo

The demo splits `1000` cents across three weighted recipients and an even `100` across
three, printing the shares and confirming each sums back to its total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/allocate"
)

func main() {
	for _, c := range []struct {
		total   int64
		weights []int64
	}{
		{1000, []int64{50, 30, 20}},
		{100, []int64{1, 1, 1}},
	} {
		parts, err := allocate.Allocate(c.total, c.weights)
		if err != nil {
			fmt.Println("allocate:", err)
			continue
		}
		var sum int64
		for _, p := range parts {
			sum += p
		}
		fmt.Printf("total=%d weights=%v -> %v (sum=%d)\n", c.total, c.weights, parts, sum)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=1000 weights=[50 30 20] -> [500 300 200] (sum=1000)
total=100 weights=[1 1 1] -> [34 33 33] (sum=100)
```

### Tests

`TestSumsToTotal` is the core property: over a matrix of totals and weight sets, the
allocation must sum to exactly the total, with no share negative. `TestDeterministic`
pins the canonical `{34,33,33}` split and asserts that running the same inputs twice
produces identical output — the reproducibility two services rely on.
`TestEdgeCases` covers a zero total, a single recipient (which takes the whole amount),
and the rejected inputs.

Create `allocate_test.go`:

```go
package allocate

import (
	"errors"
	"slices"
	"testing"
)

func TestSumsToTotal(t *testing.T) {
	t.Parallel()

	totals := []int64{0, 1, 7, 100, 101, 9999, 1_000_000}
	weightSets := [][]int64{
		{1, 1, 1},
		{50, 30, 20},
		{1, 2, 3, 4},
		{7},
		{1, 1, 1, 1, 1, 1, 1},
	}
	for _, total := range totals {
		for _, weights := range weightSets {
			parts, err := Allocate(total, weights)
			if err != nil {
				t.Fatalf("Allocate(%d, %v): %v", total, weights, err)
			}
			var sum int64
			for _, p := range parts {
				if p < 0 {
					t.Fatalf("Allocate(%d, %v) produced negative share %d", total, weights, p)
				}
				sum += p
			}
			if sum != total {
				t.Fatalf("Allocate(%d, %v) = %v sums to %d, want %d", total, weights, parts, sum, total)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	first, err := Allocate(100, []int64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, []int64{34, 33, 33}) {
		t.Fatalf("Allocate(100, [1 1 1]) = %v, want [34 33 33]", first)
	}
	second, _ := Allocate(100, []int64{1, 1, 1})
	if !slices.Equal(first, second) {
		t.Fatalf("non-deterministic: %v then %v", first, second)
	}
}

func TestEdgeCases(t *testing.T) {
	t.Parallel()

	if parts, err := Allocate(0, []int64{1, 2, 3}); err != nil || !slices.Equal(parts, []int64{0, 0, 0}) {
		t.Fatalf("Allocate(0, ...) = %v,%v; want [0 0 0],nil", parts, err)
	}
	if parts, err := Allocate(500, []int64{7}); err != nil || !slices.Equal(parts, []int64{500}) {
		t.Fatalf("Allocate(500, [7]) = %v,%v; want [500],nil", parts, err)
	}
	if _, err := Allocate(100, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Allocate(100, nil) error = %v, want ErrInvalid", err)
	}
	if _, err := Allocate(100, []int64{0, 0}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Allocate(100, [0 0]) error = %v, want ErrInvalid", err)
	}
	if _, err := Allocate(-1, []int64{1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Allocate(-1, [1]) error = %v, want ErrInvalid", err)
	}
}
```

## Review

The splitter is correct when the parts always reconcile and the split is reproducible.
`TestSumsToTotal` is the invariant that matters most — over every total and weight set,
`sum(parts) == total` with no negative share, because the leftover (always fewer than
the number of shares) is distributed one unit per recipient. `TestDeterministic` proves
the tie-break is stable, so `{34,33,33}` is the answer on every run and in every
service. The two mistakes to avoid: computing `total * weight` in `int64` (it overflows
for large inputs — `big.Int.QuoRem` keeps it exact), and using an unstable sort or an
undefined tie-break, which would let two services disagree on which recipient receives
the leftover cent.

## Resources

- [`math/big#Int.QuoRem`](https://pkg.go.dev/math/big#Int.QuoRem) — truncated quotient and exact remainder in one call, overflow-free.
- [`slices.SortStableFunc`](https://pkg.go.dev/slices#SortStableFunc) — stable sort that preserves the index tie-break.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the three-way comparison used for the deterministic ordering.
- [Largest remainder method](https://en.wikipedia.org/wiki/Largest_remainder_method) — the allocation rule and its exact-sum guarantee.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-multi-currency-minor-units.md](09-multi-currency-minor-units.md)
