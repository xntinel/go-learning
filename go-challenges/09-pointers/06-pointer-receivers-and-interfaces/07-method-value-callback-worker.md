# Exercise 7: Method Values as Callbacks: Binding the Receiver for a Worker Pool

When you hand `meter.Record` to a fan-out of goroutines, you are relying on a
precise rule: a method value bound from a pointer receiver captures the pointer, so
every goroutine updates the same underlying meter. This module builds a
`*RateMeter`, dispatches its bound `Record` method across a worker pool, and shows
the method-expression form produces the identical aggregate.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
ratemeter/                  independent module: example.com/ratemeter
  go.mod                    go 1.25
  meter.go                  type RateMeter (atomic.Int64); Record (pointer recv); Total
  cmd/
    demo/
      main.go               fan out meter.Record across goroutines
  meter_test.go             method value vs method expression, -race aggregate
```

- Files: `meter.go`, `cmd/demo/main.go`, `meter_test.go`.
- Implement: a `RateMeter` wrapping `atomic.Int64` with a pointer-receiver `Record(n int64)` and `Total()`.
- Test: capture `meter.Record` into a `func(int64)` callback, dispatch N goroutines, and assert the aggregate under `-race`; a subtest uses the method expression `(*RateMeter).Record` and asserts the same total.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/06-pointer-receivers-and-interfaces/07-method-value-callback-worker/cmd/demo
cd go-solutions/09-pointers/06-pointer-receivers-and-interfaces/07-method-value-callback-worker
go mod edit -go=1.25
```

### Method value vs method expression, and why binding a pointer shares state

Two ways to turn a method into a first-class function look similar but differ in
what they capture:

- A **method value** `meter.Record` binds the receiver *now*. Because `Record` has
  a pointer receiver, the bound value captures the pointer `meter`. The result has
  type `func(int64)` and every call goes through that same pointer — so a hundred
  goroutines holding copies of the callback all mutate one `RateMeter`. This is
  exactly what you want for a shared metric.
- A **method expression** `(*RateMeter).Record` leaves the receiver as an explicit
  first parameter. Its type is `func(*RateMeter, int64)`; you pass the receiver at
  each call. It is the same underlying method, just uncurried.

The trap the concepts file warns about is binding a *value*-receiver method: if
`Record` had a value receiver, `meter.Record` would capture a **copy** of the
meter, and increments through the callback would land on that copy and vanish. The
whole worker-pool pattern depends on `Record` having a pointer receiver so the
bound method value shares one `atomic.Int64`. The `atomic.Int64` then makes the
concurrent `Add` race-free.

Create `meter.go`:

```go
// meter.go
package ratemeter

import "sync/atomic"

// RateMeter accumulates a running total across many goroutines. Record has a
// pointer receiver so a bound method value (meter.Record) shares one meter.
type RateMeter struct {
	total atomic.Int64
}

// New returns a zeroed meter.
func New() *RateMeter {
	return &RateMeter{}
}

// Record atomically adds n to the running total. Pointer receiver: the update
// must persist and the atomic must not be copied.
func (m *RateMeter) Record(n int64) {
	m.total.Add(n)
}

// Total returns the accumulated sum.
func (m *RateMeter) Total() int64 {
	return m.total.Load()
}
```

### The runnable demo

The demo captures `meter.Record` as a plain `func(int64)` callback and dispatches
it from 100 goroutines, then prints the aggregate — proving the bound callback
shares the underlying meter.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"sync"

	"example.com/ratemeter"
)

func main() {
	meter := ratemeter.New()

	// Method value: binds the pointer receiver, so record is func(int64) sharing
	// the same meter.
	record := meter.Record

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(2) // every goroutine updates the SAME meter
		}()
	}
	wg.Wait()

	fmt.Printf("total via bound callback: %d\n", meter.Total())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total via bound callback: 200
```

### Tests

`TestMethodValueCallback` is the core: bind `meter.Record`, fan out, assert the
sum. `TestMethodExpressionEquivalent` uses `(*RateMeter).Record`, passing the
receiver explicitly, and asserts it reaches the same total — demonstrating the two
forms are the same underlying method. Both run under `-race`.

Create `meter_test.go`:

```go
// meter_test.go
package ratemeter

import (
	"sync"
	"testing"
)

func TestMethodValueCallback(t *testing.T) {
	t.Parallel()

	meter := New()
	var cb func(int64) = meter.Record // bound method value: shares the pointer

	const workers = 500
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb(3)
		}()
	}
	wg.Wait()

	if got := meter.Total(); got != workers*3 {
		t.Fatalf("Total() = %d, want %d", got, workers*3)
	}
}

func TestMethodExpressionEquivalent(t *testing.T) {
	t.Parallel()

	meter := New()
	// Method expression: receiver is an explicit first parameter.
	var record func(*RateMeter, int64) = (*RateMeter).Record

	const workers = 500
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(meter, 3)
		}()
	}
	wg.Wait()

	if got := meter.Total(); got != workers*3 {
		t.Fatalf("Total() via method expression = %d, want %d", got, workers*3)
	}
}

func TestRegistryOfCallbacks(t *testing.T) {
	t.Parallel()

	// A callback registry: several meters, each contributing its bound Record.
	m1, m2 := New(), New()
	callbacks := []func(int64){m1.Record, m2.Record}
	for _, cb := range callbacks {
		cb(10)
		cb(5)
	}
	if m1.Total() != 15 || m2.Total() != 15 {
		t.Fatalf("m1=%d m2=%d, want 15 and 15", m1.Total(), m2.Total())
	}
}
```

## Review

The pattern is correct when the bound `meter.Record` and the method expression
`(*RateMeter).Record` both drive the same meter to the same total — which they do
only because `Record` has a pointer receiver. Had it been a value receiver,
`TestMethodValueCallback` would fail with a total near zero: each goroutine's
callback would mutate a private copy. `TestRegistryOfCallbacks` shows the everyday
use — a slice of bound callbacks, each carrying its own receiver — which is how
worker pools, event handlers, and middleware chains carry state without a global.
The `atomic.Int64` is what keeps the shared updates race-free; run under `-race`
to confirm.

## Resources

- [Go Specification: Method values](https://go.dev/ref/spec#Method_values) — binding the receiver at the point of expression.
- [Go Specification: Method expressions](https://go.dev/ref/spec#Method_expressions) — the receiver-as-first-parameter form.
- [sync/atomic: Int64](https://pkg.go.dev/sync/atomic#Int64) — the race-free accumulator behind Record.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-repository-port-interface.md](06-repository-port-interface.md) | Next: [08-stringer-addressability-in-collections.md](08-stringer-addressability-in-collections.md)
