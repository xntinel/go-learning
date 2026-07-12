# Exercise 26: Check Bloom Filter Membership Before Deduplicating

**Nivel: Intermedio** — validacion rapida (un test corto).

A deduplication pipeline ingesting millions of order or event IDs cannot
afford to ask the database "have I seen this before?" for every single
record — that turns a batch job into a storm of point lookups against
whatever store holds the canonical set. A Bloom filter sits in front of
that lookup as a cheap, all-in-memory pre-check: it can say "definitely
not seen" with total certainty, cutting most of the traffic to the real
store, at the cost of occasionally saying "probably seen" about something
that is actually new. Getting this right means routing a filter's answer to
exactly the right handling — a hard miss short-circuits straight to
"new," a probable hit still means "treat as a duplicate," and neither
should be confused with the filter refusing to process malformed input.
This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
bloom-filter-existence-check/   independent module: example.com/bloom-filter-existence-check
  go.mod                        go 1.24
  bloomfilter.go                 (*Filter).Check(item string) any; Dedupe(f *Filter, items []string) ([]string, error)
  cmd/
    demo/
      main.go                    dedupes a batch, then classifies one check result
  bloomfilter_test.go             empty item, definite miss, probable hit, dedupe behavior
```

- Files: `bloomfilter.go`, `cmd/demo/main.go`, `bloomfilter_test.go`.
- Implement: `(*Filter).Check(item string) any` returning `MissResult`,
  `HitResult`, or `ErrorResult`; `Dedupe(f *Filter, items []string)
  ([]string, error)` type-switching on `Check`'s result to decide whether
  to keep, drop, or fail on each item.
- Test: an empty item is rejected on both `Check` and `Add`, an unseen item
  in a large sparse filter is a definite miss, an added item is a hit with
  a false-positive rate strictly between 0 and 1, and `Dedupe` both keeps
  distinct items while dropping a repeat and propagates an error for a
  malformed one.

Set up the module:

```bash
go mod edit -go=1.24
```

A Bloom filter's one-sided guarantee is the whole design: it can never
produce a false negative, only a false positive, because every bit an item
sets stays set forever and is shared with whatever other items happen to
hash into the same position. `Check` exploits that asymmetry by returning
the instant it finds any unset bit position — that alone proves the item
was never added, with total certainty, and no further hashing is needed.
Only when every position is already set does `Check` have to hedge with
`HitResult`, because it cannot distinguish "this item was really added"
from "these bits were all set by the coincidental union of other items."
The double-hashing trick (`h1 + i*h2` for `i` in `0..k-1`) is what lets one
filter use `k` seemingly independent bit positions per item while only ever
computing two real hash functions — computing `k` genuinely independent
hashes would cost more without materially changing the false-positive math,
which is the standard Kirsch–Mitzenmacher result this filter relies on
without proving it here.

Create `bloomfilter.go`:

```go
package bloomfilter

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
)

// ErrEmptyItem is returned when the filter is asked to hash an empty
// string, which carries no information to deduplicate against.
var ErrEmptyItem = errors.New("bloomfilter: item must not be empty")

// MissResult means the item is definitely not in the filter: a Bloom filter
// never produces false negatives, so this is a hard guarantee, not an
// estimate.
type MissResult struct{}

// HitResult means every bit position the item hashes to is already set.
// This may be a true positive (the item really was added) or a false
// positive (an unlucky collision from other items sharing those same bit
// positions) — FalsePositiveRate estimates how likely the latter is, given
// how full the filter currently is.
type HitResult struct {
	FalsePositiveRate float64
}

// ErrorResult wraps an item the filter refuses to hash.
type ErrorResult struct {
	Err error
}

// Filter is a fixed-size Bloom filter using k independent bit positions per
// item, derived from two FNV-1a hashes via double hashing
// (Kirsch–Mitzenmacher): position i is (h1 + i*h2) mod m. This avoids
// running k separate hash functions while still giving positions that
// behave like independent hashes for false-positive-rate purposes.
type Filter struct {
	bits []bool
	k    int
	n    int // number of items added, tracked for the false-positive estimate
}

// New returns a Filter with m bits and k hash positions per item. Larger m
// relative to the number of items added lowers the false-positive rate;
// larger k lowers it further up to a point, then starts raising it again as
// the bit array saturates.
func New(m, k int) *Filter {
	return &Filter{bits: make([]bool, m), k: k}
}

