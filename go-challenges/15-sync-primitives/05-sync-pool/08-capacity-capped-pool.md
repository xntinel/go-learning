# Exercise 8: A Capacity-Capped Pool That Drops Oversized Buffers

An uncapped buffer pool has a memory profile shaped by your worst traffic, not
your typical traffic: one rare multi-megabyte payload grows a buffer, `Put`
pins it, and your steady-state footprint ratchets up until the next GC. This
module builds the pool a log-ingest service actually needs — one whose `Put`
inspects capacity, silently drops oversized buffers, and counts the drops so an
operator can see the cap working.

This module is fully self-contained: its own `go mod init`, its own demo, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
cappedpool/                   independent module: example.com/cappedpool
  go.mod                      go 1.26
  logbuf/
    pool.go                   Pool with maxCap; Get/Put; atomic allocated/gets/puts/dropped; Stats
    pool_test.go              drop-on-oversize, reset-on-get, reuse range, concurrent mix, Example
  cmd/
    demo/
      main.go                 five normal records, one 8 MiB spike, prints the drop
```

Files: `logbuf/pool.go`, `logbuf/pool_test.go`, `cmd/demo/main.go`.
Implement: a `Pool` wrapping `sync.Pool` whose `Get` returns a reset `*bytes.Buffer` and whose `Put` drops (does not pool) any buffer with `Cap()` above `maxCap`, incrementing an atomic `dropped` counter exposed through `Stats`.
Test: an oversized buffer is dropped and the next `Get` allocates fresh; under-cap buffers recycle (`allocated` far below op count, asserted as a range); dirty buffers come back clean; a concurrent mixed-size stress under `-race` where the drop count is exactly the number of oversized Puts.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p cappedpool/logbuf cappedpool/cmd/demo
cd cappedpool
go mod init example.com/cappedpool
```

### The ratchet: why an uncapped pool leaks by design

`bytes.Buffer` never shrinks. Once a single log record — a stack trace with a
giant attached payload, a batch flush, a pathological client — grows a buffer
to 8 MiB, that capacity is permanent for the buffer's lifetime. `Put` that
buffer back and the pool now holds 8 MiB of dead weight that the next `Get`
hands to a caller who needs 200 bytes. Under per-P sharding you can pin one
such buffer per P simultaneously: on a 16-core ingest box that is 128 MiB of
heap doing nothing, invisible to any leak detector because it is all
"reachable," and it stays until a GC (two GCs, with the victim cache) finally
clears the pool. The pool's memory footprint has ratcheted to the largest
object ever pooled — a resource leak driven by worst-case traffic rather than
by any bug in your code.

