# Exercise 28: Bloom Filter Inverted Assumption Misses Collisions

**Nivel: Intermedio** — validacion rapida (un test corto).

A deduplication layer in front of an expensive downstream — an
idempotency check before a payment write, a duplicate-event filter in
front of a notification fanout — reaches for a Bloom filter because it
answers "have I seen this before" in constant time and constant
memory, at the cost of a small, known false-positive rate. The
guarantee only runs in one direction: a miss is certain (the key was
never added, full stop), a hit is only a possibility (every bit the
key hashes to happens to be set, which other keys could have done).
Code that reads a hit as a certainty inverts the one asymmetry the
whole data structure exists to provide, and it fails exactly where it
hurts most — production traffic at real scale, once the filter holds
far more keys than it was sized for and collisions stop being a
theoretical footnote. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
dedup/                       independent module: example.com/bloom-filter-inverted-assumption
  go.mod                      go 1.21
  dedup.go                     Filter, NewFilter, Add, MightContain, Deduper, Seen
  cmd/
    demo/
      main.go                  runnable demo: an undersized filter forced into a real collision
  dedup_test.go                 exact-duplicate case, plus a brute-forced collision edge case
```

- Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
- Implement: `Deduper.Seen(key string) bool` that trusts a Bloom filter miss completely but confirms a filter hit against an exact secondary set before treating it as a real duplicate.
- Test: a same-key-seen-twice sanity case; an edge case that deliberately undersizes the filter to brute-force a genuine hash collision and asserts the colliding, never-added key is still correctly treated as new.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/28-bloom-filter-inverted-assumption/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/28-bloom-filter-inverted-assumption
```

### Why a filter hit is a question, not an answer

The version that ships first treats `MightContain` as if its name
were `Contains`:

```go
// BUG: a Bloom filter hit only means "possibly seen"; treating it as
// certain drops brand-new keys that happen to collide with existing ones.
func (d *Deduper) Seen(key string) bool {
	if d.filter.MightContain(key) {
		return true
	}
	d.filter.Add(key)
	return false
}
```

This is correct for the overwhelming majority of keys, which is
exactly what makes it dangerous: it works in every manual test and in
every staging run with a handful of keys, because collisions are rare
when the filter is nowhere near full. Bloom filters have no false
negatives and a bounded, known rate of false positives — but the rate
is bounded by *capacity relative to the number of keys actually
stored*, and production dedup layers get pushed toward that boundary
by exactly the traffic growth they were built to survive. Once the
filter holds enough keys, a brand-new key's `k` hash positions start
landing on bits that other, unrelated keys already set. The buggy
`Seen` reads that as "already recorded" and returns `true` without
ever calling `Add` — the key is never actually recorded anywhere, and
whatever the caller does with a "true" (skip processing, drop the
event, treat a fresh order as a retried duplicate) happens to a piece
of data that was never really seen before. There is no error, no
panic, and no log line: the event is simply gone, indistinguishable
from a true duplicate from the outside.

The fix keeps the filter's asymmetry intact instead of collapsing it:
a miss is trusted completely (skip the expensive check, this key is
certainly new), and a hit only earns a look at an authoritative exact
set before the code commits to calling something a duplicate.

```go
func (d *Deduper) Seen(key string) bool {
	if !d.filter.MightContain(key) {
		d.record(key)
		return false // definitely new: the filter guarantees no false negatives
	}
	_, alreadySeen := d.exact[key]
	if !alreadySeen {
		d.record(key)
	}
	return alreadySeen
}
```

Now a filter hit costs one map lookup instead of a wrongly dropped
event — a fair trade, since a hit is by construction the rare case the
filter was sized to keep small.

Create `dedup.go`:

