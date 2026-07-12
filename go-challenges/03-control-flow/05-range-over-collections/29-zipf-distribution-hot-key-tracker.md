# Exercise 29: Zipf Distribution Hot-Key Identification from Event Stream

**Nivel: Intermedio** — validacion rapida (un test corto).

Production access patterns are almost never uniform: a Zipf distribution —
where the second most popular key gets roughly half the traffic of the
first, the third gets a third, and so on — describes cache hit patterns,
CDN object popularity, and per-tenant API load with uncanny accuracy. The
operational consequence is that a small handful of keys can be responsible
for the majority of traffic while a long tail of keys sees almost none, and
treating every key identically wastes cache capacity on the tail while
leaving the head under-protected. This module streams access events through
a counter map, ranges that map to rank keys by frequency and compute each
key's cumulative share of total traffic, and identifies the minimal set of
"hot" keys that account for a target fraction of load — the keys worth
pinning in a hot cache tier or wrapping in a per-key circuit breaker. The
module is fully self-contained: its own `go mod init`, no external
dependencies.

## What you'll build

```text
hotkeys/                    independent module: example.com/zipf-distribution-hot-key-tracker
  go.mod                    go 1.24
  hotkeys.go                type Tracker; Record, Rank, HotKeys
  cmd/
    demo/
      main.go               runnable demo: 2000 Zipf-distributed accesses over 50 keys
  hotkeys_test.go            table test: ranking order + hot-key threshold cases
```

- Files: `hotkeys.go`, `cmd/demo/main.go`, `hotkeys_test.go`.
- Implement: `Tracker.Record`, `Tracker.Rank`, and the package function
  `HotKeys(ranks []KeyRank, threshold float64) []string`.
- Test: a ranking-order case and a table covering an empty tracker, a single
  dominant key, and a threshold that requires every key.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/29-zipf-distribution-hot-key-tracker/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/29-zipf-distribution-hot-key-tracker
go mod edit -go=1.24
```

### Two ranges over the same data: one to sort, one to accumulate

`Rank` cannot compute a cumulative share while it is still ranging the raw
`t.counts` map, because a map's range order is randomized and cumulative
share only means something over a frequency-sorted sequence — you have to
know every key's final position before you can say what fraction of traffic
the keys ranked above it represent. So `Rank` ranges the map exactly once to
copy `(key, count)` pairs into a plain slice (also computing `total` in the
same pass, since that requires no ordering), sorts that slice by count
descending, and only then ranges the *sorted* slice a second time to build
each `KeyRank` with a running `cumulative` total divided by `total`. Trying
to fold both steps into a single map range is not just awkward, it is
impossible: you cannot know a key's cumulative share until you know the
sorted order it belongs to, and the map itself has no order to give you.

`HotKeys` takes advantage of `Rank`'s sorted output to do its own job with a
single range and an early `break`: because `ranks` is already ordered
highest-count first, the first key whose `CumulativeShare` reaches
`threshold` is, by construction, the last key needed to explain that
fraction of all traffic — every key before it in the slice contributed more
individually, so there is no need to keep scanning once the threshold is
crossed. That early exit is what turns "find the hot keys" from an O(n) scan
with no early exit into one that stops the moment the answer is known, which
matters when `ranks` covers a production key space that is far larger than
50.

Create `hotkeys.go`:

```go
package hotkeys

import "sort"

// Tracker counts accesses per key from a live event stream.
type Tracker struct {
	counts map[string]int
}

// New builds an empty Tracker.
func New() *Tracker {
	return &Tracker{counts: make(map[string]int)}
}

// Record registers one access to key.
func (t *Tracker) Record(key string) {
	t.counts[key]++
}

// KeyRank is one key's position in the access-frequency ranking:
// CumulativeShare is the fraction of all recorded accesses attributable to
// this key and every key ranked above it.
type KeyRank struct {
	Key             string
	Count           int
	CumulativeShare float64
}

// Rank ranges the tracker's counts to build a frequency-sorted ranking
// (highest count first, key ascending to break ties), then ranges that
// sorted order once more to accumulate each key's running share of total
// traffic. Real-world access patterns are Zipf-distributed — a handful of
// keys account for most traffic, with a long thin tail — so CumulativeShare
// tends to climb steeply for the first few keys and then flatten out.
func (t *Tracker) Rank() []KeyRank {
	type kv struct {
		key   string
		count int
	}
	entries := make([]kv, 0, len(t.counts))
	total := 0
	for k, c := range t.counts {
		entries = append(entries, kv{key: k, count: c})
		total += c
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].key < entries[j].key
	})

	ranks := make([]KeyRank, 0, len(entries))
	cumulative := 0
	for _, e := range entries {
		cumulative += e.count
		share := 0.0
		if total > 0 {
			share = float64(cumulative) / float64(total)
		}
		ranks = append(ranks, KeyRank{Key: e.key, Count: e.count, CumulativeShare: share})
	}
	return ranks
}

