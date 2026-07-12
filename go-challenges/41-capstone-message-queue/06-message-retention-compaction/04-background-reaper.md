# Exercise 4: A Background Reaper for Combined Retention

A real topic carries both limits at once: keep data for at most seven days *and* at most a hundred gigabytes, whichever bites first. Neither policy is allowed to stop the producers, so the enforcement has to run on its own goroutine, snapshot the segment list, decide off to the side, and install the survivors atomically while appends keep arriving. This exercise builds that reaper and proves it is correct under the race detector.

This module is fully self-contained. It bundles the segmented `Log`, both retention policies, and the `Reaper` that drives them on a ticker, with its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
reaper.go              Log, Segment; TimeRetention, SizeRetention; Reaper (ReapOnce, Start, Stop)
cmd/
  demo/
    main.go            append eight segments, run one combined reap, print survivors
reaper_test.go         time-then-size composition, the never-empty floor, concurrent reap under -race
```

- Files: `reaper.go`, `cmd/demo/main.go`, `reaper_test.go`.
- Implement: `Reaper.ReapOnce` (apply time then size, never empty), and `Start`/`Stop` for the ticker-driven goroutine.
- Test: time-then-size composition, the floor that survives when both policies would empty the log, and a concurrent run where the reaper trims while producers append, under `-race`.
- Verify: `go test -race ./...`

### Composing two policies in one pass

`ReapOnce` is the heart of the reaper, and the order of operations is deliberate. It snapshots the segments, applies time retention first, then size retention to whatever time retention left, then installs the result. Time-first is the right order because dropping aged-out segments shrinks the byte total, so the size policy may have nothing left to do — and when it does act, it acts on already-current data rather than wasting its budget on segments that are about to expire anyway.

The composition has to preserve the never-empty floor through *both* stages. Time retention on its own would happily delete every segment if the whole log is older than `MaxAge`; that is correct for the policy in isolation but fatal for a live log, because a consumer that has read nothing needs an entry point. So `ReapOnce` keeps the newest segment as a hard floor: if the time filter removes everything, it falls back to the single newest segment before handing off to the size stage, and the size stage carries its own `i < len(sorted)-1` floor on top. The result is a log that can be over budget or fully aged out but is never empty.

The snapshot-decide-install shape is what makes this safe to run beside producers. `ReapOnce` reads `log.Segments()` (a copy, under the read lock), does all of its sorting and filtering on that copy with no lock held, and commits with a single `log.ReplaceSegments` under the write lock. A producer appending during the decision adds a segment that this reap simply will not see; the next tick picks it up. The reaper never holds the lock across its O(S log S) sort, so it never blocks an append for longer than the final swap.

### Driving it on a ticker

`Start` launches a goroutine that reaps on a fixed interval; `Stop` shuts it down and waits for it to exit. The pattern is the standard `time.Ticker` plus a stop channel, with a separate done channel so `Stop` can join the goroutine and guarantee no reap is in flight when it returns.

Two details make the shutdown clean. First, `Stop` closes the stop channel and then receives from done, so it blocks until the goroutine has observed the close and returned — there is no window where a reap runs after `Stop` returns. Second, the goroutine `defer`s both `ticker.Stop()` and `close(done)`, so the ticker is always released and `Stop` always unblocks even if a reap panics. The injectable `now` function lets a test drive `ReapOnce` with deterministic timestamps while the live reaper uses `time.Now`.

Create `reaper.go`:

```go
package retention

import (
	"sort"
	"sync"
	"time"
)

// Message is a single record in the log.
type Message struct {
	Key       []byte
	Value     []byte
	Offset    int64
	Timestamp time.Time
}

// Segment is an immutable, sealed slice of messages.
type Segment struct {
	messages      []Message
	sizeBytes     int64
	lastTimestamp time.Time
	baseOffset    int64
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
	return &Segment{
		messages:      msgs,
		sizeBytes:     size,
		lastTimestamp: msgs[len(msgs)-1].Timestamp,
		baseOffset:    msgs[0].Offset,
	}
}

// SizeBytes returns the segment's total key+value byte length.
func (s *Segment) SizeBytes() int64 { return s.sizeBytes }

// LastTimestamp is the timestamp of the youngest message.
func (s *Segment) LastTimestamp() time.Time { return s.lastTimestamp }

// BaseOffset is the log offset of the first message.
func (s *Segment) BaseOffset() int64 { return s.baseOffset }

// Log is a sequence of sealed segments safe for concurrent append and reap.
type Log struct {
	mu       sync.RWMutex
	segments []*Segment
	nextOff  int64
}

// NewLog creates an empty Log.
func NewLog() *Log { return &Log{} }