```go
package dedup

import "hash/fnv"

// Filter is a fixed-size Bloom filter: a bit array of m bits and k
// hash functions derived from two FNV hashes via double hashing
// (h_i(x) = h1(x) + i*h2(x) mod m), the standard technique for
// generating many hash functions from two without implementing k
// separate hash algorithms.
type Filter struct {
	bits []bool
	m    uint32
	k    int
}

// NewFilter creates a Filter with m bits and k hash functions.
func NewFilter(m uint32, k int) *Filter {
	return &Filter{bits: make([]bool, m), m: m, k: k}
}

func (f *Filter) indices(key string) []uint32 {
	h1 := fnv.New32a()
	h1.Write([]byte(key))
	sum1 := h1.Sum32()

	h2 := fnv.New32()
	h2.Write([]byte(key))
	sum2 := h2.Sum32()

	idx := make([]uint32, f.k)
	for i := 0; i < f.k; i++ {
		idx[i] = (sum1 + uint32(i)*sum2) % f.m
	}
	return idx
}

// Add records key's k positions as set.
func (f *Filter) Add(key string) {
	for _, i := range f.indices(key) {
		f.bits[i] = true
	}
}

// MightContain reports whether key may have been added. false is a
// mathematical guarantee: key was never added. true is only a
// possibility -- every one of the k positions key hashes to happens to be
// set, which can happen because some other combination of added keys set
// them, not necessarily key itself.
func (f *Filter) MightContain(key string) bool {
	for _, i := range f.indices(key) {
		if !f.bits[i] {
			return false
		}
	}
	return true
}

// Deduper wraps a Filter to reject already-seen keys. The Bloom filter is
// used purely as a cheap fast-path negative check: MightContain == false
// is a mathematical guarantee the key was never added, so Seen can skip
// the authoritative exact check entirely. MightContain == true is only a
// possibility -- a hash collision with some other key -- so Seen must
// confirm against the exact set before deciding a key is a duplicate,
// rather than rejecting it outright on a filter hit alone.
type Deduper struct {
	filter *Filter
	exact  map[string]struct{}
}

// NewDeduper creates a Deduper backed by an m-bit, k-hash-function filter.
func NewDeduper(m uint32, k int) *Deduper {
	return &Deduper{filter: NewFilter(m, k), exact: make(map[string]struct{})}
}

// MightContain exposes the underlying filter's raw signal, useful for
// demonstrating (or testing) a hash collision independent of Seen's
// bookkeeping.
func (d *Deduper) MightContain(key string) bool {
	return d.filter.MightContain(key)
}

// Seen reports whether key has already been recorded, recording it if
// not. A filter miss is trusted completely (no false negatives are
// possible); a filter hit is only a signal to check the authoritative
// exact set before concluding the key is really a duplicate.
func (d *Deduper) Seen(key string) bool {
	if !d.filter.MightContain(key) {
		d.record(key)
		return false // definitely new: the filter guarantees no false negatives
	}

	_, alreadySeen := d.exact[key]
	if !alreadySeen {
		d.record(key)
	}
	return alreadySeen
}

func (d *Deduper) record(key string) {
	d.filter.Add(key)
	d.exact[key] = struct{}{}
}
```

### The runnable demo

The demo deliberately undersizes the filter (32 bits, 2 hash
functions) so a handful of insertions is enough to force a genuine
collision, then searches for a never-added key that the filter
nonetheless reports as a hit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bloom-filter-inverted-assumption"
)

