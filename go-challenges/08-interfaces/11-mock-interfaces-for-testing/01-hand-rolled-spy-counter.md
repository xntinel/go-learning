# Exercise 1: Hand-Rolled Spy — Assert a Service Calls Its Storage Port Correctly

A `Counter` service persists its running total through a one-method `Storage`
port after every mutation. The unit test cannot look inside a real database, so
it injects a hand-rolled spy that records every value passed to `Save` and then
asserts the exact recorded sequence — the canonical state-based verification of
an outbound contract.

This module is fully self-contained: its own module, its own package, its own
demo, and its own test. Nothing here imports another exercise.

## What you'll build

```text
spycounter/                  independent module: example.com/spycounter
  go.mod                     go 1.26
  counter.go                 Storage interface; Counter with Inc, Add, Value
  cmd/
    demo/
      main.go                runnable demo wiring a tiny real Storage
  counter_test.go            spyStorage recording Save values; sequence asserts
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `Counter` over a `Storage` interface, persisting its new total via `Save` after each `Inc`/`Add`, exposing `Value` via `atomic.Int64`.
- Test: a mutex-guarded `spyStorage` recording every `Save(value)`; assert the sequence is `[1,2]` for two `Inc` and `[1,6,7]` for `Inc`, `Add(5)`, `Inc`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/11-mock-interfaces-for-testing/01-hand-rolled-spy-counter/cmd/demo
cd go-solutions/08-interfaces/11-mock-interfaces-for-testing/01-hand-rolled-spy-counter
```

### Why a spy, and why state-based verification

The `Counter` has one job you cannot observe from its return values alone: it
must persist its new total to storage after every mutation. `Inc` returns only an
error; nothing in its signature tells you it called `Save`, let alone with what.
That outbound call *is* the contract, so the test must observe it — and the way to
observe it is to inject a double that records what it was asked to do.

A *spy* is exactly that double: an implementation of `Storage` that appends every
`Save` argument to a slice. After driving the `Counter`, the test reads that slice
and asserts it equals the sequence the contract requires. This is *state-based*
verification: we assert on the recorded state the spy accumulated, not on a
pre-programmed expectation. It is the least-coupled way to check an interaction —
the spy imposes no ordering or count constraints of its own; the test decides
exactly what to assert.

The `Storage` interface is deliberately one method wide. It is defined at the
consumer (`counter.go`), sized to precisely what `Counter` uses. That is why the
spy is a handful of lines: a fat interface would force the spy to implement
methods the `Counter` never calls, and those would be dead weight that asserts
nothing.

### Why atomic and why a mutex on the spy

`Counter.value` is a `sync/atomic.Int64` so that concurrent `Inc` calls each get a
distinct, correctly-ordered total without a lock in the SUT. The spy, meanwhile,
guards its recorded slice with a `sync.Mutex`: the SUT may call `Save` from many
goroutines (Exercise 2 pushes this hard), and a slice appended from several
goroutines without synchronization is a data race the `-race` detector will flag.
The accessor `Calls()` returns a *defensive copy* so a test iterating the record
cannot race a concurrent append and cannot mutate the spy's internal slice.

Create `counter.go`:

```go
package spycounter

import "sync/atomic"

// Storage is the outbound port the Counter persists through. It is defined here,
// at the consumer, and is exactly one method wide.
type Storage interface {
	Save(value int64) error
}

// Counter keeps a running total and persists every new total through Storage.
type Counter struct {
	storage Storage
	value   atomic.Int64
}

// New wires a Counter to a Storage.
func New(s Storage) *Counter {
	return &Counter{storage: s}
}

// Inc adds one to the total and persists the new total.
func (c *Counter) Inc() error {
	v := c.value.Add(1)
	return c.storage.Save(v)
}

// Add adds n to the total and persists the new total.
func (c *Counter) Add(n int64) error {
	v := c.value.Add(n)
	return c.storage.Save(v)
}

// Value reports the current total without touching storage.
func (c *Counter) Value() int64 {
	return c.value.Load()
}
```

### The runnable demo

The demo wires the `Counter` to a trivial real `Storage` (an in-memory recorder)
so you can watch the persisted sequence against an actual implementation, not a
test double. It runs `Inc`, `Add(5)`, `Inc` and prints what storage received.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/spycounter"
)

