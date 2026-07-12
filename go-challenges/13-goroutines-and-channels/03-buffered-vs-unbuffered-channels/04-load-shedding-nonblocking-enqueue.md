# Exercise 4: Load-Shedding Ingest Buffer with select+default (return 503 instead of blocking)

An ingest endpoint under a traffic spike has two honest options: block the request
goroutine until the queue drains (and watch goroutines and memory pile up until the
server falls over) or reject the request immediately and return 503 so the caller
backs off. The second is load shedding, and its core is a non-blocking send:
`select { case ch <- job: default: }`. This exercise builds that ingest buffer.

This module is fully self-contained.

## What you'll build

```text
ingest/                      module: example.com/ingest
  go.mod                     go 1.26
  ingest.go                  type Buffer; Enqueue (non-blocking), Dequeue, Close, Accepted/Shed
  cmd/
    demo/
      main.go                overload a small buffer and count accepted vs shed
  ingest_test.go             full-buffer sheds, consumer unblocks, burst accounting
```

- Files: `ingest.go`, `cmd/demo/main.go`, `ingest_test.go`.
- Implement: `Enqueue(job) (accepted bool)` doing a non-blocking send into a bounded buffer; a `Dequeue`, a `Close`, and counters for accepted/shed.
- Test: fill to capacity with no consumer and assert the next `Enqueue` sheds without blocking; start a consumer and assert subsequent `Enqueue` succeeds; under a burst of C > cap with no draining, assert `accepted == cap`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the non-blocking send is the whole pattern

A plain `ch <- job` blocks the calling goroutine when the buffer is full. In an HTTP
handler that goroutine is the request; blocking it means the request hangs, the client
times out and retries, more goroutines accumulate on the blocked send, and memory and
scheduler pressure climb until the process degrades or is OOM-killed. This is the
unbounded-goroutine anti-pattern: the server has no admission control, so a spike it
cannot serve becomes a spike that takes it down.

`select { case ch <- job: default: }` inverts that. If the send can proceed right now
(buffer not full), the first case fires and the job is accepted. If it cannot, the
`default` fires *immediately* — no blocking — and the handler returns 503/429. The
buffer capacity is now an explicit admission limit: at most `cap` jobs wait, and
everything beyond that is shed cleanly. The server degrades by rejecting excess load,
which is a recoverable state, instead of by exhausting memory, which is not.

The behavior is deterministic precisely because `default` removes the blocking: given
a full buffer and no consumer, the next `Enqueue` *always* takes the `default` and
returns `false`, with no dependence on scheduling. That determinism is what makes the
tests exact — fill to `cap`, and the `(cap+1)`th `Enqueue` sheds every time.

Counters (`accepted`, `shed`) are updated under a mutex so the metrics themselves are
race-free; they are what an ops dashboard graphs to see the shed rate climb during an
incident. `Close` is owned by the producer side and closes the channel so a draining
consumer's `range`/`Dequeue` loop can terminate.

Create `ingest.go`:

```go
package ingest

import "sync"

// Buffer is a bounded ingest queue with load shedding. Enqueue never blocks: if the
// bounded buffer is full it sheds (returns false) so a request handler can answer
// 503 instead of hanging.
type Buffer struct {
	ch chan string

	mu       sync.Mutex
	accepted int
	shed     int
}

// New returns an ingest buffer that admits at most capacity in-flight jobs.
func New(capacity int) *Buffer {
	return &Buffer{ch: make(chan string, capacity)}
}

// Enqueue attempts a non-blocking send. It returns true if the job was admitted and
// false if the buffer was full (shed). It never blocks the caller.
func (b *Buffer) Enqueue(job string) (accepted bool) {
	select {
	case b.ch <- job:
		b.mu.Lock()
		b.accepted++
		b.mu.Unlock()
		return true
	default:
		b.mu.Lock()
		b.shed++
		b.mu.Unlock()
		return false
	}
}

// Dequeue blocks for the next job; ok is false once the buffer is closed and drained.
func (b *Buffer) Dequeue() (job string, ok bool) {
	v, ok := <-b.ch
	return v, ok
}

// Close stops admission and lets a draining consumer finish. Sole producer owns it.
func (b *Buffer) Close() { close(b.ch) }

// Stats reports the admitted and shed counts for metrics.
func (b *Buffer) Stats() (accepted, shed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepted, b.shed
}
```

