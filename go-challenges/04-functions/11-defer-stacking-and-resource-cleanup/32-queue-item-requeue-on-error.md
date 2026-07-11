# Exercise 32: Queue Item Conditional Requeue — Defer Requeue if Error

**Nivel: Intermedio** — validacion rapida (un test corto).

Dequeuing a message for processing is a commitment: if the handler
succeeds, the item is done and should stay gone; if it fails, or panics,
the item needs to go back so something can try again. A single deferred
closure at the top of `Process`, conditioned on the named return `err` (and
on `recover()`), makes that distinction without the handler itself needing
to know anything about queues.

## What you'll build

```text
workqueue/                   independent module: example.com/workqueue
  go.mod
  workqueue/workqueue.go       Queue (mutex-guarded FIFO); Process (conditional requeue)
  cmd/demo/main.go              handler fails then succeeds; watch requeue at the front
  workqueue/workqueue_test.go   success (no requeue); error (requeue at front); panic (requeue + re-panic); empty queue
```

- Files: `workqueue/workqueue.go`, `cmd/demo/main.go`, `workqueue/workqueue_test.go`.
- Implement: a `Queue` wrapping a mutex-guarded `[]string` FIFO with `dequeue`/`requeueFront` helpers; and `Process(handle func(item string) error) (err error)`, which dequeues one item, defers a closure that requeues it at the front only if `err != nil` or a panic is recovered (then re-panics), and calls `handle(item)`.
- Test: a successful `handle` consumes the item permanently; a `handle` that returns an error leaves the item requeued at the front, ready to be dequeued again; a `handle` that panics still requeues before the panic propagates; calling `Process` on an empty queue returns `ErrEmpty`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/workqueue/workqueue ~/go-exercises/workqueue/cmd/demo
cd ~/go-exercises/workqueue
go mod init example.com/workqueue
go mod edit -go=1.24
```

### One defer, two conditions, no change to the handler

`Process` dequeues the item first, unconditionally — that part always
happens, because handling anything requires having it in hand. What
happens to that item afterward depends entirely on how `handle` returns,
and that decision lives in exactly one place: a deferred closure that runs
`recover()` first (if `handle` panicked, requeue and re-`panic(p)` so the
panic still propagates to the caller), and otherwise checks the function's
own named return `err` (if it is non-nil, requeue; if it is `nil`, the item
is genuinely done and the closure does nothing). Because this check lives
in `Process` rather than inside `handle`, every handler passed to `Process`
gets the same requeue behavior automatically — a handler that fails simply
returns an error, the same way any other function reports failure, without
needing to know it is running inside a queue-processing loop at all.

Create `workqueue/workqueue.go`:

```go
package workqueue

import (
	"errors"
	"sync"
)

// ErrEmpty is returned by Process when the queue has no items to dequeue.
var ErrEmpty = errors.New("workqueue: empty")

// Queue is a simple FIFO of string items, safe for concurrent use.
type Queue struct {
	mu    sync.Mutex
	items []string
}

// New returns a Queue preloaded with items, in order.
func New(items ...string) *Queue {
	q := &Queue{}
	q.items = append(q.items, items...)
	return q
}

// Len reports how many items remain in the queue.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *Queue) dequeue() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return "", false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

func (q *Queue) requeueFront(item string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append([]string{item}, q.items...)
}

// Process dequeues one item and runs handle on it. The dequeued item is put
// back at the front of the queue by a deferred closure -- but only if handle
// returns a non-nil error or panics. On a normal, successful return the item
// is considered done and is not requeued.
func (q *Queue) Process(handle func(item string) error) (err error) {
	item, ok := q.dequeue()
	if !ok {
		return ErrEmpty
	}

	defer func() {
		if p := recover(); p != nil {
			q.requeueFront(item)
			panic(p)
		}
		if err != nil {
			q.requeueFront(item)
		}
	}()

	return handle(item)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/workqueue/workqueue"
)

func main() {
	q := workqueue.New("send-email", "charge-card", "update-ledger")

	attempts := 0
	err := q.Process(func(item string) error {
		attempts++
		fmt.Printf("handling: %s (attempt %d)\n", item, attempts)
		return fmt.Errorf("card processor timeout")
	})

	fmt.Println("err:", err)
	fmt.Println("queue length after failed attempt:", q.Len())

	err = q.Process(func(item string) error {
		attempts++
		fmt.Printf("handling: %s (attempt %d)\n", item, attempts)
		return nil
	})
	fmt.Println("err:", err)
	fmt.Println("queue length after success:", q.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handling: send-email (attempt 1)
err: card processor timeout
queue length after failed attempt: 3
handling: send-email (attempt 2)
err: <nil>
queue length after success: 2
```

### Tests

Create `workqueue/workqueue_test.go`:

```go
package workqueue

import (
	"errors"
	"testing"
)

func TestProcessSuccessDoesNotRequeue(t *testing.T) {
	t.Parallel()

	q := New("a", "b")
	err := q.Process(func(item string) error { return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 (item consumed, not requeued)", got)
	}
}

func TestProcessErrorRequeuesAtFront(t *testing.T) {
	t.Parallel()

	q := New("a", "b")
	boom := errors.New("boom")

	var handled string
	err := q.Process(func(item string) error { handled = item; return boom })

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want errors.Is %v", err, boom)
	}
	if handled != "a" {
		t.Fatalf("handled = %q, want %q", handled, "a")
	}
	if got := q.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 (item requeued)", got)
	}

	// The requeued item must be back at the front.
	var handledAgain string
	_ = q.Process(func(item string) error { handledAgain = item; return nil })
	if handledAgain != "a" {
		t.Fatalf("handledAgain = %q, want %q (requeued item at front)", handledAgain, "a")
	}
}

func TestProcessPanicRequeuesThenRePanics(t *testing.T) {
	t.Parallel()

	q := New("a")

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = q.Process(func(item string) error {
			panic("handler exploded")
		})
	}()

	if got := q.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 (item requeued after panic)", got)
	}
}

func TestProcessEmptyQueueReturnsErrEmpty(t *testing.T) {
	t.Parallel()

	q := New()
	err := q.Process(func(item string) error { return nil })
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want errors.Is %v", err, ErrEmpty)
	}
}
```

## Review

`Process` is correct when a successful handler permanently removes the
item, when a failing or panicking handler leaves it requeued at the front
ready for another attempt, and when an empty queue is reported distinctly
via `ErrEmpty` rather than silently doing nothing. The mistake this pattern
exists to prevent is requeuing unconditionally in a `defer` (which would
put a successfully-handled item right back in line for reprocessing) or,
the opposite mistake, requeuing only from an explicit `if err != nil`
branch written inside `handle` itself (which a panic skips entirely,
losing the item outright). Keying the requeue off the deferred closure's
own view of `err` and `recover()` covers both exit paths from one place.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [Amazon SQS visibility timeout](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html) — the production analogue of "put it back if the handler doesn't finish".

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-context-value-isolation-restore.md](31-context-value-isolation-restore.md) | Next: [33-goroutine-cancel-panic-unwinding.md](33-goroutine-cancel-panic-unwinding.md)
