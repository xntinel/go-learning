# Exercise 6: Replayable Source (At-Least-Once With Offset Commit)

Delivery semantics are where a stream source earns its keep. This `ReplayableSource` is backed by an append-only log of offset-tagged records; a consumer acknowledges progress with `Commit(offset)`, and each `Open` replays from the first uncommitted offset. The result is at-least-once delivery: a consumer that crashes before committing sees those records again.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
replayable-source/
  go.mod
  source.go                   Record (with Offset), Metrics, Source, ErrSourceClosed
  replayable_source.go        append-only log, Commit, replay-from-committed Open
  replayable_source_test.go   redelivery on reopen, monotonic commit, fully-committed
  cmd/demo/main.go            two sessions showing at-least-once redelivery
```

- Files: `source.go`, `replayable_source.go`, `replayable_source_test.go`, `cmd/demo/main.go`.
- Implement: `ReplayableSource` with `Append`, `Commit`, `Committed`, and an `Open` that replays from `committed+1`.
- Test: records read but not committed are redelivered on the next `Open`; a stale commit never rewinds the frontier; a fully-committed log replays nothing.
- Verify: `go test -race ./...`

### An offset on every record

This module's `Record` adds one field the others do not have: `Offset`, the record's position in the durable log. The offset is the entire basis of replay — it is the small piece of state that, together with the log, determines exactly what to deliver next after a restart. `Metrics.BacklogSize` reports how many records remain uncommitted, the lag a monitoring system watches.

Create `source.go`:

```go
package replayablesource

import (
	"context"
	"errors"
	"time"
)

// Record is the atomic unit flowing through the pipeline. Offset is the record's
// position in the durable log; the consumer commits an offset to acknowledge
// every record up to and including it.
type Record struct {
	Offset    int64
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Source    string
	Metadata  map[string]string
}

// Metrics is a point-in-time snapshot of a source's counters.
type Metrics struct {
	RecordsEmitted int64
	BytesRead      int64
	ErrorsTotal    int64
	BacklogSize    int64
}

// ErrSourceClosed is returned by Close when the source was never opened.
var ErrSourceClosed = errors.New("replayablesource: source not open")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

### The at-least-once rule: commit only on acknowledgement

The source holds an in-memory append-only log (`[]Record`) and a `committed` offset that starts at `-1`, meaning nothing has been acknowledged. `Append` assigns the next offset and stores the record; in a real system this is the producer writing durably to disk, and the in-memory slice stands in for that log.

`Open` is where replay happens. Under the lock it computes `start := committed + 1` and snapshots `log[start:]` — every record after the commit point — then streams that snapshot on a fresh goroutine and closes the channel when the backlog is drained. So each `Open` is a bounded replay of exactly the uncommitted suffix. The consumer reads records, does its work, and calls `Commit(offset)` to advance the frontier. `Commit` is strictly monotonic: `if offset > committed { committed = offset }`, so a duplicate or out-of-order acknowledgement can never move the commit point backward and cause already-acknowledged records to replay forever.

The at-least-once guarantee falls directly out of this rule. If a consumer reads offsets 0–4 but commits only through 1 and then crashes (or simply closes), the next `Open` snapshots from offset 2 and redelivers 2, 3, and 4 — the records that were delivered but never acknowledged. The duplicate is the price of never losing a record, and it is exactly the contract Kafka consumer offsets provide. A re-open also has to be safe against the previous session's goroutines, so `Close` waits on a per-open `done` channel that the closer signals last, rather than racing the `WaitGroup` that the next `Open` will reuse.

Create `replayable_source.go`:

```go
package replayablesource

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ReplayableSource is a seekable, at-least-once source backed by an append-only
// in-memory log. Each Open replays every record after the last committed offset,
// so a consumer that crashes before committing sees those records again on the
// next Open. The consumer advances the commit point with Commit.
type ReplayableSource struct {
	bufferSize int

	mu        sync.Mutex
	log       []Record
	committed int64 // highest acknowledged offset; -1 means nothing committed

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	done    chan struct{}
	records chan Record
	errs    chan error

	emitted atomic.Int64
	bytes   atomic.Int64
}

// NewReplayableSource creates an empty source. committed starts at -1 so the
// first Open replays from offset 0.
func NewReplayableSource(bufferSize int) *ReplayableSource {
	return &ReplayableSource{bufferSize: bufferSize, committed: -1}
}

// Append durably records value in the log and returns its assigned offset.
// In a real system this is the producer side writing to disk; here it appends
// to the in-memory slice under the lock.
func (rs *ReplayableSource) Append(value []byte) int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	off := int64(len(rs.log))
	rs.log = append(rs.log, Record{
		Offset:    off,
		Value:     append([]byte(nil), value...),
		Timestamp: time.Now().UTC(),
		Source:    "replay",
	})
	return off
}

// Commit advances the durable commit point to offset, acknowledging every
// record up to and including it. A commit below the current point is ignored so
// duplicate or out-of-order acknowledgements never move the frontier backwards.
func (rs *ReplayableSource) Commit(offset int64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if offset > rs.committed {
		rs.committed = offset
	}
}

// Committed returns the current commit point.
func (rs *ReplayableSource) Committed() int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.committed
}

func (rs *ReplayableSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	rs.cancel = cancel
	records := make(chan Record, rs.bufferSize)
	errs := make(chan error, 4)
	rs.records = records
	rs.errs = errs

	rs.mu.Lock()
	start := rs.committed + 1
	snapshot := make([]Record, len(rs.log[start:]))
	copy(snapshot, rs.log[start:])
	rs.mu.Unlock()

	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		for _, r := range snapshot {
			rs.bytes.Add(int64(len(r.Value)))
			select {
			case records <- r:
				rs.emitted.Add(1)
			case <-inner.Done():
				return
			}
		}
	}()

	rs.done = make(chan struct{})
	done := rs.done
	go func() {
		rs.wg.Wait()
		close(records)
		close(errs)
		close(done)
	}()

	return records, errs
}

func (rs *ReplayableSource) Close() error {
	if rs.cancel == nil {
		return ErrSourceClosed
	}
	rs.cancel()
	<-rs.done
	rs.cancel = nil
	return nil
}

func (rs *ReplayableSource) Metrics() Metrics {
	rs.mu.Lock()
	backlog := int64(len(rs.log)) - (rs.committed + 1)
	rs.mu.Unlock()
	return Metrics{
		RecordsEmitted: rs.emitted.Load(),
		BytesRead:      rs.bytes.Load(),
		BacklogSize:    backlog,
	}
}

var _ Source = (*ReplayableSource)(nil)
```

### The runnable demo

The demo appends five records, then runs two sessions. Session 1 consumes all five but commits only through offset 1. Session 2 reopens and replays — and prints offsets 2, 3, 4, the redelivery that makes the guarantee "at-least-once" rather than "at-most-once."

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	rp "example.com/replayable-source"
)

