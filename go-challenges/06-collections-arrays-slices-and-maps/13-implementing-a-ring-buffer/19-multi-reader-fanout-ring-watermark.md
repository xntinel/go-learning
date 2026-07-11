# Exercise 19: Multi-Reader Fan-Out Ring Gated by the Slowest Reader

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every ring buffer earlier in this lesson assumes one consumer: a single
`tail` cursor that `Pop` advances, so eviction only has to ask "has the one
reader seen this slot yet?" A config-change broadcaster, a metrics fan-out
to several exporters, or a replicated log with more than one subscriber
needs a different answer, because more than one reader must see every
pushed value, not split it between them. This is exactly the problem the
LMAX Disruptor's multi-consumer `RingBuffer` solves by tracking a "gating
sequence" per consumer and refusing to let the producer lap the slowest one,
and the problem a Kafka partition's retention watermark solves by never
expiring a segment until every subscribed consumer group has moved past it.

The trap is that this looks like a small extension of a single-tail ring
and is not. A team under deadline pressure bolts "multiple readers" onto an
existing single-tail ring by having several goroutines call the same
`Pop` -- and it compiles, it passes a smoke test with one reader, and it
silently turns a broadcast into a work queue: whichever goroutine calls
`Pop` first steals that item, and the next caller gets the *next* item, not
the *same* one. With N readers polling a shared tail, each pushed value
reaches exactly one of them, and which one is essentially random. A
config-reload notification meant for every subscriber reaches only one,
and which one varies run to run -- one of the most deceptive silent-data-loss
bugs a ring buffer can produce, because every individual read still looks
correct.

This module builds `FanoutRing[T]`, a ring with one cursor per subscribed
reader instead of one shared tail, where `Push` refuses to overwrite a slot
any current reader has not yet consumed. The shared-tail failure mode is
never part of the API; it exists only as an unexported type in the test
file, contrasted against the broadcast contract `FanoutRing` actually
provides.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
fanoutring/                module example.com/fanoutring
  go.mod                   go 1.24
  fanoutring.go             FanoutRing[T], ReaderID; New, Subscribe, Unsubscribe, Push, Next,
                            Backlog; three sentinel errors
  fanoutring_test.go        broadcast+late-subscriber table, gating/unsubscribe edges,
                            shared-tail contrast, concurrent producer/readers, ExampleFanoutRing
```

- Files: `fanoutring.go`, `fanoutring_test.go`.
- Implement: `New[T any](capacity int) (*FanoutRing[T], error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*FanoutRing[T]).Subscribe() ReaderID`; `(*FanoutRing[T]).Unsubscribe(id ReaderID) error`; `(*FanoutRing[T]).Push(v T) error` returning `ErrWouldLapSlowestReader` when it would overwrite an unconsumed slot; `(*FanoutRing[T]).Next(id ReaderID) (T, bool, error)`; `(*FanoutRing[T]).Backlog(id ReaderID) (int, error)`.
- Test: every subscribed reader receives every value in order, a late subscriber sees only future ones; zero readers never gates `Push`, one reader does, draining or unsubscribing it frees capacity; a shared-tail queue contrasted against the broadcast contract, pinning that it splits values between readers instead of delivering all of them to all of them; a concurrent producer and two readers checked under `-race`; and `ExampleFanoutRing` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanoutring
cd ~/go-exercises/fanoutring
go mod init example.com/fanoutring
go mod edit -go=1.24
```

### One cursor per reader, and why a shared tail silently becomes a queue

A single-tail ring answers "is this slot safe to overwrite?" with one
comparison: has `tail` passed it. A fan-out ring must ask the same question
of *every* subscribed reader, because none of them may lose a value the
others have not seen yet. `FanoutRing` tracks each reader's position in a
`map[ReaderID]uint64` and computes the minimum before every `Push`:

```go
min := r.head
for _, pos := range r.readers {
    if pos < min {
        min = pos
    }
}
if r.head-min >= r.capacity {
    return ErrWouldLapSlowestReader
}
```

The failure mode this replaces is not a crash, which is why it survives
review: reuse the single-tail `Ring[T]` from earlier in this lesson and let
several goroutines call its one `Pop` concurrently, thinking "multiple
readers" just means "multiple callers":

