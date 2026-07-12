# Exercise 2: Time and Size Retention

Retention is how a log stays bounded. Two policies decide what to throw away: time retention deletes segments older than a window, and size retention deletes the oldest segments until the log fits under a byte cap. Both are phrased entirely in terms of a sealed segment's metadata, and both share one non-obvious invariant — the log must never go empty.

This module is fully self-contained. It defines its own `Message` and `Segment` so the policies have something to act on, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
policy.go              Message, Segment, NewSegment; TimeRetention, SizeRetention
cmd/
  demo/
    main.go            apply each policy to a five-segment log and print survivors
policy_test.go         time boundary cases, oldest-first eviction, the never-empty floor
```

- Files: `policy.go`, `cmd/demo/main.go`, `policy_test.go`.
- Implement: `TimeRetention.ShouldDelete` and `SizeRetention.ApplyToLog`, over a self-contained `Segment`.
- Test: the time boundary (a segment exactly at the edge is kept), oldest-first eviction under a size cap, and the floor that keeps one oversized segment alive.
- Verify: `go test -race ./...`

### Time retention: ask the youngest message

`TimeRetention.ShouldDelete` reports whether a whole segment has fallen out of the retention window. The key decision is *which* timestamp to test. The segment is ordered by offset, so its last message is the youngest; if even that youngest message is older than `MaxAge`, then every message in the segment is, and the segment is wholly expired and safe to delete. Testing `LastTimestamp()` is therefore both correct and the most conservative choice — a segment is deleted only when nothing in it is still in-window. Testing `FirstTimestamp()` instead would delete a segment while some of its messages were still inside the window, dropping live data.

The comparison is `now.Sub(seg.LastTimestamp()) > p.MaxAge`. The strict `>` makes the boundary inclusive of the window: a segment whose youngest message is *exactly* `MaxAge` old is kept, and only a segment strictly older than the window is deleted. That avoids a one-record flap at the boundary where a segment is deleted the instant it reaches the limit.

### Size retention: drop the oldest, but never the last

`SizeRetention.ApplyToLog` takes the whole segment list and returns the survivors. It sorts a *copy* by `BaseOffset` ascending so the oldest segment is first, sums the total, then walks from the oldest end dropping segments until the running total is at or below `MaxBytes`.

The loop guard is the whole point of the exercise:

```go
for total > p.MaxBytes && i < len(sorted)-1 {
	total -= sorted[i].SizeBytes()
	i++
}
```

The `i < len(sorted)-1` bound stops the loop while one segment still remains, *even if that segment alone is larger than `MaxBytes`*. Without it, a single oversized segment causes the loop to delete everything and hand back an empty log, and a consumer that has read nothing has no offset to resume from. Preserving the newest segment is the floor that keeps the log usable: you can be over budget, but you can never be empty. Sorting a copy rather than the input matters too — `ApplyToLog` is a pure function over its argument and must not reorder a slice the caller still holds.

Create `policy.go`:

```go
package retention

import (
	"sort"
	"time"
)

// Message is a single record in the log. A nil Value with a non-empty Key is a
// tombstone.
type Message struct {
	Key       []byte
	Value     []byte
	Offset    int64
	Timestamp time.Time
}

// Segment is an immutable, sealed slice of messages. Retention operates on
// whole segments, never individual messages.
type Segment struct {
	messages      []Message
	sizeBytes     int64
	lastTimestamp time.Time
	baseOffset    int64
}

// NewSegment seals an ordered slice of messages, computing its size, its last
// timestamp, and its base offset. The messages must be in ascending offset
// order. An empty slice seals to a zero-value Segment.
func NewSegment(msgs []Message) *Segment {
	if len(msgs) == 0 {
		return &Segment{}
	}
	var size int64
	for _, m := range msgs {
		size += int64(len(m.Key) + len(m.Value))
	}
	return &Segment{
		messages:      msgs,
		sizeBytes:     size,
		lastTimestamp: msgs[len(msgs)-1].Timestamp,
		baseOffset:    msgs[0].Offset,
	}
}

