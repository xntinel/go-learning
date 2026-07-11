# Exercise 2: Pinning the probabilistic contract of a sketch with tests

Testing a probabilistic data structure is a skill of its own: assert too tightly
and the test flakes on an unlucky hash draw; assert too loosely and it proves
nothing. The trick is to test only the *one-sided* guarantees the structure
actually makes — the estimate is never below the truth, and is never above the
truth by more than a generous bound — with a table wide enough that the bound
holds deterministically for the fixed input. This module rebuilds the sketch (so
it stands alone) and wraps it in that contract suite.

This module is fully self-contained: its own `go mod init`, the sketch inline,
and its own demo. Nothing here imports another exercise.

## What you'll build

```text
cms/                       independent module: example.com/cms
  go.mod
  cms.go                   the Sketch from Exercise 1 (New, Add, Estimate)
  cmd/
    demo/
      main.go              show the over-estimate bound holds for rare keys
  cms_test.go              the one-sided-contract suite (upper bound, zero, heavy-vs-rare)
```

- Files: `cms.go`, `cmd/demo/main.go`, `cms_test.go`.
- Implement: the same `Sketch` (needed so the module gates alone).
- Test: `Estimate >= trueCount` always; `Estimate == 0` for an unseen key; every seen key estimates `>= 1`; a heavy key estimates `>= its true count` while rare keys stay under a small over-estimate bound.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cms/cmd/demo
cd ~/go-exercises/cms
go mod init example.com/cms
```

### Why every assertion here is one-sided

A Count-Min Sketch makes exactly one promise: `Estimate(x)` is never less than the
true count of `x`, and its over-estimate is bounded (with high probability) by
`N / width`. A good test suite asserts only things that follow from that promise
and hold for *every* possible hash outcome, never things that depend on a
particular hash draw:

- **Upper bound.** After adding `x` exactly `k` times, `Estimate(x) >= k`. This is
  true for any hashes, because counters only increment.
- **Zero for unseen.** A key never added estimates `0`, because no counter it maps
  to was ever touched. (This one is exact, not just one-sided.)
- **At-least-one.** Every key seen at least once estimates `>= 1`.
- **Heavy stays heavy, rare stays bounded.** A key added 1000 times estimates
  `>= 1000`; keys added once estimate at most a small constant. The upper side of
  the rare-key bound is the only place a too-narrow table could bite, so the test
  chooses `width` large enough (4096) that, for the fixed input, the bound holds
  for every hash — no randomness, no flake.

Notice what is *absent*: no assertion that `Estimate(x) == trueCount(x)` (false in
general — collisions inflate it) and no assertion on the *distribution* of the
error (that would be a statistics test, not a unit test). Because the base hash
is `hash/fnv` (deterministic), the same input produces the same table on every
run, so even the "bounded over-estimate" assertion is reproducible.

Create `cms.go` (identical to Exercise 1 — bundled so this module stands alone):

```go
package cms

import "hash/fnv"

// Sketch is a Count-Min Sketch: a fixed-memory probabilistic frequency counter
// with one-sided error (it never underestimates, only overestimates).
type Sketch struct {
	rows   int
	width  int
	table  [][]uint64
	hashes []uint64
}

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
		s.hashes[i] = uint64(i+1) * 0x9e3779b97f4a7c15
	}
	return s
}

func (s *Sketch) Add(item string) {
	h := base(item)
	for i := range s.rows {
		s.table[i][mix(h, s.hashes[i])%uint64(s.width)]++
	}
}

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

func base(item string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(item))
	return h.Sum64()
}