The defense is a policy decision at the `Put` boundary: an oversized buffer is
simply not returned to the pool. Dropping it costs one future allocation; the
alternative costs pinned megabytes on every P. For per-request scratch buffers
that trade is nearly always right, and the stdlib agrees — `fmt`'s pooled
printer buffers are discarded on free once they exceed 64 KiB, a guard added
for exactly this ratchet ([golang/go issue 23199](https://github.com/golang/go/issues/23199),
"sync: Pool example suggests incorrect usage").

### Making the cap observable: the dropped counter

A silent cap is a debugging trap: if the cap is set too low for real traffic,
the pool quietly degrades into `make` and nobody knows why allocations climbed.
So `Put` increments an atomic `dropped` counter every time it declines a
buffer, and `Stats` exposes it alongside `allocated`, `gets`, and `puts`. In a
real service that counter feeds a metric; an alert on a sustained nonzero drop
rate tells the operator either the cap is mis-sized or a client is sending
pathological payloads. Note the counter's determinism, which the tests exploit:
whether a `Put` drops depends only on the buffer's capacity versus the cap —
never on pool state, GC timing, or scheduling — so a test that performs exactly
k oversized Puts can assert `dropped == k` even though it must never assert an
exact `allocated`.

Two smaller contracts round out the type. `Get` resets the buffer it returns —
resetting at the `Get` boundary (rather than trusting every `Put` caller)
makes a dirty buffer impossible to observe, which is the data-integrity half
of the pool contract. And the counters live on the `Pool` struct rather than
in package globals, so each service component can own an independently
observable pool.

Create `logbuf/pool.go`:

```go
// Package logbuf provides a capacity-capped buffer pool for log-record
// serialization: normal-size buffers recycle, oversized ones are dropped so a
// rare multi-megabyte payload cannot pin steady-state memory.
package logbuf

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// Pool recycles *bytes.Buffer values, refusing to retain any buffer whose
// capacity has grown beyond maxCap.
type Pool struct {
	pool      sync.Pool
	maxCap    int
	allocated atomic.Int64
	gets      atomic.Int64
	puts      atomic.Int64
	dropped   atomic.Int64
}

// Stats is a point-in-time snapshot of the pool's counters. Dropped is
// deterministic (it depends only on buffer capacity vs the cap); Allocated is
// not (per-P sharding), so dashboards and tests should treat it as a range.
type Stats struct {
	Allocated int64
	Gets      int64
	Puts      int64
	Dropped   int64
}

// New returns a Pool that drops buffers whose capacity exceeds maxCap.
func New(maxCap int) *Pool {
	p := &Pool{maxCap: maxCap}
	p.pool.New = func() any {
		p.allocated.Add(1)
		return new(bytes.Buffer)
	}
	return p
}

// Get returns a reset buffer. Resetting here, at the single exit point from
// the pool, makes it impossible for a caller to observe a previous user's
// bytes no matter what state the buffer was Put in.
func (p *Pool) Get() *bytes.Buffer {
	p.gets.Add(1)
	buf := p.pool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// Put returns buf to the pool, unless its capacity has grown past maxCap, in
// which case the buffer is dropped (left for the GC) and Dropped increments.
func (p *Pool) Put(buf *bytes.Buffer) {
	p.puts.Add(1)
	if buf.Cap() > p.maxCap {
		p.dropped.Add(1)
		return // let the GC take the oversized buffer instead of pinning it
	}
	p.pool.Put(buf)
}

// Stats returns a snapshot of the pool's counters.
func (p *Pool) Stats() Stats {
	return Stats{
		Allocated: p.allocated.Load(),
		Gets:      p.gets.Load(),
		Puts:      p.puts.Load(),
		Dropped:   p.dropped.Load(),
	}
}
```

### The runnable demo

The demo serializes five normal-size records through the pool, then simulates
the spike: one buffer grown to 8 MiB, whose `Put` is refused. The stats line is
what an operator's metric would show.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cappedpool/logbuf"
)

func main() {
	pool := logbuf.New(64 * 1024) // retain nothing that grew past 64 KiB

	// Steady state: normal-size records recycle buffers.
	for i := range 5 {
		buf := pool.Get()
		fmt.Fprintf(buf, `{"seq":%d,"msg":"checkout completed"}`, i)
		_ = buf.String() // ship the serialized record
		pool.Put(buf)
	}

	// The spike: one pathological payload grows a buffer far past the cap.
	big := pool.Get()
	big.Grow(8 << 20) // an 8 MiB batch that must not pin steady-state memory
	pool.Put(big)     // refused: dropped, not pooled

	s := pool.Stats()
	fmt.Printf("gets=%d puts=%d dropped=%d\n", s.Gets, s.Puts, s.Dropped)
	fmt.Printf("the 8 MiB buffer was dropped: %t\n", s.Dropped == 1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
gets=6 puts=6 dropped=1
the 8 MiB buffer was dropped: true
```

### Tests

The drop test uses a fresh single-goroutine pool where the sequence is fully
determined: after the oversized buffer is refused, the pool is empty, so the
next `Get` must allocate a fresh small buffer. The concurrent test exploits the
determinism of `dropped`: exactly one goroutine in ten grows past the cap, so
across 200 operations the drop count must be exactly 20 even under `-race`
scheduling. `allocated`, by contrast, is only ever asserted as a range.

Create `logbuf/pool_test.go`:

```go
package logbuf

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestOversizedBufferIsDropped(t *testing.T) {
	t.Parallel()

	const maxCap = 1024
	p := New(maxCap)

	buf := p.Get()
	buf.Grow(4 * maxCap)
	if buf.Cap() <= maxCap {
		t.Fatalf("Grow did not exceed the cap: cap=%d", buf.Cap())
	}
	p.Put(buf)

	if s := p.Stats(); s.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1", s.Dropped)
	}

	// Nothing was ever pooled, so this Get must allocate a fresh buffer with
	// small capacity — the oversized one is gone.
	before := p.Stats().Allocated
	next := p.Get()
	if next.Cap() > maxCap {
		t.Fatalf("Get returned an oversized buffer (cap=%d) that should have been dropped", next.Cap())
	}
	if p.Stats().Allocated != before+1 {
		t.Fatalf("expected a fresh allocation after the drop")
	}
}

func TestGetReturnsCleanBuffer(t *testing.T) {
	t.Parallel()

	p := New(1024)
	buf := p.Get()
	buf.WriteString("previous tenant's log line")
	p.Put(buf) // put back dirty on purpose

	if got := p.Get(); got.Len() != 0 {
		t.Fatalf("Get returned a dirty buffer: %q", got.String())
	}
}

func TestUnderCapBuffersRecycle(t *testing.T) {
	t.Parallel()

	const ops = 200
	p := New(64 * 1024)
	for i := range ops {
		buf := p.Get()
		fmt.Fprintf(buf, "record %d: %s", i, strings.Repeat("f", 100))
		p.Put(buf)
	}

	s := p.Stats()
	if s.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0 for under-cap traffic", s.Dropped)
	}
	// Range assertion, never exact: per-P sharding makes Allocated fuzzy, but
	// sequential reuse must keep it far below the operation count.
	if s.Allocated >= ops/2 {
		t.Fatalf("Allocated = %d across %d sequential ops; reuse is broken", s.Allocated, ops)
	}
}

