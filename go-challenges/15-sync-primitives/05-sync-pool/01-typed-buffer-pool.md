# Exercise 1: A Type-Safe Buffer Pool With Allocation Metrics

`sync.Pool` traffics in `any`, so raw use sprays `.(*bytes.Buffer)` assertions
across a codebase and offers no way to know whether the pool is actually saving
allocations. This module wraps a `sync.Pool` in a small typed API that hides the
assertion, resets every buffer on the way out, and counts allocations, gets, and
puts so a caller can quantify the win.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
bufferpool/                 independent module: example.com/bufferpool
  go.mod                    go 1.26
  pool.go                   type Pool wrapping sync.Pool; New, Get, Put, Stats
  cmd/
    demo/
      main.go               exercises the pool and prints Stats
  pool_test.go              Get returns clean buffer; reset-on-Get; counters; Example
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: a `Pool` over `sync.Pool` for `*bytes.Buffer` with `New`, `Get` (returns a reset buffer), `Put`, and `Stats() (allocated, gets, puts int64)`.
Test: `Get` returns a non-nil usable buffer; a `Get` after `Put` is clean (len 0); counters increment; run under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p bufferpool/buffers bufferpool/cmd/demo
cd bufferpool
go mod init example.com/bufferpool
```

### Why wrap the pool at all

A bare `sync.Pool` has two ergonomic problems on a hot path. First, every call
site repeats `p.Get().(*bytes.Buffer)`, and every one of those assertions is a
latent panic if someone ever forgets to set `New`. Second, there is no signal
about whether the pool earns its keep — you cannot see how many allocations it
avoided. The wrapper fixes both: `Get` returns a concrete `*bytes.Buffer`
already reset, and three `atomic.Int64` counters record exactly how the pool
behaved.

The `allocated` counter is incremented only inside `New`, which the runtime
calls only when it has to manufacture a fresh buffer. That makes `allocated` the
precise answer to "how many buffers did this pool have to create?" — and
therefore, against the `gets` count, "how much did reuse save?". On a healthy
hot path `gets` climbs into the millions while `allocated` stays near
`GOMAXPROCS`.

`Get` calls `buf.Reset()` before handing the buffer back. Resetting on the way
*out* rather than trusting callers to reset on the way *in* centralizes the
contract in one place: no matter how a buffer was left by its previous user, the
next `Get` always yields a clean, zero-length buffer that still retains its
grown capacity. That capacity retention is the whole point — the backing array
is reused, only the length is zeroed.

Create `buffers/pool.go`:

```go
package buffers

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// Pool is a type-safe wrapper over sync.Pool for *bytes.Buffer. It hides the
// any-to-*bytes.Buffer assertion, resets every buffer on Get, and records
// allocation/get/put counters so callers can quantify reuse.
type Pool struct {
	p         sync.Pool
	allocated atomic.Int64
	gets      atomic.Int64
	puts      atomic.Int64
}

// New returns a Pool whose New function allocates a fresh *bytes.Buffer and
// counts the allocation. Returning a pointer avoids the interface-boxing
// allocation a value type would incur (see staticcheck SA6002).
func New() *Pool {
	p := &Pool{}
	p.p.New = func() any {
		p.allocated.Add(1)
		return new(bytes.Buffer)
	}
	return p
}