func (f *Filter) positions(item string) ([2]uint64, error) {
	if item == "" {
		return [2]uint64{}, ErrEmptyItem
	}
	h1 := fnv.New64a()
	h1.Write([]byte(item))
	h2 := fnv.New64a()
	h2.Write([]byte(item))
	h2.Write([]byte{0xff}) // perturb so h2 is not simply h1 again
	return [2]uint64{h1.Sum64(), h2.Sum64()}, nil
}

// Add sets item's k bit positions. Add reports an error only for input
// Check would also reject; route new items through Check first so both
// paths reject the same input the same way.
func (f *Filter) Add(item string) error {
	hashes, err := f.positions(item)
	if err != nil {
		return err
	}
	size := uint64(len(f.bits))
	for i := 0; i < f.k; i++ {
		idx := (hashes[0] + uint64(i)*hashes[1]) % size
		f.bits[idx] = true
	}
	f.n++
	return nil
}

// Check reports whether item might already be present. It returns MissResult
// the instant any of the k positions is unset — a definite miss — HitResult
// when every position is set — a probable hit, since those bits could have
// been set by unrelated items — or ErrorResult for input the filter cannot
// hash at all.
func (f *Filter) Check(item string) any {
	hashes, err := f.positions(item)
	if err != nil {
		return ErrorResult{Err: err}
	}
	size := uint64(len(f.bits))
	for i := 0; i < f.k; i++ {
		idx := (hashes[0] + uint64(i)*hashes[1]) % size
		if !f.bits[idx] {
			return MissResult{}
		}
	}
	return HitResult{FalsePositiveRate: f.estimatedFalsePositiveRate()}
}

// estimatedFalsePositiveRate applies the standard Bloom filter formula
// (1 - e^(-kn/m))^k for m bits, n items added, and k hash positions.
func (f *Filter) estimatedFalsePositiveRate() float64 {
	m := float64(len(f.bits))
	k := float64(f.k)
	n := float64(f.n)
	return math.Pow(1-math.Exp(-k*n/m), k)
}

// Dedupe filters items down to those Check reports as a definite miss,
// adding each survivor to f as it is kept. Because a HitResult can be a
// false positive, Dedupe trades a small, bounded chance of dropping a
// genuinely new item for an O(1) membership check instead of a lookup
// against the full persisted record set — the exact trade a Bloom filter
// exists to make at scale, where that lookup would otherwise hit a database
// or a remote cache on every single incoming record.
func Dedupe(f *Filter, items []string) ([]string, error) {
	kept := make([]string, 0, len(items))
	for _, item := range items {
		switch res := f.Check(item).(type) {
		case MissResult:
			kept = append(kept, item)
			if err := f.Add(item); err != nil {
				return nil, fmt.Errorf("bloomfilter: add %q: %w", item, err)
			}
		case HitResult:
			// Probable duplicate: dropped, not added again.
		case ErrorResult:
			return nil, fmt.Errorf("bloomfilter: check %q: %w", item, res.Err)
		default:
			return nil, fmt.Errorf("bloomfilter: unexpected check result type %T", res)
		}
	}
	return kept, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bloom-filter-existence-check"
)

