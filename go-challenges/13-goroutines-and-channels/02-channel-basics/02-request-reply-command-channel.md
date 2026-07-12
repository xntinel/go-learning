# Exercise 2: Serialize Shared State with a Command Channel (Actor Loop)

A monotonic ID or sequence service — the source of idempotency keys, ordering
tokens, or event sequence numbers — must never hand out the same number twice, even
under heavy concurrency. The obvious implementation guards a counter with a mutex.
The idiomatic Go alternative keeps the counter inside one goroutine and lets callers
talk to it over a channel: no shared mutable state, no lock, no data race by
construction.

This module is self-contained: its own module, an `idgen` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
idgen/                       independent module: example.com/idgen
  go.mod                     go 1.26
  idgen.go                   type Sequencer; New, Next, Close (actor loop)
  cmd/demo/main.go           runnable demo: pull a few IDs, then Close
  idgen_test.go              monotonic, concurrent-uniqueness, safe-after-close
```

- Files: `idgen.go`, `cmd/demo/main.go`, `idgen_test.go`.
- Implement: `New() *Sequencer`, `(*Sequencer).Next() uint64`, `(*Sequencer).Close()`, with all counter state owned by one goroutine reached only through channels.
- Test: IDs are strictly increasing; 100 concurrent callers get 100 distinct values; `Next` after `Close` is safe.
- Verify: `go test -count=1 -race ./...`

### The command struct carries its own reply channel

The mechanism is "share memory by communicating". The counter — a single `uint64`
— lives as a local variable inside the loop goroutine and is touched by nobody
else. A caller cannot read or increment it directly; it can only *ask*. To ask, it
constructs a `request` that carries a private `reply chan uint64`, sends the
request into the shared `requests` channel, and blocks receiving on its own reply.
The loop receives one request at a time, increments the counter, and answers on
that request's reply channel. Because the loop processes requests strictly in
sequence, two callers can never observe the same value: serialization is a
consequence of the single-goroutine loop, not of any lock.

Each request allocates a fresh reply channel. That looks wasteful but it is the
point — the reply channel is how the loop routes *this* answer back to *this*
caller without any shared map or correlation ID. It is a per-call rendezvous.

### Shutting the loop down safely

The hard part of the actor pattern is lifecycle. `Close` must stop the loop, and a
`Next` call that races or follows `Close` must not panic and must not hang. The
robust design uses a separate `done` channel closed by `Close`, and a `select` on
both sides:

- The loop `select`s between receiving a request and observing `done` closed; when
  `done` fires it returns, ending the goroutine.
- `Next` `select`s between sending its request and observing `done`; if the loop is
  gone, the `done` case wins and `Next` returns a sentinel `0`.

Because the sequence starts at 1, `0` is an unambiguous "closed" signal — a caller
that gets `0` back knows the service is shut down. `Close` is guarded by
`sync.Once` so that closing twice (two shutdown paths, a `defer` plus an explicit
call) never panics on a double `close(done)`.

Create `idgen.go`:

```go
package idgen

import "sync"

// request is one caller's ask, carrying a private channel for its answer.
type request struct {
	reply chan uint64
}

// Sequencer hands out strictly increasing uint64 values. The counter lives in a
// single goroutine; callers reach it only through the requests channel, so there
// is no shared mutable state and no mutex on the counter.
type Sequencer struct {
	requests  chan request
	done      chan struct{}
	closeOnce sync.Once
}

// New starts the actor loop and returns a ready Sequencer.
func New() *Sequencer {
	s := &Sequencer{
		requests: make(chan request),
		done:     make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *Sequencer) loop() {
	var n uint64
	for {
		select {
		case req := <-s.requests:
			n++
			req.reply <- n
		case <-s.done:
			return
		}
	}
}

// Next returns the next value in the sequence, starting at 1. If the Sequencer
// has been closed it returns 0 (never a valid sequence value) instead of
// panicking or blocking forever.
func (s *Sequencer) Next() uint64 {
	reply := make(chan uint64)
	select {
	case s.requests <- request{reply: reply}:
		return <-reply
	case <-s.done:
		return 0
	}
}

// Close stops the loop. It is safe to call more than once.
func (s *Sequencer) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idgen"
)

func main() {
	s := idgen.New()
	for range 3 {
		fmt.Println("id:", s.Next())
	}
	s.Close()
	fmt.Println("after close:", s.Next())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id: 1
id: 2
id: 3
after close: 0
```

### Tests

`TestNextIsMonotonic` pins the sequential contract: successive calls return
1, 2, 3, ... `TestConcurrentCallersGetUniqueValues` is the proof that the actor
loop serializes: 100 goroutines call `Next` at once, and all 100 results must be
distinct. If the counter were touched by two goroutines, the race detector would
flag it and duplicates would appear; a clean `-race` run with 100 unique values is
the evidence that only the loop goroutine ever mutates the counter.
`TestNextAfterCloseIsSafe` confirms the shutdown path returns `0` rather than
panicking on a send to a stopped loop.

Create `idgen_test.go`:

```go
package idgen

import (
	"fmt"
	"sync"
	"testing"
)

func TestNextIsMonotonic(t *testing.T) {
	t.Parallel()
	s := New()
	defer s.Close()

	for want := uint64(1); want <= 5; want++ {
		if got := s.Next(); got != want {
			t.Fatalf("Next() = %d, want %d", got, want)
		}
	}
}

func TestConcurrentCallersGetUniqueValues(t *testing.T) {
	t.Parallel()
	s := New()
	defer s.Close()

	const n = 100
	got := make([]uint64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = s.Next()
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, v := range got {
		if v == 0 {
			t.Fatal("got 0 from an open Sequencer")
		}
		if seen[v] {
			t.Fatalf("duplicate value %d handed out twice", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d unique values, want %d", len(seen), n)
	}
}

func TestNextAfterCloseIsSafe(t *testing.T) {
	t.Parallel()
	s := New()
	s.Close()
	s.Close() // double close must not panic

	if got := s.Next(); got != 0 {
		t.Fatalf("Next() after Close = %d, want 0", got)
	}
}

func ExampleSequencer_Next() {
	s := New()
	defer s.Close()
	fmt.Println(s.Next(), s.Next(), s.Next())
	// Output: 1 2 3
}
```

## Review

The design is correct when the counter is provably touched by exactly one
goroutine. The evidence is a clean `-race` run of `TestConcurrentCallersGetUniqueValues`:
100 concurrent callers, 100 distinct non-zero values. If you see a race report, a
duplicate, or a `0`, the loop is not the sole owner of the counter or the shutdown
path let a caller through. The two mistakes to avoid: sending on `s.requests`
without a `select` on `done` (a `Next` racing `Close` would then block forever or
panic on a closed channel), and closing `done` without `sync.Once` (a second
`Close` panics). Note there is no mutex anywhere near the counter — that absence is
the lesson. The only synchronization primitive is `sync.Once`, and it guards
lifecycle, not the state.

## Resources

- [The Go Memory Model: channel communication](https://go.dev/ref/mem#chan) — why the value published by the loop is safely visible to the caller that receives it.
- [Go Blog: Share Memory By Communicating](https://go.dev/doc/codewalk/sharemem/) — the codewalk this pattern comes from.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — the exactly-once guard used for a safe `Close`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-worker-pool-fan-out-fan-in.md](01-worker-pool-fan-out-fan-in.md) | Next: [03-cursor-stream-close-comma-ok.md](03-cursor-stream-close-comma-ok.md)
