# Exercise 2: Stub Error Injection and a Concurrency-Safe Mock Under the Race Detector

A storage-backed counter has two behaviors a spy alone cannot check: does it
propagate a storage failure to its caller while still advancing its own state, and
is it correct when many goroutines increment it at once? This module answers both
with a *stub* that injects a canned error and a *spy* that stays race-free under a
hundred concurrent producers.

Fully self-contained: its own module, package, demo, and test.

## What you'll build

```text
stubcounter/                 independent module: example.com/stubcounter
  go.mod                     go 1.26
  counter.go                 Storage interface; Counter with Inc, Add, Value
  cmd/
    demo/
      main.go                runnable demo showing a propagated storage error
  counter_test.go            error-injecting stub; 100-goroutine race test
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: the same `Counter` over `Storage`, whose `Inc`/`Add` advance the total *before* calling `Save`, so a storage failure does not roll the total back.
- Test: a stub whose `Save` returns a configured error once (assert `err != nil` and `Value()==1`); a mutex-guarded spy under 100 concurrent `Inc` goroutines (assert `Value()==100` and `len(Calls())==100`).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/11-mock-interfaces-for-testing/02-stub-error-injection-and-concurrency/cmd/demo
cd go-solutions/08-interfaces/11-mock-interfaces-for-testing/02-stub-error-injection-and-concurrency
```

### The error-propagation contract, and why state still advances

`Inc` computes the new total with `c.value.Add(1)` and *then* calls
`c.storage.Save(v)`, returning whatever `Save` returns. The ordering is the
contract worth pinning: the in-memory total advances first, so even when `Save`
fails the counter's own `Value()` reflects the increment. That models a common
real design — the authoritative in-process count moves forward and the persistence
failure is surfaced to the caller to retry or log, rather than silently dropping
the increment or rolling it back.

To test that branch you need the SUT to experience a storage failure on demand,
which the real storage will not do reliably. A *stub* supplies the canned answer:
its `Save` is programmed to return a specific error. Note the difference from
Exercise 1's spy — the spy records inputs, the stub dictates outputs. Here we want
to steer the SUT down its error path, so we dictate the output. The stub returns
the configured error exactly once and then reverts to success, which lets one test
assert both "the failure propagated" and "a later call succeeds" without a fresh
stub.

### Why the double must be concurrency-safe

`Counter` is safe to call from many goroutines by design: `atomic.Int64.Add` gives
each `Inc` a distinct total with no lock. But the double the test injects is
*also* called from all those goroutines, and it is ordinary code. A spy that does
`s.saves = append(s.saves, v)` from 100 goroutines with no lock has a data race —
in the test, not the SUT — and `go test -race` will fail the build for it, exactly
as it should. The double is production-shaped code and must be correct under the
same concurrency the SUT imposes. So the spy guards its slice with a `sync.Mutex`
and hands back a defensive copy. The concurrency test then asserts two independent
facts: the atomic counter reached exactly `n` (no lost updates), and the spy
recorded exactly `n` `Save` calls (no dropped persistences).

Create `counter.go`:

```go
package stubcounter

import "sync/atomic"

// Storage is the outbound persistence port, one method wide, defined here at the
// consumer.
type Storage interface {
	Save(value int64) error
}

// Counter advances its in-memory total, then persists it. A Save failure is
// returned to the caller but does not roll the total back.
type Counter struct {
	storage Storage
	value   atomic.Int64
}

func New(s Storage) *Counter {
	return &Counter{storage: s}
}

func (c *Counter) Inc() error {
	v := c.value.Add(1)
	return c.storage.Save(v)
}

func (c *Counter) Add(n int64) error {
	v := c.value.Add(n)
	return c.storage.Save(v)
}

func (c *Counter) Value() int64 {
	return c.value.Load()
}
```

### The runnable demo

The demo wires a storage that fails the first `Save` and succeeds after, showing
that the first `Inc` returns an error yet `Value()` is already 1, and the second
`Inc` succeeds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/stubcounter"
)