func main() {
	f := bloomfilter.New(4096, 4)

	incoming := []string{"order-1001", "order-1002", "order-1001", "order-1003"}
	kept, err := bloomfilter.Dedupe(f, incoming)
	if err != nil {
		fmt.Println("dedupe error:", err)
		return
	}
	fmt.Println("kept:", kept)

	switch res := f.Check("order-1002").(type) {
	case bloomfilter.MissResult:
		fmt.Println("order-1002: definite miss")
	case bloomfilter.HitResult:
		fmt.Printf("order-1002: probable hit (fp rate %.2e)\n", res.FalsePositiveRate)
	case bloomfilter.ErrorResult:
		fmt.Println("order-1002: error:", res.Err)
	}

	switch res := f.Check("").(type) {
	case bloomfilter.ErrorResult:
		fmt.Println("empty item rejected:", res.Err)
	default:
		fmt.Println("unexpected result for empty item")
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
kept: [order-1001 order-1002 order-1003]
order-1002: probable hit (fp rate 7.32e-11)
empty item rejected: bloomfilter: item must not be empty
```

`order-1001` is dropped from `kept` because it repeats in the input batch,
even though `Dedupe` never queries a database to know that — the second
occurrence hits the bits the first occurrence already set. The reported
false-positive rate after adding only 3 items into 4096 bits is
vanishingly small, which is exactly the point: a sparsely filled filter
gives strong confidence that a hit really is a repeat.

### Tests

Create `bloomfilter_test.go`:

```go
package bloomfilter

import (
	"errors"
	"testing"
)

func TestFilter(t *testing.T) {
	t.Parallel()

	t.Run("empty item is rejected on check and add", func(t *testing.T) {
		t.Parallel()
		f := New(1024, 4)
		res, ok := f.Check("").(ErrorResult)
		if !ok {
			t.Fatalf("Check(\"\") = %#v, want ErrorResult", res)
		}
		if !errors.Is(res.Err, ErrEmptyItem) {
			t.Fatalf("Check(\"\") err = %v, want ErrEmptyItem", res.Err)
		}
		if err := f.Add(""); !errors.Is(err, ErrEmptyItem) {
			t.Fatalf("Add(\"\") err = %v, want ErrEmptyItem", err)
		}
	})

	t.Run("unseen item in a large sparse filter is a definite miss", func(t *testing.T) {
		t.Parallel()
		f := New(4096, 4)
		if err := f.Add("order-1001"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, ok := f.Check("order-9999").(MissResult); !ok {
			t.Fatalf("Check(unseen) = %#v, want MissResult", f.Check("order-9999"))
		}
	})

	t.Run("added item is a hit", func(t *testing.T) {
		t.Parallel()
		f := New(4096, 4)
		if err := f.Add("order-1001"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		res, ok := f.Check("order-1001").(HitResult)
		if !ok {
			t.Fatalf("Check(added) = %#v, want HitResult", f.Check("order-1001"))
		}
		if res.FalsePositiveRate <= 0 || res.FalsePositiveRate >= 1 {
			t.Fatalf("FalsePositiveRate = %v, want a value in (0, 1)", res.FalsePositiveRate)
		}
	})

	t.Run("dedupe drops a repeated item and keeps distinct ones", func(t *testing.T) {
		t.Parallel()
		f := New(4096, 4)
		kept, err := Dedupe(f, []string{"a", "b", "a", "c"})
		if err != nil {
			t.Fatalf("Dedupe: %v", err)
		}
		want := []string{"a", "b", "c"}
		if len(kept) != len(want) {
			t.Fatalf("Dedupe kept = %v, want %v", kept, want)
		}
		for i, w := range want {
			if kept[i] != w {
				t.Fatalf("Dedupe kept = %v, want %v", kept, want)
			}
		}
	})

	t.Run("dedupe surfaces an error for an empty item", func(t *testing.T) {
		t.Parallel()
		f := New(4096, 4)
		if _, err := Dedupe(f, []string{"a", ""}); !errors.Is(err, ErrEmptyItem) {
			t.Fatalf("Dedupe err = %v, want ErrEmptyItem", err)
		}
	})
}
```

Verify: `go test -count=1 ./...`

## Review

`Check` is correct because it returns as soon as it finds one unset bit
position, which is what makes a miss a hard guarantee rather than a
probability: a Bloom filter's soundness only runs in one direction, and
returning early on the first unset bit is what preserves it. Routing every
item in `Dedupe` back through `Check` — rather than calling `Add`
unconditionally and tracking "new" separately — is what keeps the filter's
one-sided guarantee load-bearing instead of decorative: skipping the check
and always adding would silently turn every filter into a no-op
deduplicator, since adding an item that is already present is harmless but
tells you nothing about whether it was new. The one trap this design avoids
is treating `HitResult` as certain: code that skips the false-positive
rate and treats every hit as a confirmed duplicate will, at scale, silently
drop a small fraction of genuinely new records — which is an acceptable,
budgeted trade at the filter sizes production systems use, not a bug, as
long as the false-positive rate is tracked and kept within the tolerance
the pipeline actually needs.

## Resources

- [Bloom, B. H. (1970). Space/time trade-offs in hash coding with allowable errors](https://dl.acm.org/doi/10.1145/362686.362692)
- [Kirsch & Mitzenmacher: Less hashing, same performance (double hashing)](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf)
- [hash/fnv package](https://pkg.go.dev/hash/fnv)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-sliding-window-request-classifier.md](25-sliding-window-request-classifier.md) | Next: [27-admission-control-load-shedding.md](27-admission-control-load-shedding.md)
