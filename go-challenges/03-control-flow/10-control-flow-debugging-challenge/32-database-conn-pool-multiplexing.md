# Exercise 32: Database Connection Pool Multiplexing Corrupts Protocol

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Connection poolers like PgBouncer and ProxySQL exist because a database
can only afford so many real socket connections, while an application
tier wants to hand out far more logical "connections" to its own
request handlers than that. The trick is multiplexing: many logical
connections share a smaller pool of physical ones, handed out and
returned as each logical connection's unit of work completes. The part
that is easy to gloss over is what happens the instant two logical
connections are, however briefly, both trying to use the *same*
physical connection at once — a real wire protocol has no way to tell
two interleaved requests' responses apart, so if nothing prevents two
logical connections from writing to the same physical socket
concurrently, one connection can read back a response meant for the
other. This is silent, wire-level corruption, not a Go-level data race
that merely trips `-race` and then behaves correctly anyway — the
values themselves come back wrong. This module is fully self-contained:
its own `go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
connpool/                     independent module: example.com/database-conn-pool-multiplexing
  go.mod                       go 1.21
  connpool.go                   PhysicalConn, Pool, New, Send
  cmd/
    demo/
      main.go                    runnable demo: 12 logical connections sharing 3 physical ones
  connpool_test.go                broad multiplexing stress test, plus a targeted two-logical-one-physical case
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `PhysicalConn.Send` guarded so only one logical connection's request/response round trip is ever in flight on it at a time; `Pool.Send` sharding logical connections onto physical ones.
- Test: many logical connections hammering few physical ones with distinguishable payloads, asserting zero cross-contaminated responses; a targeted case isolating exactly two logical connections that share one physical connection.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/32-database-conn-pool-multiplexing/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/32-database-conn-pool-multiplexing
```

### Why the socket needs a guard, not just the pool's bookkeeping

The version that ships first often guards the *pool's* bookkeeping —
which physical connection a logical ID maps to — but not the physical
connection's actual request/response exchange, on the assumption that
each `Send` call is self-contained:

```go
// BUG: no synchronization at all on the physical connection's shared wire
// state. Two logical connections calling Send concurrently on the same
// physical connection can interleave their write and read.
func (p *PhysicalConn) Send(payload string) string {
	p.pending = payload
	resp := p.pending
	p.pending = ""
	return resp
}
```

In isolation, one goroutine calling this looks completely correct —
write, read, clear, return exactly what was written. The corruption
only appears when a second logical connection, sharing the same
physical connection because there are more logical connections than
physical ones, calls `Send` while the first is between its write and
its read. Goroutine A writes its payload to `pending`; before A reads
it back, the scheduler runs goroutine B, which overwrites `pending`
with *its own* payload; A resumes, reads the current value of
`pending` — which is now B's payload, not A's — and hands it back to
its caller as if it were A's own response. From the outside this looks
exactly like what the brief describes: two logical connections wrote
to the same physical connection at once, and the wire-level protocol,
which has no concept of "whose turn it is" beyond strict alternation,
delivered the wrong answer to the wrong caller. A `sync.Mutex` around
the pool's own routing table would not have caught this at all,
because the routing decision (*which* physical connection to use) was
never the racy part — the racy part is what happens *on* that physical
connection once two callers reach it.

The fix puts the guard exactly where the shared, unshareable state
lives: on the physical connection itself, wrapping its entire
round trip in one critical section.

```go
func (p *PhysicalConn) Send(payload string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.pending = payload
	resp := p.pending
	p.pending = ""
	return resp
}
```

Now a second logical connection sharing the same physical connection
cannot even begin its own write until the first's entire round trip —
write, read, clear — has completed and released the lock. This is
exactly per-connection queueing in miniature: every logical connection
waiting on a busy physical connection simply blocks on `Lock()` until
its turn.

Create `connpool.go`:

```go
package connpool

import (
	"runtime"
	"sync"
)

// PhysicalConn simulates one real database wire connection. A real
// connection can only have one request in flight at a time, because the
// wire protocol has no way to tell two concurrent responses apart --
// whatever comes back next off the socket is assumed to be the answer to
// whatever was written last. pending models that shared wire state.
type PhysicalConn struct {
	mu      sync.Mutex
	pending string
}

// Send simulates one full request/response round trip: write payload onto
// the wire, yield (so a missing lock would let another goroutine's write
// land in between), then read back whatever is currently on the wire. The
// mutex is the state guard: only one logical connection may have a
// request in flight on this physical connection at a time.
func (p *PhysicalConn) Send(payload string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.pending = payload
	runtime.Gosched() // widen the window a missing lock would expose
	resp := p.pending
	p.pending = ""
	return resp
}

// Pool multiplexes many logical connections onto a fixed, smaller number
// of physical connections -- the same shape a connection pooler like
// PgBouncer or ProxySQL uses to serve more client sessions than the
// database can afford real sockets for.
type Pool struct {
	physical []*PhysicalConn
}

// New creates a Pool backed by physicalConns physical connections.
func New(physicalConns int) *Pool {
	p := &Pool{physical: make([]*PhysicalConn, physicalConns)}
	for i := range p.physical {
		p.physical[i] = &PhysicalConn{}
	}
	return p
}

// Send multiplexes logicalID's request onto its assigned physical
// connection, sharded by simple modulo.
func (p *Pool) Send(logicalID int, payload string) string {
	conn := p.physical[logicalID%len(p.physical)]
	return conn.Send(payload)
}
```