// Append seals a new segment from the given key/value pairs.
func (l *Log) Append(keys, values [][]byte, now time.Time) *Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	msgs := make([]Message, len(keys))
	for i := range keys {
		msgs[i] = Message{Key: keys[i], Value: values[i], Offset: l.nextOff, Timestamp: now}
		l.nextOff++
	}
	seg := NewSegment(msgs)
	l.segments = append(l.segments, seg)
	return seg
}

// Segments returns a point-in-time snapshot of the segment list.
func (l *Log) Segments() []*Segment {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Segment, len(l.segments))
	copy(out, l.segments)
	return out
}

// TotalSizeBytes sums the sizes of all segments.
func (l *Log) TotalSizeBytes() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var total int64
	for _, s := range l.segments {
		total += s.sizeBytes
	}
	return total
}

// ReplaceSegments atomically installs a new segment list.
func (l *Log) ReplaceSegments(segs []*Segment) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.segments = segs
}

// TimeRetention deletes segments whose youngest message is older than MaxAge.
// A zero MaxAge disables the policy.
type TimeRetention struct {
	MaxAge time.Duration
}

// ShouldDelete reports whether the segment is strictly older than MaxAge.
func (p TimeRetention) ShouldDelete(seg *Segment, now time.Time) bool {
	return now.Sub(seg.LastTimestamp()) > p.MaxAge
}

// SizeRetention drops the oldest segments until the total is at or below
// MaxBytes, always keeping at least the newest segment. A zero MaxBytes
// disables the policy.
type SizeRetention struct {
	MaxBytes int64
}

// ApplyToLog returns the segments that survive size-based retention.
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

// Reaper enforces time and size retention on a Log, on demand or on a ticker.
type Reaper struct {
	log  *Log
	time TimeRetention
	size SizeRetention
	now  func() time.Time

	stop chan struct{}
	done chan struct{}
}

// NewReaper builds a Reaper over log with the given policies. The now function
// supplies the current time; pass time.Now for production use.
func NewReaper(log *Log, t TimeRetention, s SizeRetention, now func() time.Time) *Reaper {
	return &Reaper{log: log, time: t, size: s, now: now}
}

// ReapOnce applies time retention then size retention and installs the
// survivors, never leaving the log empty. It returns the number of segments
// removed. It holds no lock while deciding, only while snapshotting and
// installing, so it does not block concurrent appends.
func (r *Reaper) ReapOnce(now time.Time) int {
	segs := r.log.Segments()
	if len(segs) == 0 {
		return 0
	}
	sort.Slice(segs, func(i, j int) bool {
		return segs[i].BaseOffset() < segs[j].BaseOffset()
	})

	var afterTime []*Segment
	if r.time.MaxAge > 0 {
		for _, s := range segs {
			if r.time.ShouldDelete(s, now) {
				continue
			}
			afterTime = append(afterTime, s)
		}
	} else {
		afterTime = segs
	}
	// Floor: time retention must never empty the log.
	if len(afterTime) == 0 {
		afterTime = []*Segment{segs[len(segs)-1]}
	}

	kept := afterTime
	if r.size.MaxBytes > 0 {
		kept = r.size.ApplyToLog(afterTime)
	}

	r.log.ReplaceSegments(kept)
	return len(segs) - len(kept)
}

// Start launches a goroutine that reaps every interval until Stop is called.
func (r *Reaper) Start(interval time.Duration) {
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.ReapOnce(r.now())
			}
		}
	}()
}

// Stop halts the reaper goroutine and waits for it to exit. No reap is in
// flight once Stop returns.
func (r *Reaper) Stop() {
	close(r.stop)
	<-r.done
}
```

### The runnable demo

The demo appends eight 11-byte segments across eight hours, then runs one reap at hour 8 with a 3-hour age window and a 33-byte cap. Time retention drops the five oldest (hours 0–4, all more than 3 hours old at hour 8), leaving hours 5, 6, 7; the 33-byte cap (three segments fit exactly) leaves all three.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/background-reaper"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	log := retention.NewLog()
	for i := range 8 {
		log.Append(
			[][]byte{[]byte("k")},
			[][]byte{[]byte("0123456789")}, // 11 bytes per segment
			base.Add(time.Duration(i)*time.Hour),
		)
	}
	fmt.Printf("before: %d segments, %d bytes\n", len(log.Segments()), log.TotalSizeBytes())

	now := base.Add(8 * time.Hour)
	r := retention.NewReaper(log,
		retention.TimeRetention{MaxAge: 3 * time.Hour},
		retention.SizeRetention{MaxBytes: 33},
		time.Now,
	)
	removed := r.ReapOnce(now)
	segs := log.Segments()
	fmt.Printf("after reap: %d segments, %d bytes, %d removed\n",
		len(segs), log.TotalSizeBytes(), removed)
	fmt.Printf("oldest surviving base offset: %d\n", segs[0].BaseOffset())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before: 8 segments, 88 bytes
after reap: 3 segments, 33 bytes, 5 removed
oldest surviving base offset: 5
```

