# Exercise 1: Lock-Free Treiber Stack as a Buffer Free-List

The Treiber stack is the canonical lock-free structure: a LIFO list whose head is
a single atomic pointer, pushed and popped with CAS loops. In production it shows
up as a *free-list* — a pool of reusable byte buffers a network server recycles
instead of allocating per request — and that is how you will build and exercise
it here.

## What you'll build

```text
freelist/                        independent module: example.com/freelist
  go.mod
  stack/
    stack.go                     node[T]; Stack[T]: Push, Pop, Size (atomic.Pointer CAS loop)
    stack_test.go                LIFO, empty-pop, size, distinct-values contract tests + ExampleStack
  cmd/
    demo/
      main.go                    a warm buffer free-list under 100 concurrent borrowers
```

- Files: `stack/stack.go`, `stack/stack_test.go`, `cmd/demo/main.go`.
- Implement: a generic `Stack[T]` with `Push`, `Pop`, and a best-effort `Size`, built on `atomic.Pointer[node[T]]` and `CompareAndSwap`; the demo uses it as a `Stack[[]byte]` free-list.
- Test: LIFO order, pop-on-empty, size after quiesce, and `TestStackDistinctValues` — 1000 values pushed concurrently are popped exactly once each.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/11-lock-free-data-structures/01-lock-free-treiber-stack/stack go-solutions/15-sync-primitives/11-lock-free-data-structures/01-lock-free-treiber-stack/cmd/demo
cd go-solutions/15-sync-primitives/11-lock-free-data-structures/01-lock-free-treiber-stack
```

### Why a server keeps a free-list

A server that reads requests into 32 KiB buffers allocates one per request if it
is naive. At tens of thousands of requests per second that is real allocator and
GC pressure. The classic fix is a free-list: finished handlers `Push` their
buffer back, new requests `Pop` a recycled one and only allocate on a miss. The
list is touched by every request from every goroutine, which makes it exactly the
kind of measured hot path where lock-free structures earn their keep. (The
standard library's `sync.Pool` solves a related problem with per-P caches and GC
integration — reach for it first in real code; this exercise builds the
underlying structure so you understand what such pools are made of.)

### The CAS loop, line by line

The whole structure is one atomic pointer to the top node. `Push` builds a new
node privately, points it at the current head, and tries to swing the head to the
new node:

```go
for {
	old := s.head.Load()
	n.next = old
	if s.head.CompareAndSwap(old, n) {
		return
	}
}
```

If another goroutine pushed or popped between the `Load` and the
`CompareAndSwap`, the head no longer equals `old`, the CAS fails, and the loop
retries from a fresh `Load`. Nothing blocks; the goroutine whose CAS succeeded
made progress, which is the lock-free guarantee. `Pop` is symmetric: read the
head, and CAS it to `head.next`.

Three details carry the correctness argument. First, `n` is unpublished until
the CAS succeeds, so writing `n.next` is race-free. Second, once published, a
node is never mutated — `Pop` reads `old.value` and `old.next` from a node that
was frozen at push time. Third, the pop-side CAS is where ABA would live: between
`Load` and `CompareAndSwap`, other goroutines might pop `old`, pop more, and push
`old`'s *value* back — but they can never push the same *node* back, because Go's
GC will not recycle `old`'s address while this goroutine still holds the
reference. In C, where nodes come from a recycled pool, this exact code corrupts
the list; in Go, `atomic.Pointer[T]` plus the GC makes pointer identity a safe
CAS token.

`Size` is a separate `atomic.Int64` updated *after* the winning CAS. It cannot be
part of the same atomic step as the head swap, so it is best-effort: exact once
the stack quiesces, approximately right during churn, and never a value to make
control decisions from. The tests only assert on it when no operation is in
flight.

Create `stack/stack.go`:

```go
package stack

import "sync/atomic"

type node[T any] struct {
	value T
	next  *node[T]
}

// Stack is a lock-free LIFO stack. The zero value is empty and ready
// to use. Stack must not be copied after first use.
type Stack[T any] struct {
	head atomic.Pointer[node[T]]
	size atomic.Int64
}

// Push adds value on top of the stack.
func (s *Stack[T]) Push(value T) {
	n := &node[T]{value: value}
	for {
		old := s.head.Load()
		n.next = old
		if s.head.CompareAndSwap(old, n) {
			s.size.Add(1)
			return
		}
	}
}

// Pop removes and returns the top value. The second result is false
// when the stack is empty.
func (s *Stack[T]) Pop() (T, bool) {
	for {
		old := s.head.Load()
		if old == nil {
			var zero T
			return zero, false
		}
		if s.head.CompareAndSwap(old, old.next) {
			s.size.Add(-1)
			return old.value, true
		}
	}
}

