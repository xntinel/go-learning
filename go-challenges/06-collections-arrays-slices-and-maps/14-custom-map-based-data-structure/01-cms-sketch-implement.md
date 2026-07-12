# Exercise 1: Count-Min Sketch for stream frequency estimation

A rate limiter needs to know which clients are hammering the API, but keeping an
exact `map[clientID]int` for millions of clients would not fit the memory budget.
A Count-Min Sketch answers "roughly how often have I seen this item?" in a fixed
amount of memory that does not grow with the number of distinct items — at the
price of occasionally over-counting. This module builds the sketch; the next one
pins its probabilistic contract with tests.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
cms/                       independent module: example.com/cms
  go.mod
  cms.go                   type Sketch; New(rows,width), Add(item), Estimate(item)
  cmd/
    demo/
      main.go              count client requests in a fixed table, print estimates
  cms_test.go              upper-bound + zero-for-missing tests, Example
```

- Files: `cms.go`, `cmd/demo/main.go`, `cms_test.go`.
- Implement: `Sketch{rows, width, table [][]uint64, hashes []uint64}` with `New(rows, width)`, `Add(item string)` incrementing one salted counter per row, and `Estimate(item string)` returning the minimum across rows.
- Test: adding an item three times estimates at least three; an unseen item estimates zero; an `Example` with deterministic output.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/01-cms-sketch-implement/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/01-cms-sketch-implement
```

### The shape: a 2D counter table plus one hash per row

A Count-Min Sketch is a `rows × width` table of unsigned counters. `Add(item)`
hashes the item once, derives a distinct column per row, and increments the
counter at `(row, column)` in every row. `Estimate(item)` recomputes the same
columns and returns the **minimum** of those counters. Two facts make this work.
First, collisions can only *add* to a counter, never subtract, so every counter
that an item touches is an upper bound on that item's true count — and the
tightest of those upper bounds is the minimum across rows, which is why
`Estimate` returns the min. Returning the max would give a useless lower bound.
Second, the more rows there are, the more independent chances there are for at
least one row to be collision-free for a given item, which is why depth raises
confidence in the bound.

The independence across rows is the subtle part. The error analysis assumes the
per-row hashes are pairwise independent. Using the *same* hash for every row is
the classic bug: the columns would be perfectly correlated and the extra rows
would buy nothing. The practical construction is one strong base hash combined
with a distinct per-row salt. Here the base hash is FNV-1a (a fixed,
deterministic hash — good for reproducibility), and each row XORs in a salt and
then runs a murmur-style avalanche finalizer so that even rows whose salts differ
only in high bits produce different *low* bits, which is what `% width` actually
consumes.

Create `cms.go`:

```go
package cms

import "hash/fnv"

// Sketch is a Count-Min Sketch: a fixed-memory probabilistic frequency counter.
// Its footprint is rows*width counters regardless of how many distinct items it
// sees, and its error is one-sided — it never underestimates, only overestimates.
type Sketch struct {
	rows   int
	width  int
	table  [][]uint64
	hashes []uint64 // per-row salt, giving each row an independent hash
}

// New returns a Sketch with the given number of rows (depth) and width. Both are
// clamped to at least 1 so a misconfigured caller cannot produce a zero-size
// table (which would divide by zero on Add).
func New(rows, width int) *Sketch {
	if rows <= 0 {
		rows = 1
	}
	if width <= 0 {
		width = 1
	}
	s := &Sketch{
		rows:   rows,
		width:  width,
		table:  make([][]uint64, rows),
		hashes: make([]uint64, rows),
	}
	for i := range rows {
		s.table[i] = make([]uint64, width)
		// Distinct, low-bit-varied salt per row (golden-ratio constant times
		// the row index) so the rows hash independently.
		s.hashes[i] = uint64(i+1) * 0x9e3779b97f4a7c15
	}
	return s
}

// Add records one occurrence of item, incrementing its counter in every row.
func (s *Sketch) Add(item string) {
	h := base(item)
	for i := range s.rows {
		s.table[i][mix(h, s.hashes[i])%uint64(s.width)]++
	}
}

// Estimate returns the estimated number of times item was added. The result is
// an upper bound on the true count (never less), tightest as the minimum across
// rows.
func (s *Sketch) Estimate(item string) uint64 {
	h := base(item)
	var m uint64
	for i := range s.rows {
		v := s.table[i][mix(h, s.hashes[i])%uint64(s.width)]
		if i == 0 || v < m {
			m = v
		}
	}
	return m
}

// base is the single strong base hash (FNV-1a, deterministic across processes).
func base(item string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(item))
	return h.Sum64()
}

// mix folds a per-row salt into the base hash and avalanches it, so the low bits
// that % width consumes differ from row to row even when salts share high bits.
func mix(h, salt uint64) uint64 {
	x := h ^ salt
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return x
}
```

