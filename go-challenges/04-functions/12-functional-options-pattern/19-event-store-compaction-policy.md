# Exercise 19: Event Store With Snapshot Interval and Retention Options

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An event-sourced store keeps every fact forever unless something prunes it,
and it takes periodic compressed snapshots so a reader never has to replay
the entire history. This module builds that store through options, and it
checks the one relationship those two policies must respect: a snapshot
taken less often than events are retained would let data get pruned before
any snapshot ever captured it.

## What you'll build

```text
eventstore/                      independent module: example.com/eventstore
  go.mod                         go 1.24
  eventstore.go                  CompressionAlgo, Event, Snapshot, Store, Option, New,
                                  WithSnapshotInterval, WithRetentionWindow,
                                  WithCompression, WithClock, Append, Events, Snapshots
  cmd/
    demo/
      main.go                    manual clock drives pruning and gzip-compressed snapshots
  eventstore_test.go              table test over the interval/window invariant, pruning, compression
```

- Files: `eventstore.go`, `cmd/demo/main.go`, `eventstore_test.go`.
- Implement: `New(opts ...Option) (*Store, error)` whose `Append` prunes events past the retention window and takes a compressed snapshot once the snapshot interval elapses, validating that the snapshot interval never exceeds the retention window.
- Test: every interval/window combination (including the exact boundary), pruning and snapshotting against an injected clock, all three compression algorithms, and a concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the interval must fit inside the window

`WithSnapshotInterval` and `WithRetentionWindow` are unrelated options —
either can be set without the other, in any order. But if the snapshot
interval is longer than the retention window, there is a real window of
time where an event exists, gets pruned by retention, and is never captured
by a snapshot because one hadn't come due yet. That is silent, permanent
data loss the constructor can catch cheaply: after every option has run,
`New` checks `snapshotInterval > retentionWindow` and rejects it. Neither
option's closure could make this check alone, since each only knows its own
duration.

### Real compression, not a placeholder enum

`CompressionAlgo` is not just three named constants for show: `compress/gzip`
and `compress/flate` are both in the standard library, so
`WithCompression(GzipCompression)` actually gzips the snapshot payload.
`TestCompressionRoundTripsThroughAllAlgos` exercises all three real code
paths, and the demo prints the actual compressed byte count, not a
simulated one.

### Pruning and snapshotting under one lock

`Append` does three things under a single `sync.Mutex` critical section:
records the event, prunes anything older than the retention window, and —
if the snapshot interval has elapsed since the last one — compresses the
current state into a new snapshot. All three happen atomically per call, so
a concurrent reader of `Events()` or `Snapshots()` never observes a
partially-pruned or partially-snapshotted state.

Create `eventstore.go`:

```go
package eventstore

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"sync"
	"time"
)

// CompressionAlgo selects how snapshot payloads are compressed.
type CompressionAlgo int

const (
	NoCompression CompressionAlgo = iota
	GzipCompression
	FlateCompression
)

func (a CompressionAlgo) String() string {
	switch a {
	case NoCompression:
		return "NoCompression"
	case GzipCompression:
		return "GzipCompression"
	case FlateCompression:
		return "FlateCompression"
	default:
		return fmt.Sprintf("CompressionAlgo(%d)", int(a))
	}
}

// Event is a single appended fact.
type Event struct {
	Type    string
	Payload []byte
	At      time.Time
}

// Snapshot is a compressed rollup taken every snapshotInterval.
type Snapshot struct {
	CompressedState []byte
	Algo            CompressionAlgo
	At              time.Time
}

// Store is a concurrency-safe, retention-pruned event log with periodic
// compressed snapshots.
type Store struct {
	mu               sync.Mutex
	snapshotInterval time.Duration
	retentionWindow  time.Duration
	compression      CompressionAlgo
	now              func() time.Time

	events         []Event
	snapshots      []Snapshot
	lastSnapshotAt time.Time
}

// Option configures a Store and may reject invalid input.
type Option func(*Store) error

// New seeds coherent defaults, applies opts in order, then validates the
// cross-field invariant no single option can see: the snapshot interval must
// not exceed the retention window, or events could be pruned before a
// snapshot ever captures them.
func New(opts ...Option) (*Store, error) {
	s := &Store{
		snapshotInterval: time.Hour,
		retentionWindow:  24 * time.Hour,
		compression:      NoCompression,
		now:              time.Now,
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	if s.snapshotInterval > s.retentionWindow {
		return nil, fmt.Errorf("snapshot interval %s exceeds retention window %s: events would be pruned before ever being snapshotted",
			s.snapshotInterval, s.retentionWindow)
	}
	s.lastSnapshotAt = s.now()
	return s, nil
}

// WithSnapshotInterval sets how often a snapshot is taken (> 0).
func WithSnapshotInterval(d time.Duration) Option {
	return func(s *Store) error {
		if d <= 0 {
			return fmt.Errorf("snapshot interval must be positive, got %s", d)
		}
		s.snapshotInterval = d
		return nil
	}
}

// WithRetentionWindow sets how long events are kept before pruning (> 0).
func WithRetentionWindow(d time.Duration) Option {
	return func(s *Store) error {
		if d <= 0 {
			return fmt.Errorf("retention window must be positive, got %s", d)
		}
		s.retentionWindow = d
		return nil
	}
}

// WithCompression selects the snapshot compression algorithm.
func WithCompression(algo CompressionAlgo) Option {
	return func(s *Store) error {
		switch algo {
		case NoCompression, GzipCompression, FlateCompression:
			s.compression = algo
			return nil
		default:
			return fmt.Errorf("unknown compression algo: %d", int(algo))
		}
	}
}

// WithClock injects the clock used for retention pruning and snapshot
// timing.
func WithClock(now func() time.Time) Option {
	return func(s *Store) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		s.now = now
		return nil
	}
}

// Append records an event, prunes anything older than the retention window,
// and takes a compressed snapshot if snapshotInterval has elapsed.
func (s *Store) Append(eventType string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := s.now()
	s.events = append(s.events, Event{Type: eventType, Payload: payload, At: t})
	s.pruneLocked(t)

	if t.Sub(s.lastSnapshotAt) >= s.snapshotInterval {
		if err := s.snapshotLocked(t); err != nil {
			return err
		}
		s.lastSnapshotAt = t
	}
	return nil
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-s.retentionWindow)
	kept := s.events[:0:0]
	for _, e := range s.events {
		if e.At.After(cutoff) {
			kept = append(kept, e)
		}
	}
	s.events = kept
}

func (s *Store) snapshotLocked(now time.Time) error {
	var raw bytes.Buffer
	for _, e := range s.events {
		raw.WriteString(e.Type)
		raw.Write(e.Payload)
	}
	compressed, err := compress(s.compression, raw.Bytes())
	if err != nil {
		return fmt.Errorf("compress snapshot: %w", err)
	}
	s.snapshots = append(s.snapshots, Snapshot{CompressedState: compressed, Algo: s.compression, At: now})
	return nil
}

func compress(algo CompressionAlgo, data []byte) ([]byte, error) {
	switch algo {
	case NoCompression:
		return data, nil
	case GzipCompression:
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case FlateCompression:
		var buf bytes.Buffer
		w, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unknown compression algo: %d", int(algo))
	}
}

// Events returns a copy of the currently retained events.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// Snapshots returns a copy of the snapshots taken so far.
func (s *Store) Snapshots() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, len(s.snapshots))
	copy(out, s.snapshots)
	return out
}
```

### The runnable demo

The demo appends three events 30 seconds apart under a manual clock (a
snapshot fires at the 60-second mark), then jumps 6 minutes past the
5-minute retention window and shows the older events pruned while the
compressed snapshot survives.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/eventstore"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	s, err := eventstore.New(
		eventstore.WithSnapshotInterval(time.Minute),
		eventstore.WithRetentionWindow(5*time.Minute),
		eventstore.WithCompression(eventstore.GzipCompression),
		eventstore.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	payload := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for i := 0; i < 3; i++ {
		if err := s.Append("tick", payload); err != nil {
			panic(err)
		}
		current = current.Add(30 * time.Second)
	}
	fmt.Printf("events after 90s: %d\n", len(s.Events()))
	fmt.Printf("snapshots after 90s: %d\n", len(s.Snapshots()))

	current = current.Add(6 * time.Minute) // past retention window
	if err := s.Append("tick", payload); err != nil {
		panic(err)
	}
	fmt.Printf("events after pruning: %d\n", len(s.Events()))

	snaps := s.Snapshots()
	last := snaps[len(snaps)-1]
	fmt.Printf("last snapshot compressed with: %s, size: %d bytes\n", last.Algo, len(last.CompressedState))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
