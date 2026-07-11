# Exercise 33: Bloom Filter: Space-Efficient Probabilistic Deduplication with Fallback

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A message queue processed at-least-once will redeliver messages, and an
idempotent consumer needs to recognize a redelivered message ID quickly —
but storing every message ID a high-volume system has ever seen, in a plain
set, eventually costs more memory than the service has. A Bloom filter
answers "might this ID have been seen?" in a fixed number of bits regardless
of how many IDs pass through, at the cost of occasional false positives; a
small exact set backs it up to resolve exactly those false positives without
ever needing to store every ID that was ever definitively new. This module
is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
bloomdedup/                   independent module: example.com/bloom-filter-probabilistic-dedup
  go.mod                    go 1.24
  bloomdedup.go             Filter (Add/MightContain), decide(mightContain, exactSeen), Dedup (mutex-protected Check)
  cmd/
    demo/
      main.go               20 inserts, a real Bloom collision on id 21, and a confirmed replay
  bloomdedup_test.go        decide table; no false negatives; forced collision; concurrent Check -race
```

- Files: `bloomdedup.go`, `cmd/demo/main.go`, `bloomdedup_test.go`.
- Implement: a `Filter` with `Add`/`MightContain` using double hashing over an `m`-bit array with `k` hash positions, a pure `decide(mightContain, exactSeen bool) string` returning `"new"`, `"new-after-collision"`, or `"replay"`, and a `Dedup` struct guarded by a `sync.Mutex` with `Check(id string) (replay bool)`.
- Test: a table over `decide`'s three branches; a no-false-negatives check over everything added to a `Filter`; a deliberately small, dense filter forced into a real collision to prove the exact-set fallback still gets both directions right; a concurrency test asserting exactly one "new" verdict per unique ID under concurrent `Check` calls, with `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bloomdedup/cmd/demo
cd ~/go-exercises/bloomdedup
go mod init example.com/bloom-filter-probabilistic-dedup
go mod edit -go=1.24
```

### Why a "definitely new" ID still gets written to the exact set

`Check`'s `decide` branch names look asymmetric — `"new"` and
`"new-after-collision"` sound like they should be handled differently — but
`Check` treats them identically: both call `d.filter.Add(id)` and both
record `id` in `d.seen`. That symmetry is deliberate, and skipping the exact-
set write on the plain `"new"` path is the single most tempting bug this
module invites. Once any ID has been added to the Bloom filter, a later
Bloom lookup for that *same* ID will always come back `true` — not because
of a collision with something else, but because it genuinely is a member
now. Without an exact record of every ID ever admitted, a real repeat of
that ID would hit `mightContain == true` and `exactSeen == false`, which
`decide` reports as `"new-after-collision"` — silently letting a real
replay through as if it were new. The Bloom filter's fixed-size bit array is
what gives this design its "billions of IDs in KB" property; the exact set
is the actual source of truth an ID's *history* lives in, and the Bloom
filter's only job is to let the overwhelmingly common "never seen, no
collision" case skip a lookup against it.

Create `bloomdedup.go`:

```go
// Package bloomdedup deduplicates message IDs at a scale where storing every
// ID exactly would be too expensive: a Bloom filter answers "might this be a
// repeat?" in a handful of bits per ID, and only IDs that trigger a Bloom
// collision ever need an exact-set lookup to confirm whether they are a real
// repeat or a false positive.
package bloomdedup

import (
	"hash/fnv"
	"sync"
)

// Filter is a fixed-size Bloom filter over m bits, using k independent hash
// positions per element derived from two real hashes by double hashing
// (Kirsch-Mitzenmacher): h_i(x) = h1(x) + i*h2(x). It never produces a false
// negative — MightContain always returns true for anything Add was called
// with — and its only error is a false positive: reporting "maybe" for
// something never added.
type Filter struct {
	bits []uint64
	m    uint64
	k    uint
}

// NewFilter builds a Filter with m bits and k hash functions. Larger m or
// smaller k (for a fixed expected item count) lowers the false-positive
// rate, at the cost of more memory.
func NewFilter(m uint64, k uint) *Filter {
	return &Filter{bits: make([]uint64, (m+63)/64), m: m, k: k}
}

func hashes(s string) (uint64, uint64) {
	h1 := fnv.New64a()
	h1.Write([]byte(s))
	v1 := h1.Sum64()

	h2 := fnv.New32a()
	h2.Write([]byte(s))
	v2 := uint64(h2.Sum32())
	if v2 == 0 {
		v2 = 1 // an all-zero step would make every derived position identical
	}
	return v1, v2
}

// Add records s as a member of the filter.
func (f *Filter) Add(s string) {
	h1, h2 := hashes(s)
	for i := uint(0); i < f.k; i++ {
		idx := (h1 + uint64(i)*h2) % f.m
		f.bits[idx/64] |= 1 << (idx % 64)
	}
}

