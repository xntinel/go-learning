# Exercise 6: Thread-Safe Monotonic ID Generator as a Closure

The clearest demonstration that each factory call creates an independent
environment is a sequence generator. `NewSequence(start)` returns a `Next`
closure over a private atomic counter; call the factory twice and you get two
generators with fully separate state. Framed here as a request-ID / span-ID
allocator, it also shows how to make the captured state concurrency-safe with
`sync/atomic` instead of a mutex.

This module is fully self-contained.

## What you'll build

```text
sequence/                  independent module: example.com/sequence
  go.mod                   go 1.26
  sequence.go              NewSequence(start int64) func() int64
  cmd/
    demo/
      main.go              two independent generators, interleaved
  sequence_test.go         monotonic, independence, 1000-goroutine distinctness
```

- Files: `sequence.go`, `cmd/demo/main.go`, `sequence_test.go`.
- Implement: `NewSequence(start int64) func() int64` returning a `Next` closure over a private `atomic.Int64`, yielding `start, start+1, ...`.
- Test: sequential calls return `start, start+1, ...`; two generators do not share state; 1000 goroutines calling `Next` produce 1000 distinct values under `-race`.
- Verify: `go test -count=1 -race ./...`

### One factory call, one private counter

`NewSequence` declares an `atomic.Int64`, seeds it to `start-1`, and returns a
closure that calls `counter.Add(1)` — so the first `Next` yields `start`, the
second `start+1`, and so on. The counter is captured by the returned closure and
reachable only through it: there is no package-level global, no shared registry.
That is the point. Because the counter is a local variable moved to the heap by
escape analysis, *each* call to `NewSequence` allocates a fresh one. Two
generators built from two factory calls increment two different counters and can
never collide — the isolation that makes closures a substitute for per-instance
struct fields.

Concurrency safety comes from the type, not a lock. `atomic.Int64.Add` is a
single atomic read-modify-write, so many goroutines can call `Next` at once
without losing an update or handing out a duplicate. A mutex would work too, but
for a lone counter the atomic is smaller and faster and expresses the intent
exactly: one monotonically increasing integer, safe under concurrency. The
1000-goroutine test proves there are no lost updates by collecting every returned
value and asserting they are all distinct.

Create `sequence.go`:

```go
package sequence

import "sync/atomic"

// NewSequence returns a Next closure yielding start, start+1, start+2, ...
// Each call to NewSequence creates an independent generator with its own private
// counter; two generators never share state. Next is safe for concurrent use.
func NewSequence(start int64) func() int64 {
	var counter atomic.Int64
	counter.Store(start - 1)
	return func() int64 {
		return counter.Add(1)
	}
}
```

### The runnable demo

The demo shows independence directly: it interleaves two generators and you can
see each one advance on its own private counter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sequence"
)

func main() {
	requestID := sequence.NewSequence(1000)
	fmt.Println(requestID())
	fmt.Println(requestID())
	fmt.Println(requestID())

	spanID := sequence.NewSequence(1) // separate, private counter
	fmt.Println(spanID())
	fmt.Println(requestID()) // requestID kept advancing independently
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1000
1001
1002
1
1003
```

### Tests

Create `sequence_test.go`:

```go
package sequence

import (
	"sync"
	"testing"
)

func TestSequenceIsMonotonic(t *testing.T) {
	t.Parallel()
	next := NewSequence(100)
	for i := int64(100); i < 105; i++ {
		if got := next(); got != i {
			t.Fatalf("Next() = %d, want %d", got, i)
		}
	}
}

func TestGeneratorsAreIndependent(t *testing.T) {
	t.Parallel()
	a := NewSequence(0)
	b := NewSequence(0)

	if got := a(); got != 0 {
		t.Fatalf("a() = %d, want 0", got)
	}
	if got := a(); got != 1 {
		t.Fatalf("a() = %d, want 1", got)
	}
	if got := b(); got != 0 {
		t.Fatalf("b() = %d, want 0 (b must not see a's increments)", got)
	}
}

func TestConcurrentDistinct(t *testing.T) {
	t.Parallel()
	next := NewSequence(1)

	const n = 1000
	ids := make(chan int64, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- next()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[int64]bool, n)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %d (lost update)", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct ids, want %d", len(seen), n)
	}
}
```

## Review

The generator is correct when `Next` returns a strictly increasing run starting
at `start`, when two generators from separate factory calls advance
independently, and when 1000 concurrent calls yield 1000 distinct values.
`TestGeneratorsAreIndependent` is the closure-isolation proof: `b` starts at 0
regardless of how far `a` has advanced, because each `NewSequence` captured its
own counter. `TestConcurrentDistinct` is the safety proof: the atomic guarantees
no two callers observe the same value, so the distinctness set is full under
`-race`. Reach for `sync/atomic` rather than a mutex whenever the shared state is
a single counter. Run `go test -race`.

## Resources

- [pkg.go.dev: sync/atomic Int64](https://pkg.go.dev/sync/atomic#Int64) — `Store` and `Add` on an atomic integer.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — closures capturing local variables.
- [The Go Memory Model](https://go.dev/ref/mem) — why atomics are safe across goroutines.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-memoize-config-loader.md](05-memoize-config-loader.md) | Next: [07-circuit-breaker-closure.md](07-circuit-breaker-closure.md)