```go
// What "just let two goroutines call Pop" actually gives you.
go func() { for { v, _ := ring.Pop(); handleA(v) } }()
go func() { for { v, _ := ring.Pop(); handleB(v) } }()
```

Every value in the ring has exactly one `tail`, so exactly one of those two
goroutines gets each value -- never both. Ten pushed values become five
delivered to A and five to B, not ten delivered to each. `FanoutRing`'s fix
is structural, not a smarter `Pop`: give every reader its own cursor, so
`Next(id)` reads from `readers[id]` instead of a value shared across every
caller, and a value is only ever evicted once the slowest of those cursors
has moved past it.

Create `fanoutring.go`:

```go
// Package fanoutring implements a ring buffer with independent per-reader
// cursors: every registered reader sees every pushed value exactly once, in
// order, instead of values being split across readers like a work queue.
// This is the shape of the LMAX Disruptor's multi-consumer RingBuffer,
// which tracks each consumer's "gating sequence" so the producer never
// overwrites a slot a slow consumer has not read, and of a Kafka
// partition's retention watermark, which cannot advance past the slowest
// consumer group. See the tests for what breaks when a "multi-reader" ring
// is built by sharing one single-tail Pop across goroutines instead.
package fanoutring

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors returned by New, Push, and Next.
var (
	// ErrInvalidCapacity means capacity was not positive.
	ErrInvalidCapacity = errors.New("fanoutring: capacity must be positive")
	// ErrWouldLapSlowestReader means Push would overwrite a slot the
	// slowest registered reader has not yet consumed.
	ErrWouldLapSlowestReader = errors.New("fanoutring: push would lap the slowest reader")
	// ErrUnknownReader means the ReaderID was never issued or was unsubscribed.
	ErrUnknownReader = errors.New("fanoutring: unknown reader id")
)

// ReaderID identifies one subscriber's independent read cursor.
type ReaderID int

// FanoutRing is a fixed-capacity ring buffer that broadcasts every pushed
// value to every subscribed reader, each tracked by its own cursor.
//
// A reader that Subscribes sees only values pushed after it subscribes, not
// history, the same as a consumer tailing a Kafka partition from its
// high-water mark. Push refuses to overwrite a slot any current reader has
// not consumed, returning ErrWouldLapSlowestReader; the caller retries,
// drops the value, or Unsubscribes a stalled reader to free capacity. With
// zero readers subscribed, nothing gates Push.
//
// Concurrency contract: safe for concurrent use; every operation is
// guarded by one mutex.
type FanoutRing[T any] struct {
	mu           sync.Mutex
	buf          []T
	capacity     uint64
	head         uint64
	readers      map[ReaderID]uint64
	nextReaderID ReaderID
}

// New returns a FanoutRing with room for capacity elements, or
// ErrInvalidCapacity if capacity is not positive.
func New[T any](capacity int) (*FanoutRing[T], error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &FanoutRing[T]{
		buf:      make([]T, capacity),
		capacity: uint64(capacity),
		readers:  make(map[ReaderID]uint64),
	}, nil
}

// Subscribe registers a new reader whose cursor starts at the current head,
// so it observes only values pushed from now on, and returns its ReaderID.
func (r *FanoutRing[T]) Subscribe() ReaderID {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextReaderID
	r.nextReaderID++
	r.readers[id] = r.head
	return id
}

// Unsubscribe removes a reader's cursor, which may let Push proceed past a
// point it had not reached -- as a slow Kafka group leaving a topic frees
// that partition's retention floor. Returns ErrUnknownReader if id is unknown.
func (r *FanoutRing[T]) Unsubscribe(id ReaderID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.readers[id]; !ok {
		return fmt.Errorf("%w: %d", ErrUnknownReader, id)
	}
	delete(r.readers, id)
	return nil
}

// Push appends v for every current and future-subscribed reader to see.
// Returns ErrWouldLapSlowestReader, wrapped with how far behind, if writing
// v would overwrite a slot the slowest reader has not consumed.
func (r *FanoutRing[T]) Push(v T) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	min := r.head
	for _, pos := range r.readers {
		if pos < min {
			min = pos
		}
	}
	if r.head-min >= r.capacity {
		return fmt.Errorf("%w: slowest reader is %d slots behind", ErrWouldLapSlowestReader, r.head-min)
	}
	r.buf[r.head%r.capacity] = v
	r.head++
	return nil
}

// Next returns the next value reader id has not consumed; ok is false if
// it caught up (not an error). Returns ErrUnknownReader if id is unknown.
func (r *FanoutRing[T]) Next(id ReaderID) (T, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var zero T
	pos, ok := r.readers[id]
	if !ok {
		return zero, false, fmt.Errorf("%w: %d", ErrUnknownReader, id)
	}
	if pos == r.head {
		return zero, false, nil
	}
	v := r.buf[pos%r.capacity]
	r.readers[id] = pos + 1
	return v, true, nil
}

// Backlog reports how many unread values are pending for reader id, or
// ErrUnknownReader if id is unknown.
func (r *FanoutRing[T]) Backlog(id ReaderID) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pos, ok := r.readers[id]
	if !ok {
		return 0, fmt.Errorf("%w: %d", ErrUnknownReader, id)
	}
	return int(r.head - pos), nil
}
```

