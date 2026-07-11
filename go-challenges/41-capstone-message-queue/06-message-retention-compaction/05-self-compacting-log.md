# Exercise 5: A Self-Compacting Log with Reclamation Accounting

The two-pass compactor is a mechanism; this exercise builds the policy around it. A self-compacting log appends like any log, but after each write it measures its own dirty ratio and runs a compaction cycle the moment redundant versions cross a threshold — then reports the bytes it reclaimed, so the amortized cost of keeping the log lean is a number you can read off. The payoff test proves the thing compaction promises: superseded values are reclaimed while the latest value of every key, and a within-window tombstone, survive.

This module is fully self-contained. It bundles the `Segment`, the two-pass `Compactor`, the `DirtyRatio` metric, and the `CompactingLog` that ties them together, with its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
log.go                 Segment, Compactor (two-pass), DirtyRatio; CompactingLog (Append, Delete, Reclaimed)
cmd/
  demo/
    main.go            write three versions of two keys, watch bytes get reclaimed
log_test.go            reclamation accounting, latest-per-key survival, tombstone survival
```

- Files: `log.go`, `cmd/demo/main.go`, `log_test.go`.
- Implement: `CompactingLog.Append`/`Delete`, the dirty-ratio-triggered `compact`, and `Reclaimed`.
- Test: that writing many versions reclaims bytes (`Reclaimed() > 0`), that only the latest value per key survives, and that a within-window tombstone survives.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p self-compacting-log/cmd/demo && cd self-compacting-log
go mod init example.com/self-compacting-log
```

### Triggering on the dirty ratio, not on every write

Compaction is sequential I/O over the whole keyed log, so running it on every append would dominate the cost of writing. The `CompactingLog` instead measures the dirty ratio after each append and compacts only when it crosses a configured `threshold`. Dirty bytes are bytes in segments whose base offset is above `compactedUpTo` — everything written since the last cycle. When `dirty / total` reaches the threshold, enough redundancy has accumulated that a cycle pays for itself; below it, the log is lean enough to leave alone.

After a cycle, `compactedUpTo` advances to the highest offset written so far, which resets the dirty count to zero: the freshly compacted segment is entirely at or below `compactedUpTo`, so it counts as clean, and only subsequent appends are dirty again. The threshold therefore controls the sawtooth — a higher threshold lets more versions pile up between cycles (cheaper amortized I/O, more transient storage), a lower one keeps the log tighter (more frequent cycles). `compactedUpTo` starts at -1 so the very first append, at offset 0, already counts as dirty.

### Measuring what compaction reclaims

Each cycle records `before` (the summed size of all current segments) and `after` (the size of the single compacted segment it produces), and adds `before - after` to a running `reclaimed` total. This difference is always non-negative: the compacted segment's messages are a subset of the input messages — one survivor per key — so its size can never exceed the input. Superseded versions are exactly the bytes that disappear, and `reclaimed` is their running sum. Exposing it turns the abstract claim "compaction saves space" into an observable quantity, which is what a real broker surfaces as a metric so an operator can see whether compaction is keeping up with the write rate.

The compaction itself is the two-pass algorithm from the compaction exercise, bundled here: pass 1 maps each key to its highest offset, pass 2 emits one message per key in offset order, and tombstones are held for `DeleteRetention` before being reclaimed. The `CompactingLog` wraps that pure compactor behind a mutex so `Append`, `Delete`, and the read accessors are all safe for concurrent use.

Create `log.go`:

```go
package retention

import (
	"sort"
	"sync"
	"time"
)

// Message is a single record. A nil Value with a non-empty Key is a tombstone.
type Message struct {
	Key       []byte
	Value     []byte
	Offset    int64
	Timestamp time.Time
}

// IsTombstone reports whether the message marks a key as deleted.
func (m Message) IsTombstone() bool {
	return len(m.Key) > 0 && m.Value == nil
}

// Segment is an immutable, sealed slice of messages.
type Segment struct {
	messages   []Message
	sizeBytes  int64
	baseOffset int64
}

// NewSegment seals an ordered slice of messages. An empty slice seals to a
// zero-value Segment.
func NewSegment(msgs []Message) *Segment {
	if len(msgs) == 0 {
		return &Segment{}
	}
	var size int64
	for _, m := range msgs {
		size += int64(len(m.Key) + len(m.Value))
	}
	return &Segment{messages: msgs, sizeBytes: size, baseOffset: msgs[0].Offset}
}

// Messages returns the messages in the segment. Do not mutate the slice.
func (s *Segment) Messages() []Message { return s.messages }

// SizeBytes returns the segment's total key+value byte length.
func (s *Segment) SizeBytes() int64 { return s.sizeBytes }

// BaseOffset is the log offset of the first message.
func (s *Segment) BaseOffset() int64 { return s.baseOffset }

// DirtyRatio returns the fraction of log bytes in segments whose BaseOffset is
// above compactedUpTo. An empty log has ratio 0.
func DirtyRatio(segs []*Segment, compactedUpTo int64) float64 {
	var total, dirty int64
	for _, s := range segs {
		total += s.SizeBytes()
		if s.BaseOffset() > compactedUpTo {
			dirty += s.SizeBytes()
		}
	}
	if total == 0 {
		return 0
	}
	return float64(dirty) / float64(total)
}

// Compactor reduces a set of segments to one message per key, keeping the
// highest-offset value and holding tombstones for DeleteRetention.
type Compactor struct {
	DeleteRetention time.Duration
	tombstones      map[string]time.Time
}

// NewCompactor returns a Compactor with the given tombstone retention window.
func NewCompactor(deleteRetention time.Duration) *Compactor {
	return &Compactor{DeleteRetention: deleteRetention, tombstones: map[string]time.Time{}}
}

// Compact reduces segs to one message per key in two passes.
func (c *Compactor) Compact(segs []*Segment, now time.Time) *Segment {
	latest := map[string]int64{}
	for _, seg := range segs {
		for _, m := range seg.Messages() {
			if len(m.Key) == 0 {
				continue
			}
			k := string(m.Key)
			if cur, ok := latest[k]; !ok || m.Offset > cur {
				latest[k] = m.Offset
			}
		}
	}

	var all []Message
	for _, seg := range segs {
		all = append(all, seg.Messages()...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Offset < all[j].Offset })

	var kept []Message
	for _, m := range all {
		if len(m.Key) == 0 {
			kept = append(kept, m)
			continue
		}
		k := string(m.Key)
		if m.Offset != latest[k] {
			continue
		}
		if m.IsTombstone() {
			created, seen := c.tombstones[k]
			if !seen {
				c.tombstones[k] = now
				kept = append(kept, m)
				continue
			}
			if now.Sub(created) < c.DeleteRetention {
				kept = append(kept, m)
				continue
			}
			delete(c.tombstones, k)
			continue
		}
		delete(c.tombstones, k)
		kept = append(kept, m)
	}
	return NewSegment(kept)
}

// CompactingLog appends messages and compacts automatically when the dirty
// ratio crosses a threshold, tracking the bytes reclaimed.
type CompactingLog struct {
	mu            sync.Mutex
	segs          []*Segment
	nextOff       int64
	compactedUpTo int64
	threshold     float64
	compactor     *Compactor
	reclaimed     int64
	now           func() time.Time
}

// NewCompactingLog builds a log that compacts when DirtyRatio reaches threshold.
// deleteRetention is the tombstone window; now supplies timestamps.
func NewCompactingLog(threshold float64, deleteRetention time.Duration, now func() time.Time) *CompactingLog {
	return &CompactingLog{
		compactedUpTo: -1,
		threshold:     threshold,
		compactor:     NewCompactor(deleteRetention),
		now:           now,
	}
}

// Append writes one message as its own segment, then compacts if the dirty
// ratio has reached the threshold.
func (l *CompactingLog) Append(key, value []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seg := NewSegment([]Message{{Key: key, Value: value, Offset: l.nextOff, Timestamp: l.now()}})
	l.nextOff++
	l.segs = append(l.segs, seg)
	if DirtyRatio(l.segs, l.compactedUpTo) >= l.threshold {
		l.compact()
	}
}

// Delete appends a tombstone for key.
func (l *CompactingLog) Delete(key []byte) { l.Append(key, nil) }

// Compact forces a compaction cycle regardless of the dirty ratio.
func (l *CompactingLog) Compact() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.compact()
}

// compact runs one cycle and updates the reclaimed total. The caller holds mu.
func (l *CompactingLog) compact() {
	if len(l.segs) == 0 {
		return
	}
	var before int64
	for _, s := range l.segs {
		before += s.SizeBytes()
	}
	compacted := l.compactor.Compact(l.segs, l.now())
	l.reclaimed += before - compacted.SizeBytes()
	if len(compacted.Messages()) == 0 {
		l.segs = nil
	} else {
		l.segs = []*Segment{compacted}
	}
	l.compactedUpTo = l.nextOff - 1
}

// Reclaimed returns the total bytes removed by compaction so far.
func (l *CompactingLog) Reclaimed() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reclaimed
}

// Messages returns all live messages in offset order.
func (l *CompactingLog) Messages() []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Message
	for _, s := range l.segs {
		out = append(out, s.Messages()...)
	}
	return out
}
```

### The runnable demo