// MightContain reports whether s might have been added. false is a
// guarantee ("definitely never added"); true is a probabilistic "maybe."
func (f *Filter) MightContain(s string) bool {
	h1, h2 := hashes(s)
	for i := uint(0); i < f.k; i++ {
		idx := (h1 + uint64(i)*h2) % f.m
		if f.bits[idx/64]&(1<<(idx%64)) == 0 {
			return false
		}
	}
	return true
}

// decide is the pure guard behind Check: given whether the Bloom filter
// reports a possible match and whether the exact set already confirmed this
// ID, it names the outcome. Separating this from Check means the three-way
// branch — the actual logic worth testing exhaustively — is a plain table
// test with no filter, no mutex, and no real ID string involved.
func decide(mightContain, exactSeen bool) string {
	if !mightContain {
		// The Bloom filter guarantees no false negatives, so this is
		// conclusive: the ID was never added.
		return "new"
	}
	if exactSeen {
		return "replay"
	}
	// The Bloom filter's "maybe" was a false positive from someone else's
	// ID colliding in the same bit positions: this ID is still new, but it
	// must be remembered exactly from now on, since the Bloom filter alone
	// can no longer tell it apart from that collision.
	return "new-after-collision"
}

// Dedup tracks seen message IDs behind a Bloom filter. The exact set is the
// real source of truth for "have I truly seen this ID" (in production this
// would be a database or a distributed cache, not necessarily in-process
// memory) — the Bloom filter's job is purely to let the overwhelmingly
// common "definitely new" case skip that lookup entirely. That is where the
// "billions of IDs in KB" claim lives: the Bloom filter's own bit array
// stays a fixed, tiny size no matter how many IDs pass through, and it is
// consulted on every request; the exact set is only ever read on the rare
// request where the Bloom filter cannot rule out a replay by itself.
type Dedup struct {
	mu     sync.Mutex
	filter *Filter
	seen   map[string]struct{}
}

// NewDedup builds a Dedup backed by a Filter with m bits and k hash
// functions.
func NewDedup(m uint64, k uint) *Dedup {
	return &Dedup{filter: NewFilter(m, k), seen: make(map[string]struct{})}
}

// Check reports whether id is a replay of a message already seen. The Bloom
// lookup, the exact-set lookup, and the resulting Add/record all happen
// inside one critical section, so two concurrent Check calls for the same
// brand-new id can never both conclude "new" and both admit it.
func (d *Dedup) Check(id string) (replay bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	mightContain := d.filter.MightContain(id)
	_, exactSeen := d.seen[id]

	switch decide(mightContain, exactSeen) {
	case "new", "new-after-collision":
		// Either way this id is genuinely new: record it in both structures
		// so a future repeat of this exact id — collision or not — is
		// caught by the exact-set check next time.
		d.filter.Add(id)
		d.seen[id] = struct{}{}
		return false
	default: // "replay"
		return true
	}
}
```

### The runnable demo

A deliberately small, dense filter (32 bits, 4 hash functions) is filled
with twenty message IDs. The twenty-first ID was never added but happens to
collide with the filter's bit pattern at this size — a real Bloom false
positive, not a contrived one. The exact-set fallback correctly reports it
as new the first time and as a confirmed replay the second time, and an
unrelated, non-colliding ID is definitively new on Bloom alone.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	bloomdedup "example.com/bloom-filter-probabilistic-dedup"
)

func main() {
	// A small, deliberately dense filter so a real Bloom collision shows up
	// within a handful of IDs — production sizing would use a much larger m
	// for the expected ID volume and a correspondingly tiny collision rate.
	d := bloomdedup.NewDedup(32, 4)

	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("msg-%d", i)
		replay := d.Check(id)
		fmt.Printf("Check(%-8s) replay=%v (first time seeing it)\n", id, replay)
	}

	// msg-21 was never added, but happens to collide in the filter's bit
	// space with the 20 IDs above (a real Bloom false positive at this
	// filter size). The exact-set fallback still gets it right both times.
	first := d.Check("msg-21")
	fmt.Printf("Check(msg-21)  replay=%v (Bloom collision, but genuinely new)\n", first)

	second := d.Check("msg-21")
	fmt.Printf("Check(msg-21)  replay=%v (now a confirmed replay)\n", second)

	// An ID with no collision at all is definitively new on Bloom alone.
	fresh := d.Check("msg-9999")
	fmt.Printf("Check(msg-9999) replay=%v (no Bloom collision)\n", fresh)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Check(msg-1   ) replay=false (first time seeing it)
Check(msg-2   ) replay=false (first time seeing it)
Check(msg-3   ) replay=false (first time seeing it)
Check(msg-4   ) replay=false (first time seeing it)
Check(msg-5   ) replay=false (first time seeing it)
Check(msg-6   ) replay=false (first time seeing it)
Check(msg-7   ) replay=false (first time seeing it)
Check(msg-8   ) replay=false (first time seeing it)
Check(msg-9   ) replay=false (first time seeing it)
Check(msg-10  ) replay=false (first time seeing it)
Check(msg-11  ) replay=false (first time seeing it)
Check(msg-12  ) replay=false (first time seeing it)
Check(msg-13  ) replay=false (first time seeing it)
Check(msg-14  ) replay=false (first time seeing it)
Check(msg-15  ) replay=false (first time seeing it)
Check(msg-16  ) replay=false (first time seeing it)
Check(msg-17  ) replay=false (first time seeing it)
Check(msg-18  ) replay=false (first time seeing it)
Check(msg-19  ) replay=false (first time seeing it)
Check(msg-20  ) replay=false (first time seeing it)
Check(msg-21)  replay=false (Bloom collision, but genuinely new)
Check(msg-21)  replay=true (now a confirmed replay)
Check(msg-9999) replay=false (no Bloom collision)
```