### The runnable demo

The demo models the rate-limiter use case: a `4 × 2048` table (32 KiB of
counters, fixed) counts requests per client IP. One client sends 500 requests,
another sends 3. With only a handful of distinct keys and a wide table there are
no collisions across all four rows, so the estimates come out exact — and an
unseen client estimates exactly zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cms"
)

func main() {
	s := cms.New(4, 2048)
	for range 500 {
		s.Add("203.0.113.7") // a noisy client hammering the API
	}
	for range 3 {
		s.Add("198.51.100.4") // a normal client
	}

	fmt.Printf("noisy client estimate:  %d\n", s.Estimate("203.0.113.7"))
	fmt.Printf("normal client estimate: %d\n", s.Estimate("198.51.100.4"))
	fmt.Printf("unseen client estimate: %d\n", s.Estimate("192.0.2.1"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
noisy client estimate:  500
normal client estimate: 3
unseen client estimate: 0
```

### Tests

This module's tests are a floor for the contract: adding an item N times must
estimate at least N (the sketch never undercounts), and an unseen item must
estimate zero (no counter it touches was ever incremented). The statistical,
over-estimate-bounded tests live in Exercise 2.

Create `cms_test.go`:

```go
package cms

import (
	"fmt"
	"testing"
)

func TestEstimateIsAtLeastTrueCount(t *testing.T) {
	t.Parallel()

	s := New(4, 1024)
	s.Add("hello")
	s.Add("hello")
	s.Add("hello")

	if got := s.Estimate("hello"); got < 3 {
		t.Fatalf("Estimate(hello) = %d, want >= 3 (one-sided error)", got)
	}
}

func TestEstimateZeroForUnseen(t *testing.T) {
	t.Parallel()

	s := New(4, 1024)
	if got := s.Estimate("never-added"); got != 0 {
		t.Fatalf("Estimate(never-added) = %d, want 0", got)
	}
}

func TestNewClampsNonPositiveDimensions(t *testing.T) {
	t.Parallel()

	s := New(0, 0) // must not produce a zero-size table
	s.Add("x")     // would divide by zero if width were 0
	if got := s.Estimate("x"); got < 1 {
		t.Fatalf("Estimate(x) = %d after one Add, want >= 1", got)
	}
}

func Example() {
	s := New(4, 1024)
	for range 10 {
		s.Add("GET /login")
	}
	fmt.Println(s.Estimate("GET /login"))
	fmt.Println(s.Estimate("GET /health"))
	// Output:
	// 10
	// 0
}
```

## Review

The sketch is correct when `Estimate` is a true upper bound: for any item added N
times, `Estimate >= N`, and for any item never added, `Estimate == 0`. Both follow
from the same fact — counters only ever increment, so the minimum across rows can
be inflated by collisions but never deflated below the truth. The two structural
mistakes to avoid are reusing one hash across all rows (which correlates the
counters and voids the independence the error bound assumes — the per-row salt in
`mix` prevents it) and returning the maximum instead of the minimum (the max is a
lower bound; the Count-Min Sketch returns the min). The `New(0, 0)` test exists
because a zero width would make `% uint64(s.width)` a divide-by-zero panic on the
first `Add`; clamping to 1 keeps a misconfigured caller safe. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [`hash/fnv` package](https://pkg.go.dev/hash/fnv) — the FNV-1a base hash and its `Sum64`.
- [Count-Min Sketch (Wikipedia)](https://en.wikipedia.org/wiki/Count%E2%80%93min_sketch) — the structure, the one-sided error, and the `N/width` bound.
- [`hash` package](https://pkg.go.dev/hash) — the `Hash64` interface `New64a` returns.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cms-probabilistic-tests.md](02-cms-probabilistic-tests.md)
