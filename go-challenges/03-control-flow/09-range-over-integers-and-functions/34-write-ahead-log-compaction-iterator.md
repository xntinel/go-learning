# Exercise 34: Write-Ahead Log Compaction Iterator — Scan Entries, Track Offset, Garbage Collect Stale Data

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A write-ahead log that only ever appends grows forever unless something
identifies which records are dead weight: a key overwritten five times has
four stale copies sitting in the log, and a deleted key's tombstone is the
only record anyone still needs, not the value it deleted. Compaction
answers "which offsets can I safely reclaim" without ever revising a record
a reader may already have consumed, which is exactly why it is expressed
here as a high-water mark that only ever advances, plus an incremental scan
that can notice a previously-compacted entry has since gone stale. This
exercise is an independent module with its own `go mod init`.

## What you'll build

```text
wal/                       independent module: example.com/write-ahead-log-compaction-iterator
  go.mod                    module example.com/write-ahead-log-compaction-iterator
  wal.go                    Entry, Log, New, Append, Delete, Scan, Compact, HighWaterMark
  cmd/
    demo/
      main.go               runnable demo: 5 entries, one overwrite, one delete
  wal_test.go                live-entry scan, partial scan, incremental compaction, early-stop, concurrent readers, panics
```

Implement: `New() *Log`, `(*Log) Append(key, value string) int`, `(*Log) Delete(key string) int`, `(*Log) Scan(upTo int) iter.Seq[Entry]` yielding each key's live entry within `[0, upTo)`, `(*Log) Compact(upTo int) []int` returning newly reclaimable offsets and advancing the high-water mark, and `(*Log) HighWaterMark() int`.
Test: `Scan` yields only the final, non-tombstoned write per key within range; a partial `upTo` ignores later entries entirely; `Compact` reports offsets superseded within its walked range, including an offset compacted in an *earlier* call that a brand-new write has just made stale; a consumer break stops `Scan`; concurrent `Scan` calls interleaved with concurrent `Append` calls never race and never yield the same key twice in one scan; a negative `upTo` panics.
Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/34-write-ahead-log-compaction-iterator/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/34-write-ahead-log-compaction-iterator
go mod edit -go=1.24
```

The one design decision that separates a correct incremental compactor
from a naive one is what `Compact` does with entries it already classified
in an *earlier* call. A first attempt might walk only the new delta
`[highWaterMark, upTo)` and check each entry against a freshly built
`map[key]lastIndex` scoped to that delta alone -- which works for entries
written after the previous compaction, but silently misses the case where
a brand-new write supersedes a key whose live offset was fixed several
compactions ago. `Compact` instead keeps a persistent `liveOffset` map that
survives across calls: when entry `i` writes a key `liveOffset` already
tracks, the *previous* offset -- wherever it sits, compacted long ago or
not -- becomes reclaimable right now, in this call. That is what lets a
long-running compactor amortize its work: it never rescans the whole log,
yet it still eventually reports every stale offset exactly once, the
moment something proves it stale. `Scan`, by contrast, has no persistent
state at all -- it recomputes which entries are live from scratch every
call, because it has to support being called with an arbitrary, possibly
shrinking `upTo` and must never assume calls arrive in any particular
order.

Create `wal.go`:

```go
package wal

import (
	"iter"
	"sync"
)

// Entry is one write-ahead log record: a key/value write, or a tombstone
// marking that key deleted.
type Entry struct {
	Offset    int
	Key       string
	Value     string
	Tombstone bool
}

// Log is an append-only write-ahead log with a compaction high-water mark.
// A sync.RWMutex guards it: appends and compaction take the write lock,
// but multiple goroutines can Scan concurrently under the read lock,
// which matters because a compactor rewriting live entries into a fresh
// segment must not block ordinary readers scanning the log for other
// purposes at the same time.
type Log struct {
	mu            sync.RWMutex
	entries       []Entry
	highWaterMark int
	liveOffset    map[string]int // key -> offset of its current live entry, as of highWaterMark
}

// New creates an empty Log.
func New() *Log {
	return &Log{}
}

// Append adds a key/value write and returns its offset.
func (l *Log) Append(key, value string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	off := len(l.entries)
	l.entries = append(l.entries, Entry{Offset: off, Key: key, Value: value})
	return off
}

// Delete appends a tombstone for key and returns its offset.
func (l *Log) Delete(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	off := len(l.entries)
	l.entries = append(l.entries, Entry{Offset: off, Key: key, Tombstone: true})
	return off
}

// HighWaterMark reports the offset up to which Compact has already run.
// Entries at or after this offset have not yet been classified as live or
// reclaimable by any Compact call.
func (l *Log) HighWaterMark() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.highWaterMark
}