// recorder is a tiny real Storage that keeps every saved value.
type recorder struct {
	saved []int64
}

func (r *recorder) Save(v int64) error {
	r.saved = append(r.saved, v)
	return nil
}

func main() {
	r := &recorder{}
	c := spycounter.New(r)

	_ = c.Inc()
	_ = c.Add(5)
	_ = c.Inc()

	fmt.Printf("value: %d\n", c.Value())
	fmt.Printf("saved: %v\n", r.saved)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value: 7
saved: [1 6 7]
```

### Tests

`TestCounterPersistsIncrementSequence` proves two `Inc` calls persist `[1,2]`.
`TestCounterPersistsMixedSequence` proves `Inc`, `Add(5)`, `Inc` persists
`[1,6,7]` — the call-order contract from the original lesson's verification, now a
first-class test. Both use `slices.Equal` on the spy's defensive copy. The
`Example` documents the persisted sequence with an `// Output:` block that
`go test` verifies.

Create `counter_test.go`:

```go
package spycounter

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// spyStorage records every value passed to Save. It is concurrency-safe so it
// can be reused by the -race concurrency test in later exercises.
type spyStorage struct {
	mu    sync.Mutex
	saves []int64
}

func (s *spyStorage) Save(v int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, v)
	return nil
}

// Calls returns a defensive copy of the recorded values.
func (s *spyStorage) Calls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.saves)
}

func TestCounterPersistsIncrementSequence(t *testing.T) {
	t.Parallel()

	spy := &spyStorage{}
	c := New(spy)

	if err := c.Inc(); err != nil {
		t.Fatalf("Inc: %v", err)
	}
	if err := c.Inc(); err != nil {
		t.Fatalf("Inc: %v", err)
	}

	got, want := spy.Calls(), []int64{1, 2}
	if !slices.Equal(got, want) {
		t.Fatalf("saved = %v, want %v", got, want)
	}
}

func TestCounterPersistsMixedSequence(t *testing.T) {
	t.Parallel()

	spy := &spyStorage{}
	c := New(spy)

	if err := c.Inc(); err != nil {
		t.Fatalf("Inc: %v", err)
	}
	if err := c.Add(5); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Inc(); err != nil {
		t.Fatalf("Inc: %v", err)
	}

	got, want := spy.Calls(), []int64{1, 6, 7}
	if !slices.Equal(got, want) {
		t.Fatalf("saved = %v, want %v", got, want)
	}
	if c.Value() != 7 {
		t.Fatalf("Value = %d, want 7", c.Value())
	}
}

func Example() {
	spy := &spyStorage{}
	c := New(spy)
	_ = c.Inc()
	_ = c.Add(5)
	_ = c.Inc()
	fmt.Println(spy.Calls())
	// Output: [1 6 7]
}
```

## Review

The `Counter` is correct when the value it persists equals the value it computes:
`Inc` and `Add` both call `Save` with the *new* total, and the spy's recorded
sequence is the proof. The two sequence tests pin both the values and their order
without any pre-programmed expectation object — the spy just records, the test
decides what to assert. That is the least-coupled form of interaction check:
nothing here breaks if you rename an internal field or reorder unrelated code, but
it goes red the instant the persisted values or their order change.

The traps are the ones from the concepts file made concrete. Do not widen
`Storage` beyond `Save` — a fat port makes the spy carry dead methods. Do not skip
the assertion on `spy.Calls()` — a spy you never read is a no-op. And keep the
spy's slice behind its mutex with a defensive-copy accessor: run `go test -race`
to confirm the record is safe, which matters the moment concurrency enters in the
next exercise.

## Resources

- [`testing` package](https://pkg.go.dev/testing) — the test harness, `t.Parallel`, and `Example` output verification.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` for lock-free counter updates.
- [Martin Fowler: Mocks Aren't Stubs](https://martinfowler.com/articles/mocksArentStubs.html) — the spy/stub/mock/fake taxonomy and state-vs-interaction verification.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-stub-error-injection-and-concurrency.md](02-stub-error-injection-and-concurrency.md)
