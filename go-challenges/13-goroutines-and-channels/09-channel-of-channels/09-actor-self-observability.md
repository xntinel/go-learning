# Exercise 9: Querying Actor State Without a Lock (Metrics Snapshot)

An actor that owns mutable state must expose that state for observability —
in-flight count, total processed, queue depth, average latency — without
reintroducing the lock its single-goroutine design eliminated. The idiomatic
answer is to make the metrics query just another request on the same loop: a stats
request carrying its own reply channel, answered in the same `select` that handles
work, so the snapshot is consistent and never torn. This exercise builds that
observability hook.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
observable/                independent module: example.com/observable
  go.mod
  observable.go            type Service; Call and Stats routed through one loop
  cmd/
    demo/
      main.go              runnable demo: run traffic, print a Stats snapshot
  observable_test.go        processed-count, consistency-under-load tests
```

- Files: `observable.go`, `cmd/demo/main.go`, `observable_test.go`.
- Implement: an actor whose run loop owns `processed`, cumulative latency, and a buffered inbox; a `Stats()` method that sends a stats request through the same channel and reads back a consistent `Stats{Processed, QueueDepth, AvgLatency}` snapshot.
- Test: after known traffic, `Stats().Processed` equals the number of calls and `AvgLatency > 0`; under a concurrent burst, every snapshot is internally consistent (`Processed` monotonic, within bounds) and the final snapshot is exact; all under `-race`.
- Verify: `go test -count=1 -race ./...`

### Metrics as a request on the same loop

The counters — `processed` and cumulative `totalLatency` — are ordinary local
variables inside the run loop, touched only by the one goroutine that also handles
requests. Reading them from another goroutine through shared struct fields would
be a data race and would yield torn or stale values. Instead, `Stats()` builds a
snapshot by sending a *stats request* onto a dedicated channel that the run loop
selects on alongside the work channel:

```
select {
case req := <-s.requests:  // do work, bump counters
case rc := <-s.stats:      // answer with a Stats snapshot
case <-s.quit:
	return
}
```

Because the loop handles either a work request or a stats request but never both
at once, the snapshot it produces reflects a single, quiescent point between
requests: `Processed` and `AvgLatency` are mutually consistent, never a mix of
before-and-after. There is no mutex, and there is no race, for the same reason as
the counter in Exercise 1 — one goroutine owns the state. The cost is that a stats
query is answered *between* requests, which is exactly the consistency you want:
you never observe a half-updated actor.

`QueueDepth` is `len(s.requests)` read inside the loop — how many work requests
are buffered and waiting. `AvgLatency` is `totalLatency / processed`, computed from
the time each `handle` call took, measured with `time.Since`. `Stats()` itself
uses the same buffered-reply, `quit`-guarded pattern as `Call`, so it never blocks
forever if the service is shutting down.

Create `observable.go`:

```go
package observable

import (
	"errors"
	"time"
)

// ErrShuttingDown is returned once the service has been shut down.
var ErrShuttingDown = errors.New("observable: shutting down")

// Stats is a consistent snapshot of the actor's internal state.
type Stats struct {
	Processed  int
	QueueDepth int
	AvgLatency time.Duration
}

type request struct {
	n     int
	reply chan response
}

type response struct {
	value int
}

// Service is an actor that also answers metrics queries on its own loop.
type Service struct {
	requests chan request
	stats    chan chan Stats
	work     time.Duration
	quit     chan struct{}
	done     chan struct{}
}