### The runnable demo

The demo admits a burst of 6 into a buffer of 2 with no consumer running, so exactly 2
are accepted and 4 are shed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ingest"
)

func main() {
	b := ingest.New(2)
	for i := range 6 {
		if b.Enqueue(fmt.Sprintf("req-%d", i)) {
			fmt.Printf("req-%d accepted\n", i)
		} else {
			fmt.Printf("req-%d shed (503)\n", i)
		}
	}
	accepted, shed := b.Stats()
	fmt.Printf("accepted=%d shed=%d\n", accepted, shed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (two admits fill the cap-2 buffer, the remaining four are shed):

```
req-0 accepted
req-1 accepted
req-2 shed (503)
req-3 shed (503)
req-4 shed (503)
req-5 shed (503)
accepted=2 shed=4
```

### Tests

`TestFullBufferSheds` fills to `cap` with no consumer and asserts the next `Enqueue`
returns `false` — the shed path, deterministic thanks to `default`.
`TestConsumerUnblocksAdmission` fills the buffer, dequeues once to free a slot, and
asserts a subsequent `Enqueue` succeeds. `TestBurstAccounting` fires C > cap enqueues
with no draining and asserts `accepted == cap` and `shed == C-cap`. All run under
`-race` to prove the counters are guarded.

Create `ingest_test.go`:

```go
package ingest

import (
	"fmt"
	"testing"
)

func TestFullBufferSheds(t *testing.T) {
	t.Parallel()

	b := New(2)
	if !b.Enqueue("a") || !b.Enqueue("b") {
		t.Fatal("first two enqueues into a cap-2 buffer must be accepted")
	}
	if b.Enqueue("c") {
		t.Fatal("Enqueue into a full buffer must shed (return false)")
	}
}

func TestConsumerUnblocksAdmission(t *testing.T) {
	t.Parallel()

	b := New(1)
	if !b.Enqueue("a") {
		t.Fatal("first enqueue must be accepted")
	}
	if b.Enqueue("b") {
		t.Fatal("second enqueue must shed while buffer is full")
	}
	if v, ok := b.Dequeue(); !ok || v != "a" {
		t.Fatalf("Dequeue = %q,%v; want \"a\",true", v, ok)
	}
	if !b.Enqueue("b") {
		t.Fatal("enqueue after freeing a slot must be accepted")
	}
}

func TestBurstAccounting(t *testing.T) {
	t.Parallel()

	const capacity, burst = 4, 20
	b := New(capacity)
	for i := range burst {
		b.Enqueue(fmt.Sprintf("r%d", i))
	}
	accepted, shed := b.Stats()
	if accepted != capacity {
		t.Fatalf("accepted = %d, want %d", accepted, capacity)
	}
	if shed != burst-capacity {
		t.Fatalf("shed = %d, want %d", shed, burst-capacity)
	}
}

func ExampleBuffer_Enqueue() {
	b := New(1)
	fmt.Println(b.Enqueue("a")) // admitted
	fmt.Println(b.Enqueue("b")) // buffer full -> shed
	// Output:
	// true
	// false
}
```

## Review

The buffer is correct when `Enqueue` is a `select`+`default` non-blocking send: it
admits while the bounded buffer has room and sheds the instant it is full, never
blocking the caller. That non-blocking property is the entire safety argument — it is
what lets an HTTP handler answer 503 rather than hang and leak goroutines. The tests
are exact because `default` removes scheduling dependence: a full buffer with no
consumer always sheds the next job. The mistake to avoid is "just make the buffer
bigger" — that only moves the cliff and, when it finally fills under a large enough
spike, restores the blocking-and-OOM failure mode. Bound the buffer, shed past it, and
graph the shed counter.

## Resources

- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking send via the `default` case.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded channels and shedding.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — why servers must shed rather than queue unboundedly.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-queue-depth-saturation-metric.md](03-queue-depth-saturation-metric.md) | Next: [05-concurrency-limiter-token-buffer.md](05-concurrency-limiter-token-buffer.md)
