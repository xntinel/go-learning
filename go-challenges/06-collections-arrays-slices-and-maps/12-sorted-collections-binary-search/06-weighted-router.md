# Exercise 6: Weighted Traffic Splitter via Cumulative-Weight Binary Search

A canary rollout or weighted load balancer routes a fraction of traffic to each
variant in proportion to an integer weight. Compile the weights into a cumulative
(prefix-sum) table and a single binary search maps a random draw `r` in
`[0, total)` to the owning variant. The exact same structure powers percentage
feature-flag rollouts and A/B splits.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
weightedrouter/              independent module: example.com/weightedrouter
  go.mod
  router.go                  type Variant, Router; New (builds prefix sums), Pick, Total
  cmd/
    demo/
      main.go                sweep the draw space, show the split
  router_test.go             boundary draws, exact distribution, zero-weight, single variant
```

Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
Implement: `Router` with `New([]Variant) (*Router, error)`, `Pick(r int) string`, `Total() int`, using a cumulative-weight table and `sort.Search`.
Test: boundary draws `r=0`, `r=total-1`, and each bucket edge land in the right variant; a full sweep gives each variant exactly `weight` picks; zero-weight variants are never picked; a single-variant table always returns it.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/weightedrouter/cmd/demo
cd ~/go-exercises/weightedrouter
go mod init example.com/weightedrouter
```

### Prefix sums turn weights into half-open buckets

Give variants weights `[3, 2, 5]`. The cumulative table is `[3, 5, 10]`, and it
carves `[0, 10)` into three half-open buckets: variant 0 owns `[0, 3)`, variant 1
owns `[3, 5)`, variant 2 owns `[5, 10)`. A draw `r` belongs to the first variant
whose cumulative boundary *exceeds* `r`:

```go
i := sort.Search(len(cum), func(i int) bool { return cum[i] > r })
```

The predicate is `cum[i] > r` (strictly greater), not `>=`. That strictness is
what makes the buckets half-open and puts each edge on the correct side: for
`[3, 5)`, the draw `r = 3` must land in variant 1, and `cum[0] = 3 > 3` is false
while `cum[1] = 5 > 3` is true, so the search returns index 1. Using `>=` here
would put `r = 3` in variant 0 and shift every bucket by one — the canonical
off-by-one at a bucket edge.

A zero-weight variant contributes no width: with weights `[3, 0, 2]` the
cumulative table is `[3, 3, 5]`, and the bucket for the middle variant is `[3, 3)`
— empty. Because the predicate is strict, no draw ever selects it: the search
skips straight over the repeated cumulative value. That is the correct behavior
for a disabled variant, and it falls out of the half-open discipline with no
special case.

`New` validates that no weight is negative and that the total is positive (an
all-zero table can route nothing), building the prefix-sum slice once. `Pick`
assumes `r` is in `[0, total)` — the caller draws `rand.IntN(total)` — and clamps
a stray `r >= total` to the last bucket so a boundary draw can never index out of
range.

Create `router.go`:

```go
package weightedrouter

import (
	"errors"
	"fmt"
	"sort"
)

// ErrNoWeight is returned when the total weight is not positive.
var ErrNoWeight = errors.New("weightedrouter: total weight must be positive")

// ErrNegativeWeight is returned when a variant has a negative weight.
var ErrNegativeWeight = errors.New("weightedrouter: negative weight")

// Variant is a routing target with an integer weight.
type Variant struct {
	Name   string
	Weight int
}

// Router maps a draw in [0, Total) to a variant in proportion to weight.
type Router struct {
	names []string
	cum   []int // cum[i] = sum of weights[0..i]; strictly the bucket upper bounds
	total int
}

// New compiles variants into a cumulative-weight table.
func New(variants []Variant) (*Router, error) {
	r := &Router{
		names: make([]string, len(variants)),
		cum:   make([]int, len(variants)),
	}
	sum := 0
	for i, v := range variants {
		if v.Weight < 0 {
			return nil, fmt.Errorf("%w: %s has weight %d", ErrNegativeWeight, v.Name, v.Weight)
		}
		sum += v.Weight
		r.names[i] = v.Name
		r.cum[i] = sum
	}
	if sum <= 0 {
		return nil, ErrNoWeight
	}
	r.total = sum
	return r, nil
}

// Total is the sum of all weights; draw r from [0, Total).
func (r *Router) Total() int { return r.total }

// Pick returns the variant owning draw r. r is expected in [0, Total); a value
// at or past Total is clamped to the last bucket.
func (r *Router) Pick(draw int) string {
	if draw < 0 {
		draw = 0
	}
	if draw >= r.total {
		draw = r.total - 1
	}
	i := sort.Search(len(r.cum), func(i int) bool { return r.cum[i] > draw })
	return r.names[i]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/weightedrouter"
)

func main() {
	r, err := weightedrouter.New([]weightedrouter.Variant{
		{Name: "stable", Weight: 7},
		{Name: "canary", Weight: 2},
		{Name: "off", Weight: 0},
		{Name: "experimental", Weight: 1},
	})
	if err != nil {
		panic(err)
	}

	counts := map[string]int{}
	for draw := range r.Total() {
		counts[r.Pick(draw)]++
	}
	fmt.Printf("total=%d\n", r.Total())
	fmt.Printf("stable=%d canary=%d experimental=%d off=%d\n",
		counts["stable"], counts["canary"], counts["experimental"], counts["off"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=10
stable=7 canary=2 experimental=1 off=0
```