func TestConcurrentMixedSizes(t *testing.T) {
	t.Parallel()

	const (
		n      = 200
		maxCap = 8 * 1024
	)
	p := New(maxCap)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			buf := p.Get()
			if i%10 == 0 {
				buf.Grow(4 * maxCap) // every tenth op is a spike
			} else {
				fmt.Fprintf(buf, "record %d", i)
			}
			p.Put(buf)
		}()
	}
	wg.Wait()

	s := p.Stats()
	if s.Gets != n || s.Puts != n {
		t.Fatalf("gets=%d puts=%d, want %d each", s.Gets, s.Puts, n)
	}
	// The drop decision depends only on capacity vs cap, so it is exact even
	// under concurrency: exactly n/10 goroutines grew past the cap.
	if s.Dropped != n/10 {
		t.Fatalf("Dropped = %d, want %d", s.Dropped, n/10)
	}
}

func ExamplePool() {
	p := New(1024)

	buf := p.Get()
	buf.WriteString(`{"level":"info","msg":"served"}`)
	p.Put(buf)

	spike := p.Get()
	spike.Grow(1 << 20)
	p.Put(spike)

	fmt.Println("dropped:", p.Stats().Dropped)
	// Output: dropped: 1
}
```

## Review

The pool is correct when oversized buffers vanish (dropped, counted, and never
handed back out), under-cap traffic recycles (allocated stays far below the op
count), and every `Get` is clean regardless of how dirty the `Put` was. The
judgment call worth internalizing is the asymmetry of the trade: dropping a
buffer costs one future allocation on one request, while pooling it costs
pinned worst-case memory on every P until the GC clears the pool — so when in
doubt, cap low and watch the dropped metric rather than capping high and
watching RSS. Notice also which counters the tests treat as exact: `dropped`
is a pure function of capacity, so `n/10` spikes mean exactly `n/10` drops even
under `-race`, while `allocated` is pool-state-dependent and only ever asserted
as a range. Run `go test -count=1 -race ./...`.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — items may be removed at any time; retention is never guaranteed, only bounded here.
- [`bytes.Buffer.Cap`](https://pkg.go.dev/bytes#Buffer.Cap) — the total capacity of the underlying slice, the drop criterion.
- [`bytes.Buffer.Grow`](https://pkg.go.dev/bytes#Buffer.Grow) — how capacity ratchets up and never back down.
- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — lock-free counters safe under concurrent Get/Put.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-byte-slice-pool-pointer.md](07-byte-slice-pool-pointer.md) | Next: [09-pooled-bufio-reader-line-protocol.md](09-pooled-bufio-reader-line-protocol.md)
