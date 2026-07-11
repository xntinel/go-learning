# Exercise 31: Detect Duplicates Using Bloom Filter With Secondary Confirmation

**Nivel: Intermedio** — validacion rapida (un test corto).

A log-ingestion or event-processing pipeline that needs to deduplicate
against every item it has ever seen cannot afford to keep an exact set of
every ID in memory forever — that set grows without bound. A Bloom filter
solves the memory problem (a fixed-size bit array, no matter how much
history it has absorbed) at the cost of sometimes saying "maybe" when the
true answer is "no" — a false positive, never a false negative. Pairing it
with a small, bounded exact-match window over the most recent items is how
production pipelines resolve that "maybe" into a certain answer whenever
they can, and honestly report the uncertainty when they can't. This module
builds that two-tier deduplicator. It is self-contained: its own
`go mod init`, code, demo, and test.

## What you'll build

```text
bloomcheck/                 independent module: example.com/bloom-filter-membership-checker
  go.mod                     go 1.24
  bloomcheck.go               package bloomcheck; Verdict; Deduplicator; New(bits, k, recentCap) *Deduplicator; Check(item) Verdict
  cmd/demo/main.go            runnable demo over five items showing all three verdicts
  bloomcheck_test.go          unseen-then-duplicate, eviction producing a maybe, and the no-false-negative guarantee
```

- Implement: `Check(item string) Verdict` — consults and updates a Bloom filter unconditionally, then a tagless switch over the bounded exact window's confirmation resolves the Bloom filter's "maybe" into `DUPLICATE` or `MAYBE_DUPLICATE`.
- Test: the unseen-then-exact-duplicate path, a case where the exact window evicts an item and a later re-check can only report `MAYBE_DUPLICATE`, and a check that a previously recorded item is never reported `UNSEEN` again.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bloomcheck/cmd/demo
cd ~/go-exercises/bloomcheck
go mod init example.com/bloom-filter-membership-checker
go mod edit -go=1.24
```

### Why the exact window is bounded, and what that buys the "maybe" verdict

If the secondary exact-match structure held every item ever seen, it would
make the Bloom filter pointless — you'd just consult the exact set and
never need the probabilistic one. The whole reason a real pipeline pairs
the two is that the exact set is deliberately small (the last few thousand
IDs, say, held in memory) while the Bloom filter spans the *entire*
history at a fraction of the memory cost. That asymmetry is what makes
`MAYBE_DUPLICATE` a meaningful, honest answer rather than a cop-out: when
the Bloom filter says "maybe" but the bounded window can't confirm it,
there are exactly two possibilities — the item is a genuine duplicate that
aged out of the window long ago, or it's one of the Bloom filter's
inherent false positives — and the deduplicator cannot tell which without
consulting a full history it was specifically built not to keep. `Check`'s
tagless switch makes this explicit instead of hiding it behind a
`bool`:

```go
switch {
case confirmed:
    return Duplicate
default:
    d.recordRecent(item)
    return MaybeDuplicate
}
```

The Bloom filter itself never produces a false negative — once a bit is
set, it stays set, so a genuinely unseen item is the *only* way to get a
`false` from `bloomContains`. That guarantee is what lets `Check` treat
`!maybeSeen` as certain (`Unseen`, no further check needed) while treating
`maybeSeen` as merely a reason to consult the secondary structure. Getting
this backwards — trusting a Bloom filter "yes" as certain — is the
textbook Bloom filter bug; getting it right means only ever trusting the
filter's "no."

Create `bloomcheck.go`:

```go
// Package bloomcheck deduplicates a stream of item identifiers using a
// Bloom filter as the cheap first pass across the entire history of items
// ever seen, backed by a small, bounded exact-match window for confirming
// recent duplicates precisely. This two-tier shape is exactly what a
// production log-ingestion or event-processing pipeline uses: keeping an
// exact set of every ID ever seen doesn't fit in memory, but a Bloom filter
// spanning all of history plus an exact set of the last few thousand IDs
// does, at the cost of sometimes only being able to say "maybe."
package bloomcheck

import "hash/fnv"

// Verdict is the outcome of checking one item against the deduplicator.
type Verdict string

