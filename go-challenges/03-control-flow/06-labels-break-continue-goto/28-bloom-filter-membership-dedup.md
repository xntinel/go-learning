# Exercise 28: Bloom filter approximate membership for deduplication

**Nivel: Intermedio** — validacion rapida (un test corto).

A stream deduplicator sitting in front of an idempotency-sensitive sink
cannot afford to remember every ID it has ever seen — that is an
ever-growing set with no bound, the exact problem a Bloom filter exists to
avoid. A Bloom filter answers "have I seen this?" in constant space at the
cost of occasional false positives (it may say yes to something new), never
false negatives (a no is always correct). That asymmetry means a filter
alone is unsafe to trust blindly: a false positive would silently and
permanently drop a legitimate, never-before-seen record. This module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
bloomdedup/                 independent module: example.com/bloomdedup
  go.mod                     go 1.24
  bloomdedup.go                Filter, Deduper; Add, MightContain, Dedup
  cmd/
    demo/
      main.go                runnable demo: redelivered stream events + measured false-positive rate
  bloomdedup_test.go           table test: no false negatives, bounded false-positive rate, exact duplicates within the confirm window, no duplicates, empty stream
```

- Files: `bloomdedup.go`, `cmd/demo/main.go`, `bloomdedup_test.go`.
- Implement: `Filter.Add`/`MightContain` (a Bloom filter over `m` bits with `k` hash functions), and `Deduper.Dedup(ids []string) (kept, duplicates []string)`, treating a Bloom hit as a genuine duplicate only when it is confirmed against a small exact-match ring buffer of recently seen IDs.
- Test: every added item still tests positive (no false negatives are possible), an empirically measured false-positive rate against never-added items stays well under a generous bound, an exact duplicate within the confirmation window is caught, a stream with no duplicates keeps everything, and an empty stream.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the confirmed-duplicate case needs a labeled continue, not a bare one

`Dedup` walks the incoming IDs in an outer loop. For each one whose Bloom
test comes back positive, an inner loop scans a small ring buffer of the
most recently *confirmed* IDs, looking for an exact match — because a
positive Bloom test is only ever a suspicion, never proof, and a stream
deduplicator that treated every suspicion as certain would occasionally drop
a record that was never actually seen before. The confirmation buffer is
deliberately bounded: real duplicate delivery (message-queue redelivery,
client retry storms after a timeout) happens within a short window, so
remembering only the last few dozen or hundred confirmed IDs is enough in
practice, without storing every ID ever processed — which would defeat the
whole reason for using a Bloom filter in the first place.

The moment the inner loop finds an exact match, the ID is a *confirmed*
duplicate, and two things must happen together: record it as a duplicate,
and skip the unconditional "add to the filter and keep it" logic that
follows the inner loop, back in the outer loop's body. `continue records`,
fired from inside the confirmation loop, does exactly that in one statement.
A bare `continue` there would only advance to the next entry in the
confirmation buffer — and once that inner loop naturally ran out of entries
to check, control would fall through to the trailing code and silently
re-admit an ID the code had just finished proving was a duplicate. This is
the same shape as staging a batch's records and abandoning the whole batch
on one bad entry (see exercise 27): the correct fix is not a sentinel flag
checked after the loop, it is jumping straight past the trailing logic with
a named `continue`.

Create `bloomdedup.go`:

```go
package bloomdedup

import "hash/fnv"

// Filter is a Bloom filter over m bits using k independent hash functions,
// derived from two FNV hashes via double hashing (Kirsch-Mitzenmacher): a
// space-efficient probabilistic set that never false-negatives (MightContain
// returning false is certain) but can false-positive on items never added.
type Filter struct {
	bits []bool
	m    uint64
	k    int
}

// NewFilter builds a Filter with m bits and k hash functions per item.
func NewFilter(m uint64, k int) *Filter {
	return &Filter{bits: make([]bool, m), m: m, k: k}
}

func (f *Filter) positions(item string) []uint64 {
	h1, h2 := hash2(item)
	pos := make([]uint64, f.k)
	for i := range f.k {
		pos[i] = (h1 + uint64(i)*h2) % f.m
	}
	return pos
}

// Add sets every bit position derived from item.
func (f *Filter) Add(item string) {
	for _, p := range f.positions(item) {
		f.bits[p] = true
	}
}

// MightContain reports whether every bit position derived from item is
// already set. False is certain (item was never added); true may be a
// false positive caused by another item's bits overlapping item's.
func (f *Filter) MightContain(item string) bool {
	for _, p := range f.positions(item) {
		if !f.bits[p] {
			return false
		}
	}
	return true
}