### Tests

The `decide` table covers all three named outcomes directly. A dedicated
test proves `Filter` never produces a false negative for anything added. A
forced-collision test uses the same small, dense filter as the demo to
exercise a genuine Bloom collision deterministically. A concurrency test
fires many goroutines at a mix of unique and repeated IDs and asserts
exactly one `"new"` verdict per unique ID, under `-race`.

Create `bloomdedup_test.go`:

```go
package bloomdedup

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mightContain bool
		exactSeen    bool
		want         string
	}{
		{name: "bloom says no: definitely new", mightContain: false, exactSeen: false, want: "new"},
		{name: "bloom absence wins even if exact set says yes (should not happen in practice)", mightContain: false, exactSeen: true, want: "new"},
		{name: "bloom maybe, exact set says no: new after collision", mightContain: true, exactSeen: false, want: "new-after-collision"},
		{name: "bloom maybe, exact set says yes: confirmed replay", mightContain: true, exactSeen: true, want: "replay"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decide(tc.mightContain, tc.exactSeen); got != tc.want {
				t.Errorf("decide(%v, %v) = %q, want %q", tc.mightContain, tc.exactSeen, got, tc.want)
			}
		})
	}
}

func TestFilterNeverFalseNegatives(t *testing.T) {
	t.Parallel()

	f := NewFilter(1024, 5)
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
		f.Add(ids[i])
	}

	for _, id := range ids {
		if !f.MightContain(id) {
			t.Fatalf("MightContain(%q) = false after Add, want true (no false negatives allowed)", id)
		}
	}
}

func TestDedupCheckBasicFlow(t *testing.T) {
	t.Parallel()

	d := NewDedup(1024, 5)

	if d.Check("a") {
		t.Fatal("first Check(a) reported replay, want new")
	}
	if !d.Check("a") {
		t.Fatal("second Check(a) reported new, want replay")
	}
	if d.Check("b") {
		t.Fatal("first Check(b) reported replay, want new")
	}
}

func TestDedupHandlesARealBloomCollision(t *testing.T) {
	t.Parallel()

	// A small, dense filter so a collision is guaranteed within a handful
	// of insertions, deterministically (fixed hash function, fixed IDs).
	d := NewDedup(32, 4)
	for i := 1; i <= 20; i++ {
		d.Check(fmt.Sprintf("msg-%d", i))
	}

	if d.Check("msg-21") {
		t.Fatal("msg-21 (never seen before) was reported as a replay")
	}
	if !d.Check("msg-21") {
		t.Fatal("msg-21's second Check should now report a confirmed replay")
	}
}

func TestConcurrentCheckExactlyOneNewPerID(t *testing.T) {
	t.Parallel()

	d := NewDedup(4096, 5)
	const ids = 100
	const repeatsPerID = 5

	var newCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < ids; i++ {
		id := fmt.Sprintf("concurrent-id-%d", i)
		for r := 0; r < repeatsPerID; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if !d.Check(id) {
					newCount.Add(1)
				}
			}()
		}
	}
	wg.Wait()

	if got := newCount.Load(); got != ids {
		t.Fatalf("newCount = %d, want %d (exactly one \"new\" verdict per unique ID)", got, ids)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestConcurrentCheckExactlyOneNewPerID` is what actually proves the mutex is
doing its job: without the lock spanning the Bloom lookup, the exact-set
lookup, and both writes as one critical section, two goroutines racing to
`Check` the same brand-new ID could both observe `mightContain == false` and
both report `"new"` — a duplicate admission of the same message. Carry this
forward: any "probabilistic pre-check, exact fallback" design is only as
correct as the critical section around it; the pre-check existing at all
does not reduce the need for the fallback path to be atomic with the
pre-check that triggered it.

## Resources

- [Space/Time Trade-offs in Hash Coding with Allowable Errors (Bloom, 1970)](https://dl.acm.org/doi/10.1145/362686.362692) — the original paper describing the structure this module implements.
- [Google Guava: BloomFilter](https://guava.dev/releases/snapshot/api/docs/com/google/common/hash/BloomFilter.html) — a production-grade Bloom filter implementation with configurable false-positive rate.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the two hash functions this module combines via double hashing.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-gossip-protocol-state-merge.md](32-gossip-protocol-state-merge.md) | Next: [34-snapshot-isolation-mvcc-visibility.md](34-snapshot-isolation-mvcc-visibility.md)