### Tests

`TestBucketEdges` pins each boundary draw to its variant, proving no off-by-one at
the edges. `TestExactDistribution` sweeps every draw in `[0, total)` and asserts
each variant is picked exactly `weight` times. `TestZeroWeightNeverPicked` and
`TestSingleVariant` cover the degenerate shapes, and `TestNewRejectsEmpty` /
`TestNewRejectsNegative` pin the constructor's guards.

Create `router_test.go`:

```go
package weightedrouter

import (
	"errors"
	"fmt"
	"testing"
)

func mustRouter(t *testing.T, vs ...Variant) *Router {
	t.Helper()
	r, err := New(vs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestBucketEdges(t *testing.T) {
	t.Parallel()

	// weights 3,2,5 -> buckets [0,3) [3,5) [5,10)
	r := mustRouter(t,
		Variant{"a", 3}, Variant{"b", 2}, Variant{"c", 5})

	cases := []struct {
		draw int
		want string
	}{
		{0, "a"}, {2, "a"}, {3, "b"}, {4, "b"}, {5, "c"}, {9, "c"},
	}
	for _, tc := range cases {
		if got := r.Pick(tc.draw); got != tc.want {
			t.Fatalf("Pick(%d) = %q, want %q", tc.draw, got, tc.want)
		}
	}
}

func TestExactDistribution(t *testing.T) {
	t.Parallel()

	weights := []Variant{{"a", 3}, {"b", 2}, {"c", 5}}
	r := mustRouter(t, weights...)

	counts := map[string]int{}
	for draw := range r.Total() {
		counts[r.Pick(draw)]++
	}
	for _, v := range weights {
		if counts[v.Name] != v.Weight {
			t.Fatalf("variant %s picked %d times, want %d", v.Name, counts[v.Name], v.Weight)
		}
	}
}

func TestZeroWeightNeverPicked(t *testing.T) {
	t.Parallel()

	r := mustRouter(t, Variant{"live", 4}, Variant{"disabled", 0}, Variant{"beta", 1})
	for draw := range r.Total() {
		if r.Pick(draw) == "disabled" {
			t.Fatalf("zero-weight variant picked at draw %d", draw)
		}
	}
}

func TestSingleVariant(t *testing.T) {
	t.Parallel()

	r := mustRouter(t, Variant{"only", 1})
	for _, draw := range []int{0, 5, -1, 100} {
		if got := r.Pick(draw); got != "only" {
			t.Fatalf("Pick(%d) = %q, want only", draw, got)
		}
	}
}

func TestNewRejectsEmpty(t *testing.T) {
	t.Parallel()

	if _, err := New(nil); !errors.Is(err, ErrNoWeight) {
		t.Fatalf("New(nil): err = %v, want ErrNoWeight", err)
	}
	if _, err := New([]Variant{{"z", 0}}); !errors.Is(err, ErrNoWeight) {
		t.Fatalf("New(all-zero): err = %v, want ErrNoWeight", err)
	}
}

func TestNewRejectsNegative(t *testing.T) {
	t.Parallel()

	if _, err := New([]Variant{{"bad", -1}}); !errors.Is(err, ErrNegativeWeight) {
		t.Fatalf("New(negative): err = %v, want ErrNegativeWeight", err)
	}
}

func Example() {
	r, _ := New([]Variant{{"stable", 8}, {"canary", 2}})
	counts := map[string]int{}
	for draw := range r.Total() {
		counts[r.Pick(draw)]++
	}
	fmt.Println(counts["stable"], counts["canary"])
	// Output: 8 2
}
```

## Review

The router is correct when the cumulative boundaries are strict upper bounds and
the search predicate is `cum[i] > r`. The distribution sweep is the definitive
test: any off-by-one at a bucket edge, or a `>=` instead of `>`, would make some
variant's count drift by one and the test would fail. Zero-weight variants
vanishing is not a special case — it is the half-open buckets doing their job.
Keep `Total()` and the draw range in sync: callers must draw from `[0, Total())`,
and `Pick` clamps anything past the end so a boundary draw cannot panic. Run
`go test -race`.

## Resources

- [`sort.Search`](https://pkg.go.dev/sort#Search) — the cumulative-boundary search.
- [`slices` package](https://pkg.go.dev/slices) — `IsSorted` to assert the prefix-sum invariant if you add a debug check.
- [Feature flags and percentage rollouts](https://martinfowler.com/articles/feature-toggles.html) — where weighted routing shows up in delivery.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-migration-floor-ceiling.md](07-migration-floor-ceiling.md)