func hash2(s string) (uint64, uint64) {
	h1 := fnv.New64a()
	h1.Write([]byte(s))
	sum1 := h1.Sum64()

	h2 := fnv.New32a()
	h2.Write([]byte(s))
	h2.Write([]byte{0xff})           // perturb so the second hash differs from the first
	sum2 := uint64(h2.Sum32())*2 + 1 // force odd, so it can reach every slot mod m

	return sum1, sum2
}

// Deduper wraps a Bloom filter with a small exact-match confirmation ring
// buffer. The filter alone is not enough: a false positive on a brand-new ID
// would silently drop it forever, and in a real stream that ID might be
// perfectly legitimate, just unlucky enough to collide with an earlier one's
// bit pattern. The confirmation buffer holds the most recent confirmWindow
// IDs actually seen, so a Bloom hit only counts as a genuine duplicate when
// it is ALSO found there. Real duplicate delivery (message-queue redelivery,
// client retry storms) happens within a short window, so bounding the exact
// buffer's size keeps confirmation O(1) memory instead of storing every ID
// ever seen -- which would defeat the entire point of using a Bloom filter.
type Deduper struct {
	filter *Filter
	recent []string // ring buffer of the confirmWindow most recently seen IDs
	next   int
}

// NewDeduper builds a Deduper backed by an m-bit, k-hash Bloom filter and an
// exact-match confirmation window of the given size.
func NewDeduper(m uint64, k, confirmWindow int) *Deduper {
	return &Deduper{
		filter: NewFilter(m, k),
		recent: make([]string, 0, confirmWindow),
	}
}

func (d *Deduper) remember(id string) {
	if len(d.recent) < cap(d.recent) {
		d.recent = append(d.recent, id)
		return
	}
	d.recent[d.next] = id
	d.next = (d.next + 1) % len(d.recent)
}

// Dedup processes a stream of record IDs in order, returning the ones kept
// (added to the filter and forwarded downstream) and the ones identified as
// duplicates (dropped). For each id whose Bloom test is positive, the
// confirmation buffer is scanned for an exact match. The moment one is
// found, the id is a CONFIRMED duplicate, and continue records -- fired from
// inside the confirmation loop, one level below the records loop it names --
// skips straight to the next id, bypassing the add-to-filter-and-keep logic
// entirely. A bare continue there would only advance to the next entry of
// the confirmation buffer; once that inner loop ran out of entries to check,
// control would fall through to the unconditional keep logic below and
// silently re-admit a duplicate the code had just finished confirming.
func (d *Deduper) Dedup(ids []string) (kept, duplicates []string) {
records:
	for _, id := range ids {
		if d.filter.MightContain(id) {
			for _, r := range d.recent {
				if r == id {
					duplicates = append(duplicates, id)
					continue records
				}
			}
			// Bloom said maybe, but the confirmation window disagrees:
			// a false positive. Fall through and treat id as new.
		}
		d.filter.Add(id)
		d.remember(id)
		kept = append(kept, id)
	}
	return kept, duplicates
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bloomdedup"
)