const (
	// Duplicate means the recent-exact window confirms this exact item
	// was seen before -- certain.
	Duplicate Verdict = "DUPLICATE"
	// MaybeDuplicate means the Bloom filter (which spans all of history
	// and never false-negatives) believes the item might have been seen,
	// but the bounded recent-exact window can no longer confirm it --
	// either it's a genuine duplicate that aged out of the window, or a
	// Bloom filter false positive. Both are possible; the caller decides
	// how to handle the ambiguity (a heavier out-of-band check, a metric,
	// or just accepting the small false-positive rate).
	MaybeDuplicate Verdict = "MAYBE_DUPLICATE"
	// Unseen means the Bloom filter says no, which -- because a Bloom
	// filter never produces false negatives -- is a certain answer.
	Unseen Verdict = "UNSEEN"
)

// Deduplicator combines a Bloom filter (unbounded history, probabilistic)
// with a bounded FIFO window of exact recent items (certain, but limited
// by recentCap).
type Deduplicator struct {
	bits       []bool
	k          int
	recentSet  map[string]struct{}
	recentFIFO []string
	recentCap  int
}

// New builds a Deduplicator with a Bloom filter of bits bits and k hash
// rounds, backed by a recent-exact window holding up to recentCap items.
func New(bits, k, recentCap int) *Deduplicator {
	return &Deduplicator{
		bits:      make([]bool, bits),
		k:         k,
		recentSet: make(map[string]struct{}, recentCap),
		recentCap: recentCap,
	}
}

// Check classifies item and records it for future checks. The Bloom filter
// is consulted (and updated) unconditionally, since adding an item to a
// Bloom filter never harms a future lookup; the recent-exact window is only
// consulted to break the "maybe" the Bloom filter can never resolve on its
// own.
func (d *Deduplicator) Check(item string) Verdict {
	maybeSeen := d.bloomContains(item)
	d.bloomAdd(item)

	if !maybeSeen {
		d.recordRecent(item)
		return Unseen
	}

	_, confirmed := d.recentSet[item]
	switch {
	case confirmed:
		return Duplicate
	default:
		d.recordRecent(item)
		return MaybeDuplicate
	}
}

// recordRecent adds item to the bounded exact window, evicting the oldest
// entry first if the window is already full.
func (d *Deduplicator) recordRecent(item string) {
	if _, exists := d.recentSet[item]; exists {
		return
	}
	if len(d.recentFIFO) >= d.recentCap {
		oldest := d.recentFIFO[0]
		d.recentFIFO = d.recentFIFO[1:]
		delete(d.recentSet, oldest)
	}
	d.recentFIFO = append(d.recentFIFO, item)
	d.recentSet[item] = struct{}{}
}

// bloomContains reports whether every one of the k hash positions for item
// is already set.
func (d *Deduplicator) bloomContains(item string) bool {
	for _, pos := range d.positions(item) {
		if !d.bits[pos] {
			return false
		}
	}
	return true
}

// bloomAdd sets every one of the k hash positions for item.
func (d *Deduplicator) bloomAdd(item string) {
	for _, pos := range d.positions(item) {
		d.bits[pos] = true
	}
}

// positions computes d.k bit positions for item using the standard
// Kirsch-Mitzenmacher double-hashing technique: two independent base
// hashes combined linearly stand in for k truly independent hash
// functions, which is what every production Bloom filter implementation
// actually does rather than running k separate hash algorithms.
func (d *Deduplicator) positions(item string) []int {
	h1 := fnv.New64a()
	h1.Write([]byte(item))
	sum1 := h1.Sum64()

	h2 := fnv.New64()
	h2.Write([]byte(item))
	sum2 := h2.Sum64()

	positions := make([]int, d.k)
	for i := 0; i < d.k; i++ {
		combined := sum1 + uint64(i)*sum2
		positions[i] = int(combined % uint64(len(d.bits)))
	}
	return positions
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	bloomcheck "example.com/bloom-filter-membership-checker"
)