// liveIndex returns, for the entries in [0, upTo), a map from key to the
// index of that key's last occurrence in the range -- the entry a fresh
// read of the log would actually observe for that key. Callers must hold
// at least l.mu.RLock() while using the returned indices against l.entries.
func (l *Log) liveIndex(upTo int) map[string]int {
	last := make(map[string]int, upTo)
	for i := 0; i < upTo; i++ {
		last[l.entries[i].Key] = i
	}
	return last
}

// Scan yields, in ascending offset order, the live entries within [0, upTo):
// for each key, only its final write in that range, and only if that final
// write is not a tombstone. An entry superseded by a later write or delete
// within the range is never yielded -- Scan reconstructs exactly what the
// key/value store looks like as of upTo, not a raw replay of every record.
func (l *Log) Scan(upTo int) iter.Seq[Entry] {
	if upTo < 0 {
		panic("wal: upTo must be >= 0")
	}
	return func(yield func(Entry) bool) {
		l.mu.RLock()
		defer l.mu.RUnlock()

		bound := upTo
		if bound > len(l.entries) {
			bound = len(l.entries)
		}
		last := l.liveIndex(bound)

		for i := 0; i < bound; i++ {
			e := l.entries[i]
			if last[e.Key] != i || e.Tombstone {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// Compact advances the high-water mark from its current position to upTo
// and returns every offset newly proven reclaimable in the process --
// including, critically, offsets *before* the previous high-water mark
// whose key is superseded by a write that only just arrived. Compact keeps
// a persistent liveOffset map (key -> its current live offset) across
// calls specifically so it can notice that: when entry i writes a key that
// liveOffset already tracks, the *previous* offset recorded for that key is
// now reclaimable even though it was already compacted in an earlier call,
// because nothing will ever need to read it again. A tombstone is
// reclaimable the moment it is processed (Scan never yields tombstones,
// so once compaction has passed one there is no reason to keep it), and it
// also retires the key from liveOffset so a later re-Append of the same
// key starts a fresh lineage. Because each call only walks
// [previous high-water mark, upTo), repeated calls with a growing upTo
// only pay for the new delta, which is what keeps compaction cheap on a
// log that is compacted incrementally as it grows rather than all at once.
func (l *Log) Compact(upTo int) []int {
	if upTo < 0 {
		panic("wal: upTo must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	bound := upTo
	if bound > len(l.entries) {
		bound = len(l.entries)
	}
	if bound <= l.highWaterMark {
		return nil
	}
	if l.liveOffset == nil {
		l.liveOffset = make(map[string]int)
	}

	var reclaimable []int
	for i := l.highWaterMark; i < bound; i++ {
		e := l.entries[i]
		if prev, ok := l.liveOffset[e.Key]; ok {
			reclaimable = append(reclaimable, prev)
		}
		if e.Tombstone {
			delete(l.liveOffset, e.Key)
			reclaimable = append(reclaimable, i)
		} else {
			l.liveOffset[e.Key] = i
		}
	}
	l.highWaterMark = bound
	return reclaimable
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/write-ahead-log-compaction-iterator"
)

func main() {
	l := wal.New()
	l.Append("user:1", "v1") // offset 0
	l.Append("user:2", "v1") // offset 1
	l.Append("user:1", "v2") // offset 2, supersedes offset 0
	l.Delete("user:2")       // offset 3, tombstones offset 1
	l.Append("user:3", "v1") // offset 4

	fmt.Println("live entries:")
	for e := range l.Scan(5) {
		fmt.Printf("  offset=%d key=%s value=%s\n", e.Offset, e.Key, e.Value)
	}

	reclaimable := l.Compact(5)
	fmt.Printf("reclaimable offsets: %v\n", reclaimable)
	fmt.Printf("high-water mark: %d\n", l.HighWaterMark())
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
live entries:
  offset=2 key=user:1 value=v2
  offset=4 key=user:3 value=v1
reclaimable offsets: [0 1 3]
high-water mark: 5
```

`Scan` reconstructs the log's current state as of offset 5: `user:1` at its
latest value, `user:3` present, and `user:2` absent entirely because its
last record is a tombstone. `Compact` independently confirms which three
physical offsets no longer contribute anything a reader could observe:
offset 0 (superseded by offset 2), offset 1 (tombstoned by offset 3), and
offset 3 itself (the tombstone, now safely past).

### Tests

Create `wal_test.go`:

```go
package wal

import (
	"fmt"
	"sync"
	"testing"
)

func TestScanYieldsOnlyLiveEntries(t *testing.T) {
	t.Parallel()

	l := New()
	l.Append("user:1", "v1")
	l.Append("user:2", "v1")
	l.Append("user:1", "v2")
	l.Delete("user:2")
	l.Append("user:3", "v1")

	var got []Entry
	for e := range l.Scan(5) {
		got = append(got, e)
	}

	want := []Entry{
		{Offset: 2, Key: "user:1", Value: "v2"},
		{Offset: 4, Key: "user:3", Value: "v1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestScanPartialUpToOnlyConsidersEarlierEntries(t *testing.T) {
	t.Parallel()

	l := New()
	l.Append("k", "v1") // offset 0
	l.Append("k", "v2") // offset 1

	var got []Entry
	for e := range l.Scan(1) { // only offset 0 is in scope
		got = append(got, e)
	}
	if len(got) != 1 || got[0].Value != "v1" {
		t.Fatalf("got %+v, want a single entry with value v1", got)
	}
}

func TestCompactReturnsOnlyNewlyReclaimableOffsetsAndAdvancesHighWaterMark(t *testing.T) {
	t.Parallel()

	l := New()
	l.Append("user:1", "v1") // 0, superseded
	l.Append("user:2", "v1") // 1, superseded (tombstoned)
	l.Append("user:1", "v2") // 2, live
	l.Delete("user:2")       // 3, reclaimable tombstone
	l.Append("user:3", "v1") // 4, live

	got := l.Compact(5)
	want := []int{0, 1, 3}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	if l.HighWaterMark() != 5 {
		t.Fatalf("HighWaterMark() = %d, want 5", l.HighWaterMark())
	}

	// A second Compact call at the same offset must find nothing new.
	if got := l.Compact(5); got != nil {
		t.Fatalf("second Compact(5) = %v, want nil (already compacted up to here)", got)
	}

	// Growing the log and compacting the delta must only report the new
	// entries, never re-classifying offsets already covered.
	l.Append("user:1", "v3") // 5, supersedes offset 2
	got2 := l.Compact(6)
	want2 := []int{2}
	if len(got2) != len(want2) || got2[0] != want2[0] {
		t.Fatalf("delta Compact = %v, want %v", got2, want2)
	}
}

func TestScanStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	l := New()
	for i := 0; i < 10; i++ {
		l.Append(fmt.Sprintf("k%d", i), "v")
	}

	count := 0
	for range l.Scan(10) {
		count++
		if count == 3 {
			break
		}
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
}

func TestConcurrentScansAndAppendsDoNotRace(t *testing.T) {
	t.Parallel()

	l := New()
	l.Append("seed", "v")

	var wg sync.WaitGroup

	// Writer: keeps appending new keys.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.Append(fmt.Sprintf("k%d", i), "v")
		}
	}()

	// Readers: repeatedly scan a growing prefix of the log concurrently
	// with the writer above; the assertion is only that this never
	// triggers the race detector and every yielded entry is genuinely live
	// (no duplicate keys in one Scan's output).
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				seen := map[string]bool{}
				for e := range l.Scan(1 << 30) {
					if seen[e.Key] {
						t.Errorf("key %q yielded more than once in a single Scan", e.Key)
					}
					seen[e.Key] = true
				}
			}
		}()
	}

	wg.Wait()
}

func TestScanAndCompactPanicOnNegativeUpTo(t *testing.T) {
	t.Parallel()

	l := New()
	l.Append("k", "v")

	mustPanic := func(name string, fn func()) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn()
		})
	}
	mustPanic("scan", func() {
		for range l.Scan(-1) {
		}
	})
	mustPanic("compact", func() { l.Compact(-1) })
}
```

## Review

The delta-recompaction test is the one that matters most: it proves
`Compact` does not silently under-report just because a stale offset
happens to predate the previous high-water mark. A compactor that only
ever compared entries within the *current* call's delta would report zero
reclaimable offsets for `l.Append("user:1", "v3")` in that test, because
offset 2 (the entry it supersedes) sits entirely outside `[5, 6)` --
exactly the kind of bug that looks correct in a quick manual test with a
single `Compact` call and only shows up once a log is compacted
incrementally in production over its whole lifetime. The concurrency test
defends the other half of the contract: readers must be able to `Scan` a
growing log while a writer appends to it without the race detector ever
firing, which is precisely what the `RWMutex` split between read-only
`Scan`/`HighWaterMark` and write-locked `Append`/`Delete`/`Compact` is
there to guarantee.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Bitcask: A Log-Structured Hash Table for Fast Key/Value Data](https://riak.com/assets/bitcask-intro.pdf)
- [The Go Memory Model](https://go.dev/ref/mem)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-hierarchical-token-quota-manager.md](33-hierarchical-token-quota-manager.md) | Next: [35-watermark-based-event-time-windowing.md](35-watermark-based-event-time-windowing.md)