// Get returns a reset (zero-length) buffer, allocating one only if the pool is
// empty. The returned buffer retains any capacity a previous user grew it to.
func (p *Pool) Get() *bytes.Buffer {
	p.gets.Add(1)
	buf := p.p.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// Put returns a buffer to the pool for reuse.
func (p *Pool) Put(buf *bytes.Buffer) {
	p.puts.Add(1)
	p.p.Put(buf)
}

// Stats reports the cumulative counters: buffers allocated by New, total Gets,
// and total Puts. allocated much smaller than gets means reuse is working.
func (p *Pool) Stats() (allocated, gets, puts int64) {
	return p.allocated.Load(), p.gets.Load(), p.puts.Load()
}
```

### The runnable demo

The demo drives a handful of Get/write/Put cycles on a single goroutine and
prints the counters. Because the calls are sequential with no GC, the pool's
per-P private slot hands the same buffer back each time, so exactly one buffer
is ever allocated.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bufferpool/buffers"
)

func main() {
	p := buffers.New()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		buf := p.Get()
		fmt.Fprintf(buf, "user:%s", name)
		fmt.Println(buf.String())
		p.Put(buf)
	}
	allocated, gets, puts := p.Stats()
	fmt.Printf("allocated=%d gets=%d puts=%d\n", allocated, gets, puts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:alpha
user:beta
user:gamma
allocated=1 gets=3 puts=3
```

### Tests

The tests pin the two behaviors that make the wrapper correct: `Get` yields a
usable buffer, and a buffer that was left dirty comes back clean. The counters
test confirms `gets`/`puts` track every call.

Create `buffers/pool_test.go`:

```go
package buffers

import (
	"fmt"
	"sync"
	"testing"
)

func TestGetReturnsUsableBuffer(t *testing.T) {
	t.Parallel()

	p := New()
	buf := p.Get()
	if buf == nil {
		t.Fatal("Get returned nil")
	}
	buf.WriteString("hello")
	if got := buf.String(); got != "hello" {
		t.Fatalf("buffer = %q, want %q", got, "hello")
	}
	p.Put(buf)
}

func TestGetResetsDirtyBuffer(t *testing.T) {
	t.Parallel()

	p := New()
	buf := p.Get()
	buf.WriteString("stale data from a previous request")
	p.Put(buf) // returned dirty on purpose

	got := p.Get()
	if got.Len() != 0 {
		t.Fatalf("Get after Put returned dirty buffer: len=%d, want 0", got.Len())
	}
	p.Put(got)
}

func TestStatsCounters(t *testing.T) {
	t.Parallel()

	p := New()
	const n = 100
	for range n {
		buf := p.Get()
		buf.WriteByte('x')
		p.Put(buf)
	}
	allocated, gets, puts := p.Stats()
	if gets != n {
		t.Errorf("gets = %d, want %d", gets, n)
	}
	if puts != n {
		t.Errorf("puts = %d, want %d", puts, n)
	}
	if allocated < 1 || allocated > n {
		t.Errorf("allocated = %d, want between 1 and %d", allocated, n)
	}
}

func TestConcurrentGetPut(t *testing.T) {
	t.Parallel()

	p := New()
	const goroutines = 50
	const perGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				buf := p.Get()
				buf.WriteByte('x')
				if buf.String() != "x" {
					t.Errorf("dirty buffer: %q", buf.String())
				}
				p.Put(buf)
			}
		}()
	}
	wg.Wait()
}

func ExamplePool() {
	p := New()
	buf := p.Get()
	fmt.Fprint(buf, "hello")
	fmt.Println(buf.String())
	p.Put(buf)
	// Output: hello
}
```

## Review

The wrapper is correct when `Get` never returns a dirty or `nil` buffer and the
counters reflect reality. `TestGetResetsDirtyBuffer` deliberately returns a
buffer full of "previous request" bytes and proves the next `Get` is clean —
that is the reset contract that keeps one caller's data out of another's. Do not
be tempted to assert `allocated == 1`: per-P sharding and `-race` scheduling
make the exact number nondeterministic, which is why the counters test checks a
range. The demo can print `allocated=1` only because it is single-goroutine and
sequential; the moment concurrency enters, allocation count becomes a range, not
a constant. Run `go test -race` to confirm the atomic counters and the pool are
safe under concurrent Get/Put.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — Get, Put, New, and the "may be removed at any time" guarantee.
- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — the lock-free counters used for Stats.
- [`bytes.Buffer.Reset`](https://pkg.go.dev/bytes#Buffer.Reset) — clears length while retaining capacity.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pooled-append-helper.md](02-pooled-append-helper.md)
