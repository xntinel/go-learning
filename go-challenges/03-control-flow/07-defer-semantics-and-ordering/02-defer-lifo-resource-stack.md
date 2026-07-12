# Exercise 2: LIFO Teardown of a Layered Resource Stack

Real connection setup acquires resources in a strict order — open a pooled
connection, begin a transaction on it, take an advisory lock inside that
transaction — and *must* release them in the exact reverse order. This exercise
models that stack with an ordered teardown recorder and proves that stacked
`defer`s unwind LIFO, so reverse-order release comes for free, while a
hand-ordered cleanup that releases the connection before the lock is a bug.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
resstack/                    independent module: example.com/resstack
  go.mod                     module example.com/resstack
  resstack.go                Recorder, Resource, Setup (defer-LIFO), SetupWrongOrder (bug)
  cmd/
    demo/
      main.go                runnable demo: acquire the stack, watch it unwind
  resstack_test.go           LIFO proof, reverse-teardown proof, the bug contrast
```

- Files: `resstack.go`, `cmd/demo/main.go`, `resstack_test.go`.
- Implement: a `Recorder` that appends event strings in order, a `Resource` interface with a `Close`, an acquire path `Setup` that opens conn, tx, and lock and defers each release, and a `SetupWrongOrder` that releases in acquisition order to show the bug.
- Test: register N defers appending their index and assert the order is `[N-1..0]`; drive `Setup` and assert teardown events are the exact reverse of acquisition; drive `SetupWrongOrder` and assert its teardown order is wrong.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/02-defer-lifo-resource-stack/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/02-defer-lifo-resource-stack
```

### Why LIFO is the correct default for a resource stack

Layered resources have a dependency order. The advisory lock is taken *inside* the
transaction, so it must be released *before* the transaction ends. The transaction
runs *on* the pooled connection, so it must finish before the connection returns
to the pool. The correct release order is therefore the exact reverse of the
acquisition order: lock, then transaction, then connection. If you release the
connection first while the lock still references it, you have a use-after-release
bug — the kind that surfaces as a driver panic or a poisoned pooled connection
under load.

Go's `defer` gives you this for free. Registering one `defer` per acquisition, in
acquisition order, means the releases fire in reverse — LIFO — because the last
`defer` registered runs first. You never write the reverse order by hand, so you
cannot get it wrong. `Setup` below acquires conn, tx, lock and defers each
`Close` immediately after it succeeds; on return the closes run lock, tx, conn.
`SetupWrongOrder` is the anti-pattern: it drops the defers and calls the closes
manually in acquisition order, which is what a careless refactor produces, and the
test proves its teardown order is reversed from what correctness requires.

Each `Resource` records an `acquire-<name>` event when created and a `close-<name>`
event when closed, into a shared `Recorder`. The recorded event stream is the
observable proof of ordering, and the `Recorder` is mutex-guarded so the same code
is safe to reason about even if resources were closed from multiple goroutines.

Create `resstack.go`:

```go
package resstack

import "sync"

// Recorder captures an ordered log of lifecycle events. It is the observable
// proof that teardown ran in the right order.
type Recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *Recorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// Events returns a copy of the recorded event stream.
func (r *Recorder) Events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// Resource is one layer of the stack: it can be closed exactly once.
type Resource interface {
	Close() error
}

type namedResource struct {
	name string
	rec  *Recorder
}

func (n *namedResource) Close() error {
	n.rec.record("close-" + n.name)
	return nil
}

// acquire creates a named resource and records its acquisition.
func acquire(rec *Recorder, name string) Resource {
	rec.record("acquire-" + name)
	return &namedResource{name: name, rec: rec}
}

// Setup acquires conn, tx, and lock in order and defers each release. The
// deferred closes unwind LIFO, so teardown is lock, tx, conn: the exact reverse
// of acquisition, which is what correctness requires.
func Setup(rec *Recorder) error {
	conn := acquire(rec, "conn")
	defer conn.Close()

	tx := acquire(rec, "tx")
	defer tx.Close()

	lock := acquire(rec, "lock")
	defer lock.Close()

	rec.record("work")
	return nil
}

// SetupWrongOrder is the bug: it releases in acquisition order (conn, tx, lock)
// instead of reverse. The lock outlives nothing, but the conn is returned to the
// pool while the tx and lock still reference it.
func SetupWrongOrder(rec *Recorder) error {
	conn := acquire(rec, "conn")
	tx := acquire(rec, "tx")
	lock := acquire(rec, "lock")

	rec.record("work")

	// Wrong: releasing in acquisition order, not reverse.
	conn.Close()
	tx.Close()
	lock.Close()
	return nil
}
```