### Using it

`Subscribe` is the entry point: it returns a `ReaderID` whose cursor starts
at the ring's current write position, so a reader never sees values pushed
before it joined -- matching a consumer that starts tailing a Kafka
partition from its high-water mark rather than replaying history. `Push`
returns `ErrWouldLapSlowestReader`, wrapped with how far behind that reader
is, which gives the caller enough information to decide whether to retry,
drop the value, or forcibly `Unsubscribe` a reader that has stopped
consuming -- exactly the operational choice an operator makes when a
consumer group falls permanently behind a Kafka topic. `Backlog` answers
"how far behind is this particular reader," the same number a consumer-lag
dashboard reports per partition per group.

The type is safe for concurrent use: a single mutex guards every method, so
one producer goroutine and any number of reader goroutines may call `Push`,
`Subscribe`, `Next`, and `Unsubscribe` at once. `ExampleFanoutRing`, shown
in full in the Tests section below, subscribes two readers before pushing
three values and shows both draining all three independently -- the
broadcast contract in miniature. `go test` runs it and compares its stdout
against the `// Output:` comment.

### Tests

`TestBroadcastAndLateSubscriber` is the table: it pushes values before and
after a second reader subscribes, and asserts the early reader sees
everything while the late one sees only what came after it joined --
proving broadcast, not split delivery. `TestPushGatingAndUnsubscribe` is the
edge case this ring exists for: zero readers never gates `Push`; once a
reader is subscribed and the ring fills to its position, `Push` is refused
with `ErrWouldLapSlowestReader`; draining that reader by one slot, or
removing it with `Unsubscribe`, frees exactly the capacity it was holding
back; and every method returns `ErrUnknownReader` for an id that was never
issued.

`TestSharedTailQueueSplitsInsteadOfBroadcasting` is the module's antipattern
contrast: `sharedTailQueue` is unexported and unreachable from
`FanoutRing`'s API, reusing a single-tail ring's shape with one `next()`
shared by two simulated readers. It pins the exact failure this module
avoids: the ten values pushed split between the two callers -- never all
ten reaching both. `TestConcurrentProducerAndReaders` exercises the
declared concurrency contract for real: one producer and two reader
goroutines driving `Push`/`Next` at once, checked under `-race`, each
reader ending with every one of the 2,000 pushed values.

Create `fanoutring_test.go`:

```go
package fanoutring

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
)

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, -1, -7} {
		if _, err := New[int](capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) err = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func drain[T any](t *testing.T, r *FanoutRing[T], id ReaderID) []T {
	t.Helper()
	var got []T
	for {
		v, ok, err := r.Next(id)
		if err != nil {
			t.Fatalf("Next(%d): %v", id, err)
		}
		if !ok {
			return got
		}
		got = append(got, v)
	}
}

// TestBroadcastAndLateSubscriber is the table: every subscribed reader
// sees every value, in order, and a late subscriber sees only future ones.
func TestBroadcastAndLateSubscriber(t *testing.T) {
	t.Parallel()
	r, err := New[int](8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := r.Subscribe()
	for i := 1; i <= 3; i++ {
		if err := r.Push(i); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	b := r.Subscribe()
	if got := drain(t, r, b); len(got) != 0 {
		t.Fatalf("late subscriber saw %v, want none of the pre-existing history", got)
	}
	if err := r.Push(4); err != nil {
		t.Fatalf("Push(4): %v", err)
	}
	if got := drain(t, r, a); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("reader a got %v, want [1 2 3 4]", got)
	}
	if got := drain(t, r, b); !slices.Equal(got, []int{4}) {
		t.Fatalf("late reader b got %v, want [4]", got)
	}
}

// TestPushGatingAndUnsubscribe covers the edge that gives this ring its
// name: zero readers never gates Push; once one is subscribed, Push must
// refuse to overwrite a slot it has not consumed, and draining that
// reader, or removing it, must free the slot.
func TestPushGatingAndUnsubscribe(t *testing.T) {
	t.Parallel()
	r, err := New[int](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Push(0); err != nil {
		t.Fatalf("Push(0) with no readers: %v", err)
	}
	slow, fast := r.Subscribe(), r.Subscribe()
	for i := 1; i <= 3; i++ {
		if err := r.Push(i); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	if err := r.Push(4); !errors.Is(err, ErrWouldLapSlowestReader) {
		t.Fatalf("Push(4) err = %v, want ErrWouldLapSlowestReader", err)
	}
	drain(t, r, fast) // fast alone draining changes nothing: slow is the gate
	if err := r.Push(4); !errors.Is(err, ErrWouldLapSlowestReader) {
		t.Fatalf("Push(4) after draining fast err = %v, want still gated", err)
	}
	if _, _, err := r.Next(slow); err != nil { // frees exactly one slot
		t.Fatalf("Next: %v", err)
	}
	if err := r.Push(4); err != nil {
		t.Fatalf("Push(4) after drain: %v", err)
	}
	if err := r.Push(5); !errors.Is(err, ErrWouldLapSlowestReader) {
		t.Fatalf("Push(5) err = %v, want ErrWouldLapSlowestReader (slow behind again)", err)
	}
	if err := r.Unsubscribe(slow); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if err := r.Push(5); err != nil {
		t.Fatalf("Push(5) after unsubscribing slow: %v", err)
	}
	bogus := ReaderID(999)
	if _, _, err := r.Next(bogus); !errors.Is(err, ErrUnknownReader) {
		t.Errorf("Next(bogus) err = %v, want ErrUnknownReader", err)
	}
	if err := r.Unsubscribe(bogus); !errors.Is(err, ErrUnknownReader) {
		t.Errorf("Unsubscribe(bogus) err = %v, want ErrUnknownReader", err)
	}
}

// sharedTailQueue is the naive way to bolt "multiple readers" onto a
// single-tail ring: several callers share one next(). It is never
// reachable from FanoutRing's API; it exists to pin what goes wrong.
type sharedTailQueue struct {
	buf      []int
	capacity int
	head     int
	tail     int // the one read cursor, shared by every caller of next
}

func newSharedTailQueue(capacity int) *sharedTailQueue {
	return &sharedTailQueue{buf: make([]int, capacity), capacity: capacity}
}

func (q *sharedTailQueue) push(v int) { q.buf[q.head%q.capacity] = v; q.head++ }

func (q *sharedTailQueue) next() (int, bool) {
	if q.tail == q.head {
		return 0, false
	}
	v := q.buf[q.tail%q.capacity]
	q.tail++
	return v, true
}

// TestSharedTailQueueSplitsInsteadOfBroadcasting is the heart of the
// module: with one shared tail, each pushed value goes to exactly one
// caller of next, so two "readers" alternating calls split m values
// between them instead of each seeing all m -- a work queue, not a
// fan-out log. The broadcast test above already pins the fix.
func TestSharedTailQueueSplitsInsteadOfBroadcasting(t *testing.T) {
	t.Parallel()
	const m = 10
	q := newSharedTailQueue(m)
	for i := 1; i <= m; i++ {
		q.push(i)
	}
	var readerA, readerB int
	for i := 0; i < m; i++ {
		if i%2 == 0 {
			if _, ok := q.next(); ok {
				readerA++
			}
		} else if _, ok := q.next(); ok {
			readerB++
		}
	}
	if readerA+readerB != m {
		t.Fatalf("readerA+readerB = %d, want exactly %d total", readerA+readerB, m)
	}
	if readerA == m || readerB == m {
		t.Fatalf("one reader got all %d items on a shared tail: A=%d B=%d", m, readerA, readerB)
	}
}

// TestConcurrentProducerAndReaders exercises the declared concurrency
// contract: one producer and two readers driving Push/Next concurrently.
func TestConcurrentProducerAndReaders(t *testing.T) {
	t.Parallel()
	const n = 2000
	r, err := New[int](16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ids := [2]ReaderID{r.Subscribe(), r.Subscribe()}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			for r.Push(i) != nil {
				runtime.Gosched()
			}
		}
	}()
	var counts [2]int
	for i, id := range ids {
		go func(idx int, id ReaderID) {
			defer wg.Done()
			for counts[idx] < n {
				if _, ok, err := r.Next(id); err != nil {
					t.Errorf("Next(%d): %v", id, err)
					return
				} else if ok {
					counts[idx]++
				} else {
					runtime.Gosched()
				}
			}
		}(i, id)
	}
	wg.Wait()
	if counts[0] != n || counts[1] != n {
		t.Errorf("counts = %v, want both = %d", counts, n)
	}
}

// ExampleFanoutRing subscribes two readers before pushing three values and
// shows both independently draining all three -- the broadcast contract.
func ExampleFanoutRing() {
	r, err := New[string](4)
	if err != nil {
		panic(err)
	}
	alice, bob := r.Subscribe(), r.Subscribe()
	for _, v := range []string{"a", "b", "c"} {
		if err := r.Push(v); err != nil {
			panic(err)
		}
	}
	var got []string
	for {
		v, ok, err := r.Next(alice)
		if err != nil || !ok {
			break
		}
		got = append(got, v)
	}
	fmt.Println("alice:", got)
	backlog, _ := r.Backlog(bob)
	fmt.Println("bob backlog:", backlog)
	// Output:
	// alice: [a b c]
	// bob backlog: 3
}
```