// Size returns the current approximate size. The count is updated
// atomically but is not perfectly synchronized with Push/Pop returns
// because it changes after the CAS, not with it. Use it for
// monitoring, never for control flow.
func (s *Stack[T]) Size() int64 {
	return s.size.Load()
}
```

### The demo: a warm free-list under a request burst

The demo warms the free-list with four 32 KiB buffers, then runs 100 concurrent
"requests". Each borrows a buffer (allocating on a miss and counting it), uses
it, and returns it. Afterwards the free-list must hold exactly the warm buffers
plus every miss-allocated one — the invariant a leaking pool would break. The
demo prints the invariant rather than the raw size because the miss count
legitimately varies with scheduling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"example.com/freelist/stack"
)

const bufSize = 32 * 1024

func main() {
	free := &stack.Stack[[]byte]{}
	for range 4 {
		free.Push(make([]byte, 0, bufSize))
	}
	fmt.Printf("warm free-list: size=%d\n", free.Size())

	var wg sync.WaitGroup
	var misses atomic.Int64
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf, ok := free.Pop()
			if !ok {
				misses.Add(1)
				buf = make([]byte, 0, bufSize)
			}
			buf = append(buf[:0], "response payload"...)
			_ = buf
			free.Push(buf[:0])
		}()
	}
	wg.Wait()

	fmt.Printf("invariant size == warm+misses: %v\n",
		free.Size() == 4+misses.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
warm free-list: size=4
invariant size == warm+misses: true
```

### Tests

`TestStackLIFOOrder`, `TestStackPopEmpty`, and `TestStackSizeMonotonic` pin the
sequential contract: LIFO order, `(zero, false)` from an empty (including
zero-value) stack, and an exact `Size` once operations have quiesced.
`TestStackDistinctValues` is the concurrency contract: 10 goroutines push the
values 0..999 (disjoint ranges), then the test pops exactly 1000 times into a
set. If any CAS interleaving could lose an item or deliver one twice, the set
comes up short or a duplicate is caught by name. Run everything under `-race`;
the race detector understands `sync/atomic` and will flag any unsynchronized
touch of node memory.

Create `stack/stack_test.go`:

```go
package stack

import (
	"fmt"
	"sync"
	"testing"
)

func TestStackLIFOOrder(t *testing.T) {
	t.Parallel()

	s := &Stack[int]{}
	for i := range 10 {
		s.Push(i)
	}
	for i := 9; i >= 0; i-- {
		v, ok := s.Pop()
		if !ok {
			t.Fatalf("Pop #%d returned ok=false", i)
		}
		if v != i {
			t.Fatalf("Pop #%d = %d, want %d", i, v, i)
		}
	}
	if _, ok := s.Pop(); ok {
		t.Fatal("Pop on empty stack returned ok=true")
	}
}

func TestStackPopEmpty(t *testing.T) {
	t.Parallel()

	var s Stack[string]
	v, ok := s.Pop()
	if ok {
		t.Fatal("Pop on zero-value stack returned ok=true")
	}
	if v != "" {
		t.Fatalf("Pop on empty stack returned %q, want zero value", v)
	}
}

func TestStackSizeMonotonic(t *testing.T) {
	t.Parallel()

	s := &Stack[int]{}
	for i := range 100 {
		s.Push(i)
	}
	if got := s.Size(); got != 100 {
		t.Fatalf("Size after 100 pushes = %d, want 100", got)
	}
	for range 100 {
		s.Pop()
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("Size after 100 pops = %d, want 0", got)
	}
}

func TestStackDistinctValues(t *testing.T) {
	t.Parallel()

	const goroutines = 10
	const perGoroutine = 100

	s := &Stack[int]{}
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range perGoroutine {
				s.Push(g*perGoroutine + j)
			}
		}()
	}
	wg.Wait()

	seen := make(map[int]bool, goroutines*perGoroutine)
	for range goroutines * perGoroutine {
		v, ok := s.Pop()
		if !ok {
			t.Fatalf("stack empty after %d pops, want %d values",
				len(seen), goroutines*perGoroutine)
		}
		if seen[v] {
			t.Fatalf("value %d popped twice", v)
		}
		seen[v] = true
	}
	if _, ok := s.Pop(); ok {
		t.Fatal("stack not empty after popping every pushed value")
	}
	if len(seen) != goroutines*perGoroutine {
		t.Fatalf("distinct values = %d, want %d", len(seen), goroutines*perGoroutine)
	}
}

func ExampleStack() {
	s := &Stack[int]{}
	s.Push(1)
	s.Push(2)
	v, _ := s.Pop()
	fmt.Println(v)
	// Output: 2
}
```

Run the gate:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

## Review

The structure is correct when three things hold: a node is written only before
its publishing CAS, `Pop`'s CAS moves the head to a `next` pointer read from the
node it is removing (not from a re-loaded head), and every retry starts with a
fresh `Load`. The classic bugs are reusing the stale `old` across retries (the
loop never converges) and mutating a node after pushing it (a data race the
detector reports immediately). `TestStackDistinctValues` is the test that
matters: losing an item or delivering one twice are precisely the failures a
subtly wrong CAS loop produces, and a set of 1000 distinct values pins both at
once. Remember what `Size` is: a monitoring number that is exact only at
quiescence — the demo's invariant check waits for `wg.Wait()` before trusting
it, and so should you.

## Resources

- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — the typed atomic pointer: `Load`, `CompareAndSwap`, and the nil-ready zero value.
- [Treiber stack](https://en.wikipedia.org/wiki/Treiber_stack) — the original algorithm and its ABA caveat in non-GC languages.
- [The Go Memory Model](https://go.dev/ref/mem) — why a value written before a successful CAS is visible to the goroutine whose Load observes it.
- [sync.Pool](https://pkg.go.dev/sync#Pool) — the standard library's production answer to buffer recycling; compare its per-P design with this global head.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-mutex-stack-baseline.md](02-mutex-stack-baseline.md)