### Tests

`TestReapTimeThenSize` checks the composition: time retention prunes the aged segments, then size retention trims the rest to the cap. `TestReapNeverEmpties` drives a log where both policies would otherwise delete everything and asserts the newest segment survives. `TestReaperConcurrent` is the `-race` probe: it starts the ticker reaper, hammers the log with appends from several goroutines, stops the reaper, and asserts the log is non-empty and within budget after a final deterministic reap.

Create `reaper_test.go`:

```go
package retention

import (
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestReapTimeThenSize(t *testing.T) {
	t.Parallel()
	log := NewLog()
	// Four recent 11-byte segments, one per hour ending at the present.
	for i := range 4 {
		log.Append([][]byte{[]byte("k")}, [][]byte{[]byte("0123456789")},
			epoch.Add(time.Duration(i)*time.Hour))
	}
	now := epoch.Add(3 * time.Hour)
	r := NewReaper(log,
		TimeRetention{MaxAge: 24 * time.Hour}, // deletes nothing
		SizeRetention{MaxBytes: 25},           // 44 bytes -> drop oldest to 22
		func() time.Time { return now },
	)
	removed := r.ReapOnce(now)
	segs := log.Segments()
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if segs[0].BaseOffset() != 2 {
		t.Errorf("oldest surviving base offset = %d, want 2", segs[0].BaseOffset())
	}
}

func TestReapNeverEmpties(t *testing.T) {
	t.Parallel()
	log := NewLog()
	// Three segments, all far older than the window.
	for i := range 3 {
		log.Append([][]byte{[]byte("k")}, [][]byte{[]byte("0123456789")},
			epoch.Add(time.Duration(i)*time.Hour))
	}
	now := epoch.Add(100 * time.Hour)
	r := NewReaper(log,
		TimeRetention{MaxAge: 1 * time.Hour}, // would delete all three
		SizeRetention{MaxBytes: 1},           // would delete all but one
		func() time.Time { return now },
	)
	r.ReapOnce(now)
	segs := log.Segments()
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1 (never empty)", len(segs))
	}
	if segs[0].BaseOffset() != 2 {
		t.Errorf("surviving base offset = %d, want 2 (newest)", segs[0].BaseOffset())
	}
}

func TestReaperConcurrent(t *testing.T) {
	t.Parallel()
	log := NewLog()
	r := NewReaper(log,
		TimeRetention{}, // disabled
		SizeRetention{MaxBytes: 50},
		time.Now,
	)
	r.Start(time.Millisecond)

	var wg sync.WaitGroup
	for g := range 6 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range 200 {
				log.Append([][]byte{[]byte("k")}, [][]byte{[]byte("0123456789")}, time.Now())
			}
		}(g)
	}
	wg.Wait()
	r.Stop()

	// One final deterministic reap with no appends in flight.
	r.ReapOnce(time.Now())
	segs := log.Segments()
	if len(segs) == 0 {
		t.Fatal("log emptied by reaper")
	}
	if got := log.TotalSizeBytes(); got > 50 && len(segs) > 1 {
		t.Errorf("total %d bytes exceeds cap with %d segments", got, len(segs))
	}
}
```

## Review

The reaper is correct when it composes time-then-size and never empties the log. Time retention runs first so the size stage works on current data; both stages carry the never-empty floor, so a fully aged-out or wildly over-budget log still keeps its newest segment. The composition mistake is to apply size first and then time, which spends the size budget evicting segments that time retention was about to delete anyway, doing redundant work.

The concurrency is correct when `ReapOnce` holds the lock only to snapshot and to install, never across its sort and filter. `TestReaperConcurrent` under `-race` is the real proof: a reaper that mutated the live segment slice, or that read-modified-wrote it without the `ReplaceSegments` swap, would trip the detector here even though a non-race run would pass. `Stop` joins the goroutine through the done channel, so no reap outlives the call.

## Resources

- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the periodic trigger and the `Stop` contract used to drive and halt the reaper.
- [Kafka: `log.retention.bytes` and `log.retention.ms`](https://kafka.apache.org/documentation/#brokerconfigs_log.retention.bytes) — the two production limits this reaper enforces together.
- [Go Concurrency Patterns](https://go.dev/blog/pipelines) — the stop/done channel idiom for a goroutine you start and later join.

---

Back to [03-log-compaction.md](03-log-compaction.md) | Next: [05-self-compacting-log.md](05-self-compacting-log.md)