### The runnable demo

Twelve logical connections, only three physical connections behind
them, each logical connection firing 500 rounds of uniquely tagged
payloads at once. Every payload has its own logical ID and round
number baked into it, so any cross-talk between logical connections
would show up as a nonzero mismatch count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/database-conn-pool-multiplexing"
)

func main() {
	pool := connpool.New(3)
	const logicalConns = 12
	const roundsPerConn = 500

	var wg sync.WaitGroup
	var mismatches int64
	wg.Add(logicalConns)
	for id := 0; id < logicalConns; id++ {
		id := id
		go func() {
			defer wg.Done()
			for r := 0; r < roundsPerConn; r++ {
				payload := fmt.Sprintf("logical-%d-req-%d", id, r)
				resp := pool.Send(id, payload)
				if resp != payload {
					atomic.AddInt64(&mismatches, 1)
				}
			}
		}()
	}
	wg.Wait()

	fmt.Println("logical connections:", logicalConns, "rounds each:", roundsPerConn)
	fmt.Println("mismatched responses:", mismatches)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
logical connections: 12 rounds each: 500
mismatched responses: 0
```

### Tests

`TestSendNeverReturnsAnotherCallersResponse` is the broad concurrency
stress case: eight logical connections sharing four physical
connections, 200 rounds each, asserting zero cross-contaminated
responses under `-race`.
`TestTwoLogicalConnsSharingOnePhysicalConnDontCorrupt` isolates the
exact contention path directly — two specific logical IDs chosen so
they map to the *same* physical connection — rather than relying on
statistical pressure from many IDs to eventually hit that path.

Create `connpool_test.go`:

```go
package connpool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSendNeverReturnsAnotherCallersResponse is the concurrency stress
// case: many logical connections, sharing few physical connections, all
// sending distinguishable payloads at once. Every response must exactly
// match what that logical connection sent -- a missing per-connection
// guard would let two logical connections interleave their write and read
// of the same physical connection's shared wire state.
func TestSendNeverReturnsAnotherCallersResponse(t *testing.T) {
	pool := New(4)
	const logicalConns = 8
	const rounds = 200

	var wg sync.WaitGroup
	var mismatches int64
	wg.Add(logicalConns)
	for id := 0; id < logicalConns; id++ {
		id := id
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				payload := fmt.Sprintf("logical-%d-req-%d", id, r)
				if resp := pool.Send(id, payload); resp != payload {
					atomic.AddInt64(&mismatches, 1)
				}
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&mismatches); got != 0 {
		t.Fatalf("mismatched responses = %d, want 0 (a logical connection received another one's response)", got)
	}
}

// TestTwoLogicalConnsSharingOnePhysicalConnDontCorrupt isolates the exact
// contention path: two logical connection IDs that map to the SAME
// physical connection (id and id+physicalConns both mod to 0), hammering
// it concurrently with distinguishable payload prefixes.
func TestTwoLogicalConnsSharingOnePhysicalConnDontCorrupt(t *testing.T) {
	const physicalConns = 4
	pool := New(physicalConns)

	const logicalA = 0
	const logicalB = physicalConns // 0 % 4 == 4 % 4 == 0: same physical conn
	const rounds = 500

	var wg sync.WaitGroup
	var mismatches int64
	wg.Add(2)

	send := func(logicalID int, prefix string) {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			payload := fmt.Sprintf("%s-req-%d", prefix, r)
			if resp := pool.Send(logicalID, payload); resp != payload {
				atomic.AddInt64(&mismatches, 1)
			}
		}
	}
	go send(logicalA, "A")
	go send(logicalB, "B")
	wg.Wait()

	if got := atomic.LoadInt64(&mismatches); got != 0 {
		t.Fatalf("mismatched responses = %d, want 0 (both logical connections shared one physical connection unsafely)", got)
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Send` is correct when every response a logical connection receives is
exactly the payload it sent — proven under `-race` with a large
enough burst of overlapping logical connections, and pinned precisely
with a test that deliberately forces two specific logical connections
onto one shared physical connection rather than hoping enough random
IDs collide. The mistake this design avoids is guarding the wrong
layer: synchronizing the pool's routing table (which physical
connection a logical ID maps to) protects a decision that never
actually races, while leaving the physical connection's real
request/response exchange — the part that genuinely has shared,
order-sensitive state — completely unguarded. `runtime.Gosched()`
inside `Send` is there to widen the window a missing lock would
expose, the same tool the demo and tests lean on to make a
timing-dependent bug reproducible instead of merely theoretically
possible.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the smallest possible span of genuinely shared, order-sensitive state.
- [PgBouncer FAQ: how transaction pooling works](https://www.pgbouncer.org/features.html) — the real-world shape of multiplexing many client sessions onto fewer server connections.
- [runtime.Gosched](https://pkg.go.dev/runtime#Gosched) — voluntarily yielding the current goroutine, used here to widen a race window for reproducibility.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-consistent-hash-ring-rebalance.md](31-consistent-hash-ring-rebalance.md) | Next: [33-cache-double-check-locking-race.md](33-cache-double-check-locking-race.md)
