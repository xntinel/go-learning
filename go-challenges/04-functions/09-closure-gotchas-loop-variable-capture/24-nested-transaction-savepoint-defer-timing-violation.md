# Exercise 24: Nested Transaction Savepoints: Defer Timing in Transaction Loops

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A batch processor commits a list of orders, each opening an outer savepoint
and a nested inner savepoint. The obvious `defer sp.Release()` inside the
loop body for both savepoints holds every order's savepoints open until the
whole batch function returns, instead of unwinding each order's nesting as
soon as it finishes. The relative LIFO order the releases finally run in
happens to be correct for nesting (inner before its own outer) — but the
TIMING is wrong: order 1's savepoints are still open while order 50 is being
processed.

## What you'll build

```text
nestedtx/                    independent module: example.com/nestedtx
  go.mod                     go 1.24
  nestedtx.go                  Tracker, Savepoint, ProcessOrdersLeaky, ProcessOrdersScoped
  cmd/
    demo/
      main.go                runnable demo: run both, print peak-open and release order
  nestedtx_test.go             table test: peak-open==2 vs ==2N, release order, edge cases
```

- Files: `nestedtx.go`, `cmd/demo/main.go`, `nestedtx_test.go`.
- Implement: `ProcessOrdersLeaky` (defer both releases inside the loop, every savepoint outlives the loop) and `ProcessOrdersScoped` (a per-order helper that scopes both defers), both aggregating errors with `errors.Join`.
- Test: a `Tracker` records open/release events; assert scoped keeps peak concurrent-open at 2 (one order's outer plus its own inner) and releases inner-then-outer per order before the next order opens, while leaky peaks at 2N; cover a single-order and an empty-batch edge case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why LIFO order does not save you from defer timing

`defer` schedules a call to run when the *enclosing function* returns, not
when the loop iteration ends. In `ProcessOrdersLeaky`, every savepoint's
`Release()` is queued and none runs until `ProcessOrdersLeaky` itself
returns. During the loop, order 1's outer AND inner savepoints are still open
while order 2 opens its own, and so on: the peak number of simultaneously
open savepoints equals twice the order count. The releases that finally run
when the function returns do fire in LIFO order — which happens to match the
correct nesting rule (an inner savepoint must release before its own outer)
— but that correctness is incidental. What is broken is that order 1's
savepoints stay open for the ENTIRE batch instead of just order 1's own
processing, holding whatever locks or resources those savepoints represent
far longer than necessary.

`ProcessOrdersScoped` fixes it by moving each order's outer-then-inner-open,
inner-then-outer-release sequence into a helper function whose own `return`
scopes both defers. Each call to the helper opens the outer savepoint, defers
its release, opens the inner savepoint, defers its release, and returns — at
which point both deferred releases run, in the correct inner-then-outer
order, before the next order even opens anything. Peak concurrently open is
2, never more.

Create `nestedtx.go`:

```go
package nestedtx

import (
	"errors"
	"sync"
)

// Event is one savepoint open or release observed by the Tracker.
type Event struct {
	Name    string
	Release bool // false = open, true = release
}

// Tracker stands in for a database's savepoint stack while a batch of orders
// is processed, each order opening an outer savepoint and a nested inner
// savepoint. It records the sequence of opens and releases and the peak
// number of savepoints simultaneously open.
type Tracker struct {
	mu     sync.Mutex
	events []Event
	live   int
	peak   int
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{}
}

// Savepoint is one open savepoint on the Tracker's stack.
type Savepoint struct {
	name string
	t    *Tracker
}

// Open records opening a savepoint and returns a handle to it.
func (t *Tracker) Open(name string) *Savepoint {
	t.mu.Lock()
	t.events = append(t.events, Event{Name: name})
	t.live++
	if t.live > t.peak {
		t.peak = t.live
	}
	t.mu.Unlock()
	return &Savepoint{name: name, t: t}
}

// Release records releasing this savepoint.
func (s *Savepoint) Release() error {
	s.t.mu.Lock()
	s.t.events = append(s.t.events, Event{Name: s.name, Release: true})
	s.t.live--
	s.t.mu.Unlock()
	return nil
}

// Peak reports the maximum number of savepoints open at the same instant.
func (t *Tracker) Peak() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peak
}

// ReleaseOrder returns the names of savepoints in the order they released.
func (t *Tracker) ReleaseOrder() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, e := range t.events {
		if e.Release {
			out = append(out, e.Name)
		}
	}
	return out
}

// ProcessOrdersLeaky opens an outer savepoint and a nested inner savepoint
// per order, deferring BOTH releases inside the loop body. `defer` only runs
// when ProcessOrdersLeaky itself returns, so EVERY order's savepoints --
// outer and inner alike -- stay open simultaneously for the whole batch
// instead of unwinding as soon as that order finishes. The relative
// LIFO order the releases finally run in is correct for nested savepoints
// (inner before its own outer), but the TIMING is not: order 1's savepoints
// are still open while order 50 is being processed.
func ProcessOrdersLeaky(t *Tracker, orderIDs []string) error {
	var errs []error
	for _, id := range orderIDs {
		outer := t.Open("order:" + id)
		defer func() {
			if err := outer.Release(); err != nil {
				errs = append(errs, err)
			}
		}()
		inner := t.Open("item:" + id)
		defer func() {
			if err := inner.Release(); err != nil {
				errs = append(errs, err)
			}
		}()
	}
	return errors.Join(errs...)
}

// ProcessOrdersScoped opens and releases each order's outer and inner
// savepoints inside a per-order helper whose own return scopes the defers,
// so both release immediately after that order finishes and before the next
// order's savepoints even open. Peak concurrently open is 2 (one order's
// outer plus its own inner), never more.
func ProcessOrdersScoped(t *Tracker, orderIDs []string) error {
	var errs []error
	for _, id := range orderIDs {
		err := func() (err error) {
			outer := t.Open("order:" + id)
			defer func() {
				if cerr := outer.Release(); cerr != nil {
					err = cerr
				}
			}()
			inner := t.Open("item:" + id)
			defer func() {
				if cerr := inner.Release(); cerr != nil {
					err = cerr
				}
			}()
			return nil
		}()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo processes three orders with both variants and prints the peak-open
count and the release order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/nestedtx"
)

func main() {
	orders := []string{"1", "2", "3"}

	leaky := nestedtx.NewTracker()
	_ = nestedtx.ProcessOrdersLeaky(leaky, orders)
	fmt.Println("leaky  peak-open:", leaky.Peak(), "release-order:", leaky.ReleaseOrder())

	scoped := nestedtx.NewTracker()
	_ = nestedtx.ProcessOrdersScoped(scoped, orders)
	fmt.Println("scoped peak-open:", scoped.Peak(), "release-order:", scoped.ReleaseOrder())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky  peak-open: 6 release-order: [item:3 order:3 item:2 order:2 item:1 order:1]
scoped peak-open: 2 release-order: [item:1 order:1 item:2 order:2 item:3 order:3]
```

### Tests

`TestProcessOrders` is a table test asserting scoped peaks at 2 concurrently
open savepoints and releases inner-then-outer per order before the next
order opens, while leaky peaks at twice the order count and only unwinds
everything, LIFO, once the whole batch returns. `TestProcessOrdersSingle
OrderEdgeCase` and `TestProcessOrdersEmptyBatchEdgeCase` cover the boundaries
where nesting has only one order to unwind or none at all.

Create `nestedtx_test.go`:

```go
package nestedtx

import (
	"fmt"
	"testing"
)

func TestProcessOrders(t *testing.T) {
	orders := []string{"1", "2", "3"}

	tests := []struct {
		name      string
		process   func(*Tracker, []string) error
		wantPeak  int
		wantOrder []string
	}{
		{
			name:     "scoped unwinds each order before the next opens",
			process:  ProcessOrdersScoped,
			wantPeak: 2,
			wantOrder: []string{
				"item:1", "order:1",
				"item:2", "order:2",
				"item:3", "order:3",
			},
		},
		{
			name:     "leaky holds every savepoint open until the batch returns",
			process:  ProcessOrdersLeaky,
			wantPeak: 2 * len(orders),
			wantOrder: []string{
				"item:3", "order:3",
				"item:2", "order:2",
				"item:1", "order:1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker()
			if err := tt.process(tr, orders); err != nil {
				t.Fatalf("process: %v", err)
			}
			if got := tr.Peak(); got != tt.wantPeak {
				t.Fatalf("peak-open = %d, want %d", got, tt.wantPeak)
			}
			got := tr.ReleaseOrder()
			if len(got) != len(tt.wantOrder) {
				t.Fatalf("release-order = %v, want %v", got, tt.wantOrder)
			}
			for i := range tt.wantOrder {
				if got[i] != tt.wantOrder[i] {
					t.Fatalf("release-order = %v, want %v", got, tt.wantOrder)
				}
			}
		})
	}
}

func TestProcessOrdersSingleOrderEdgeCase(t *testing.T) {
	tr := NewTracker()
	if err := ProcessOrdersScoped(tr, []string{"solo"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got := tr.Peak(); got != 2 {
		t.Fatalf("peak-open = %d, want 2", got)
	}
	want := []string{"item:solo", "order:solo"}
	got := tr.ReleaseOrder()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("release-order = %v, want %v", got, want)
		}
	}
}

func TestProcessOrdersEmptyBatchEdgeCase(t *testing.T) {
	tr := NewTracker()
	if err := ProcessOrdersScoped(tr, nil); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got := tr.Peak(); got != 0 {
		t.Fatalf("peak-open = %d, want 0", got)
	}
}

func ExampleProcessOrdersScoped() {
	tr := NewTracker()
	_ = ProcessOrdersScoped(tr, []string{"a", "b"})
	fmt.Println("peak:", tr.Peak())
	// Output: peak: 2
}
```

## Review

The batch processor is correct when scoped processing keeps peak
concurrently open savepoints at 2 (one order's own nesting) and unwinds
inner-then-outer before the next order even opens, while leaky peaks at twice
the order count. The mechanism to keep straight is that `defer` fires at
function return, and LIFO order alone does not make a bare loop safe: it
happens to reproduce the correct nesting relationship (inner released before
outer) but says nothing about WHEN that unwinding happens relative to the
rest of the batch. A per-order helper whose own `return` scopes both defers
is what ties the unwind timing to that order's own scope instead of the
whole function's.

## Resources

- [Effective Go: defer](https://go.dev/doc/effective_go#defer) — defer timing and LIFO ordering.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — when deferred calls actually run.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors into one that `errors.Is` can match.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-grpc-listener-address-handler-binding-closure.md](23-grpc-listener-address-handler-binding-closure.md) | Next: [25-pubsub-topic-handler-registration-loop-capture.md](25-pubsub-topic-handler-registration-loop-capture.md)