func main() {
	d := bloomdedup.NewDeduper(2048, 4, 8)

	// A stream with a message-queue-style redelivery: evt-3 arrives twice in
	// a row, evt-1 arrives again a bit later, still within the confirm window.
	stream := []string{
		"evt-1", "evt-2", "evt-3", "evt-3", "evt-4", "evt-1", "evt-5",
	}

	kept, duplicates := d.Dedup(stream)
	fmt.Println("kept:", kept)
	fmt.Println("duplicates:", duplicates)

	// Empirically measure the false-positive rate: query the filter with
	// IDs that were never added and count how many it reports as present.
	f := bloomdedup.NewFilter(8192, 4)
	for i := range 500 {
		f.Add(fmt.Sprintf("added-%d", i))
	}
	falsePositives := 0
	const probes = 2000
	for i := range probes {
		if f.MightContain(fmt.Sprintf("never-added-%d", i)) {
			falsePositives++
		}
	}
	fmt.Printf("false positives: %d/%d\n", falsePositives, probes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
kept: [evt-1 evt-2 evt-3 evt-4 evt-5]
duplicates: [evt-3 evt-1]
false positives: 11/2000
```

`evt-3`'s second arrival and `evt-1`'s later re-arrival are both caught as
confirmed duplicates, in the order they occurred in the stream. With `m =
8192` bits, `k = 4` hash functions, and 500 items added, the theoretical
false-positive rate is `(1 - e^(-kn/m))^k ≈ 0.2%`; the measured 11/2000
(0.55%) is in the same ballpark, since FNV-derived hash positions are not
perfectly uniform.

### Tests

`TestFilterNeverFalseNegatives` proves the one guarantee a Bloom filter
must never break. `TestFilterFalsePositiveRateStaysBounded` measures the
empirical rate against never-added items and asserts it stays well under a
generous bound, catching a badly undersized filter or a broken hash
function without flaking on ordinary hash-distribution noise.
`TestDedupConfirmsExactDuplicatesWithinWindow` is the core `Dedup` case;
`TestDedupNoDuplicatesKeepsEverything` and `TestDedupEmptyStream` round out
the table.

Create `bloomdedup_test.go`:

```go
package bloomdedup

import (
	"fmt"
	"slices"
	"testing"
)

func TestFilterNeverFalseNegatives(t *testing.T) {
	t.Parallel()

	f := NewFilter(1024, 5)
	added := make([]string, 0, 200)
	for i := range 200 {
		id := fmt.Sprintf("item-%d", i)
		f.Add(id)
		added = append(added, id)
	}
	for _, id := range added {
		if !f.MightContain(id) {
			t.Fatalf("MightContain(%q) = false, want true for an added item (false negatives are impossible)", id)
		}
	}
}

func TestFilterFalsePositiveRateStaysBounded(t *testing.T) {
	t.Parallel()

	f := NewFilter(8192, 4)
	for i := range 500 {
		f.Add(fmt.Sprintf("added-%d", i))
	}

	falsePositives := 0
	const probes = 2000
	for i := range probes {
		if f.MightContain(fmt.Sprintf("never-added-%d", i)) {
			falsePositives++
		}
	}
	// With m=8192, k=4, n=500 the expected false-positive rate is roughly
	// (1-e^(-kn/m))^k =~ 0.2%. Assert a generous upper bound so the test does
	// not flake on hash distribution noise while still catching a filter
	// that is badly undersized or miscomputing hash positions.
	if falsePositives > probes/10 {
		t.Fatalf("false positives = %d/%d, want well under %d (10%%)", falsePositives, probes, probes/10)
	}
}

func TestDedupConfirmsExactDuplicatesWithinWindow(t *testing.T) {
	t.Parallel()

	d := NewDeduper(2048, 4, 8)
	stream := []string{"evt-1", "evt-2", "evt-3", "evt-3", "evt-4", "evt-1", "evt-5"}

	kept, duplicates := d.Dedup(stream)

	wantKept := []string{"evt-1", "evt-2", "evt-3", "evt-4", "evt-5"}
	wantDuplicates := []string{"evt-3", "evt-1"}
	if !slices.Equal(kept, wantKept) {
		t.Fatalf("kept = %v, want %v", kept, wantKept)
	}
	if !slices.Equal(duplicates, wantDuplicates) {
		t.Fatalf("duplicates = %v, want %v", duplicates, wantDuplicates)
	}
}

func TestDedupNoDuplicatesKeepsEverything(t *testing.T) {
	t.Parallel()

	d := NewDeduper(2048, 4, 8)
	stream := []string{"a", "b", "c", "d"}

	kept, duplicates := d.Dedup(stream)
	if !slices.Equal(kept, stream) {
		t.Fatalf("kept = %v, want %v", kept, stream)
	}
	if duplicates != nil {
		t.Fatalf("duplicates = %v, want none", duplicates)
	}
}

func TestDedupEmptyStream(t *testing.T) {
	t.Parallel()

	d := NewDeduper(1024, 3, 4)
	kept, duplicates := d.Dedup(nil)
	if kept != nil || duplicates != nil {
		t.Fatalf("Dedup(nil) = (%v, %v), want (nil, nil)", kept, duplicates)
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The deduplicator is correct when a genuine duplicate — one that is both a
Bloom hit and an exact match in the confirmation buffer — is dropped and
never re-added to the filter, while a Bloom false positive on a truly new ID
still gets kept. The bug this exercise guards against is treating a Bloom
hit as sufficient proof on its own, or worse, using a bare `continue` inside
the confirmation loop and letting control fall through to the unconditional
keep logic after the inner loop exhausts its entries — silently re-admitting
an ID the code had just confirmed as a duplicate. `TestFilterNeverFalseNegatives`
pins the one absolute guarantee a Bloom filter provides; everything else in
the design exists to manage the one guarantee it does not.

## Resources

- [Space/Time Trade-offs in Hash Coding with Allowable Errors (Bloom, 1970)](https://dl.acm.org/doi/10.1145/362686.362692) — the original paper.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the hash functions used to derive bit positions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-wal-compaction-grooming.md](27-wal-compaction-grooming.md) | Next: [29-admission-control-load-shedding.md](29-admission-control-load-shedding.md)