func main() {
	rs := rp.NewReplayableSource(16)
	for _, v := range []string{"a", "b", "c", "d", "e"} {
		rs.Append([]byte(v))
	}

	// Session 1: consume all, but only acknowledge through offset 1.
	fmt.Println("session 1:")
	recs, _ := rs.Open(context.Background())
	for r := range recs {
		fmt.Printf("  offset=%d value=%s\n", r.Offset, r.Value)
		if r.Offset == 1 {
			rs.Commit(r.Offset)
		}
	}
	rs.Close()
	fmt.Printf("committed through offset %d\n", rs.Committed())

	// Session 2: the source replays from the first uncommitted offset.
	fmt.Println("session 2 (replay, at-least-once):")
	recs2, _ := rs.Open(context.Background())
	for r := range recs2 {
		fmt.Printf("  offset=%d value=%s\n", r.Offset, r.Value)
	}
	rs.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
session 1:
  offset=0 value=a
  offset=1 value=b
  offset=2 value=c
  offset=3 value=d
  offset=4 value=e
committed through offset 1
session 2 (replay, at-least-once):
  offset=2 value=c
  offset=3 value=d
  offset=4 value=e
```

### Tests

`TestReplayFromCommitted` is the core property: it consumes five records committing only through offset 1, reopens and asserts exactly offsets 2–4 are redelivered, then commits through 4 and asserts a third session replays nothing. `TestCommitMonotonic` commits offset 1 then offset 0 and asserts the frontier stays at 1 — a stale ack is ignored. `TestCloseBeforeOpen` asserts the sentinel.

Create `replayable_source_test.go`:

```go
package replayablesource

import (
	"context"
	"testing"
	"time"
)

func collect(ch <-chan Record, max int, timeout time.Duration) []Record {
	var out []Record
	deadline := time.After(timeout)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
			if len(out) >= max {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

// TestReplayFromCommitted verifies at-least-once: records read but not committed
// in one session are redelivered in the next.
func TestReplayFromCommitted(t *testing.T) {
	t.Parallel()

	rs := NewReplayableSource(16)
	for i := 0; i < 5; i++ {
		rs.Append([]byte{byte('a' + i)})
	}

	// Session 1: read all 5, but only commit through offset 1.
	recs, _ := rs.Open(context.Background())
	got := collect(recs, 5, time.Second)
	if len(got) != 5 {
		t.Fatalf("session1 got %d, want 5", len(got))
	}
	if got[0].Offset != 0 || got[4].Offset != 4 {
		t.Fatalf("session1 offsets = %d..%d, want 0..4", got[0].Offset, got[4].Offset)
	}
	rs.Commit(1)
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}

	// Session 2: replay must resume at offset 2 (committed+1), redelivering 2,3,4.
	recs2, _ := rs.Open(context.Background())
	got2 := collect(recs2, 5, time.Second)
	if len(got2) != 3 {
		t.Fatalf("session2 got %d, want 3 (redelivery of 2,3,4)", len(got2))
	}
	if got2[0].Offset != 2 {
		t.Errorf("session2 first offset = %d, want 2", got2[0].Offset)
	}
	rs.Commit(4)
	rs.Close()

	// Session 3: everything committed, nothing to replay.
	recs3, _ := rs.Open(context.Background())
	got3 := collect(recs3, 5, time.Second)
	if len(got3) != 0 {
		t.Errorf("session3 got %d, want 0 (all committed)", len(got3))
	}
	rs.Close()
}

// TestCommitMonotonic verifies a lower offset never moves the frontier back.
func TestCommitMonotonic(t *testing.T) {
	t.Parallel()

	rs := NewReplayableSource(4)
	rs.Append([]byte("x"))
	rs.Append([]byte("y"))
	rs.Commit(1)
	rs.Commit(0) // stale ack, must be ignored
	if c := rs.Committed(); c != 1 {
		t.Errorf("Committed = %d, want 1", c)
	}
}

// TestCloseBeforeOpen verifies the sentinel.
func TestCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	rs := NewReplayableSource(1)
	if err := rs.Close(); err != ErrSourceClosed {
		t.Errorf("Close = %v, want %v", err, ErrSourceClosed)
	}
}
```

## Review

The source is correct when replay resumes from `committed+1` on every `Open` and the commit point only ever moves forward. Confirm `Append`, `Commit`, and the `Open` snapshot all touch the log and `committed` under the same lock, that `Commit` ignores any offset not greater than the current one, and that `Close` waits on the per-open `done` channel so a re-open never collides with the prior session's closer. The common mistakes are advancing the commit point at delivery time instead of on acknowledgement (which silently degrades the guarantee to at-most-once), letting a stale or reordered ack rewind the frontier (which replays committed records forever), and reusing the `WaitGroup` across opens without a barrier. The redelivery and monotonic-commit tests under `-race` pin all three.

## Resources

- [Kafka consumer offsets and delivery semantics](https://kafka.apache.org/documentation/#semantics) — the production model this source distills: committed offsets, replay, and at-least-once.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the log and the commit point so concurrent `Append`/`Commit`/`Open` stay consistent.
- [Designing Data-Intensive Applications — exactly-once and idempotence](https://martin.kleppmann.com/2015/05/27/logs-for-data-infrastructure.html) — why at-least-once plus idempotent consumers is the pragmatic target.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-generator-source.md](05-generator-source.md) | Next: [07-rate-limited-source.md](07-rate-limited-source.md)
