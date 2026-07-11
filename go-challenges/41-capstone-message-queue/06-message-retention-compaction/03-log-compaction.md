# Exercise 3: Two-Pass Log Compaction

Compaction collapses a log to one message per key: the latest value wins, superseded versions are reclaimed, and deletes are signalled by tombstones that linger just long enough for every consumer to observe them. This exercise builds the compactor as a pure function over segments, plus the dirty-ratio metric that decides when running it is worth the I/O.

This module is fully self-contained. It defines its own `Message` and `Segment`, the `Compactor`, and the `DirtyRatio` helper, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
compaction.go          Message, Segment, NewSegment; Compactor.Compact, DirtyRatio
cmd/
  demo/
    main.go            compact three versions of two keys down to two messages
compaction_test.go     latest-per-key, keyless pass-through, tombstone window, dirty ratio
```

- Files: `compaction.go`, `cmd/demo/main.go`, `compaction_test.go`.
- Implement: `Compactor.Compact` (two passes, tombstone window) and the `DirtyRatio` function.
- Test: reduce versions to the latest per key, keep keyless messages, retain a tombstone within the window and expire it after, and compute the dirty ratio.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p log-compaction/cmd/demo && cd log-compaction
go mod init example.com/log-compaction
```

### Why two passes, and why forward both times

The keep/drop decision for a message depends on a fact about the *future* of the log: is there a later message with the same key? A single forward pass cannot answer that without buffering every value it has seen, which is O(N) memory. So `Compact` splits the work.

**Pass 1** scans every message and records, per key, the highest offset seen so far in a `map[string]int64`. After the pass, `latest[k]` is the offset of the one message that should survive for key `k`. The map is O(K) in the number of *distinct* keys, regardless of how many versions each key has — that is the entire memory advantage, and it is why compaction can run on a log far larger than RAM.

**Pass 2** re-reads the messages in offset order and emits a message only when its offset equals `latest[key]`. A superseded version has a lower offset than its key's latest, so it is dropped and counted as removed. Both passes run front-to-back, matching the sequential read pattern storage is fast at; the misleading alternative — a single backward pass that keeps the first occurrence of each key — produces results in descending order and needs a reversing pass to fix, so it is the same cost wearing a disguise.

Keyless messages (empty key) have no "latest value per key" relation and are always passed through unchanged. Offsets are compared as `int64`, never as strings: lexicographically `"9" > "10"`, which would pick an older version as the survivor.

### The tombstone window

A tombstone — non-empty key, nil value — is the latest message for its key, so pass 2 reaches it as a survivor. But it cannot be emitted forever, and it cannot be dropped immediately either. The compactor keeps a `tombstones` map from key to the time it *first* saw the tombstone:

- First compaction after the delete: the tombstone is not in the map, so record `CreatedAt = now` and emit it. Consumers reading from here on will see the delete.
- Later compaction, still within `DeleteRetention`: the tombstone is in the map and `now - CreatedAt < DeleteRetention`, so emit it again — the grace period has not elapsed.
- Compaction after `DeleteRetention`: `now - CreatedAt >= DeleteRetention`, so delete the map entry, drop the tombstone, and count it removed. The key is now fully reclaimed.

The window exists so a slow consumer always has at least `DeleteRetention` to observe a deletion before the evidence of it is reclaimed. Dropping the tombstone the first time it is seen is the classic bug: a consumer that reads the compacted log afterward never learns the key was deleted and caches its old value forever.

A live (non-tombstone) message that survives clears any stale tombstone metadata for its key, because a key that was deleted and then written again is alive, not pending deletion.

### The dirty ratio