func main() {
	d := bloomcheck.New(1024, 3, 2) // 1024 bits, 3 hash rounds, exact window of 2

	items := []string{"user-1", "user-2", "user-1", "user-3", "user-1"}
	for _, item := range items {
		fmt.Printf("%-8s -> %s\n", item, d.Check(item))
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
user-1   -> UNSEEN
user-2   -> UNSEEN
user-1   -> DUPLICATE
user-3   -> UNSEEN
user-1   -> MAYBE_DUPLICATE
```

The last line is the interesting one: `user-1` was seen twice before, but
the exact window (capacity 2) evicted it once `user-3` arrived, so the
third check can only say "maybe" — the Bloom filter still remembers
`user-1`'s bits, but the window that could confirm it exactly has moved
on.

### Tests

`TestCheckUnseenThenExactDuplicate` covers the straightforward path.
`TestCheckMaybeDuplicateAfterWindowEviction` reproduces the demo's eviction
scenario deterministically with a window capacity of 2.
`TestCheckNeverProducesFalseNegative` checks the Bloom filter's core
guarantee: once an item has been recorded, it is never reported `Unseen`
again, regardless of how the exact window's contents evolve.

Create `bloomcheck_test.go`:

```go
package bloomcheck

import "testing"

func TestCheckUnseenThenExactDuplicate(t *testing.T) {
	t.Parallel()

	d := New(1024, 3, 10)

	if got := d.Check("a"); got != Unseen {
		t.Fatalf("first check of %q = %s, want Unseen", "a", got)
	}
	if got := d.Check("b"); got != Unseen {
		t.Fatalf("first check of %q = %s, want Unseen", "b", got)
	}
	if got := d.Check("a"); got != Duplicate {
		t.Fatalf("second check of %q = %s, want Duplicate (still in the exact window)", "a", got)
	}
}

func TestCheckMaybeDuplicateAfterWindowEviction(t *testing.T) {
	t.Parallel()

	d := New(1024, 3, 2) // window only holds 2 items

	if got := d.Check("a"); got != Unseen {
		t.Fatalf("Check(a) = %s, want Unseen", got)
	}
	if got := d.Check("b"); got != Unseen {
		t.Fatalf("Check(b) = %s, want Unseen", got)
	}
	// c evicts a from the exact window (capacity 2: window now holds b, c).
	if got := d.Check("c"); got != Unseen {
		t.Fatalf("Check(c) = %s, want Unseen", got)
	}
	// a is still set in the Bloom filter (Bloom filters never forget), but
	// it fell out of the exact window, so it can only be reported as
	// probabilistic, not certain.
	if got := d.Check("a"); got != MaybeDuplicate {
		t.Fatalf("Check(a) after eviction = %s, want MaybeDuplicate", got)
	}
}

func TestCheckNeverProducesFalseNegative(t *testing.T) {
	t.Parallel()

	d := New(2048, 4, 1)
	items := []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8"}
	for _, item := range items {
		d.Check(item)
	}
	for _, item := range items {
		if got := d.Check(item); got == Unseen {
			t.Errorf("Check(%q) after it was already recorded = Unseen, want Duplicate or MaybeDuplicate (never Unseen again)", item)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The deduplicator is correct when a genuinely unseen item is always
reported `Unseen` (the Bloom filter's no-false-negative guarantee), when
an item still inside the bounded exact window is reported `Duplicate` with
certainty, and when an item the Bloom filter recognizes but the window can
no longer confirm is honestly reported `MaybeDuplicate` rather than forced
into one of the two certain verdicts. Carry this forward: whenever a cheap
probabilistic check and an expensive exact check are combined, the switch
that resolves them should preserve the honest three-way outcome —
certainly yes, certainly no, and genuinely unsure — rather than collapsing
"unsure" into either certainty for the sake of a simpler boolean return
type.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [hash/fnv package](https://pkg.go.dev/hash/fnv) — the hash functions used to compute bit positions.
- [Kirsch & Mitzenmacher: Less Hashing, Same Performance](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf) — the double-hashing technique `positions` implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-transaction-isolation-level-selector.md](30-transaction-isolation-level-selector.md) | Next: [32-vector-clock-ordering-classifier.md](32-vector-clock-ordering-classifier.md)