// SizeBytes returns the segment's total key+value byte length.
func (s *Segment) SizeBytes() int64 { return s.sizeBytes }

// LastTimestamp is the timestamp of the youngest message in the segment.
func (s *Segment) LastTimestamp() time.Time { return s.lastTimestamp }

// BaseOffset is the log offset of the oldest message; it orders segments.
func (s *Segment) BaseOffset() int64 { return s.baseOffset }

// TimeRetention deletes segments whose youngest message is older than MaxAge.
// Checking LastTimestamp means a segment is deleted only when every message in
// it is past the window, so no partial deletion is ever required.
type TimeRetention struct {
	MaxAge time.Duration
}

// ShouldDelete reports whether the segment's youngest message is strictly older
// than MaxAge. A segment exactly at the boundary is kept.
func (p TimeRetention) ShouldDelete(seg *Segment, now time.Time) bool {
	return now.Sub(seg.LastTimestamp()) > p.MaxAge
}

// SizeRetention caps total log volume. ApplyToLog deletes the oldest segments
// until the total is at or below MaxBytes, always preserving at least the
// newest segment so consumers retain a starting point.
type SizeRetention struct {
	MaxBytes int64
}

// ApplyToLog returns the segments that survive size-based retention, oldest
// dropped first. It sorts a copy, so the caller's slice is left untouched.
func (p SizeRetention) ApplyToLog(segs []*Segment) []*Segment {
	sorted := make([]*Segment, len(segs))
	copy(sorted, segs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].BaseOffset() < sorted[j].BaseOffset()
	})
	var total int64
	for _, s := range sorted {
		total += s.SizeBytes()
	}
	i := 0
	for total > p.MaxBytes && i < len(sorted)-1 {
		total -= sorted[i].SizeBytes()
		i++
	}
	return sorted[i:]
}
```

### The runnable demo

The demo builds five 12-byte segments (60 bytes total), applies a time window that expires the three oldest, then applies a 30-byte cap that drops oldest-first, and prints how many survive each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/retention-policies"
)

func seg(offset int64, key, value string, ts time.Time) *retention.Segment {
	return retention.NewSegment([]retention.Message{
		{Key: []byte(key), Value: []byte(value), Offset: offset, Timestamp: ts},
	})
}

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(5 * time.Hour)

	// Five segments, one per hour, each 12 bytes (2-byte key + 10-byte value).
	var segs []*retention.Segment
	for i := range 5 {
		segs = append(segs, seg(int64(i), fmt.Sprintf("k%d", i), "0123456789", base.Add(time.Duration(i)*time.Hour)))
	}
	fmt.Printf("start: %d segments, 60 bytes\n", len(segs))

	// Time retention: keep only segments younger than 2 hours.
	time2h := retention.TimeRetention{MaxAge: 2 * time.Hour}
	var survivors []*retention.Segment
	for _, s := range segs {
		if !time2h.ShouldDelete(s, now) {
			survivors = append(survivors, s)
		}
	}
	fmt.Printf("after time retention (2h): %d segments survive\n", len(survivors))

	// Size retention on the original five: cap at 30 bytes.
	size30 := retention.SizeRetention{MaxBytes: 30}
	kept := size30.ApplyToLog(segs)
	fmt.Printf("after size retention (30 bytes): %d segments survive, oldest base offset %d\n",
		len(kept), kept[0].BaseOffset())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: 5 segments, 60 bytes
after time retention (2h): 2 segments survive
after size retention (30 bytes): 2 segments survive, oldest base offset 3
```

Time retention keeps the hour-4 and hour-3 segments (younger than 2 hours before `now` at hour 5; hour-3 is exactly at the boundary and kept). Size retention drops oldest-first while the total exceeds 30: 60 → 48 (drop offset 0) → 36 (drop offset 1) → 24 (drop offset 2), then stops at 24 bytes. Offsets 3 and 4 survive, so the oldest surviving base offset is 3.

### Tests