`DirtyRatio` is the trigger metric, not part of `Compact`. Dirty bytes are bytes in segments whose `BaseOffset()` is above the last compacted offset — data that has not been through a compaction cycle yet. The ratio is `dirty / total`. A caller compacts when it crosses a threshold (Kafka's default is 0.5): high enough that a cycle reclaims a worthwhile amount, low enough that the log never bloats with redundant versions. An empty log has ratio 0.

Create `compaction.go`:

```go
package retention

import (
	"sort"
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

// NewSegment seals an ordered slice of messages, computing its size and base
// offset. An empty slice seals to a zero-value Segment.
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

// TombstoneMeta records when a tombstone was first observed during compaction.
type TombstoneMeta struct {
	CreatedAt time.Time
}

// Compactor reduces a log to one message per key, keeping the highest-offset
// value. Tombstones are retained for DeleteRetention after first observation,
// then removed on the next cycle.
type Compactor struct {
	// DeleteRetention is how long a tombstone survives after first observation.
	DeleteRetention time.Duration

	tombstones map[string]TombstoneMeta
}

// NewCompactor returns a Compactor with the given tombstone retention window.
func NewCompactor(deleteRetention time.Duration) *Compactor {
	return &Compactor{
		DeleteRetention: deleteRetention,
		tombstones:      make(map[string]TombstoneMeta),
	}
}

// Compact reduces segs to one message per key in two passes and returns the
// compacted segment and the number of messages removed. Keyless messages are
// kept unchanged. Tombstones obey the DeleteRetention window.
func (c *Compactor) Compact(segs []*Segment, now time.Time) (*Segment, int) {
	if len(segs) == 0 {
		return NewSegment(nil), 0
	}

	// Pass 1: highest offset per key.
	latest := make(map[string]int64)
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

	// Gather all messages and order by offset for the second pass.
	var all []Message
	for _, seg := range segs {
		all = append(all, seg.Messages()...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Offset < all[j].Offset })

	// Pass 2: emit each key's latest message, applying the tombstone window.
	var kept []Message
	removed := 0
	for _, m := range all {
		if len(m.Key) == 0 {
			kept = append(kept, m)
			continue
		}
		k := string(m.Key)
		if m.Offset != latest[k] {
			removed++
			continue
		}
		if m.IsTombstone() {
			meta, seen := c.tombstones[k]
			if !seen {
				c.tombstones[k] = TombstoneMeta{CreatedAt: now}
				kept = append(kept, m)
				continue
			}
			if now.Sub(meta.CreatedAt) < c.DeleteRetention {
				kept = append(kept, m)
				continue
			}
			delete(c.tombstones, k)
			removed++
			continue
		}
		delete(c.tombstones, k)
		kept = append(kept, m)
	}

	return NewSegment(kept), removed
}

// DirtyRatio returns the fraction of log bytes in segments whose BaseOffset is
// above compactedUpToOffset. Crossing a threshold (commonly 0.5) triggers a
// compaction cycle. An empty log has ratio 0.
func DirtyRatio(segs []*Segment, compactedUpToOffset int64) float64 {
	var total, dirty int64
	for _, s := range segs {
		total += s.SizeBytes()
		if s.BaseOffset() > compactedUpToOffset {
			dirty += s.SizeBytes()
		}
	}
	if total == 0 {
		return 0
	}
	return float64(dirty) / float64(total)
}
```

### The runnable demo

The demo writes three versions of two keys, compacts, and shows two messages survive (the latest of each) with four removed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/log-compaction"
)

func main() {
	epoch := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var msgs []retention.Message
	off := int64(0)
	for v := 1; v <= 3; v++ {
		for _, k := range []string{"user:1", "user:2"} {
			msgs = append(msgs, retention.Message{
				Key:       []byte(k),
				Value:     []byte(fmt.Sprintf(`{"v":%d}`, v)),
				Offset:    off,
				Timestamp: epoch,
			})
			off++
		}
	}
	seg := retention.NewSegment(msgs)

	c := retention.NewCompactor(24 * time.Hour)
	compacted, removed := c.Compact([]*retention.Segment{seg}, epoch)
	fmt.Printf("before: %d messages\n", len(msgs))
	fmt.Printf("after: %d messages, %d removed\n", len(compacted.Messages()), removed)
	for _, m := range compacted.Messages() {
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
before: 6 messages
after: 2 messages, 4 removed
  user:1 = {"v":3}
  user:2 = {"v":3}
```

### Tests

`TestCompactReducesToLatestPerKey` writes three versions of three keys and asserts each key collapses to its highest-offset value. `TestCompactKeepsKeylessMessages` proves a keyless message survives untouched. `TestCompactTombstoneWithinWindow` and `TestCompactTombstoneExpiresAfterWindow` pin the two-stage tombstone lifecycle. `TestDirtyRatio` checks the metric on a two-segment log.

Create `compaction_test.go`:

```go
package retention

import (
	"fmt"
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestCompactReducesToLatestPerKey(t *testing.T) {
	t.Parallel()
	var msgs []Message
	off := int64(0)
	for v := 1; v <= 3; v++ {
		for _, k := range []string{"a", "b", "c"} {
			msgs = append(msgs, Message{
				Key:    []byte(k),
				Value:  []byte(fmt.Sprintf("%s%d", k, v)),
				Offset: off, Timestamp: epoch,
			})
			off++
		}
	}
	seg := NewSegment(msgs)
	c := NewCompactor(24 * time.Hour)
	compacted, removed := c.Compact([]*Segment{seg}, epoch)

	if removed != 6 {
		t.Errorf("removed = %d, want 6", removed)
	}
	got := map[string]string{}
	for _, m := range compacted.Messages() {
		got[string(m.Key)] = string(m.Value)
	}
	for _, k := range []string{"a", "b", "c"} {
		if want := k + "3"; got[k] != want {
			t.Errorf("key %q = %q, want %q", k, got[k], want)
		}
	}
}

func TestCompactKeepsKeylessMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Key: nil, Value: []byte("control-1"), Offset: 0, Timestamp: epoch},
		{Key: []byte("k"), Value: []byte("v1"), Offset: 1, Timestamp: epoch},
		{Key: []byte("k"), Value: []byte("v2"), Offset: 2, Timestamp: epoch},
		{Key: nil, Value: []byte("control-2"), Offset: 3, Timestamp: epoch},
	}
	c := NewCompactor(24 * time.Hour)
	compacted, _ := c.Compact([]*Segment{NewSegment(msgs)}, epoch)

	keyless := 0
	for _, m := range compacted.Messages() {
		if len(m.Key) == 0 {
			keyless++
		}
	}
	if keyless != 2 {
		t.Errorf("kept %d keyless messages, want 2", keyless)
	}
}