// HotKeys ranges an already-sorted ranking and returns the smallest
// key-count-descending prefix whose CumulativeShare reaches threshold — the
// minimal set of keys that, together, account for that fraction of all
// traffic. Those are the keys a caller should pin in a hot cache tier or
// guard with a per-key circuit breaker, since they carry disproportionate
// load.
func HotKeys(ranks []KeyRank, threshold float64) []string {
	var hot []string
	for _, r := range ranks {
		hot = append(hot, r.Key)
		if r.CumulativeShare >= threshold {
			break
		}
	}
	return hot
}
```

### The runnable demo

The demo draws 2000 samples from a seeded `rand.Zipf` generator over 50
possible keys — a fixed seed keeps the sequence, and therefore this demo's
output, identical on every run — then prints the top 5 keys by traffic and
how many distinct keys are needed to explain 80% of it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math/rand"

	"example.com/zipf-distribution-hot-key-tracker"
)

func main() {
	// A deterministic Zipf-distributed access pattern over 50 possible
	// cache keys: a fixed seed makes the sequence, and therefore this
	// demo's output, reproducible across every run.
	src := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(src, 1.5, 1, 49)

	tr := hotkeys.New()
	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("key-%d", zipf.Uint64())
		tr.Record(key)
	}

	ranks := tr.Rank()
	fmt.Println("top 5 keys by traffic:")
	for _, r := range ranks[:5] {
		fmt.Printf("  %s count=%d cumulative_share=%.3f\n", r.Key, r.Count, r.CumulativeShare)
	}

	hot := hotkeys.HotKeys(ranks, 0.8)
	fmt.Printf("hot keys (80%% of traffic): %d of %d distinct keys\n", len(hot), len(ranks))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
top 5 keys by traffic:
  key-0 count=846 cumulative_share=0.423
  key-1 count=286 cumulative_share=0.566
  key-2 count=151 cumulative_share=0.641
  key-3 count=108 cumulative_share=0.696
  key-4 count=83 cumulative_share=0.737
hot keys (80% of traffic): 7 of 50 distinct keys
```

### Tests

The ranking test checks both the sort order (count descending, key ascending
to break ties) and that the last rank's `CumulativeShare` is exactly `1.0`.
The `HotKeys` table covers an empty tracker, a single key so dominant it
alone crosses the threshold, and a threshold of `1.0` that forces every key
into the result.

Create `hotkeys_test.go`:

```go
package hotkeys

import "testing"

func TestRankOrdersByCountThenKey(t *testing.T) {
	t.Parallel()

	tr := New()
	for i := 0; i < 10; i++ {
		tr.Record("popular")
	}
	for i := 0; i < 3; i++ {
		tr.Record("medium")
	}
	tr.Record("rare-b")
	tr.Record("rare-a")

	ranks := tr.Rank()
	wantOrder := []string{"popular", "medium", "rare-a", "rare-b"}
	if len(ranks) != len(wantOrder) {
		t.Fatalf("Rank() len = %d, want %d", len(ranks), len(wantOrder))
	}
	for i, want := range wantOrder {
		if ranks[i].Key != want {
			t.Fatalf("Rank()[%d].Key = %q, want %q", i, ranks[i].Key, want)
		}
	}

	last := ranks[len(ranks)-1]
	if last.CumulativeShare != 1.0 {
		t.Fatalf("last rank CumulativeShare = %v, want 1.0", last.CumulativeShare)
	}
}

func TestHotKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		counts    map[string]int
		threshold float64
		want      []string
	}{
		{
			name:      "empty tracker returns no hot keys",
			counts:    map[string]int{},
			threshold: 0.8,
			want:      nil,
		},
		{
			name: "one dominant key crosses threshold alone",
			counts: map[string]int{
				"hot":   90,
				"warm":  6,
				"cold":  3,
				"icier": 1,
			},
			threshold: 0.8,
			want:      []string{"hot"},
		},
		{
			name: "threshold of 1.0 requires every key",
			counts: map[string]int{
				"a": 5,
				"b": 3,
				"c": 2,
			},
			threshold: 1.0,
			want:      []string{"a", "b", "c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := New()
			for key, n := range tc.counts {
				for i := 0; i < n; i++ {
					tr.Record(key)
				}
			}

			got := HotKeys(tr.Rank(), tc.threshold)
			if len(got) != len(tc.want) {
				t.Fatalf("HotKeys() = %v, want %v", got, tc.want)
			}
			for i, key := range tc.want {
				if got[i] != key {
					t.Fatalf("HotKeys()[%d] = %q, want %q", i, got[i], key)
				}
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The tracker is correct when `Rank` orders keys strictly by descending count
with a deterministic tie-break, `CumulativeShare` on the last ranked key is
always exactly `1.0`, and `HotKeys` returns the smallest possible prefix
that reaches the requested threshold. The bug this design specifically
avoids is trying to compute rank and cumulative share in one pass over the
raw map: because map iteration order is randomized, any attempt to
accumulate a running share while ranging `t.counts` directly would produce a
different, meaningless "cumulative" value on every run — the sort has to
happen first, on a materialized slice, before cumulative share means
anything at all.

## Resources

- [math/rand: NewZipf](https://pkg.go.dev/math/rand#NewZipf) — the generator this exercise's demo uses to produce a realistic access pattern.
- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_statements)
- [sort.Slice](https://pkg.go.dev/sort#Slice)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-request-coalescing-singleflight.md](28-request-coalescing-singleflight.md) | Next: [30-graceful-config-reload-dual-write.md](30-graceful-config-reload-dual-write.md)