func mix(h, salt uint64) uint64 {
	x := h ^ salt
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return x
}
```

### The runnable demo

The demo makes the over-estimate bound visible: pour 1000 hits into one hot key
and one hit each into 200 rare keys, then report the hot key's estimate and the
worst over-estimate seen across all rare keys. With a `4 × 4096` table the rare
keys barely move.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cms"
)

func main() {
	s := cms.New(4, 4096)
	for range 1000 {
		s.Add("hot")
	}
	var worstRare uint64
	for i := range 200 {
		key := fmt.Sprintf("rare-%d", i)
		s.Add(key)
		if e := s.Estimate(key); e > worstRare {
			worstRare = e
		}
	}

	fmt.Printf("hot estimate (true 1000): %d\n", s.Estimate("hot"))
	fmt.Printf("worst rare estimate (true 1): %d\n", worstRare)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hot estimate (true 1000): 1000
worst rare estimate (true 1): 1
```

### Tests

Create `cms_test.go`:

```go
package cms

import (
	"fmt"
	"testing"
)

func TestEstimateIsUpperBound(t *testing.T) {
	t.Parallel()

	s := New(4, 1024)
	for range 3 {
		s.Add("hello")
	}
	if got := s.Estimate("hello"); got < 3 {
		t.Fatalf("Estimate(hello) = %d, want >= 3 (never underestimates)", got)
	}
}

func TestEstimateZeroForMissing(t *testing.T) {
	t.Parallel()

	s := New(4, 1024)
	if got := s.Estimate("missing"); got != 0 {
		t.Fatalf("Estimate(missing) = %d, want 0", got)
	}
}

func TestEverySeenKeyIsAtLeastOne(t *testing.T) {
	t.Parallel()

	s := New(4, 1024)
	for i := range 100 {
		s.Add(fmt.Sprintf("item-%d", i))
	}
	for i := range 100 {
		key := fmt.Sprintf("item-%d", i)
		if got := s.Estimate(key); got < 1 {
			t.Fatalf("Estimate(%s) = %d, want >= 1", key, got)
		}
	}
}

func TestHeavyKeyDominatesRareKeys(t *testing.T) {
	t.Parallel()

	// width 4096 is wide enough that, for this fixed input, no rare key
	// over-estimates past the generous bound below — one-sided, never flaky.
	s := New(4, 4096)
	for range 1000 {
		s.Add("common")
	}
	for i := range 100 {
		s.Add(fmt.Sprintf("rare-%d", i))
	}

	if got := s.Estimate("common"); got < 1000 {
		t.Fatalf("Estimate(common) = %d, want >= 1000", got)
	}
	const overEstimateBound = 10
	for i := range 100 {
		key := fmt.Sprintf("rare-%d", i)
		if got := s.Estimate(key); got > overEstimateBound {
			t.Fatalf("Estimate(%s) = %d, want <= %d", key, got, overEstimateBound)
		}
	}
}

func Example() {
	s := New(4, 1024)
	for range 5 {
		s.Add("x")
	}
	// Never below the truth; exactly zero for the unseen.
	fmt.Println(s.Estimate("x") >= 5)
	fmt.Println(s.Estimate("y") == 0)
	// Output:
	// true
	// true
}
```

## Review

The suite is honest when every assertion is a consequence of the one-sided error
guarantee and holds for any hash outcome: `>=` the true count, `== 0` for the
unseen, `>= 1` for anything seen, and `<=` a *generous* bound for rare keys with a
table wide enough that the bound is not close. The failure you are guarding
against is a suite that asserts equality with the true count (false whenever a
collision inflates a counter) or that narrows the over-estimate bound until it
depends on the exact hash draw — either turns a correct sketch into a flaky test.
Because `hash/fnv` is deterministic, `-count=1` gives the same table every run, so
even the bounded assertions are reproducible. Run `go test -count=1 -race ./...`.

## Resources

- [Count-Min Sketch (Wikipedia)](https://en.wikipedia.org/wiki/Count%E2%80%93min_sketch) — the error bound `N/width` and the confidence from depth.
- [`testing` package](https://pkg.go.dev/testing) — `T.Parallel`, `T.Fatalf`, and `Example` output verification.
- [`hash/fnv` package](https://pkg.go.dev/hash/fnv) — why a deterministic hash makes the probabilistic test reproducible.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-cms-sketch-implement.md](01-cms-sketch-implement.md) | Next: [03-generic-set.md](03-generic-set.md)