## Review

The ring is correct when every subscribed reader can drain every value
pushed after it subscribed, independently of every other reader's pace --
`TestBroadcastAndLateSubscriber` pins that directly, and
`TestSharedTailQueueSplitsInsteadOfBroadcasting` pins the failure mode it
replaces: a shared tail delivers each value once, total, not once per
reader. Around that core, `New` rejects a non-positive capacity with
`ErrInvalidCapacity`, `Push` refuses to overwrite a slot the slowest current
reader has not consumed with `ErrWouldLapSlowestReader`, and every method
taking a `ReaderID` returns `ErrUnknownReader` for one never issued or
already removed. Zero subscribed readers never gates `Push`, since there is
no backlog to protect; a stalled reader can be drained or forcibly
unsubscribed to free the capacity it was holding back, the same operational
choice an operator faces with a Kafka consumer group that has stopped
consuming. The whole type is safe for concurrent use by a producer and any
number of readers at once, proven under `-race` by
`TestConcurrentProducerAndReaders`. `ExampleFanoutRing` is the executable
documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [The LMAX Disruptor technical paper](https://lmax-exchange.github.io/disruptor/disruptor.html) — the multi-consumer `RingBuffer` and its per-consumer "gating sequence" this module's slowest-reader gate mirrors.
- [Kafka replication design docs](https://kafka.apache.org/documentation/#replication) — the high-water-mark and consumer-lag concepts behind "a late subscriber sees only future values" and `Backlog`.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the single lock guarding every operation, and why one lock is sufficient for a map of reader cursors plus a shared backing array.
- [Chronicle Queue](https://github.com/OpenHFT/Chronicle-Queue) — a production Java library built around the same "independent tailers over one shared append-only ring" shape.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-hashed-wheel-timer-slotted-ring.md](18-hashed-wheel-timer-slotted-ring.md) | Next: [20-content-defined-chunker-rolling-hash-ring.md](20-content-defined-chunker-rolling-hash-ring.md)