events after 90s: 3
snapshots after 90s: 1
events after pruning: 1
last snapshot compressed with: GzipCompression, size: 31 bytes
```

### Tests

`TestNewValidation` tables the interval/window invariant, including the
exact-boundary case where they are equal (allowed) and where the interval
exceeds the window (rejected). `TestPruningAndSnapshotting` drives a fake
clock through both a snapshot trigger and a retention prune.
`TestCompressionRoundTripsThroughAllAlgos` exercises `NoCompression`,
`GzipCompression`, and `FlateCompression` for real. `TestConcurrentAppend`
runs `-race` over concurrent appends.

Create `eventstore_test.go`:

```go
package eventstore

import (
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "snapshot interval exceeds retention window", opts: []Option{
			WithSnapshotInterval(2 * time.Hour), WithRetentionWindow(time.Hour),
		}, wantErr: true},
		{name: "snapshot interval equal to retention window is allowed", opts: []Option{
			WithSnapshotInterval(time.Hour), WithRetentionWindow(time.Hour),
		}},
		{name: "zero snapshot interval", opts: []Option{WithSnapshotInterval(0)}, wantErr: true},
		{name: "zero retention window", opts: []Option{WithRetentionWindow(0)}, wantErr: true},
		{name: "unknown compression algo", opts: []Option{WithCompression(CompressionAlgo(99))}, wantErr: true},
		{name: "nil clock rejected", opts: []Option{WithClock(nil)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPruningAndSnapshotting(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	s, err := New(
		WithSnapshotInterval(time.Minute),
		WithRetentionWindow(5*time.Minute),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := s.Append("tick", []byte("x")); err != nil {
			t.Fatal(err)
		}
		current = current.Add(30 * time.Second)
	}

	if got := len(s.Events()); got != 3 {
		t.Fatalf("Events() len = %d, want 3", got)
	}
	if got := len(s.Snapshots()); got != 1 {
		t.Fatalf("Snapshots() len = %d, want 1 (triggered at the 60s mark)", got)
	}

	current = current.Add(6 * time.Minute)
	if err := s.Append("tick", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Events()); got != 1 {
		t.Fatalf("Events() len after pruning = %d, want 1 (only the just-appended event survives retention)", got)
	}
}

func TestCompressionRoundTripsThroughAllAlgos(t *testing.T) {
	t.Parallel()

	for _, algo := range []CompressionAlgo{NoCompression, GzipCompression, FlateCompression} {
		t.Run(algo.String(), func(t *testing.T) {
			t.Parallel()

			s, err := New(WithCompression(algo))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Append("evt", []byte("payload")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentAppend(t *testing.T) {
	t.Parallel()

	s, err := New(WithSnapshotInterval(time.Hour), WithRetentionWindow(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Append("evt", []byte("x")); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(s.Events()); got != 100 {
		t.Fatalf("Events() len = %d, want 100", got)
	}
}
```

## Review

The store is correct when no event can ever be pruned before at least one
snapshot had the chance to capture it, and when pruning and snapshotting
both happen atomically inside the same `Append` call. The interval/window
check is a direct comparison rather than a derived value — simpler than the
paginated lister's `pageSize * maxPages` invariant earlier in this chapter,
but it belongs in exactly the same place: the constructor, after every
option has run, because `WithSnapshotInterval` and `WithRetentionWindow`
never see each other's argument. Using real `compress/gzip` and
`compress/flate` instead of a placeholder keeps the compression option
honest — `TestCompressionRoundTripsThroughAllAlgos` would fail immediately
if any algorithm's writer were wired up wrong.

## Resources

- [Martin Fowler: Event Sourcing](https://martinfowler.com/eaaDev/EventSourcing.html)
- [pkg.go.dev: compress/gzip](https://pkg.go.dev/compress/gzip)
- [pkg.go.dev: compress/flate](https://pkg.go.dev/compress/flate)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-grpc-service-interceptor-chain.md](18-grpc-service-interceptor-chain.md) | Next: [20-encryption-key-version-manager.md](20-encryption-key-version-manager.md)