`TestTimeRetentionBoundary` checks the three regions of the window: clearly old (deleted), exactly at `MaxAge` (kept), and recent (kept). `TestSizeRetentionEvictsOldest` verifies oldest-first eviction and the exact survivor. `TestSizeRetentionPreservesNewest` is the floor test: a single segment larger than `MaxBytes` must still survive.

Create `policy_test.go`:

```go
package retention

import (
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func seg(offset int64, sizeValue string, ts time.Time) *Segment {
	return NewSegment([]Message{
		{Key: []byte("k"), Value: []byte(sizeValue), Offset: offset, Timestamp: ts},
	})
}

func TestTimeRetentionBoundary(t *testing.T) {
	t.Parallel()
	policy := TimeRetention{MaxAge: 24 * time.Hour}
	now := epoch.Add(48 * time.Hour)

	cases := []struct {
		name   string
		lastTS time.Time
		want   bool
	}{
		{"clearly old", epoch, true},
		{"exactly at boundary", now.Add(-24 * time.Hour), false},
		{"recent", now.Add(-1 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := seg(0, "v", tc.lastTS)
			if got := policy.ShouldDelete(s, now); got != tc.want {
				t.Errorf("ShouldDelete = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSizeRetentionEvictsOldest(t *testing.T) {
	t.Parallel()
	// Three 11-byte segments (1-byte key + 10-byte value): 33 bytes total.
	s0 := seg(0, "0123456789", epoch)
	s1 := seg(1, "0123456789", epoch)
	s2 := seg(2, "0123456789", epoch)

	policy := SizeRetention{MaxBytes: 15}
	got := policy.ApplyToLog([]*Segment{s0, s1, s2})
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1", len(got))
	}
	if got[0].BaseOffset() != 2 {
		t.Errorf("surviving base offset = %d, want 2 (newest)", got[0].BaseOffset())
	}
}

func TestSizeRetentionDoesNotReorderInput(t *testing.T) {
	t.Parallel()
	s2 := seg(2, "0123456789", epoch)
	s0 := seg(0, "0123456789", epoch)
	in := []*Segment{s2, s0}
	_ = SizeRetention{MaxBytes: 100}.ApplyToLog(in)
	if in[0].BaseOffset() != 2 || in[1].BaseOffset() != 0 {
		t.Errorf("ApplyToLog reordered the caller's slice: %d,%d",
			in[0].BaseOffset(), in[1].BaseOffset())
	}
}

func TestSizeRetentionPreservesNewest(t *testing.T) {
	t.Parallel()
	// One segment larger than MaxBytes must still survive.
	big := seg(0, "01234567890123456789", epoch)
	policy := SizeRetention{MaxBytes: 5}
	got := policy.ApplyToLog([]*Segment{big})
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1 (newest always preserved)", len(got))
	}
}
```

## Review

Time retention is correct when it tests the youngest message: `ShouldDelete` consults `LastTimestamp()`, so a segment is deleted only when nothing in it remains in-window, and the strict `>` keeps a segment that is exactly at the boundary. The mistake to avoid is testing `FirstTimestamp()`, which would delete a segment while some of its messages were still live.

Size retention is correct when it drops oldest-first and stops at one survivor. The `i < len(sorted)-1` guard is the floor: it keeps the newest segment even when that segment alone exceeds `MaxBytes`, so the log can be over budget but never empty. `TestSizeRetentionPreservesNewest` pins this; deleting the guard makes it fail by returning an empty slice. `ApplyToLog` also sorts a copy, so it never reorders the caller's slice — `TestSizeRetentionDoesNotReorderInput` enforces that.

## Resources

- [Kafka: `log.retention.ms` and `log.retention.bytes`](https://kafka.apache.org/documentation/#brokerconfigs_log.retention.ms) — the production names for these two policies and how they compose.
- [`sort.Slice`](https://pkg.go.dev/sort#Slice) — the by-offset ordering used to evict oldest-first.
- [`time.Time.Sub`](https://pkg.go.dev/time#Time.Sub) — the duration arithmetic behind the retention-window comparison.

---

Back to [01-segmented-log.md](01-segmented-log.md) | Next: [03-log-compaction.md](03-log-compaction.md)
