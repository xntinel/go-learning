# Exercise 26: Observable Stat Delta — Capture Snapshot and Compute Final Delta

**Nivel: Intermedio** — validacion rapida (un test corto).

A function might adjust an observable metric — a connection-pool size, a
queue depth, bytes buffered — several times over its lifetime, and in
different ways depending on which branch it takes. Rather than threading a
running total through every branch by hand, snapshot the metric once at
entry and defer a closure that computes the net delta at exit, whatever
path the function took to get there.

## What you'll build

```text
statdelta/                  independent module: example.com/statdelta
  go.mod
  statdelta/statdelta.go     Gauge; Record (snapshot at entry, deferred delta at exit)
  cmd/demo/main.go            connection-pool gauge; net delta over a request
  statdelta/statdelta_test.go success delta; delta recorded on error; delta recorded on panic
```

- Files: `statdelta/statdelta.go`, `cmd/demo/main.go`, `statdelta/statdelta_test.go`.
- Implement: a `Gauge` with `Add(delta int64)` and `Value() int64`; and `Record(g *Gauge, record func(delta int64), work func() error) (err error)`, which snapshots `g.Value()` before calling `work`, and defers a closure that calls `record` with `g.Value() - start` no matter how `work` returns.
- Test: `work` adjusts the gauge several times and the correct net delta is recorded on success; `work` returns an error and the delta up to that point is still recorded; `work` panics and the delta is still recorded before the panic propagates.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/26-observable-stat-snapshot-defer/statdelta go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/26-observable-stat-snapshot-defer/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/26-observable-stat-snapshot-defer
go mod edit -go=1.24
```

### Why a deferred closure, not a return value

`work`'s own return type is `error` — there is nowhere in its signature to
also report "and by the way, here is how much the gauge moved." `Record`
solves that by owning the bookkeeping itself: it takes the snapshot before
calling `work`, and defers a closure that reads the gauge's value one more
time right before `Record` returns. Because a deferred function runs after
the surrounding function's other statements have finished — including after
a `return` statement has evaluated its arguments, and even after a panic
begins unwinding — the delta calculation always sees the gauge's true final
value, regardless of which of `work`'s internal branches actually ran or
whether `work` even returned normally. The caller does not need to trust
`work` to report its own effect; `Record` observes it directly from the
outside.

Create `statdelta/statdelta.go`:

```go
package statdelta

// Gauge is a simple observable integer metric -- e.g. an in-flight
// connection count or a queue depth -- that a function under instrumentation
// may adjust an arbitrary number of times while it runs.
type Gauge struct {
	value int64
}

// Add adjusts the gauge by delta (positive or negative).
func (g *Gauge) Add(delta int64) { g.value += delta }

// Value returns the gauge's current value.
func (g *Gauge) Value() int64 { return g.value }

// Record snapshots g at entry, runs work, and -- via a deferred closure --
// computes the net delta g moved by during work and passes it to record.
// Because the closure is deferred, the delta is captured on every exit path
// from work: normal return, error return, or panic.
func Record(g *Gauge, record func(delta int64), work func() error) (err error) {
	start := g.Value()
	defer func() {
		record(g.Value() - start)
	}()

	return work()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statdelta/statdelta"
)

func main() {
	pool := &statdelta.Gauge{}
	pool.Add(5) // five connections already open before this request

	var delta int64
	err := statdelta.Record(pool, func(d int64) { delta = d }, func() error {
		pool.Add(3)  // opened three more
		pool.Add(-1) // closed one
		return nil
	})

	fmt.Println("err:", err)
	fmt.Println("delta:", delta)
	fmt.Println("pool now:", pool.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: <nil>
delta: 2
pool now: 7
```

### Tests

Create `statdelta/statdelta_test.go`:

```go
package statdelta

import (
	"errors"
	"testing"
)

func TestRecordCapturesNetDeltaOnSuccess(t *testing.T) {
	t.Parallel()

	g := &Gauge{}
	g.Add(10)

	var delta int64
	err := Record(g, func(d int64) { delta = d }, func() error {
		g.Add(5)
		g.Add(-2)
		return nil
	})

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if delta != 3 {
		t.Fatalf("delta = %d, want 3", delta)
	}
	if g.Value() != 13 {
		t.Fatalf("g.Value() = %d, want 13", g.Value())
	}
}

func TestRecordCapturesDeltaEvenOnError(t *testing.T) {
	t.Parallel()

	g := &Gauge{}
	boom := errors.New("boom")

	var delta int64
	err := Record(g, func(d int64) { delta = d }, func() error {
		g.Add(4)
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want errors.Is %v", err, boom)
	}
	if delta != 4 {
		t.Fatalf("delta = %d, want 4 (recorded despite error)", delta)
	}
}

func TestRecordCapturesDeltaEvenOnPanic(t *testing.T) {
	t.Parallel()

	g := &Gauge{}
	var delta int64

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = Record(g, func(d int64) { delta = d }, func() error {
			g.Add(9)
			panic("work exploded")
		})
	}()

	if delta != 9 {
		t.Fatalf("delta = %d, want 9 (recorded despite panic)", delta)
	}
}
```

## Review

The delta is correct when it reflects however many times, and in whichever
direction, `work` adjusted the gauge, on every exit path — success, error,
or panic. The mistake this pattern exists to prevent is computing the delta
inline right before each individual `return` statement inside `work`,
which has to be duplicated at every exit point and is silently skipped by
any exit point someone forgets to update (most dangerously, a panic, which
skips ordinary code entirely but still runs defers). Moving the snapshot
and the delta calculation into a wrapper that defers the second half means
there is exactly one place the bookkeeping can go wrong, and it is
unreachable code to get it wrong at.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-nested-transaction-chain-rollback.md](25-nested-transaction-chain-rollback.md) | Next: [27-schema-migration-down-stack.md](27-schema-migration-down-stack.md)
