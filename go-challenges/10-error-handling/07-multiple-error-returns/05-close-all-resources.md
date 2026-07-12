# Exercise 5: Graceful Shutdown — Close Every Resource, Join Every Close Error

A shutdown path that stops at the first `Close` error leaks every resource after
it: the DB pool stays open because the cache client failed to close. This exercise
builds `CloseAll`, which closes every resource in reverse acquisition order,
continues past failures, and returns one `errors.Join` of all the close errors.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
shutdown/                  independent module: example.com/shutdown
  go.mod                   go 1.26
  shutdown.go              CloseAll(...io.Closer) error; Manager with Shutdown()
  cmd/
    demo/
      main.go              closes three fake resources, one of which errors
  shutdown_test.go         all closers invoked; errors.Is each failure; reverse order
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: `CloseAll(closers ...io.Closer) error` that closes in reverse order, continues past failures, and joins all errors; a `Manager` that registers closers and calls `CloseAll` on `Shutdown`.
- Test: fakes where some `Close` return errors and some nil — assert every closer was invoked even when an earlier one errors, assert `errors.Is` each failing closer's sentinel, and assert all-clean returns nil.
- Verify: `go test -count=1 -race ./...`

### Why reverse order, and why continue past failures

Resources are acquired in a dependency order: open the DB pool, then the cache that
sits in front of it, then the listener that accepts traffic. Shutdown must run the
reverse — stop accepting traffic first, then tear down the cache, then the pool —
so nothing is torn out from under a still-live dependent. `CloseAll` closes the
slice back to front for exactly this reason.

The other half is that a `Close` failure must not abort the loop. Each resource is
independent to close; if closing the cache client errors, the DB pool and listener
still need closing or they leak. So `CloseAll` records each error and keeps going,
then returns `errors.Join` of whatever failed. A caller can log the aggregate and
know every close was attempted. This is the fail-complete pattern again, and the
cost of getting it wrong is a resource leak on every shutdown, not just a hidden
message.

Note the deferred-close discipline this generalizes: `defer f.Close()` on a write
path silently drops the close error, which for a buffered writer is where the final
flush failure surfaces. `CloseAll` makes that error a first-class return, joined
with the rest, so a failed final flush is visible rather than lost.

Create `shutdown.go`:

```go
package shutdown

import (
	"errors"
	"fmt"
	"io"
)

// CloseAll closes every closer in reverse order, continues past any failure, and
// returns errors.Join of all close errors. It returns nil when every close
// succeeds. Reverse order mirrors acquisition: last acquired is first closed.
func CloseAll(closers ...io.Closer) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].Close(); err != nil {
			errs = append(errs, fmt.Errorf("closer %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// Manager owns a set of resources registered in acquisition order.
type Manager struct {
	closers []io.Closer
}

// Register adds a closer in acquisition order. Later registrations close first.
func (m *Manager) Register(c io.Closer) {
	m.closers = append(m.closers, c)
}

// Shutdown closes every registered resource via CloseAll.
func (m *Manager) Shutdown() error {
	return CloseAll(m.closers...)
}
```

### The runnable demo

The demo registers three fake resources where the cache client fails to close, and
shows that the pool and listener still close and the failure is reported.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/shutdown"
)

type resource struct {
	name string
	err  error
}

func (r *resource) Close() error {
	fmt.Printf("closing %s\n", r.name)
	return r.err
}

func main() {
	var m shutdown.Manager
	m.Register(&resource{name: "db-pool"})
	m.Register(&resource{name: "cache", err: errors.New("connection reset")})
	m.Register(&resource{name: "listener"})

	if err := m.Shutdown(); err != nil {
		fmt.Println("shutdown errors:")
		fmt.Println(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (closers run back to front: listener at index 2, then cache at
index 1, then db-pool at index 0):

```
closing listener
closing cache
closing db-pool
shutdown errors:
closer 1: connection reset
```

### Tests

The tests use a `fakeCloser` with a call counter and a configurable error.
`TestCloseAllInvokesEveryCloser` puts a failing closer in the middle and asserts
every closer's `Close` ran exactly once — the anti-leak property. `TestCloseAllJoinsErrors`
asserts the aggregate is `errors.Is` each failing sentinel. `TestCloseAllReverseOrder`
records the order closers ran and asserts it is the reverse of registration.
`TestCloseAllAllClean` asserts all-success returns nil.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"errors"
	"testing"
)

type fakeCloser struct {
	id     int
	err    error
	calls  int
	record *[]int
}

func (f *fakeCloser) Close() error {
	f.calls++
	if f.record != nil {
		*f.record = append(*f.record, f.id)
	}
	return f.err
}

var (
	errPool = errors.New("pool close failed")
	errNet  = errors.New("listener close failed")
)

func TestCloseAllInvokesEveryCloser(t *testing.T) {
	t.Parallel()

	a := &fakeCloser{id: 0}
	b := &fakeCloser{id: 1, err: errPool}
	c := &fakeCloser{id: 2}

	_ = CloseAll(a, b, c)

	for _, f := range []*fakeCloser{a, b, c} {
		if f.calls != 1 {
			t.Errorf("closer %d called %d times, want 1", f.id, f.calls)
		}
	}
}

func TestCloseAllJoinsErrors(t *testing.T) {
	t.Parallel()

	a := &fakeCloser{id: 0, err: errPool}
	b := &fakeCloser{id: 1}
	c := &fakeCloser{id: 2, err: errNet}

	err := CloseAll(a, b, c)
	if err == nil {
		t.Fatal("CloseAll = nil, want error")
	}
	if !errors.Is(err, errPool) {
		t.Error("aggregate missing errPool")
	}
	if !errors.Is(err, errNet) {
		t.Error("aggregate missing errNet")
	}
}

func TestCloseAllReverseOrder(t *testing.T) {
	t.Parallel()

	var order []int
	a := &fakeCloser{id: 0, record: &order}
	b := &fakeCloser{id: 1, record: &order}
	c := &fakeCloser{id: 2, record: &order}

	if err := CloseAll(a, b, c); err != nil {
		t.Fatalf("CloseAll = %v, want nil", err)
	}
	want := []int{2, 1, 0}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestCloseAllAllClean(t *testing.T) {
	t.Parallel()

	a := &fakeCloser{id: 0}
	b := &fakeCloser{id: 1}
	if err := CloseAll(a, b); err != nil {
		t.Fatalf("CloseAll = %v, want nil", err)
	}
}
```

## Review

`CloseAll` is correct when every closer runs exactly once regardless of earlier
failures, the returned error is `errors.Is` each failing closer's sentinel, and
all-clean returns nil. The invoke-every-closer test is the anti-leak guard: if
someone rewrites the loop to `return err` on the first failure, that test fails
because the later closers never run. Reverse order is asserted explicitly because it
is a correctness property, not a nicety — closing a dependency before its dependent
can corrupt in-flight work. Run with `-race`; the closers here are sequential, but
a real manager may be closed from a signal handler goroutine.

## Resources

- [io.Closer](https://pkg.go.dev/io#Closer) — the single-method interface every resource satisfies.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating the close errors.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — why deferred close errors need capturing.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-validate-all-fields.md](04-validate-all-fields.md) | Next: [06-batch-partial-success.md](06-batch-partial-success.md)