func main() {
	// A deliberately small, undersized filter (32 bits, 2 hash functions)
	// makes collisions easy to hit after only a handful of insertions --
	// exactly the pressure a production filter comes under once it holds
	// far more keys than it was sized for.
	d := dedup.NewDeduper(32, 2)

	processed := []string{"order-1001", "order-1002", "order-1003"}
	for _, id := range processed {
		fmt.Printf("%s seen=%v (first time processing it)\n", id, d.Seen(id))
	}

	// Search for a brand-new order ID whose Bloom positions all happen to
	// already be set by the orders above -- a genuine hash collision, not
	// a duplicate.
	var collision string
	for i := 2000; i < 3000; i++ {
		candidate := fmt.Sprintf("order-%d", i)
		if d.MightContain(candidate) {
			collision = candidate
			break
		}
	}

	if collision == "" {
		fmt.Println("no collision found in the search range")
		return
	}

	fmt.Printf("found colliding candidate: %s (never added, but its Bloom bits are all set)\n", collision)
	fmt.Printf("%s seen=%v (must be false: it is genuinely new)\n", collision, d.Seen(collision))
	fmt.Printf("%s seen=%v (now it really has been recorded)\n", collision, d.Seen(collision))
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
order-1001 seen=false (first time processing it)
order-1002 seen=false (first time processing it)
order-1003 seen=false (first time processing it)
found colliding candidate: order-2003 (never added, but its Bloom bits are all set)
order-2003 seen=false (must be false: it is genuinely new)
order-2003 seen=true (now it really has been recorded)
```

### Tests

`TestSeenTracksExactDuplicatesWithoutCollision` is the basic sanity
case with a filter large enough that a collision is very unlikely.
`TestSeenSurvivesHashCollisionOnANeverAddedKey` is the edge case that
matters: it deliberately undersizes the filter, brute-forces a real
colliding key, and asserts that key is still treated as new — the
exact scenario the inverted assumption gets wrong.

Create `dedup_test.go`:

```go
package dedup

import (
	"fmt"
	"testing"
)

func TestSeenTracksExactDuplicatesWithoutCollision(t *testing.T) {
	d := NewDeduper(1024, 4) // large enough that a collision is very unlikely here

	if d.Seen("event-A") {
		t.Fatal("Seen(event-A) = true on first sight, want false")
	}
	if !d.Seen("event-A") {
		t.Fatal("Seen(event-A) = false on second sight, want true")
	}
	if d.Seen("event-B") {
		t.Fatal("Seen(event-B) = true for a never-seen key, want false")
	}
}

// TestSeenSurvivesHashCollisionOnANeverAddedKey is the core scenario: a
// deliberately undersized filter forces a real hash collision -- a key
// that was never added, but whose Bloom bits are all set because of other
// keys -- and asserts the collision is still correctly treated as new,
// not dropped as a false duplicate.
func TestSeenSurvivesHashCollisionOnANeverAddedKey(t *testing.T) {
	d := NewDeduper(24, 2)

	for i := 0; i < 5; i++ {
		d.Seen(fmt.Sprintf("order-%d", i))
	}

	var collision string
	for i := 1000; i < 10000; i++ {
		candidate := fmt.Sprintf("order-%d", i)
		if d.MightContain(candidate) {
			collision = candidate
			break
		}
	}
	if collision == "" {
		t.Fatal("could not find a colliding candidate to test with; widen the search range")
	}

	if d.Seen(collision) {
		t.Fatalf("Seen(%q) = true for a key that was never added (only its Bloom bits collided), want false", collision)
	}
	if !d.Seen(collision) {
		t.Fatalf("Seen(%q) = false the second time, want true now that it is genuinely recorded", collision)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`Seen` is correct when a filter miss is always trusted (the only
guarantee the data structure actually makes) and a filter hit is
always confirmed against an authoritative set before being reported as
a duplicate — proven with a test that manufactures a real collision
instead of hoping one shows up under normal random keys. The mistake
this design avoids is reading a probabilistic data structure's "maybe"
as a "yes": a Bloom filter's entire value proposition is a fast,
cheap, always-correct "no," paired with a rare, deliberately
inexpensive-to-double-check "maybe" — collapsing the "maybe" into a
"yes" throws away the one thing that made using a probabilistic
structure safe in the first place. The fix costs one map lookup on the
rare hit path and, in exchange, makes the false-positive rate a tuning
parameter for the filter's size instead of a source of silent data
loss for the whole pipeline.

## Resources

- [Bloom filter (Wikipedia)](https://en.wikipedia.org/wiki/Bloom_filter) — the false-positive/no-false-negative guarantee and the effect of undersizing relative to inserted elements.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV hash implementations used here to derive k independent-enough hash functions via double hashing.
- [Kirsch & Mitzenmacher, "Less Hashing, Same Performance: Building a Better Bloom Filter"](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf) — the double-hashing technique for generating k hash functions from two.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-token-bucket-refill-concurrent-race.md](27-token-bucket-refill-concurrent-race.md) | Next: [29-dns-cache-ttl-expiry-ignored.md](29-dns-cache-ttl-expiry-ignored.md)