### The runnable demo

The demo drives `Setup` and prints the recorded events so you can watch the stack
unwind in reverse.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/resstack"
)

func main() {
	rec := &resstack.Recorder{}
	if err := resstack.Setup(rec); err != nil {
		fmt.Println("setup:", err)
		return
	}
	for _, e := range rec.Events() {
		fmt.Println(e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquire-conn
acquire-tx
acquire-lock
work
close-lock
close-tx
close-conn
```

### Tests

`TestDeferOrderIsLIFO` pins the raw rule: N deferred appends produce `[N-1..0]`.
`TestSetupUnwindsInReverse` drives the real acquire path and asserts the teardown
half of the event stream is the exact reverse of the acquisition half.
`TestWrongOrderIsDetected` proves the hand-ordered variant releases conn first,
which is the bug the LIFO pattern prevents.

Create `resstack_test.go`:

```go
package resstack

import (
	"slices"
	"testing"
)

// assertEqual is a small helper that reports the whole slices on mismatch.
func assertEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("events =\n  %v\nwant\n  %v", got, want)
	}
}

func TestDeferOrderIsLIFO(t *testing.T) {
	t.Parallel()

	const n = 6
	var order []int
	func() {
		for i := range n {
			defer func() { order = append(order, i) }()
		}
	}()

	want := []int{5, 4, 3, 2, 1, 0}
	if !slices.Equal(order, want) {
		t.Fatalf("defer order = %v, want %v", order, want)
	}
}

func TestSetupUnwindsInReverse(t *testing.T) {
	t.Parallel()

	rec := &Recorder{}
	if err := Setup(rec); err != nil {
		t.Fatalf("Setup() = %v", err)
	}

	want := []string{
		"acquire-conn", "acquire-tx", "acquire-lock",
		"work",
		"close-lock", "close-tx", "close-conn",
	}
	assertEqual(t, rec.Events(), want)

	// The teardown suffix must be the exact reverse of the acquisition prefix.
	acquire := []string{"acquire-conn", "acquire-tx", "acquire-lock"}
	teardown := rec.Events()[4:]
	reversed := make([]string, len(acquire))
	for i, e := range acquire {
		// strip the "acquire-"/"close-" prefixes to compare the resource names
		name := e[len("acquire-"):]
		reversed[len(acquire)-1-i] = "close-" + name
	}
	assertEqual(t, teardown, reversed)
}

func TestWrongOrderIsDetected(t *testing.T) {
	t.Parallel()

	rec := &Recorder{}
	if err := SetupWrongOrder(rec); err != nil {
		t.Fatalf("SetupWrongOrder() = %v", err)
	}

	// The buggy variant closes conn first: the opposite of correct teardown.
	teardown := rec.Events()[4:]
	if teardown[0] != "close-conn" {
		t.Fatalf("first teardown = %q, want close-conn (the bug)", teardown[0])
	}

	correct := []string{"close-lock", "close-tx", "close-conn"}
	if slices.Equal(teardown, correct) {
		t.Fatal("SetupWrongOrder unexpectedly released in the correct reverse order")
	}
}
```

## Review

The stack is correct when teardown is the exact reverse of acquisition, and the
LIFO property of stacked `defer`s delivers that without any manual ordering:
`TestSetupUnwindsInReverse` derives the expected teardown by reversing the
acquisition list and compares. `SetupWrongOrder` exists to make the failure mode
concrete — releasing in acquisition order returns the connection while the
transaction and lock still depend on it. In real code the resources are a
`*sql.Conn`, a `*sql.Tx`, and a `pg_advisory_lock`, and the driver enforces the
ordering by failing loudly; the discipline of "one `defer` per acquisition, right
after it succeeds" is what keeps you on the correct side of it. Note the deferred
closes also run on the panic and error paths, so a failure deep in the stack still
unwinds every layer that was successfully acquired.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — the LIFO execution guarantee.
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — the acquire-then-defer-release idiom and reverse-order unwinding.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — comparing the recorded event streams.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-txstore-deferred-rollback.md](01-txstore-deferred-rollback.md) | Next: [03-defer-loop-fd-leak.md](03-defer-loop-fd-leak.md)