func TestCompactTombstoneWithinWindow(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Key: []byte("x"), Value: []byte("v1"), Offset: 0, Timestamp: epoch},
		{Key: []byte("x"), Value: nil, Offset: 1, Timestamp: epoch},
	}
	c := NewCompactor(24 * time.Hour)
	compacted, _ := c.Compact([]*Segment{NewSegment(msgs)}, epoch)

	found := false
	for _, m := range compacted.Messages() {
		if string(m.Key) == "x" && m.IsTombstone() {
			found = true
		}
	}
	if !found {
		t.Error("tombstone for x must be retained within the delete-retention window")
	}
}

func TestCompactTombstoneExpiresAfterWindow(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Key: []byte("y"), Value: []byte("v1"), Offset: 0, Timestamp: epoch},
		{Key: []byte("y"), Value: nil, Offset: 1, Timestamp: epoch},
	}
	seg := NewSegment(msgs)
	c := NewCompactor(1 * time.Hour)
	// First cycle records the tombstone at CreatedAt = epoch and keeps it.
	c.Compact([]*Segment{seg}, epoch)
	// Second cycle two hours later: window is 1h, so the tombstone is reclaimed.
	compacted, _ := c.Compact([]*Segment{seg}, epoch.Add(2*time.Hour))
	for _, m := range compacted.Messages() {
		if string(m.Key) == "y" {
			t.Errorf("expired tombstone for y must be removed, got %+v", m)
		}
	}
}

func TestDirtyRatio(t *testing.T) {
	t.Parallel()
	// Two equal-size segments; offset 0 is clean (<= 0), offset 1 is dirty (> 0).
	s0 := NewSegment([]Message{{Key: []byte("k"), Value: []byte("0123456789"), Offset: 0, Timestamp: epoch}})
	s1 := NewSegment([]Message{{Key: []byte("k"), Value: []byte("0123456789"), Offset: 1, Timestamp: epoch}})
	if got := DirtyRatio([]*Segment{s0, s1}, 0); got < 0.45 || got > 0.55 {
		t.Errorf("DirtyRatio = %.4f, want ~0.5", got)
	}
	if got := DirtyRatio(nil, 0); got != 0 {
		t.Errorf("DirtyRatio(empty) = %.4f, want 0", got)
	}
}
```

## Review

Compaction is correct when it keeps exactly the highest-offset message per key. Pass 1 builds the offset map in O(K), pass 2 emits in offset order and drops the rest; both passes run forward. The mistakes that matter: a one-pass backward scan that needs reversing (same cost, misleading shape), comparing offsets as strings so `"9"` beats `"10"`, and forgetting that keyless messages have no per-key relation and must pass through.

The tombstone window is correct when a tombstone survives the first compaction that sees it and is reclaimed only after `DeleteRetention` elapses. `TestCompactTombstoneWithinWindow` proves the first half; `TestCompactTombstoneExpiresAfterWindow` proves the second by compacting twice with `now` advanced past the window. Dropping a tombstone on first sight is the bug that leaves slow consumers caching deleted values. Confirm a re-written (live) key clears its stale tombstone metadata so a delete-then-write does not later vanish.

## Resources

- [Kafka: Log Compaction](https://kafka.apache.org/documentation/#compaction) — the reference semantics for latest-per-key, tombstones, `delete.retention.ms`, and the dirty-ratio trigger.
- [`sort.Slice`](https://pkg.go.dev/sort#Slice) — the offset ordering used between the two passes.
- [Effective Go: maps](https://go.dev/doc/effective_go#maps) — the comma-ok form used to fold the per-key highest-offset scan.

---

Back to [02-retention-policies.md](02-retention-policies.md) | Next: [04-background-reaper.md](04-background-reaper.md)