// flakyStorage fails the first Save, then succeeds.
type flakyStorage struct {
	calls int
}

func (f *flakyStorage) Save(int64) error {
	f.calls++
	if f.calls == 1 {
		return errors.New("disk full")
	}
	return nil
}

func main() {
	c := stubcounter.New(&flakyStorage{})

	err := c.Inc()
	fmt.Printf("first Inc err: %v, value: %d\n", err, c.Value())

	err = c.Inc()
	fmt.Printf("second Inc err: %v, value: %d\n", err, c.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first Inc err: disk full, value: 1
second Inc err: <nil>, value: 2
```

### Tests

`TestCounterPropagatesStorageError` injects the stub, asserts `Inc` returns a
non-nil error, and asserts `Value()==1` (state advanced despite the failure).
`TestCounterSafeUnderConcurrentInc` fans out 100 goroutines through a `done`
channel and asserts the final value and the recorded-call count both equal 100.
Run under `-race` this proves both the SUT and the spy are concurrency-correct.

Create `counter_test.go`:

```go
package stubcounter

import (
	"errors"
	"slices"
	"sync"
	"testing"
)

// errStub returns a configured error from Save exactly once, then succeeds.
type errStub struct {
	nextErr error
}

func (s *errStub) Save(int64) error {
	if s.nextErr != nil {
		err := s.nextErr
		s.nextErr = nil
		return err
	}
	return nil
}

// spyStorage records every saved value; concurrency-safe for the -race test.
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

func (s *spyStorage) Calls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.saves)
}

var errDiskFull = errors.New("disk full")

func TestCounterPropagatesStorageError(t *testing.T) {
	t.Parallel()

	c := New(&errStub{nextErr: errDiskFull})

	err := c.Inc()
	if err == nil {
		t.Fatal("Inc returned nil; want the injected storage error")
	}
	if !errors.Is(err, errDiskFull) {
		t.Fatalf("Inc error = %v, want errDiskFull", err)
	}
	if c.Value() != 1 {
		t.Fatalf("Value = %d after failed Save, want 1 (state must still advance)", c.Value())
	}
}

func TestCounterSafeUnderConcurrentInc(t *testing.T) {
	t.Parallel()

	spy := &spyStorage{}
	c := New(spy)

	const n = 100
	done := make(chan struct{})
	for range n {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = c.Inc()
		}()
	}
	for range n {
		<-done
	}

	if c.Value() != n {
		t.Fatalf("Value = %d, want %d", c.Value(), n)
	}
	if got := len(spy.Calls()); got != n {
		t.Fatalf("recorded %d Save calls, want %d", got, n)
	}
}
```

## Review

Two contracts are pinned here. First, error propagation with state preservation:
`Inc` advances the atomic total and then returns `Save`'s error unchanged, so the
stub's injected `errDiskFull` surfaces via `errors.Is` while `Value()` is already
1. If you reordered `Inc` to save before advancing, or swallowed the error, one of
those assertions goes red. Second, concurrency correctness of both the SUT and its
double: 100 goroutines produce exactly 100 in the counter and exactly 100 recorded
saves, and `-race` proves neither the atomic nor the mutex-guarded slice races.

The mistake this exercise exists to prevent is an unsynchronized double. It is
tempting to write the spy's `Save` as a bare `append` because "it's just a test" —
but the double runs under the SUT's full concurrency, and an unguarded append is a
real race. Guard the record, copy it out, and let `-race` be the arbiter. The
stub, by contrast, is only touched by one goroutine per test and needs no lock;
match the double's synchronization to how the SUT actually calls it.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the double's recorded state.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `go test -race` catches and why a racy double fails it.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — asserting a propagated sentinel error through wrapping.

---

Back to [01-hand-rolled-spy-counter.md](01-hand-rolled-spy-counter.md) | Next: [03-fake-in-memory-repository.md](03-fake-in-memory-repository.md)
