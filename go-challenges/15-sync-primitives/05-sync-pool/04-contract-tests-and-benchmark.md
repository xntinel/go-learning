# Exercise 4: Pinning 0 allocs/op — The Benchmark That Proves It

A pool that you have not benchmarked is a liability, not an optimization. This
module is the proof harness: the full contract test suite (reset-on-reuse,
concurrent Get/Put with a reuse assertion) plus a `BenchmarkPool` versus
`BenchmarkNoPool` pair that demonstrates the pooled path reports `0 allocs/op`
after warmup while the unpooled path allocates every iteration.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
poolcontract/               independent module: example.com/poolcontract
  go.mod                    go 1.26
  buffers/
    pool.go                 the typed *bytes.Buffer pool (bundled)
  cmd/
    demo/
      main.go               prints the concurrent reuse ratio
  buffers/pool_test.go      reset, concurrent reuse, BenchmarkPool vs BenchmarkNoPool
```

Files: `buffers/pool.go`, `cmd/demo/main.go`, `buffers/pool_test.go`.
Implement: reuse the typed `Pool`; add the full test suite and a benchmark pair.
Test: reset-on-reuse; a many-goroutine Get/Put stress asserting `allocated < total ops` (reuse occurred); `BenchmarkPool` (pooled) vs `BenchmarkNoPool` (fresh allocation).
Verify: `go test -count=1 -race ./...`, then `go test -bench=. -benchmem -run=^$ ./buffers`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/05-sync-pool/04-contract-tests-and-benchmark/buffers go-solutions/15-sync-primitives/05-sync-pool/04-contract-tests-and-benchmark/cmd/demo
cd go-solutions/15-sync-primitives/05-sync-pool/04-contract-tests-and-benchmark
```

### What the benchmark actually proves, and what it cannot

Two things make the benchmark the module's hard gate. `BenchmarkPool` runs
`Get`/write/`Put` in a loop; after the first iteration warms the pool, every
subsequent `Get` returns a recycled buffer, so `-benchmem` reports `0 allocs/op`
and `0 B/op`. `BenchmarkNoPool` does the honest baseline — `new(bytes.Buffer)`
every iteration — and reports at least one allocation per op. The delta between
the two is the entire justification for the pool, expressed as a number you can
put in a commit message.

What the benchmark cannot prove is an *exact* allocation count, and the
concurrent test is deliberately written to respect that. With per-P sharding
under `GOMAXPROCS > 1`, and extra Ps injected by `-race`, up to one buffer can be
live per P at any instant, and GC timing perturbs the total. So the correct
assertion is a *range*: after tens of thousands of operations across many
goroutines, `allocated` must be far below the total number of operations —
proving reuse happened — but you must never assert it equals one. A test that
asserts `allocated == 1` is not stricter; it is broken, and it will flake under
`-race` on a multi-core machine.

The bundled `Pool` is the same typed wrapper from Exercise 1.

Create `buffers/pool.go`:

```go
package buffers

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// Pool is a type-safe wrapper over sync.Pool for *bytes.Buffer.
type Pool struct {
	p         sync.Pool
	allocated atomic.Int64
	gets      atomic.Int64
	puts      atomic.Int64
}

// New returns a Pool whose New allocates and counts a fresh *bytes.Buffer.
func New() *Pool {
	p := &Pool{}
	p.p.New = func() any {
		p.allocated.Add(1)
		return new(bytes.Buffer)
	}
	return p
}

// Get returns a reset buffer, allocating only if the pool is empty.
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

// Stats reports the cumulative counters: allocated, gets, puts.
func (p *Pool) Stats() (allocated, gets, puts int64) {
	return p.allocated.Load(), p.gets.Load(), p.puts.Load()
}
```

### The runnable demo

The demo runs a concurrent stress batch and prints the reuse ratio: total
operations against buffers actually allocated. The exact allocated count varies
by machine and scheduling, so the demo prints the ratio as evidence of reuse
rather than a fixed number.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/poolcontract/buffers"
)

func main() {
	p := buffers.New()
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
				p.Put(buf)
			}
		}()
	}
	wg.Wait()

	allocated, gets, _ := p.Stats()
	fmt.Printf("gets=%d allocated=%d reuse=%t\n", gets, allocated, allocated < gets)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the exact `allocated` count varies; `reuse=true` always holds):

```
gets=10000 allocated=13 reuse=true
```

### Tests and benchmarks

`TestResetOnReuse` proves a dirty buffer comes back clean. `TestConcurrentReuse`
is the hard gate: it asserts `allocated < totalOps`, i.e. the pool reused buffers
rather than allocating one per operation. The benchmark pair quantifies the win.

Create `buffers/pool_test.go`:

```go
package buffers

import (
	"bytes"
	"sync"
	"testing"
)

func TestResetOnReuse(t *testing.T) {
	t.Parallel()

	p := New()
	buf := p.Get()
	buf.WriteString("previous request body")
	p.Put(buf)

	got := p.Get()
	if got.Len() != 0 {
		t.Fatalf("reused buffer not reset: len=%d, want 0", got.Len())
	}
	p.Put(got)
}

func TestConcurrentReuse(t *testing.T) {
	t.Parallel()

	p := New()
	const goroutines = 50
	const perGoroutine = 200
	const totalOps = int64(goroutines * perGoroutine)

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

	allocated, gets, puts := p.Stats()
	if gets != totalOps || puts != totalOps {
		t.Fatalf("gets=%d puts=%d, want %d each", gets, puts, totalOps)
	}
	// The exact count is nondeterministic (per-P sharding, GOMAXPROCS, -race).
	// The provable contract is that reuse happened at all: far fewer buffers
	// were allocated than operations performed.
	if allocated >= totalOps {
		t.Fatalf("allocated=%d >= totalOps=%d: no reuse occurred", allocated, totalOps)
	}
}

func BenchmarkPool(b *testing.B) {
	p := New()
	b.ReportAllocs()
	for range b.N {
		buf := p.Get()
		buf.WriteString("x")
		p.Put(buf)
	}
}

func BenchmarkNoPool(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		buf := new(bytes.Buffer)
		buf.WriteString("x")
		_ = buf
	}
}
```

Measure the delta:

```bash
go test -bench=. -benchmem -run=^$ ./buffers
```

Expected shape (numbers vary by machine; the allocs/op column is the point):

```
BenchmarkPool-10        100000000    10.5 ns/op     0 B/op     0 allocs/op
BenchmarkNoPool-10       30000000    38.2 ns/op    64 B/op     1 allocs/op
```

## Review

The suite is correct when `TestConcurrentReuse` passes with `allocated` far below
`totalOps` and the benchmark reports `0 allocs/op` for the pooled path against a
nonzero baseline. The single most important discipline here is what you do *not*
assert: never `allocated == 1`. Per-P sharding means several buffers can be live
at once, `-race` adds Ps, and GC can drain the pool mid-run — a fixed-count
assertion is a flake generator. Assert the reuse *range* in tests and read the
exact number from `-benchmem`. Run `go test -race` for correctness, then the
`-bench` line for the numbers that justify the pool's existence.

## Resources

- [`testing.B` and benchmarks](https://pkg.go.dev/testing#B) — `b.N`, `b.ReportAllocs`, and how `-benchmem` reports allocs/op.
- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — the per-P sharding model that makes exact counts nondeterministic.
- [Go blog: Profiling Go Programs](https://go.dev/blog/pprof) — turning a benchmark signal into an allocation profile.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-pool-demo-cli.md](03-pool-demo-cli.md) | Next: [05-json-response-encoder-handler.md](05-json-response-encoder-handler.md)