// New returns a Service with a buffered inbox of the given capacity. work is the
// per-request processing time. Start it with go s.Run().
func New(inboxCap int, work time.Duration) *Service {
	return &Service{
		requests: make(chan request, inboxCap),
		stats:    make(chan chan Stats),
		work:     work,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Run is the actor loop. It owns the counters, so no lock is needed to read them
// consistently: the stats query is handled in the same select as work.
func (s *Service) Run() {
	defer close(s.done)
	var processed int
	var totalLatency time.Duration
	for {
		select {
		case req := <-s.requests:
			start := time.Now()
			time.Sleep(s.work)
			req.reply <- response{value: req.n * 2}
			processed++
			totalLatency += time.Since(start)
		case rc := <-s.stats:
			var avg time.Duration
			if processed > 0 {
				avg = totalLatency / time.Duration(processed)
			}
			rc <- Stats{
				Processed:  processed,
				QueueDepth: len(s.requests),
				AvgLatency: avg,
			}
		case <-s.quit:
			return
		}
	}
}

// Shutdown stops the loop and waits for it to exit.
func (s *Service) Shutdown() {
	close(s.quit)
	<-s.done
}

// Capacity reports the inbox capacity.
func (s *Service) Capacity() int { return cap(s.requests) }

// Call sends n and returns the doubled value.
func (s *Service) Call(n int) (int, error) {
	reply := make(chan response, 1)
	select {
	case s.requests <- request{n: n, reply: reply}:
	case <-s.quit:
		return 0, ErrShuttingDown
	}
	select {
	case resp := <-reply:
		return resp.value, nil
	case <-s.quit:
		return 0, ErrShuttingDown
	}
}

// Stats returns a consistent snapshot of the actor's internal state.
func (s *Service) Stats() (Stats, error) {
	rc := make(chan Stats, 1)
	select {
	case s.stats <- rc:
	case <-s.quit:
		return Stats{}, ErrShuttingDown
	}
	select {
	case st := <-rc:
		return st, nil
	case <-s.quit:
		return Stats{}, ErrShuttingDown
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/observable"
)

func main() {
	s := observable.New(8, 0)
	go s.Run()
	defer s.Shutdown()

	for i := range 5 {
		s.Call(i)
	}

	st, _ := s.Stats()
	fmt.Printf("processed: %d\n", st.Processed)
	fmt.Printf("queue depth: %d\n", st.QueueDepth)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed: 5
queue depth: 0
```

### Tests

`TestStatsAfterKnownTraffic` makes a known number of synchronous calls (so all are
processed before the query), then asserts `Processed` equals that number,
`QueueDepth` is zero at steady state, and `AvgLatency` is positive because the
handler takes real time. `TestStatsConsistentUnderLoad` fires a concurrent burst
while repeatedly polling `Stats`, asserting every snapshot is internally
consistent — `Processed` never exceeds the total and never decreases — and that
the final snapshot is exact. That is the property the same-loop design buys:
snapshots are never torn.

Create `observable_test.go`:

```go
package observable

import (
	"sync"
	"testing"
	"time"
)

func TestStatsAfterKnownTraffic(t *testing.T) {
	t.Parallel()
	const n = 12
	s := New(8, time.Millisecond)
	go s.Run()
	defer s.Shutdown()

	for i := range n {
		if _, err := s.Call(i); err != nil {
			t.Fatalf("Call(%d) error = %v", i, err)
		}
	}

	st, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats error = %v", err)
	}
	if st.Processed != n {
		t.Fatalf("Processed = %d, want %d", st.Processed, n)
	}
	if st.QueueDepth != 0 {
		t.Fatalf("QueueDepth = %d, want 0 at steady state", st.QueueDepth)
	}
	if st.AvgLatency <= 0 {
		t.Fatalf("AvgLatency = %v, want > 0", st.AvgLatency)
	}
}

func TestStatsConsistentUnderLoad(t *testing.T) {
	t.Parallel()
	const n = 100
	s := New(64, 0)
	go s.Run()
	defer s.Shutdown()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			s.Call(i)
		}
	}()

	// Poll stats while the burst runs; every snapshot must be consistent.
	prev := 0
	for range 50 {
		st, err := s.Stats()
		if err != nil {
			t.Fatalf("Stats error = %v", err)
		}
		if st.Processed < prev {
			t.Fatalf("Processed went backwards: %d then %d", prev, st.Processed)
		}
		if st.Processed > n {
			t.Fatalf("Processed = %d exceeds total %d", st.Processed, n)
		}
		if st.QueueDepth < 0 || st.QueueDepth > s.Capacity() {
			t.Fatalf("QueueDepth = %d out of range [0,%d]", st.QueueDepth, s.Capacity())
		}
		prev = st.Processed
	}

	wg.Wait()
	final, _ := s.Stats()
	if final.Processed != n {
		t.Fatalf("final Processed = %d, want %d", final.Processed, n)
	}
}
```

The consistency test calls `s.Capacity()` (defined above) to bound `QueueDepth`
against the inbox capacity.

## Review

The observability hook is correct when a snapshot never reflects a half-updated
actor. `TestStatsAfterKnownTraffic` pins the exact case: after twelve synchronous
calls, `Processed` is exactly twelve and `AvgLatency` is positive.
`TestStatsConsistentUnderLoad` pins the concurrent case: across fifty snapshots
taken during a burst, `Processed` only ever rises and stays within bounds, and the
final value is exact. Both pass under `-race` because the counters are read by the
same goroutine that writes them — the query is serialized with the work.

The mistakes to avoid: never expose `processed` as a struct field read from
another goroutine — that is the data race the actor avoids, and `go test -race`
would flag it; route the read through the loop. Do not expect `Stats` to observe a
non-zero `QueueDepth` mid-request — the loop answers stats only between requests,
so depth reflects the buffered backlog, not the one being handled. And keep the
stats reply buffered and `quit`-guarded so a shutdown never wedges a caller waiting
on a snapshot. Run `go test -race` to confirm the whole thing is lock-free and
race-free.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the `chan chan Stats` used for the query reply.
- [Go Memory Model](https://go.dev/ref/mem) — why single-goroutine ownership makes the snapshot consistent without a lock.
- [`time.Since`](https://pkg.go.dev/time#Since) — measuring per-request latency for the average.
- [Effective Go: Share by communicating](https://go.dev/doc/effective_go#sharing) — communicating state instead of sharing it behind a lock.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-connection-pool-lease-checkout.md](10-connection-pool-lease-checkout.md)