The demo writes three versions of two keys. With a 0.6 threshold the log compacts mid-stream, and by the end only the newest version of each key remains while the reclaimed counter shows the bytes the superseded versions gave back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/self-compacting-log"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := retention.NewCompactingLog(0.6, 24*time.Hour, func() time.Time { return base })

	for v := 1; v <= 3; v++ {
		for _, k := range []string{"user:1", "user:2"} {
			l.Append([]byte(k), []byte(fmt.Sprintf(`{"v":%d}`, v)))
		}
	}

	fmt.Println("appended 6 messages")
	fmt.Printf("reclaimed %d bytes\n", l.Reclaimed())
	msgs := l.Messages()
	fmt.Printf("final: %d messages\n", len(msgs))
	for _, m := range msgs {
		fmt.Printf("  %s = %s\n", m.Key, m.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
appended 6 messages
reclaimed 52 bytes
final: 2 messages
  user:1 = {"v":3}
  user:2 = {"v":3}
```

### Tests

`TestReclaimsSupersededKeepsLatest` writes five versions of three keys, forces a final cycle, and asserts the log collapsed to one live message per key holding the latest value, with a strictly positive reclaimed count — the central guarantee of compaction. `TestTombstoneSurvivesWithinWindow` deletes a key and asserts the tombstone is still present after compaction, so a deletion is observable. `TestNoReclaimWithoutDuplicates` checks the accounting honestly reports zero when every key is unique and nothing can be reclaimed.

Create `log_test.go`:

```go
package retention

import (
	"fmt"
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func fixed() time.Time { return epoch }

func TestReclaimsSupersededKeepsLatest(t *testing.T) {
	t.Parallel()
	l := NewCompactingLog(0.6, 24*time.Hour, fixed)
	for v := 1; v <= 5; v++ {
		for _, k := range []string{"a", "b", "c"} {
			l.Append([]byte(k), []byte(fmt.Sprintf("%s%d", k, v)))
		}
	}
	l.Compact()

	if l.Reclaimed() <= 0 {
		t.Errorf("Reclaimed = %d, want > 0", l.Reclaimed())
	}
	got := map[string]string{}
	for _, m := range l.Messages() {
		got[string(m.Key)] = string(m.Value)
	}
	if len(got) != 3 {
		t.Fatalf("live keys = %d, want 3", len(got))
	}
	for _, k := range []string{"a", "b", "c"} {
		if want := k + "5"; got[k] != want {
			t.Errorf("key %q = %q, want %q (only latest survives)", k, got[k], want)
		}
	}
}

func TestTombstoneSurvivesWithinWindow(t *testing.T) {
	t.Parallel()
	l := NewCompactingLog(0.6, 24*time.Hour, fixed)
	l.Append([]byte("k"), []byte("v1"))
	l.Append([]byte("k"), []byte("v2"))
	l.Delete([]byte("k"))
	l.Compact()

	msgs := l.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (the tombstone)", len(msgs))
	}
	if !msgs[0].IsTombstone() {
		t.Errorf("surviving message must be the tombstone, got value %q", msgs[0].Value)
	}
}

func TestNoReclaimWithoutDuplicates(t *testing.T) {
	t.Parallel()
	l := NewCompactingLog(0.6, 24*time.Hour, fixed)
	for i := range 10 {
		l.Append([]byte(fmt.Sprintf("k%d", i)), []byte("v"))
	}
	l.Compact()
	if l.Reclaimed() != 0 {
		t.Errorf("Reclaimed = %d, want 0 (every key unique)", l.Reclaimed())
	}
	if len(l.Messages()) != 10 {
		t.Errorf("live messages = %d, want 10", len(l.Messages()))
	}
}
```

## Review

The self-compacting log is correct when the reclaimed total is the exact running sum of superseded bytes and never goes negative: each cycle records `before - after`, and because the compacted segment is a subset of its input, `after <= before` always holds. `TestNoReclaimWithoutDuplicates` pins the honest-zero case — ten unique keys reclaim nothing — and `TestReclaimsSupersededKeepsLatest` pins the positive case while also proving the survival invariant: exactly one live message per key, holding the latest value.

The trigger is correct when `compactedUpTo` advances to the highest written offset after each cycle, so the freshly compacted segment counts as clean and only later appends are dirty. The mistake to avoid is failing to advance it, which leaves the compacted segment perpetually "dirty" and compacts on every subsequent append. `TestTombstoneSurvivesWithinWindow` confirms a delete remains observable: the tombstone is the one surviving message, not silently dropped.

## Resources

- [Kafka: Log Compaction](https://kafka.apache.org/documentation/#compaction) — the dirty-ratio trigger (`min.cleanable.dirty.ratio`) and the latest-per-key guarantee this log implements.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock that makes append, delete, and the read accessors safe for concurrent use.
- [Kafka: `min.cleanable.dirty.ratio`](https://kafka.apache.org/documentation/#topicconfigs_min.cleanable.dirty.ratio) — the production knob this exercise's `threshold` models.

---

Back to [04-background-reaper.md](04-background-reaper.md) | Next: [../07-tcp-protocol-client/00-concepts.md](../07-tcp-protocol-client/00-concepts.md)
